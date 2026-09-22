// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// ollamapull.go is Ollama's half of partial-file cleanup: naming the candidate
// files and deciding which of them this pull created. The safety test they are
// then put through is engine-neutral and lives in partials.go; lmspull.go is
// the LM Studio counterpart of this file.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var ollamaDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// ollamaPartialsBefore records the partial blobs already on disk when a
// download started. A nil map means no snapshot was taken, so nothing is
// attributable to that download and cancelling it deletes nothing.
type ollamaPartialsBefore map[string]bool

// ollamaPartialSnapshot lists the partial blobs present before a download
// starts, which is what decides ownership at cancellation.
//
// A blob is content-addressed, so a digest names shared content rather than
// ownership: Ollama coalesces a pull of the same layer — from its CLI, its
// desktop app, the TUI's own engine-manager, or any other client on the daemon
// — onto the very same partial file. A partial that was already there is
// therefore not this download's to remove. It is either another client's
// transfer, which may sit still for far longer than any settle window while it
// waits on a slow server, or one PAIR deliberately kept when an earlier
// attempt's context died without a cancel request. Only the files this pull
// creates are unambiguously its own.
func ollamaPartialSnapshot(root string) (ollamaPartialsBefore, error) {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return ollamaPartialsBefore{}, nil
	}
	if err != nil {
		return nil, err
	}
	before := make(ollamaPartialsBefore, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.Contains(entry.Name(), "-partial") {
			before[filepath.Join(root, entry.Name())] = true
		}
	}
	return before, nil
}

// cleanupOllamaPartials removes the partial blobs this pull both reported and
// created. Completed blobs may be shared by installed models, and a partial
// that predates this download belongs to someone else; both are deliberately
// retained. A file this pull created can still be shared — another client can
// coalesce onto it afterwards — so one that is still moving is left alone and
// reported through busy, letting the caller look again.
func cleanupOllamaPartials(
	ctx context.Context,
	root string,
	digests map[string]bool,
	before ollamaPartialsBefore,
) (busy bool, err error) {
	if before == nil {
		return false, nil
	}
	candidates, err := ollamaPartialPaths(root, digests)
	if err != nil || len(candidates) == 0 {
		return false, err
	}
	created := make([]string, 0, len(candidates))
	for _, path := range candidates {
		if !before[path] {
			created = append(created, path)
		}
	}
	stable, busy := quiescentPaths(ctx, created)
	for _, candidate := range stable {
		removed, err := removeIfUnchanged(candidate)
		if err != nil {
			return true, err
		}
		if !removed {
			// Claimed between the last observation and the unlink. Leave it
			// and let the retry decide.
			busy = true
		}
	}
	return busy, nil
}

// ollamaPartialPaths lists the blob files matching any digest this pull
// reported. The directory holds one entry per layer of every installed model,
// so it is read once rather than per digest.
func ollamaPartialPaths(root string, digests map[string]bool) ([]string, error) {
	prefixes := make([]string, 0, len(digests))
	for digest := range digests {
		if ollamaDigestPattern.MatchString(digest) {
			prefixes = append(prefixes, strings.ReplaceAll(digest, ":", "-")+"-partial")
		}
	}
	if len(prefixes) == 0 {
		return nil, nil
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		for _, prefix := range prefixes {
			if entry.Name() == prefix || strings.HasPrefix(entry.Name(), prefix+"-") {
				paths = append(paths, filepath.Join(root, entry.Name()))
				break
			}
		}
	}
	return paths, nil
}

func ollamaBlobsDir(st *engineState) string {
	root := st.plat.Runtime.Env["OLLAMA_MODELS"]
	if root == "" {
		root = os.Getenv("OLLAMA_MODELS")
	}
	if root == "" {
		root = "~/.ollama/models"
	}
	return filepath.Join(expandPath(root), "blobs")
}

// partialCleanupRetryInterval spaces the cleanup passes after a cancellation
// and partialCleanupBudget bounds them. Ollama releases its background blob
// writers asynchronously once the pull request closes, so a Windows sharing
// violation and a file that has not stopped moving yet both tend to resolve
// within a few passes. The budget holds several full settle windows, and a file
// still busy when it runs out belongs to someone else.
const (
	partialCleanupRetryInterval = 100 * time.Millisecond
	partialCleanupBudget        = 5 * time.Second
)

// cleanupOllamaAfterCancel retries until every partial this pull created is
// removed or confirmed to be another client's. One still busy at the deadline
// is left in place; that is the only safe outcome, and is not a cleanup failure.
func cleanupOllamaAfterCancel(
	ctx context.Context,
	root string,
	digests map[string]bool,
	before ollamaPartialsBefore,
) error {
	deadline := time.Now().Add(partialCleanupBudget)
	for {
		time.Sleep(partialCleanupRetryInterval)
		busy, err := cleanupOllamaPartials(ctx, root, digests, before)
		if err == nil && !busy {
			return nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				return nil
			}
			return fmt.Errorf("download stopped, but partial-file cleanup failed: %w", err)
		}
	}
}
