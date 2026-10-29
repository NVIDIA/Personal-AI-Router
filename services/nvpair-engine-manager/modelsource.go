// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net"
	"net/http"
	"time"
)

func modelSource(rt Runtime) string {
	if rt.ModelSource == "" {
		return "huggingface"
	}
	return rt.ModelSource
}

// Only an actual engine start creates a listener; settings previews stay read-only.
func (e *Executor) modelScopeEndpoint() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.shuttingDown.Load() {
		return "", fmt.Errorf("engine manager is shutting down")
	}
	if e.modelScope == nil {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", fmt.Errorf("start ModelScope adapter: %w", err)
		}
		e.modelScope = &http.Server{
			Handler:           newModelScopeHandler(e.client, modelScopeURL),
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       time.Minute,
		}
		e.modelScopeURL = "http://" + listener.Addr().String()
		go func() { _ = e.modelScope.Serve(listener) }()
	}
	return e.modelScopeURL, nil
}
