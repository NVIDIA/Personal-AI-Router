// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// pubFromDER parses a certificate DER and extracts its Ed25519 public key.
func pubFromDER(der []byte) (ed25519.PublicKey, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse cert: %w", err)
	}
	return pubKeyFromCert(cert)
}

// TrustedPin is the on-disk schema of trusted/<peerNodeUuid>.json — the pinned
// certificate and display metadata for one paired peer.
type TrustedPin struct {
	NodeUUID        string        `json:"nodeUuid"`
	NodeID          string        `json:"nodeId"`
	Name            string        `json:"name"`
	ClusterID       string        `json:"clusterId"`
	AdmissionEpoch  uint64        `json:"admissionEpoch,omitempty"`
	CertPem         string        `json:"certPem"`
	CertFingerprint string        `json:"certFingerprint"`
	PinnedAt        int64         `json:"pinnedAt"`
	Endorsements    []Endorsement `json:"endorsements,omitempty"` // signed vouchings that admitted this peer (fan-out trust)
}

// TrustStore is the in-memory view of the trusted/ directory: one file per
// pinned peer, keyed by the peer's node UUID (the filename). Writes are
// serialized; reads tolerate a just-added/removed entry without a global lock.
type TrustStore struct {
	dir  string
	mu   sync.RWMutex
	pins map[string]*TrustedPin
	ders map[string][]byte            // uuid -> cert DER, for byte-for-byte mTLS pin match
	pubs map[string]ed25519.PublicKey // uuid -> cert public key, for endorsement/tombstone verification

	// onChange is announced after any mutation of the pin set lands on disk.
	// Consumers elsewhere on this node cache answers derived from these files
	// (most importantly "do I hold a pin for this peer?", which decides whether
	// work can be routed to it), and nothing else on the machine can observe the
	// write. It lives on the store rather than at the ~19 call sites that pin and
	// unpin peers — pairing, gossip merge, removal, tombstones, teardown, startup
	// prune — because a mutation path that forgets to announce leaves those
	// consumers permanently wrong, which is the bug this exists to prevent.
	//
	// It fires after the store's lock is released, so a handler is free to read
	// back. Set once at construction; never while the store is in use.
	onChange func()
}

// SetOnChange registers the pin-set change announcement. Not safe to call
// concurrently with a mutation; the manager wires it during construction.
func (ts *TrustStore) SetOnChange(fn func()) { ts.onChange = fn }

// announce runs the change hook if one is registered. Callers defer it BEFORE
// deferring the unlock so it runs after the lock is dropped (defers unwind
// last-registered-first).
func (ts *TrustStore) announce(changed *bool) {
	if *changed && ts.onChange != nil {
		ts.onChange()
	}
}

func cloneTrustedPin(pin *TrustedPin) *TrustedPin {
	if pin == nil {
		return nil
	}
	cp := *pin
	cp.Endorsements = append([]Endorsement(nil), pin.Endorsements...)
	return &cp
}

// newTrustStore opens (creating if needed) the trusted/ subdirectory of
// clusterDir and loads all valid pins into memory.
func newTrustStore(clusterDir string) (*TrustStore, error) {
	dir := filepath.Join(clusterDir, "trusted")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create trusted dir %s: %w", dir, err)
	}
	ts := &TrustStore{
		dir:  dir,
		pins: make(map[string]*TrustedPin),
		ders: make(map[string][]byte),
		pubs: make(map[string]ed25519.PublicKey),
	}
	ts.load()
	return ts, nil
}

func (ts *TrustStore) pinPath(uuid string) string {
	return filepath.Join(ts.dir, uuid+".json")
}

// load reads trusted/*.json into memory, skipping *.tmp, unparseable files, and
// any file whose inner nodeUuid / cert subject doesn't match its filename (so a
// renamed or tampered file can't masquerade as another UUID).
func (ts *TrustStore) load() {
	entries, err := os.ReadDir(ts.dir)
	if err != nil {
		log.Printf("trusted store: read dir: %v", err)
		return
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		fileUUID := strings.TrimSuffix(name, ".json")
		data, err := os.ReadFile(filepath.Join(ts.dir, name))
		if err != nil {
			log.Printf("trusted store: read %s: %v", name, err)
			continue
		}
		var pin TrustedPin
		if err := json.Unmarshal(data, &pin); err != nil {
			log.Printf("trusted store: skip unparseable %s: %v", name, err)
			continue
		}
		der, err := validatePin(&pin, fileUUID)
		if err != nil {
			log.Printf("trusted store: skip tampered %s: %v", name, err)
			continue
		}
		pub, err := pubFromDER(der)
		if err != nil {
			log.Printf("trusted store: skip %s: %v", name, err)
			continue
		}
		ts.pins[pin.NodeUUID] = &pin
		ts.ders[pin.NodeUUID] = der
		ts.pubs[pin.NodeUUID] = pub
	}
	log.Printf("trusted store: loaded %d pin(s) from %s", len(ts.pins), ts.dir)
}

// validatePin checks that the pin's inner nodeUuid and embedded certificate
// subject/URI all agree with the expected UUID, returning the parsed cert DER.
func validatePin(pin *TrustedPin, expectUUID string) ([]byte, error) {
	if pin.NodeUUID != expectUUID {
		return nil, fmt.Errorf("inner nodeUuid %q != filename %q", pin.NodeUUID, expectUUID)
	}
	block, _ := pem.Decode([]byte(pin.CertPem))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("no CERTIFICATE block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse cert: %w", err)
	}
	if got := uuidFromCert(cert); got != expectUUID {
		return nil, fmt.Errorf("cert principal %q != filename %q", got, expectUUID)
	}
	return block.Bytes, nil
}

// Pin durably records a peer's certificate. Re-pinning the identical cert for an
// existing UUID is a no-op; a different cert for an already-pinned UUID is
// rejected (explicit re-invite required — no silent key rotation).
func (ts *TrustStore) Pin(pin *TrustedPin) error {
	der, err := validatePin(pin, pin.NodeUUID)
	if err != nil {
		return fmt.Errorf("invalid pin: %w", err)
	}
	pub, err := pubFromDER(der)
	if err != nil {
		return fmt.Errorf("invalid pin: %w", err)
	}
	changed := false
	defer ts.announce(&changed)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if existing, ok := ts.ders[pin.NodeUUID]; ok {
		if bytes.Equal(existing, der) {
			current := ts.pins[pin.NodeUUID]
			if pin.AdmissionEpoch > current.AdmissionEpoch {
				// Same cryptographic principal, newer admitted incarnation.
				// Persist the epoch/metadata transition before callers clear the
				// older removal proof.
				updated := *pin
				updated.Endorsements = append(append([]Endorsement(nil), current.Endorsements...), pin.Endorsements...)
				if err := ts.writePinLocked(&updated); err != nil {
					return err
				}
				changed = true
				return nil
			}
			if pin.AdmissionEpoch < current.AdmissionEpoch {
				return nil
			}
			// Identical re-pin: fold in any newly-seen endorsements so the
			// gossiped trust web thickens (idempotent on the cert itself).
			err := ts.mergeEndorsementsLocked(pin.NodeUUID, pin.Endorsements)
			changed = err == nil
			return err
		}
		return fmt.Errorf("uuid %s already pinned to a different certificate; remove it first to re-pin", pin.NodeUUID)
	}
	if err := ts.writePinLocked(pin); err != nil {
		return err
	}
	ts.ders[pin.NodeUUID] = der
	ts.pubs[pin.NodeUUID] = pub
	changed = true
	return nil
}

// writePinLocked persists a pin atomically and updates the in-memory pin map.
// Caller holds ts.mu.
func (ts *TrustStore) writePinLocked(pin *TrustedPin) error {
	data, err := json.MarshalIndent(pin, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(ts.pinPath(pin.NodeUUID), data, 0o600); err != nil {
		return err
	}
	ts.pins[pin.NodeUUID] = cloneTrustedPin(pin)
	return nil
}

// mergeEndorsementsLocked unions incoming endorsements into an existing pin
// (dedup by signer+signature) and persists if anything new was added. Caller
// holds ts.mu. A missing pin is a silent no-op.
func (ts *TrustStore) mergeEndorsementsLocked(uuid string, incoming []Endorsement) error {
	pin, ok := ts.pins[uuid]
	if !ok || len(incoming) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(pin.Endorsements))
	for _, e := range pin.Endorsements {
		seen[endorsementKey(e)] = struct{}{}
	}
	updated := cloneTrustedPin(pin)
	added := false
	for _, e := range incoming {
		k := endorsementKey(e)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		updated.Endorsements = append(updated.Endorsements, e)
		added = true
	}
	if !added {
		return nil
	}
	return ts.writePinLocked(updated)
}

func endorsementKey(e Endorsement) string {
	sig := e.SigV2
	if sig == "" {
		sig = e.Sig
	}
	return e.By + "|" + sig
}

// AddEndorsements unions new endorsements into an already-pinned peer.
func (ts *TrustStore) AddEndorsements(uuid string, endorsements []Endorsement) error {
	changed := false
	defer ts.announce(&changed)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	err := ts.mergeEndorsementsLocked(uuid, endorsements)
	changed = err == nil
	return err
}

// MigrateLegacyAdmissions assigns epoch 1 to pre-v2 pins that were admitted
// before the protocol carried an incarnation. Every legacy principal can only
// be in its first admission, so this mapping is deterministic across upgraded
// cluster members and makes an offline legacy member removable.
func (ts *TrustStore) MigrateLegacyAdmissions(clusterID string) error {
	migrated := false
	defer ts.announce(&migrated)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for uuid, pin := range ts.pins {
		if pin.ClusterID != "" && pin.ClusterID != clusterID {
			continue
		}
		updated := *pin
		changed := false
		if updated.ClusterID == "" {
			updated.ClusterID = clusterID
			changed = true
		}
		if updated.AdmissionEpoch == 0 {
			updated.AdmissionEpoch = legacyAdmissionEpoch
			changed = true
		}
		if !changed {
			continue
		}
		if err := ts.writePinLocked(&updated); err != nil {
			return fmt.Errorf("migrate legacy pin %s: %w", uuid, err)
		}
		migrated = true
	}
	return nil
}

// UpdateIdentity refreshes a pinned peer's display fields (nodeId/name) without
// touching the pinned certificate, persisting only if something changed. Empty
// inputs are ignored so a partial roster entry can't blank a known name. This is
// how a peer's PC rename propagates into our pin instead of leaving a stale
// hostname behind the stable UUID. A missing pin is a no-op.
func (ts *TrustStore) UpdateIdentity(uuid, nodeID, name string) (bool, error) {
	persisted := false
	defer ts.announce(&persisted)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	pin, ok := ts.pins[uuid]
	if !ok {
		return false, nil
	}
	updated := cloneTrustedPin(pin)
	changed := false
	if nodeID != "" && updated.NodeID != nodeID {
		updated.NodeID = nodeID
		changed = true
	}
	if name != "" && updated.Name != name {
		updated.Name = name
		changed = true
	}
	if !changed {
		return false, nil
	}
	if err := ts.writePinLocked(updated); err != nil {
		return false, err
	}
	persisted = true
	return true, nil
}

// PubKey returns the Ed25519 public key of a pinned peer, used to verify that
// peer's endorsements/tombstones.
func (ts *TrustStore) PubKey(uuid string) (ed25519.PublicKey, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	pub, ok := ts.pubs[uuid]
	if !ok {
		return nil, false
	}
	return append(ed25519.PublicKey(nil), pub...), true
}

// Remove deletes a peer's pin. A missing file is treated as already-removed
// (idempotent success).
func (ts *TrustStore) Remove(uuid string) error {
	changed := false
	defer ts.announce(&changed)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if err := os.Remove(ts.pinPath(uuid)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove pin %s: %w", uuid, err)
	}
	_, held := ts.pins[uuid]
	delete(ts.pins, uuid)
	delete(ts.ders, uuid)
	delete(ts.pubs, uuid)
	changed = held
	return nil
}

// Forget removes a pin from live authorization without touching disk. Teardown
// uses it after a durable delete failure so a broken filesystem cannot leave an
// old certificate trusted while the journal forces recovery on restart.
func (ts *TrustStore) Forget(uuid string) {
	changed := false
	defer ts.announce(&changed)
	ts.mu.Lock()
	_, held := ts.pins[uuid]
	delete(ts.pins, uuid)
	delete(ts.ders, uuid)
	delete(ts.pubs, uuid)
	changed = held
	ts.mu.Unlock()
}

// Get returns the pin for a UUID, if present.
func (ts *TrustStore) Get(uuid string) (*TrustedPin, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	pin, ok := ts.pins[uuid]
	return cloneTrustedPin(pin), ok
}

// MatchDER reports whether the supplied certificate DER byte-for-byte matches
// the pinned cert for uuid. This is the only trust decision for mTLS.
func (ts *TrustStore) MatchDER(uuid string, der []byte) bool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	pinned, ok := ts.ders[uuid]
	return ok && bytes.Equal(pinned, der)
}

// DER returns a copy of the pinned certificate DER for uuid, used to pin the
// server cert when this node makes an outbound mTLS call to a peer.
func (ts *TrustStore) DER(uuid string) ([]byte, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	der, ok := ts.ders[uuid]
	if !ok {
		return nil, false
	}
	cp := make([]byte, len(der))
	copy(cp, der)
	return cp, true
}

// List returns a snapshot of all pins.
func (ts *TrustStore) List() []*TrustedPin {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	out := make([]*TrustedPin, 0, len(ts.pins))
	for _, p := range ts.pins {
		out = append(out, cloneTrustedPin(p))
	}
	return out
}
