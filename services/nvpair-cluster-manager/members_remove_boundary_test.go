// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type signaledPipeBody struct {
	*io.PipeReader
	started chan struct{}
	once    sync.Once
}

func (b *signaledPipeBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	return b.PipeReader.Read(p)
}

func removeRequest(t *testing.T, remover *Manager, proof RemovalProof, body io.ReadCloser) *http.Request {
	t.Helper()
	if body == nil {
		body = io.NopCloser(bytes.NewReader(mustJSON(t, membersRemoveRequest{
			NodeUUID: remover.identity.NodeUUID,
			Proof:    proof,
		})))
	}
	req := httptest.NewRequest(http.MethodPost, membersRemovePath, body)
	cert, err := x509.ParseCertificate(remover.identity.Cert.Certificate[0])
	require.NoError(t, err)
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	return req
}

func TestMembersRemoveRevalidatesAfterBlockedBody(t *testing.T) {
	victim := newTestManagerPort(t, 15121)
	remover := newTestManagerPort(t, 15122)
	pinTrusted(t, victim, remover.identity.NodeUUID, string(remover.identity.CertPEM), remover.identity.CertFingerprint)
	_, oldEpoch := victim.currentAdmission()
	proof, err := remover.newRemovalProof(victim.identity.NodeUUID, oldEpoch)
	require.NoError(t, err)
	payload := mustJSON(t, membersRemoveRequest{NodeUUID: remover.identity.NodeUUID, Proof: proof})

	pr, pw := io.Pipe()
	started := make(chan struct{})
	body := &signaledPipeBody{PipeReader: pr, started: started}
	req := removeRequest(t, remover, proof, body)
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		victim.handleMembersRemove(rr, req)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "test expectation failed", "remove handler did not begin body read")
	}

	// The old request is authenticated but stalled. Tear down, re-admit into the
	// same cluster with a newer epoch, and restore a valid new composition before
	// releasing the old body.
	victim.teardownClusterLocal()
	newEpoch := activateTestCluster(t, victim, "cluster-1")
	require.Greater(t, newEpoch, oldEpoch, "new admission")
	pinTrusted(t, victim, remover.identity.NodeUUID, string(remover.identity.CertPEM), remover.identity.CertFingerprint)
	victim.addSelfMember()
	{
		_, err := pw.Write(payload)
		require.NoError(t, err)
	}
	_ = pw.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "test expectation failed", "remove handler did not finish")
	}
	require.Equal(t, http.StatusConflict, rr.Code, "status")
	{
		cid, epoch := victim.currentAdmission()
		require.Equal(t, "cluster-1", cid, "stale request cleared new admission (%v, %v)", cid, epoch)
		require.Equal(t, newEpoch, epoch, "stale request cleared new admission (%v, %v)", cid, epoch)
	}
	{
		_, ok := victim.trust.Get(remover.identity.NodeUUID)
		require.True(t, ok, "stale request cleared the newly re-established remover pin")
	}
}

func TestMembersRemoveCurrentAdmissionSucceeds(t *testing.T) {
	victim := newTestManagerPort(t, 15123)
	remover := newTestManagerPort(t, 15124)
	pinTrusted(t, victim, remover.identity.NodeUUID, string(remover.identity.CertPEM), remover.identity.CertFingerprint)
	_, epoch := victim.currentAdmission()
	proof, err := remover.newRemovalProof(victim.identity.NodeUUID, epoch)
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	victim.handleMembersRemove(rr, removeRequest(t, remover, proof, nil))
	require.Equal(t, http.StatusOK, rr.Code, "status")
	assertFullyUnclustered(t, victim)
}
