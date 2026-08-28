// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"

	"nvpair-shared/appdir"
	"nvpair-shared/errors"
)

const (
	ollamaHostAliasBlockedID = "ollama-proxy:ollama-host-alias-blocked"

	// These ports are fixed by the broker-owned service topology. Keep the
	// values here as the broker's single source and use them when registering
	// the corresponding workers as well as when reserving an OLLAMA_HOST alias.
	nodeInfoHTTPPort       = 14318
	errorsHTTPPort         = 14319
	workloadHTTPPort       = 14320
	clusterManagerHTTPPort = 14321

	lmstudioProxyPortFile = "lmstudio-proxy-port.json"
)

type ollamaHostAlias struct {
	Address          string
	AlternateAddress string
	Port             int
}

func (b *Broker) setOllamaHostAlias(alias ollamaHostAlias) {
	b.ollamaHostAliasMu.Lock()
	b.ollamaHostAlias = alias
	b.ollamaHostAliasMu.Unlock()
}

func (b *Broker) currentOllamaHostAlias() ollamaHostAlias {
	b.ollamaHostAliasMu.RLock()
	defer b.ollamaHostAliasMu.RUnlock()
	return b.ollamaHostAlias
}

func (b *Broker) beginOllamaProxyGeneration() (uint64, ollamaHostAlias) {
	b.ollamaHostAliasMu.Lock()
	defer b.ollamaHostAliasMu.Unlock()
	b.ollamaProxyGeneration++
	return b.ollamaProxyGeneration, b.ollamaHostAlias
}

func (b *Broker) currentOllamaProxyGeneration() uint64 {
	b.ollamaHostAliasMu.RLock()
	defer b.ollamaHostAliasMu.RUnlock()
	return b.ollamaProxyGeneration
}

// inheritedOllamaHostAlias interprets OLLAMA_HOST using Ollama's host/default
// port rules, then narrows it to the only endpoints NVPAIR may safely claim:
// local plaintext HTTP on a non-default port. It never resolves or contacts the
// configured host. Unspecified bind addresses are converted to their loopback
// client equivalents, matching Ollama's ConnectableHost behavior.
func inheritedOllamaHostAlias(raw string) (ollamaHostAlias, bool) {
	s := strings.Trim(strings.TrimSpace(raw), "\"'")
	defaultPort := "11434"
	scheme, hostport, hasScheme := strings.Cut(s, "://")
	if !hasScheme {
		scheme, hostport = "http", s
		if s == "ollama.com" {
			scheme, hostport = "https", "ollama.com:443"
		}
	} else {
		scheme = strings.ToLower(scheme)
		switch scheme {
		case "http":
			defaultPort = "80"
		case "https":
			defaultPort = "443"
		}
	}
	if scheme != "http" {
		return ollamaHostAlias{}, false
	}

	// A base path changes the request paths seen by the proxy, and credentials,
	// query strings, and fragments are not host configuration. Refuse those
	// shapes rather than claiming a port for a target we cannot faithfully
	// emulate.
	if strings.ContainsAny(hostport, "?#") {
		return ollamaHostAlias{}, false
	}
	authority, path, hasPath := strings.Cut(hostport, "/")
	if strings.Contains(authority, "@") || (hasPath && path != "") {
		return ollamaHostAlias{}, false
	}
	hostport = authority
	host, portText, err := net.SplitHostPort(hostport)
	if err != nil {
		host, portText = "127.0.0.1", defaultPort
		if ip := net.ParseIP(strings.Trim(hostport, "[]")); ip != nil {
			host = ip.String()
		} else if hostport != "" {
			host = hostport
		}
	}
	port64, err := strconv.ParseInt(portText, 10, 32)
	if err != nil || port64 < 1 || port64 > 65535 {
		// Ollama falls back to the scheme's default for an invalid port.
		port64, _ = strconv.ParseInt(defaultPort, 10, 32)
	}
	port := int(port64)
	if port == managedOllamaFacadePort {
		return ollamaHostAlias{}, false
	}

	localhost := strings.EqualFold(strings.TrimSuffix(host, "."), "localhost")
	bothLoopbacks := false
	switch {
	case host == "":
		bothLoopbacks = true
		host = "127.0.0.1"
	case localhost:
		// Resolve the conventional name deterministically. Binding the name
		// itself selects only one of its IPv4/IPv6 results, so reserve both
		// canonical families atomically in the proxy.
		bothLoopbacks = true
		host = "127.0.0.1"
	case net.ParseIP(host) != nil:
		ip := net.ParseIP(host)
		switch {
		case ip.IsUnspecified():
			// Go's wildcard TCP listener is dual-stack where the platform
			// supports it, including Ollama's supported Windows path. Mirror
			// both client-visible loopbacks rather than silently losing one
			// address family for 0.0.0.0, ::, or an empty host.
			bothLoopbacks = true
			host = "127.0.0.1"
		case ip.IsLoopback():
			host = ip.String()
		default:
			return ollamaHostAlias{}, false
		}
	default:
		return ollamaHostAlias{}, false
	}

	alias := ollamaHostAlias{Address: net.JoinHostPort(host, strconv.Itoa(port)), Port: port}
	if bothLoopbacks {
		alias.AlternateAddress = net.JoinHostPort("::1", strconv.Itoa(port))
	}
	return alias, true
}

// prepareOllamaHostAlias records the optional secondary endpoint before the
// proxy supervisor starts. The existing force_ports preference is the opt-out.
// A custom backend already using the requested port wins and is never moved.
func (b *Broker) prepareOllamaHostAlias(enabled bool, backendPort int) {
	b.setOllamaHostAlias(ollamaHostAlias{})
	if !enabled {
		b.forwardErrorsClear(ollamaHostAliasBlockedID)
		return
	}
	alias, ok := inheritedOllamaHostAlias(os.Getenv("OLLAMA_HOST"))
	if !ok {
		b.forwardErrorsClear(ollamaHostAliasBlockedID)
		return
	}
	if backendPort > 0 && alias.Port == backendPort {
		b.reportOllamaHostAliasBlocked(alias.displayAddress(),
			fmt.Sprintf("Ollama's backend is already configured on port %d", backendPort))
		return
	}
	enginePorts, err := b.configuredEnginePorts()
	if err != nil {
		b.reportOllamaHostAliasBlocked(alias.displayAddress(),
			fmt.Sprintf("configured engine ports could not be verified: %v", err))
		return
	}
	if backendPort > 0 {
		enginePorts[backendPort] = "ollama"
	}
	if reason := reservedOllamaHostAliasPort(alias.Port, enginePorts, configuredLMStudioProxyPort()); reason != "" {
		b.reportOllamaHostAliasBlocked(alias.displayAddress(), reason)
		return
	}
	b.setOllamaHostAlias(alias)
	b.forwardErrorsClear(ollamaHostAliasBlockedID)
	slog.Info("prepared inherited OLLAMA_HOST loopback alias", "address", alias.displayAddress())
}

func (a ollamaHostAlias) displayAddress() string {
	if a.AlternateAddress == "" {
		return a.Address
	}
	return a.Address + " and " + a.AlternateAddress
}

// prepareOllamaHostAliasCandidate runs after settings but before engine-manager
// starts. It reserves the candidate in engine-manager's startup flags so the
// remote-control endpoint cannot adopt the future proxy listener during the
// narrow startup window. Full engine/proxy collision validation runs after
// engine-manager is available and can clear this provisional reservation.
func (b *Broker) prepareOllamaHostAliasCandidate() {
	b.setOllamaHostAlias(ollamaHostAlias{})
	settings := b.getSettings()
	if settings == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcWorkerCallTimeout)
	defer cancel()
	result, rpcErr, err := settings.Call(ctx, "settings/get-force-ports", nil)
	if err != nil || rpcErr != nil {
		return
	}
	var policy struct {
		Value bool `json:"value"`
	}
	if json.Unmarshal(result, &policy) != nil || !policy.Value {
		return
	}
	alias, ok := inheritedOllamaHostAlias(os.Getenv("OLLAMA_HOST"))
	if !ok {
		return
	}
	b.setOllamaHostAlias(alias)
}

func (b *Broker) syncCurrentEngineOllamaHostAliasReservation() {
	b.ollamaHostAliasSyncMu.Lock()
	defer b.ollamaHostAliasSyncMu.Unlock()
	worker := b.getEngineMgr()
	if worker == nil {
		return
	}
	aliasPort := b.currentOllamaHostAlias().Port
	params, _ := json.Marshal(map[string]int{"port": aliasPort})
	ctx, cancel := context.WithTimeout(context.Background(), rpcWorkerCallTimeout)
	defer cancel()
	if _, rpcErr, err := worker.Call(ctx, "internal:set-reserved-port", params); err != nil || rpcErr != nil {
		slog.Warn("failed to synchronize OLLAMA_HOST alias reservation with engine-manager",
			"port", aliasPort, "err", err, "rpcErr", rpcErr)
	}
}

// availableOffOllamaHostAlias wraps a port-availability probe so managed
// ownership never plans a listener onto the alias endpoint. The alias is bound
// by the proxy after both facades are prepared, so a plain TCP probe still
// reports it free; treating it as taken is what keeps a backend move from
// landing on it and orphaning the alias.
func (b *Broker) availableOffOllamaHostAlias(available func(int) bool) func(int) bool {
	aliasPort := b.currentOllamaHostAlias().Port
	if aliasPort <= 0 {
		return available
	}
	return func(port int) bool {
		return port != aliasPort && available(port)
	}
}

func (b *Broker) releaseOllamaHostAliasReservation() {
	b.setOllamaHostAlias(ollamaHostAlias{})
	b.syncCurrentEngineOllamaHostAliasReservation()
}

func (b *Broker) releaseOllamaHostAliasReservationForGeneration(generation uint64) bool {
	b.ollamaHostAliasMu.Lock()
	if generation != b.ollamaProxyGeneration || b.ollamaHostAlias.Port == 0 {
		b.ollamaHostAliasMu.Unlock()
		return false
	}
	b.ollamaHostAlias = ollamaHostAlias{}
	b.ollamaHostAliasMu.Unlock()
	b.syncCurrentEngineOllamaHostAliasReservation()
	return true
}

func (b *Broker) disableOllamaHostAliasReservation() {
	b.releaseOllamaHostAliasReservation()
	b.forwardErrorsClear(ollamaHostAliasBlockedID)
}

// configuredEnginePorts reads every known engine's configured port before the
// alias is allowed to claim one. get-installed returns configured ports even
// while engines are stopped, closing the startup race where the alias binds
// first and desired-state restore can no longer start a bundled or custom
// manifest backend.
func (b *Broker) configuredEnginePorts() (map[int]string, error) {
	em := b.getEngineMgr()
	if em == nil {
		return nil, fmt.Errorf("engine manager is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcWorkerCallTimeout)
	defer cancel()
	result, rpcErr, err := em.Call(ctx, "engine:get-installed", nil)
	if err != nil {
		return nil, err
	}
	if rpcErr != nil {
		return nil, fmt.Errorf("engine:get-installed: %s", rpcErr.Message)
	}
	var response struct {
		Engines []struct {
			Engine string `json:"engine"`
			Port   int    `json:"port"`
		} `json:"engines"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return nil, fmt.Errorf("decode engine:get-installed: %w", err)
	}
	ports := make(map[int]string, len(response.Engines))
	for _, engine := range response.Engines {
		if engine.Port > 0 {
			ports[engine.Port] = engine.Engine
		}
	}
	return ports, nil
}

// configuredLMStudioProxyPort mirrors lmstudio-proxy's small persisted-port
// contract so the Ollama alias cannot win its bind race. Invalid/missing state
// means the proxy will use its default.
func configuredLMStudioProxyPort() int {
	path, err := appdir.Path(lmstudioProxyPortFile)
	if err != nil {
		return managedLMStudioFacadePort
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return managedLMStudioFacadePort
	}
	var stored struct {
		Port int `json:"port"`
	}
	if json.Unmarshal(data, &stored) != nil || stored.Port < 1 || stored.Port > 65535 {
		return managedLMStudioFacadePort
	}
	return stored.Port
}

func reservedOllamaHostAliasPort(port int, enginePorts map[int]string, lmstudioProxy int) string {
	if engine, ok := enginePorts[port]; ok {
		if engine == "" {
			engine = "an engine"
		}
		return fmt.Sprintf("%s backend is configured on port %d", engine, port)
	}
	switch port {
	case lmstudioProxy:
		return fmt.Sprintf("the LM Studio proxy is configured on port %d", port)
	case managedLMStudioFacadePort:
		return fmt.Sprintf("the LM Studio compatibility proxy uses port %d", port)
	case managedLMStudioBackendStart:
		// Managed LM Studio ownership is prepared after this check and moves a
		// colliding backend here, so the alias must not be sitting on it.
		return fmt.Sprintf("the managed LM Studio backend uses port %d", port)
	case nodeInfoHTTPPort:
		return fmt.Sprintf("the node-info service uses port %d", port)
	case errorsHTTPPort:
		return fmt.Sprintf("the errors service uses port %d", port)
	case workloadHTTPPort:
		return fmt.Sprintf("the workload service uses port %d", port)
	case clusterManagerHTTPPort:
		return fmt.Sprintf("the cluster manager uses port %d", port)
	case engineManagerHTTPPort:
		return fmt.Sprintf("the engine model service uses port %d", port)
	case engineControlPort:
		return fmt.Sprintf("the engine control service uses port %d", port)
	}
	return ""
}

func (b *Broker) reportOllamaHostAliasBlocked(address, reason string) {
	b.forwardErrorsReport(errors.ServiceError{
		ID: ollamaHostAliasBlockedID,
		Message: fmt.Sprintf(
			"NVPAIR kept its primary Ollama compatibility proxy separate, but could not claim the local OLLAMA_HOST alias %s: %s. Stop or reconfigure the application using that port, or change or unset OLLAMA_HOST, then restart NVPAIR. No process was stopped.",
			address, reason),
		Timestamp: nowMillis(),
		NodeID:    b.nodeID,
		Severity:  "warning",
		Action:    "none",
	})
}
