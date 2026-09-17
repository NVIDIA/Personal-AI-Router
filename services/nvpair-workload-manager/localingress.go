// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nvpair-shared/appdir"
)

// localIngressConfigFile is the app-data file that turns the local ingress on
// when the broker launches this worker without --local-ingress:
//
//	<appdir>/workload-ingress.json   {"listen": "127.0.0.1:14324"}
//
// It is file-registered the way an engine manifest is under <appdir>/engines/,
// so a third-party producer or an operator can enable it without rebuilding
// the desktop application. Absent, empty or malformed means off.
const localIngressConfigFile = "workload-ingress.json"

// errBrokerWrite marks an ingest failure on the UPWARD write to the broker (as
// opposed to a validation failure, which is the producer's fault). The local
// interface treats a severed broker as the shutdown signal; the ingress just
// reports 500 so the producer can retry after the restart.
var errBrokerWrite = errors.New("broker write failed")

// localIngress is the optional plaintext loopback listener for third-party
// workload producers on THIS machine: an external scheduler, a local inference
// harness that routes around the proxies, anything that runs work PAIR should
// count and show. It shares the peer port's frame format and validation but
// not its trust model — it never leaves loopback and is off unless configured.
//
// A frame accepted here is treated as local origin: tracked for re-sync,
// broadcast to pinned peers, and emitted UP to the broker as the translated
// workloads:upsert / workloads:remove, exactly as if one of the proxies had
// produced it. That upward emission is the difference from the stdio path
// (whose frames the broker has already applied before forwarding them here).
type localIngress struct {
	addr     string
	selfUUID string
	ingest   func(method string, params json.RawMessage) error
	ln       net.Listener
	srv      *http.Server
}

// newLocalIngress validates the bind address. Anything that is not loopback is
// refused: a network-reachable plaintext ingress would let any host on the LAN
// forge this node's workloads, which is precisely what the peer port's cluster
// mTLS exists to prevent.
func newLocalIngress(addr, selfUUID string, ingest func(string, json.RawMessage) error) (*localIngress, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("local ingress address %q: %w", addr, err)
	}
	if !isLoopbackHost(host) {
		return nil, fmt.Errorf("local ingress address %q is not loopback; refusing to bind", addr)
	}
	return &localIngress{addr: addr, selfUUID: selfUUID, ingest: ingest}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Listen binds the address. Kept separate from Serve so a caller (and the
// tests) can learn the bound port before serving, and so a bind failure is
// reported synchronously at startup rather than from a goroutine.
func (li *localIngress) Listen() error {
	ln, err := net.Listen("tcp", li.addr)
	if err != nil {
		return fmt.Errorf("local ingress listen on %s: %w", li.addr, err)
	}
	li.ln = ln
	return nil
}

// Addr is the bound address once Listen has run, else the configured one.
func (li *localIngress) Addr() string {
	if li.ln == nil {
		return li.addr
	}
	return li.ln.Addr().String()
}

// Serve blocks until ctx is cancelled. Listen must have been called first.
func (li *localIngress) Serve(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc(eventsPath, li.handle)
	li.srv = &http.Server{Handler: mux, ReadHeaderTimeout: interNodeReadHeaderTimeout}
	errCh := make(chan error, 1)
	go func() {
		slog.Info("local workload ingress listening (loopback, plaintext)", "addr", li.Addr())
		if err := li.srv.Serve(li.ln); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = li.srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

func (li *localIngress) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		li.badRequest(w, "read body: "+err.Error())
		return
	}
	var msg Message
	if err := json.Unmarshal(body, &msg); err != nil {
		li.badRequest(w, "invalid JSON: "+err.Error())
		return
	}
	if msg.JSONRPC != "2.0" {
		li.badRequest(w, "unsupported jsonrpc version")
		return
	}
	if !isLifecycleMethod(msg.Method) && msg.Method != MethodRemove {
		li.badRequest(w, "unknown method: "+msg.Method)
		return
	}
	params, err := stampOrigin(msg.Method, msg.Params, li.selfUUID)
	if err != nil {
		li.badRequest(w, err.Error())
		return
	}
	if err := li.ingest(msg.Method, params); err != nil {
		if errors.Is(err, errBrokerWrite) {
			slog.Error("local ingress could not reach the broker", "method", msg.Method, "err", err)
			http.Error(w, "broker unavailable", http.StatusInternalServerError)
			return
		}
		li.badRequest(w, err.Error())
		return
	}
	slog.Info("accepted local-ingress workload frame", "method", msg.Method)
	w.WriteHeader(http.StatusOK)
}

func (li *localIngress) badRequest(w http.ResponseWriter, reason string) {
	slog.Warn("local ingress request rejected", "status", http.StatusBadRequest, "reason", reason)
	http.Error(w, reason, http.StatusBadRequest)
}

// stampOrigin fills originatedFrom with this node's UUID when a local producer
// left it empty — the same courtesy the broker extends to the proxies. Lifecycle
// frames carry it inside params.workloadInfo; removals carry it at the top level.
// The params are edited as generic JSON so this file never duplicates the
// Workload schema, and an explicit origin is left untouched.
func stampOrigin(method string, params json.RawMessage, selfUUID string) (json.RawMessage, error) {
	var top map[string]json.RawMessage
	if len(params) > 0 {
		if err := json.Unmarshal(params, &top); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
	}
	if top == nil {
		top = map[string]json.RawMessage{}
	}
	quoted, err := json.Marshal(selfUUID)
	if err != nil {
		return nil, err
	}
	if method == MethodRemove {
		if isEmptyJSONString(top["originatedFrom"]) {
			top["originatedFrom"] = quoted
		}
		return json.Marshal(top)
	}
	var info map[string]json.RawMessage
	if raw, ok := top["workloadInfo"]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &info); err != nil {
			return nil, fmt.Errorf("invalid params.workloadInfo: %w", err)
		}
	}
	if info == nil {
		return nil, fmt.Errorf("missing params.workloadInfo")
	}
	if isEmptyJSONString(info["originatedFrom"]) {
		info["originatedFrom"] = quoted
	}
	infoRaw, err := json.Marshal(info)
	if err != nil {
		return nil, err
	}
	top["workloadInfo"] = infoRaw
	return json.Marshal(top)
}

// isEmptyJSONString is true for an absent field, JSON null, or "".
func isEmptyJSONString(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return true
	}
	var s string
	return json.Unmarshal(raw, &s) == nil && s == ""
}

// resolveLocalIngressAddr picks the ingress bind address: the flag when set,
// else <appdir>/workload-ingress.json beside the cluster dir, else "" (off).
func resolveLocalIngressAddr(flagValue, clusterDir string) string {
	if v := strings.TrimSpace(flagValue); v != "" {
		return v
	}
	base := ""
	if clusterDir != "" {
		base = filepath.Dir(clusterDir)
	} else if d, err := appdir.Dir(); err == nil {
		base = d
	}
	if base == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(base, localIngressConfigFile))
	if err != nil {
		return ""
	}
	var cfg struct {
		Listen string `json:"listen"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		slog.Warn("ignoring malformed local ingress config", "file", localIngressConfigFile, "err", err)
		return ""
	}
	return strings.TrimSpace(cfg.Listen)
}
