// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/svc"
)

const windowsServiceName = "nvpair-host-helper"
const windowsHeadlessServiceName = "nvpair-headless"
const windowsHeadlessBrokerPath = `C:\Program Files\NVIDIA Corporation\PAIR\product\nvpair-ui-broker.exe`
const windowsHeadlessShutdownGrace = 18 * time.Second

type windowsServiceHandler struct {
	serve func(context.Context) error
}

func (handler windowsServiceHandler) Execute(
	_ []string,
	requests <-chan svc.ChangeRequest,
	statuses chan<- svc.Status,
) (bool, uint32) {
	controls := make(chan serviceControl, 1)
	lifecycleStatuses := make(chan serviceStatus, 4)
	done := make(chan error, 1)
	go func() {
		done <- runServiceLifecycle(
			context.Background(),
			controls,
			lifecycleStatuses,
			handler.serve,
		)
	}()
	for {
		select {
		case status := <-lifecycleStatuses:
			switch status {
			case serviceStatusStartPending:
				statuses <- svc.Status{State: svc.StartPending}
			case serviceStatusRunning:
				statuses <- svc.Status{
					State:   svc.Running,
					Accepts: svc.AcceptStop | svc.AcceptShutdown,
				}
			case serviceStatusStopPending:
				statuses <- svc.Status{State: svc.StopPending}
			case serviceStatusStopped:
				<-done
				statuses <- svc.Status{State: svc.Stopped}
				return false, 0
			}
		case request := <-requests:
			switch request.Cmd {
			case svc.Stop:
				controls <- serviceControlStop
			case svc.Shutdown:
				controls <- serviceControlShutdown
			case svc.Interrogate:
				statuses <- request.CurrentStatus
			}
		}
	}
}

func runPlatformService(context.Context) error {
	return svc.Run(windowsServiceName, windowsServiceHandler{
		serve: func(ctx context.Context) error {
			return runHelperServer(ctx, false)
		},
	})
}

func runPlatformHeadlessService(context.Context) error {
	return svc.Run(windowsHeadlessServiceName, windowsServiceHandler{
		serve: runWindowsHeadlessBroker,
	})
}

func runWindowsHeadlessBroker(ctx context.Context) error {
	command := windowsHeadlessBrokerCommand()
	stdin, err := command.StdinPipe()
	if err != nil {
		return err
	}
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		done <- command.Wait()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_, _ = io.WriteString(
			stdin,
			"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"shutdown\"}\n",
		)
		_ = stdin.Close()
		timer := time.NewTimer(windowsHeadlessShutdownGrace)
		defer timer.Stop()
		select {
		case <-done:
			return nil
		case <-timer.C:
			if command.Process != nil {
				_ = command.Process.Kill()
			}
			<-done
			return nil
		}
	}
}

func windowsHeadlessBrokerCommand() *exec.Cmd {
	command := exec.Command(windowsHeadlessBrokerPath)
	command.Dir = filepath.Dir(windowsHeadlessBrokerPath)
	return command
}
