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
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func cleanupTestRequest() cableCleanupRequest {
	return cableCleanupRequest{Protocol: cableCleanupProtocol, RunID: "fixture-run", ReviewID: "fixture-review", AttemptID: "fixture-attempt", Challenge: strings.Repeat("a", 32), NodeID: "fixture-node", Principal: "fixture-principal", UID: 1000,
		Scope: cableCleanupScope{PID: 200, StartTicks: "123", BootID: "11111111-2222-3333-4444-555555555555", PIDNS: "pid:[1]", MountNS: "mnt:[2]", NetNS: "net:[3]", UserNS: "user:[4]"}, WorkerSHA256: strings.Repeat("b", 64), OriginalWorkerSHA256: strings.Repeat("c", 64)}
}

type cleanupTestIO struct {
	ops                    cableCleanupIO
	rows                   map[int]cableCleanupProcess
	passes, closed, checks int
	held                   bool
	events                 []string
}

func newCleanupTestIO(t *testing.T) *cleanupTestIO {
	t.Helper()
	f := &cleanupTestIO{rows: map[int]cableCleanupProcess{
		100: {PID: 100, Parent: 99, UID: 0, StartTicks: "10", Executable: "/memfd:nvpair-cable-worker (deleted)", Args: []string{"nvpair-engine-manager", cableCleanupInspectorFlag}, Bytes: 100},
		99:  {PID: 99, Parent: 1, UID: 0, StartTicks: "9", Executable: "/usr/bin/sudo", Args: []string{"/usr/bin/sudo", "-S", "-p", "", "--", "/usr/bin/python3", "-I", "-c", cableCleanupLoaderScript()}, Bytes: 100},
		200: {PID: 200, Parent: 1, UID: 1000, StartTicks: "123", Executable: "/bundle/nvpair-engine-manager", Args: []string{"nvpair-engine-manager", "--stdio"}, Bytes: 100},
		300: {PID: 300, Gone: true, Bytes: 20},
		301: {PID: 301, Parent: 1, UID: 1000, StartTicks: "31", Executable: "/usr/bin/sleep", Args: []string{"sleep", "1"}, Bytes: 100},
	}}
	f.ops = cableCleanupIO{
		verify: func(ctx context.Context, r cableCleanupRequest) (cableCleanupScope, cableCleanupProcess, error) {
			f.events = append(f.events, "verify")
			return r.Scope, f.rows[100], ctx.Err()
		},
		processes: func(context.Context) ([]int, error) {
			f.events = append(f.events, "pass")
			f.passes++
			if !f.held {
				t.Fatal("process inspection ran without owning the reservation")
			}
			if f.passes == 1 {
				return []int{100, 99, 200, 300}, nil
			}
			return []int{100, 99, 200, 301}, nil
		},
		process: func(ctx context.Context, pid int) (cableCleanupProcess, error) {
			f.events = append(f.events, "process")
			if !f.held {
				t.Fatal("process classification ran after reservation release")
			}
			p, ok := f.rows[pid]
			if !ok {
				return cableCleanupProcess{}, cableCleanupFault("candidate-unreadable")
			}
			return p, ctx.Err()
		},
		lock: func(ctx context.Context) (cableCleanupLock, error) {
			f.events = append(f.events, "lock")
			f.held = true
			return cableCleanupLock{FD: 40, Device: 1, Inode: 2}, ctx.Err()
		},
		checkLock: func(ctx context.Context, lock cableCleanupLock) error {
			f.events = append(f.events, "check")
			f.checks++
			if !f.held || lock.FD != 40 {
				t.Fatal("reservation validation lost its owned descriptor")
			}
			return ctx.Err()
		},
		close: func(lock cableCleanupLock) error {
			f.events = append(f.events, "close")
			if !f.held || lock.FD != 40 {
				t.Fatal("reservation closed twice or foreign descriptor used")
			}
			f.held = false
			f.closed++
			return nil
		},
	}
	return f
}

func cleanupTestInput(t *testing.T, r cableCleanupRequest, command string) io.Reader {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if command == "EOF" {
		return strings.NewReader(string(data) + "\n")
	}
	cmd := cableCleanupCommand{AttemptID: r.AttemptID, Challenge: r.Challenge, Command: command}
	if command == "wrong-challenge" {
		cmd.Command = "release"
		cmd.Challenge = "foreign"
	}
	second, err := json.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReader(string(data) + "\n" + string(second) + "\n")
}

func cleanupTestMessages(t *testing.T, data []byte) []cableCleanupMessage {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	var result []cableCleanupMessage
	for {
		var message cableCleanupMessage
		err := decoder.Decode(&message)
		if err == io.EOF {
			return result
		}
		if err != nil {
			t.Fatal(err)
		}
		if !cableCleanupCodeValid(message.Code) {
			t.Fatal("unclassified error escaped")
		}
		result = append(result, message)
		if len(result) > 2 {
			t.Fatal("output message bound exceeded")
		}
	}
}

func TestCableCleanupInspectorUnrelatedChurnAndRelease(t *testing.T) {
	f := newCleanupTestIO(t)
	request := cleanupTestRequest()
	var output bytes.Buffer
	if err := runCableCleanupInspectorWithIO(context.Background(), cleanupTestInput(t, request, "release"), &output, f.ops); err != nil {
		t.Fatal(err)
	}
	messages := cleanupTestMessages(t, output.Bytes())
	if len(messages) != 2 || messages[0].State != "ready" || messages[0].CleanupConfirmed || messages[0].Passes != 2 || messages[0].Processes != 8 || messages[0].Candidates != 0 || messages[0].RemainingMs <= 0 || messages[0].RemainingMs > 5000 {
		t.Fatalf("wrong ready proof: %+v", messages)
	}
	if messages[1].State != "closed" || messages[1].Code != "released" || !messages[1].CleanupConfirmed || messages[1].Scope != request.Scope || f.closed != 1 || f.held {
		t.Fatalf("release was not joined and closed: %+v", messages)
	}
	if f.events[len(f.events)-1] != "close" {
		t.Fatal("inspection resumed after reservation release")
	}
}

func TestCableCleanupInspectorHoldsRelevantUncertainty(t *testing.T) {
	for _, name := range []string{"active", "ambiguous", "unreadable", "foreign-cleanup", "same-pid-new-generation", "ancestor-changed-command", "lock-busy", "lock-open-close-failure", "lock-replaced", "scope-mismatch", "filtered-owner", "process-limit", "metadata-limit", "unexpected-error"} {
		t.Run(name, func(t *testing.T) {
			f := newCleanupTestIO(t)
			want := "candidate-active"
			switch name {
			case "active":
				p := f.rows[301]
				p.Args = []string{"nvpair-engine-manager", "--cable-probe-once"}
				f.rows[301] = p
			case "ambiguous":
				p := f.rows[301]
				p.Executable = "/memfd:nvpair-cable-worker (deleted)"
				f.rows[301] = p
				want = "candidate-ambiguous"
			case "unreadable":
				read := f.ops.process
				f.ops.process = func(ctx context.Context, pid int) (cableCleanupProcess, error) {
					if pid == 301 {
						return cableCleanupProcess{}, cableCleanupFault("candidate-unreadable")
					}
					return read(ctx, pid)
				}
				want = "candidate-unreadable"
			case "foreign-cleanup":
				p := f.rows[301]
				p.Args = []string{"python3", "-I", "-c", cableCleanupLoaderScript()}
				f.rows[301] = p
				want = "candidate-ambiguous"
			case "same-pid-new-generation":
				read := f.ops.process
				f.ops.process = func(ctx context.Context, pid int) (cableCleanupProcess, error) {
					p, err := read(ctx, pid)
					if pid == 99 && f.passes > 0 {
						p.StartTicks = "999"
					}
					return p, err
				}
				want = "candidate-ambiguous"
			case "ancestor-changed-command":
				read := f.ops.process
				f.ops.process = func(ctx context.Context, pid int) (cableCleanupProcess, error) {
					p, err := read(ctx, pid)
					if pid == 99 && f.passes > 0 {
						p.Args = []string{"nvpair-engine-manager", "--cable-probe-once"}
					}
					return p, err
				}
			case "lock-busy":
				f.ops.lock = func(context.Context) (cableCleanupLock, error) {
					return cableCleanupLock{}, cableCleanupFault("lock-busy")
				}
				want = "lock-busy"
			case "lock-open-close-failure":
				f.ops.lock = func(context.Context) (cableCleanupLock, error) {
					return cableCleanupLock{}, cableCleanupFault("close-unconfirmed")
				}
				want = "close-unconfirmed"
			case "lock-replaced":
				check := f.ops.checkLock
				f.ops.checkLock = func(ctx context.Context, l cableCleanupLock) error {
					err := check(ctx, l)
					if f.checks == 2 {
						return cableCleanupFault("lock-changed")
					}
					return err
				}
				want = "lock-changed"
			case "scope-mismatch":
				f.ops.verify = func(context.Context, cableCleanupRequest) (cableCleanupScope, cableCleanupProcess, error) {
					return cableCleanupScope{}, cableCleanupProcess{}, cableCleanupFault("scope-mismatch")
				}
				want = "scope-mismatch"
			case "filtered-owner":
				f.ops.processes = func(context.Context) ([]int, error) { return []int{100, 99, 301}, nil }
				want = "scope-mismatch"
			case "process-limit":
				f.ops.processes = func(context.Context) ([]int, error) { return make([]int, cableCleanupProcessLimit+1), nil }
				want = "inspection-limit"
			case "metadata-limit":
				p := f.rows[301]
				p.Bytes = cableCleanupMetadataLimit
				f.rows[301] = p
				want = "inspection-limit"
			case "unexpected-error":
				f.ops.processes = func(context.Context) ([]int, error) { return nil, errors.New("synthetic-private-text") }
				want = "proc-unavailable"
			}
			var output bytes.Buffer
			if err := runCableCleanupInspectorWithIO(context.Background(), cleanupTestInput(t, cleanupTestRequest(), "release"), &output, f.ops); err != nil {
				t.Fatal(err)
			}
			messages := cleanupTestMessages(t, output.Bytes())
			if len(messages) != 1 || messages[0].State != "blocked" || messages[0].Code != want || messages[0].CleanupConfirmed != (name != "lock-open-close-failure") || f.held {
				t.Fatalf("uncertainty did not hold: %+v", messages)
			}
			if strings.Contains(output.String(), "synthetic-private-text") || strings.Contains(output.String(), cableCleanupLoaderScript()) {
				t.Fatal("raw process/error text escaped")
			}
		})
	}
}

func TestCableCleanupInspectorOwnerControlAndCloseFailure(t *testing.T) {
	for _, command := range []string{"cancel", "EOF", "wrong-challenge", "unknown", "close-failure"} {
		t.Run(command, func(t *testing.T) {
			f := newCleanupTestIO(t)
			want := map[string]string{"cancel": "cancelled", "EOF": "owner-disconnected", "wrong-challenge": "invalid-command", "unknown": "invalid-command", "close-failure": "close-unconfirmed"}[command]
			actual := command
			if command == "close-failure" {
				actual = "release"
				closeFD := f.ops.close
				f.ops.close = func(l cableCleanupLock) error { _ = closeFD(l); return errors.New("synthetic close failed") }
			}
			var output bytes.Buffer
			if err := runCableCleanupInspectorWithIO(context.Background(), cleanupTestInput(t, cleanupTestRequest(), actual), &output, f.ops); err != nil {
				t.Fatal(err)
			}
			messages := cleanupTestMessages(t, output.Bytes())
			if len(messages) != 2 || messages[1].State != "closed" || messages[1].Code != want || messages[1].CleanupConfirmed != (command != "close-failure") || f.closed != 1 {
				t.Fatalf("owner control/close truth changed: %+v", messages)
			}
		})
	}
}

func TestCableCleanupInspectorKeepsTimeBounds(t *testing.T) {
	for _, mode := range []string{"inspection", "caller", "cancelled", "command"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newCleanupTestIO(t)
				request := cleanupTestRequest()
				var output bytes.Buffer
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				began := time.Now()
				input := cleanupTestInput(t, request, "release")
				switch mode {
				case "inspection":
					f.ops.processes = func(ctx context.Context) ([]int, error) { <-ctx.Done(); return nil, ctx.Err() }
				case "caller":
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(ctx, 2*time.Millisecond)
					defer stop()
					f.ops.processes = func(ctx context.Context) ([]int, error) { <-ctx.Done(); return nil, ctx.Err() }
				case "cancelled":
					cancel()
				case "command":
					reader, writer := io.Pipe()
					defer writer.Close()
					input = reader
					data, _ := json.Marshal(request)
					go func() { _, _ = fmt.Fprintf(writer, "%s\n", data) }()
				}
				err := runCableCleanupInspectorWithIO(ctx, input, &output, f.ops)
				messages := cleanupTestMessages(t, output.Bytes())
				for _, message := range messages {
					if message.Code == "released" {
						t.Fatal("expired/cancelled attempt released the hold")
					}
				}
				if mode == "inspection" && (err != nil || len(messages) != 1 || messages[0].Code != "deadline-exceeded" || time.Since(began) != 5*time.Second) {
					t.Fatalf("inspection deadline changed: elapsed=%s err=%v messages=%+v", time.Since(began), err, messages)
				}
				if mode == "caller" && (err == nil || time.Since(began) != 2*time.Millisecond || f.closed != 1) {
					t.Fatal("caller deadline was extended or owned lock not closed")
				}
				if mode == "cancelled" && (err == nil || f.held || f.closed != 0) {
					t.Fatal("pre-cancelled attempt acquired resources")
				}
				if mode == "command" && (time.Since(began) != 5*time.Second || f.closed != 1 || len(messages) != 2 || messages[1].Code != "deadline-exceeded") {
					t.Fatalf("command lease changed: elapsed=%s err=%v messages=%+v", time.Since(began), err, messages)
				}
			})
		})
	}
}

func TestCableCleanupRequestAndScopeValidationBeforeIO(t *testing.T) {
	for _, mode := range []string{"protocol", "challenge", "scope", "digest", "uid", "trailing"} {
		t.Run(mode, func(t *testing.T) {
			r := cleanupTestRequest()
			switch mode {
			case "protocol":
				r.Protocol = "unknown"
			case "challenge":
				r.Challenge = "short"
			case "scope":
				r.Scope.NetNS = "untrusted"
			case "digest":
				r.WorkerSHA256 = "path"
			case "uid":
				r.UID = 0
			}
			data, _ := json.Marshal(r)
			if mode == "trailing" {
				data = append(data, []byte(" {}")...)
			}
			var output bytes.Buffer
			f := newCleanupTestIO(t)
			if err := runCableCleanupInspectorWithIO(context.Background(), strings.NewReader(string(data)+"\n"), &output, f.ops); err == nil || len(f.events) != 0 || output.Len() != 0 {
				t.Fatal("invalid request reached inspection or reflected untrusted fields")
			}
		})
	}
}
