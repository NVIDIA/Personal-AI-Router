// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustPinPath(t *testing.T, ts *TrustStore, uuid string) string {
	t.Helper()
	path, err := ts.pinPath(uuid)
	require.NoError(t, err, "pin path")
	return path
}

// pinWithCommonName supplies a real certificate whose CN carries the principal,
// including crafted identifiers that a trusted peer can sign and endorse.
func pinWithCommonName(t *testing.T, uuid string) *TrustedPin {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err, "generate key")
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: uuid},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, priv)
	require.NoError(t, err, "create certificate")
	return &TrustedPin{
		NodeUUID:  uuid,
		ClusterID: "cluster-1",
		CertPem:   string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
	}
}

func TestTrustStore_RejectsUnsafePinIdentifiers(t *testing.T) {
	test := func(name, uuid string) {
		t.Run(name, func(t *testing.T) {
			ts, count := newAnnouncingStore(t)
			victim := filepath.Join(filepath.Dir(ts.dir), "victim.json")
			require.NoError(t, os.WriteFile(victim, []byte("preserve me"), 0o600), "write victim")
			require.Error(t, ts.Pin(pinWithCommonName(t, uuid)), "unsafe principal was pinned")
			data, err := os.ReadFile(victim)
			require.NoError(t, err, "read victim")
			assert.Equal(t, "preserve me", string(data), "victim file changed")
			entries, err := os.ReadDir(ts.dir)
			require.NoError(t, err, "read trusted directory")
			assert.Empty(t, entries, "rejected pin changed files")
			assert.Empty(t, ts.List(), "rejected pin changed live state")
			assert.Zero(t, count(), "rejected pin announced a change")
		})
	}
	test("empty principal", "")
	test("current directory", ".")
	test("parent directory", "..")
	test("Unix traversal", "../victim")
	test("Windows traversal", `..\victim`)
	test("nested Unix traversal", "sub/../../victim")
	test("nested Windows traversal", `sub\..\..\victim`)
	test("absolute path", filepath.Join(t.TempDir(), "victim"))
	test("drive relative path", "C:victim")
	test("UNC path", `\\server\share\victim`)
	test("alternate data stream", "peer:stream")
	test("NUL byte", "peer\x00victim")
	if runtime.GOOS == "windows" {
		test("Windows reserved device", "CON")
		test("Windows reserved serial port", "COM1")
		test("Windows reserved printer port", "LPT1")
	}
}

func TestTrustStore_RejectsUnsafeRemoveIdentifiers(t *testing.T) {
	test := func(name, uuid string) {
		t.Run(name, func(t *testing.T) {
			ts, count := newAnnouncingStore(t)
			pin := testPin(t, "safe-peer")
			require.NoError(t, ts.Pin(pin), "pin safe peer")
			victim := filepath.Join(filepath.Dir(ts.dir), "victim.json")
			require.NoError(t, os.WriteFile(victim, []byte("preserve me"), 0o600), "write victim")
			require.Error(t, ts.Remove(uuid), "unsafe removal succeeded")
			data, err := os.ReadFile(victim)
			require.NoError(t, err, "read victim")
			assert.Equal(t, "preserve me", string(data), "victim file changed")
			_, ok := ts.Get(pin.NodeUUID)
			assert.True(t, ok, "rejected removal removed the safe peer")
			assert.Len(t, ts.List(), 1, "rejected removal changed live state")
			assert.Equal(t, 1, count(), "rejected removal announced a change")
		})
	}
	test("empty principal", "")
	test("current directory", ".")
	test("parent directory", "..")
	test("Unix traversal", "../victim")
	test("Windows traversal", `..\victim`)
	test("nested Unix traversal", "sub/../../victim")
	test("nested Windows traversal", `sub\..\..\victim`)
	test("absolute path", filepath.Join(t.TempDir(), "victim"))
	test("drive relative path", "C:victim")
	test("UNC path", `\\server\share\victim`)
	test("alternate data stream", "peer:stream")
	test("NUL byte", "peer\x00victim")
	if runtime.GOOS == "windows" {
		test("Windows reserved device", "CON")
		test("Windows reserved serial port", "COM1")
		test("Windows reserved printer port", "LPT1")
	}
}

func TestTrustStoreLoad_SkipsUnsafePinIdentifiers(t *testing.T) {
	test := func(name, uuid string) {
		t.Run(name, func(t *testing.T) {
			ts, err := newTrustStore(t.TempDir())
			require.NoError(t, err, "open store")
			data, err := json.Marshal(pinWithCommonName(t, uuid))
			require.NoError(t, err, "encode pin")
			require.NoError(t, os.WriteFile(filepath.Join(ts.dir, uuid+".json"), data, 0o600), "write unsafe pin")
			reloaded, err := newTrustStore(filepath.Dir(ts.dir))
			require.NoError(t, err, "reload store")
			require.Empty(t, reloaded.List(), "unsafe pin was loaded")
		})
	}
	test("empty principal", "")
	test("current directory principal", ".")
	test("parent directory principal", "..")
}

func TestTrustStore_PreservesSafeOpaqueIdentifierAcrossReload(t *testing.T) {
	ts, err := newTrustStore(t.TempDir())
	require.NoError(t, err, "open store")
	pin := testPin(t, "peer.name-1")
	require.NoError(t, ts.Pin(pin), "pin safe opaque identifier")
	reloaded, err := newTrustStore(filepath.Dir(ts.dir))
	require.NoError(t, err, "reload store")
	got, ok := reloaded.Get(pin.NodeUUID)
	require.True(t, ok, "safe opaque identifier did not survive reload")
	require.Equal(t, pin.CertPem, got.CertPem, "safe opaque identifier's certificate changed")
	require.NoError(t, reloaded.Remove(pin.NodeUUID), "remove safe pin")
	require.NoError(t, reloaded.Remove(pin.NodeUUID), "repeat safe removal")
}

func TestMergeRoster_RejectsTraversalPrincipal(t *testing.T) {
	m := newTestManager(t)
	peerUUID, peerCert, peerFP, peerKey := makeNode(t, "trusted-peer")
	pinTrusted(t, m, peerUUID, peerCert, peerFP)
	identityPath := filepath.Join(filepath.Dir(m.trust.dir), "identity.json")
	before, err := os.ReadFile(identityPath)
	require.NoError(t, err, "read identity before merge")
	pin := pinWithCommonName(t, "../identity")
	fingerprint, err := certFingerprintFromPEM([]byte(pin.CertPem))
	require.NoError(t, err, "fingerprint crafted certificate")
	endorsement := signEndorsement(peerKey, peerUUID, pin.NodeUUID, fingerprint, "cluster-1", time.Now().UnixMilli(), 1, 1)
	m.mergeRoster(&Roster{
		ClusterID: "cluster-1",
		Members: []RosterEntry{{
			NodeUUID: pin.NodeUUID, CertPem: pin.CertPem, CertFingerprint: fingerprint,
			AdmissionEpoch: 1, Endorsements: []Endorsement{endorsement},
		}},
	}, peerUUID)
	_, ok := m.trust.Get(pin.NodeUUID)
	assert.False(t, ok, "trusted endorsement admitted a traversal principal")
	_, ok = m.memberByNodeID(pin.NodeUUID)
	assert.False(t, ok, "traversal principal became a member")
	after, err := os.ReadFile(identityPath)
	require.NoError(t, err, "read identity after merge")
	assert.Equal(t, before, after, "roster merge overwrote identity.json")
}

// newAnnouncingStore returns a trust store wired to count change announcements.
func newAnnouncingStore(t *testing.T) (*TrustStore, func() int) {
	t.Helper()
	ts, err := newTrustStore(t.TempDir())
	require.NoError(t, err, "new trust store")
	var count int
	ts.SetOnChange(func() { count++ })
	return ts, func() int { return count }
}

func testPin(t *testing.T, uuid string) *TrustedPin {
	t.Helper()
	certPEM, _, err := generateLeaf(uuid, uuid)
	require.NoError(t, err, "generate leaf")
	return &TrustedPin{
		NodeUUID:  uuid,
		NodeID:    uuid,
		Name:      uuid,
		ClusterID: "cluster-1",
		CertPem:   string(certPEM),
		PinnedAt:  time.Now().UnixMilli(),
	}
}

// TestTrustStoreAnnouncesEveryMutation is the load-bearing property of the
// event-driven design that replaced the periodic re-derive: every consumer that
// caches an answer derived from the pin set learns about a change only because
// this store says so. A mutation path that lands on disk without announcing
// leaves those consumers permanently wrong — which is exactly the failure the
// announcement exists to prevent — so the hook lives on the store rather than at
// the ~19 call sites that pin and unpin peers.
//
// Pinning, removing, and a display-name update mutate disk state; forgetting
// intentionally changes only live authorization. Each must announce once.
func TestTrustStore_AnnouncesEveryMutation(t *testing.T) {
	ts, count := newAnnouncingStore(t)
	const uuid = "principal-peer"

	require.NoError(t, ts.Pin(testPin(t, uuid)), "pin")
	require.Equal(t, 1, count(), "announcements after pin")

	ok, err := ts.UpdateIdentity(uuid, "renamed-host", "Renamed")
	require.NoError(t, err, "update identity: ok (%v, %v)", ok, err)
	require.True(t, ok, "update identity: ok (%v, %v)", ok, err)
	require.Equal(t, 2, count(), "announcements after rename")

	require.NoError(t, ts.Remove(uuid), "remove")
	require.Equal(t, 3, count(), "announcements after remove")

	require.NoError(t, ts.Pin(testPin(t, uuid)), "re-pin")
	ts.Forget(uuid)
	require.Equal(t, 5, count(), "announcements after re-pin + forget")
}

type storedEndorsementsSnapshot struct {
	live, persisted *TrustedPin
	reloadErr       error
}

// snapshotStoredEndorsements observes memory and disk at announcement time,
// including when the callback runs in a worker goroutine.
func snapshotStoredEndorsements(ts *TrustStore, uuid string) storedEndorsementsSnapshot {
	var snapshot storedEndorsementsSnapshot
	snapshot.live, _ = ts.Get(uuid)
	reloaded, err := newTrustStore(filepath.Dir(ts.dir))
	snapshot.reloadErr = err
	if err == nil {
		snapshot.persisted, _ = reloaded.Get(uuid)
	}
	return snapshot
}

// assertStoredEndorsements checks a captured snapshot in the test goroutine.
func assertStoredEndorsements(t *testing.T, snapshot storedEndorsementsSnapshot, want []Endorsement) {
	t.Helper()
	require.NotNil(t, snapshot.live, "live endorsements (%v)", want)
	assert.Equal(t, want, snapshot.live.Endorsements, "live endorsements")
	require.NoError(t, snapshot.reloadErr, "reload trust store")
	require.NotNil(t, snapshot.persisted, "reloaded endorsements (%v)", want)
	assert.Equal(t, want, snapshot.persisted.Endorsements, "reloaded endorsements")
}

type endorsementMerge func(*TrustStore, *TrustedPin, []Endorsement) error

func addEndorsements(ts *TrustStore, pin *TrustedPin, batch []Endorsement) error {
	return ts.AddEndorsements(pin.NodeUUID, batch)
}

func pinWithEndorsements(ts *TrustStore, pin *TrustedPin, batch []Endorsement) error {
	updated := *pin
	updated.Endorsements = batch
	return ts.Pin(&updated)
}

func TestTrustStore_AnnouncesNewEndorsementsAfterPersistence(t *testing.T) {
	test := func(name string, merge endorsementMerge) {
		t.Run(name, func(t *testing.T) {
			ts, _ := newAnnouncingStore(t)
			pin := testPin(t, "principal-peer")
			first := Endorsement{By: "trusted-peer", Sig: "signature-1"}
			second := Endorsement{By: "trusted-peer", SigV2: "signature-2"}
			pin.Endorsements = []Endorsement{first}
			require.NoError(t, ts.Pin(pin), "pin")
			want := []Endorsement{first, second}
			calls := 0
			ts.SetOnChange(func() {
				calls++
				// Acquiring Get's read lock here also witnesses that the
				// mutation lock was released before announcing the change.
				assertStoredEndorsements(t, snapshotStoredEndorsements(ts, pin.NodeUUID), want)
			})
			// Mix an existing endorsement, a new endorsement, and an
			// in-batch duplicate. One operation causes one announcement.
			batch := []Endorsement{first, second, second}
			require.NoError(t, merge(ts, pin, batch), "merge")
			require.Equal(t, 1, calls, "announcements after merge")
			assertStoredEndorsements(t, snapshotStoredEndorsements(ts, pin.NodeUUID), want)
			before, err := os.ReadFile(mustPinPath(t, ts, pin.NodeUUID))
			require.NoError(t, err)
			require.NoError(t, merge(ts, pin, batch), "duplicate merge")
			require.Equal(t, 1, calls, "announcements after duplicate merge")
			require.NoError(t, merge(ts, pin, nil), "empty merge")
			require.Equal(t, 1, calls, "announcements after empty merge")
			after, err := os.ReadFile(mustPinPath(t, ts, pin.NodeUUID))
			require.NoError(t, err, "read pin after no-op merges")
			assert.Equal(t, before, after, "no-op merges changed disk contents")
			assertStoredEndorsements(t, snapshotStoredEndorsements(ts, pin.NodeUUID), want)
		})
	}
	test("AddEndorsements", addEndorsements)
	test("IdenticalPin", pinWithEndorsements)
}

func TestTrustStore_StaysSilentWhenEndorsementWriteFails(t *testing.T) {
	test := func(name string, merge endorsementMerge) {
		t.Run(name, func(t *testing.T) {
			ts, count := newAnnouncingStore(t)
			pin := testPin(t, "principal-peer")
			first := Endorsement{By: "trusted-peer", Sig: "signature-1"}
			second := Endorsement{By: "trusted-peer", SigV2: "signature-2"}
			pin.Endorsements = []Endorsement{first}
			require.NoError(t, ts.Pin(pin), "pin")
			before, err := os.ReadFile(mustPinPath(t, ts, pin.NodeUUID))
			require.NoError(t, err)
			beforeCount := count()
			// Fail the final replace, after the temporary file was written.
			// The existing pin must remain intact on disk and in memory.
			originalRename := renameFile
			writeErr := errors.New("injected endorsement replace failure")
			renameFile = func(_, _ string) error { return writeErr }
			t.Cleanup(func() { renameFile = originalRename })
			require.ErrorIs(t, merge(ts, pin, []Endorsement{second}), writeErr, "merge error")
			require.Equal(t, beforeCount, count(), "announcements after failed write")
			after, err := os.ReadFile(mustPinPath(t, ts, pin.NodeUUID))
			require.NoError(t, err, "read pin after failed write")
			assert.Equal(t, before, after, "failed write changed disk contents")
			assertStoredEndorsements(t, snapshotStoredEndorsements(ts, pin.NodeUUID), []Endorsement{first})
			entries, err := os.ReadDir(ts.dir)
			require.NoError(t, err, "list trusted directory after failed write")
			require.Len(t, entries, 1, "failed write left temporary residue: entries")
			require.Equal(t, pin.NodeUUID+".json", entries[0].Name(), "failed write left temporary residue: entries (%v)", entries)
			renameFile = originalRename
			require.NoError(t, merge(ts, pin, []Endorsement{second}), "retry after storage recovery")
			require.Equal(t, beforeCount+1, count(), "announcements after retry")
			assertStoredEndorsements(t, snapshotStoredEndorsements(ts, pin.NodeUUID), []Endorsement{first, second})
		})
	}
	test("AddEndorsements", addEndorsements)
	test("IdenticalPin", pinWithEndorsements)
}

func TestTrustStore_AnnouncesConcurrentDuplicateEndorsementsOnce(t *testing.T) {
	ts, err := newTrustStore(t.TempDir())
	require.NoError(t, err)
	pin := testPin(t, "principal-peer")
	require.NoError(t, ts.Pin(pin))
	endorsement := Endorsement{By: "trusted-peer", SigV2: "signature-1"}
	want := []Endorsement{endorsement}
	var calls atomic.Int32
	var snapshotsMu sync.Mutex
	var snapshots []storedEndorsementsSnapshot
	ts.SetOnChange(func() {
		calls.Add(1)
		snapshot := snapshotStoredEndorsements(ts, pin.NodeUUID)
		snapshotsMu.Lock()
		snapshots = append(snapshots, snapshot)
		snapshotsMu.Unlock()
	})
	const workers = 16
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				updated := *pin
				updated.Endorsements = want
				errs <- ts.Pin(&updated)
			} else {
				errs <- ts.AddEndorsements(pin.NodeUUID, want)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err, "concurrent merge")
	}
	for _, snapshot := range snapshots {
		assertStoredEndorsements(t, snapshot, want)
	}
	assert.Equal(t, int32(1), calls.Load(), "announcements for concurrent identical submissions")
	assertStoredEndorsements(t, snapshotStoredEndorsements(ts, pin.NodeUUID), want)
}

func TestTrustStore_EmitsNothingWhenEndorsementTargetIsMissing(t *testing.T) {
	ts, count := newAnnouncingStore(t)
	require.NoError(t, ts.AddEndorsements("principal-stranger", []Endorsement{{By: "trusted-peer", SigV2: "signature-1"}}))
	require.Equal(t, 0, count(), "missing-target merge changed live state: announcements")
	require.Empty(t, ts.List(), "missing-target merge changed live state: announcements")
	entries, err := os.ReadDir(ts.dir)
	require.NoError(t, err, "missing-target merge changed disk state: entries (%v, %v)", entries, err)
	require.Empty(t, entries, "missing-target merge changed disk state: entries (%v, %v)", entries, err)
}

// TestTrustStoreStaysSilentWhenNothingChanged keeps the announcement meaningful.
// The scanner answers it by walking its whole directory, and the broker relays
// it, so a store that announced on every call — including the idempotent re-pin
// that pairing and roster gossip perform routinely — would turn steady-state
// reconciliation into a broadcast loop.
func TestTrustStore_StaysSilentWhenNothingChanged(t *testing.T) {
	ts, count := newAnnouncingStore(t)
	const uuid = "principal-peer"
	pin := testPin(t, uuid)

	require.NoError(t, ts.Pin(pin), "pin")
	before := count()

	// An identical re-pin folds in no new endorsements and rewrites nothing.
	// Reuse the exact pin: generating another fixture would mint a different
	// certificate and exercise the key-rotation rejection path instead.
	require.NoError(t, ts.Pin(pin), "identical re-pin")
	// An empty endorsement merge is also a no-op and must stay silent.
	require.NoError(t, ts.AddEndorsements(uuid, nil), "empty endorsement merge")
	// A rename to the values already stored changes nothing.
	ok, err := ts.UpdateIdentity(uuid, uuid, uuid)
	require.NoError(t, err, "no-op rename: ok (%v, %v)", ok, err)
	require.False(t, ok, "no-op rename: ok (%v, %v)", ok, err)
	// Removing a peer we do not hold is not a change.
	require.NoError(t, ts.Remove("principal-stranger"), "remove unknown")
	ts.Forget("principal-stranger")

	require.Equal(t, before, count(), "announcements")
}

func TestTrustStore_PersistsPinsAcrossReloadAndRemovesThem(t *testing.T) {
	dir := t.TempDir()
	ts, err := newTrustStore(dir)
	require.NoError(t, err, "open")
	pin := testPin(t, "11111111-1111-4111-8111-111111111111")
	require.NoError(t, ts.Pin(pin), "pin")
	der, ok := ts.DER(pin.NodeUUID)
	require.True(t, ok, "expected the pinned DER to match itself")
	require.True(t, ts.MatchDER(pin.NodeUUID, der), "expected the pinned DER to match itself")

	// Reopen: the pin must reload from disk.
	reopened, err := newTrustStore(dir)
	require.NoError(t, err, "reopen")
	_, ok = reopened.Get(pin.NodeUUID)
	require.True(t, ok, "pin did not survive reload")

	require.NoError(t, reopened.Remove(pin.NodeUUID), "remove")
	_, ok = reopened.Get(pin.NodeUUID)
	require.False(t, ok, "pin still present after removal")
}

func TestTrustStore_RejectsCertificateChangesForPinnedPeer(t *testing.T) {
	dir := t.TempDir()
	ts, _ := newTrustStore(dir)
	pin := testPin(t, "11111111-1111-4111-8111-111111111111")
	require.NoError(t, ts.Pin(pin), "pin")
	// Identical re-pin is an idempotent no-op.
	require.NoError(t, ts.Pin(pin), "identical re-pin should be a no-op")
	// A different cert for the same UUID is rejected.
	other := testPin(t, pin.NodeUUID)
	require.Error(t, ts.Pin(other), "expected re-pinning a different cert for the same UUID to fail")
}

func TestTrustStore_SkipsPinsWhoseFilenameDoesNotMatchIdentity(t *testing.T) {
	dir := t.TempDir()
	// Pre-create a tampered file: filename UUID != the inner nodeUuid / cert.
	trustedDir := filepath.Join(dir, "trusted")
	require.NoError(t, os.MkdirAll(trustedDir, 0o700), "mkdir")
	pin := testPin(t, "11111111-1111-4111-8111-111111111111")
	data, _ := json.MarshalIndent(pin, "", "  ")
	wrongName := filepath.Join(trustedDir, "00000000-0000-4000-8000-000000000000.json")
	require.NoError(t, os.WriteFile(wrongName, data, 0o600), "write tampered")

	ts, err := newTrustStore(dir)
	require.NoError(t, err, "open")
	_, ok := ts.Get(pin.NodeUUID)
	require.False(t, ok, "tampered (renamed) pin should have been skipped on load")
	require.Empty(t, ts.List(), "expected no valid pins")
}
