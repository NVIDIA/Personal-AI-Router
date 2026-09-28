// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"nvpair-shared/cableprobe"
	"nvpair-shared/clustertrust"
)

func localFactsPublicKey(t *testing.T) (ssh.PublicKey, diagnosticBootstrapPublicKey) {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return key, diagnosticBootstrapPublicKey{Algorithm: key.Type(), Blob: base64.StdEncoding.EncodeToString(key.Marshal()), Fingerprint: ssh.FingerprintSHA256(key)}
}

type localFactsConn struct {
	closed   bool
	deadline time.Time
}

func (*localFactsConn) Read([]byte) (int, error)  { return 0, io.EOF }
func (*localFactsConn) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (c *localFactsConn) Close() error            { c.closed = true; return nil }
func (*localFactsConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 40000}
}
func (*localFactsConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 22}
}
func (c *localFactsConn) SetDeadline(t time.Time) error  { c.deadline = t; return nil }
func (*localFactsConn) SetReadDeadline(time.Time) error  { return nil }
func (*localFactsConn) SetWriteDeadline(time.Time) error { return nil }

func TestDiagnosticMPILocalPublicExchangeStopsBeforeAuthentication(t *testing.T) {
	key, expected := localFactsPublicKey(t)
	for _, kind := range []string{"observed", "bad-signature", "wrong-address", "wrong-port", "unexpected-success"} {
		t.Run(kind, func(t *testing.T) {
			conn := &localFactsConn{}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result, err := exchangeDiagnosticMPILocalHostKey(ctx, "192.0.2.1", func(_ context.Context, network, address string) (net.Conn, error) {
				if network != "tcp" || address != "192.0.2.1:22" {
					t.Fatal("public observation widened its endpoint")
				}
				return conn, nil
			}, func(_ net.Conn, address string, config *ssh.ClientConfig) error {
				if len(config.Auth) != 0 || config.User != "pair-public-key-observation" || config.HostKeyCallback == nil || len(config.HostKeyAlgorithms) != 4 {
					t.Fatal("public observation acquired authentication or a generic host-key policy")
				}
				if kind == "bad-signature" {
					return errors.New("fixture signature verification failed before callback")
				}
				remote := conn.RemoteAddr()
				if kind == "wrong-address" {
					remote = &net.TCPAddr{IP: net.ParseIP("192.0.2.99"), Port: 22}
				}
				if kind == "wrong-port" {
					remote = &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 2222}
				}
				callbackErr := config.HostKeyCallback(address, remote, key)
				if callbackErr == nil {
					t.Fatal("observation allowed authentication to begin")
				}
				if kind == "unexpected-success" {
					return nil
				}
				return callbackErr
			})
			if !conn.closed || conn.deadline.IsZero() || time.Until(conn.deadline) > time.Second {
				t.Fatal("public observation lost its socket close or deadline")
			}
			if kind == "observed" {
				if err != nil || result != expected {
					t.Fatalf("public observation=%+v %v", result, err)
				}
			} else if err == nil || result != (diagnosticBootstrapPublicKey{}) {
				t.Fatalf("unconfirmed exchange produced public authority: %+v %v", result, err)
			}
		})
	}
}

func TestDiagnosticMPILocalInterfaceRequiresUniqueUpNativeAddress(t *testing.T) {
	mac, _ := net.ParseMAC("02:00:00:00:00:01")
	for _, kind := range []string{"up", "down", "loopback", "remote-address", "duplicate", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			interfaces := []net.Interface{{Name: "eth0", Index: 2, HardwareAddr: mac, Flags: net.FlagUp}}
			if kind == "down" {
				interfaces[0].Flags = 0
			}
			if kind == "loopback" {
				interfaces[0].Flags |= net.FlagLoopback
			}
			if kind == "duplicate" {
				interfaces = append(interfaces, net.Interface{Name: "eth1", Index: 3, HardwareAddr: mac, Flags: net.FlagUp})
			}
			result, err := diagnosticMPILocalInterface("192.0.2.1", interfaces, func(net.Interface) ([]net.Addr, error) {
				if kind == "unreadable" {
					return nil, errors.New("fixture unavailable")
				}
				address := "192.0.2.1"
				if kind == "remote-address" {
					address = "192.0.2.2"
				}
				return []net.Addr{&net.IPNet{IP: net.ParseIP(address), Mask: net.CIDRMask(24, 32)}}, nil
			})
			if kind == "up" {
				if err != nil || result.Name != "eth0" || result.Index != 2 || result.MAC != mac.String() {
					t.Fatalf("local interface=%+v %v", result, err)
				}
			} else if err == nil {
				t.Fatalf("%s was accepted as a local endpoint", kind)
			}
		})
	}
}

func TestDiagnosticMPILocalHostIdentityBindsAccountAndRechecksAfterExchange(t *testing.T) {
	for _, kind := range []string{"bound", "wrong-account", "public-identity", "changed-interface", "changed-certificate", "unconfirmed-key"} {
		t.Run(kind, func(t *testing.T) {
			d, _, request := closedRuntimeFixture(t, 3)
			adoption, err := d.adoptRuntime(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			target := adoption.Record.Targets[0]
			_, expected := localFactsPublicKey(t)
			reads, exchanges := 0, 0
			result, port, err := d.localMPIHostIdentityWithIO(context.Background(), target, func(diagnosticManagedTarget) (diagnosticMPILocalHostState, error) {
				reads++
				state := diagnosticMPILocalHostState{UID: 1000, User: "fixture", Home: "/home/fixture", PublicIdentitySHA256: strings.Repeat("a", 64), Interface: cableprobe.Interface{Name: "eth0", Index: 2, MAC: "02:00:00:00:00:01"}}
				if kind == "wrong-account" {
					state.User = "foreign-user"
				}
				if kind == "public-identity" {
					state.PublicIdentitySHA256 = strings.Repeat("b", 64)
				}
				if kind == "changed-interface" && reads > 1 {
					state.Interface.Index = 3
				}
				return state, nil
			}, func(ctx context.Context, address string) (diagnosticBootstrapPublicKey, error) {
				exchanges++
				if address != target.Address {
					t.Fatal("local observation dialed another participant")
				}
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 5*time.Second {
					t.Fatal("local public observation was unbounded")
				}
				if kind == "changed-certificate" {
					d.m.mesh = clustertrust.Open(t.TempDir())
				}
				if kind == "unconfirmed-key" {
					return diagnosticBootstrapPublicKey{}, nil
				}
				return expected, nil
			})
			if kind == "bound" {
				if err != nil || result != expected || port != 22 || reads != 2 || exchanges != 1 {
					t.Fatalf("local binding=%+v %d %v", result, port, err)
				}
			} else if err == nil || port != 0 || result != (diagnosticBootstrapPublicKey{}) {
				t.Fatalf("%s minted local key authority", kind)
			}
			if (kind == "wrong-account" || kind == "public-identity") && exchanges != 0 {
				t.Fatal("unbound account reached public exchange")
			}
		})
	}
}

func TestDiagnosticMPIFactsPeerListIsClosedAndLegacyCompatible(t *testing.T) {
	base := diagnosticMPIFactsRequest{SSHAddress: "192.0.2.1", PeerAddress: "192.0.2.2"}
	for _, kind := range []string{"legacy", "two-peers", "empty", "too-many", "duplicate", "self", "first-mismatch", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			request := base
			switch kind {
			case "two-peers":
				request.PeerAddresses = []string{"192.0.2.2", "192.0.2.3"}
			case "empty":
				request.PeerAddresses = []string{}
			case "too-many":
				request.PeerAddresses = []string{"192.0.2.2", "192.0.2.3", "192.0.2.4"}
			case "duplicate":
				request.PeerAddresses = []string{"192.0.2.2", "192.0.2.2"}
			case "self":
				request.PeerAddresses = []string{"192.0.2.2", "192.0.2.1"}
			case "first-mismatch":
				request.PeerAddresses = []string{"192.0.2.3", "192.0.2.2"}
			case "invalid":
				request.PeerAddresses = []string{"192.0.2.2", "unbound-host"}
			}
			peers, err := diagnosticMPIFactsPeers(request)
			if kind == "legacy" || kind == "two-peers" {
				if err != nil || peers[0] != base.PeerAddress {
					t.Fatalf("peer list: %v %v", peers, err)
				}
			} else if err == nil {
				t.Fatalf("%s peer list accepted", kind)
			}
		})
	}
}

func TestDiagnosticMPIFactsEverySelectedPeerMustShareOneRoute(t *testing.T) {
	for _, kind := range []string{"same", "interface", "source", "identity", "GPU", "effects"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			target := diagnosticManagedTarget{NodeID: "node-a", Principal: "node-a"}
			_, err := readDiagnosticMPIInspections(context.Background(), target, strings.Repeat("a", 64), []string{"192.0.2.2", "192.0.2.3"}, func(_ context.Context, input []byte) ([]byte, error) {
				calls++
				var request map[string]string
				_ = json.Unmarshal(input, &request)
				identity := map[string]any{"nodeId": "node-a", "principal": "node-a", "publicIdentitySha256": strings.Repeat("a", 64)}
				route := map[string]any{"peerAddress": request["peerAddress"], "sourceAddress": "192.0.2.1", "interface": "eth0"}
				gpu := map[string]any{"uuid": "GPU-fixture", "gb10Observed": true, "utilizationPercent": 0}
				result := map[string]any{"schemaVersion": 1, "action": "inspect", "effectsApplied": false, "executable": false, "identity": identity, "observations": map[string]any{"gpu": gpu, "gpuProcesses": map[string]any{"processes": []any{}}, "route": route}}
				if calls == 2 {
					switch kind {
					case "interface":
						route["interface"] = "eth1"
					case "source":
						route["sourceAddress"] = "192.0.2.9"
					case "identity":
						identity["nodeId"] = "foreign"
					case "GPU":
						gpu["uuid"] = "GPU-other"
					case "effects":
						result["effectsApplied"] = true
					}
				}
				return json.Marshal(result)
			})
			if calls != 2 || (kind == "same") != (err == nil) {
				t.Fatalf("%s routes: calls=%d error=%v", kind, calls, err)
			}
		})
	}
}
