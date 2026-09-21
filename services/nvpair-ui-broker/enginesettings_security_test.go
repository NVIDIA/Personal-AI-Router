// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSettingsDerivesCORSGuardFromAuthenticatedCaller(t *testing.T) {
	h := newSettingsHarness(t)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, _ := url.Parse("urn:nvpair:node:paired-caller")
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), URIs: []*url.URL{uri}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(h.b.clusterDir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"node.crt":       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		"node.key":       pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		"admission.json": []byte(`{"clusterId":"test","epoch":1}`),
	} {
		if err := os.WriteFile(filepath.Join(h.b.clusterDir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, caller := range []string{"", "paired-caller"} {
		request := h.request(t)
		// Deliberately supply the opposite of what the authenticated caller needs.
		request.PreserveCORS = caller == ""
		if _, err := h.b.previewEngineSettings(context.Background(), request, caller); err != nil {
			t.Fatal(err)
		}
		if h.previewPreserveCORS.Load() != (caller != "") {
			t.Fatal("preview trusted client-supplied guard")
		}
		if _, err := h.b.applyEngineSettings(context.Background(), request, caller); err != nil {
			t.Fatal(err)
		}
		if h.previewPreserveCORS.Load() != (caller != "") {
			t.Fatal("apply trusted client-supplied guard")
		}
	}
}
