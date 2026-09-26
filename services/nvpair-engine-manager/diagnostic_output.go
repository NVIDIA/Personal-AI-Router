// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"sync"
)

// Shared accounting keeps stdout and stderr inside the original combined cap.
type diagnosticOutput struct {
	mu       sync.Mutex
	data     bytes.Buffer
	stderr   bytes.Buffer
	cancel   context.CancelFunc
	exceeded bool
}

func (w *diagnosticOutput) Write(data []byte) (int, error) {
	return w.write(data, false)
}

type diagnosticStderr struct{ output *diagnosticOutput }

func (w diagnosticStderr) Write(data []byte) (int, error) {
	return w.output.write(data, true)
}

func (w *diagnosticOutput) write(data []byte, stderr bool) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.data.Len()+w.stderr.Len()+len(data) > 1<<20 {
		w.exceeded = true
		w.cancel()
		return 0, errors.New("diagnostic output limit exceeded")
	}
	if stderr {
		return w.stderr.Write(data)
	}
	return w.data.Write(data)
}
