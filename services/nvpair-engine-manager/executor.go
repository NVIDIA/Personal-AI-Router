// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	settings "nvpair-shared/enginesettings"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	winEnvRe = regexp.MustCompile(`%([^%]+)%`)
	// Match named variables only so shell parameters such as $1 and $@ reach
	// manifest commands unchanged.
	unixEnvRe = regexp.MustCompile(`\$(?:\{[A-Za-z_][A-Za-z0-9_]*\}|[A-Za-z_][A-Za-z0-9_]*)`)
)

const (
	engineResponseHeaderTimeout     = 30 * time.Second
	ollamaLoadResponseHeaderTimeout = 10 * time.Minute
)

// EngineStatus is the snapshot returned by engine:status and
// engine:get-installed.
type EngineStatus struct {
	Engine      string `json:"engine"`
	DisplayName string `json:"display_name"`
	Installed   bool   `json:"installed"`
	Running     bool   `json:"running"`
	Healthy     bool   `json:"healthy"`
	Port        int    `json:"port,omitempty"`
}

// engineState is the per-engine runtime state.
type engineState struct {
	manifest   *Manifest
	plat       *Platform
	logs       *logBuffer
	installDir string
	// modelsDir is the resolved {models_dir}: the engine's model store, which no
	// removal this service performs may delete. Empty when the manifest declares
	// none, in which case uninstall has nothing to preserve.
	modelsDir string

	// opMu serializes lifecycle operations (install / start / stop /
	// restart / uninstall) for this engine, so concurrent calls can't
	// double-spawn or interleave. Lock order is opMu → mu; the per-op
	// st.mu is released around probes, and the background health loop
	// never takes opMu.
	opMu sync.Mutex

	mu         sync.Mutex
	installed  bool
	running    bool
	healthy    bool
	stopping   bool
	adopted    bool  // started externally; we can't stop it
	gen        int64 // bumped each start; gates stale health loops
	port       int
	binPath    string
	proc       *managedProc
	healthStop context.CancelFunc
	// startCancel lets StopAll unblock doStart before waiting on opMu.
	startCancel context.CancelFunc
}

// Executor owns engine lifecycle for every engine known on this host.
// Methods are synchronous and safe for concurrent use; the JSON-RPC
// layer runs the long ones (install, start) in goroutines so the read
// loop stays responsive.
type Executor struct {
	settingsHub      settings.Hub
	settingsParent   func(context.Context, string, settings.Request, string) (json.RawMessage, error)
	reg              *Registry
	reporter         *Reporter
	emit             func(method string, params any)
	client           *http.Client
	ollamaLoadClient *http.Client
	// progress fans install/pull progress to transient subscribers (the ec
	// streaming handlers) in addition to the local engine:install-progress
	// notification path. See progress.go.
	progress *progressHub
	baseDir  string // user-scoped install base
	// desired persists explicit per-engine ON/OFF intent. Runtime state remains
	// in-memory; shutdown cleanup must not rewrite this store.
	desired *desiredStateStore
	// overrideDir is the per-user manifest-override directory (the same
	// engines/ dir buildRegistry overlays). engine:set-port persists the
	// chosen port here as a manifest override. Empty disables persistence.
	overrideDir string
	// detectTimeout bounds the post-install/uninstall detect poll
	// (installers finish their file work asynchronously). Overridable.
	detectTimeout time.Duration
	// actionTimeout bounds ordinary engine actions and CLI-driven model pulls so
	// a hung engine can't park the goroutine or starve the caller forever.
	actionTimeout time.Duration
	// pullProgressTimeout bounds how long a streaming HTTP model pull may go
	// without advancing a layer/file byte count. Advancing progress refreshes
	// the deadline, allowing large active downloads to exceed actionTimeout.
	pullProgressTimeout time.Duration
	// pullStartTimeout lets an asynchronous pull finish its start handshake even
	// after caller cancellation, so an accepted download can still be stopped.
	pullStartTimeout time.Duration
	// pullCleanupTimeout bounds the inventory check and download stop together.
	pullCleanupTimeout time.Duration
	// loadedPollInterval is the cadence of the loaded-model watcher
	// (loadedwatch.go), which polls each running engine's resident set and emits
	// engine:models-changed on change. 0 disables it. Overridable via
	// --loaded-poll-interval.
	loadedPollInterval time.Duration
	// loadedPoke lets an explicit action (load/unload/pull/…) request an
	// immediate out-of-cycle loaded-set check instead of waiting for the next
	// tick. Buffered depth 1: a coalesced signal is enough.
	loadedPoke chan struct{}

	reservedPort atomic.Int32
	// StopAll is terminal for an Executor; the gate closes its start/snapshot race.
	shuttingDown atomic.Bool
	mu           sync.Mutex
	engines      map[string]*engineState
}

func NewExecutor(reg *Registry, reporter *Reporter, emit func(string, any), baseDir string) *Executor {
	return &Executor{
		reg:                 reg,
		reporter:            reporter,
		emit:                emit,
		client:              newEngineHTTPClient(engineResponseHeaderTimeout),
		ollamaLoadClient:    newEngineHTTPClient(ollamaLoadResponseHeaderTimeout),
		progress:            newProgressHub(),
		baseDir:             baseDir,
		desired:             newDesiredStateStore(baseDir),
		detectTimeout:       30 * time.Second,
		actionTimeout:       30 * time.Minute,
		pullProgressTimeout: 30 * time.Minute,
		pullStartTimeout:    30 * time.Second,
		pullCleanupTimeout:  5 * time.Second,
		loadedPollInterval:  defaultLoadedPollSeconds * time.Second,
		loadedPoke:          make(chan struct{}, 1),
		engines:             make(map[string]*engineState),
	}
}

func (e *Executor) SetReservedPort(port int) error {
	if port < 0 || port > 65535 {
		return fmt.Errorf("reserved port must be between 0 and 65535")
	}
	e.reservedPort.Store(int32(port))
	return nil
}

func (e *Executor) reservedPortError(port int) error {
	if port > 0 && int(e.reservedPort.Load()) == port {
		return fmt.Errorf("port %d is reserved by the inherited OLLAMA_HOST proxy alias", port)
	}
	return nil
}

// newEngineHTTPClient builds a client used for downloads, loopback actions,
// and probes. The ordinary shared client keeps a 30s response-header bound;
// Ollama's cold-load action gets a separate 10m client. Both set NO total
// http.Client.Timeout: a multi-GB engine download can legitimately run
// for many minutes and every call site already bounds total time with a
// context deadline or inactivity watchdog (install download 30m, streaming
// pull pullProgressTimeout, action actionTimeout, probe 3s). What
// it adds over the zero-value client is (a) a bounded response-header
// wait so a peer that accepts the connection but never replies can't park
// a goroutine even inside a long context, and (b) a redirect policy that
// refuses an https->plaintext downgrade so a checksum-pinned download URL
// can't be silently bounced onto http before its bytes are verified.
func newEngineHTTPClient(responseHeaderTimeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// DefaultTransport already bounds dial (30s), TLS handshake (10s), and
	// idle conns; it does NOT bound the response-header wait, so add that.
	tr.ResponseHeaderTimeout = responseHeaderTimeout
	return &http.Client{
		Transport:     tr,
		CheckRedirect: noDowngradeRedirect,
	}
}

// noDowngradeRedirect caps the redirect chain and forbids a redirect that
// drops from https to a non-https scheme. Loopback action/probe calls use
// http and never redirect, so they are unaffected; only an https download
// being bounced onto plaintext is rejected.
func noDowngradeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after %d redirects", len(via))
	}
	if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect from https to %q (downgrade)", req.URL.Scheme)
	}
	return nil
}

func (e *Executor) notify(method string, params any) {
	if e.emit != nil {
		e.emit(method, params)
	}
}

// state returns (lazily creating) the per-engine state for a known
// engine that has a block for the host os/arch.
func (e *Executor) state(engine string) (*engineState, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if st, ok := e.engines[engine]; ok {
		return st, nil
	}
	m, ok := e.reg.Get(engine)
	if !ok {
		return nil, fmt.Errorf("unknown engine %q", engine)
	}
	plat, ok := m.HostPlatform()
	if !ok {
		return nil, fmt.Errorf("engine %q has no platform block for %s/%s", engine, runtime.GOOS, runtime.GOARCH)
	}
	st := &engineState{
		manifest:   m,
		plat:       plat,
		logs:       newLogBuffer(),
		port:       plat.Runtime.Port,
		installDir: filepath.Join(e.baseDir, engine),
	}
	if plat.ModelsDir != "" {
		st.modelsDir = expandPath(plat.ModelsDir)
	}
	e.engines[engine] = st
	return st, nil
}

// pathVars are the directory placeholders every templated manifest string can
// use. Both are absolute by the time they get here, so a manifest never has to
// expand "~" itself — which it could not do, since only argv and a handful of
// named fields pass through expandPath.
func (st *engineState) pathVars() map[string]string {
	vars := map[string]string{"install_dir": st.installDir}
	// Omitted rather than empty when the manifest declares no store, so a
	// template that references it fails to resolve instead of quietly becoming
	// a path rooted at "/".
	if st.modelsDir != "" {
		vars["models_dir"] = st.modelsDir
	}
	return vars
}

// emitInstallProgress reports one install-progress step to both consumers: the
// local engine:install-progress notification (this node's UI) and the progress
// hub (so an ec streaming handler can relay it to a remote initiator). A pct of
// 0 marks a step with no measurable progress and is omitted from the wire.
func (e *Executor) emitInstallProgress(engine, stage string, pct int) {
	e.emitInstallFrame(engine, stage, pct, "")
}

// emitInstallFrame is emitInstallProgress with an optional failure reason,
// carried as "error" on the notification and Message on the hub event.
func (e *Executor) emitInstallFrame(engine, stage string, pct int, errMsg string) {
	params := map[string]any{"engine": engine, "stage": stage}
	if wirePercentIncluded(pct) {
		params["percent"] = pct
	}
	if errMsg != "" {
		params["error"] = errMsg
	}
	e.notify("engine:install-progress", params)
	e.progress.publish(ProgressEvent{Engine: engine, Op: "install", Stage: stage, Percent: pct, Message: errMsg})
}

// emitPullProgress reports one model-pull step to both consumers: the local
// engine:pull-progress notification (this node's UI) and the progress hub (so an
// ec streaming handler can relay it to a remote initiator). It mirrors
// emitInstallProgress; pull carries op/message so a local UI renders the same
// "Pulling · %" feed a remote pull already gets via engine:remote-progress.
func (e *Executor) emitPullProgress(ev ProgressEvent) {
	params := map[string]any{
		"engine": ev.Engine, "op": ev.Op, "stage": ev.Stage, "message": ev.Message,
	}
	if wirePercentIncluded(ev.Percent) {
		params["percent"] = ev.Percent
	}
	e.notify("engine:pull-progress", params)
	e.progress.publish(ev)
}

// expandPath resolves OS environment references in a path: Windows
// %VAR%, Unix $VAR/${VAR}, and a leading ~ for the home directory.
func expandPath(s string) string {
	return expandPathForOS(s, runtime.GOOS)
}

func expandPathForOS(s, goos string) string {
	if goos == "windows" {
		s = winEnvRe.ReplaceAllStringFunc(s, func(tok string) string {
			return os.Getenv(tok[1 : len(tok)-1])
		})
	} else {
		s = unixEnvRe.ReplaceAllStringFunc(s, func(tok string) string {
			name := tok[1:]
			if name[0] == '{' {
				name = name[1 : len(name)-1]
			}
			return os.Getenv(name)
		})
	}
	switch {
	case s == "~":
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
	case strings.HasPrefix(s, "~/") || strings.HasPrefix(s, `~\`):
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, s[2:])
		}
	}
	return s
}

func installFailedID(engine string) string { return "engine-manager:install-failed:" + engine }

func uninstallFailedID(engine string) string { return "engine-manager:uninstall-failed:" + engine }

func pullFailedID(engine, model string) string {
	return "engine-manager:pull-failed:" + engine + ":" + model
}

func startFailedID(engine string) string { return "engine-manager:start-failed:" + engine }

func unhealthyID(engine string) string { return "engine-manager:unhealthy:" + engine }

func exitedID(engine string) string { return "engine-manager:exited:" + engine }
