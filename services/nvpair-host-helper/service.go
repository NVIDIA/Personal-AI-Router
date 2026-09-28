// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
)

type serviceControl int

const (
	serviceControlStop serviceControl = iota + 1
	serviceControlShutdown
)

type serviceStatus int

const (
	serviceStatusStartPending serviceStatus = iota + 1
	serviceStatusRunning
	serviceStatusStopPending
	serviceStatusStopped
)

func runServiceLifecycle(
	parent context.Context,
	requests <-chan serviceControl,
	statuses chan<- serviceStatus,
	serve func(context.Context) error,
) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	statuses <- serviceStatusStartPending
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serve(ctx)
	}()
	statuses <- serviceStatusRunning
	select {
	case err := <-serverDone:
		statuses <- serviceStatusStopped
		return err
	case control := <-requests:
		if control != serviceControlStop && control != serviceControlShutdown {
			cancel()
			<-serverDone
			statuses <- serviceStatusStopped
			return errors.New("unsupported service control")
		}
		statuses <- serviceStatusStopPending
		cancel()
		err := <-serverDone
		statuses <- serviceStatusStopped
		return err
	case <-parent.Done():
		statuses <- serviceStatusStopPending
		cancel()
		err := <-serverDone
		statuses <- serviceStatusStopped
		return err
	}
}
