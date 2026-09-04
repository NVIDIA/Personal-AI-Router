// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"
)

// Install obtains the engine in user mode: download (checksum-verified)
// then run the declared command. No-op if already detected.
func (e *Executor) Install(ctx context.Context, engine string) error {
	st, err := e.state(engine)
	if err != nil {
		return err
	}
	st.opMu.Lock()
	defer st.opMu.Unlock()
	if ok, _ := e.Detect(engine); ok {
		e.reporter.clear(installFailedID(engine))
		e.emitInstallProgress(engine, "already-installed", 100)
		return nil
	}
	st.mu.Lock()
	port := st.port
	st.mu.Unlock()
	presence := e.reconcilePresence(ctx, engine, st, false, port, false)
	if presence.Identified {
		// A healthy service is already present even though no managed binary
		// was detected. Treat it as an external installation and never fetch a
		// second copy. reconcilePresence emits the adopted state; the terminal
		// progress event clears any remaining install-progress UI.
		e.reporter.clear(installFailedID(engine))
		e.emitInstallProgress(engine, "already-installed", 100)
		return nil
	}
	if presence.Occupied && st.plat.Runtime.modeOrDefault() == "process" {
		err := fmt.Errorf("cannot install engine %q for port %d: the port is occupied by a service that did not identify as %s; use the existing service or choose another port", engine, port, st.manifest.DisplayName)
		e.reportInstallFailed(engine, err)
		return err
	}
	inst := st.plat.Install
	if inst == nil {
		return fmt.Errorf("engine %q has no install block for this platform", engine)
	}
	if inst.ModeOrDefault() == "admin" {
		err := fmt.Errorf("engine %q declares an admin install, which is refused (engine-manager is user-mode only)", engine)
		e.reportInstallFailed(engine, err)
		return err
	}
	if err := os.MkdirAll(st.installDir, 0o755); err != nil {
		return fmt.Errorf("create install dir: %w", err)
	}

	vars := map[string]string{"install_dir": st.installDir}

	if len(inst.Script) > 0 {
		// Escape hatch: vendor-script install with no checksum. Logged
		// loudly so the weaker guarantee is never silent.
		slog.Warn("running UNPINNED script install (no checksum verification)", "engine", engine)
		e.emitInstallProgress(engine, "installing", 50)
		argv, err := resolveArgs(inst.Script, vars)
		if err != nil {
			e.reportInstallFailed(engine, err)
			return err
		}
		for i := range argv {
			argv[i] = expandPath(argv[i])
		}
		if err := e.runCommand(ctx, argv); err != nil {
			werr := fmt.Errorf("script install failed: %w", err)
			e.reportInstallFailed(engine, werr)
			return werr
		}
	} else {
		if inst.Fetch != nil {
			e.emitInstallProgress(engine, "downloading", 0)
			dp, err := e.download(ctx, engine, inst.Fetch)
			if err != nil {
				e.reportInstallFailed(engine, err)
				return err
			}
			defer os.Remove(dp)
			vars["download"] = dp
			e.emitInstallProgress(engine, "verified", 50)
		}
		if len(inst.Run) > 0 {
			e.emitInstallProgress(engine, "installing", 75)
			args, err := resolveArgs(inst.Run, vars)
			if err != nil {
				e.reportInstallFailed(engine, err)
				return err
			}
			for i := range args {
				args[i] = expandPath(args[i])
			}
			if err := e.runCommand(ctx, args); err != nil {
				werr := fmt.Errorf("install command failed: %w", err)
				e.reportInstallFailed(engine, werr)
				return werr
			}
		}
	}

	if !e.waitDetect(engine, true, e.detectTimeout) {
		err := fmt.Errorf("engine %q was not detected after install", engine)
		e.reportInstallFailed(engine, err)
		return err
	}
	e.reporter.clear(installFailedID(engine))
	e.emitInstallProgress(engine, "done", 100)
	e.emitState(engine)
	return nil
}

// Uninstall runs the manifest's uninstall command (user-mode), stopping
// the engine first. No-op if the engine isn't currently detected.
func (e *Executor) Uninstall(ctx context.Context, engine string) error {
	st, err := e.state(engine)
	if err != nil {
		return err
	}
	st.opMu.Lock()
	defer st.opMu.Unlock()
	if ok, _ := e.Detect(engine); !ok {
		return e.setDesiredEnabled(engine, false) // already gone
	}
	un := st.plat.Uninstall
	if un == nil || len(un.Run) == 0 {
		return fmt.Errorf("engine %q has no uninstall defined for this platform", engine)
	}
	st.mu.Lock()
	binPath := st.binPath // resolved by the Detect at the top of Uninstall
	st.mu.Unlock()
	if st.plat.Runtime.modeOrDefault() == "process" && !isManagedInstallPath(binPath, st.installDir) {
		err := fmt.Errorf("cannot uninstall engine %q: its executable is outside NVPAIR's managed install directory (%s)", engine, binPath)
		e.reporter.report(serviceError{ID: uninstallFailedID(engine), Message: err.Error(), Severity: "error", Action: "none", EngineType: engine, Operation: "uninstall"})
		return err
	}
	// Detect proves only that the managed image exists, not whether a process is
	// serving. Reconcile first so doStop can either stop our exact managed
	// process (including a handle-less orphan) or refuse an external owner.
	st.mu.Lock()
	port := st.port
	st.mu.Unlock()
	presence := e.reconcilePresence(ctx, engine, st, true, port, false)
	st.mu.Lock()
	ownedProcess := st.proc != nil
	st.mu.Unlock()
	if presence.Occupied && !presence.Identified && !ownedProcess {
		err := fmt.Errorf("cannot uninstall engine %q: port %d is occupied by an unidentified service", engine, port)
		e.reporter.report(serviceError{ID: uninstallFailedID(engine), Message: err.Error(), Severity: "error", Action: "none", EngineType: engine, Operation: "uninstall"})
		return err
	}
	if err := e.doStop(st, engine); err != nil {
		werr := fmt.Errorf("cannot uninstall engine %q: %w", engine, err)
		e.reporter.report(serviceError{ID: uninstallFailedID(engine), Message: werr.Error(), Severity: "error", Action: "none", EngineType: engine, Operation: "uninstall"})
		return werr
	}

	args, err := resolveArgs(un.Run, map[string]string{"install_dir": st.installDir})
	if err != nil {
		return err
	}
	for i := range args {
		args[i] = expandPath(args[i])
	}
	var runErr error
	for attempt := 1; attempt <= uninstallRetries; attempt++ {
		if runErr = e.runCommand(ctx, args); runErr == nil {
			break
		}
		if attempt < uninstallRetries {
			slog.Warn("uninstall command failed; retrying", "engine", engine, "attempt", attempt, "of", uninstallRetries, "err", runErr)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(uninstallBackoff):
			}
		}
	}
	if runErr != nil {
		werr := fmt.Errorf("uninstall command failed after %d attempts: %w", uninstallRetries, runErr)
		e.reporter.report(serviceError{ID: uninstallFailedID(engine), Message: werr.Error(), Severity: "error", Action: "retry", EngineType: engine, Operation: "uninstall"})
		return werr
	}
	if !e.waitDetect(engine, false, e.detectTimeout) {
		uerr := fmt.Errorf("engine %q still detected after uninstall", engine)
		e.reporter.report(serviceError{ID: uninstallFailedID(engine), Message: uerr.Error(), Severity: "error", Action: "none", EngineType: engine, Operation: "uninstall"})
		return uerr
	}
	st.mu.Lock()
	st.binPath = ""
	st.mu.Unlock()
	e.reporter.clear(uninstallFailedID(engine))
	e.emitState(engine)
	return e.setDesiredEnabled(engine, false)
}

// maxDownloadBytes caps a single engine download (engine installers /
// archives run large — e.g. CUDA-bundled ~2 GB — but this guards against
// an unbounded/malicious response filling the disk). A var, not a const,
// so tests can lower it.
var maxDownloadBytes int64 = 8 << 30 // 8 GiB

// uninstallRetries / uninstallBackoff bound retrying a failed uninstall
// command. A command-mode engine's daemon (e.g. LM Studio's backend) can
// outlive its `stop` and briefly hold files open, so a straight rm hits
// "directory not empty"; a short retry lets the tree settle. Vars so tests
// can shrink the backoff.
var (
	uninstallRetries = 4
	uninstallBackoff = 3 * time.Second
)

// validateDownloadURL requires engine downloads over HTTPS, with plain
// http allowed only from loopback (the live tests serve the artifact from
// a local httptest server, and a user might run a LAN mirror). This is the
// only transport protection for an unpinned (no-sha256) fetch.
func validateDownloadURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid download url %q: %w", raw, err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if h := u.Hostname(); h == "127.0.0.1" || h == "::1" || strings.EqualFold(h, "localhost") {
			return nil
		}
		return fmt.Errorf("download url %q must be https (plain http is allowed only from loopback)", raw)
	default:
		return fmt.Errorf("download url %q must use https", raw)
	}
}

func (e *Executor) download(ctx context.Context, engine string, f *Fetch) (string, error) {
	if err := validateDownloadURL(f.URL); err != nil {
		return "", err
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(dctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", f.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", f.URL, resp.StatusCode)
	}

	// Preserve the URL suffix so tools that require one (notably PowerShell's
	// -File, which accepts only .ps1 files) can execute the downloaded artifact
	// directly instead of evaluating remote content inline.
	tmp, err := os.CreateTemp("", "nvpair-engine-"+engine+"-*"+path.Ext(req.URL.Path))
	if err != nil {
		return "", err
	}
	pw := &progressWriter{total: resp.ContentLength, onPct: func(p int) {
		e.emitInstallProgress(engine, "downloading", p)
	}}
	h := sha256.New()
	// Read one byte past the cap so we can detect (and reject) overflow.
	n, err := io.Copy(io.MultiWriter(tmp, h), io.TeeReader(io.LimitReader(resp.Body, maxDownloadBytes+1), pw))
	tmp.Close()
	if err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("download %s: %w", f.URL, err)
	}
	if n > maxDownloadBytes {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("download %s exceeds the %d-byte limit", f.URL, int64(maxDownloadBytes))
	}

	sum := hex.EncodeToString(h.Sum(nil))
	if want := strings.TrimSpace(f.SHA256); want != "" {
		if !strings.EqualFold(sum, want) {
			os.Remove(tmp.Name())
			return "", fmt.Errorf("checksum mismatch for %s: got %s, want %s", f.URL, sum, want)
		}
	} else if allowUnpinnedDownloads() {
		// Explicit operator opt-in: bytes are not integrity-checked, only
		// transport-secured (HTTPS, enforced above). Logged loudly, and the
		// computed digest is surfaced so a manifest author can pin it later.
		slog.Warn("UNPINNED download allowed by NVPAIR_ALLOW_UNPINNED_DOWNLOADS: integrity not verified",
			"engine", engine, "url", f.URL, "computed_sha256", sum)
	} else {
		// Fail closed: an executed artifact without a pinned digest means any
		// HTTPS host serving the manifest URL yields code execution. Surface
		// the digest so the manifest author can pin it immediately.
		os.Remove(tmp.Name())
		return "", fmt.Errorf("download %s has no sha256 pin in the manifest (computed sha256 %s); "+
			"pin it or set NVPAIR_ALLOW_UNPINNED_DOWNLOADS=1 to accept unverified downloads", f.URL, sum)
	}
	return tmp.Name(), nil
}

// allowUnpinnedDownloads reports whether the operator explicitly opted in to
// executing downloads whose manifests carry no sha256 pin.
func allowUnpinnedDownloads() bool {
	return os.Getenv("NVPAIR_ALLOW_UNPINNED_DOWNLOADS") == "1"
}

// runCommand executes a manifest-declared argv (an install or uninstall
// step), hiding the console window on Windows; on failure it returns the
// combined output for diagnostics.
func (e *Executor) runCommand(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	configureSysProcAttr(cmd) // hide the console window on Windows
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (e *Executor) reportInstallFailed(engine string, err error) {
	e.notify("engine:install-progress", map[string]any{"engine": engine, "stage": "failed", "percent": -1, "error": err.Error()})
	e.progress.publish(ProgressEvent{Engine: engine, Op: "install", Stage: "failed", Percent: -1, Message: err.Error()})
	e.reporter.report(serviceError{
		ID: installFailedID(engine), Message: err.Error(),
		Severity: "error", Action: "retry", EngineType: engine, Operation: "install",
	})
}

// progressWriter counts bytes and reports integer-percent download
// progress; it satisfies io.Writer for use as a TeeReader sink.
type progressWriter struct {
	total int64
	read  int64
	last  int
	onPct func(int)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n := len(b)
	p.read += int64(n)
	if p.total > 0 && p.onPct != nil {
		pct := int(p.read * 100 / p.total)
		if pct != p.last {
			p.last = pct
			p.onPct(pct)
		}
	}
	return n, nil
}
