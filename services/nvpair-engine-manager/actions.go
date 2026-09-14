// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Action runs a manifest-declared action against the engine and, when the
// action declares restart_after, restarts the engine on success.
func (e *Executor) Action(ctx context.Context, engine, action string, params json.RawMessage) (json.RawMessage, error) {
	st, err := e.state(engine)
	if err != nil {
		return nil, err
	}
	act, ok := st.manifest.Actions[action]
	if !ok {
		return nil, fmt.Errorf("engine %q has no action %q", engine, action)
	}
	res, err := e.dispatchAction(ctx, st, engine, action, act, params)
	if err != nil {
		return nil, err
	}
	// The restart gets the caller's context, not the action's: it is a stop plus
	// a readiness-probed start, which the action timeout does not budget for.
	if act.RestartAfter {
		if err := e.restartAfterAction(ctx, st, engine, action); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// restartAfterAction restarts a running engine after one of its actions
// succeeded (see Action.RestartAfter). A stopped engine has no serving state to
// reconcile, so it stays stopped. The restart failing is reported as the
// action failing: the action's effect is only observable once the engine comes
// back, and leaving it down silently would look like a successful no-op.
//
// This deliberately does not call Restart. Two reasons:
//
//   - Restart takes opMu itself, so the "is it running?" check would have to sit
//     outside the lock, and a Stop landing in that window would be undone — the
//     deletion would start an engine the user had just turned off. Holding opMu
//     across both makes the two orderings the only possible ones: a Stop either
//     wins the lock first (we observe stopped and leave it down) or waits and
//     then stops the engine we brought back.
//   - Restart ends with setDesiredEnabled(engine, true), which is the right
//     thing for a user-initiated restart and the wrong thing here: deleting a
//     model must not rewrite the engine's saved ON/OFF intent. doStop/doStart
//     are the intent-neutral pair (StopAll uses them for the same reason).
func (e *Executor) restartAfterAction(ctx context.Context, st *engineState, engine, action string) error {
	st.opMu.Lock()
	defer st.opMu.Unlock()
	st.mu.Lock()
	running := st.running
	st.mu.Unlock()
	if !running {
		return nil
	}
	slog.Info("restarting engine after action", "engine", engine, "action", action)
	if err := e.doStop(st, engine); err != nil {
		return fmt.Errorf("action %q: engine %q failed to restart: %w", action, engine, err)
	}
	if err := e.doStart(ctx, st, engine, startOpts{}); err != nil {
		return fmt.Errorf("action %q: engine %q failed to restart: %w", action, engine, err)
	}
	return nil
}

// dispatchAction invokes the action itself: a guarded filesystem removal, a CLI
// command, or an HTTP call against the engine's loopback control API with the
// caller's params as the body.
func (e *Executor) dispatchAction(ctx context.Context, st *engineState, engine, action string, act Action, params json.RawMessage) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, e.actionTimeout)
	defer cancel()
	st.mu.Lock()
	running := st.running
	port := st.port
	st.mu.Unlock()

	// CLI actions (act.Cmd) drive the engine's control binary, which
	// commonly works while the server is down (e.g. listing or pulling
	// models), so they are intentionally NOT gated on running. HTTP
	// actions hit the engine's loopback control API and therefore require
	// it to be up.
	if act.RemovePath != nil {
		return e.runRemovePathAction(ctx, st, act, params)
	}
	if len(act.Cmd) > 0 {
		return e.runCmdAction(ctx, st, act, port, params)
	}
	if !running {
		return nil, fmt.Errorf("engine %q is not running", engine)
	}

	path, err := resolvePlaceholders(act.HTTP.Path, map[string]string{"port": strconv.Itoa(port)})
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)

	body, err := actionBody(act, params)
	if err != nil {
		return nil, fmt.Errorf("action %q: %w", action, err)
	}
	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(act.HTTP.Method), url, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(engineIdentityProbeHeader, "1")
	client := e.client
	// A model load is slow on any engine: weights have to come off disk before
	// the first response header can be written. The engine's default 30s header
	// timeout is right for a control call and wrong for this one, so an action
	// declares long_running and gets the patient client. The Ollama pair below
	// predates the flag and is kept as-is so its manifest needs no change.
	if act.LongRunning && e.slowActionClient != nil {
		client = e.slowActionClient
	}
	if engine == "ollama" && action == "run_model" && e.slowActionClient != nil {
		client = e.slowActionClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("action %q: %w", action, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("action %q: engine returned HTTP %d: %s", action, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if len(data) == 0 {
		return json.RawMessage("null"), nil
	}
	if json.Valid(data) {
		return json.RawMessage(data), nil
	}
	wrapped, _ := json.Marshal(string(data))
	return wrapped, nil
}

// actionBody builds an HTTP action's request body. By default the caller's
// params are the body verbatim. A manifest that declares http.body instead
// supplies a fixed JSON document whose {placeholder} tokens are filled from
// those params, JSON-escaped, so an engine whose only way to perform an
// operation is a request the caller cannot compose (mlx-lm has no load
// endpoint, so load_model is a one-token chat completion) keeps that shape in
// the manifest rather than in every caller.
func actionBody(act Action, params json.RawMessage) (io.Reader, error) {
	if len(act.HTTP.Body) == 0 {
		if len(params) > 0 && string(params) != "null" {
			return bytes.NewReader(params), nil
		}
		return nil, nil
	}
	vars := map[string]string{}
	if len(params) > 0 {
		var pm map[string]any
		if err := json.Unmarshal(params, &pm); err == nil {
			for k, v := range pm {
				enc, err := json.Marshal(fmt.Sprint(v))
				if err != nil {
					continue
				}
				// Strip the quotes json.Marshal adds: the manifest template
				// carries them, so what is substituted is the escaped inner
				// text. Without this a model name holding a quote or
				// backslash would produce a malformed body.
				vars[k] = string(enc[1 : len(enc)-1])
			}
		}
	}
	resolved, err := resolvePlaceholders(string(act.HTTP.Body), vars)
	if err != nil {
		return nil, err
	}
	if !json.Valid([]byte(resolved)) {
		return nil, fmt.Errorf("http.body did not resolve to valid JSON")
	}
	return strings.NewReader(resolved), nil
}

// runRemovePathAction resolves templated path/root placeholders and deletes
// the target when it stays under the declared root.
func (e *Executor) runRemovePathAction(ctx context.Context, st *engineState, act Action, params json.RawMessage) (json.RawMessage, error) {
	if act.RemovePath == nil {
		return nil, fmt.Errorf("remove_path action missing spec")
	}
	vars := map[string]string{
		"install_dir": st.installDir,
		"models_dir":  engineModelsDir(st.manifest.Engine),
	}
	if len(params) > 0 {
		var pm map[string]any
		if err := json.Unmarshal(params, &pm); err == nil {
			for k, v := range pm {
				if allowedPlaceholders[k] {
					continue
				}
				vars[k] = fmt.Sprint(v)
			}
		}
	}
	root, err := resolvePlaceholders(act.RemovePath.Root, vars)
	if err != nil {
		return nil, err
	}
	root = expandPath(root)

	if act.ModelResolution == modelResolutionLMSDiskPath {
		cli := st.plat.Runtime.CLI
		targets, err := e.lmsDiskPathsToDelete(ctx, cli, root, vars["model"])
		if err != nil {
			return nil, err
		}
		if len(targets) == 0 {
			fallback := expandPath(filepath.Join(root, vars["model"]))
			if _, statErr := os.Stat(fallback); statErr == nil {
				targets = []string{fallback}
			} else if os.IsNotExist(statErr) {
				return nil, fmt.Errorf("delete_model: model %q not found on disk", vars["model"])
			} else {
				return nil, fmt.Errorf("delete_model: %w", statErr)
			}
		}
		for _, target := range targets {
			if err := safeRemoveUnderRoot(root, target); err != nil {
				return nil, err
			}
		}
		return json.RawMessage("null"), nil
	}

	target, err := resolvePlaceholders(act.RemovePath.Path, vars)
	if err != nil {
		return nil, err
	}
	target = expandPath(target)
	if err := safeRemoveUnderRoot(root, target); err != nil {
		return nil, err
	}
	return json.RawMessage("null"), nil
}

// runCmdAction executes a CLI action, templating the action's params as
// placeholders (e.g. {model}), and returns the command's stdout as the
// result (parsed as JSON when it is valid JSON, else a JSON string).
func (e *Executor) runCmdAction(ctx context.Context, st *engineState, act Action, port int, params json.RawMessage) (json.RawMessage, error) {
	// A caller param can never override a runner-owned placeholder
	// ({bin},{cli},{port},{download},{install_dir}): those keys are dropped
	// from params, then seeded from trusted values — so a malicious param
	// can't hijack argv[0]. Param values are also not env-expanded (only
	// the manifest-authored {cli} value is).
	vars := map[string]string{}
	if len(params) > 0 {
		var pm map[string]any
		if err := json.Unmarshal(params, &pm); err == nil {
			for k, v := range pm {
				if allowedPlaceholders[k] {
					continue
				}
				vars[k] = fmt.Sprint(v)
			}
		}
	}
	vars["port"] = strconv.Itoa(port)
	vars["install_dir"] = st.installDir
	vars["pair_bin"] = pairBinDir()
	if cli := st.plat.Runtime.CLI; cli != "" {
		// The manifest's cli may itself be templated ("{install_dir}/venv/bin/hf"
		// for an engine PAIR installs into its own directory, rather than a fixed
		// path like LM Studio's ~/.lmstudio/bin/lms). resolveArgs substitutes in a
		// single pass, so a {cli} that expands to another placeholder would reach
		// exec as a literal "{install_dir}/..." path. Resolve the runner-owned
		// values inside it first. expandPath still runs after, for ~ and $VAR.
		resolved, err := resolvePlaceholders(cli, map[string]string{
			"install_dir": st.installDir,
			"port":        strconv.Itoa(port),
		})
		if err != nil {
			return nil, fmt.Errorf("runtime.cli: %w", err)
		}
		vars["cli"] = expandPath(resolved)
	}

	// Most cmd actions run once with the params as given. An action that
	// opts into model_resolution expands {model} into ordered fallback
	// candidates (e.g. lms-get: as-given → Hub id → Hugging Face URL),
	// tried in turn until one resolves. Fallback only advances on a
	// resolution failure, never on a download/runtime error.
	candidates := []string{vars["model"]}
	lmsGet := act.ModelResolution == modelResolutionLMSGet
	if lmsGet {
		if c := lmsGetCandidates(vars["model"]); len(c) > 0 {
			candidates = c
		}
	}

	var out string
	var lastErr error
	for i, cand := range candidates {
		vars["model"] = cand
		argv, err := resolveArgs(act.Cmd, vars)
		if err != nil {
			return nil, err
		}
		// An lms-get download resumes on re-run, so a transient stall or
		// timeout is retried in place before we give up. A resolution
		// failure is *not* retried in place — it falls through to the next
		// source below (and runWithResume returns it immediately).
		if lmsGet {
			out, lastErr = e.runWithResume(ctx, argv)
		} else {
			out, lastErr = e.runCommandOutput(ctx, argv)
		}
		if lastErr == nil {
			break
		}
		if i == len(candidates)-1 || !isLMSResolveFailure(lastErr) {
			break
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("action command failed: %w", lastErr)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return json.RawMessage("null"), nil
	}
	if json.Valid([]byte(out)) {
		return json.RawMessage(out), nil
	}
	wrapped, _ := json.Marshal(out)
	return wrapped, nil
}

// runCommandOutput runs argv and returns its stdout; on failure it
// returns the error with stderr attached for diagnostics.
func (e *Executor) runCommandOutput(ctx context.Context, argv []string) (string, error) {
	if len(argv) == 0 {
		return "", nil
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	configureSysProcAttr(cmd)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// lmsGetResumeAttempts bounds how many times a transient `lms get` download
// failure is retried in place before giving up; lms get resumes a partial
// download on re-run, so a stalled or timed-out transfer recovers without
// the user re-issuing the pull. lmsGetResumeBackoff spaces the retries.
// Vars (not consts) so tests can shrink them. The whole sequence is still
// bounded by the caller's actionTimeout context.
var (
	lmsGetResumeAttempts = 3
	lmsGetResumeBackoff  = 2 * time.Second
)

// runWithResume runs argv like runCommandOutput but retries the same command
// in place on a transient download failure (see isLMSTransientDownloadError),
// up to lmsGetResumeAttempts and honoring ctx. Used only on the lms-get
// model-pull path, where re-running resumes the partial download. A
// resolution failure or any hard error returns immediately, so the caller's
// source-fallback (or final error) is reached.
func (e *Executor) runWithResume(ctx context.Context, argv []string) (string, error) {
	var out string
	var err error
	for attempt := 1; ; attempt++ {
		out, err = e.runCommandOutput(ctx, argv)
		if err == nil {
			return out, nil
		}
		if attempt >= lmsGetResumeAttempts || ctx.Err() != nil || !isLMSTransientDownloadError(err) {
			return out, err
		}
		slog.Warn("lms get download failed transiently; resuming",
			"attempt", attempt, "of", lmsGetResumeAttempts, "err", err)
		select {
		case <-ctx.Done():
			return out, err
		case <-time.After(lmsGetResumeBackoff):
		}
	}
}
