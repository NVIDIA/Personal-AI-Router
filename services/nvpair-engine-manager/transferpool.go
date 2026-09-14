// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strconv"
	"sync"
)

// Concurrent model transfer, and a resume that skips what is already here.
//
// Concurrency is worth about 1.7x on Wi-Fi (see defaultCopyStreams for the
// measured curve). The resume matters at least as much: an 11 GB copy that dies
// at 90% used to start again from zero.
//
// Nothing about integrity changes. Each file is fetched, verified against the
// digest the manifest named, and renamed into place on its own; workers share no
// state but the progress counter.

// defaultCopyStreams is the concurrency for a model transfer.
//
// Four is the knee of the measured curve, not a guess. Between two Apple Silicon
// laptops on 802.11ac 80MHz at -25/-34 dBm, over an otherwise idle link:
//
//	1 stream    24 MiB/s
//	4 streams   40 MiB/s   <- knee
//	8 streams   40 MiB/s
//	16 streams  37 MiB/s
//
// One TCP flow leaves a third of the link unused: its window is bounded by
// round-trip time and halved by every loss, so the radio idles waiting for ACKs.
// Independent flows do not stall together, which recovers most of that. Past
// four there is nothing left to recover and the connection count starts costing
// more in contention than it returns.
//
// Measure before changing this. An earlier reading of the same link said
// concurrency HURT (1 stream 4 MiB/s, 4 streams 3 MiB/s) -- taken while an 11 GB
// transfer was saturating the link underneath the benchmark. A contended link
// makes every stream count look equally bad and inverts the conclusion.
//
// NVPAIR_MODEL_COPY_STREAMS overrides it for a link with a different shape.
const defaultCopyStreams = 4

// maxCopyStreams caps what the environment can ask for. Past this the connection
// count costs more in contention on both ends than it recovers in throughput,
// and a typo should not open hundreds of streams against a peer.
const maxCopyStreams = 32

func copyStreams() int {
	if raw := os.Getenv("NVPAIR_MODEL_COPY_STREAMS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 1 && n <= maxCopyStreams {
			return n
		}
	}
	return defaultCopyStreams
}

// runTransfers fetches every item with a bounded pool, calling onDone for each
// success. It returns the first error and abandons the rest.
//
// The first failure cancels the shared context so in-flight streams stop pulling
// bytes for a transfer that is already lost, rather than running to completion
// against a peer that has gone away.
func runTransfers(
	ctx context.Context,
	items []cacheFile,
	fetch func(context.Context, cacheFile) error,
	onDone func(cacheFile),
) error {
	if len(items) == 0 {
		return nil
	}
	workers := copyStreams()
	if workers > len(items) {
		workers = len(items)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	work := make(chan cacheFile)
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range work {
				if ctx.Err() != nil {
					return
				}
				if err := fetch(ctx, f); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					cancel()
					return
				}
				mu.Lock()
				onDone(f)
				mu.Unlock()
			}
		}()
	}

	for _, f := range items {
		select {
		case work <- f:
		case <-ctx.Done():
			// A worker failed; stop feeding and let the rest drain.
		}
	}
	close(work)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if firstErr != nil {
		return firstErr
	}
	// A cancelled parent (the user stopped the copy) is not a transfer failure
	// the caller should report as corruption, but it is still not success.
	return ctx.Err()
}

// fileAlreadyPresent reports whether dest already holds exactly the bytes the
// manifest describes, so a re-run after a failed transfer can skip it.
//
// Size is checked first because it is free and rules out almost everything; the
// digest is only computed for a file that could plausibly be the right one.
// Reading a local file to avoid re-fetching it over the network is a good trade
// at any link speed.
func fileAlreadyPresent(dest string, size int64, wantOID string) bool {
	info, err := os.Stat(dest)
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return false
	}
	f, err := os.Open(dest)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == wantOID
}
