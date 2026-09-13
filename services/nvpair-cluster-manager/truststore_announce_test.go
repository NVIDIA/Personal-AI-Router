// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
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
// Pinning, removing, and a display-name update mutate disk state; forgetting
// intentionally changes only live authorization. Each must announce once.
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

// assertStoredEndorsements checks the live snapshot and a separately loaded
// store. It is also safe to call from an onChange callback in a joined worker.
func assertStoredEndorsements(t *testing.T, ts *TrustStore, uuid string, want []Endorsement) {
	t.Helper()
	pin, ok := ts.Get(uuid)
	if !ok || !reflect.DeepEqual(pin.Endorsements, want) {
		t.Errorf("live endorsements = %+v, want %+v", pin, want)
	}
	reloaded, err := newTrustStore(filepath.Dir(ts.dir))
	if err != nil {
		t.Errorf("reload trust store: %v", err)
		return
	}
	pin, ok = reloaded.Get(uuid)
	if !ok || !reflect.DeepEqual(pin.Endorsements, want) {
		t.Errorf("reloaded endorsements = %+v, want %+v", pin, want)
	}
}

func TestTrustStoreAnnouncesNewEndorsementsAfterPersistence(t *testing.T) {
	for _, identicalPin := range []bool{false, true} {
		name := "AddEndorsements"
		if identicalPin {
			name = "IdenticalPin"
		}
		t.Run(name, func(t *testing.T) {
			ts, _ := newAnnouncingStore(t)
			pin := testPin(t, "principal-peer")
			first := Endorsement{By: "trusted-peer", Sig: "signature-1"}
			second := Endorsement{By: "trusted-peer", SigV2: "signature-2"}
			pin.Endorsements = []Endorsement{first}
			if err := ts.Pin(pin); err != nil {
				t.Fatalf("pin: %v", err)
			}
			want := []Endorsement{first, second}
			calls := 0
			ts.SetOnChange(func() {
				calls++
				// Acquiring Get's read lock here also witnesses that the
				// mutation lock was released before announcing the change.
				assertStoredEndorsements(t, ts, pin.NodeUUID, want)
			})
			merge := func(batch []Endorsement) error {
				if identicalPin {
					updated := *pin
					updated.Endorsements = batch
					return ts.Pin(&updated)
				}
				return ts.AddEndorsements(pin.NodeUUID, batch)
			}
			// Mix an existing endorsement, a new endorsement, and an
			// in-batch duplicate. One operation causes one announcement.
			batch := []Endorsement{first, second, second}
			if err := merge(batch); err != nil {
				t.Fatalf("merge: %v", err)
			}
			if calls != 1 {
				t.Fatalf("announcements after merge = %d, want 1", calls)
			}
			assertStoredEndorsements(t, ts, pin.NodeUUID, want)
			before, err := os.ReadFile(ts.pinPath(pin.NodeUUID))
			if err != nil {
				t.Fatal(err)
			}
			for _, noOp := range [][]Endorsement{batch, nil} {
				if err := merge(noOp); err != nil {
					t.Fatalf("no-op merge: %v", err)
				}
				if calls != 1 {
					t.Fatalf("announcements after no-op = %d, want 1", calls)
				}
			}
			after, err := os.ReadFile(ts.pinPath(pin.NodeUUID))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("no-op changed disk contents: %v", err)
			}
			assertStoredEndorsements(t, ts, pin.NodeUUID, want)
		})
	}
}

func TestTrustStoreStaysSilentWhenEndorsementWriteFails(t *testing.T) {
	for _, identicalPin := range []bool{false, true} {
		name := "AddEndorsements"
		if identicalPin {
			name = "IdenticalPin"
		}
		t.Run(name, func(t *testing.T) {
			ts, count := newAnnouncingStore(t)
			pin := testPin(t, "principal-peer")
			first := Endorsement{By: "trusted-peer", Sig: "signature-1"}
			second := Endorsement{By: "trusted-peer", SigV2: "signature-2"}
			pin.Endorsements = []Endorsement{first}
			if err := ts.Pin(pin); err != nil {
				t.Fatalf("pin: %v", err)
			}
			before, err := os.ReadFile(ts.pinPath(pin.NodeUUID))
			if err != nil {
				t.Fatal(err)
			}
			beforeCount := count()
			// Fail the final replace, after the temporary file was written.
			// The existing pin must remain intact on disk and in memory.
			originalRename := renameFile
			writeErr := errors.New("injected endorsement replace failure")
			renameFile = func(_, _ string) error { return writeErr }
			t.Cleanup(func() { renameFile = originalRename })
			merge := func() error {
				if identicalPin {
					updated := *pin
					updated.Endorsements = []Endorsement{second}
					return ts.Pin(&updated)
				}
				return ts.AddEndorsements(pin.NodeUUID, []Endorsement{second})
			}
			if err := merge(); !errors.Is(err, writeErr) {
				t.Fatalf("merge error = %v, want injected replace failure", err)
			}
			if count() != beforeCount {
				t.Fatalf("announcements after failed write = %d, want %d", count(), beforeCount)
			}
			after, err := os.ReadFile(ts.pinPath(pin.NodeUUID))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("failed write changed disk contents: %v", err)
			}
			assertStoredEndorsements(t, ts, pin.NodeUUID, []Endorsement{first})
			entries, err := os.ReadDir(ts.dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != pin.NodeUUID+".json" {
				t.Fatalf("failed write left temporary residue: entries=%v err=%v", entries, err)
			}
			renameFile = originalRename
			if err := merge(); err != nil {
				t.Fatalf("retry after storage recovery: %v", err)
			}
			if count() != beforeCount+1 {
				t.Fatalf("announcements after retry = %d, want %d", count(), beforeCount+1)
			}
			assertStoredEndorsements(t, ts, pin.NodeUUID, []Endorsement{first, second})
		})
	}
}

func TestTrustStoreConcurrentDuplicateEndorsementsAnnounceOnce(t *testing.T) {
	ts, err := newTrustStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pin := testPin(t, "principal-peer")
	if err := ts.Pin(pin); err != nil {
		t.Fatal(err)
	}
	endorsement := Endorsement{By: "trusted-peer", SigV2: "signature-1"}
	want := []Endorsement{endorsement}
	var calls atomic.Int32
	ts.SetOnChange(func() {
		calls.Add(1)
		assertStoredEndorsements(t, ts, pin.NodeUUID, want)
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
		if err != nil {
			t.Errorf("concurrent merge: %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("announcements for concurrent identical submissions = %d, want 1", got)
	}
	assertStoredEndorsements(t, ts, pin.NodeUUID, want)
}

func TestTrustStoreMissingEndorsementTargetStaysSilent(t *testing.T) {
	ts, count := newAnnouncingStore(t)
	if err := ts.AddEndorsements("principal-stranger", []Endorsement{{By: "trusted-peer", SigV2: "signature-1"}}); err != nil {
		t.Fatal(err)
	}
	if count() != 0 || len(ts.List()) != 0 {
		t.Fatalf("missing-target merge changed live state: announcements=%d pins=%v", count(), ts.List())
	}
	entries, err := os.ReadDir(ts.dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("missing-target merge changed disk state: entries=%v err=%v", entries, err)
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
