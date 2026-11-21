// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cross-process gate for the receiving half of a peer model alias write.
//
// A node's model aliases belong to that node, so a write from another machine is
// a courier delivery: nvpair-workload-manager accepts it on the cluster mTLS
// listener, and this node's own settings service decides whether the document is
// acceptable. Two things can only be proven with the real binaries:
//
//   - The caller's identity is the cluster id on the client certificate. Nothing
//     in the body names a node, so a peer cannot write as somebody else, and a
//     host this node does not pin never reaches the validator at all.
//   - The document lands in the same store the local interface reads and writes.
//     A peer write that applied into a private copy would report success and
//     still leave the owner's editor showing the old list.
//
// The unit tests cover frame handling (nvpair-workload-manager/aliaseswrite_test.go)
// and both broker roles (nvpair-ui-broker/peermodelaliases_test.go) against stubs.
// They cannot observe a real certificate chain deciding who the caller is, a real
// settings process deciding whether to accept, or the answer the owner's interface
// will read back.
package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
	"nvpair-shared/jsonrpc"
	"nvpair-shared/noderec"
)

// peerAliasWriteURL is the workload manager's cluster-mTLS route that carries a
// peer's alias document. The port is fixed by contract, like the other
// inter-node surfaces.
const peerAliasWriteURL = "https://127.0.0.1:14320/v1/settings/model-aliases"

// aliasWriteReceipt is the frame's answer, spelled as wire shape rather than
// imported from a Go type so that renaming a field on one side fails here rather
// than silently tolerating a missing one.
type aliasWriteReceipt struct {
	RequestID string `json:"requestId"`
	Applied   bool   `json:"applied"`
	Error     string `json:"error"`
}

// storedModelAliases is what this node's settings service reports for its own
// document — the same reply the alias card on this machine renders.
type storedModelAliases struct {
	Aliases []struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	} `json:"aliases"`
}

// startClusteredBrokerWithSettings starts the real broker as a cluster member
// with the two workers this route needs: the manager that carries the frame and
// the settings service that owns the document.
func startClusteredBrokerWithSettings(t *testing.T, clusterDir string) (io.Writer, <-chan jsonrpc.Message) {
	t.Helper()
	stdin, msgs, stderrLines, cleanup := startBrokerWithDirs(t, t.TempDir(), clusterDir,
		"--workload-manager-path", workloadMgrBin,
		"--settings-path", nodeSettingsBin,
	)
	// The reader goroutine keeps the pipe drained; nothing here asserts on logs.
	_ = stderrLines
	t.Cleanup(cleanup)
	waitForMethod(t, msgs, "app:ready", 15*time.Second)
	waitTCP(t, "127.0.0.1:14320", 15*time.Second)
	return stdin, msgs
}

// readStoredAliases asks the broker for this node's own alias document, as the
// interface on this machine sees it.
func readStoredAliases(t *testing.T, stdin io.Writer, msgs <-chan jsonrpc.Message) storedModelAliases {
	t.Helper()
	var stored storedModelAliases
	resp := callRPC(t, stdin, msgs, "settings/get-cluster-model-aliases", nil, 10*time.Second)
	require.Nil(t, resp.Error, "settings/get-cluster-model-aliases was refused")
	require.NoError(t, json.Unmarshal(resp.Result, &stored), "alias document shape")
	return stored
}

// assertStoredAliasNames requires this node's document to be exactly the given
// names, in order.
func assertStoredAliasNames(t *testing.T, stdin io.Writer, msgs <-chan jsonrpc.Message, want ...string) {
	t.Helper()
	stored := readStoredAliases(t, stdin, msgs)
	// Length first, then names by index: comparing built slices would make a nil
	// and an empty one look different, which is not a fact about the document.
	require.Len(t, stored.Aliases, len(want), "this node's alias document is not what was expected")
	for i, name := range want {
		assert.Equal(t, name, stored.Aliases[i].Name, "alias %d", i)
	}
}

// aliasWriteMethod is the frame's method name as this file spells it. The route
// serves more than one operation on this port, so a frame must name the one it
// wants; the check below keeps this file's literal tied to the name the two
// workers share, so a rename fails here with a sentence about the frame rather
// than as an unexplained 400.
const aliasWriteMethod = "settings/replace-model-aliases"

// assertAliasWriteMethodNamesWhatTheWorkersAgreeOn.
func assertAliasWriteMethodNamesWhatTheWorkersAgreeOn(t *testing.T) {
	t.Helper()
	require.Equal(t, noderec.MethodAliasWrite, aliasWriteMethod,
		"this file's frames name a method the workload manager no longer accepts")
}

// postAliasWrite posts one envelope to the route and returns the status with the
// decoded receipt. The envelope is built here and the params are a literal at
// each call site, so a missing field — or one the route does not read — is
// visible in the test that sends it.
//
// Only the success answer is JSON: a refusal is http.Error's plain sentence,
// which is the text the operator across the cluster is meant to read, so the
// helper returns it as the receipt's reason instead of failing to parse it.
func postAliasWrite(t *testing.T, client *http.Client, params string) (int, aliasWriteReceipt) {
	t.Helper()
	assertAliasWriteMethodNamesWhatTheWorkersAgreeOn(t)
	frame := `{"jsonrpc":"2.0","method":"` + aliasWriteMethod + `","params":` + params + `}`

	resp, err := client.Post(peerAliasWriteURL, "application/json", strings.NewReader(frame))
	require.NoError(t, err, "post to the alias route")
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	require.NoError(t, readErr, "read alias receipt")
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '{' {
		var receipt aliasWriteReceipt
		require.NoError(t, json.Unmarshal(trimmed, &receipt), "alias receipt shape: %s", body)
		return resp.StatusCode, receipt
	}
	return resp.StatusCode, aliasWriteReceipt{Error: strings.TrimSpace(string(body))}
}

// TestPeerModelAliases_TrustedPeerWriteLandsInTheOwningNodesSettings: a pinned
// peer delivers a document, and this node accepts it into the store its own
// interface edits — not a copy, and not a pending state awaiting the owner.
func TestPeerModelAliases_TrustedPeerWriteLandsInTheOwningNodesSettings(t *testing.T) {
	if portBusy(14320) {
		t.Skip("the workload-manager inter-node port is already in use; skipping")
	}
	fx := newInterNodeCluster(t)
	stdin, msgs := startClusteredBrokerWithSettings(t, fx.nodeDir)

	t.Run("a document this node never configured reads back as nothing configured", func(t *testing.T) {
		// Control for every assertion below: without it a row left over from
		// another run could satisfy "the peer's write arrived".
		assertStoredAliasNames(t, stdin, msgs)
	})

	t.Run("a pinned peer's document is applied and reads back unchanged", func(t *testing.T) {
		status, receipt := postAliasWrite(t, fx.clientAsPeer(t),
			`{"requestId":"interop-apply-1","aliases":[{"name":"peer-writer","model":"qwen/qwen3.6-35b-a3b"}]}`)
		require.Equal(t, http.StatusOK, status, "a pinned peer's write was refused: %s", receipt.Error)
		assert.Equal(t, "interop-apply-1", receipt.RequestID, "the receipt must answer the request it carries")
		assert.True(t, receipt.Applied, "the route must confirm the owner applied it")
		assert.Empty(t, receipt.Error)

		stored := readStoredAliases(t, stdin, msgs)
		require.Len(t, stored.Aliases, 1, "the peer's document is not what this node serves")
		assert.Equal(t, "peer-writer", stored.Aliases[0].Name)
		assert.Equal(t, "qwen/qwen3.6-35b-a3b", stored.Aliases[0].Model,
			"the model name must reach the owner's store exactly as the writer typed it")
	})

	t.Run("an empty document erases rather than being read as no request", func(t *testing.T) {
		// The write replaces the whole document, so an empty list is a meaningful
		// edit. Treated as "nothing to do", the peer's Save would report success
		// while the rows stayed.
		status, receipt := postAliasWrite(t, fx.clientAsPeer(t), `{"requestId":"interop-erase-1","aliases":[]}`)
		require.Equal(t, http.StatusOK, status, "erasing from across the cluster was refused: %s", receipt.Error)
		assert.True(t, receipt.Applied)

		assertStoredAliasNames(t, stdin, msgs)
	})
}

// TestPeerModelAliases_WritesThatDoNotQualifyAreRefusedBeforeApplying: every
// refusal must leave the stored document alone, because "no" and "no, after a
// partial write" are the same message to whoever opens the card a minute later.
func TestPeerModelAliases_WritesThatDoNotQualifyAreRefusedBeforeApplying(t *testing.T) {
	if portBusy(14320) {
		t.Skip("the workload-manager inter-node port is already in use; skipping")
	}
	fx := newInterNodeCluster(t)
	stdin, msgs := startClusteredBrokerWithSettings(t, fx.nodeDir)

	// Seed a document to refuse over, so "unchanged" is a fact rather than the
	// absence of anything to change.
	status, receipt := postAliasWrite(t, fx.clientAsPeer(t),
		`{"requestId":"interop-seed-1","aliases":[{"name":"kept","model":"google/gemma-4-12b"}]}`)
	require.Equal(t, http.StatusOK, status, "seeding the document failed: %s", receipt.Error)
	assertStoredAliasNames(t, stdin, msgs, "kept")

	t.Run("a host this node does not pin never reaches the validator", func(t *testing.T) {
		// The stranger pins this node, so it can verify the server and complete a
		// handshake: a rejection here is the pin gate, not a failed transport.
		strangerDir := t.TempDir()
		mintClusterIdentity(t, strangerDir, "alias-write-stranger")
		writePin(t, strangerDir, fx.nodeUUID, readNodeCert(t, fx.nodeDir))
		writeActiveAdmission(t, strangerDir)
		cfg, ok := clustertrust.Open(strangerDir).ClientTLSConfig(fx.nodeUUID)
		require.True(t, ok, "the stranger must be able to build a pinned client")
		stranger := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}

		resp, err := stranger.Post(peerAliasWriteURL, "application/json",
			strings.NewReader(`{"jsonrpc":"2.0","method":"`+aliasWriteMethod+
				`","params":{"requestId":"interop-stranger-1","aliases":[{"name":"forged","model":"x"}]}}`))
		if err == nil {
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			require.Equal(t, http.StatusForbidden, resp.StatusCode, "an unpinned cluster host was admitted")
		} else {
			t.Logf("unpinned host refused at the transport: %v", err)
		}

		assertStoredAliasNames(t, stdin, msgs, "kept")
	})

	t.Run("a frame with no document is refused rather than read as an erasure", func(t *testing.T) {
		// Absent and empty mean different things here: the first is a caller that
		// sent no edit, the second is an edit that deletes everything.
		status, receipt := postAliasWrite(t, fx.clientAsPeer(t), `{"requestId":"interop-nodoc-1"}`)
		require.Equal(t, http.StatusBadRequest, status, "a document-less frame was accepted: %+v", receipt)

		assertStoredAliasNames(t, stdin, msgs, "kept")
	})

	t.Run("a frame with no request id is refused because no answer could be sent", func(t *testing.T) {
		status, _ := postAliasWrite(t, fx.clientAsPeer(t), `{"aliases":[{"name":"unanswerable","model":"x"}]}`)
		require.Equal(t, http.StatusBadRequest, status, "an unanswerable frame was accepted")

		assertStoredAliasNames(t, stdin, msgs, "kept")
	})

	t.Run("reading the route is not a way to write it", func(t *testing.T) {
		resp, err := fx.clientAsPeer(t).Get(peerAliasWriteURL)
		require.NoError(t, err, "GET the alias route")
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, "GET was answered as something")
	})
}
