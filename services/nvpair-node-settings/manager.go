// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"nvpair-shared/applog"
)

// Version is stamped at build time via -ldflags "-X main.Version=...".
// See versions.json at the repo root for the source of truth.
var Version = "dev"

// Settings is the on-disk schema. This whole surface is still
// SCAFFOLDING: auto_join_invites and the opaque cluster_secret were
// dropped; cluster_id and cluster_friendly_name took their place.
// How each field is used end-to-end depends on the final node-joining
// and clustering model. The shape is in place so the UI, advertiser,
// and broker can be wired up; semantics will land later.
//
// Adding a field is non-breaking on disk — clients that don't know
// about it ignore it on read, and unknown JSON keys are dropped on
// load. Removing a field is also non-breaking — stale keys on disk
// are quietly discarded on the next save (this is how cluster_secret,
// cluster_auto_sync's old companion auto_join_invites, and the
// previous cluster_identity derivation all disappeared without a
// migration step).
type Settings struct {
	// ForcePorts enables managed compatibility-port ownership. Consumers must
	// leave running or unidentified owners untouched and surface a blocked
	// state; this setting never grants generic process-kill authority.
	ForcePorts bool `json:"force_ports"`

	// ClusterAutoSync: if true, this node accepts automatic syncs
	// of cluster-managed state (model lists, engine settings, etc.)
	// from peers. Set-from-another-node-capable in the eventual
	// design; today the datastore doesn't distinguish remote vs.
	// local callers.
	ClusterAutoSync bool `json:"cluster_auto_sync"`

	// ClusterID is the stable identifier for the cluster this node
	// belongs to. Empty string means "not in a cluster". The exact
	// format (UUID? hash? human-typed?) is owned by the security /
	// node-joining design; for now the datastore treats it as an
	// opaque string. The setter emits a connection/cluster-identity
	// push so live consumers update without polling.
	ClusterID string `json:"cluster_id"`

	// ClusterFriendlyName is the human-presentable label for the
	// cluster (e.g. "Lab 3 desks"). Display-only; cluster_id is the
	// identity that anything operational keys off. No push — the
	// label is purely UI sugar and a getter call on first paint is
	// enough.
	ClusterFriendlyName string `json:"cluster_friendly_name"`
}

func defaultSettings() Settings {
	return Settings{ForcePorts: true}
}

// ReadyParams is the payload of the "ready" notification we emit on
// startup. Matches the shape used by every other NVPAIR subprocess so
// the supervising broker's existing event readers don't need to
// special-case us.
type ReadyParams struct {
	Version string `json:"version"`
}

// ClusterIdentityParams is the payload of the connection/cluster-identity
// notification. Run() does NOT emit this on startup — only the "ready"
// notification fires there. The push lifecycle is change-only: the
// manager emits one frame after every successful
// settings/set-cluster-id, so React-init code that needs the current
// state before the first change calls settings/get-cluster-id once
// and lives on the push thereafter.
//
// The payload carries the raw cluster_id verbatim. Consumers that
// only care about "are we in a cluster?" derive that locally from
// `id != ""`; that derivation deliberately lives at every consumer
// rather than in this payload because the membership predicate may
// grow beyond a string-emptiness check once the security model
// lands, and embedding the boolean now would freeze the wrong
// answer.
type ClusterIdentityParams struct {
	ID string `json:"id"`
}

// ClusterAutoSyncParams is the payload of the connection/cluster-auto-sync
// notification. Same change-only lifecycle as ClusterIdentityParams:
// Run() does not emit this on startup; the manager pushes one frame
// after every successful settings/set-cluster-auto-sync. The payload
// is just the resolved bool — the UI doesn't need to re-fetch on
// every change. Any extra fields the eventual design requires can be
// added without breaking existing consumers.
type ClusterAutoSyncParams struct {
	Value bool `json:"value"`
}

type Manager struct {
	codec  *Codec
	cancel context.CancelFunc

	mu       sync.RWMutex
	path     string
	settings Settings
}

func NewManager(codec *Codec, path string) (*Manager, error) {
	m := &Manager{
		codec:    codec,
		path:     path,
		settings: defaultSettings(),
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	defer cancel()

	if err := m.codec.Notify("ready", ReadyParams{Version: Version}); err != nil {
		return fmt.Errorf("failed to send ready notification: %w", err)
	}

	return m.readLoop(ctx)
}

// emitClusterIdentity sends connection/cluster-identity with the
// current cluster_id. Callers that mutate the id should call this
// AFTER a successful save so the wire state is consistent with disk.
func (m *Manager) emitClusterIdentity() error {
	m.mu.RLock()
	id := m.settings.ClusterID
	m.mu.RUnlock()
	return m.codec.Notify("connection/cluster-identity", ClusterIdentityParams{ID: id})
}

// emitClusterAutoSync sends connection/cluster-auto-sync with the
// current boolean value. Same lifecycle conventions as
// emitClusterIdentity.
func (m *Manager) emitClusterAutoSync() error {
	m.mu.RLock()
	v := m.settings.ClusterAutoSync
	m.mu.RUnlock()
	return m.codec.Notify("connection/cluster-auto-sync", ClusterAutoSyncParams{Value: v})
}

// load reads the settings file from disk and merges it into
// m.settings. Three outcomes:
//
//   - Missing file → clean zero-values (first run).
//   - Well-formed file → parsed in; unknown JSON keys are dropped
//     (Go's default unmarshal behavior), so schema evolution is
//     non-breaking. This is how legacy keys like cluster_secret
//     and auto_join_invites are silently discarded when an
//     older settings.json is loaded after a schema change.
//   - Malformed file → renamed aside as `<path>.corrupt-<unix-ts>`
//     and we start with zero-values. This is the degraded-but-
//     responsive path: refusing to start (the previous behavior)
//     wedged every settings call in the UI behind a parse error
//     until the user located and deleted the file by hand. The
//     rename keeps a forensic copy so the user can still recover
//     their settings if they want to, but the app stays usable.
//     A failure to rename (e.g. EACCES from an AV scanner holding
//     a lock on the file) is logged loudly and we still proceed
//     with defaults — the next save() will overwrite the bad
//     file. Losing a few settings is preferable to a totally
//     unresponsive UI.
func (m *Manager) load() error {
	data, err := os.ReadFile(m.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Info("settings file not found, using defaults", "path", m.path)
			return nil
		}
		return fmt.Errorf("read %s: %w", m.path, err)
	}
	// Decode over product defaults so a missing force_ports key (including a
	// first-run or older partial file) enables the recommended managed-port
	// policy. An explicitly persisted false still wins and remains an opt-out.
	s := defaultSettings()
	if err := json.Unmarshal(data, &s); err != nil {
		backup := fmt.Sprintf("%s.corrupt-%d", m.path, time.Now().Unix())
		if renameErr := os.Rename(m.path, backup); renameErr != nil {
			slog.Error("settings file is malformed; failed to rename aside, starting with defaults",
				"path", m.path, "parse_err", err, "rename_err", renameErr)
		} else {
			slog.Warn("settings file was malformed; renamed aside and starting with defaults",
				"path", m.path, "backup", backup, "parse_err", err)
		}
		return nil
	}
	m.mu.Lock()
	m.settings = s
	m.mu.Unlock()
	slog.Info("settings loaded",
		"path", m.path,
		"force_ports", s.ForcePorts,
		"cluster_auto_sync", s.ClusterAutoSync,
		"has_cluster_id", s.ClusterID != "",
		"has_cluster_friendly_name", s.ClusterFriendlyName != "")
	return nil
}

// save serializes m.settings to disk. Equivalent to saveSettings with
// the current in-memory snapshot; kept as a thin wrapper so callers
// that just want "persist whatever's in memory" don't have to take
// their own snapshot.
func (m *Manager) save() error {
	m.mu.RLock()
	snapshot := m.settings
	m.mu.RUnlock()
	return m.saveSettings(snapshot)
}

// saveSettings persists the supplied Settings value via the standard
// write-to-tmp + os.Rename atomic-replace pattern. A crash mid-save
// leaves either the previous file intact or the new one fully
// written — never a torn half-file. The temp file lives next to the
// destination so the rename stays on the same filesystem (cross-FS
// rename is not atomic).
//
// Setters use this variant to apply the copy-then-save-then-commit
// pattern: serialize a candidate value, persist it, and only then
// flip the in-memory state. That way a save failure never leaves
// m.settings advertising a value the disk doesn't agree with.
//
// File and directory permissions are pinned at 0o700 / 0o600
// regardless of umask. Even though cluster_secret was removed (the
// previous hard requirement for tight permissions), the rest of the
// file is still per-user state and shouldn't be world-readable.
// os.Chmod after rename matters because
// os.WriteFile honors umask and Windows ignores the mode bits — we
// re-set them explicitly to keep both platforms aligned.
func (m *Manager) saveSettings(s Settings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}

	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir %s: %w", dir, err)
	}

	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, m.path); err != nil {
		// Best-effort cleanup so we don't leave a stray .tmp behind
		// when the rename fails (e.g. EACCES on Windows because some
		// AV scanner is holding a read lock on the destination).
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, m.path, err)
	}
	if err := os.Chmod(m.path, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", m.path, err)
	}
	return nil
}

func (m *Manager) readLoop(ctx context.Context) error {
	for {
		msg, err := m.codec.Read()
		if err != nil {
			if err == io.EOF || ctx.Err() != nil {
				return nil
			}
			log.Printf("JSON-RPC read error: %v", err)
			continue
		}
		m.handleMessage(msg)
		if ctx.Err() != nil {
			return nil
		}
	}
}

// boolValueParams and stringValueParams are the canonical params
// shapes for setter calls. Defined once so a malformed `value` field
// produces the same error regardless of which setter it hit.
type boolValueParams struct {
	Value *bool `json:"value"`
}

type stringValueParams struct {
	Value *string `json:"value"`
}

func (m *Manager) handleMessage(msg *Message) {
	if msg.Method == applog.SetLevelMethod {
		resolved, err := applog.HandleSetLevelParams(msg.Params)
		if msg.IsRequest() {
			if err != nil {
				m.codec.RespondError(msg.ID, -32602, err.Error())
				return
			}
			m.codec.Respond(msg.ID, map[string]string{"level": resolved})
		}
		if err != nil {
			slog.Warn("log/set-level rejected", "err", err)
		} else {
			slog.Info("log level changed", "level", resolved)
		}
		return
	}

	if !msg.IsRequest() {
		if msg.IsNotification() {
			log.Printf("ignoring incoming notification: %s", msg.Method)
		}
		return
	}

	switch msg.Method {
	case "settings/get-force-ports":
		m.mu.RLock()
		v := m.settings.ForcePorts
		m.mu.RUnlock()
		m.codec.Respond(msg.ID, map[string]bool{"value": v})

	case "settings/set-force-ports":
		var p boolValueParams
		if err := json.Unmarshal(msg.Params, &p); err != nil || p.Value == nil {
			m.codec.RespondError(msg.ID, -32602, `invalid params: expected {"value": <bool>}`)
			return
		}
		// Copy-then-save-then-commit: persist the candidate first
		// and only flip in-memory state on success, so a save
		// failure never leaves m.settings advertising a value the
		// disk doesn't agree with. handleMessage runs serially
		// (one frame at a time off readLoop), so no other setter
		// can race the snapshot/commit window here.
		m.mu.RLock()
		newSettings := m.settings
		m.mu.RUnlock()
		newSettings.ForcePorts = *p.Value
		if err := m.saveSettings(newSettings); err != nil {
			slog.Error("failed to persist force-ports", "err", err)
			m.codec.RespondError(msg.ID, -32603, "failed to persist setting: "+err.Error())
			return
		}
		m.mu.Lock()
		m.settings = newSettings
		m.mu.Unlock()
		m.codec.Respond(msg.ID, map[string]bool{"ok": true})

	case "settings/get-cluster-id":
		m.mu.RLock()
		id := m.settings.ClusterID
		m.mu.RUnlock()
		m.codec.Respond(msg.ID, map[string]string{"value": id})

	case "settings/set-cluster-id":
		var p stringValueParams
		if err := json.Unmarshal(msg.Params, &p); err != nil || p.Value == nil {
			m.codec.RespondError(msg.ID, -32602, `invalid params: expected {"value": <string>}`)
			return
		}
		m.mu.RLock()
		newSettings := m.settings
		m.mu.RUnlock()
		newSettings.ClusterID = *p.Value
		if err := m.saveSettings(newSettings); err != nil {
			slog.Error("failed to persist cluster-id", "err", err)
			m.codec.RespondError(msg.ID, -32603, "failed to persist setting: "+err.Error())
			return
		}
		m.mu.Lock()
		m.settings = newSettings
		m.mu.Unlock()
		m.codec.Respond(msg.ID, map[string]bool{"ok": true})
		// Fire the push AFTER acknowledging the request, so the
		// peer's "set completed" callback always observes a
		// notification strictly later than its own response. Emit
		// errors are non-fatal: the response went out, the file is
		// written, and the peer can re-query via
		// settings/get-cluster-id if the push happens to drop.
		if err := m.emitClusterIdentity(); err != nil {
			slog.Warn("failed to emit cluster-identity notification after set", "err", err)
		}

	case "settings/get-cluster-friendly-name":
		m.mu.RLock()
		name := m.settings.ClusterFriendlyName
		m.mu.RUnlock()
		m.codec.Respond(msg.ID, map[string]string{"value": name})

	case "settings/set-cluster-friendly-name":
		var p stringValueParams
		if err := json.Unmarshal(msg.Params, &p); err != nil || p.Value == nil {
			m.codec.RespondError(msg.ID, -32602, `invalid params: expected {"value": <string>}`)
			return
		}
		m.mu.RLock()
		newSettings := m.settings
		m.mu.RUnlock()
		newSettings.ClusterFriendlyName = *p.Value
		if err := m.saveSettings(newSettings); err != nil {
			slog.Error("failed to persist cluster-friendly-name", "err", err)
			m.codec.RespondError(msg.ID, -32603, "failed to persist setting: "+err.Error())
			return
		}
		m.mu.Lock()
		m.settings = newSettings
		m.mu.Unlock()
		m.codec.Respond(msg.ID, map[string]bool{"ok": true})
		// Intentional: no push. The friendly name is presentation
		// metadata; consumers that need live updates can either
		// subscribe to a future cluster-info push or re-fetch on a
		// settings:changed event from their parent.

	case "settings/get-cluster-auto-sync":
		m.mu.RLock()
		v := m.settings.ClusterAutoSync
		m.mu.RUnlock()
		m.codec.Respond(msg.ID, map[string]bool{"value": v})

	case "settings/set-cluster-auto-sync":
		var p boolValueParams
		if err := json.Unmarshal(msg.Params, &p); err != nil || p.Value == nil {
			m.codec.RespondError(msg.ID, -32602, `invalid params: expected {"value": <bool>}`)
			return
		}
		m.mu.RLock()
		newSettings := m.settings
		m.mu.RUnlock()
		newSettings.ClusterAutoSync = *p.Value
		if err := m.saveSettings(newSettings); err != nil {
			slog.Error("failed to persist cluster-auto-sync", "err", err)
			m.codec.RespondError(msg.ID, -32603, "failed to persist setting: "+err.Error())
			return
		}
		m.mu.Lock()
		m.settings = newSettings
		m.mu.Unlock()
		m.codec.Respond(msg.ID, map[string]bool{"ok": true})
		if err := m.emitClusterAutoSync(); err != nil {
			slog.Warn("failed to emit cluster-auto-sync notification after set", "err", err)
		}

	case "shutdown":
		if err := m.codec.Respond(msg.ID, nil); err != nil {
			log.Printf("failed to respond to shutdown: %v", err)
		}
		log.Println("shutdown requested via JSON-RPC")
		m.cancel()

	default:
		if err := m.codec.RespondError(msg.ID, -32601, fmt.Sprintf("method not found: %s", msg.Method)); err != nil {
			log.Printf("failed to send error response: %v", err)
		}
	}
}
