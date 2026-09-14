// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func files(n int) []cacheFile {
	out := make([]cacheFile, n)
	for i := range out {
		out[i] = cacheFile{Path: "f" + strconv.Itoa(i), OID: "oid" + strconv.Itoa(i)}
	}
	return out
}

// Every file must move exactly once. A transfer that drops one leaves a model
// that looks complete and cannot load; one that fetches twice wastes the link
// this change exists to use better.
func TestRunTransfersFetchesEveryItemExactlyOnce(t *testing.T) {
	t.Setenv("NVPAIR_MODEL_COPY_STREAMS", "8")
	items := files(50)

	var mu sync.Mutex
	fetched := map[string]int{}
	var doneCount int64

	err := runTransfers(context.Background(), items,
		func(_ context.Context, f cacheFile) error {
			mu.Lock()
			fetched[f.Path]++
			mu.Unlock()
			return nil
		},
		func(cacheFile) { atomic.AddInt64(&doneCount, 1) })
	if err != nil {
		t.Fatalf("runTransfers: %v", err)
	}
	if len(fetched) != len(items) {
		t.Fatalf("fetched %d distinct files, want %d", len(fetched), len(items))
	}
	for path, n := range fetched {
		if n != 1 {
			t.Errorf("%s fetched %d times, want 1", path, n)
		}
	}
	if doneCount != int64(len(items)) {
		t.Errorf("progress reported %d times, want %d", doneCount, len(items))
	}
}

// The whole point of the change: work has to actually overlap.
func TestRunTransfersRunsConcurrently(t *testing.T) {
	t.Setenv("NVPAIR_MODEL_COPY_STREAMS", "8")
	var inFlight, peak int64

	err := runTransfers(context.Background(), files(32),
		func(_ context.Context, _ cacheFile) error {
			n := atomic.AddInt64(&inFlight, 1)
			for {
				old := atomic.LoadInt64(&peak)
				if n <= old || atomic.CompareAndSwapInt64(&peak, old, n) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt64(&inFlight, -1)
			return nil
		},
		func(cacheFile) {})
	if err != nil {
		t.Fatalf("runTransfers: %v", err)
	}
	if peak < 2 {
		t.Fatalf("peak concurrency was %d; transfers ran serially", peak)
	}
}

// A failure has to surface and stop the rest: continuing to pull gigabytes for a
// transfer that is already lost is exactly what the cancel is for.
func TestRunTransfersReturnsFirstErrorAndStops(t *testing.T) {
	t.Setenv("NVPAIR_MODEL_COPY_STREAMS", "4")
	boom := errors.New("peer went away")
	var started int64

	err := runTransfers(context.Background(), files(200),
		func(ctx context.Context, f cacheFile) error {
			atomic.AddInt64(&started, 1)
			if f.Path == "f0" {
				return boom
			}
			// Give the failing worker time to cancel before this one finishes.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Millisecond):
				return nil
			}
		},
		func(cacheFile) {})

	if !errors.Is(err, boom) {
		t.Fatalf("runTransfers returned %v, want the fetch error", err)
	}
	if n := atomic.LoadInt64(&started); n >= 200 {
		t.Errorf("started %d of 200 transfers after a failure; the rest should be abandoned", n)
	}
}

func TestRunTransfersEmptyIsNotAnError(t *testing.T) {
	if err := runTransfers(context.Background(), nil,
		func(context.Context, cacheFile) error { return errors.New("must not be called") },
		func(cacheFile) {}); err != nil {
		t.Fatalf("empty transfer returned %v", err)
	}
}

func TestCopyStreamsClampsTheEnvironment(t *testing.T) {
	for _, tc := range []struct {
		set  string
		want int
	}{
		{"", defaultCopyStreams},
		{"16", 16},
		{"1", 1},
		{"0", defaultCopyStreams},
		{"-4", defaultCopyStreams},
		{"9999", defaultCopyStreams},
		{"lots", defaultCopyStreams},
	} {
		t.Setenv("NVPAIR_MODEL_COPY_STREAMS", tc.set)
		if tc.set == "" {
			os.Unsetenv("NVPAIR_MODEL_COPY_STREAMS")
		}
		if got := copyStreams(); got != tc.want {
			t.Errorf("copyStreams() with %q = %d, want %d", tc.set, got, tc.want)
		}
	}
}

// Resume: an 11 GB transfer that dies partway must not start from zero.
func TestFileAlreadyPresentOnlyAcceptsAnExactMatch(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MLX_MODELS_DIRS", root)
	src := mkModel(t, root, "m", map[string]string{"model.safetensors": "the real weights"})
	m, err := readDirManifest(src)
	if err != nil {
		t.Fatal(err)
	}
	f := m.Files[0]
	dest := filepath.Join(src, filepath.FromSlash(f.Path))

	if !fileAlreadyPresent(dest, m.Sizes[f.OID], f.OID) {
		t.Error("an identical file was not recognised; the transfer would refetch it")
	}
	if fileAlreadyPresent(dest, m.Sizes[f.OID], "0000000000000000000000000000000000000000000000000000000000000000") {
		t.Error("a file with the wrong digest was accepted as already present")
	}
	if fileAlreadyPresent(dest, m.Sizes[f.OID]+1, f.OID) {
		t.Error("a file with the wrong size was accepted as already present")
	}
	if fileAlreadyPresent(filepath.Join(src, "absent"), 1, f.OID) {
		t.Error("a missing file was reported as present")
	}
	// A directory must never be mistaken for a completed file.
	if fileAlreadyPresent(src, m.Sizes[f.OID], f.OID) {
		t.Error("a directory was reported as a present file")
	}
}
