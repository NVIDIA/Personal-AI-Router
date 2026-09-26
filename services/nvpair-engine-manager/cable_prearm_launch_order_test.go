// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// A full request can be consumed remotely before its local Write call returns.
// The callback between stdout lines proves the first terminal has already been
// parsed/enqueued before the later stream error ends launch admission.
type launchOrderPrearmInput struct {
	delivered chan struct{}
	release   chan struct{}
	once      sync.Once
}

// Unlike the original counterfixture, this input is released by session close
// and can report a local error/short write after remote request consumption.
type launchOrderHeldInput struct {
	delivered chan struct{}
	release   chan struct{}
	once      sync.Once
	writes    atomic.Int32
	writeErr  error
	short     bool
}

func (w *launchOrderHeldInput) Write(data []byte) (int, error) {
	if w.writes.Add(1) != 1 {
		return 0, errors.New("unexpected command after initial request")
	}
	close(w.delivered)
	<-w.release
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if w.short {
		return len(data) - 1, nil
	}
	return len(data), nil
}

func (w *launchOrderHeldInput) Close() error {
	w.once.Do(func() { close(w.release) })
	return nil
}

type launchOrderHeldOutput struct {
	delivered      <-chan struct{}
	closed         chan struct{}
	first          string
	parsed         chan struct{}
	afterParsed    func()
	withoutPrimary func()
	readCount      int
	once           sync.Once
}

func (r *launchOrderHeldOutput) Read(data []byte) (int, error) {
	<-r.delivered
	r.readCount++
	if r.first == "" {
		if r.withoutPrimary != nil {
			r.withoutPrimary()
		}
		<-r.closed
		return 0, io.EOF
	}
	if r.readCount == 1 {
		return copy(data, r.first), nil
	}
	if r.readCount == 2 {
		close(r.parsed) // scanner has enqueued the complete first message.
		if r.afterParsed != nil {
			r.afterParsed()
		}
	}
	return 0, io.EOF
}

func (r *launchOrderHeldOutput) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

type launchOrderHeldSession struct {
	*launchTestSession
	heldInput *launchOrderHeldInput
}

func (s *launchOrderHeldSession) StdinPipe() (io.WriteCloser, error) { return s.heldInput, nil }
func (s *launchOrderHeldSession) Close() error {
	_ = s.heldInput.Close()
	return s.launchTestSession.Close()
}

func TestCablePrearmInitialWriteFailureRetainsOnlyParsedPrimary(t *testing.T) {
	for _, mode := range []string{"write-error", "short-write", "parent-cancel", "write-timeout", "no-primary-write-error", "no-primary-parent-cancel", "no-primary-write-timeout"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				plan, _, _ := launchTestPlan(t)
				request := launchTestRequest(plan)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				input := &launchOrderHeldInput{delivered: make(chan struct{}), release: make(chan struct{})}
				output := &launchOrderHeldOutput{delivered: input.delivered, closed: make(chan struct{}), parsed: make(chan struct{}), first: prearmTestLine(t, request) + "\n"}
				noPrimary := strings.HasPrefix(mode, "no-primary-")
				if noPrimary {
					output.first = ""
				}
				switch mode {
				case "write-error", "no-primary-write-error":
					input.writeErr = errors.New("synthetic-private-local-write-error")
					if noPrimary {
						_ = input.Close()
					} else {
						output.afterParsed = func() { _ = input.Close() }
					}
				case "short-write":
					input.short = true
					output.afterParsed = func() { _ = input.Close() }
				case "parent-cancel", "no-primary-parent-cancel":
					if noPrimary {
						output.withoutPrimary = cancel
					} else {
						output.afterParsed = cancel
					}
				}
				session := &launchOrderHeldSession{launchTestSession: &launchTestSession{output: output, done: make(chan struct{})}, heldInput: input}
				defer session.Close()
				client := &onboardingSSH{testCableSession: func() (cableSSHSession, error) { return session, nil }}
				began := time.Now()
				worker, err := openCableWorker(ctx, client, plan, request, "synthetic-admin-input")
				if noPrimary {
					if err == nil || worker != nil {
						t.Fatalf("launch without primary was promoted: worker=%v error=%v", worker != nil, err)
					}
					if strings.Contains(err.Error(), "synthetic-private") {
						t.Fatal("raw local write error escaped")
					}
				} else {
					select {
					case <-output.parsed:
					default:
						t.Fatal("fixture did not establish prior parsing")
					}
					if err != nil || worker == nil {
						t.Fatalf("parsed primary lost during initial request failure: worker=%v error=%v", worker != nil, err)
					}
					defer worker.close()
					if worker.send(cableProbeCommand{RunID: request.RunID, Command: "start"}) == nil {
						t.Fatal("closed initial launch admitted start")
					}
					message, readErr := worker.read(ctx)
					if readErr != nil || !validCablePrearmWorkerMessage(message, request.RunID, request.Review.ReviewID) || message.Prearm.Code != "prepare-configure-failed" || message.CleanupConfirmed || message.Prearm.CleanupConfirmed {
						t.Fatalf("invalid retained primary: %+v %v", message, readErr)
					}
					if repeated, repeatedErr := worker.read(ctx); repeatedErr == nil || repeated.Prearm != nil {
						t.Fatal("prefetched preparation terminal was delivered more than once")
					}
					for _, command := range []string{"start", "cancel"} {
						if worker.send(cableProbeCommand{RunID: request.RunID, Command: command}) == nil {
							t.Fatal("terminal accepted command")
						}
					}
					if joinErr := worker.wait(context.Background()); joinErr == nil {
						t.Fatal("initial transport failure disappeared from join")
					}
					if mode == "parent-cancel" && !errors.Is(worker.wait(context.Background()), context.Canceled) {
						t.Fatal("parent cancellation cause was replaced")
					}
				}
				if input.writes.Load() != 1 || time.Since(began) > 2*time.Second {
					t.Fatalf("unexpected I/O/budget: writes=%d elapsed=%v", input.writes.Load(), time.Since(began))
				}
				select {
				case <-session.done:
				default:
					t.Fatal("failed initial launch left session open")
				}
			})
		})
	}
}
func (w *launchOrderPrearmInput) Write(b []byte) (int, error) {
	w.once.Do(func() { close(w.delivered) })
	<-w.release
	return len(b), nil
}
func (*launchOrderPrearmInput) Close() error { return nil }

type launchOrderPrearmReader struct {
	delivered <-chan struct{}
	parsed    chan struct{}
	first     string
	calls     int
	badSuffix bool
	release   chan struct{}
}

func (r *launchOrderPrearmReader) Read(b []byte) (int, error) {
	<-r.delivered
	r.calls++
	if r.calls == 1 {
		return copy(b, r.first), nil
	}
	if r.calls == 2 {
		close(r.parsed)
		if r.badSuffix {
			return copy(b, "{}\n"), nil
		}
		close(r.release)
	}
	return 0, io.EOF
}

type launchOrderPrearmSession struct {
	*launchTestSession
	inputWriter io.WriteCloser
}

func (s *launchOrderPrearmSession) StdinPipe() (io.WriteCloser, error) { return s.inputWriter, nil }

func TestCablePrearmLocalWriteCompletionOrder(t *testing.T) {
	for _, badSuffix := range []bool{false, true} {
		name := "complete-control"
		if badSuffix {
			name = "parsed-primary-then-invalid-suffix"
		}
		t.Run(name, func(t *testing.T) {
			plan, _, _ := launchTestPlan(t)
			request := launchTestRequest(plan)
			delivered, release, parsed := make(chan struct{}), make(chan struct{}), make(chan struct{})
			if badSuffix {
				defer close(release)
			}
			reader := &launchOrderPrearmReader{delivered: delivered, parsed: parsed, first: prearmTestLine(t, request) + "\n", badSuffix: badSuffix, release: release}
			session := &launchOrderPrearmSession{launchTestSession: &launchTestSession{output: reader, done: make(chan struct{})}, inputWriter: &launchOrderPrearmInput{delivered: delivered, release: release}}
			defer session.Close()
			client := &onboardingSSH{testCableSession: func() (cableSSHSession, error) { return session, nil }}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			worker, err := openCableWorker(ctx, client, plan, request, "synthetic-admin-input")
			select {
			case <-parsed:
			default:
				t.Fatal("fixture did not parse the primary before returning from launch")
			}
			if err != nil || worker == nil {
				t.Fatalf("parsed preparation primary became inaccessible before local Write returned: worker=%v err=%v", worker != nil, err)
			}
			defer worker.close()
			got, readErr := worker.read(context.Background())
			if readErr != nil || got.Prearm == nil || got.Prearm.Code != "prepare-configure-failed" || !strings.HasPrefix(got.Prearm.ResourceOutcome, "rollback-") {
				t.Fatalf("parsed preparation primary lost: %+v %v", got, readErr)
			}
			if got.CleanupConfirmed || got.Prearm.CleanupConfirmed || worker.send(cableProbeCommand{RunID: request.RunID, Command: "start"}) == nil {
				t.Fatal("retained preparation failure admitted start or confirmed cleanup")
			}
			if badSuffix {
				var outputErr cableWorkerOutputError
				if !errors.As(worker.wait(context.Background()), &outputErr) {
					t.Fatal("invalid suffix transport cause disappeared from join")
				}
			}
		})
	}
}
