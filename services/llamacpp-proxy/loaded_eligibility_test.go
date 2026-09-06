// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"nvpair-shared/noderec"
)

func TestSubscribedToNodeUsesLoadedNotCatalog(t *testing.T) {
	n := noderec.DirectoryNode{
		Name:     "box",
		HostUUID: "uuid-1",
		IP:       "127.0.0.1",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceLlamaCpp: {Port: 8084},
		},
		ModelsByEngine: map[string][]string{
			"llamacpp": {"ISTA-DASLab-Qwen3.8-27B-GSQ-RCO-GGUF_IQ3_XXS-mtp", "other"},
		},
		LoadedByEngine: map[string][]string{
			"llamacpp": {"ISTA-DASLab-Qwen3.8-27B-GSQ-RCO-GGUF_IQ3_XXS-mtp"},
		},
	}
	node, ok := subscribedToNode(n)
	if !ok {
		t.Fatal("expected lc node")
	}
	if !nodeAdvertisesModel(node, "ISTA-DASLab-Qwen3.8-27B-GSQ-RCO-GGUF_IQ3_XXS-mtp") {
		t.Fatal("loaded id must be eligible")
	}
	if nodeAdvertisesModel(node, "other") {
		t.Fatal("catalog-only id must not be eligible")
	}
}

func TestCatalogOnlyChatDoesNotHitBackend(t *testing.T) {
	hits := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions" {
			hits++
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	u, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split backend host: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("backend port: %v", err)
	}

	n := noderec.DirectoryNode{
		Name:     "box",
		HostUUID: "uuid-1",
		IP:       host,
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceLlamaCpp: {Port: port},
		},
		ModelsByEngine: map[string][]string{
			"llamacpp": {"other"},
		},
		LoadedByEngine: map[string][]string{
			"llamacpp": {},
		},
	}
	node, ok := subscribedToNode(n)
	if !ok {
		t.Fatal("expected lc node")
	}

	disc := NewDiscovery()
	disc.AddManual(node)
	p := testProxy(disc, 8084)

	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"model":"other","messages":[{"role":"user","content":"hi"}]}`)
	p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if hits != 0 {
		t.Fatalf("backend hits = %d, want 0 (catalog-only id must not be forwarded)", hits)
	}
}
