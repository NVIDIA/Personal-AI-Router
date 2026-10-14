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
	"mime"
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

// Ingress server limits. The ingress serves a handful of local producers, so
// the bounds are tight: a client that trickles a request, or parks an idle
// connection, must not pin a goroutine indefinitely. There
// is deliberately no write timeout: net/http starts that clock when the request
// headers are read and it covers the whole handler, so a broker write that
// takes longer than the timeout would drop the response while the frame still
// lands, and the producer would retry a frame that was already accepted.
const (
	localIngressBodyCap     = 1 << 20
	localIngressReadTimeout = 10 * time.Second
	localIngressIdleTimeout = 60 * time.Second
)

// errBrokerWrite marks an ingest failure on the UPWARD write to the broker (as
// opposed to a validation failure, which is the producer's fault). The local
// interface treats a severed broker as the shutdown signal; the ingress just
// reports 500 so the producer can retry after the restart.
var errBrokerWrite = errors.New("broker write failed")

// errIngressOrigin is a producer naming a node other than this one as the
// origin of a frame. The ingress reports workloads that run here; a frame for
// another node's workload would be re-asserted to peers as this node's own.
var errIngressOrigin = errors.New("originatedFrom must be empty or this node's UUID")

// localIngress is the optional plaintext loopback listener for third-party
// workload producers on THIS machine: an external scheduler, a local inference
// harness that routes around nvpair-proxy, anything that runs work PAIR should
// count and show. It shares the peer port's frame format and validation but
// not its trust model — it never leaves loopback and is off unless configured.
//
// A frame accepted here is treated as local origin: emitted UP to the broker as
// the translated workloads:upsert / workloads:remove, exactly as if one of
// nvpair-proxy's engine facades had produced it, and then tracked for re-sync
// and broadcast to pinned peers (Manager.ingestLocal). That upward emission is
// the difference from the stdio path (whose frames the broker has already
// applied before forwarding them here).
//
// Loopback is not a trust boundary against a browser: a web page can POST to
// 127.0.0.1, and a rebinding page can reach it under its own hostname. The
// handler therefore refuses anything a browser could send (an Origin header, a
// non-JSON content type, a non-loopback Host) before it reads the body.
type localIngress struct {
	addr   string
	ingest func(method string, params json.RawMessage) error
	ln     net.Listener
	srv    *http.Server
}

// newLocalIngress validates the bind address. Anything that is not loopback is
// refused: a network-reachable plaintext ingress would let any host on the LAN
// forge this node's workloads, which is precisely what the peer port's cluster
// mTLS exists to prevent.
func newLocalIngress(addr string, ingest func(string, json.RawMessage) error) (*localIngress, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("local ingress address %q: %w", addr, err)
	}
	if !isLoopbackHost(host) {
		return nil, fmt.Errorf("local ingress address %q is not loopback; refusing to bind", addr)
	}
	return &localIngress{addr: addr, ingest: ingest}, nil
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

func (li *localIngress) newServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc(eventsPath, li.handle)
	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: interNodeReadHeaderTimeout,
		ReadTimeout:       localIngressReadTimeout,
		IdleTimeout:       localIngressIdleTimeout,
	}
}

// Serve blocks until ctx is cancelled. Listen must have been called first.
func (li *localIngress) Serve(ctx context.Context) error {
	li.srv = li.newServer()
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

// hostAllowed reports whether a request's Host names this listener: a loopback
// name or IP literal carrying the bound port. A DNS-rebinding page arrives with
// its own hostname in Host, so anything else is refused.
func (li *localIngress) hostAllowed(hostport string) bool {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return false
	}
	_, boundPort, err := net.SplitHostPort(li.Addr())
	return err == nil && port == boundPort && isLoopbackHost(host)
}

// handle checks the request's headers before it reads a byte of the body:
// 405 for a method other than POST, 421 for a Host that is not this loopback
// listener, 403 for any Origin header (no legitimate producer is a browser page),
// and 415 for a content type other than application/json (which also keeps a
// cross-origin "simple" request from delivering a body at all).
func (li *localIngress) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !li.hostAllowed(r.Host) {
		li.reject(w, http.StatusMisdirectedRequest, "host must be a loopback name or address with the ingress port")
		return
	}
	if _, ok := r.Header["Origin"]; ok {
		li.reject(w, http.StatusForbidden, "requests carrying an Origin header are not accepted")
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		li.reject(w, http.StatusUnsupportedMediaType, "content type must be application/json")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, localIngressBodyCap))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			li.reject(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
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
	if err := li.ingest(msg.Method, msg.Params); err != nil {
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
	li.reject(w, http.StatusBadRequest, reason)
}

// reject answers a refused request and closes the connection after the answer.
// The header checks run before the body is read, and net/http drains an unread
// body on a keep-alive connection before it sends the reply, so a client that
// declares a body it never sends would otherwise wait out the read timeout for
// its answer.
func (li *localIngress) reject(w http.ResponseWriter, status int, reason string) {
	slog.Warn("local ingress request rejected", "status", status, "reason", reason)
	w.Header().Set("Connection", "close")
	http.Error(w, reason, status)
}

// parseIngressFrame validates a frame from a local producer. It is the ingress's
// trust boundary, applied on top of the checks every local frame gets
// (parseLocalFrame): the producer may only report workloads that run on this
// node, in a lifecycle state the rest of the system knows.
func parseIngressFrame(method string, params json.RawMessage, selfUUID string) (localFrame, error) {
	params, err := normalizeIngressParams(method, params, selfUUID)
	if err != nil {
		return localFrame{}, err
	}
	f, err := parseLocalFrame(method, params)
	if err != nil {
		return localFrame{}, err
	}
	if err := validateIngressFrame(f, selfUUID); err != nil {
		return localFrame{}, err
	}
	return f, nil
}

// validateIngressFrame checks the DECODED values of a frame, the ones that are
// emitted to the broker, tracked and sent to peers. It does not depend on how
// the producer spelt the JSON, so the rules hold even for an input that
// normalizeIngressParams did not anticipate.
func validateIngressFrame(f localFrame, selfUUID string) error {
	if f.wl != nil {
		if f.wl.OriginatedFrom != selfUUID {
			return errIngressOrigin
		}
		// Any state other than the three terminal ones is tracked as active with
		// no expiry, so a misspelt state would be re-asserted to peers forever.
		if !isWorkloadState(f.wl.State) {
			return fmt.Errorf("workloadInfo.state %q is not a known state", f.wl.State)
		}
		return nil
	}
	if f.removeNode != selfUUID {
		return errIngressOrigin
	}
	return nil
}

func isWorkloadState(s WorkloadState) bool {
	switch s {
	case StateQueued, StateRunning, StateCompleted, StateFailed, StateCancelled:
		return true
	}
	return false
}

// normalizeIngressParams fills originatedFrom with this node's UUID when a local
// producer left it empty, the same courtesy the broker extends to nvpair-proxy's
// workloads, and rejects any other value. Lifecycle frames carry it inside
// params.workloadInfo; removals carry it at the top level. The params are edited
// as generic JSON so this file never duplicates the Workload schema; unknown
// fields inside them are passed through unchanged.
func normalizeIngressParams(method string, params json.RawMessage, selfUUID string) (json.RawMessage, error) {
	var top map[string]json.RawMessage
	if len(params) > 0 {
		if err := json.Unmarshal(params, &top); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
	}
	if top == nil {
		top = map[string]json.RawMessage{}
	}
	self, err := json.Marshal(selfUUID)
	if err != nil {
		return nil, err
	}
	switch {
	case method == MethodRemove:
		if err := checkIngressOrigin(top["originatedFrom"], selfUUID); err != nil {
			return nil, err
		}
		top["originatedFrom"] = self
		return json.Marshal(top)
	case isLifecycleMethod(method):
		var info map[string]json.RawMessage
		if raw, ok := top["workloadInfo"]; ok && len(raw) > 0 {
			if err := json.Unmarshal(raw, &info); err != nil {
				return nil, fmt.Errorf("invalid params.workloadInfo: %w", err)
			}
		}
		if info == nil {
			return nil, fmt.Errorf("missing params.workloadInfo")
		}
		if err := checkIngressOrigin(info["originatedFrom"], selfUUID); err != nil {
			return nil, err
		}
		info["originatedFrom"] = self
		infoRaw, err := json.Marshal(info)
		if err != nil {
			return nil, err
		}
		top["workloadInfo"] = infoRaw
		return json.Marshal(top)
	}
	return nil, fmt.Errorf("unknown method: %s", method)
}

// checkIngressOrigin accepts an absent, null or empty origin, or this node's.
func checkIngressOrigin(raw json.RawMessage, selfUUID string) error {
	if isEmptyJSONString(raw) {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s != selfUUID {
		return errIngressOrigin
	}
	return nil
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
