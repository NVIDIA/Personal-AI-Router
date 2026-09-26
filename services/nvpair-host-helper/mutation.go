// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sync"

	"nvpair-shared/hostbootstrap"
)

type mutationCoordinator struct {
	process sync.Mutex
	acquire func() (func() error, error)
}

func (coordinator *mutationCoordinator) Run(operation func() error) error {
	coordinator.process.Lock()
	defer coordinator.process.Unlock()
	if coordinator.acquire == nil {
		return hostbootstrap.ErrHelperProtocol
	}
	release, err := coordinator.acquire()
	if err != nil {
		return err
	}
	if release == nil {
		return hostbootstrap.ErrHelperProtocol
	}
	operationErr := operation()
	releaseErr := release()
	if operationErr != nil {
		return operationErr
	}
	return releaseErr
}
