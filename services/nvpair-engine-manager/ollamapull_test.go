// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Digests and blob names are written out in full rather than built with
// strings.Repeat, so a failure message can be grepped straight back to the case
// that produced it.
const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	blobA   = "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	blobB   = "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	blobC   = "sha256-cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// writeBlob seeds a file named like an Ollama blob in root.
func writeBlob(t *testing.T, root, name string) string {
	t.Helper()
	path := filepath.Join(root, name)
	writePartial(t, path, "data")
	return path
}

// Cleanup has to look where the daemon actually writes. For a daemon PAIR
// launched, that is the OLLAMA_MODELS it was launched with, which a user's
// launch override can set, rather than whatever the engine manager's own
// environment says.
func TestOllamaBlobsDir_FollowsTheEnvironmentOllamaRunsIn(t *testing.T) {
	// Ollama finds its home folder the way Go's os.UserHomeDir does.
	homeVariable := "HOME"
	if runtime.GOOS == "windows" {
		homeVariable = "USERPROFILE"
	}
	launchedHome := filepath.Join(t.TempDir(), "home")
	launchedDefault := filepath.Join(launchedHome, ".ollama", "models", "blobs")
	launchedDir := filepath.Join(t.TempDir(), "launched")
	overrideDir := filepath.Join(t.TempDir(), "override")
	inheritedDir := filepath.Join(t.TempDir(), "inherited")
	t.Setenv("OLLAMA_MODELS", inheritedDir)
	launchedWith := func(env ...string) *engineState {
		return &engineState{proc: &managedProc{cmd: &exec.Cmd{Env: env}}}
	}
	test := func(name string, st *engineState, want string) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, ollamaBlobsDir(st))
		})
	}

	test("a launched daemon uses the value it was launched with",
		launchedWith("OLLAMA_MODELS="+launchedDir), filepath.Join(launchedDir, "blobs"))
	test("a launch override assigned after the inherited value wins",
		launchedWith("OLLAMA_MODELS="+inheritedDir, "OLLAMA_MODELS="+overrideDir), filepath.Join(overrideDir, "blobs"))
	test("surrounding spaces and quotes are trimmed as Ollama trims them",
		launchedWith(`OLLAMA_MODELS= "`+launchedDir+`" `), filepath.Join(launchedDir, "blobs"))
	test("a launched daemon without the variable uses Ollama's default under the home it was launched with",
		launchedWith(homeVariable+"="+launchedHome, "OLLAMA_HOST=127.0.0.1:11434"), launchedDefault)
	test("an empty value uses Ollama's default",
		launchedWith(homeVariable+"="+launchedHome, "OLLAMA_MODELS="), launchedDefault)
	test("a launched daemon without a home folder has no default to look in",
		launchedWith("OLLAMA_HOST=127.0.0.1:11434"), "")
	test("an adopted daemon is assumed to share the engine manager's environment",
		&engineState{}, filepath.Join(inheritedDir, "blobs"))
	test("a relative value is refused rather than resolved against an unknown directory",
		launchedWith("OLLAMA_MODELS=models"), "")
	test("a home-relative value is refused, because Ollama does not expand it",
		launchedWith("OLLAMA_MODELS=~/models"), "")
	if runtime.GOOS == "windows" {
		test("names compare case-insensitively on Windows",
			launchedWith("ollama_models="+launchedDir), filepath.Join(launchedDir, "blobs"))
	}
}

// An unknown blobs directory must not be read as the current directory, where
// a snapshot that found nothing would let cleanup delete files named like
// blobs relative to wherever the engine manager happens to run.
func TestOllamaPartialSnapshot_RefusesAnUnknownBlobsDirectory(t *testing.T) {
	before, err := ollamaPartialSnapshot("")

	assert.Error(t, err)
	assert.Nil(t, before, "a snapshot that was not taken must stay distinct from an empty one")
}

// Cancelling a download removes the partial blobs that download created and
// nothing else. A completed blob can be a layer of an installed model, and a
// partial for a digest this pull never reported belongs to another transfer.
func TestCleanupOllamaAfterCancel_RemovesOnlyPartialsThisPullCreated(t *testing.T) {
	root := t.TempDir()
	completedLayer := writeBlob(t, root, blobA)
	before, err := ollamaPartialSnapshot(root)
	require.NoError(t, err)
	created := writeBlob(t, root, blobA+"-partial")
	createdChunk := writeBlob(t, root, blobA+"-partial-0")
	// A transfer another client started during this pull postdates the
	// snapshot too, so only the digests this pull reported set its partial
	// apart.
	otherTransfer := writeBlob(t, root, blobB+"-partial")
	// "../escape" stands for a digest the engine never could have reported; it
	// must not be turned into a path.
	digests := map[string]bool{digestA: true, "../escape": true}

	require.NoError(t, cleanupOllamaAfterCancel(settledContext(t), root, digests, before))

	assertRemoved(t, created)
	assertRemoved(t, createdChunk)
	assertPresent(t, completedLayer)
	assertPresent(t, otherTransfer)
}

// A partial already on disk when the download started is not that download's to
// delete. Blobs are content-addressed, so it is either another client's
// transfer — which can sit still for far longer than any quiet period while it
// waits on a slow server — or one PAIR deliberately kept when an earlier
// attempt's context died without a cancel request. Only the files a pull
// creates are unambiguously its own.
func TestCleanupOllamaAfterCancel_KeepsPartialsOlderThanThePull(t *testing.T) {
	root := t.TempDir()
	stalled := writeBlob(t, root, blobA+"-partial")
	before, err := ollamaPartialSnapshot(root)
	require.NoError(t, err)

	require.NoError(t, cleanupOllamaAfterCancel(settledContext(t), root, map[string]bool{digestA: true}, before))

	assertPresent(t, stalled)
}

// Without a snapshot nothing can be attributed to the pull, so cancelling it
// deletes nothing rather than guess at ownership.
func TestCleanupOllamaAfterCancel_DeletesNothingWithoutASnapshot(t *testing.T) {
	root := t.TempDir()
	partial := writeBlob(t, root, blobA+"-partial")

	require.NoError(t, cleanupOllamaAfterCancel(settledContext(t), root, map[string]bool{digestA: true}, nil))

	assertPresent(t, partial)
}

// Ollama coalesces a concurrent pull of the same layer — from its CLI, its
// desktop app, the TUI's own engine-manager, or any other client on the daemon
// — onto the very same partial file, including one this pull created. A file
// still growing after this transfer stopped is therefore shared, and survives.
func TestCleanupOllamaAfterCancel_KeepsAPartialAnotherClientIsWriting(t *testing.T) {
	root := t.TempDir()
	before, err := ollamaPartialSnapshot(root)
	require.NoError(t, err)
	shared := writeBlob(t, root, blobC+"-partial")
	ctx := withPartialSettle(t.Context(), func() { growFile(t, shared) })

	require.NoError(t, cleanupOllamaAfterCancel(ctx, root, map[string]bool{digestC: true}, before))

	assertPresent(t, shared)
}

// Ollama keeps a transfer's resume state beside its data file, one
// "-partial-N" file per chunk, and fails to finish the blob if any of them is
// missing. A chunk that has finished holds still while the data file goes on
// growing, so a shared transfer keeps its finished chunks' files as well.
func TestCleanupOllamaAfterCancel_KeepsTheResumeFilesOfAPartialAnotherClientIsWriting(t *testing.T) {
	root := t.TempDir()
	before, err := ollamaPartialSnapshot(root)
	require.NoError(t, err)
	shared := writeBlob(t, root, blobC+"-partial")
	finishedChunk := writeBlob(t, root, blobC+"-partial-0")
	ctx := withPartialSettle(t.Context(), func() { growFile(t, shared) })

	require.NoError(t, cleanupOllamaAfterCancel(ctx, root, map[string]bool{digestC: true}, before))

	assertPresent(t, shared)
	assertPresent(t, finishedChunk)
}

// Ollama writes a transfer's resume files before it creates the data file. A
// data file appearing after the snapshot beside resume files that were already
// there is another client's transfer getting under way, not this pull's.
func TestCleanupOllamaAfterCancel_KeepsAPartialWhoseResumeFilesPredateThePull(t *testing.T) {
	root := t.TempDir()
	olderChunk := writeBlob(t, root, blobA+"-partial-0")
	before, err := ollamaPartialSnapshot(root)
	require.NoError(t, err)
	data := writeBlob(t, root, blobA+"-partial")

	require.NoError(t, cleanupOllamaAfterCancel(settledContext(t), root, map[string]bool{digestA: true}, before))

	assertPresent(t, olderChunk)
	assertPresent(t, data)
}

// Ollama finishes a blob by removing its resume files and then renaming the
// data file into place. Resume files vanishing after the grace is another
// client finishing the transfer, so the data file it is about to rename stays.
func TestCleanupOllamaAfterCancel_KeepsAPartialAnotherClientIsFinishing(t *testing.T) {
	root := t.TempDir()
	before, err := ollamaPartialSnapshot(root)
	require.NoError(t, err)
	data := writeBlob(t, root, blobC+"-partial")
	chunk := writeBlob(t, root, blobC+"-partial-0")
	waits := 0
	ctx := withPartialSettle(t.Context(), func() {
		waits++
		if waits == partialGraceSamples {
			require.NoError(t, os.Remove(chunk))
		}
	})

	require.NoError(t, cleanupOllamaAfterCancel(ctx, root, map[string]bool{digestC: true}, before))

	assertPresent(t, data)
}

// newOllamaPullExecutor wires an executor whose ollama pull_model action points
// at a fake /api/pull server.
func newOllamaPullExecutor(t *testing.T, serverURL string, emit func(method string, params any)) *Executor {
	t.Helper()
	_, portText, err := net.SplitHostPort(strings.TrimPrefix(serverURL, "http://"))
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	manifest := testEngineManifest(fakeEngineBin)
	manifest.Engine = "ollama"
	manifest.Actions[pullModelAction] = Action{HTTP: &ActionHTTP{Method: http.MethodPost, Path: "/api/pull"}}
	reg := NewRegistry()
	reg.engines["ollama"] = manifest
	ex := NewExecutor(reg, NewReporter(nil), emit, t.TempDir())
	st, err := ex.state("ollama")
	require.NoError(t, err)
	st.running = true
	st.port = port
	return ex
}

// newOllamaBlobsDir points the engine at an empty blobs directory. The partial
// arrives once the download starts, the way a real one does, so the snapshot
// the pull takes beforehand can attribute it.
func newOllamaBlobsDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("OLLAMA_MODELS", root)
	blobs := filepath.Join(root, "blobs")
	require.NoError(t, os.MkdirAll(blobs, 0700))
	return blobs
}

// ollamaTransfer serves an Ollama pull that writes the partial blob a transfer
// would, streams statuses, and then holds the connection open until the client
// lets go of it. The partial appears only once the pull has started, so it is
// one the pull created.
func ollamaTransfer(t *testing.T, partial string, statuses ...string) (server *httptest.Server, disconnected <-chan struct{}) {
	t.Helper()
	closed := make(chan struct{})
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(closed)
		if !assert.NoError(t, os.WriteFile(partial, []byte("partial"), 0o600)) {
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, status := range statuses {
			_, err := fmt.Fprintf(w, `{"status":%q,"digest":%q,"total":100,"completed":25}`+"\n", status, digestA)
			if !assert.NoError(t, err) {
				return
			}
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	return server, closed
}

// startOllamaPull runs a pull of "demo" against an Ollama transfer on its own
// goroutine, and returns once the engine manager has read the transfer's first
// status. By then the partial blob exists and the pull knows its digest, so
// whatever stops the pull from here on finds a file the pull can claim.
func startOllamaPull(t *testing.T, ctx context.Context, server *httptest.Server) (*Executor, <-chan outcome) {
	t.Helper()
	downloading := make(chan struct{})
	var once sync.Once
	ex := newOllamaPullExecutor(t, server.URL, func(method string, _ any) {
		if method == "engine:pull-progress" {
			once.Do(func() { close(downloading) })
		}
	})
	done := make(chan outcome, 1)
	go func() {
		var got outcome
		got.result, got.err = ex.PullModelStream(ctx, "ollama", "demo", nil)
		done <- got
	}()
	select {
	case <-downloading:
	case <-ctx.Done():
		require.FailNow(t, "the pull did not start")
	}
	return ex, done
}

// A cancel someone asked for stops Ollama's transfer and removes the partial
// blob the pull created.
func TestOllamaCancellation_StopsTheTransferAndRemovesItsPartial(t *testing.T) {
	partial := filepath.Join(newOllamaBlobsDir(t), blobA+"-partial")
	server, disconnected := ollamaTransfer(t, partial, "pulling layer")
	ctx, cancel := context.WithTimeout(settledContext(t), 30*time.Second)
	defer cancel()
	ex, done := startOllamaPull(t, ctx, server)

	require.NoError(t, ex.CancelModelPull(ctx, "ollama", "demo"))
	got := <-done

	require.NoError(t, got.err)
	assertPullStatus(t, got.result, "cancelled")
	select {
	case <-disconnected:
	case <-ctx.Done():
		require.FailNow(t, "the HTTP transfer did not disconnect")
	}
	assertRemoved(t, partial)
}

// Cleanup that cannot finish is not a failed cancellation. The transfer has
// already stopped, which is what was asked for, and the bytes left behind are
// the vendor's to resume. Reporting the cleanup error as the pull's outcome
// denied the one thing that did happen and left the row on "Canceling" over a
// download that was already gone.
func TestOllamaCancellation_SucceedsWhenCleanupCannotFinish(t *testing.T) {
	partial := filepath.Join(newOllamaBlobsDir(t), blobA+"-partial")
	server, _ := ollamaTransfer(t, partial, "pulling layer")
	ctx, cancel := context.WithTimeout(settledContext(t), 60*time.Second)
	defer cancel()
	ex, done := startOllamaPull(t, ctx, server)
	// The partial exists and is attributable to this pull by now, so this is
	// the point its removal can be made to fail.
	blockPartialRemoval(t, partial)

	require.NoError(t, ex.CancelModelPull(ctx, "ollama", "demo"))
	got := <-done

	require.NoError(t, got.err, "the cancellation should be reported as a cancellation")
	assertPullStatus(t, got.result, "cancelled")
	// Cleanup could not remove it, and saying so is the warning's job, not the
	// cancel's. The file stays for the next attempt to resume.
	assertPresent(t, partial)
}

// The pull's context also dies when PAIR quits mid-download, when a remote
// initiator disconnects, and when the action timeout elapses. None of those
// mean the user gave up on the bytes already on disk, so a resumable transfer
// has to survive them instead of restarting from zero on the next attempt.
func TestOllamaPull_KeepsItsPartialWhenStoppedWithoutACancelRequest(t *testing.T) {
	partial := filepath.Join(newOllamaBlobsDir(t), blobA+"-partial")
	server, _ := ollamaTransfer(t, partial, "pulling layer")
	ctx, cancel := context.WithCancel(settledContext(t))
	defer cancel()
	_, done := startOllamaPull(t, ctx, server)

	cancel()
	got := <-done

	require.ErrorIs(t, got.err, context.Canceled)
	data, err := os.ReadFile(partial)
	require.NoError(t, err, "a resumable partial was deleted without a cancel request")
	assert.Equal(t, "partial", string(data))
}

// Ollama's success record is its last: the model is installed. The pull
// reports success on it even while the connection stays open, and a cancel
// that lands afterwards finds nothing to stop and leaves the files alone.
func TestOllamaPull_SucceedsOnOllamasSuccessRecord(t *testing.T) {
	test := func(name string, statuses ...string) {
		t.Run(name, func(t *testing.T) {
			partial := filepath.Join(newOllamaBlobsDir(t), blobA+"-partial")
			server, _ := ollamaTransfer(t, partial, statuses...)
			ex := newOllamaPullExecutor(t, server.URL, nil)
			ctx, cancel := context.WithTimeout(settledContext(t), 30*time.Second)
			defer cancel()

			result, err := ex.PullModelStream(ctx, "ollama", "demo", nil)
			require.NoError(t, err)
			require.NoError(t, ex.CancelModelPull(ctx, "ollama", "demo"))

			assertPullStatus(t, result, "success")
			assertPresent(t, partial)
		})
	}
	test("success is the last record", "pulling layer", "success")
	test("a record follows success", "pulling layer", "success", "")
}

// Ollama ends every pull with a success or an error record. A stream that
// closes without either was cut off — the daemon exited or dropped the
// connection — so the model is not installed. Reporting success there left the
// UI showing a download complete for a model the engine does not have.
func TestOllamaPull_FailsWhenTheStreamEndsBeforeSuccess(t *testing.T) {
	newOllamaBlobsDir(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, err := fmt.Fprintf(w, `{"status":"pulling layer","digest":%q,"total":100,"completed":25}`+"\n", digestA)
		assert.NoError(t, err, "send pull progress")
	}))
	t.Cleanup(server.Close)
	ex := newOllamaPullExecutor(t, server.URL, nil)
	events, unsubscribe := ex.progress.subscribe("ollama")
	defer unsubscribe()

	_, err := ex.PullModelStream(t.Context(), "ollama", "demo", nil)

	require.ErrorContains(t, err, "ended the download without reporting it complete")
	frames := publishedFrames(events)
	require.NotEmpty(t, frames, "the pull published no frames")
	terminal := frames[len(frames)-1]
	assert.Equal(t, "error", terminal.Stage, "the last frame must report the failure")
	assert.Equal(t, -1, terminal.Percent)
}
