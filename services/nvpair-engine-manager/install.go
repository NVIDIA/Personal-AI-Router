// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Install obtains the engine in user mode for a request that originated on this
// machine: download (checksum-verified), run the declared command, then publish
// the CLI directory on this user's PATH.
func (e *Executor) Install(ctx context.Context, engine string) error {
	return e.install(ctx, engine, true)
}

// InstallForPeer installs on behalf of a paired cluster node.
//
// It is the same install with the PATH step withheld. A peer may put an engine
// on this machine, but editing this user's login shell configuration and
// HKCU\Environment is a different kind of change: the pairing PIN is a
// convenience code, cluster membership is not proof of a vetted peer, and
// nothing in the remote-install flow tells the person sitting at this machine
// that their shell startup files are about to be rewritten.
func (e *Executor) InstallForPeer(ctx context.Context, engine string) error {
	return e.install(ctx, engine, false)
}

func (e *Executor) install(ctx context.Context, engine string, setUserPath bool) error {
	st, err := e.state(engine)
	if err != nil {
		return err
	}
	st.opMu.Lock()
	defer st.opMu.Unlock()
	if ok, _ := e.Detect(engine); ok {
		if setUserPath {
			e.installPath(engine, st, false)
		}
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
	installEnv := inst.environ()

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
		if err := e.runCommand(ctx, argv, installEnv...); err != nil {
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
			if err := e.runCommand(ctx, args, installEnv...); err != nil {
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
	if setUserPath {
		e.installPath(engine, st, true)
	}
	e.reporter.clear(installFailedID(engine))
	e.emitInstallProgress(engine, "done", 100)
	e.emitState(engine)
	return nil
}

// installPath publishes the engine's command-line directory on the user's PATH.
//
// A PATH problem is a warning, never an install failure. The engine is
// installed and fully usable without it, and the caller gates the optional
// start step on install succeeding — so failing here would leave a working
// engine stopped, carrying an error card, behind a retry that clears nothing.
func (e *Executor) installPath(engine string, st *engineState, installedNow bool) {
	dir, err := e.updateInstallPath(engine, st, installedNow)
	if err != nil {
		e.reportPathFailed(engine, st.manifest.DisplayName, dir, err)
		return
	}
	if dir == "" {
		// An external install PAIR declined to touch. Nothing was published, so
		// there is nothing to report cleared: clearing here would retract a
		// standing warning about an entry still missing from the user's PATH.
		return
	}
	e.reporter.clear(pathFailedID(engine))
}

// pathCLIDir resolves the directory to publish on PATH.
//
// The manifest's declared runtime.cli is authoritative. A detect entry is only
// a fallback and only from inside the engine's managed install directory:
// detect deliberately matches vendor installs PAIR does not own (Ollama's
// darwin list starts at /Applications/Ollama.app), and its first hit is a
// discovery result, not a statement about where the CLI lives.
func pathCLIDir(st *engineState) (string, error) {
	var cli string
	if declared := st.plat.Runtime.CLI; declared != "" {
		// {install_dir} is a documented runtime placeholder that detect and
		// runtime.bin both resolve, so PATH has to resolve it too. Without this
		// a manifest written to MANIFEST.md produced a non-absolute path and
		// published nothing but a warning.
		resolved, err := resolvePlaceholders(declared, map[string]string{"install_dir": st.installDir})
		if err != nil {
			return "", err
		}
		cli = expandPath(resolved)
	} else {
		if !managedCLI(st) {
			return "", fmt.Errorf("its command-line executable is outside the directory PAIR installs into")
		}
		// Detect has already expanded binPath; expanding it again would corrupt
		// a directory that legitimately contains a $ or %.
		st.mu.Lock()
		cli = st.binPath
		st.mu.Unlock()
	}
	if !filepath.IsAbs(cli) {
		// Anchoring this to the daemon's working directory would produce a PATH
		// entry that means nothing; a manifest has to declare an absolute path.
		return "", fmt.Errorf("the manifest resolves its command-line executable to a relative path")
	}
	// An unreadable executable is not an absent one. Collapsing the two told the
	// user to reinstall over a permission or I/O error, which fails identically.
	if _, err := os.Stat(cli); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("its command-line executable was not found")
		}
		return "", fmt.Errorf("its command-line executable could not be read: %w", err)
	}
	return filepath.Dir(cli), nil
}

// managedCLI reports whether the detected executable is one PAIR placed. An
// engine whose vendor installer owns its own location (LM Studio writes
// ~/.lmstudio) is never managed by this test, so a lost receipt there is
// indistinguishable from an external install and is left alone.
func managedCLI(st *engineState) bool {
	st.mu.Lock()
	bin := st.binPath
	st.mu.Unlock()
	return isManagedInstallPath(bin, st.installDir)
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
		// Joined rather than sequenced: this branch is the documented retry
		// after the files are already gone, so a desired-state write that keeps
		// failing must not be what makes PATH cleanup unreachable.
		return errors.Join(e.setDesiredEnabled(engine, false), e.uninstallPath(engine))
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
	e.emitState(engine)
	// The engine is gone either way, so both steps run and both errors travel.
	// Short-circuiting here left PATH published and the previous attempt's
	// error card standing whenever the desired-state write failed.
	return errors.Join(e.setDesiredEnabled(engine, false), e.uninstallPath(engine))
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
	} else {
		// Unpinned download: bytes are not integrity-checked, only
		// transport-secured (HTTPS, enforced above) — the same weaker
		// guarantee as a `script` install. Logged loudly, and the computed
		// digest is surfaced so a manifest author can pin it later.
		slog.Warn("UNPINNED download: manifest has no sha256, integrity not verified",
			"engine", engine, "url", f.URL, "computed_sha256", sum)
	}
	return tmp.Name(), nil
}

// runCommand executes a manifest-declared argv (an install or uninstall
// step), hiding the console window on Windows; on failure it returns the
// combined output for diagnostics.
func (e *Executor) runCommand(ctx context.Context, argv []string, env ...string) error {
	if len(argv) == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	configureSysProcAttr(cmd) // hide the console window on Windows
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// reportPathFailed surfaces a PATH problem as a dismissible warning telling the
// user what to do instead.
//
// The reported text is streamed to a remote install's initiator and push-synced
// to cluster peers, so the home directory is folded back to "~" before it
// leaves. That keeps the account name off the wire while still naming the
// profile the user has to edit. The full paths stay in this node's log.
func (e *Executor) reportPathFailed(engine, displayName, dir string, err error) {
	slog.Warn("could not add an engine's command-line tools to PATH",
		"engine", engine, "directory", dir, "err", err)
	message := fmt.Sprintf(
		"%s is installed and ready, but its command-line tools could not be added to your PATH (%s). Run the engine using its full path, or add its directory to your PATH yourself — the service log names the directory.",
		displayName, redactHome(err.Error()))
	e.reporter.report(serviceError{
		ID: pathFailedID(engine), Message: message,
		Severity: "warning", Action: "dismiss", EngineType: engine, Operation: "install",
	})
}

// redactHome keeps the user's home directory out of text that leaves this node.
func redactHome(text string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return text
	}
	return strings.ReplaceAll(text, home, "~")
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
