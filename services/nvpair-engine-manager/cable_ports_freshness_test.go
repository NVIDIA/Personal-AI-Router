// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
)

type cableFreshnessTransport func(*http.Request) (*http.Response, error)

func (f cableFreshnessTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func cableFreshnessFixture(t *testing.T, read cableFreshnessTransport) (*cableLocalFacts, *clustertrust.Mesh, string) {
	t.Helper()
	dir := t.TempDir()
	clustertrusttest.Join(t, dir, "freshness-cluster", "freshness-principal", "peer-principal")
	facts := &cableLocalFacts{nodeID: "freshness-host", port: 14318, http: &http.Client{Transport: read}}
	return facts, clustertrust.Open(dir), dir
}

func cableFreshnessInfo(t *testing.T, observedAt time.Time) cableNodeInfo {
	t.Helper()
	var info cableNodeInfo
	if err := json.Unmarshal([]byte(`{"hostUuid":"freshness-host","connections":{"source":"host-os","status":"observed","interfaces":[{"name":"eth-fixture","index":2,"mac":"02:00:00:00:00:01","physical":true,"physicalPort":{"source":"linux-sysfs","switchId":"001122334455","portName":"p0"}}]}}`), &info); err != nil {
		t.Fatal(err)
	}
	info.Connections.ObservedAt = observedAt.UnixMilli()
	return info
}

func cableFreshnessResponse(t *testing.T, info cableNodeInfo) *http.Response {
	t.Helper()
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}
}

func TestCableLocalFactsRetryCacheBoundaryOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		deadline, _ := ctx.Deadline()
		calls := 0
		facts, mesh, _ := cableFreshnessFixture(t, func(r *http.Request) (*http.Response, error) {
			calls++
			if r.Method != http.MethodGet || r.URL.String() != "http://127.0.0.1:14318/v1/node-info" || r.Context() != ctx {
				t.Fatal("reread changed the fixed local GET or caller context")
			}
			if got, ok := r.Context().Deadline(); !ok || got != deadline {
				t.Fatal("reread changed the caller deadline")
			}
			info := cableFreshnessInfo(t, time.Now())
			if calls == 1 {
				info.Connections.ObservedAt -= 1999
				time.Sleep(time.Millisecond) // Virtual transit crosses the actual 2000ms cutoff.
				if _, err := projectCablePorts(info, "freshness-host", "freshness-principal", time.Now()); !errors.Is(err, errCableFactsStale) {
					t.Fatalf("cache boundary was not classified as typed stale: %v", err)
				}
			}
			return cableFreshnessResponse(t, info), nil
		})
		result, err := facts.snapshot(ctx, mesh)
		if err != nil || calls != 2 || result.AgeMs != 0 || len(result.Ports) != 1 {
			t.Fatalf("fresh reread: calls=%d result=%+v error=%v", calls, result, err)
		}
	})
}

func TestCableLocalFactsPersistentStaleStopsAtTwo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		facts, mesh, _ := cableFreshnessFixture(t, func(*http.Request) (*http.Response, error) {
			calls++
			return cableFreshnessResponse(t, cableFreshnessInfo(t, time.Now().Add(-2*time.Second))), nil
		})
		result, err := facts.snapshot(context.Background(), mesh)
		if !errors.Is(err, errCableFactsStale) || calls != 2 || result.Ports != nil {
			t.Fatalf("persistent stale: calls=%d result=%+v error=%v", calls, result, err)
		}
	})
}

func TestCableLocalFactsNeverRetriesOtherFailures(t *testing.T) {
	for _, name := range []string{"fresh", "wrong-host", "malformed-json", "truncated", "unavailable", "future", "bad-mac", "missing-groups", "http-503", "transport", "certificate-rotation"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				var dir string
				facts, mesh, fixtureDir := cableFreshnessFixture(t, func(*http.Request) (*http.Response, error) {
					calls++
					info := cableFreshnessInfo(t, time.Now().Add(-2*time.Second))
					switch name {
					case "fresh":
						info.Connections.ObservedAt = time.Now().UnixMilli()
					case "wrong-host":
						info.HostUUID = "different-host"
					case "malformed-json":
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString("{"))}, nil
					case "truncated":
						info.Connections.Truncated = true
					case "unavailable":
						info.Connections.Status = "unavailable"
					case "future":
						info.Connections.ObservedAt = time.Now().Add(time.Millisecond).UnixMilli()
					case "bad-mac":
						info.Connections.Interfaces[0].MAC = "not-a-mac"
					case "missing-groups":
						info.Connections.Interfaces[0].PhysicalPort = nil
					case "http-503":
						response := cableFreshnessResponse(t, info)
						response.StatusCode = http.StatusServiceUnavailable
						return response, nil
					case "transport":
						return nil, errors.New("synthetic transport unavailable")
					case "certificate-rotation":
						clustertrusttest.WriteKeypair(t, dir, "freshness-principal")
					}
					return cableFreshnessResponse(t, info), nil
				})
				dir = fixtureDir
				_, err := facts.snapshot(context.Background(), mesh)
				if calls != 1 || errors.Is(err, errCableFactsStale) || (err == nil) != (name == "fresh") {
					t.Fatalf("non-retryable %s: calls=%d error=%v", name, calls, err)
				}
			})
		})
	}
}

func TestCableLocalFactsRetryDoesNotExtendContext(t *testing.T) {
	for _, duringSecond := range []bool{false, true} {
		t.Run(map[bool]string{false: "expires-before-reread", true: "expires-during-reread"}[duringSecond], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
				defer cancel()
				began := time.Now()
				calls := 0
				facts, mesh, _ := cableFreshnessFixture(t, func(r *http.Request) (*http.Response, error) {
					calls++
					if r.Context() != ctx {
						t.Fatal("reread replaced the caller context")
					}
					if duringSecond && calls == 2 {
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					info := cableFreshnessInfo(t, time.Now().Add(-1999*time.Millisecond))
					if duringSecond {
						time.Sleep(time.Millisecond)
					} else {
						time.Sleep(2 * time.Millisecond)
					}
					return cableFreshnessResponse(t, info), nil
				})
				_, err := facts.snapshot(ctx, mesh)
				wantCalls := 1
				if duringSecond {
					wantCalls = 2
				}
				if err == nil || ctx.Err() != context.DeadlineExceeded || calls != wantCalls || time.Since(began) != 2*time.Millisecond {
					t.Fatalf("deadline: calls=%d elapsed=%s error=%v context=%v", calls, time.Since(began), err, ctx.Err())
				}
			})
		})
	}
}
