// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// hfsnapshot.go is the hf_snapshot pull: it downloads every file of a public
// Hugging Face model repository into the engine's model store, for an engine
// that reads a model as a directory (Colibri) rather than fetching it itself.
//
// The download lands in "<dest>.partial" and is renamed into place only once
// every file has arrived and matches its published size, so the store never
// lists a half-downloaded model. Each file is written as "<file>.part" and
// resumed with an HTTP Range request, both after a transient error within one
// pull and across pulls, so cancelling a 400 GB download loses nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// hfDefaultBaseURL is the Hugging Face hub, overridable per executor in
	// tests through Executor.hfBaseURL.
	hfDefaultBaseURL = "https://huggingface.co"
	// hfSnapshotSpareBytes is left free on the store's volume beyond the
	// download itself, so a pull never fills the disk to the last byte.
	hfSnapshotSpareBytes = 1 << 30
)

var (
	// hfSnapshotAttempts bounds how often one file is tried within a pull
	// after a transient failure; each retry resumes where the last stopped.
	// Vars so tests can shrink them.
	hfSnapshotAttempts     = 3
	hfSnapshotRetryBackoff = 2 * time.Second
	// freeDiskBytes reports the bytes available to this user on the volume
	// holding path. A var so tests can simulate a full disk.
	freeDiskBytes = diskFreeBytes
)

// hfRepoMeta is the subset of GET /api/models/{repo}?blobs=true used here.
type hfRepoMeta struct {
	Private bool `json:"private"`
	// Gated is false for an open repo and a string ("auto", "manual") for one
	// that needs its terms accepted first, so it is kept raw.
	Gated    json.RawMessage `json:"gated"`
	Siblings []hfRepoFile    `json:"siblings"`
	// Config is the hub's parse of the root config.json.
	Config *hfModelConfig `json:"config"`
}

// modelType is the repository's normalized root config.json model_type, empty
// when the hub reports none.
func (m hfRepoMeta) modelType() string {
	if m.Config == nil {
		return ""
	}
	return normalizeModelType(m.Config.ModelType)
}

// hfRepoFile is one file of a repository. Size is zero when unpublished.
type hfRepoFile struct {
	RFilename string `json:"rfilename"`
	Size      int64  `json:"size"`
}

// hfPlannedFile is one file to fetch and where it goes.
type hfPlannedFile struct {
	name string // repository path, "/" separated
	size int64  // published size; zero when unknown
	path string // destination inside the partial directory
}

// hfStatusError is an HTTP status from Hugging Face. A 5xx or 429 is worth
// retrying; anything else is final.
type hfStatusError struct {
	file   string
	status int
}

func (e hfStatusError) Error() string {
	return fmt.Sprintf("Hugging Face answered HTTP %d for %s", e.status, e.file)
}

// hfTransient reports a failure that a resumed retry can fix.
func hfTransient(err error) bool {
	var status hfStatusError
	if errors.As(err, &status) {
		return status.status >= 500 || status.status == http.StatusTooManyRequests
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func (e *Executor) hfBase() string {
	if e.hfBaseURL != "" {
		return strings.TrimRight(e.hfBaseURL, "/")
	}
	return hfDefaultBaseURL
}

// pullHFSnapshot runs an hf_snapshot pull_model action for model, an
// owner/name repository id, and returns {"model","files","bytes"}. A
// repository the manifest lists is downloaded by its entry's rules.
func (e *Executor) pullHFSnapshot(ctx context.Context, st *engineState, engine, model string, act Action) (json.RawMessage, error) {
	if !hfRepoPattern.MatchString(model) {
		return nil, fmt.Errorf("%q is not a Hugging Face repository id (owner/name)", model)
	}
	dest, err := st.resolveModelPath(act.HFSnapshot.Dest, model)
	if err != nil {
		return nil, err
	}
	entry, listed := st.manifest.catalogEntry(model)
	marker, _, layout := st.manifest.modelStoreLayout()
	if layout && hasMarker(dest, marker) {
		// Complete but for a step: a copy downloaded before the step was listed,
		// or one whose step failed after the files were in.
		if err := e.runAfterDownload(ctx, st, model, entry.AfterDownload, dest); err != nil {
			return nil, err
		}
		e.emitPullProgress(ProgressEvent{Engine: engine, Op: "pull", Stage: "success", Percent: 100, Message: model})
		return json.Marshal(map[string]any{"model": model, "already": true})
	}

	// Two pulls of one model would write the same partial files.
	if _, busy := e.snapshotPulls.LoadOrStore(dest, struct{}{}); busy {
		return nil, fmt.Errorf("%s is already being downloaded", model)
	}
	defer e.snapshotPulls.Delete(dest)

	pullCtx, watchdog := newPullProgressWatchdog(ctx, e.pullProgressTimeout)
	defer watchdog.stop()
	e.emitPullProgress(ProgressEvent{Engine: engine, Op: "pull", Stage: "pulling", Message: model})

	meta, err := e.fetchHFRepoMeta(pullCtx, model, entry.Revision)
	if err != nil {
		return nil, err
	}
	// Checked before a byte is written: a model the engine has no code for
	// would be hundreds of gigabytes that never start. A listed repository is
	// the engine's own choice and is not second-guessed.
	if !listed && !st.manifest.servesModelType(meta.modelType()) {
		if meta.modelType() == "" {
			return nil, fmt.Errorf("%s declares no model_type in its config.json, so %s cannot tell how to run it", model, st.manifest.DisplayName)
		}
		return nil, fmt.Errorf("%s is a %q model, which %s cannot run", model, meta.modelType(), st.manifest.DisplayName)
	}
	partial := dest + partialSuffix
	files, total, err := planHFSnapshot(meta, partial, entry.Exclude)
	if err != nil {
		return nil, fmt.Errorf("%s cannot be downloaded: %w", model, err)
	}
	if layout && !hasRootFile(files, marker) {
		return nil, fmt.Errorf("%s has no %s, so it is not a model %s can serve", model, marker, st.manifest.DisplayName)
	}
	if err := os.MkdirAll(partial, 0o755); err != nil {
		return nil, fmt.Errorf("create the download folder: %w", err)
	}
	have := bytesOnDisk(files)
	if free, err := freeDiskBytes(partial); err == nil {
		if need := total - have + hfSnapshotSpareBytes; need > 0 && free < uint64(need) {
			return nil, fmt.Errorf("not enough disk space for %s: it needs %s more and %s is free in %s",
				model, formatGB(need), formatGB(int64(free)), st.modelsDir)
		}
	} else {
		slog.Warn("cannot read free disk space; downloading without the check", "engine", engine, "err", err)
	}

	progress := &snapshotProgress{engine: engine, model: model, total: total, lastPct: -1, emit: e.emitPullProgress}
	slog.Info("model download started", "engine", engine, "model", model, "files", len(files), "bytes", total)
	for _, f := range files {
		if err := e.downloadHFFile(pullCtx, model, entry.ref(), f, progress, watchdog); err != nil {
			return nil, fmt.Errorf("download %s: %w", f.name, watchdog.resolveError(pullCtx, err))
		}
	}
	// Before the rename, so the store never lists a model its engine cannot
	// read yet.
	if err := e.runAfterDownload(pullCtx, st, model, entry.AfterDownload, partial); err != nil {
		return nil, err
	}

	// A directory at dest without the marker is an earlier, incomplete copy of
	// this same model; the finished download replaces it.
	if _, err := os.Lstat(dest); err == nil {
		if err := os.RemoveAll(dest); err != nil {
			return nil, fmt.Errorf("replace the incomplete copy of %s: %w", model, err)
		}
	}
	if err := os.Rename(partial, dest); err != nil {
		return nil, fmt.Errorf("move the downloaded model into place: %w", err)
	}
	slog.Info("model download finished", "engine", engine, "model", model)

	// The first model an engine gets is the one it serves, so it can start.
	st.mu.Lock()
	current := st.plat.Runtime.Model
	st.mu.Unlock()
	if current == "" && runtimeReferencesModel(st.plat.Runtime) {
		st.opMu.Lock()
		if err := e.persistLaunchModel(engine, st, model); err != nil {
			slog.Warn("could not save the downloaded model as the launch model", "engine", engine, "model", model, "err", err)
		}
		st.opMu.Unlock()
	}
	e.emitPullProgress(ProgressEvent{Engine: engine, Op: "pull", Stage: "success", Percent: 100, Message: model})
	return json.Marshal(map[string]any{"model": model, "files": len(files), "bytes": total})
}

// fetchHFRepoMeta reads a repository's file list and access settings, at a
// pinned revision when one is given, and refuses one PAIR cannot download
// without credentials.
func (e *Executor) fetchHFRepoMeta(ctx context.Context, repo, revision string) (hfRepoMeta, error) {
	ctx, cancel := context.WithTimeout(ctx, catalogHTTPTimeout)
	defer cancel()
	endpoint := e.hfBase() + "/api/models/" + repo
	if revision != "" {
		endpoint += "/revision/" + url.PathEscape(revision)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?blobs=true", nil)
	if err != nil {
		return hfRepoMeta{}, fmt.Errorf("build the Hugging Face request: %w", withoutURL(err))
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "PAIR/1.0")
	resp, err := e.client.Do(req)
	if err != nil {
		return hfRepoMeta{}, fmt.Errorf("could not reach Hugging Face: %w", withoutURL(err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound:
		// Hugging Face answers 401 for a missing repo as well as a private one.
		return hfRepoMeta{}, fmt.Errorf("%s was not found on Hugging Face, or it is private or gated", repo)
	case resp.StatusCode != http.StatusOK:
		return hfRepoMeta{}, fmt.Errorf("Hugging Face returned %s for %s", resp.Status, repo)
	}
	var meta hfRepoMeta
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCatalogBody)).Decode(&meta); err != nil {
		return hfRepoMeta{}, fmt.Errorf("read the file list of %s: %w", repo, err)
	}
	// Only an explicit false is open, the same rule as the catalog.
	if meta.Private || strings.TrimSpace(string(meta.Gated)) != "false" {
		return hfRepoMeta{}, fmt.Errorf("%s is private or gated on Hugging Face, and PAIR only downloads open models", repo)
	}
	return meta, nil
}

// planHFSnapshot maps a repository's files into partial, leaving out the
// excluded ones and refusing any name that could land outside it or that the
// filesystem cannot hold.
func planHFSnapshot(meta hfRepoMeta, partial string, exclude []string) ([]hfPlannedFile, int64, error) {
	var files []hfPlannedFile
	var total int64
	for _, sibling := range meta.Siblings {
		name := sibling.RFilename
		if name == "" || name == ".gitattributes" || excluded(name, exclude) {
			continue
		}
		if !safeRepoFileName(name) {
			return nil, 0, fmt.Errorf("the repository contains a file name that is not safe to write: %q", name)
		}
		path := filepath.Join(partial, filepath.FromSlash(name))
		if path == filepath.Clean(partial) || !pathWithinRoot(filepath.Clean(partial), path) {
			return nil, 0, fmt.Errorf("the repository contains a file name that is not safe to write: %q", name)
		}
		size := max(sibling.Size, 0)
		files = append(files, hfPlannedFile{name: name, size: size, path: path})
		total += size
	}
	if len(files) == 0 {
		return nil, 0, errors.New("the repository has no files")
	}
	return files, total, nil
}

// safeRepoFileName accepts a relative "/"-separated path of plain names.
func safeRepoFileName(name string) bool {
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, ".part") {
		return false
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" || segment == "." || segment == ".." || strings.TrimSpace(segment) != segment {
			return false
		}
		for _, r := range segment {
			if r < 0x20 || strings.ContainsRune(`\:*?"<>|`, r) {
				return false
			}
		}
	}
	return true
}

// hasRootFile reports a planned file directly at the repository root.
func hasRootFile(files []hfPlannedFile, name string) bool {
	for _, f := range files {
		if f.name == name {
			return true
		}
	}
	return false
}

// bytesOnDisk counts what an earlier, interrupted pull already fetched.
func bytesOnDisk(files []hfPlannedFile) int64 {
	var have int64
	for _, f := range files {
		if fi, err := os.Stat(f.path); err == nil && (f.size == 0 || fi.Size() == f.size) {
			have += fi.Size()
		} else if fi, err := os.Stat(f.path + ".part"); err == nil && (f.size == 0 || fi.Size() <= f.size) {
			have += fi.Size()
		}
	}
	return have
}

// downloadHFFile fetches one file, skipping it when already complete and
// retrying a transient failure in place, resumed from what was written.
func (e *Executor) downloadHFFile(ctx context.Context, repo, ref string, f hfPlannedFile, progress *snapshotProgress, watchdog *pullProgressWatchdog) error {
	if fi, err := os.Stat(f.path); err == nil && f.size > 0 && fi.Size() == f.size {
		progress.add(f.size)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return err
	}
	for attempt := 1; ; attempt++ {
		err := e.fetchHFFileOnce(ctx, repo, ref, f, progress, watchdog)
		if err == nil {
			return nil
		}
		if attempt >= hfSnapshotAttempts || ctx.Err() != nil || !hfTransient(err) {
			return err
		}
		slog.Warn("model file download failed; resuming", "model", repo, "attempt", attempt, "of", hfSnapshotAttempts, "err", err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(hfSnapshotRetryBackoff):
		}
	}
}

// fetchHFFileOnce makes one ranged request for the rest of a file at ref (a
// branch or commit), appends it to the file's .part and renames that into place
// once it is complete.
func (e *Executor) fetchHFFileOnce(ctx context.Context, repo, ref string, f hfPlannedFile, progress *snapshotProgress, watchdog *pullProgressWatchdog) error {
	part := f.path + ".part"
	var offset int64
	if fi, err := os.Stat(part); err == nil {
		offset = fi.Size()
	}
	if f.size > 0 && offset > f.size {
		// Larger than the published file: not a prefix of it, start over.
		if err := os.Remove(part); err != nil {
			return err
		}
		offset = 0
	}
	if f.size > 0 && offset == f.size {
		progress.add(offset)
		return os.Rename(part, f.path)
	}

	segments := strings.Split(f.name, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.hfBase()+"/"+repo+"/resolve/"+url.PathEscape(ref)+"/"+strings.Join(segments, "/"), nil)
	if err != nil {
		return withoutURL(err)
	}
	req.Header.Set("User-Agent", "PAIR/1.0")
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return withoutURL(err)
	}
	defer resp.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	switch {
	case resp.StatusCode == http.StatusPartialContent && offset > 0 && rangeStartsAt(resp.Header.Get("Content-Range"), offset):
		flags |= os.O_APPEND
	case resp.StatusCode == http.StatusOK:
		// The server sent the whole file: discard what was there.
		offset = 0
		flags |= os.O_TRUNC
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		_ = os.Remove(part)
		return hfStatusError{file: f.name, status: http.StatusServiceUnavailable}
	default:
		return hfStatusError{file: f.name, status: resp.StatusCode}
	}
	out, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return err
	}
	counted := &countingWriter{w: out, done: offset, onWrite: func(done int64, n int) {
		watchdog.recordProgress(f.name, done)
		progress.add(int64(n))
	}}
	progress.add(offset)
	_, copyErr := io.Copy(counted, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		progress.add(-counted.done)
		return copyErr
	}
	if closeErr != nil {
		progress.add(-counted.done)
		return closeErr
	}
	if f.size > 0 && counted.done != f.size {
		progress.add(-counted.done)
		return fmt.Errorf("%s arrived as %d bytes, expected %d", f.name, counted.done, f.size)
	}
	return os.Rename(part, f.path)
}

// rangeStartsAt reports a Content-Range that resumes at offset.
func rangeStartsAt(contentRange string, offset int64) bool {
	rest, ok := strings.CutPrefix(contentRange, "bytes ")
	if !ok {
		return false
	}
	start, _, ok := strings.Cut(rest, "-")
	return ok && start == strconv.FormatInt(offset, 10)
}

// countingWriter tracks the bytes a file holds as they are written.
type countingWriter struct {
	w       io.Writer
	done    int64
	onWrite func(done int64, n int)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.done += int64(n)
	if n > 0 {
		c.onWrite(c.done, n)
	}
	return n, err
}

// snapshotProgress turns bytes into pull-progress frames, one per percent.
type snapshotProgress struct {
	engine, model string
	total, done   int64
	lastPct       int
	emit          func(ProgressEvent)
}

func (p *snapshotProgress) add(n int64) {
	p.done += n
	if p.total <= 0 {
		return
	}
	// 100 is the success frame's, sent once the model is in place.
	pct := int(min(max(p.done*100/p.total, 0), 99))
	if pct == p.lastPct {
		return
	}
	p.lastPct = pct
	p.emit(ProgressEvent{Engine: p.engine, Op: "pull", Stage: "downloading", Percent: pct, Message: p.model})
}

// formatGB renders a byte count for an error message.
func formatGB(n int64) string {
	return fmt.Sprintf("%.1f GB", float64(n)/1e9)
}
