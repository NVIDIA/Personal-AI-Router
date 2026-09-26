// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"nvpair-shared/cableprobe"
	"nvpair-shared/clustertrust"
)

const controlCablePortsPath = "/v1/cables/ports"
const cableFactsMaxBytes = 256 << 10

var errCableFactsStale = errors.New("current native physical-port facts are stale")

type cableFactsError struct {
	message     string
	unsupported bool
}

func (e *cableFactsError) Error() string     { return e.message }
func cableFactsFailure(message string) error { return &cableFactsError{message: message} }
func cableFactsMessage(err error) string {
	if errors.Is(err, errCableFactsStale) {
		return "The native port facts remained stale after a bounded refresh; review again."
	}
	var failure *cableFactsError
	if errors.As(err, &failure) {
		return failure.message
	}
	return "The paired participant connection or authentication is unavailable for current port facts."
}

// These are passive OS facts. Their timestamps never become run observations.
type cablePortSnapshot struct {
	NodeID     string            `json:"nodeId"`
	Principal  string            `json:"principal"`
	ObservedAt int64             `json:"observedAt"`
	AgeMs      int64             `json:"ageMs"`
	Ports      []cableprobe.Port `json:"ports"`
}

// Only this subset of the existing node-info contract is consumed. LLDP cache
// data, addresses, GPU information and supplied cluster claims are not targets.
type cableNodeInfo struct {
	HostUUID    string `json:"hostUuid"`
	Connections *struct {
		Source     string `json:"source"`
		Status     string `json:"status"`
		ObservedAt int64  `json:"observedAt"`
		Truncated  bool   `json:"truncated"`
		Interfaces []struct {
			Name         string `json:"name"`
			Index        int    `json:"index"`
			MAC          string `json:"mac"`
			Physical     bool   `json:"physical"`
			PhysicalPort *struct {
				Source   string `json:"source"`
				SwitchID string `json:"switchId"`
				PortName string `json:"portName"`
			} `json:"physicalPort"`
		} `json:"interfaces"`
	} `json:"connections"`
}

type cableLocalFacts struct {
	nodeID      string
	port        int
	http        *http.Client
	profileDir  string
	controlPort int
}

func newCableLocalFacts(nodeID string, port int) *cableLocalFacts {
	return &cableLocalFacts{nodeID: nodeID, port: port, http: &http.Client{
		Timeout: 2 * time.Second,
		// The parent supplies a port, never a URL. Bypass environment proxies.
		Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext},
	}}
}

func cableIdentifier(value string, max int) bool {
	if value == "" || len(value) > max || !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return false
	}
	for _, c := range value {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}

func cableInterfaceValid(iface cableprobe.Interface) bool {
	mac, err := net.ParseMAC(iface.MAC)
	return cableIdentifier(iface.Name, 128) && iface.Index > 0 && iface.Index <= 2147483647 &&
		err == nil && len(mac) == 6 && strings.EqualFold(mac.String(), iface.MAC) && mac[0]&1 == 0 && mac.String() != "00:00:00:00:00:00"
}

// Neither response redirects nor incomplete JSON can introduce another target.
func readCableJSON(ctx context.Context, client *http.Client, url string, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return errors.New("port facts request unavailable")
	}
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := bounded.Do(req)
	if err != nil {
		var networkError net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout()) {
			return cableFactsFailure("The current port-fact request exceeded its deadline.")
		}
		if errors.Is(err, context.Canceled) {
			return cableFactsFailure("The current port-fact request was cancelled.")
		}
		return cableFactsFailure("The paired participant connection or authentication is unavailable for current port facts.")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
			return &cableFactsError{message: "This paired participant does not expose the cable-port review capability.", unsupported: true}
		case http.StatusUnauthorized, http.StatusForbidden:
			return cableFactsFailure("The participant rejected paired access to its current port facts.")
		default:
			return cableFactsFailure(fmt.Sprintf("The participant port-fact service is unavailable (HTTP %d).", resp.StatusCode))
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, cableFactsMaxBytes+1))
	if err != nil || len(data) > cableFactsMaxBytes {
		return classifyCableFactsRead("invalid", cableFactsFailure("The current port-fact response is incomplete or exceeds its size bound."))
	}
	if json.Unmarshal(data, result) != nil {
		return classifyCableFactsRead("invalid", cableFactsFailure("The participant returned invalid current port facts."))
	}
	return nil
}

// Read the self leaf from the shared trust owner's existing TLS view. Only public
// DER is retained; the review neither reads trust files nor validates its own pins.
func cableSelfCertificate(mesh *clustertrust.Mesh, principal string) []byte {
	config, ok := mesh.ClientTLSConfig(principal)
	if !ok || len(config.Certificates) == 0 || len(config.Certificates[0].Certificate) == 0 {
		return nil
	}
	return bytes.Clone(config.Certificates[0].Certificate[0])
}

func (f *cableLocalFacts) snapshot(ctx context.Context, mesh *clustertrust.Mesh) (cablePortSnapshot, error) {
	if f == nil || !cableIdentifier(f.nodeID, 128) || f.port < 1 || f.port > 65535 {
		return cablePortSnapshot{}, errors.New("parent-owned node identity or node-info service is unavailable")
	}
	mesh.Refresh()
	principal := mesh.NodeUUID()
	selfCertificate := cableSelfCertificate(mesh, principal)
	if !mesh.Clustered() || !cableIdentifier(principal, 256) || len(selfCertificate) == 0 {
		return cablePortSnapshot{}, errors.New("this node is not a current paired cluster member")
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return cablePortSnapshot{}, err
		}
		var info cableNodeInfo
		if err := readCableJSON(ctx, f.http, "http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(f.port))+"/v1/node-info", &info); err != nil {
			return cablePortSnapshot{}, err
		}
		result, err := projectCablePorts(info, f.nodeID, principal, time.Now())
		mesh.Refresh()
		if !mesh.Clustered() || mesh.NodeUUID() != principal || !bytes.Equal(selfCertificate, cableSelfCertificate(mesh, principal)) {
			return cablePortSnapshot{}, classifyCableFactsRead("binding-changed", errors.New("cluster membership or certificate changed during the port read"))
		}
		if !errors.Is(err, errCableFactsStale) || attempt == 1 {
			return result, err
		}
		// A cache hit just below two seconds can expire in transit. Reread
		// only that valid-but-stale local response, once, in the same context.
	}
}

func projectCablePorts(info cableNodeInfo, nodeID, principal string, now time.Time) (cablePortSnapshot, error) {
	invalid := classifyCableFactsRead("invalid", errors.New("current complete native physical-port facts are unavailable"))
	c := info.Connections
	if info.HostUUID != nodeID || c == nil || c.Source != "host-os" || c.Status != "observed" ||
		c.Truncated || c.ObservedAt <= 0 || c.ObservedAt > now.UnixMilli() ||
		c.Interfaces == nil || len(c.Interfaces) > 128 {
		return cablePortSnapshot{}, invalid
	}
	result := cablePortSnapshot{NodeID: nodeID, Principal: principal, ObservedAt: c.ObservedAt,
		AgeMs: now.UnixMilli() - c.ObservedAt, Ports: []cableprobe.Port{}}
	groups := map[[2]string]int{}
	seenIndexes, seenNames := map[int]bool{}, map[string]bool{}
	for _, iface := range c.Interfaces {
		p := iface.PhysicalPort
		if p == nil {
			continue
		}
		alias := cableprobe.Interface{Name: iface.Name, Index: iface.Index, MAC: strings.ToLower(iface.MAC)}
		if !iface.Physical || p.Source != "linux-sysfs" || !cableIdentifier(p.SwitchID, 128) ||
			!cableIdentifier(p.PortName, 128) || !cableInterfaceValid(alias) || seenIndexes[alias.Index] || seenNames[alias.Name] {
			return cablePortSnapshot{}, invalid
		}
		seenIndexes[alias.Index], seenNames[alias.Name] = true, true
		key := [2]string{p.SwitchID, p.PortName}
		i, found := groups[key]
		if !found {
			i = len(result.Ports)
			groups[key] = i
			result.Ports = append(result.Ports, cableprobe.Port{SwitchID: p.SwitchID, PortName: p.PortName, Interfaces: []cableprobe.Interface{}})
		}
		result.Ports[i].Interfaces = append(result.Ports[i].Interfaces, alias)
		if len(result.Ports[i].Interfaces) > 4 {
			return cablePortSnapshot{}, classifyCableFactsRead("invalid", errors.New("a native physical port exceeds the supported four-alias bound"))
		}
	}
	// Malformed or mismatched facts remain non-retryable even when also old.
	if result.AgeMs >= 2000 {
		if len(result.Ports) == 0 {
			return cablePortSnapshot{}, invalid
		}
		return cablePortSnapshot{}, errCableFactsStale
	}
	for i := range result.Ports {
		sort.Slice(result.Ports[i].Interfaces, func(a, b int) bool { return result.Ports[i].Interfaces[a].Index < result.Ports[i].Interfaces[b].Index })
	}
	sort.Slice(result.Ports, func(i, j int) bool {
		a, b := result.Ports[i], result.Ports[j]
		if a.SwitchID == b.SwitchID {
			return a.PortName < b.PortName
		}
		return a.SwitchID < b.SwitchID
	})
	return result, nil
}

func (s *controlServer) handleCablePorts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	result, err := s.cableLocal.snapshot(ctx, s.mesh)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	s.mesh.Refresh()
	if _, pinned := s.mesh.VerifyClientPin(r); !pinned {
		http.Error(w, "forbidden: participant pairing changed during the read", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(result)
}
