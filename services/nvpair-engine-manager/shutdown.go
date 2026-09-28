// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"time"
)

// Explicit fabric rollback has separate 45-second admission and cleanup phases.
// Owners drain concurrently within one epoch; prepare and EOF never renew it.
// Broker, desktop and TUI parents reserve room beyond this complete window.
const managerShutdownBudget = 100 * time.Second

func (m *Manager) shutdown() error {
	m.shutdownOnce.Do(func() {
		m.shutdownDone = make(chan struct{})
		ctx, cancel := context.WithTimeout(context.Background(), managerShutdownBudget)
		m.exec.shuttingDown.Store(true)
		go func() {
			defer close(m.shutdownDone)
			defer cancel()
			joined := make(chan error, 1)
			go func() {
				// Each closure shares its owner's publication mutex. Operations that
				// won admission first are captured by that owner's subsequent join.
				m.exec.diagnostics.closePackageAdmission()
				m.exec.fabric.closeAdmission()
				m.cables.mu.Lock()
				m.cables.shuttingDown = true
				m.cables.mu.Unlock()
				results := make(chan error, 4)
				go func() { m.cables.shutdown(); results <- nil }()
				go func() { results <- m.exec.fabric.shutdown(ctx) }()
				go func() { m.exec.diagnostics.shutdown(); results <- nil }()
				go func() { results <- m.exec.stopAll() }()
				var err error
				for range 4 {
					err = errors.Join(err, <-results)
				}
				joined <- err
			}()
			select {
			case m.shutdownErr = <-joined:
			case <-ctx.Done():
				m.shutdownErr = errors.Join(errors.New("manager shutdown is incomplete; owned work remains unconfirmed"), ctx.Err())
			}
		}()
	})
	<-m.shutdownDone
	return m.shutdownErr
}
