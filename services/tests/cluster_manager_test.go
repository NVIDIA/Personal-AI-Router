// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"

	"nvpair-shared/jsonrpc"
)

// cmProc is a running nvpair-cluster-manager subprocess driven over stdio.
type cmProc struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	msgs   <-chan jsonrpc.Message
	buf    []jsonrpc.Message
	nextID int
}

func startCM(t *testing.T, configDir string, port int) *cmProc {
	return startCMEnv(t, configDir, port, nil)
}

// startCMEnv is startCM with extra environment variables appended to the
// subprocess env (used to drive test-only fault injection, e.g. the
// cancel-vs-Completion commit-window delay).
func startCMEnv(t *testing.T, configDir string, port int, extraEnv []string) *cmProc {
	t.Helper()
	cmd := exec.Command(clusterMgrBin, "--config-dir", configDir, "--port", strconv.Itoa(port))
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start cluster-manager: %v", err)
	}
	return &cmProc{t: t, cmd: cmd, stdin: stdin, msgs: startMsgReader(stdout), nextID: 1}
}

func (p *cmProc) stop() {
	_ = p.stdin.Close()
	done := make(chan struct{})
	go func() { _ = p.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = p.cmd.Process.Kill()
	}
}

func (p *cmProc) pump(want func(jsonrpc.Message) bool, timeout time.Duration) jsonrpc.Message {
	p.t.Helper()
	for i, m := range p.buf {
		if want(m) {
			p.buf = append(p.buf[:i], p.buf[i+1:]...)
			return m
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case m, ok := <-p.msgs:
			if !ok {
				p.t.Fatal("cluster-manager stdout closed unexpectedly")
			}
			if want(m) {
				return m
			}
			p.buf = append(p.buf, m)
		case <-timer.C:
			p.t.Fatal("timed out waiting on cluster-manager message")
		}
	}
}

func idEquals(raw *json.RawMessage, id int) bool {
	if raw == nil {
		return false
	}
	var got int
	return json.Unmarshal(*raw, &got) == nil && got == id
}

func (p *cmProc) call(method string, params any) jsonrpc.Message {
	p.t.Helper()
	id := p.nextID
	p.nextID++
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	b = append(b, '\n')
	if _, err := p.stdin.Write(b); err != nil {
		p.t.Fatalf("write %s: %v", method, err)
	}
	resp := p.pump(func(m jsonrpc.Message) bool { return m.Method == "" && idEquals(m.ID, id) }, 15*time.Second)
	if resp.Error != nil {
		p.t.Fatalf("%s returned error %d: %s", method, resp.Error.Code, resp.Error.Message)
	}
	return resp
}

// callExpectError issues a request and returns the response without failing on
// a JSON-RPC error, so a test can assert the error path.
func (p *cmProc) callExpectError(method string, params any) jsonrpc.Message {
	p.t.Helper()
	id := p.nextID
	p.nextID++
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	b = append(b, '\n')
	if _, err := p.stdin.Write(b); err != nil {
		p.t.Fatalf("write %s: %v", method, err)
	}
	return p.pump(func(m jsonrpc.Message) bool { return m.Method == "" && idEquals(m.ID, id) }, 15*time.Second)
}

func (p *cmProc) waitNotify(method string) jsonrpc.Message {
	p.t.Helper()
	return p.pump(func(m jsonrpc.Message) bool { return m.Method == method }, 15*time.Second)
}

func decodeResult[T any](t *testing.T, msg jsonrpc.Message) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(msg.Result, &v); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return v
}

// TestClusterManagerPairing exercises the full happy path across two real
// processes: create -> invite -> respond (PIN) -> paired -> remove, over stdio
// JSON-RPC and the HTTP/mTLS inter-node channel.
func TestClusterManagerPairing(t *testing.T) {
	a := startCM(t, t.TempDir(), 14821)
	defer a.stop()
	b := startCM(t, t.TempDir(), 14822)
	defer b.stop()

	type nodeID struct {
		NodeUUID  string `json:"nodeUuid"`
		ClusterID string `json:"clusterId"`
	}
	aInfo := decodeResult[nodeID](t, a.call("cluster:get-node-id", nil))
	bInfo := decodeResult[nodeID](t, b.call("cluster:get-node-id", nil))
	if aInfo.NodeUUID == "" || bInfo.NodeUUID == "" {
		t.Fatal("expected non-empty node UUIDs")
	}
	if bInfo.ClusterID != "" {
		t.Fatalf("node B should start unclustered, got %q", bInfo.ClusterID)
	}

	// Give both inter-node listeners a moment to bind.
	time.Sleep(time.Second)

	// A founds a cluster, then invites B.
	a.call("cluster:create", map[string]any{"clusterFriendlyName": "Test Lab"})

	type inviteResult struct {
		InviteID string `json:"inviteId"`
		State    string `json:"state"`
		Pin      string `json:"pin"`
	}
	inv := decodeResult[inviteResult](t, a.call("cluster:invite-node", map[string]any{
		"address": "127.0.0.1", "port": 14822, "nodeId": "node-b",
	}))
	if inv.State != "pending" {
		t.Fatalf("invite-node state = %q, want pending", inv.State)
	}
	if len(inv.Pin) != 6 {
		t.Fatalf("expected a six-digit PIN, got %q", inv.Pin)
	}

	// B is prompted.
	rcv := b.waitNotify("cluster:invite-received")
	var rcvInvite struct {
		InviteID string `json:"inviteId"`
	}
	if err := json.Unmarshal(rcv.Params, &rcvInvite); err != nil {
		t.Fatalf("decode invite-received: %v", err)
	}
	if rcvInvite.InviteID != inv.InviteID {
		t.Fatalf("invite-received id %q != %q", rcvInvite.InviteID, inv.InviteID)
	}

	// B accepts with the PIN -> paired, adopts A's cluster.
	type invite struct {
		State string `json:"state"`
	}
	resp := decodeResult[invite](t, b.call("cluster:respond-to-invite", map[string]any{
		"inviteId": inv.InviteID, "accept": true, "pin": inv.Pin,
	}))
	if resp.State != "paired" {
		t.Fatalf("respond-to-invite state = %q, want paired", resp.State)
	}

	bAdopted := decodeResult[nodeID](t, b.call("cluster:get-node-id", nil))
	if bAdopted.ClusterID != aInfo.ClusterID && bAdopted.ClusterID == "" {
		t.Fatalf("node B did not adopt a cluster: %q", bAdopted.ClusterID)
	}

	// A's invite flips to paired (completion is served async on A).
	waitInviteState(t, a, inv.InviteID, "paired")

	// Both sides now list two members.
	if got := memberCount(t, a); got != 2 {
		t.Fatalf("node A members = %d, want 2", got)
	}
	if got := memberCount(t, b); got != 2 {
		t.Fatalf("node B members = %d, want 2", got)
	}

	// A removes B.
	type removeResult struct {
		Removed bool `json:"removed"`
	}
	rm := decodeResult[removeResult](t, a.call("nodes:remove", map[string]any{"nodeUuid": bInfo.NodeUUID}))
	if !rm.Removed {
		t.Fatal("nodes:remove removed = false, want true")
	}
	if got := memberCount(t, a); got != 1 {
		t.Fatalf("after removal node A members = %d, want 1", got)
	}
	// B must become unclustered, not a zombie that still shows "Joined".
	waitMembers(t, b, 0)
	bAfter := decodeResult[nodeID](t, b.call("cluster:get-node-id", nil))
	if bAfter.ClusterID != "" {
		t.Fatalf("node B still clustered after being removed: %q", bAfter.ClusterID)
	}
}

// TestInviteAutoFoundsClusterWhenUnclustered verifies that a node that is
// not yet clustered founds a cluster of one as a side effect of the first
// cluster:invite-node, with no separate cluster:create step. The founding is
// authoritative and idempotent: it emits cluster:identity-changed, the invite
// still proceeds to a pending PIN, and a second invite does not re-found.
func TestInviteAutoFoundsClusterWhenUnclustered(t *testing.T) {
	a := startCM(t, t.TempDir(), 14831)
	defer a.stop()
	b := startCM(t, t.TempDir(), 14832)
	defer b.stop()

	// A starts unclustered — there is no cluster:create call anywhere below.
	aInfo := decodeResult[cmNodeID](t, a.call("cluster:get-node-id", nil))
	if aInfo.ClusterID != "" {
		t.Fatalf("node A should start unclustered, got %q", aInfo.ClusterID)
	}

	// Let both inter-node listeners bind.
	time.Sleep(time.Second)

	type inviteResult struct {
		InviteID string `json:"inviteId"`
		State    string `json:"state"`
		Pin      string `json:"pin"`
	}
	inv := decodeResult[inviteResult](t, a.call("cluster:invite-node", map[string]any{
		"address": "127.0.0.1", "port": 14832, "nodeId": "node-b",
	}))
	if inv.State != "pending" || len(inv.Pin) != 6 {
		t.Fatalf("invite-node = %+v, want pending + six-digit pin (invite must proceed after auto-found)", inv)
	}

	// The auto-found emitted cluster:identity-changed with a fresh clusterId so
	// the Broker persists it, exactly like an explicit cluster:create would.
	changed := a.waitNotify("cluster:identity-changed")
	var id struct {
		ClusterID string `json:"clusterId"`
	}
	if err := json.Unmarshal(changed.Params, &id); err != nil {
		t.Fatalf("decode cluster:identity-changed: %v", err)
	}
	if id.ClusterID == "" {
		t.Fatal("cluster:identity-changed carried an empty clusterId; auto-found did not mint one")
	}

	// A is now clustered (reporting the founded id) and is its own sole member.
	aAfter := decodeResult[cmNodeID](t, a.call("cluster:get-node-id", nil))
	if aAfter.ClusterID != id.ClusterID {
		t.Fatalf("get-node-id clusterId = %q, want the founded %q", aAfter.ClusterID, id.ClusterID)
	}
	if got := memberCount(t, a); got != 1 {
		t.Fatalf("after auto-found node A members = %d, want 1 (self)", got)
	}

	// Idempotency: a second invite reuses the existing cluster, never re-founds.
	inv2 := decodeResult[inviteResult](t, a.call("cluster:invite-node", map[string]any{
		"address": "127.0.0.1", "port": 14832, "nodeId": "node-b",
	}))
	if inv2.State != "pending" || len(inv2.Pin) != 6 {
		t.Fatalf("second invite-node = %+v, want pending + six-digit pin", inv2)
	}
	aFinal := decodeResult[cmNodeID](t, a.call("cluster:get-node-id", nil))
	if aFinal.ClusterID != id.ClusterID {
		t.Fatalf("clusterId changed on a second invite: %q != %q (re-founded?)", aFinal.ClusterID, id.ClusterID)
	}
}

// TestRepeatedInviteSameSenderSupersedesPrior verifies that re-inviting the same
// standalone node leaves exactly the newest pairing session usable. This keeps
// PIN-only clients from seeing multiple indistinguishable inbound invites from
// one sender while preserving the separate-sender race covered below.
func TestRepeatedInviteSameSenderSupersedesPrior(t *testing.T) {
	a := startCM(t, t.TempDir(), 14911)
	defer a.stop()
	b := startCM(t, t.TempDir(), 14912)
	defer b.stop()

	time.Sleep(time.Second)

	type inviteResult struct {
		InviteID string `json:"inviteId"`
		State    string `json:"state"`
		Pin      string `json:"pin"`
	}
	invite := func() inviteResult {
		return decodeResult[inviteResult](t, a.call("cluster:invite-node", map[string]any{
			"address": "127.0.0.1", "port": 14912, "nodeId": "node-b",
		}))
	}

	first := invite()
	if first.State != "pending" || len(first.Pin) != 6 {
		t.Fatalf("first invite = %+v, want pending + six-digit PIN", first)
	}
	b.waitNotify("cluster:invite-received")

	second := invite()
	if second.State != "pending" || len(second.Pin) != 6 || second.InviteID == first.InviteID {
		t.Fatalf("second invite = %+v, want a new pending invite + six-digit PIN", second)
	}

	canceledMsg := b.waitNotify("cluster:invite-canceled")
	var canceled inviteResult
	if err := json.Unmarshal(canceledMsg.Params, &canceled); err != nil {
		t.Fatalf("decode cluster:invite-canceled: %v", err)
	}
	if canceled.InviteID != first.InviteID || canceled.State != "canceled" {
		t.Fatalf("canceled invite = %+v, want first invite %s canceled", canceled, first.InviteID)
	}
	receivedMsg := b.waitNotify("cluster:invite-received")
	var received inviteResult
	if err := json.Unmarshal(receivedMsg.Params, &received); err != nil {
		t.Fatalf("decode second cluster:invite-received: %v", err)
	}
	if received.InviteID != second.InviteID {
		t.Fatalf("received inviteId = %q, want newest %q", received.InviteID, second.InviteID)
	}

	waitInviteState(t, a, first.InviteID, "declined")
	waitInviteState(t, b, first.InviteID, "canceled")
	stale := b.callExpectError("cluster:respond-to-invite", map[string]any{
		"inviteId": first.InviteID, "accept": true, "pin": first.Pin,
	})
	if stale.Error == nil {
		t.Fatal("accepting superseded invite succeeded, want invalid-state error")
	}

	paired := decodeResult[inviteResult](t, b.call("cluster:respond-to-invite", map[string]any{
		"inviteId": second.InviteID, "accept": true, "pin": second.Pin,
	}))
	if paired.State != "paired" {
		t.Fatalf("accept newest invite state = %q, want paired", paired.State)
	}
	waitInviteState(t, a, second.InviteID, "paired")
}

// cmNodeID is the subset of cluster:get-node-id we assert on.
type cmNodeID struct {
	NodeUUID  string `json:"nodeUuid"`
	ClusterID string `json:"clusterId"`
}

// pairNodes runs a full invite -> PIN -> paired handshake from inviter to joiner.
func pairNodes(t *testing.T, inviter, joiner *cmProc, joinerPort int, joinerNodeID string) {
	t.Helper()
	type inviteResult struct {
		InviteID string `json:"inviteId"`
		State    string `json:"state"`
		Pin      string `json:"pin"`
	}
	inv := decodeResult[inviteResult](t, inviter.call("cluster:invite-node", map[string]any{
		"address": "127.0.0.1", "port": joinerPort, "nodeId": joinerNodeID,
	}))
	if inv.State != "pending" || len(inv.Pin) != 6 {
		t.Fatalf("invite-node = %+v, want pending + six-digit pin", inv)
	}
	joiner.waitNotify("cluster:invite-received")
	resp := decodeResult[struct {
		State string `json:"state"`
	}](t, joiner.call("cluster:respond-to-invite", map[string]any{
		"inviteId": inv.InviteID, "accept": true, "pin": inv.Pin,
	}))
	if resp.State != "paired" {
		t.Fatalf("respond-to-invite state = %q, want paired", resp.State)
	}
	waitInviteState(t, inviter, inv.InviteID, "paired")
}

// memberUUIDs returns the set of node UUIDs in a node's membership view.
func memberUUIDs(t *testing.T, p *cmProc) map[string]bool {
	t.Helper()
	res := decodeResult[struct {
		Nodes []struct {
			NodeUUID string `json:"nodeUuid"`
		} `json:"nodes"`
	}](t, p.call("nodes:get-initial", nil))
	out := make(map[string]bool, len(res.Nodes))
	for _, n := range res.Nodes {
		out[n.NodeUUID] = true
	}
	return out
}

// waitMembers blocks until a node's membership reaches the wanted count.
func waitMembers(t *testing.T, p *cmProc, want int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if memberCount(t, p) == want {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("node did not reach %d members (have %d)", want, memberCount(t, p))
}

// TestClusterFanout verifies transitive trust: A founds a cluster and invites B
// then C in two independent pairings; B and C must end up trusting each other
// without any direct B<->C pairing, and a removal must fan out cluster-wide.
func TestClusterFanout(t *testing.T) {
	a := startCM(t, t.TempDir(), 14841)
	defer a.stop()
	b := startCM(t, t.TempDir(), 14842)
	defer b.stop()
	c := startCM(t, t.TempDir(), 14843)
	defer c.stop()

	bInfo := decodeResult[cmNodeID](t, b.call("cluster:get-node-id", nil))
	cInfo := decodeResult[cmNodeID](t, c.call("cluster:get-node-id", nil))

	// Let the inter-node listeners bind.
	time.Sleep(time.Second)

	a.call("cluster:create", map[string]any{"clusterFriendlyName": "Test Lab"})
	pairNodes(t, a, b, 14842, "node-b") // A <-> B
	pairNodes(t, a, c, 14843, "node-c") // A <-> C

	// Fan-out: all three converge to a 3-member cluster.
	waitMembers(t, a, 3)
	waitMembers(t, b, 3)
	waitMembers(t, c, 3)

	// The key property: B and C trust each other despite never pairing directly.
	if !memberUUIDs(t, b)[cInfo.NodeUUID] {
		t.Fatal("B did not transitively learn C")
	}
	if !memberUUIDs(t, c)[bInfo.NodeUUID] {
		t.Fatal("C did not transitively learn B")
	}

	// Removal fans out: A removes C, and B drops C too (not just A).
	a.call("nodes:remove", map[string]any{"nodeUuid": cInfo.NodeUUID})
	waitMembers(t, a, 2)
	waitMembers(t, b, 2)
	if memberUUIDs(t, b)[cInfo.NodeUUID] {
		t.Fatal("B still trusts C after a cluster-wide removal")
	}

	// The removed node itself must become unclustered (not merely drop the
	// remover while keeping clusterId + remaining peers — the online-remove
	// zombie that left Settings showing "Joined").
	waitMembers(t, c, 0)
	cAfter := decodeResult[cmNodeID](t, c.call("cluster:get-node-id", nil))
	if cAfter.ClusterID != "" {
		t.Fatalf("C still clustered after being removed: %q", cAfter.ClusterID)
	}
}

// TestClusterLeave verifies a node can cleanly unjoin itself: after C leaves a
// 3-node cluster, C ends up unclustered with no members, and A and B drop C.
// It also checks that nodes:remove refuses to remove self.
func TestClusterLeave(t *testing.T) {
	a := startCM(t, t.TempDir(), 14851)
	defer a.stop()
	b := startCM(t, t.TempDir(), 14852)
	defer b.stop()
	c := startCM(t, t.TempDir(), 14853)
	defer c.stop()

	cInfo := decodeResult[cmNodeID](t, c.call("cluster:get-node-id", nil))

	time.Sleep(time.Second)

	a.call("cluster:create", map[string]any{"clusterFriendlyName": "Test Lab"})
	pairNodes(t, a, b, 14852, "node-b")
	pairNodes(t, a, c, 14853, "node-c")
	waitMembers(t, a, 3)
	waitMembers(t, b, 3)
	waitMembers(t, c, 3)

	// nodes:remove must refuse to remove self.
	selfRm := c.callExpectError("nodes:remove", map[string]any{"nodeUuid": cInfo.NodeUUID})
	if selfRm.Error == nil {
		t.Fatal("nodes:remove on self should be rejected")
	}

	// C leaves the cluster.
	left := decodeResult[struct {
		Left bool `json:"left"`
	}](t, c.call("cluster:leave", nil))
	if !left.Left {
		t.Fatal("cluster:leave left = false, want true")
	}

	// C is now unclustered with no members.
	cAfter := decodeResult[cmNodeID](t, c.call("cluster:get-node-id", nil))
	if cAfter.ClusterID != "" {
		t.Fatalf("C still clustered after leave: %q", cAfter.ClusterID)
	}
	waitMembers(t, c, 0)

	// A and B drop C and converge back to 2 members.
	waitMembers(t, a, 2)
	waitMembers(t, b, 2)
	if memberUUIDs(t, a)[cInfo.NodeUUID] {
		t.Fatal("A still lists C after C left")
	}
	if memberUUIDs(t, b)[cInfo.NodeUUID] {
		t.Fatal("B still lists C after C left")
	}
}

// TestClusterCancelInvite verifies the inviter can abort a pending invite: after
// A cancels, the still-valid PIN no longer works — B entering the correct PIN is
// rejected and never joins — and B is told to drop its prompt
// (cluster:invite-canceled) and clears the pending-inbound member.
func TestClusterCancelInvite(t *testing.T) {
	a := startCM(t, t.TempDir(), 14861)
	defer a.stop()
	b := startCM(t, t.TempDir(), 14862)
	defer b.stop()

	bInfo := decodeResult[cmNodeID](t, b.call("cluster:get-node-id", nil))

	// Let the inter-node listeners bind.
	time.Sleep(time.Second)

	a.call("cluster:create", map[string]any{"clusterFriendlyName": "Test Lab"})

	type inviteResult struct {
		InviteID string `json:"inviteId"`
		State    string `json:"state"`
		Pin      string `json:"pin"`
	}
	inv := decodeResult[inviteResult](t, a.call("cluster:invite-node", map[string]any{
		"address": "127.0.0.1", "port": 14862, "nodeId": "node-b",
	}))
	if inv.State != "pending" || len(inv.Pin) != 6 {
		t.Fatalf("invite-node = %+v, want pending + six-digit pin", inv)
	}

	// B is prompted and briefly holds a pending-inbound member for A.
	b.waitNotify("cluster:invite-received")
	waitMembers(t, b, 1)

	// A cancels the still-pending invite.
	canceled := decodeResult[struct {
		State string `json:"state"`
	}](t, a.call("cluster:cancel-invite", map[string]any{"inviteId": inv.InviteID}))
	if canceled.State != "canceled" {
		t.Fatalf("cancel-invite state = %q, want canceled", canceled.State)
	}

	// B is told to drop its prompt and clears the pending-inbound member.
	b.waitNotify("cluster:invite-canceled")
	waitMembers(t, b, 0)

	// The core of the bug: entering the correct PIN after a cancel must be
	// rejected — B must not join.
	resp := b.callExpectError("cluster:respond-to-invite", map[string]any{
		"inviteId": inv.InviteID, "accept": true, "pin": inv.Pin,
	})
	if resp.Error == nil {
		t.Fatal("respond-to-invite after cancel should be rejected, but succeeded")
	}

	// Neither side ended up paired.
	if memberUUIDs(t, a)[bInfo.NodeUUID] {
		t.Fatal("A paired with B despite canceling the invite")
	}
	bAfter := decodeResult[cmNodeID](t, b.call("cluster:get-node-id", nil))
	if bAfter.ClusterID != "" {
		t.Fatalf("B joined a cluster despite the cancel: %q", bAfter.ClusterID)
	}
	if got := memberCount(t, a); got != 1 {
		t.Fatalf("A members = %d, want 1 (itself only)", got)
	}

	// A's invite reads canceled.
	waitInviteState(t, a, inv.InviteID, "canceled")

	// cancel-invite on an unknown invite → -32001.
	unk := a.callExpectError("cluster:cancel-invite", map[string]any{"inviteId": "inv-does-not-exist"})
	if unk.Error == nil || unk.Error.Code != -32001 {
		t.Fatalf("cancel unknown invite: got %+v, want error code -32001", unk.Error)
	}
}

// TestClusterCancelInviteRace drives the cancel-vs-Completion race directly at
// its commit window and asserts the outcome is never torn. cancel and the
// joiner-driven Completion serialize on the pairing session's mutex and re-check
// the invite state inside it, so exactly one wins: whichever it is, the
// inviter's invite state, the inviter's membership, and the joiner's cluster
// membership must all agree. In particular, a cancel that reports "canceled"
// must mean the joiner did NOT join (the reported bug: cancel returned
// "canceled" while a racing Completion still pinned/added the joiner and
// overwrote the invite to "paired").
//
// To make the window deterministic across processes, the inviter is started with
// NVPAIR_TEST_PAIR_COMMIT_DELAY_MS so onInviterPaired pauses (holding sess.mu,
// after the pending check, before the commit); the joiner's accept is launched
// first, then cancel is fired mid-pause so it contends for the same lock.
func TestClusterCancelInviteRace(t *testing.T) {
	const commitDelayMS = 800
	a := startCMEnv(t, t.TempDir(), 14871, []string{
		fmt.Sprintf("NVPAIR_TEST_PAIR_COMMIT_DELAY_MS=%d", commitDelayMS),
	})
	defer a.stop()
	time.Sleep(time.Second)
	a.call("cluster:create", map[string]any{"clusterFriendlyName": "Race Lab"})

	type inviteResult struct {
		InviteID string `json:"inviteId"`
		State    string `json:"state"`
		Pin      string `json:"pin"`
	}
	type inviteState struct {
		State string `json:"state"`
	}

	const iterations = 6
	for i := 0; i < iterations; i++ {
		port := 14880 + i
		b := startCM(t, t.TempDir(), port)
		time.Sleep(600 * time.Millisecond) // let B's inter-node listener bind
		bInfo := decodeResult[cmNodeID](t, b.call("cluster:get-node-id", nil))

		inv := decodeResult[inviteResult](t, a.call("cluster:invite-node", map[string]any{
			"address": "127.0.0.1", "port": port, "nodeId": fmt.Sprintf("node-b-%d", i),
		}))
		if inv.State != "pending" || len(inv.Pin) != 6 {
			b.stop()
			t.Fatalf("iter %d: invite-node = %+v, want pending + six-digit pin", i, inv)
		}
		b.waitNotify("cluster:invite-received")

		// Accept first (it drives Completion into onInviterPaired, which then
		// pauses holding sess.mu); fire cancel mid-pause so both contend for the
		// session lock. They touch different processes, so the cmProc readers
		// don't clash.
		var wg sync.WaitGroup
		var cancelMsg, respMsg jsonrpc.Message
		wg.Add(2)
		go func() {
			defer wg.Done()
			respMsg = b.callExpectError("cluster:respond-to-invite", map[string]any{
				"inviteId": inv.InviteID, "accept": true, "pin": inv.Pin,
			})
		}()
		go func() {
			defer wg.Done()
			// Land inside the inviter's commit pause (after Completion has entered
			// it, well before it ends).
			time.Sleep(commitDelayMS / 3 * time.Millisecond)
			cancelMsg = a.callExpectError("cluster:cancel-invite", map[string]any{"inviteId": inv.InviteID})
		}()
		wg.Wait()

		cancelSucceeded := cancelMsg.Error == nil && decodeResult[inviteState](t, cancelMsg).State == "canceled"
		acceptPaired := respMsg.Error == nil && decodeResult[inviteState](t, respMsg).State == "paired"
		if cancelSucceeded && acceptPaired {
			b.stop()
			t.Fatalf("iter %d: both cancel and accept reported success (torn)", i)
		}

		finalState := decodeResult[inviteState](t, a.call("cluster:invite-status",
			map[string]any{"inviteId": inv.InviteID})).State
		aHasB := memberUUIDs(t, a)[bInfo.NodeUUID]
		bCluster := decodeResult[cmNodeID](t, b.call("cluster:get-node-id", nil)).ClusterID

		switch finalState {
		case "paired":
			if cancelSucceeded {
				b.stop()
				t.Fatalf("iter %d: invite is paired but cancel returned canceled (torn)", i)
			}
			if !aHasB {
				b.stop()
				t.Fatalf("iter %d: invite is paired but inviter does not list the joiner (torn)", i)
			}
			if bCluster == "" {
				b.stop()
				t.Fatalf("iter %d: invite is paired but joiner did not join a cluster (torn)", i)
			}
		case "canceled", "failed":
			if aHasB {
				b.stop()
				t.Fatalf("iter %d: invite is %s but inviter still lists the joiner as a member (torn)", i, finalState)
			}
			if cancelSucceeded && bCluster != "" {
				b.stop()
				t.Fatalf("iter %d: cancel returned canceled but joiner joined cluster %q (torn)", i, bCluster)
			}
		default:
			b.stop()
			t.Fatalf("iter %d: unexpected final invite state %q", i, finalState)
		}
		b.stop()
	}
}

// TestClusterDeclineInvite verifies a joiner decline propagates to the inviter:
// after B declines, A's invite-status flips to declined, A emits
// cluster:invite-declined, and — because A's cluster was invite-created — A
// leaves so both sides end standalone. A late Completion cannot pair.
func TestClusterDeclineInvite(t *testing.T) {
	a := startCM(t, t.TempDir(), 14871)
	defer a.stop()
	b := startCM(t, t.TempDir(), 14872)
	defer b.stop()

	bInfo := decodeResult[cmNodeID](t, b.call("cluster:get-node-id", nil))

	// Let the inter-node listeners bind.
	time.Sleep(time.Second)

	type inviteResult struct {
		InviteID string `json:"inviteId"`
		State    string `json:"state"`
		Pin      string `json:"pin"`
	}
	inv := decodeResult[inviteResult](t, a.call("cluster:invite-node", map[string]any{
		"address": "127.0.0.1", "port": 14872, "nodeId": "node-b",
	}))
	if inv.State != "pending" || len(inv.Pin) != 6 {
		t.Fatalf("invite-node = %+v, want pending + six-digit pin", inv)
	}

	// B is prompted and briefly holds a pending-inbound member for A.
	b.waitNotify("cluster:invite-received")
	waitMembers(t, b, 1)

	// B declines.
	declined := decodeResult[struct {
		State string `json:"state"`
	}](t, b.call("cluster:respond-to-invite", map[string]any{
		"inviteId": inv.InviteID, "accept": false,
	}))
	if declined.State != "declined" {
		t.Fatalf("respond-to-invite decline state = %q, want declined", declined.State)
	}

	// Joiner clears the pending-inbound member and stays unclustered.
	waitMembers(t, b, 0)
	bAfter := decodeResult[cmNodeID](t, b.call("cluster:get-node-id", nil))
	if bAfter.ClusterID != "" {
		t.Fatalf("B clustered after decline: %q", bAfter.ClusterID)
	}

	// Inviter learns about the decline, then leaves the invite-created solo cluster.
	a.waitNotify("cluster:invite-declined")
	waitInviteState(t, a, inv.InviteID, "declined")
	waitUnclustered(t, a)
	waitMembers(t, a, 0)

	// A late Completion with the still-known PIN must not pair either side.
	resp := b.callExpectError("cluster:respond-to-invite", map[string]any{
		"inviteId": inv.InviteID, "accept": true, "pin": inv.Pin,
	})
	if resp.Error == nil {
		t.Fatal("respond-to-invite after decline should be rejected, but succeeded")
	}
	if memberUUIDs(t, a)[bInfo.NodeUUID] {
		t.Fatal("A paired with B despite the decline")
	}
}

// TestClusterInviteExpiresBothSides verifies that when the receiving node never
// answers, the pending invite times out on BOTH sides and both return to a
// usable state that allows a retry. B is given a short invite TTL so its inbound
// invite expires on its own; A is given a long TTL so its outbound invite can
// only be torn down by B's phase:"expire" signal — proving the two-sided
// teardown, not merely each side's independent timer.
func TestClusterInviteExpiresBothSides(t *testing.T) {
	a := startCMEnv(t, t.TempDir(), 14911, []string{
		"NVPAIR_TEST_INVITE_TTL_MS=60000", // A won't self-expire during the test
		"NVPAIR_TEST_INVITE_EXPIRY_INTERVAL_MS=300",
	})
	defer a.stop()
	b := startCMEnv(t, t.TempDir(), 14912, []string{
		"NVPAIR_TEST_INVITE_TTL_MS=1500", // B's inbound invite expires quickly
		"NVPAIR_TEST_INVITE_EXPIRY_INTERVAL_MS=300",
	})
	defer b.stop()

	// Let the inter-node listeners bind.
	time.Sleep(time.Second)

	type inviteResult struct {
		InviteID string `json:"inviteId"`
		State    string `json:"state"`
		Pin      string `json:"pin"`
	}
	inv := decodeResult[inviteResult](t, a.call("cluster:invite-node", map[string]any{
		"address": "127.0.0.1", "port": 14912, "nodeId": "node-b",
	}))
	if inv.State != "pending" || len(inv.Pin) != 6 {
		t.Fatalf("invite-node = %+v, want pending + six-digit pin", inv)
	}

	// B is prompted and briefly holds a pending-inbound member for A.
	b.waitNotify("cluster:invite-received")
	waitMembers(t, b, 1)

	// Nobody answers. B's TTL elapses first: it expires its inbound invite,
	// drops the pending-inbound member, and signals A (phase:"expire").
	b.waitNotify("cluster:invite-expired")
	waitInviteState(t, b, inv.InviteID, "expired")
	waitMembers(t, b, 0)
	waitUnclustered(t, b)

	// A learns of the expiry via B's signal — A's own TTL is 60s, so an expired
	// state here can only come from the phase:"expire" signal — and returns to a
	// clean standalone state (its invite-created solo cluster dissolves).
	a.waitNotify("cluster:invite-expired")
	waitInviteState(t, a, inv.InviteID, "expired")
	waitUnclustered(t, a)
	waitMembers(t, a, 0)

	// Retry: with both sides usable again, a fresh invite mints a new PIN and B is
	// prompted anew.
	inv2 := decodeResult[inviteResult](t, a.call("cluster:invite-node", map[string]any{
		"address": "127.0.0.1", "port": 14912, "nodeId": "node-b",
	}))
	if inv2.State != "pending" || len(inv2.Pin) != 6 {
		t.Fatalf("retry invite-node = %+v, want pending + six-digit pin", inv2)
	}
	if inv2.InviteID == inv.InviteID {
		t.Fatal("retry must mint a fresh inviteId")
	}
	b.waitNotify("cluster:invite-received")
}

// TestClusterDeclinePreservesIntentionalSolo verifies an explicitly created
// cluster of one is not dissolved when an invite is declined.
func TestClusterDeclinePreservesIntentionalSolo(t *testing.T) {
	a := startCM(t, t.TempDir(), 14881)
	defer a.stop()
	b := startCM(t, t.TempDir(), 14882)
	defer b.stop()

	time.Sleep(time.Second)

	a.call("cluster:create", map[string]any{"clusterFriendlyName": "Intentional Lab"})
	before := decodeResult[cmNodeID](t, a.call("cluster:get-node-id", nil))
	if before.ClusterID == "" {
		t.Fatal("A should be clustered after create")
	}

	inv := decodeResult[struct {
		InviteID string `json:"inviteId"`
		State    string `json:"state"`
		Pin      string `json:"pin"`
	}](t, a.call("cluster:invite-node", map[string]any{
		"address": "127.0.0.1", "port": 14882, "nodeId": "node-b",
	}))
	if inv.State != "pending" {
		t.Fatalf("invite-node state = %q, want pending", inv.State)
	}
	b.waitNotify("cluster:invite-received")

	declined := decodeResult[struct {
		State string `json:"state"`
	}](t, b.call("cluster:respond-to-invite", map[string]any{
		"inviteId": inv.InviteID, "accept": false,
	}))
	if declined.State != "declined" {
		t.Fatalf("decline state = %q, want declined", declined.State)
	}

	a.waitNotify("cluster:invite-declined")
	waitInviteState(t, a, inv.InviteID, "declined")

	after := decodeResult[cmNodeID](t, a.call("cluster:get-node-id", nil))
	if after.ClusterID != before.ClusterID {
		t.Fatalf("intentional solo cluster changed: before=%q after=%q", before.ClusterID, after.ClusterID)
	}
	if got := memberCount(t, a); got != 1 {
		t.Fatalf("A members = %d, want 1 (itself)", got)
	}
}

// TestClusterDeclineKeepsSiblingPendingInvite verifies that with two pending
// outbound invites, declining one keeps the invite-created cluster alive so the
// sibling session can still complete against a stable cluster id.
func TestClusterDeclineKeepsSiblingPendingInvite(t *testing.T) {
	a := startCM(t, t.TempDir(), 14891)
	defer a.stop()
	b := startCM(t, t.TempDir(), 14892)
	defer b.stop()
	c := startCM(t, t.TempDir(), 14893)
	defer c.stop()

	time.Sleep(time.Second)

	type inviteResult struct {
		InviteID string `json:"inviteId"`
		State    string `json:"state"`
		Pin      string `json:"pin"`
	}
	invB := decodeResult[inviteResult](t, a.call("cluster:invite-node", map[string]any{
		"address": "127.0.0.1", "port": 14892, "nodeId": "node-b",
	}))
	// The first invite auto-founds A's provenance-tracked invite-created cluster.
	before := decodeResult[cmNodeID](t, a.call("cluster:get-node-id", nil))
	if before.ClusterID == "" {
		t.Fatal("first invite did not auto-found A's cluster")
	}
	invC := decodeResult[inviteResult](t, a.call("cluster:invite-node", map[string]any{
		"address": "127.0.0.1", "port": 14893, "nodeId": "node-c",
	}))
	if invB.State != "pending" || invC.State != "pending" {
		t.Fatalf("want both pending, got B=%+v C=%+v", invB, invC)
	}
	b.waitNotify("cluster:invite-received")
	c.waitNotify("cluster:invite-received")

	// B declines first — A must stay clustered while C's invite is still pending.
	_ = decodeResult[struct {
		State string `json:"state"`
	}](t, b.call("cluster:respond-to-invite", map[string]any{
		"inviteId": invB.InviteID, "accept": false,
	}))
	a.waitNotify("cluster:invite-declined")
	waitInviteState(t, a, invB.InviteID, "declined")

	mid := decodeResult[cmNodeID](t, a.call("cluster:get-node-id", nil))
	if mid.ClusterID != before.ClusterID {
		t.Fatalf("A left while sibling invite still pending: before=%q mid=%q", before.ClusterID, mid.ClusterID)
	}
	waitInviteState(t, a, invC.InviteID, "pending")

	// C declines — now no pending outbound remains, so the invite-created cluster leaves.
	_ = decodeResult[struct {
		State string `json:"state"`
	}](t, c.call("cluster:respond-to-invite", map[string]any{
		"inviteId": invC.InviteID, "accept": false,
	}))
	a.waitNotify("cluster:invite-declined")
	waitInviteState(t, a, invC.InviteID, "declined")
	waitUnclustered(t, a)
}

// TestClusterWrongPinFailsBothSides verifies a wrong PIN ends the invitation on
// both sides: the joiner returns state:"failed" reason:"incorrect-pin"
// and clears its pending-inbound member; the inviter learns via
// cluster:invite-failed, its invite-status flips to failed (reason
// incorrect-pin), and — because its cluster was invite-created — it leaves so
// both sides end standalone. Neither side pins the other.
func TestClusterWrongPinFailsBothSides(t *testing.T) {
	portB := freePort(t)
	a := startCM(t, t.TempDir(), freePort(t))
	defer a.stop()
	b := startCM(t, t.TempDir(), portB)
	defer b.stop()

	aInfo := decodeResult[cmNodeID](t, a.call("cluster:get-node-id", nil))
	bInfo := decodeResult[cmNodeID](t, b.call("cluster:get-node-id", nil))

	// Let the inter-node listeners bind.
	time.Sleep(time.Second)

	type inviteResult struct {
		InviteID string `json:"inviteId"`
		State    string `json:"state"`
		Pin      string `json:"pin"`
		Reason   string `json:"reason"`
	}
	inv := decodeResult[inviteResult](t, a.call("cluster:invite-node", map[string]any{
		"address": "127.0.0.1", "port": portB, "nodeId": "node-b",
	}))
	if inv.State != "pending" || len(inv.Pin) != 6 {
		t.Fatalf("invite-node = %+v, want pending + six-digit pin", inv)
	}

	// B is prompted and briefly holds a pending-inbound member for A.
	b.waitNotify("cluster:invite-received")
	waitMembers(t, b, 1)

	// B responds with a wrong PIN (flip the first digit to guarantee a mismatch).
	failed := decodeResult[inviteResult](t, b.call("cluster:respond-to-invite", map[string]any{
		"inviteId": inv.InviteID, "accept": true, "pin": mutatePin(inv.Pin),
	}))
	if failed.State != "failed" {
		t.Fatalf("respond-to-invite wrong-pin state = %q, want failed", failed.State)
	}
	if failed.Reason != "incorrect-pin" {
		t.Fatalf("respond-to-invite wrong-pin reason = %q, want incorrect-pin", failed.Reason)
	}

	// Joiner clears the pending-inbound member and stays unclustered.
	waitMembers(t, b, 0)
	bAfter := decodeResult[cmNodeID](t, b.call("cluster:get-node-id", nil))
	if bAfter.ClusterID != "" {
		t.Fatalf("B clustered after wrong pin: %q", bAfter.ClusterID)
	}

	// Inviter learns about the failure (with the reason), then leaves the
	// invite-created solo cluster so both sides end standalone.
	a.waitNotify("cluster:invite-failed")
	waitInviteState(t, a, inv.InviteID, "failed")
	aStatus := decodeResult[inviteResult](t, a.call("cluster:invite-status", map[string]any{"inviteId": inv.InviteID}))
	if aStatus.Reason != "incorrect-pin" {
		t.Fatalf("inviter invite-status reason = %q, want incorrect-pin", aStatus.Reason)
	}
	waitUnclustered(t, a)
	waitMembers(t, a, 0)

	// Neither side pinned the other.
	if memberUUIDs(t, a)[bInfo.NodeUUID] {
		t.Fatal("A paired with B despite the wrong pin")
	}
	if memberUUIDs(t, b)[aInfo.NodeUUID] {
		t.Fatal("B paired with A despite the wrong pin")
	}
}

// TestClusterWrongPinPreservesIntentionalSolo verifies an explicitly created
// cluster of one is not dissolved when a pairing fails on a wrong PIN,
// mirroring the decline variant.
func TestClusterWrongPinPreservesIntentionalSolo(t *testing.T) {
	portB := freePort(t)
	a := startCM(t, t.TempDir(), freePort(t))
	defer a.stop()
	b := startCM(t, t.TempDir(), portB)
	defer b.stop()

	time.Sleep(time.Second)

	a.call("cluster:create", map[string]any{"clusterFriendlyName": "Intentional Lab"})
	before := decodeResult[cmNodeID](t, a.call("cluster:get-node-id", nil))
	if before.ClusterID == "" {
		t.Fatal("A should be clustered after create")
	}

	type inviteResult struct {
		InviteID string `json:"inviteId"`
		State    string `json:"state"`
		Pin      string `json:"pin"`
		Reason   string `json:"reason"`
	}
	inv := decodeResult[inviteResult](t, a.call("cluster:invite-node", map[string]any{
		"address": "127.0.0.1", "port": portB, "nodeId": "node-b",
	}))
	if inv.State != "pending" {
		t.Fatalf("invite-node state = %q, want pending", inv.State)
	}
	b.waitNotify("cluster:invite-received")

	failed := decodeResult[inviteResult](t, b.call("cluster:respond-to-invite", map[string]any{
		"inviteId": inv.InviteID, "accept": true, "pin": mutatePin(inv.Pin),
	}))
	if failed.State != "failed" {
		t.Fatalf("wrong-pin state = %q, want failed", failed.State)
	}

	a.waitNotify("cluster:invite-failed")
	waitInviteState(t, a, inv.InviteID, "failed")

	after := decodeResult[cmNodeID](t, a.call("cluster:get-node-id", nil))
	if after.ClusterID != before.ClusterID {
		t.Fatalf("intentional solo cluster changed: before=%q after=%q", before.ClusterID, after.ClusterID)
	}
	if got := memberCount(t, a); got != 1 {
		t.Fatalf("A members = %d, want 1 (itself)", got)
	}
}

// mutatePin returns a six-digit PIN guaranteed to differ from the input by
// flipping its first digit — enough to force an EAP-NOOB MAC verification
// failure (a "wrong PIN") in tests.
func mutatePin(pin string) string {
	b := []byte(pin)
	if len(b) == 0 {
		return "000000"
	}
	if b[0] == '0' {
		b[0] = '1'
	} else {
		b[0] = '0'
	}
	return string(b)
}

func waitInviteState(t *testing.T, p *cmProc, inviteID, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		inv := decodeResult[struct {
			State string `json:"state"`
		}](t, p.call("cluster:invite-status", map[string]any{"inviteId": inviteID}))
		if inv.State == want {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("invite %s did not reach state %q", inviteID, want)
}

// waitUnclustered blocks until cluster:get-node-id reports an empty clusterId.
func waitUnclustered(t *testing.T, p *cmProc) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		id := decodeResult[cmNodeID](t, p.call("cluster:get-node-id", nil))
		if id.ClusterID == "" {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	got := decodeResult[cmNodeID](t, p.call("cluster:get-node-id", nil))
	t.Fatalf("node did not leave cluster, still has clusterId %q", got.ClusterID)
}

func memberCount(t *testing.T, p *cmProc) int {
	t.Helper()
	res := decodeResult[struct {
		Nodes []json.RawMessage `json:"nodes"`
	}](t, p.call("nodes:get-initial", nil))
	return len(res.Nodes)
}

// waitUnclusteredWithin blocks until a node reports an empty clusterId, failing
// if it stays clustered past timeout. Distinct from the 5s waitUnclustered used
// by the leave/cancel tests: the offline self-remove path can take a full
// reconcile heartbeat, so its test needs a longer, explicit budget.
func waitUnclusteredWithin(t *testing.T, p *cmProc, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if decodeResult[cmNodeID](t, p.call("cluster:get-node-id", nil)).ClusterID == "" {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("node did not become unclustered within %s", timeout)
}

// TestClusterSelfRemoveWhenRemovedOffline covers the removal path the
// direct-notify teardown cannot reach: a node removed while it is offline never
// receives the members/remove notification, so on return it would still believe
// it is clustered while every peer has dropped its pin. The periodic reconcile
// must detect that unanimous 403 and self-remove.
func TestClusterSelfRemoveWhenRemovedOffline(t *testing.T) {
	baseA, baseB := t.TempDir(), t.TempDir()
	portB := freePort(t)
	a := startCM(t, baseA, freePort(t))
	defer a.stop()
	b := startCM(t, baseB, portB)

	bInfo := decodeResult[cmNodeID](t, b.call("cluster:get-node-id", nil))

	// Let the inter-node listeners bind before pairing.
	time.Sleep(time.Second)

	a.call("cluster:create", map[string]any{"clusterFriendlyName": "Offline Lab"})
	pairNodes(t, a, b, portB, "node-b")
	waitMembers(t, a, 2)
	waitMembers(t, b, 2)

	bClustered := decodeResult[cmNodeID](t, b.call("cluster:get-node-id", nil))
	if bClustered.ClusterID == "" {
		t.Fatal("B should be clustered after pairing")
	}

	// B goes offline; A removes it. The direct members/remove notify fails (B is
	// down), so A just drops B's pin + membership. B never learns of its eviction.
	b.stop()
	a.call("nodes:remove", map[string]any{"nodeUuid": bInfo.NodeUUID})
	waitMembers(t, a, 1)

	// B returns on the same config dir (same identity, pins, and members.json).
	// A standalone cluster-manager doesn't persist clusterId — the broker does —
	// so reproduce the broker restoring it via cluster:set-identity. B is now the
	// zombie: clusterId set, A still pinned and listed as a member.
	b2 := startCM(t, baseB, portB)
	defer b2.stop()
	b2.call("cluster:set-identity", map[string]any{
		"clusterId":           bClustered.ClusterID,
		"clusterFriendlyName": "Offline Lab",
	})
	if got := decodeResult[cmNodeID](t, b2.call("cluster:get-node-id", nil)); got.ClusterID == "" {
		t.Fatal("precondition: B should have a restored clusterId (the zombie state)")
	}

	// The periodic reconcile reaches A, gets a 403 with A's signed removal
	// tombstone (A removed B, so it holds one). A is B's only peer, so the
	// rejection is unanimous and proven, and B self-removes.
	waitUnclusteredWithin(t, b2, 20*time.Second)
	if got := memberCount(t, b2); got != 0 {
		t.Fatalf("after self-remove B members = %d, want 0", got)
	}
}

// TestClusterNoSelfRemoveWhenPeerLeft is the survivor counterpart to
// TestClusterSelfRemoveWhenRemovedOffline: a node that was offline when its only
// peer voluntarily LEFT must remain the solo cluster rather than self-evict. A
// leave drops the leaver's pins (so the returning survivor's reconcile gets a
// 403) but leaves no removal tombstone naming the survivor, so that 403 carries
// no authenticated proof — and a bare 403 must not tear the survivor down.
func TestClusterNoSelfRemoveWhenPeerLeft(t *testing.T) {
	baseA, baseB := t.TempDir(), t.TempDir()
	portA, portB := freePort(t), freePort(t)
	a := startCM(t, baseA, portA)
	// B stays up (unclustered) for the whole test so A's reconcile reaches it
	// and gets a live 403 rather than an "unreachable" that would block teardown
	// for the wrong reason.
	b := startCM(t, baseB, portB)
	defer b.stop()

	// Let the inter-node listeners bind before pairing.
	time.Sleep(time.Second)

	a.call("cluster:create", map[string]any{"clusterFriendlyName": "Survivor Lab"})
	pairNodes(t, a, b, portB, "node-b")
	waitMembers(t, a, 2)
	waitMembers(t, b, 2)

	aClustered := decodeResult[cmNodeID](t, a.call("cluster:get-node-id", nil))
	if aClustered.ClusterID == "" {
		t.Fatal("A should be clustered after pairing")
	}

	// A goes offline; B leaves. B's departure push to A fails (A is down), and
	// its teardown drops A's pin and clears its tombstones — so B holds no
	// removal tombstone for A.
	a.stop()
	b.call("cluster:leave", nil)
	waitUnclustered(t, b)

	// A returns on the same config dir (same identity, pins, and members.json).
	// The broker restores its clusterId, so A is once again the (only) live
	// member and its reconcile fires against the still-running, now-unclustered B.
	a2 := startCM(t, baseA, portA)
	defer a2.stop()
	a2.call("cluster:set-identity", map[string]any{
		"clusterId":           aClustered.ClusterID,
		"clusterFriendlyName": "Survivor Lab",
	})
	if got := decodeResult[cmNodeID](t, a2.call("cluster:get-node-id", nil)); got.ClusterID == "" {
		t.Fatal("precondition: A should have a restored clusterId")
	}

	// A's reconcile reaches B and gets a bare 403 (B dropped A's pin on leave but
	// holds no tombstone naming A). With no authenticated removal proof, A must
	// keep its own cluster identity but de-pin B as the departed peer.
	assertStillClustered(t, a2, 10*time.Second)
	if got := memberCount(t, a2); got != 1 {
		t.Fatalf("A members = %d, want 1 (surviving self only)", got)
	}
}

// assertStillClustered waits past the first reconcile pass and fails if the node
// self-removed during that window. wait must comfortably exceed the
// cluster-manager's initialReconcileDelay (5s) so at least one pass — and thus
// the unanimous-403 self-remove check — has actually run and declined to tear
// down.
func assertStillClustered(t *testing.T, p *cmProc, wait time.Duration) {
	t.Helper()
	time.Sleep(wait)
	if got := decodeResult[cmNodeID](t, p.call("cluster:get-node-id", nil)); got.ClusterID == "" {
		t.Fatal("node self-removed but should have stayed clustered")
	}
}

// TestClusterNoSelfRemoveWhenPeerAccepts is the soundness counterpart to
// TestClusterSelfRemoveWhenRemovedOffline: a node whose peer still holds its pin
// must NOT self-remove. Only a *unanimous* 403 tears down, so a 200 from the
// (single) peer has to block it — otherwise a healthy or still-converging
// cluster could self-evict.
func TestClusterNoSelfRemoveWhenPeerAccepts(t *testing.T) {
	baseA, baseB := t.TempDir(), t.TempDir()
	portB := freePort(t)
	a := startCM(t, baseA, freePort(t))
	defer a.stop()
	b := startCM(t, baseB, portB)

	// Let the inter-node listeners bind before pairing.
	time.Sleep(time.Second)

	a.call("cluster:create", map[string]any{"clusterFriendlyName": "Healthy Lab"})
	pairNodes(t, a, b, portB, "node-b")
	waitMembers(t, a, 2)
	waitMembers(t, b, 2)

	bClustered := decodeResult[cmNodeID](t, b.call("cluster:get-node-id", nil))
	if bClustered.ClusterID == "" {
		t.Fatal("B should be clustered after pairing")
	}

	// Restart B so its initial reconcile pass fires ~5s after startup (rather
	// than a full heartbeat later). A stays up and never removed B, so B's
	// reconcile gets a 200 — the rejection is not unanimous and B must stay put.
	b.stop()
	b2 := startCM(t, baseB, portB)
	defer b2.stop()
	b2.call("cluster:set-identity", map[string]any{
		"clusterId":           bClustered.ClusterID,
		"clusterFriendlyName": "Healthy Lab",
	})

	assertStillClustered(t, b2, 10*time.Second)
	if got := memberCount(t, b2); got != 2 {
		t.Fatalf("B members = %d, want 2 (self + A); B wrongly self-removed?", got)
	}
}

// TestClusterNoSelfRemoveWhenPeerUnreachable covers the other blocking branch:
// an unreachable peer counts as neither accept nor reject, so it must block
// teardown rather than trigger it. A transient outage (peer down but not
// removed) must never make the surviving node self-evict.
func TestClusterNoSelfRemoveWhenPeerUnreachable(t *testing.T) {
	baseA, baseB := t.TempDir(), t.TempDir()
	portB := freePort(t)
	a := startCM(t, baseA, freePort(t))
	defer a.stop()
	b := startCM(t, baseB, portB)

	// Let the inter-node listeners bind before pairing.
	time.Sleep(time.Second)

	a.call("cluster:create", map[string]any{"clusterFriendlyName": "Outage Lab"})
	pairNodes(t, a, b, portB, "node-b")
	waitMembers(t, a, 2)
	waitMembers(t, b, 2)

	bClustered := decodeResult[cmNodeID](t, b.call("cluster:get-node-id", nil))
	if bClustered.ClusterID == "" {
		t.Fatal("B should be clustered after pairing")
	}

	// A goes down WITHOUT removing B (a transient outage). B restarts and its
	// reconcile can't reach A → unreachable, not a 403 → teardown is blocked and
	// B stays clustered.
	a.stop()
	b.stop()
	b2 := startCM(t, baseB, portB)
	defer b2.stop()
	b2.call("cluster:set-identity", map[string]any{
		"clusterId":           bClustered.ClusterID,
		"clusterFriendlyName": "Outage Lab",
	})

	assertStillClustered(t, b2, 10*time.Second)
	if got := memberCount(t, b2); got != 2 {
		t.Fatalf("B members = %d, want 2 (self + A); B wrongly self-removed on a transient outage?", got)
	}
}

// TestClusterInviteAlreadyClusteredRejected verifies the already-paired guard:
// once a node is clustered it refuses a fresh pairing with state:"rejected"
// (reason "already-clustered", no PIN), and only accepts an invite again after
// it leaves the cluster.
func TestClusterInviteAlreadyClusteredRejected(t *testing.T) {
	a := startCM(t, t.TempDir(), 14881)
	defer a.stop()
	b := startCM(t, t.TempDir(), 14882)
	defer b.stop()

	// Let the inter-node listeners bind.
	time.Sleep(time.Second)

	a.call("cluster:create", map[string]any{"clusterFriendlyName": "Test Lab"})
	pairNodes(t, a, b, 14882, "node-b")
	waitMembers(t, a, 2)
	waitMembers(t, b, 2)

	type inviteResult struct {
		InviteID string `json:"inviteId"`
		State    string `json:"state"`
		Pin      string `json:"pin"`
		Reason   string `json:"reason"`
	}

	// Re-inviting the already-paired node is rejected: no PIN, reason surfaced.
	reinv := decodeResult[inviteResult](t, a.call("cluster:invite-node", map[string]any{
		"address": "127.0.0.1", "port": 14882, "nodeId": "node-b",
	}))
	if reinv.State != "rejected" {
		t.Fatalf("re-invite of a paired node: state = %q, want rejected", reinv.State)
	}
	if reinv.Pin != "" {
		t.Fatalf("re-invite must not mint a PIN, got %q", reinv.Pin)
	}
	if reinv.Reason != "already-clustered" {
		t.Fatalf("re-invite reason = %q, want already-clustered", reinv.Reason)
	}

	// After B leaves the cluster it becomes invitable again: a fresh invite now
	// yields a pending PIN (the guard is keyed on the joiner's cluster state).
	b.call("cluster:leave", nil)
	waitUnclustered(t, b)

	inv := decodeResult[inviteResult](t, a.call("cluster:invite-node", map[string]any{
		"address": "127.0.0.1", "port": 14882, "nodeId": "node-b",
	}))
	if inv.State != "pending" || len(inv.Pin) != 6 {
		t.Fatalf("post-leave invite = %+v, want pending + six-digit PIN", inv)
	}
}

// TestClusterInviteTwoPendingThenClustered reproduces the race where a joiner
// session is opened while the node is still standalone and only accepted after
// it joins a cluster. The handlePairingInitial guard doesn't fire for the second
// invite (its session already exists), so the accept path itself must re-check
// cluster identity. A and C each found a cluster and open an invite to standalone
// B before B joins anything; B accepts A (adopting A's cluster), then accepting
// C's already-pending invite must be rejected -- B must stay in A's cluster and
// never adopt C's or trust C.
func TestClusterInviteTwoPendingThenClustered(t *testing.T) {
	a := startCM(t, t.TempDir(), 14891)
	defer a.stop()
	b := startCM(t, t.TempDir(), 14892)
	defer b.stop()
	c := startCM(t, t.TempDir(), 14893)
	defer c.stop()

	bInfo := decodeResult[cmNodeID](t, b.call("cluster:get-node-id", nil))
	cInfo := decodeResult[cmNodeID](t, c.call("cluster:get-node-id", nil))

	// Let the inter-node listeners bind.
	time.Sleep(time.Second)

	// A and C each found their own cluster.
	type createResult struct {
		ClusterID string `json:"clusterId"`
	}
	aClu := decodeResult[createResult](t, a.call("cluster:create", map[string]any{"clusterFriendlyName": "Cluster A"}))
	c.call("cluster:create", map[string]any{"clusterFriendlyName": "Cluster C"})

	type inviteResult struct {
		InviteID string `json:"inviteId"`
		State    string `json:"state"`
		Pin      string `json:"pin"`
		Reason   string `json:"reason"`
	}

	// Both A and C open an invite to standalone B *before* B joins anything, so B
	// ends up holding two pending-inbound invites (two live joiner sessions).
	invA := decodeResult[inviteResult](t, a.call("cluster:invite-node", map[string]any{
		"address": "127.0.0.1", "port": 14892, "nodeId": bInfo.NodeUUID,
	}))
	if invA.State != "pending" || len(invA.Pin) != 6 {
		t.Fatalf("A invite = %+v, want pending + six-digit PIN", invA)
	}
	b.waitNotify("cluster:invite-received")

	invC := decodeResult[inviteResult](t, c.call("cluster:invite-node", map[string]any{
		"address": "127.0.0.1", "port": 14892, "nodeId": bInfo.NodeUUID,
	}))
	if invC.State != "pending" || len(invC.Pin) != 6 {
		t.Fatalf("C invite = %+v, want pending + six-digit PIN", invC)
	}
	b.waitNotify("cluster:invite-received")

	// B accepts A first -> paired, adopts A's cluster.
	respA := decodeResult[inviteResult](t, b.call("cluster:respond-to-invite", map[string]any{
		"inviteId": invA.InviteID, "accept": true, "pin": invA.Pin,
	}))
	if respA.State != "paired" {
		t.Fatalf("B accept A = %q, want paired", respA.State)
	}
	waitInviteState(t, a, invA.InviteID, "paired")

	bAfterA := decodeResult[cmNodeID](t, b.call("cluster:get-node-id", nil))
	if bAfterA.ClusterID != aClu.ClusterID {
		t.Fatalf("B cluster after accepting A = %q, want A's %q", bAfterA.ClusterID, aClu.ClusterID)
	}

	// Now B accepts C's still-pending invite. B is already clustered, so the
	// accept must be rejected -- the Completion Exchange must not run.
	respC := decodeResult[inviteResult](t, b.call("cluster:respond-to-invite", map[string]any{
		"inviteId": invC.InviteID, "accept": true, "pin": invC.Pin,
	}))
	if respC.State != "rejected" {
		t.Fatalf("B accept C while clustered = %q, want rejected", respC.State)
	}

	// B must still be in A's cluster, not C's, and must not trust C.
	bAfterC := decodeResult[cmNodeID](t, b.call("cluster:get-node-id", nil))
	if bAfterC.ClusterID != aClu.ClusterID {
		t.Fatalf("B cluster after rejecting C = %q, want A's %q (must not adopt C)", bAfterC.ClusterID, aClu.ClusterID)
	}
	if memberUUIDs(t, b)[cInfo.NodeUUID] {
		t.Fatal("B lists C as a member after a rejected accept")
	}
}

// TestClusterInviteRejectedDissolvesAutoFoundedSolo covers the interaction
// between auto-found-on-first-invite and the already-clustered reject:
// an unclustered node that invites an already-clustered target auto-founds a solo
// cluster for the invite, the target rejects it, and the inviter must dissolve
// that solo cluster again (provenance-safe cleanup) rather than stay stuck in a
// one-node cluster it only created to make the rejected invite.
func TestClusterInviteRejectedDissolvesAutoFoundedSolo(t *testing.T) {
	a := startCM(t, t.TempDir(), 14901)
	defer a.stop()
	b := startCM(t, t.TempDir(), 14902)
	defer b.stop()

	// Let the inter-node listeners bind.
	time.Sleep(time.Second)

	// B is already in its own cluster; A stays standalone.
	b.call("cluster:create", map[string]any{"clusterFriendlyName": "Cluster B"})
	aBefore := decodeResult[cmNodeID](t, a.call("cluster:get-node-id", nil))
	if aBefore.ClusterID != "" {
		t.Fatalf("A should start unclustered, got %q", aBefore.ClusterID)
	}

	// A invites B: A auto-founds a solo cluster for the invite, B rejects it
	// because it is already clustered.
	type inviteResult struct {
		State  string `json:"state"`
		Pin    string `json:"pin"`
		Reason string `json:"reason"`
	}
	inv := decodeResult[inviteResult](t, a.call("cluster:invite-node", map[string]any{
		"address": "127.0.0.1", "port": 14902, "nodeId": "node-b",
	}))
	if inv.State != "rejected" || inv.Reason != "already-clustered" {
		t.Fatalf("invite of an already-clustered node = %+v, want rejected/already-clustered", inv)
	}
	if inv.Pin != "" {
		t.Fatalf("rejected invite must not mint a PIN, got %q", inv.Pin)
	}

	// A must dissolve the solo cluster it auto-founded for the rejected invite
	// (nothing joined), ending unclustered again.
	waitUnclustered(t, a)
}
