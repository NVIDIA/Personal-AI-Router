// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestCableWorkerInternalClosureRetainsCause(t *testing.T) {
	for _, test := range []struct{ name, want string }{
		{"pre-arm-eof", "cable worker output is incomplete or exceeds its bound"},
		{"malformed-output", "cable worker returned an invalid bounded protocol message"},
		{"stderr-bound", "cable worker diagnostic output exceeds its bound"},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, _, _ := launchTestPlan(t)
			request := launchTestRequest(plan)
			reader, writer := io.Pipe()
			defer writer.Close()
			stderr, stderrWriter := io.Pipe()
			defer stderr.Close()
			defer stderrWriter.Close()
			session := &launchTestSession{output: reader, stderr: stderr, done: make(chan struct{})}
			client := &onboardingSSH{testCableSession: func() (cableSSHSession, error) { return session, nil }}
			worker, err := openCableWorker(context.Background(), client, plan, request, "synthetic-elevation-input")
			if err != nil {
				t.Fatal(err)
			}
			defer worker.close()
			switch test.name {
			case "pre-arm-eof":
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
			case "malformed-output":
				if _, err := io.WriteString(writer, "{}\n"); err != nil {
					t.Fatal(err)
				}
			case "stderr-bound":
				if _, err := io.WriteString(stderrWriter, strings.Repeat("x", (16<<10)+1)); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-session.done:
			case <-time.After(time.Second):
				t.Fatal("provider did not close its rejected fixture stream")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			// Both the closed stream and internal context are ready. Repeated reads
			// must retain the cause, not race it against cancellation or plain EOF.
			for i := 0; i < 2; i++ {
				message, err := worker.read(ctx)
				if err == nil || err.Error() != test.want || errors.Is(err, context.Canceled) || message.CleanupConfirmed {
					t.Fatalf("internal cause was masked on read %d: %v", i, err)
				}
				if failure := cableFailure("arm", "arm-invalid", err); failure.Phase != "arm" || failure.Code != "worker-output-invalid" {
					t.Fatalf("provider cause did not reach the fixed public arm code: %+v", failure)
				}
			}
			if err := worker.wait(ctx); err == nil || err.Error() != test.want {
				t.Fatalf("internal cause was masked during join: %v", err)
			}
		})
	}
}

func TestCableWorkerParentCancellationRemainsCancellation(t *testing.T) {
	plan, _, _ := launchTestPlan(t)
	request := launchTestRequest(plan)
	reader, writer := io.Pipe()
	defer writer.Close()
	session := &launchTestSession{output: reader, done: make(chan struct{})}
	client := &onboardingSSH{testCableSession: func() (cableSSHSession, error) { return session, nil }}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker, err := openCableWorker(parent, client, plan, request, "synthetic-elevation-input")
	if err != nil {
		t.Fatal(err)
	}
	defer worker.close()
	cancel()
	select {
	case <-session.done:
	case <-time.After(time.Second):
		t.Fatal("cancelled fixture did not close")
	}
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	for i := 0; i < 2; i++ {
		if message, err := worker.read(ctx); !errors.Is(err, context.Canceled) || message.CleanupConfirmed {
			t.Fatalf("caller cancellation was relabeled: %v", err)
		}
	}
	if err := worker.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("join relabeled caller cancellation: %v", err)
	}
}
