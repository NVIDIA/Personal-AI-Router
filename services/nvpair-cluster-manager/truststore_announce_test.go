// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newAnnouncingStore returns a trust store wired to count change announcements.
func newAnnouncingStore(t *testing.T) (*TrustStore, func() int) {
	t.Helper()
	ts, err := newTrustStore(t.TempDir())
	if err != nil {
		t.Fatalf("new trust store: %v", err)
	}
	var count int
	ts.SetOnChange(func() { count++ })
	return ts, func() int { return count }
}

func testPin(t *testing.T, uuid string) *TrustedPin {
	t.Helper()
	certPEM, _, err := generateLeaf(uuid, uuid)
	if err != nil {
		t.Fatalf("generate leaf: %v", err)
	}
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
// Pinning, removing, forgetting, and a display-name update all mutate what is on
// disk and must each announce exactly once.
func TestTrustStoreAnnouncesEveryMutation(t *testing.T) {
	ts, count := newAnnouncingStore(t)
	const uuid = "principal-peer"

	if err := ts.Pin(testPin(t, uuid)); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if count() != 1 {
		t.Fatalf("announcements after pin = %d, want 1", count())
	}

	if ok, err := ts.UpdateIdentity(uuid, "renamed-host", "Renamed"); err != nil || !ok {
		t.Fatalf("update identity: ok=%v err=%v", ok, err)
	}
	if count() != 2 {
		t.Fatalf("announcements after rename = %d, want 2", count())
	}

	if err := ts.Remove(uuid); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if count() != 3 {
		t.Fatalf("announcements after remove = %d, want 3", count())
	}

	if err := ts.Pin(testPin(t, uuid)); err != nil {
		t.Fatalf("re-pin: %v", err)
	}
	ts.Forget(uuid)
	if count() != 5 {
		t.Fatalf("announcements after re-pin + forget = %d, want 5", count())
	}
}

func TestTrustStoreAnnouncesNewEndorsementOnce(t *testing.T) {
	ts, count := newAnnouncingStore(t)
	const uuid = "principal-peer"

	if err := ts.Pin(testPin(t, uuid)); err != nil {
		t.Fatalf("pin: %v", err)
	}
	endorsement := Endorsement{
		By:          "trusted-peer",
		Fingerprint: "sha256:target",
		ClusterID:   "cluster-1",
		IssuedAt:    1,
		Sig:         "signature-1",
	}

	if err := ts.AddEndorsements(uuid, []Endorsement{endorsement}); err != nil {
		t.Fatalf("add endorsement: %v", err)
	}
	if count() != 2 {
		t.Fatalf("announcements after new endorsement = %d, want 2", count())
	}
	stored, ok := ts.Get(uuid)
	if !ok || len(stored.Endorsements) != 1 || stored.Endorsements[0] != endorsement {
		t.Fatalf("endorsement was not persisted: %+v", stored)
	}

	if err := ts.AddEndorsements(uuid, []Endorsement{endorsement}); err != nil {
		t.Fatalf("repeat endorsement: %v", err)
	}
	if count() != 2 {
		t.Fatalf("announcements after duplicate endorsement = %d, want 2", count())
	}
	stored, ok = ts.Get(uuid)
	if !ok || len(stored.Endorsements) != 1 {
		t.Fatalf("duplicate endorsement changed persisted state: %+v", stored)
	}
}

func TestTrustStoreAnnouncesNewEndorsementOnIdenticalPinOnce(t *testing.T) {
	ts, count := newAnnouncingStore(t)
	const uuid = "principal-peer"
	pin := testPin(t, uuid)

	if err := ts.Pin(pin); err != nil {
		t.Fatalf("pin: %v", err)
	}
	endorsement := Endorsement{
		By:          "trusted-peer",
		Fingerprint: "sha256:target",
		ClusterID:   "cluster-1",
		IssuedAt:    1,
		Sig:         "signature-1",
	}
	withEndorsement := *pin
	withEndorsement.Endorsements = []Endorsement{endorsement}

	if err := ts.Pin(&withEndorsement); err != nil {
		t.Fatalf("re-pin with new endorsement: %v", err)
	}
	if count() != 2 {
		t.Fatalf("announcements after new endorsement on identical pin = %d, want 2", count())
	}
	stored, ok := ts.Get(uuid)
	if !ok || len(stored.Endorsements) != 1 || stored.Endorsements[0] != endorsement {
		t.Fatalf("endorsement was not persisted through identical pin: %+v", stored)
	}

	if err := ts.Pin(&withEndorsement); err != nil {
		t.Fatalf("repeat re-pin with endorsement: %v", err)
	}
	if count() != 2 {
		t.Fatalf("announcements after duplicate endorsement on identical pin = %d, want 2", count())
	}
}

func TestTrustStoreStaysSilentWhenEndorsementWriteFails(t *testing.T) {
	ts, count := newAnnouncingStore(t)
	const uuid = "principal-peer"
	if err := ts.Pin(testPin(t, uuid)); err != nil {
		t.Fatalf("pin: %v", err)
	}
	beforeCount := count()

	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("create persistence blocker: %v", err)
	}
	ts.dir = blocker
	endorsement := Endorsement{
		By:          "trusted-peer",
		Fingerprint: "sha256:target",
		ClusterID:   "cluster-1",
		IssuedAt:    1,
		Sig:         "signature-1",
	}
	if err := ts.AddEndorsements(uuid, []Endorsement{endorsement}); err == nil {
		t.Fatal("failed endorsement write unexpectedly succeeded")
	}
	if count() != beforeCount {
		t.Fatalf("announcements after failed endorsement write = %d, want %d", count(), beforeCount)
	}
	stored, ok := ts.Get(uuid)
	if !ok || len(stored.Endorsements) != 0 {
		t.Fatalf("failed endorsement write mutated live state: %+v", stored)
	}
}

// TestTrustStoreStaysSilentWhenNothingChanged keeps the announcement meaningful.
// The scanner answers it by walking its whole directory, and the broker relays
// it, so a store that announced on every call — including the idempotent re-pin
// that pairing and roster gossip perform routinely — would turn steady-state
// reconciliation into a broadcast loop.
func TestTrustStoreStaysSilentWhenNothingChanged(t *testing.T) {
	ts, count := newAnnouncingStore(t)
	const uuid = "principal-peer"
	pin := testPin(t, uuid)

	if err := ts.Pin(pin); err != nil {
		t.Fatalf("pin: %v", err)
	}
	before := count()

	// An identical re-pin folds in no new endorsements and rewrites nothing.
	// Reuse the exact pin: generating another fixture would mint a different
	// certificate and exercise the key-rotation rejection path instead.
	if err := ts.Pin(pin); err != nil {
		t.Fatalf("identical re-pin: %v", err)
	}
	// An empty endorsement merge is also a no-op and must stay silent.
	if err := ts.AddEndorsements(uuid, nil); err != nil {
		t.Fatalf("empty endorsement merge: %v", err)
	}
	// A rename to the values already stored changes nothing.
	if ok, err := ts.UpdateIdentity(uuid, uuid, uuid); err != nil || ok {
		t.Fatalf("no-op rename: ok=%v err=%v, want false/nil", ok, err)
	}
	// Removing a peer we do not hold is not a change.
	if err := ts.Remove("principal-stranger"); err != nil {
		t.Fatalf("remove unknown: %v", err)
	}
	ts.Forget("principal-stranger")

	if count() != before {
		t.Fatalf("announcements = %d, want %d — a no-op must stay silent", count(), before)
	}
}
