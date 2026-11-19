// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

func modelSource(rt Runtime) string {
	if rt.ModelSource == "" {
		return "huggingface"
	}
	return rt.ModelSource
}

func redactModelScopeEndpoint(text, endpoint string) string {
	if endpoint == "" {
		return text
	}
	text = strings.ReplaceAll(text, endpoint, "[redacted]")
	token := endpoint[strings.LastIndexByte(endpoint, '/')+1:]
	if token != "" {
		text = strings.ReplaceAll(text, token, "[redacted]")
	}
	return text
}

// Only an actual engine start creates a listener; settings previews stay read-only.
func (e *Executor) modelScopeEndpoint() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.shuttingDown.Load() {
		return "", fmt.Errorf("engine manager is shutting down")
	}
	if e.modelScope == nil {
		var token [32]byte
		if _, err := rand.Read(token[:]); err != nil {
			return "", fmt.Errorf("create ModelScope adapter token: %w", err)
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", fmt.Errorf("start ModelScope adapter: %w", err)
		}
		host := listener.Addr().String()
		prefix := "/" + hex.EncodeToString(token[:])
		handler := http.StripPrefix(prefix, newModelScopeHandler(e.client, modelScopeURL))
		e.modelScope = &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Reject DNS-rebound hosts and callers without the private endpoint
				// before any request can reach ModelScope.
				if r.Host != host || !strings.HasPrefix(r.URL.Path, prefix+"/") {
					http.NotFound(w, r)
					return
				}
				handler.ServeHTTP(w, r)
			}),
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       time.Minute,
		}
		e.modelScopeURL = "http://" + host + prefix
		go func() { _ = e.modelScope.Serve(listener) }()
	}
	return e.modelScopeURL, nil
}
