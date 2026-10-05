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

	"github.com/stretchr/testify/require"
)

func TestSettingsDerivesCORSGuardFromAuthenticatedCaller(t *testing.T) {
	h := newSettingsHarness(t)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	uri, _ := url.Parse("urn:nvpair:node:paired-caller")
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), URIs: []*url.URL{uri}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(h.b.clusterDir, 0700))
	for name, data := range map[string][]byte{
		"node.crt":       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		"node.key":       pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		"admission.json": []byte(`{"clusterId":"test","epoch":1}`),
	} {
		require.NoError(t, os.WriteFile(filepath.Join(h.b.clusterDir, name), data, 0600))
	}
	for _, caller := range []string{"", "paired-caller"} {
		request := h.request(t)
		// Deliberately supply the opposite of what the authenticated caller needs.
		request.PreserveCORS = caller == ""
		{
			_, err := h.b.previewEngineSettings(context.Background(), request, caller)
			require.NoError(t, err)
		}
		require.True(t, h.previewPreserveCORS.Load() == (caller != ""), "preview trusted client-supplied guard")
		{
			_, err := h.b.applyEngineSettings(context.Background(), request, caller)
			require.NoError(t, err)
		}
		require.True(t, h.previewPreserveCORS.Load() == (caller != ""), "apply trusted client-supplied guard")
	}
}
