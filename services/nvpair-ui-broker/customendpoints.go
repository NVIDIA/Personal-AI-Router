// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Custom-endpoint configuration plane.
//
// This mirrors manualnodes.go in shape and diverges from it in exactly one
// load-bearing way: ownership. For manual nodes the nvpair-manual-nodes
// worker is the source of truth and the broker only persists what the worker
// accepted. For Custom endpoints the broker owns the config; the prober is a
// probe cache. Consequences: persistence decides the response (a down or
// missing prober must not fail a valid add — the entry still becomes probeable
// when the worker respawns and this file is replayed), the API key persists in
// this file (0600, the only sanctioned store besides in-memory request
// plumbing), and it rides the local broker↔worker stdio link on add, remove,
// and replay. A peer-driven add (customremote.go) crosses the pinned mTLS
// control channel to reach this file — sanctioned request plumbing, not an
// event — and a key is never a log line or an event payload either way.
//
// Port policy lives here rather than in the form: the port is a mandatory,
// explicit field, never inferred from the scheme or lifted out of the URL when
// the field is missing, and an endpoint whose host is loopback and whose port
// is one this node's facades serve is refused outright (self-forward loop;
// the proxy's own self/alias guard is defense-in-depth, not permission).
//
// Mutation serialization deliberately reuses manualMutationMu: Custom adds and
// removes arrive on the same client link and drive the same prober worker, and
// one serialization stream for all external-endpoint mutations keeps the
// worker's view monotonically consistent with the store without a second
// mutex ordering to reason about.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"nvpair-shared/appdir"
	"nvpair-shared/noderec"
)

// persistedCustomEndpoint is one row of custom-endpoints.json. ID is only
// populated when the user supplied one; the routing/probe identity is
// key(), which derives from BaseURL otherwise, so editing an endpoint (which
// is remove-then-re-add, there being no "off" state to toggle) cannot
// accidentally fork a second entry by changing only the display name.
type persistedCustomEndpoint struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
	BaseURL string `json:"baseUrl"`
	APIKey  string `json:"apiKey,omitempty"`
}

func (e persistedCustomEndpoint) key() string {
	if e.ID != "" {
		return e.ID
	}
	return "custom:" + e.BaseURL
}

// customEndpointView is the client-facing projection of an entry. KeySet
// reports credential presence without carrying the credential. Healthy is
// nil while the broker holds no probe verdict for the entry — "never
// checked" is spelled null on the wire, which the desktop renders as "Not
// checked yet" and a confirmed false renders as "Not working". Models ride
// only alongside a healthy verdict: a down endpoint's last-seen list must
// not keep claiming service.
type customEndpointView struct {
	ID          string   `json:"id"`
	Name        string   `json:"name,omitempty"`
	BaseURL     string   `json:"baseUrl"`
	KeySet      bool     `json:"keySet"`
	Healthy     *bool    `json:"healthy"`
	LastChecked string   `json:"lastChecked,omitempty"`
	Models      []string `json:"models,omitempty"`
}

// customProbeRecord is the broker's memory of one endpoint's latest probe
// verdict, keyed by endpoint key. It exists because a client's read must be
// answerable without waiting on a probe cycle; it is display truth only.
// Routing keeps consuming the same custom/updated events through the bridge
// below — the cache changes what custom/list says, never what may be routed.
type customProbeRecord struct {
	healthy   bool
	models    []string
	checkedAt time.Time
}

// recordCustomProbeState, forgetCustomProbeState, and peekCustomProbeState
// guard the verdict cache. A down verdict is stored with nil models so an
// endpoint that previously answered cannot keep showing its old list while
// it is down. The map is lazily created: a bare &Broker{} (the test
// harnesses) must stay usable without a constructor pass.

func (b *Broker) recordCustomProbeState(id string, rec customProbeRecord) {
	b.customProbeMu.Lock()
	defer b.customProbeMu.Unlock()
	if b.customProbeState == nil {
		b.customProbeState = make(map[string]customProbeRecord)
	}
	b.customProbeState[id] = rec
}

func (b *Broker) forgetCustomProbeState(id string) {
	b.customProbeMu.Lock()
	defer b.customProbeMu.Unlock()
	delete(b.customProbeState, id)
}

func (b *Broker) peekCustomProbeState(id string) (customProbeRecord, bool) {
	b.customProbeMu.Lock()
	defer b.customProbeMu.Unlock()
	rec, ok := b.customProbeState[id]
	return rec, ok
}

func (b *Broker) customEndpointViewOf(e persistedCustomEndpoint) customEndpointView {
	view := customEndpointView{
		ID:      e.key(),
		Name:    e.Name,
		BaseURL: e.BaseURL,
		KeySet:  e.APIKey != "",
	}
	if rec, ok := b.peekCustomProbeState(view.ID); ok {
		healthy := rec.healthy
		view.Healthy = &healthy
		view.LastChecked = rec.checkedAt.Format(time.RFC3339)
		if rec.healthy && len(rec.models) > 0 {
			view.Models = append([]string(nil), rec.models...)
		}
	}
	return view
}

func defaultCustomEndpointsPath() string {
	if path, err := appdir.Path("configs", "custom-endpoints.json"); err == nil {
		return path
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "configs", "custom-endpoints.json")
	}
	return filepath.Join("configs", "custom-endpoints.json")
}

func loadPersistedCustomEndpoints(path string) ([]persistedCustomEndpoint, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []persistedCustomEndpoint
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	valid := entries[:0]
	for _, entry := range entries {
		if entry.BaseURL != "" {
			valid = append(valid, entry)
		}
	}
	return valid, nil
}

func writePersistedCustomEndpoints(path string, entries []persistedCustomEndpoint) error {
	sort.Slice(entries, func(i, j int) bool { return entries[i].key() < entries[j].key() })
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	// 0600, not the 0644 a default write would give: this file is the one
	// sanctioned home for an endpoint credential.
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// customAddParams is the custom/add wire shape. URL and Port are separate
// fields on purpose (the desktop form's two inputs): the broker composes the
// canonical baseUrl, so a user's paste-in URL port can never quietly win
// over or lose to the explicit port field except by exact agreement.
// ReplaceId names the entry an edit is editing (an editor opened on a
// missing entry is refused, never silently turned into a fork); KeyAction is
// the editor's credential intent, where only "clear" is valid.
type customAddParams struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	URL       string `json:"url"`
	Port      int    `json:"port"`
	APIKey    string `json:"apiKey,omitempty"`
	ReplaceId string `json:"replaceId,omitempty"`
	KeyAction string `json:"keyAction,omitempty"`
}

// customAddRejection is a refused custom/add: a request-shape answer (bad
// keyAction, unknown replaceId, malformed endpoint address) that the local
// caller replies as -32602 and the peer relay settles as the same text. A
// persistence failure is deliberately not one of these: that stays -32000 /
// the opaque cross-network store error, because the request was fine.
type customAddRejection struct{ msg string }

func (e customAddRejection) Error() string { return e.msg }

func isCustomAddRejection(err error) bool {
	var rejection customAddRejection
	return errors.As(err, &rejection)
}

// customAddRejectf builds a rejection from a format string, keeping every
// validation message in the shape the pre-edit contract already used.
func customAddRejectf(format string, args ...any) error {
	return customAddRejection{msg: fmt.Sprintf(format, args...)}
}

// Dispatch for the three custom/* methods lives in customremote.go: a
// peer-addressed request forks onto the relay, everything else lands on the
// runners below. The broker is the config's source of truth, so these stay
// broker-local rather than relayed to a worker; the worker traffic they
// generate is a follow-up nudge, not the operation.

// customListSnapshot projects the persisted config into client views, each
// merged with the cached probe verdict for its key (nil healthy when the
// prober has never answered for it). The local list reply and a peer-driven
// list share it so both answer from identical truth.
func (b *Broker) customListSnapshot() ([]customEndpointView, error) {
	b.customPersistMu.Lock()
	entries, err := loadPersistedCustomEndpoints(b.customEndpointsPath)
	b.customPersistMu.Unlock()
	if err != nil {
		return nil, err
	}
	views := make([]customEndpointView, 0, len(entries))
	for _, entry := range entries {
		views = append(views, b.customEndpointViewOf(entry))
	}
	return views, nil
}

func (b *Broker) customList(msg *Message) {
	views, err := b.customListSnapshot()
	if err != nil {
		_ = b.codec.RespondError(msg.ID, -32000, "custom-endpoints store unreadable: "+err.Error())
		return
	}
	// Always answer with a (possibly empty) array, never null: the desktop
	// renders the list straight out of this payload.
	_ = b.codec.Respond(msg.ID, views)
}

// customRefresh answers the desktop's Refresh (and the peer relay's
// op:"refresh"): one fresh probe cycle for the addressed scope, then the
// enriched snapshot. Unlike custom/list it does not trust the cache alone —
// the point of pressing Refresh is to re-ask the endpoint right now. With no
// prober attached there is nothing to ask, so it answers from the cache
// rather than hanging; with one, RelayRequest resolves asynchronously (peer
// close included), so the stdio read loop never waits on a probe cycle.
func (b *Broker) customRefresh(msg *Message) {
	var params struct {
		ID string `json:"id"`
	}
	if len(msg.Params) > 0 {
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			_ = b.codec.RespondError(msg.ID, -32602, "custom/refresh: expected {\"id\": \"...\"}")
			return
		}
	}
	b.refreshCustomSnapshot(b.getManualNodes(), params.ID, func(views []customEndpointView, err error) {
		if err != nil {
			_ = b.codec.RespondError(msg.ID, -32000, "custom-endpoints store unreadable: "+err.Error())
			return
		}
		_ = b.codec.Respond(msg.ID, views)
	})
}

// refreshCustomSnapshot runs the probe-then-snapshot pipeline shared by the
// local custom/reply path and the peer relay: ask the prober for a fresh
// scoped snapshot, merge what it answers into the verdict cache, then build
// the reply from persisted config merged with that cache. Any prober failure
// (down, error reply, write failure) degrades to a cache-answered reply —
// Refresh must never fail a read because re-probing was impossible.
func (b *Broker) refreshCustomSnapshot(worker *rpcWorker, id string, done func([]customEndpointView, error)) {
	replyFromCache := func() {
		views, err := b.customListSnapshot()
		done(views, err)
	}
	if worker == nil {
		replyFromCache()
		return
	}
	// The prober reads the same "id" spelling; no scope sends an empty
	// params object, which it reads as "probe every tracked endpoint".
	var params any = map[string]any{}
	if id != "" {
		params = map[string]string{"id": id}
	}
	raw, err := json.Marshal(params)
	if err != nil {
		replyFromCache()
		return
	}
	if relayErr := worker.RelayRequest("custom/probe", raw, func(result json.RawMessage, rpcErr *RPCError, err error) {
		if err != nil {
			slog.Warn("custom probe refresh failed", "err", err)
		} else if rpcErr != nil {
			slog.Warn("custom probe refresh rejected", "code", rpcErr.Code, "msg", rpcErr.Message)
		} else {
			b.mergeCustomProbeSnapshot(result)
		}
		replyFromCache()
	}); relayErr != nil {
		slog.Warn("custom probe refresh failed", "err", relayErr)
		replyFromCache()
	}
}

// mergeCustomProbeSnapshot folds one custom/probe reply into the verdict
// cache. Only ids that are still persisted configuration are recorded: an
// endpoint removed (or replaced onto another key) mid-probe must not
// resurrect as an entry or a claim, and unconfigured ids would grow the
// cache forever. A down verdict stores nil models, matching the event path.
func (b *Broker) mergeCustomProbeSnapshot(raw json.RawMessage) {
	var reply struct {
		Endpoints []customEndpointStatus `json:"endpoints"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil || len(reply.Endpoints) == 0 {
		return
	}
	b.customPersistMu.Lock()
	entries, err := loadPersistedCustomEndpoints(b.customEndpointsPath)
	b.customPersistMu.Unlock()
	if err != nil {
		return
	}
	configured := make(map[string]bool, len(entries))
	for _, entry := range entries {
		configured[entry.key()] = true
	}
	checkedAt := time.Now().UTC()
	for _, endpoint := range reply.Endpoints {
		if !configured[endpoint.ID] {
			continue
		}
		rec := customProbeRecord{healthy: endpoint.Healthy, checkedAt: checkedAt}
		if endpoint.Healthy {
			rec.models = append([]string(nil), endpoint.Models...)
		}
		b.recordCustomProbeState(endpoint.ID, rec)
	}
}

// applyCustomEdit is the add/edit pipeline shared by the local custom/add
// reply path and the peer relay, so a remote edit advances store, prober
// cache, and candidate bookkeeping exactly like a local one. The worker is
// looked up before the mutex (same read the manual-node relay does, so this
// adds no new mutex ordering).
//
// Key resolution follows the editor's intent, in this order: a typed apiKey
// sets the credential; keyAction "clear" deletes it; a blank key field on an
// edit carries over the replaced entry's stored key. The desktop form never
// round-trips the secret, so "blank" must mean keep — reading it as drop
// would disarm the endpoint on every unrelated edit. Fresh adds are
// unchanged: a blank key means none.
//
// replaceId must resolve to persisted config (by key or explicit id) or the
// request is refused: a stale editor fails loudly rather than fork a second
// entry. When the edit changes the composed key, delete-old-plus-insert-new
// is one atomic mutation under manualMutationMu, the old key's routing
// candidate is retracted, and the prober is told custom-remove-then-
// custom-add so a respawned prober can never track both addresses. An edit
// that rewires the endpoint without changing its composed key — a credential
// rotation, or an address change under an explicit id — retracts that key's
// routing candidate here too. The prober's replaced entry re-reports only
// when a later probe succeeds (its change-gate compares against the reset
// unprobed state, so a failing reconfiguration emits no event at all), and a
// candidate bridged from the old target or credential must not outlive the
// configuration that produced it. The next successful probe re-bridges from
// the new one.
func (b *Broker) applyCustomEdit(params customAddParams, done func(customEndpointView, error)) {
	if action := params.KeyAction; action != "" && action != "clear" {
		done(customEndpointView{}, customAddRejection{`custom/add: unknown keyAction "` + action + `" (only "clear" is valid)`})
		return
	}
	entry, err := b.composeCustomEndpoint(params)
	if err != nil {
		done(customEndpointView{}, err)
		return
	}
	worker := b.getManualNodes()

	b.manualMutationMu.Lock()

	oldKey := ""
	// rewired marks a target-or-credential change: the condition for
	// retiring the endpoint's current routing candidate, independent of
	// whether the composed key moved with it.
	rewired := false
	if id := strings.TrimSpace(params.ReplaceId); id != "" {
		b.customPersistMu.Lock()
		entries, loadErr := loadPersistedCustomEndpoints(b.customEndpointsPath)
		b.customPersistMu.Unlock()
		if loadErr != nil {
			b.manualMutationMu.Unlock()
			done(customEndpointView{}, loadErr)
			return
		}
		var previous *persistedCustomEndpoint
		for i := range entries {
			if entries[i].key() == id || entries[i].ID == id {
				previous = &entries[i]
				break
			}
		}
		if previous == nil {
			b.manualMutationMu.Unlock()
			done(customEndpointView{}, customAddRejection{"custom/add: replaceId does not match a configured endpoint"})
			return
		}
		switch {
		case entry.APIKey != "":
		case params.KeyAction == "clear":
			entry.APIKey = ""
		default:
			entry.APIKey = previous.APIKey
		}
		if previous.key() != entry.key() {
			oldKey = previous.key()
		}
		rewired = previous.BaseURL != entry.BaseURL || previous.APIKey != entry.APIKey
	}

	// A persisted edit is new configuration. An entry's probe verdict belongs
	// to the endpoint that produced it, so stale records are evicted before
	// the reply is built — neither this reply nor the next list may show the
	// previous configuration's health or model list. The follow-up probe (the
	// nudges below, or the editor's refresh) re-establishes the truth.
	var writeErr error
	if oldKey != "" {
		writeErr = b.replaceCustomEndpoint(oldKey, entry)
	} else {
		writeErr = b.putCustomEndpoint(entry)
	}
	if writeErr != nil {
		b.manualMutationMu.Unlock()
		done(customEndpointView{}, writeErr)
		return
	}
	b.forgetCustomProbeState(entry.key())
	if oldKey != "" {
		b.forgetCustomProbeState(oldKey)
		b.dropCustomCandidate(oldKey)
	}
	if rewired {
		b.dropCustomCandidate(entry.key())
	}
	view := b.customEndpointViewOf(entry)

	// The prober forwards are advisory by design: the entry is configured the
	// moment it is persisted, and whether it is *routable* is decided by the
	// next probe. Failing the caller because a nudge did not land would read
	// as "your endpoint was not saved" when it was, so the forwards' only
	// job is to skip the up-to-10s wait for the next scheduled cycle; a lost
	// nudge costs latency, not correctness.
	if oldKey != "" {
		b.forwardToProber(worker, "custom/remove", map[string]string{"id": oldKey}, func() {
			b.forwardToProber(worker, "custom/add", entry, func() {
				defer b.manualMutationMu.Unlock()
				done(view, nil)
			})
		})
		return
	}
	b.forwardToProber(worker, "custom/add", entry, func() {
		defer b.manualMutationMu.Unlock()
		done(view, nil)
	})
}

func (b *Broker) customAdd(msg *Message) {
	var params customAddParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		_ = b.codec.RespondError(msg.ID, -32602, "custom/add: "+err.Error())
		return
	}
	b.applyCustomEdit(params, func(view customEndpointView, err error) {
		if err != nil {
			if isCustomAddRejection(err) {
				_ = b.codec.RespondError(msg.ID, -32602, err.Error())
			} else {
				_ = b.codec.RespondError(msg.ID, -32000, "custom-endpoints persistence failed: "+err.Error())
			}
			return
		}
		_ = b.codec.Respond(msg.ID, view)
	})
}

// applyCustomRemove deletes the entry keyed by id and retracts any candidate
// it was bridged as, then runs done. Shared by the local reply path and the
// peer relay; a no-op (unknown id) answers done(false, nil) so both callers
// keep the idempotent contract.
func (b *Broker) applyCustomRemove(id string, done func(removed bool, err error)) {
	worker := b.getManualNodes()

	b.manualMutationMu.Lock()
	key, removed, err := b.removeCustomEndpoint(id)
	if err != nil {
		b.manualMutationMu.Unlock()
		done(false, err)
		return
	}
	if !removed {
		b.manualMutationMu.Unlock()
		// Idempotent: removing an id this node never configured is a no-op
		// answer, not an error, so a client retry converges quietly.
		done(false, nil)
		return
	}
	b.dropCustomCandidate(key)
	// Removal evicts the verdict with the config: re-adding the same key is
	// new configuration and must not inherit the old endpoint's health.
	b.forgetCustomProbeState(key)
	b.forwardToProber(worker, "custom/remove", map[string]string{"id": key}, func() {
		defer b.manualMutationMu.Unlock()
		done(true, nil)
	})
}

func (b *Broker) customRemove(msg *Message) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil || params.ID == "" {
		_ = b.codec.RespondError(msg.ID, -32602, "custom/remove requires an id")
		return
	}
	b.applyCustomRemove(params.ID, func(removed bool, err error) {
		if err != nil {
			_ = b.codec.RespondError(msg.ID, -32000, "custom-endpoints persistence failed: "+err.Error())
			return
		}
		_ = b.codec.Respond(msg.ID, map[string]bool{"removed": removed})
	})
}

// composeCustomEndpoint validates the wire params and builds the persisted
// row, composing the canonical baseUrl (scheme://host:port/path) from the
// separate url and port fields. Every rejection is a -32602 from the caller's
// point of view: nothing about these inputs is guessable and nothing here
// fills in a default the user did not state.
func (b *Broker) composeCustomEndpoint(params customAddParams) (persistedCustomEndpoint, error) {
	if params.Port <= 0 || params.Port > 65535 {
		return persistedCustomEndpoint{}, customAddRejectf("custom/add requires an explicit port (never inferred): got %d", params.Port)
	}
	raw := strings.TrimSpace(params.URL)
	if raw == "" {
		return persistedCustomEndpoint{}, customAddRejectf("custom/add requires a url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return persistedCustomEndpoint{}, customAddRejectf("custom/add: url: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return persistedCustomEndpoint{}, customAddRejectf("custom/add: scheme must be http or https, got %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return persistedCustomEndpoint{}, customAddRejectf("custom/add: url has no host")
	}
	if urlPort := u.Port(); urlPort != "" {
		parsed, err := strconv.Atoi(urlPort)
		if err != nil || parsed != params.Port {
			return persistedCustomEndpoint{}, customAddRejectf("custom/add: port field (%d) disagrees with the url's port (%s); supply one or make them match", params.Port, urlPort)
		}
	}
	if isLoopbackHost(host) && b.frontsFacadePort(params.Port) {
		return persistedCustomEndpoint{}, customAddRejectf("custom/add: %s:%d points at a PAIR facade port on this node; an endpoint may not forward to its own proxy", host, params.Port)
	}
	authority := host
	if strings.Contains(authority, ":") {
		authority = "[" + authority + "]"
	}
	baseURL := u.Scheme + "://" + authority + ":" + strconv.Itoa(params.Port) + u.EscapedPath()

	entry := persistedCustomEndpoint{
		Name:    strings.TrimSpace(params.Name),
		BaseURL: baseURL,
	}
	if id := strings.TrimSpace(params.ID); id != "" {
		entry.ID = id
	}
	if key := strings.TrimSpace(params.APIKey); key != "" {
		entry.APIKey = key
	}
	return entry, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// frontsFacadePort reports whether a port is a facade port on this node: an
// engine's stock facade default, a relocation override planned for the next
// spawn, or a port a live facade is currently bound to. Checked regardless
// of bind state: a self-forward request is wrong whether or not the facade
// happens to be up right now, and resolving hostnames is out of scope — this
// only has to catch loopback literals aimed at our own ports, which is where
// the footgun lives.
func (b *Broker) frontsFacadePort(port int) bool {
	for _, profile := range engineProxyProfiles {
		if profile.FacadePort == port {
			return true
		}
		if runtime := b.engineProxy(profile); runtime != nil &&
			int(runtime.startupPort.Load()) == port {
			return true
		}
		if bound := b.engineProxyListenPort(profile); bound == port {
			return true
		}
	}
	return false
}

func (b *Broker) putCustomEndpoint(entry persistedCustomEndpoint) error {
	b.customPersistMu.Lock()
	defer b.customPersistMu.Unlock()
	entries, err := loadPersistedCustomEndpoints(b.customEndpointsPath)
	if err != nil {
		return err
	}
	replaced := false
	for i := range entries {
		if entries[i].key() == entry.key() {
			entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, entry)
	}
	return writePersistedCustomEndpoints(b.customEndpointsPath, entries)
}

// replaceCustomEndpoint retires the entry keyed by oldKey and writes entry
// in its place under a single config read-write, so a cross-address edit is
// one atomic mutation: an entry that already occupies the new key is
// replaced beside the old one's removal, never forked next to it.
func (b *Broker) replaceCustomEndpoint(oldKey string, entry persistedCustomEndpoint) error {
	b.customPersistMu.Lock()
	defer b.customPersistMu.Unlock()
	entries, err := loadPersistedCustomEndpoints(b.customEndpointsPath)
	if err != nil {
		return err
	}
	out := make([]persistedCustomEndpoint, 0, len(entries))
	for _, existing := range entries {
		if existing.key() == oldKey || existing.key() == entry.key() {
			continue
		}
		out = append(out, existing)
	}
	return writePersistedCustomEndpoints(b.customEndpointsPath, append(out, entry))
}

// removeCustomEndpoint deletes the entry whose key, explicit ID, or display
// name matches id, mirroring the manual-node alias tolerance so a client
// holding any accepted spelling of the key can remove it.
func (b *Broker) removeCustomEndpoint(id string) (string, bool, error) {
	b.customPersistMu.Lock()
	defer b.customPersistMu.Unlock()
	entries, err := loadPersistedCustomEndpoints(b.customEndpointsPath)
	if err != nil {
		return "", false, err
	}
	out := entries[:0]
	removedKey := ""
	for _, entry := range entries {
		if entry.key() == id || entry.ID == id || entry.Name == id {
			removedKey = entry.key()
			continue
		}
		out = append(out, entry)
	}
	if removedKey == "" {
		return "", false, nil
	}
	if err := writePersistedCustomEndpoints(b.customEndpointsPath, out); err != nil {
		return "", false, err
	}
	return removedKey, true, nil
}

// forwardToProber pushes a custom/add|remove to the manual-nodes worker when
// one is supervised, then runs done with the mutation mutex still held —
// done releases it, so store and worker views advance strictly one mutation
// at a time, and a worker that is down or never answers delays rather than
// blocks the config path (RelayRequest resolves on peer close). params may
// carry the endpoint key: this crosses the local stdio link only, which is
// where the policy allows it.
func (b *Broker) forwardToProber(worker *rpcWorker, method string, params any, done func()) {
	if worker == nil {
		done()
		return
	}
	raw, err := json.Marshal(params)
	if err != nil {
		done()
		return
	}
	relayErr := worker.RelayRequest(method, raw, func(_ json.RawMessage, rpcErr *RPCError, err error) {
		if err != nil {
			slog.Warn("custom prober forward failed", "method", method, "err", err)
		} else if rpcErr != nil {
			slog.Warn("custom prober forward rejected", "method", method, "code", rpcErr.Code, "msg", rpcErr.Message)
		}
		done()
	})
	if relayErr != nil {
		slog.Warn("custom prober forward failed", "method", method, "err", relayErr)
		done()
	}
}

// replayPersistedCustomEndpoints re-pushes the persisted config into a
// (re)spawned manual-nodes worker, mirroring replayPersistedManualNodes so a
// respawned prober resumes against the same endpoints without a desktop
// round-trip.
func (b *Broker) replayPersistedCustomEndpoints(worker *rpcWorker) {
	b.manualMutationMu.Lock()
	defer b.manualMutationMu.Unlock()
	b.customPersistMu.Lock()
	entries, err := loadPersistedCustomEndpoints(b.customEndpointsPath)
	b.customPersistMu.Unlock()
	if err != nil {
		slog.Warn("failed to load persisted custom endpoints", "err", err)
		return
	}
	for _, entry := range entries {
		params, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		if _, rpcErr, err := worker.Call(context.Background(), "custom/add", params); err != nil {
			slog.Warn("failed to replay custom endpoint", "id", entry.key(), "err", err)
		} else if rpcErr != nil {
			slog.Warn("custom endpoint replay rejected", "id", entry.key(), "code", rpcErr.Code, "msg", rpcErr.Message)
		}
	}
}

// --- Routing bridge --------------------------------------------------------
//
// Probe results are the input to routability, and this is where they become
// routing: a healthy endpoint with models is issued into the custom facade's
// candidate set (the same node/add-manual mechanism manual nodes proved,
// including the probeMisses reset that restores the 401×3 budget after a
// flap) and held in the discovery store under the custom source. That slot
// is internal routing state, not a discovery entry: Snapshot() skips
// custom-only records, because the discovery wire lists machines and an
// endpoint is not one — projected, it surfaced in Add Node as an invitable
// "node" whose Invite would have attempted pairing against a facade port.
// The internal readers (claim bookkeeping, the advertised model union, the
// ingress backend set) go to the store directly, never through Snapshot.
// Anything else — unhealthy, model-less, or no longer configured — retracts
// the candidate instead of leaving a stale one. There is no enabled/disabled
// state to consult: configuration and health are the gate.
//
// The credential lives only in this hop. It is read from persisted config at
// bridge time (the probe event carries none, and the prober is not allowed to
// be a key carrier) and rides the local broker↔proxy stdio link, which policy
// sanctions for it; client events, the discovery wire, and logs never do.

// customProxyProfile is the Custom facade's broker-side profile. It exists
// whether or not the facade is currently spawned; the bridge no-ops when the
// handle is nil, same as every other profile-scoped call.
var customProxyProfile = mustEngineProxyProfile("custom")

// customEndpointStatus mirrors nvpair-manual-nodes' CustomEndpointStatus —
// the custom/updated and custom/removed payloads. There is deliberately no
// apiKey field: mirroring the producer means the broker cannot even
// accidentally receive a key on the event path.
type customEndpointStatus struct {
	ID      string   `json:"id"`
	Name    string   `json:"name,omitempty"`
	BaseURL string   `json:"baseUrl"`
	Healthy bool     `json:"healthy"`
	Models  []string `json:"models,omitempty"`
}

// customProxyCandidate mirrors nvpair-proxy's Node wire shape for
// node/add-manual (id/host/port/addresses + models + credential), so the
// proxy decodes it into the same Node its manual-node bridge defined.
type customProxyCandidate struct {
	ID        string   `json:"id"`
	Host      string   `json:"host"`
	Port      int      `json:"port"`
	Addresses []string `json:"addresses"`
	Models    []string `json:"models,omitempty"`
	APIKey    string   `json:"apiKey,omitempty"`
	Scheme    string   `json:"scheme,omitempty"`
}

// customToEnriched projects a bridged endpoint onto the discovery store. The
// endpoint key is the display ID, the independent record key, and the store
// key: it never rotates the way a learned hostUuid does, and the
// custom:<baseUrl>/explicit-id spelling cannot collide with the host-keyed
// records the scanner and manual feeds write. The record stays unverified —
// an endpoint URL proves no host identity — so the store keys it by
// DiscoveryRecordID exactly as it keys an unverified announcement or an
// unauthenticated manual entry, and never merges it into a peer's record.
func customToEnriched(status customEndpointStatus, host string, port int) EnrichedNode {
	return EnrichedNode{
		ID:                   status.ID,
		DiscoveryRecordID:    status.ID,
		IdentityVerification: noderec.IdentityUnverified,
		Host:                 host,
		Port:                 port,
		Addresses:            []string{host},
		Models:               status.Models,
		ModelsByEngine:       map[string][]string{"custom": status.Models},
	}
}

// handleCustomEndpointNotification consumes the prober's custom/updated and
// custom/removed events. It runs on the manual-nodes reader goroutine, like
// the manual-node path it shares an entry point with; the downward proxy call
// is the same bounded inline bridge, and custom events are change-gated at
// the prober, so they are rare by construction. Every event first updates the
// display cache (a removed id evicts its record; an unhealthy report replaces
// it with nil models) before the routing decision reads the same payload, so
// a client listing right after an event sees the verdict the event carried.
func (b *Broker) handleCustomEndpointNotification(method string, params json.RawMessage) {
	var status customEndpointStatus
	if err := json.Unmarshal(params, &status); err != nil {
		slog.Warn("manual-nodes emitted invalid custom payload", "method", method, "err", err)
		return
	}
	if status.ID == "" {
		slog.Warn("custom endpoint event missing id", "method", method)
		return
	}
	if method == "custom/removed" {
		b.forgetCustomProbeState(status.ID)
		b.dropCustomCandidate(status.ID)
		return
	}
	rec := customProbeRecord{healthy: status.Healthy, checkedAt: time.Now().UTC()}
	if status.Healthy {
		rec.models = append([]string(nil), status.Models...)
	}
	b.recordCustomProbeState(status.ID, rec)
	if !status.Healthy || len(status.Models) == 0 {
		b.dropCustomCandidate(status.ID)
		return
	}
	entry, configured := b.findCustomEndpoint(status.ID)
	if !configured {
		// The config is the truth about what may be routed. An update naming
		// an endpoint the user removed (an in-flight probe racing the remove)
		// resolves to the same retract as an unhealthy report, so a straggler
		// event can never resurrect a candidate the config already revoked.
		b.dropCustomCandidate(status.ID)
		return
	}
	host, port, scheme, err := customEndpointTarget(entry.BaseURL)
	if err != nil {
		slog.Warn("custom endpoint baseUrl is not routable", "id", status.ID, "err", err)
		b.dropCustomCandidate(status.ID)
		return
	}
	b.store.Upsert(customToEnriched(status, host, port), sourceCustom)
	b.callProxyManual(b.customProxyHandle(), "custom", "node/add-manual", customProxyCandidate{
		ID:        status.ID,
		Host:      host,
		Port:      port,
		Addresses: []string{host},
		Models:    status.Models,
		APIKey:    entry.APIKey,
		Scheme:    scheme,
	}, status.ID)
	slog.Debug("custom endpoint bridged", "id", status.ID, "healthy", status.Healthy,
		"models", len(status.Models), "keySet", entry.APIKey != "")
}

// dropCustomCandidate retracts a bridged endpoint from the store and the
// custom facade's candidate set. It fires only when a candidate was actually
// bridged — the store's custom slot is the record of that — so steady-state
// drops (a remove for something never routed, an idempotent retry) keep the
// wire quiet.
func (b *Broker) dropCustomCandidate(key string) {
	if !b.store.hasClaim(key, sourceCustom) {
		return
	}
	b.store.Remove(key, sourceCustom)
	b.callProxyManual(b.customProxyHandle(), "custom", "node/remove-manual", map[string]string{"id": key}, key)
}

// customProxyHandle returns the live proxy process fronting the Custom
// facade, or nil when it is not enabled or not up (every bridge call is a
// no-op against nil).
func (b *Broker) customProxyHandle() *proxyProcess {
	return b.engineProxyHandle(customProxyProfile)
}

// findCustomEndpoint looks up the persisted entry whose key matches id.
// Read-only and brief: it is the credential's only sanctioned home besides
// the in-flight payload it builds.
func (b *Broker) findCustomEndpoint(id string) (persistedCustomEndpoint, bool) {
	b.customPersistMu.Lock()
	defer b.customPersistMu.Unlock()
	entries, err := loadPersistedCustomEndpoints(b.customEndpointsPath)
	if err != nil {
		return persistedCustomEndpoint{}, false
	}
	for _, entry := range entries {
		if entry.key() == id {
			return entry, true
		}
	}
	return persistedCustomEndpoint{}, false
}

// customEndpointTarget splits a canonical baseUrl (composed by
// composeCustomEndpoint, so it always carries scheme, host, and explicit
// port) into the dial fields the proxy candidate needs.
func customEndpointTarget(baseURL string) (host string, port int, scheme string, err error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", 0, "", err
	}
	host = u.Hostname()
	port, err = strconv.Atoi(u.Port())
	if err != nil || host == "" || port <= 0 || port > 65535 {
		return "", 0, "", fmt.Errorf("base url %q has no explicit host:port", baseURL)
	}
	return host, port, u.Scheme, nil
}
