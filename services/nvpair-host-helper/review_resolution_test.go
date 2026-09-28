// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"nvpair-shared/hostbootstrap"
)

func TestHelperResponseMustCorrelateRequest(t *testing.T) {
	request := validBootstrapHelperRequest(hostbootstrap.HelperActionVerify)
	valid := hostbootstrap.HelperResponse{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   request.OperationID,
		Action:        request.Action,
		Accepted:      true,
	}
	if err := validateHelperResponseCorrelation(request, valid); err != nil {
		t.Fatalf("valid correlation error = %v", err)
	}
	wrongOperation := valid
	wrongOperation.OperationID = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	if err := validateHelperResponseCorrelation(request, wrongOperation); !errors.Is(err, hostbootstrap.ErrHelperProtocol) {
		t.Fatalf("wrong operation error = %v", err)
	}
	wrongAction := valid
	wrongAction.Action = hostbootstrap.HelperActionApply
	if err := validateHelperResponseCorrelation(request, wrongAction); !errors.Is(err, hostbootstrap.ErrHelperProtocol) {
		t.Fatalf("wrong action error = %v", err)
	}
}

func TestServiceLifecycleReportsRunningAndStopsServer(t *testing.T) {
	requests := make(chan serviceControl, 1)
	statuses := make(chan serviceStatus, 4)
	served := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runServiceLifecycle(
			context.Background(),
			requests,
			statuses,
			func(ctx context.Context) error {
				close(served)
				<-ctx.Done()
				return nil
			},
		)
	}()
	<-served
	var got []serviceStatus
	got = append(got, <-statuses, <-statuses)
	requests <- serviceControlStop
	got = append(got, <-statuses)
	if err := <-done; err != nil {
		t.Fatalf("runServiceLifecycle() error = %v", err)
	}
	want := []serviceStatus{serviceStatusStartPending, serviceStatusRunning, serviceStatusStopPending}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("statuses = %#v, want %#v", got, want)
	}
}

type deadlineRecordingConn struct {
	net.Conn
	mu             sync.Mutex
	readDeadlines  []time.Time
	writeDeadlines []time.Time
}

func (connection *deadlineRecordingConn) SetReadDeadline(deadline time.Time) error {
	connection.mu.Lock()
	connection.readDeadlines = append(connection.readDeadlines, deadline)
	connection.mu.Unlock()
	return connection.Conn.SetReadDeadline(deadline)
}

func (connection *deadlineRecordingConn) SetWriteDeadline(deadline time.Time) error {
	connection.mu.Lock()
	connection.writeDeadlines = append(connection.writeDeadlines, deadline)
	connection.mu.Unlock()
	return connection.Conn.SetWriteDeadline(deadline)
}

type timingExecutor struct {
	started chan struct{}
	release chan struct{}
}

func (timingExecutor) Inspect(
	_ context.Context,
	request hostbootstrap.Request,
) (hostbootstrap.Status, error) {
	return hostbootstrap.Status{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   request.OperationID,
		Phase:         hostbootstrap.PhaseInspect,
		Binding:       request.Binding,
		Observed:      exactHelperObservations(request),
	}, nil
}

func (executor timingExecutor) Apply(
	ctx context.Context,
	request hostbootstrap.Request,
	plan hostbootstrap.Plan,
) (hostbootstrap.Status, error) {
	close(executor.started)
	select {
	case <-executor.release:
		phase := hostbootstrap.PhaseApply
		observed := plan.Observed
		if plan.Decision == hostbootstrap.DecisionNoOp {
			phase = hostbootstrap.PhaseVerify
			observed = exactHelperObservations(request)
		}
		return hostbootstrap.Status{
			SchemaVersion: hostbootstrap.SchemaVersion,
			OperationID:   request.OperationID,
			Phase:         phase,
			Decision:      plan.Decision,
			Binding:       request.Binding,
			Observed:      observed,
		}, nil
	case <-ctx.Done():
		return hostbootstrap.Status{}, ctx.Err()
	}
}

func (timingExecutor) Verify(
	_ context.Context,
	request hostbootstrap.Request,
	plan hostbootstrap.Plan,
) (hostbootstrap.Receipt, error) {
	return hostbootstrap.NewReceipt(plan, exactHelperObservations(request))
}

func TestHelperDeadlinesSeparateReadExecuteAndWrite(t *testing.T) {
	request, plan := helperTestOperation(t)
	server, client := net.Pipe()
	recording := &deadlineRecordingConn{Conn: server}
	executor := timingExecutor{started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- handleConnection(
			context.Background(),
			recording,
			&fakeAuthorizer{},
			&fakeHelperState{
				principal: reviewedPrincipal{UID: 1000, GID: 1000},
				request:   request,
				plan:      plan,
			},
			executor,
		)
	}()
	raw, err := hostbootstrap.EncodeHelperRequest(
		validBootstrapHelperRequest(hostbootstrap.HelperActionApply),
	)
	if err != nil {
		t.Fatal(err)
	}
	writeDone := make(chan error, 1)
	go func() { writeDone <- hostbootstrap.WriteHelperFrame(client, raw) }()
	<-executor.started
	recording.mu.Lock()
	readDeadlines := append([]time.Time(nil), recording.readDeadlines...)
	writeDeadlines := append([]time.Time(nil), recording.writeDeadlines...)
	recording.mu.Unlock()
	if len(readDeadlines) < 2 || !readDeadlines[len(readDeadlines)-1].IsZero() {
		t.Fatalf("read deadlines = %#v", readDeadlines)
	}
	if len(writeDeadlines) != 0 {
		t.Fatalf("write deadline started during execution: %#v", writeDeadlines)
	}
	close(executor.release)
	if _, err := hostbootstrap.ReadHelperFrame(client); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	recording.mu.Lock()
	defer recording.mu.Unlock()
	if len(recording.writeDeadlines) != 1 ||
		recording.writeDeadlines[0].IsZero() {
		t.Fatalf("write deadlines = %#v", recording.writeDeadlines)
	}
}

func TestConcurrentMutatingRequestsSerializeProcessAndOperationLock(t *testing.T) {
	var guard sync.Mutex
	lockHeld := false
	active := 0
	maxActive := 0
	coordinator := mutationCoordinator{
		acquire: func() (func() error, error) {
			guard.Lock()
			defer guard.Unlock()
			if lockHeld {
				return nil, hostbootstrap.ErrHelperProtocol
			}
			lockHeld = true
			return func() error {
				guard.Lock()
				lockHeld = false
				guard.Unlock()
				return nil
			}, nil
		},
	}
	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- coordinator.Run(func() error {
			guard.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			guard.Unlock()
			close(started)
			<-release
			guard.Lock()
			active--
			guard.Unlock()
			return nil
		})
	}()
	<-started
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- coordinator.Run(func() error {
			guard.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			active--
			guard.Unlock()
			return nil
		})
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("second mutation completed early: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if maxActive != 1 || lockHeld {
		t.Fatalf("maxActive=%d lockHeld=%t", maxActive, lockHeld)
	}
	externalBusy := mutationCoordinator{
		acquire: func() (func() error, error) {
			return nil, hostbootstrap.ErrHelperProtocol
		},
	}
	mutated := false
	if err := externalBusy.Run(func() error {
		mutated = true
		return nil
	}); !errors.Is(err, hostbootstrap.ErrHelperProtocol) {
		t.Fatalf("external lock error = %v", err)
	}
	if mutated {
		t.Fatal("external operation lock failure allowed mutation")
	}
}
