// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// ollamapull.go is Ollama's half of partial-file cleanup: naming the candidate
// files and deciding which of them this pull created. The safety test they are
// then put through is engine-neutral and lives in partials.go; lmspull.go is
// the LM Studio counterpart of this file.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
)

var ollamaDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// ollamaPartialsBefore records the partial blobs already on disk when a
// download started. A nil map means no snapshot was taken, so nothing is
// attributable to that download and cancelling it deletes nothing.
type ollamaPartialsBefore map[string]bool

// has reports whether the snapshot held any file of the blob whose data file
// is name: the data file itself or one of its resume files, whether or not
// that file is still on disk.
func (b ollamaPartialsBefore) has(name string) bool {
	for path := range b {
		if path == name || strings.HasPrefix(path, name+"-") {
			return true
		}
	}
	return false
}

// ollamaPartialSnapshot lists the partial blobs present before a download
// starts, which is what decides ownership at cancellation.
//
// A blob is content-addressed, so a digest names shared content rather than
// ownership: Ollama coalesces a pull of the same layer — from its CLI, its
// desktop app, the TUI's own engine-manager, or any other client on the daemon
// — onto the very same partial file. A partial that was already there is
// therefore not this download's to remove. It is either another client's
// transfer, which may sit still for far longer than any quiet period while it
// waits on a slow server, or one PAIR deliberately kept when an earlier
// attempt's context died without a cancel request. Only the files this pull
// creates are unambiguously its own.
//
// An empty root is a blobs directory that could not be determined, and is
// refused rather than read: the current directory is not Ollama's cache.
func ollamaPartialSnapshot(root string) (ollamaPartialsBefore, error) {
	if root == "" {
		return nil, errors.New("the Ollama blobs directory is unknown")
	}
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

// cleanupOllamaAfterCancel removes the partial blobs this pull created once
// nothing is writing them. A file this pull created can still be shared —
// another client can coalesce onto it afterwards — so the removal waits for it
// to hold still; see removeQuiescentPartials. Without a snapshot nothing is
// attributable to the pull, so nothing is removed.
func cleanupOllamaAfterCancel(
	ctx context.Context,
	root string,
	digests map[string]bool,
	before ollamaPartialsBefore,
) error {
	if before == nil {
		return nil
	}
	return removeQuiescentPartials(ctx, func() ([]partialGroup, error) {
		return ollamaCreatedPartials(root, digests, before)
	})
}

// ollamaCreatedPartials lists the partial blobs this pull both reported and
// created. Completed blobs may be shared by installed models, and a partial
// that predates this download belongs to someone else, along with every
// resume file beside it; all are deliberately left out. A blob stays left out
// once the files the snapshot saw are gone: Ollama removes a blob's resume
// files as it finishes the blob, just before renaming its data file into
// place.
func ollamaCreatedPartials(root string, digests map[string]bool, before ollamaPartialsBefore) ([]partialGroup, error) {
	groups, err := ollamaPartialGroups(root, digests)
	if err != nil {
		return nil, err
	}
	created := make([]partialGroup, 0, len(groups))
	for _, group := range groups {
		if !before.has(group.name) {
			created = append(created, group)
		}
	}
	return created, nil
}

// ollamaPartialGroups lists the partial files of every blob this pull
// reported, one group per blob: the data file "<blob>-partial" and the resume
// files beside it, "<blob>-partial-N", one per chunk. Ollama gives up on
// finishing a blob whose resume files are not all there, so a blob's files
// are kept or removed together, resume files first: a data file left on its
// own is a fresh download to Ollama, while resume files left without their
// data file describe bytes that are gone. The directory holds one entry per
// layer of every installed model, so it is read once rather than per digest.
func ollamaPartialGroups(root string, digests map[string]bool) ([]partialGroup, error) {
	prefixes := make([]string, 0, len(digests))
	for digest := range digests {
		if ollamaDigestPattern.MatchString(digest) {
			prefixes = append(prefixes, strings.ReplaceAll(digest, ":", "-")+"-partial")
		}
	}
	if len(prefixes) == 0 {
		return nil, nil
	}
	slices.Sort(prefixes)
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	resume := make(map[string][]string, len(prefixes))
	data := make(map[string]bool, len(prefixes))
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		for _, prefix := range prefixes {
			if entry.Name() == prefix {
				data[prefix] = true
				break
			}
			if strings.HasPrefix(entry.Name(), prefix+"-") {
				resume[prefix] = append(resume[prefix], filepath.Join(root, entry.Name()))
				break
			}
		}
	}
	var groups []partialGroup
	for _, prefix := range prefixes {
		name := filepath.Join(root, prefix)
		files := resume[prefix]
		if data[prefix] {
			files = append(files, name)
		}
		if len(files) > 0 {
			groups = append(groups, partialGroup{name: name, files: files})
		}
	}
	return groups, nil
}

// ollamaBlobsDir is where the Ollama daemon writes blobs: under the
// OLLAMA_MODELS of the environment it runs in, or else Ollama's own default,
// .ollama/models in the home folder that same environment names. Empty when
// that cannot be known, which leaves cleanup nothing to attribute and so
// nothing to delete.
//
// A daemon this engine manager launched runs in the environment it was handed —
// the one the engine manager inherited, then the manifest's env, then the
// user's launch overrides, with the last assignment of a name winning — so that
// is read back from the process rather than rebuilt from settings that may have
// changed since. Both settings come from it: overriding the home folder moves
// Ollama's default as surely as overriding OLLAMA_MODELS does, and an
// environment that names no home folder leaves no default to look in. A daemon
// that was already running when PAIR adopted it was started by someone else,
// in an environment PAIR cannot see. The engine manager's own environment is
// the best guess there. A wrong guess usually finds nothing, since no partial
// of this pull's layers appears there after the snapshot. It does when the
// guess lands on another live Ollama cache, such as a second daemon's pulling
// the same layers: a partial that daemon creates after the snapshot, and
// leaves still for the quiet period, reads as this pull's own and is removed
// with the progress it held.
//
// The value is read the way Ollama reads it: surrounding spaces and quotes are
// trimmed, and "~" is not expanded. A relative path would resolve against the
// daemon's working directory, so it is refused rather than guessed at.
func ollamaBlobsDir(st *engineState) string {
	st.mu.Lock()
	var launched []string
	if st.proc != nil && st.proc.cmd != nil {
		launched = st.proc.cmd.Env
	}
	st.mu.Unlock()
	value := os.Getenv("OLLAMA_MODELS")
	if launched != nil {
		value = environmentValue(launched, "OLLAMA_MODELS")
	}
	root := strings.Trim(strings.TrimSpace(value), `"'`)
	if root == "" {
		home := ""
		if launched != nil {
			home = environmentValue(launched, homeVariable())
		} else if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
		if home == "" {
			return ""
		}
		root = filepath.Join(home, ".ollama", "models")
	}
	if !filepath.IsAbs(root) {
		return ""
	}
	return filepath.Join(root, "blobs")
}

// homeVariable names the variable os.UserHomeDir reads, and so the one a Go
// program such as Ollama finds its home folder in.
func homeVariable() string {
	if runtime.GOOS == "windows" {
		return "USERPROFILE"
	}
	return "HOME"
}

// environmentValue is the value a process started with env sees for key: the
// last assignment wins, and names compare the way the platform compares them.
func environmentValue(env []string, key string) string {
	value := ""
	for _, assignment := range env {
		name, assigned, ok := strings.Cut(assignment, "=")
		if ok && environmentKey(name) == environmentKey(key) {
			value = assigned
		}
	}
	return value
}
