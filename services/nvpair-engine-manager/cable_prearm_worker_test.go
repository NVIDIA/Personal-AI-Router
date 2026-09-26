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
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"nvpair-shared/cableprobe"
)

func prearmTestInput(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	encoded, err := json.Marshal(onceTestRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(encoded, &request); err != nil {
		t.Fatal(err)
	}
	request["protocol"] = cableWorkerProtocol
	if mutate != nil {
		mutate(request)
	}
	encoded, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded) + "\n"
}

func prearmTestManager(t *testing.T) *Manager {
	t.Helper()
	m, _, _ := cableTestManager(t, func(*http.Request) (*http.Response, error) {
		return cableTestResponse(t, cableTestInfo(t, time.Now(), 1)), nil
	})
	cableTestRemoteRead(t, m, func(*http.Request) (*http.Response, error) {
		return cableTestResponse(t, cableTestPeerSnapshot(t)), nil
	})
	return m
}

func assertPrearmTestReceipt(t *testing.T, output []byte, code, outcome string) {
	t.Helper()
	var got map[string]any
	if bytes.Count(output, []byte{'\n'}) != 1 || json.Unmarshal(output, &got) != nil {
		t.Fatalf("expected one bounded pre-arm receipt, got %q", output)
	}
	want := map[string]any{
		"state": "prearm-failed", "runId": "synthetic-run", "reviewId": "synthetic-review",
		"code": code, "resourceOutcome": outcome, "cleanupConfirmed": false, "sent": float64(0), "received": float64(0),
	}
	if code == "prepare-facts-mismatch" {
		want["factsDifference"] = map[string]any{"targetIndex": float64(0), "field": "interface-mac"}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bounded pre-arm receipt = %#v, want %#v", got, want)
	}
}

func TestCablePrearmWorkerReportsPreparationFailure(t *testing.T) {
	for _, tc := range []struct {
		name, code, outcome string
		open, configure     int
		close               int
		busy                bool
	}{
		{"open", "prepare-open-failed", "rollback-confirmed", 1, 0, -1, false},
		{"configure", "prepare-configure-failed", "rollback-confirmed", 0, 1, -1, false},
		{"configure-and-close", "prepare-configure-failed", "rollback-unconfirmed", 0, 1, 10, false},
		{"open-and-close", "prepare-open-failed", "rollback-unconfirmed", 1, 0, 100, false},
		{"reservation", "prepare-reservation-failed", "unknown", 0, 0, -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, fake := prearmTestManager(t), newProbeTestIO()
			fake.failOpen, fake.failConfigure, fake.failClose, fake.busy = tc.open, tc.configure, tc.close, tc.busy
			probeIO := fake.io()
			open := probeIO.open
			probeIO.open = func() (int, error) {
				fd, err := open()
				if err != nil {
					return fd, errors.New("synthetic-private-open-detail")
				}
				return fd, nil
			}
			var output bytes.Buffer
			err := runCableProbeWorker(context.Background(), strings.NewReader(prearmTestInput(t, nil)), &output, func(ctx context.Context, request cableProbeOnceRequest) (*cableProbeSession, error) {
				return m.prepareCableProbeOnce(ctx, request, probeIO)
			})
			if err != nil {
				t.Fatalf("preparation failure was not delivered as a bounded result: %v", err)
			}
			assertPrearmTestReceipt(t, output.Bytes(), tc.code, tc.outcome)
			if fake.readCalls != 0 || fake.writeCalls != 0 || bytes.Contains(output.Bytes(), []byte("synthetic-private")) {
				t.Fatal("pre-arm failure transmitted, received, or leaked private error detail")
			}
		})
	}
}

func TestCablePrearmWorkerRejectsUnboundRequests(t *testing.T) {
	for _, name := range []string{"malformed", "unknown-field", "missing-protocol", "old-protocol", "null-protocol", "missing-run", "control-run", "missing-review", "owner", "peer-binding", "untyped-error", "unbound-typed-error"} {
		t.Run(name, func(t *testing.T) {
			m, fake := prearmTestManager(t), newProbeTestIO()
			input := prearmTestInput(t, func(request map[string]any) {
				switch name {
				case "unknown-field":
					request["private-extra"] = "synthetic-private-detail"
				case "missing-protocol":
					delete(request, "protocol")
				case "old-protocol":
					request["protocol"] = "pair-cable-worker/1"
				case "null-protocol":
					request["protocol"] = nil
				case "missing-run":
					delete(request, "runId")
				case "control-run":
					request["runId"] = "synthetic-run\x00"
				case "missing-review":
					delete(request["review"].(map[string]any), "reviewId")
				case "owner":
					request["review"].(map[string]any)["ownerNodeId"] = "other-owner"
				case "peer-binding":
					request["peers"].([]any)[0].(map[string]any)["principal"] = "other-principal"
				}
			})
			if name == "malformed" {
				input = "{\"runId\":\"synthetic-private-detail\"\n"
			}
			var output bytes.Buffer
			prepareCalls := 0
			err := runCableProbeWorker(context.Background(), strings.NewReader(input), &output, func(ctx context.Context, request cableProbeOnceRequest) (*cableProbeSession, error) {
				prepareCalls++
				if name == "untyped-error" {
					return nil, errors.New("synthetic-private-detail")
				}
				if name == "unbound-typed-error" {
					return prepareCableProbe(ctx, true, "local", probeTestMarker, probeTestTargets(1, 1), fake.io(), func(context.Context) error {
						return errors.New("synthetic-private-detail")
					})
				}
				return m.prepareCableProbeOnce(ctx, request, fake.io())
			})
			if err == nil || output.Len() != 0 {
				t.Fatalf("unbound request produced evidence: error=%v output=%q", err, output.Bytes())
			}
			calls, _, _ := fake.counts()
			if calls != 0 {
				t.Fatal("unbound request reached probe resource I/O")
			}
			if name != "owner" && name != "peer-binding" && name != "untyped-error" && name != "unbound-typed-error" && prepareCalls != 0 {
				t.Fatal("invalid protocol/identity/decode reached preparation")
			}
		})
	}
}

func TestCablePrearmWorkerReportsBoundFactRejection(t *testing.T) {
	for _, tc := range []struct{ name, code string }{
		{"unpaired", "prepare-pairing-unavailable"},
		{"facts", "prepare-current-invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, fake := prearmTestManager(t), newProbeTestIO()
			input := prearmTestInput(t, func(request map[string]any) {
				targets := request["review"].(map[string]any)["targets"].([]any)
				if tc.name == "unpaired" {
					targets[1].(map[string]any)["principal"] = "no-longer-paired"
					request["peers"].([]any)[0].(map[string]any)["principal"] = "no-longer-paired"
				} else {
					port := targets[0].(map[string]any)["ports"].([]any)[0].(map[string]any)
					port["interfaces"].([]any)[0].(map[string]any)["mac"] = "02:00:00:00:00:fe"
				}
			})
			var output bytes.Buffer
			if err := runCableProbeWorker(context.Background(), strings.NewReader(input), &output, func(ctx context.Context, request cableProbeOnceRequest) (*cableProbeSession, error) {
				ops := fake.io()
				ops.validate = func(alias cableprobe.Interface, _ cableprobe.PortRef) error {
					if alias.MAC == "02:00:00:00:00:fe" {
						return errors.New("native alias changed")
					}
					return nil
				}
				return m.prepareCableProbeOnce(ctx, request, ops)
			}); err != nil {
				t.Fatal(err)
			}
			assertPrearmTestReceipt(t, output.Bytes(), tc.code, "not-attempted")
			if calls, _, _ := fake.counts(); calls != 0 {
				t.Fatal("rejected paired/native facts reached probe resources")
			}
		})
	}
}

func TestCablePrearmPreparationKeepsFirstCause(t *testing.T) {
	for _, stage := range []string{"open", "configure", "current-before", "current-after", "scope", "cancelled", "deadline"} {
		t.Run(stage, func(t *testing.T) {
			fake := newProbeTestIO()
			probeIO := fake.io()
			first := errors.New("synthetic-private-primary")
			code, outcome := "prepare-"+stage+"-failed", "rollback-unconfirmed"
			ctx := context.Background()
			targets := probeTestTargets(1, 1)
			currentCalls := 0
			current := func(context.Context) error {
				currentCalls++
				if (stage == "current-before" && currentCalls == 1) || (stage == "current-after" && currentCalls == 2) {
					return first
				}
				return nil
			}
			switch stage {
			case "open":
				probeIO.open = func() (int, error) { return -1, first }
			case "configure":
				probeIO.configure = func(int, cableprobe.Interface, cableprobe.PortRef) error { return first }
			case "current-before":
				code, outcome = "prepare-current-invalid", "not-attempted"
			case "current-after":
				code = "prepare-current-invalid"
			case "scope":
				targets = nil
				code, outcome = "prepare-scope-invalid", "not-attempted"
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				first, code, outcome = context.Canceled, "prepare-cancelled", "not-attempted"
			case "deadline":
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
				first, code, outcome = context.DeadlineExceeded, "prepare-deadline-exceeded", "not-attempted"
			}
			closeDescriptor := probeIO.close
			probeIO.close = func(fd int) error {
				_ = closeDescriptor(fd)
				return context.Canceled // Cleanup cancellation must not replace first.
			}
			session, err := prepareCableProbe(ctx, true, "local", probeTestMarker, targets, probeIO, current)
			var failure *cablePreparationError
			if session != nil || !errors.As(err, &failure) || failure.Code != code || failure.ResourceOutcome != outcome || failure.requestBound {
				t.Fatalf("wrong separate preparation evidence: session=%v error=%v typed=%+v", session, err, failure)
			}
			if stage != "scope" && !errors.Is(err, first) {
				t.Fatal("typed error lost the original primary cause")
			}
			if strings.Contains(err.Error(), "synthetic-private") || fake.readCalls != 0 || fake.writeCalls != 0 {
				t.Fatal("preparation error leaked private detail or used frame I/O")
			}
			if outcome == "rollback-unconfirmed" && (len(fake.closed) == 0 || fake.closed[len(fake.closed)-1] != 100) {
				t.Fatal("rollback did not release descriptors before the reservation")
			}
		})
	}
}

func TestCablePrearmWorkerUnknownBoundCauseAndOutputLimits(t *testing.T) {
	for _, mode := range []string{"unknown", "cancelled-output", "blocked-output"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := prearmTestManager(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var output bytes.Buffer
				var writer io.Writer = &output
				if mode == "blocked-output" {
					reader, pipe := io.Pipe()
					defer reader.Close()
					defer pipe.Close()
					writer = pipe
				}
				began := time.Now()
				err := runCableProbeWorker(ctx, strings.NewReader(prearmTestInput(t, nil)), writer, func(_ context.Context, request cableProbeOnceRequest) (*cableProbeSession, error) {
					if _, err := bindCableProbeRequest(m, request); err != nil {
						return nil, err
					}
					if mode == "cancelled-output" {
						cancel()
					}
					return nil, boundCablePreparationError(errors.New("synthetic-private-unknown-cause"))
				})
				if mode == "unknown" {
					if err != nil {
						t.Fatal(err)
					}
					assertPrearmTestReceipt(t, output.Bytes(), "prepare-unknown", "unknown")
				} else if err == nil || output.Len() != 0 || time.Since(began) > 2*time.Second {
					t.Fatalf("reporting detached or exceeded its bound: error=%v elapsed=%v output=%q", err, time.Since(began), output.Bytes())
				}
			})
		})
	}
}
