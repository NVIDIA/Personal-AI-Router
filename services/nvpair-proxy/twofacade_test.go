// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Coverage for one process hosting several engine facades at once.
//
// Every other test in this package builds a single-facade proxy, because one
// engine is enough to exercise routing. These are the ones that would catch a
// misroute between facades, because they are the only place two facades exist
// to be confused with each other.

import (
	"bytes"
	"encoding/json"
	stderrors "errors"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/engines"
	"nvpair-shared/schedulerwire"
)

// recordingWriter captures the newline-delimited JSON-RPC frames a proxy writes
// upward, so a test can inspect the notifications its facades emit.
type recordingWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *recordingWriter) Read([]byte) (int, error) { return 0, stderrors.New("no input") }

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *recordingWriter) lines() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out [][]byte
	for _, line := range bytes.Split(w.buf.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			out = append(out, append([]byte(nil), line...))
		}
	}
	return out
}

// enableOnFreePort enables one engine's facade on an ephemeral port, retrying
// if the port was taken between the probe and the bind.
//
// freeTCPPort can only probe-then-close — a bind probe cannot be held open and
// handed over — so the port is genuinely free when checked and may not be a
// moment later. Retrying is the only honest fix; treating the probe as
// authoritative is what made these tests flake on "address already in use".
func enableOnFreePort(t *testing.T, p *Proxy, engine string) int {
	t.Helper()
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		port := freeTCPPort(t)
		_, err := p.enableFacade(enableFacadeParams{
			Engine:              engine,
			Port:                port,
			IgnorePersistedPort: true,
		})
		if err == nil {
			return port
		}
		require.ErrorIs(t, err, errFacadeBindFailed, "enable (%v, %v)", engine, port)
		lastErr = err
	}
	require.FailNowf(t, "every probed facade port was taken before the bind", "engine %s: %v", engine, lastErr)
	return 0
}

// twoFacadeProxy enables a facade for every engine on one host, each on its own
// ephemeral port so the test does not depend on a real engine's port being free.
func twoFacadeProxy(t *testing.T) *Proxy {
	t.Helper()
	redirectConfigDir(t)

	p := NewProxy(NewCodec(rwNop{}))
	p.serveCtx = t.Context()
	for _, e := range engines.All() {
		enableOnFreePort(t, p, e.Name)
	}
	t.Cleanup(func() { p.shutdown(t.Context()) })

	require.Len(t, p.enabledFacades(), len(engines.All()), "enabled facades")
	return p
}

// Each facade binds its own listener and keeps its own dialect. A shared port
// or a shared profile would mean one engine's clients reaching the other's
// translation.
func TestTwoFacadesKeepSeparatePortsAndProfiles(t *testing.T) {
	p := twoFacadeProxy(t)

	ports := map[int]string{}
	for _, e := range engines.All() {
		f := p.facadeFor(e.Name)
		require.NotNil(t, f, "no facade enabled for")
		assert.Equal(t, e.Name, f.profile.Name, "facade for")
		assert.NotContains(t, ports, f.port, "facade %s must use a distinct port", e.Name)
		ports[f.port] = e.Name
	}
}

// Enabling an engine twice is an idempotent success on the existing listener,
// so a redelivered enable cannot tear down a working facade or displace its
// sibling.
func TestReEnablingOneFacadeLeavesBothIntact(t *testing.T) {
	p := twoFacadeProxy(t)

	first := engines.All()[0]
	before := p.facadeFor(first.Name)

	result, err := p.enableFacade(enableFacadeParams{
		Engine: first.Name,
		Port:   before.port,
	})
	require.NoError(t, err, "re-enable")
	assert.Equal(t, before.port, result.Port, "re-enable reported port")
	assert.Same(t, before, p.facadeFor(first.Name), "re-enable replaced the running facade")
	assert.Len(t, p.enabledFacades(), len(engines.All()), "enabled facades after re-enable")
}

// An enable that loses a bind race withdraws only its own engine. This is the
// property that made the collapse acceptable: before, a lost race ended the
// process, which with several facades would take working listeners down.
func TestOneFacadeFailingToBindLeavesTheOthersServing(t *testing.T) {
	redirectConfigDir(t)

	all := engines.All()
	if len(all) < 2 {
		t.Skip("needs at least two engines")
	}
	surviving, failing := all[0], all[1]

	p := NewProxy(NewCodec(rwNop{}))
	p.serveCtx = t.Context()
	t.Cleanup(func() { p.shutdown(t.Context()) })

	enableOnFreePort(t, p, surviving.Name)

	// Hold a port so the second facade cannot have it.
	squatter, err := net.Listen("tcp", ":0")
	require.NoError(t, err, "listen")
	defer squatter.Close()
	taken := squatter.Addr().(*net.TCPAddr).Port

	_, err = p.enableFacade(enableFacadeParams{
		Engine: failing.Name, Port: taken, IgnorePersistedPort: true,
	})
	require.Error(t, err, "enabling (%v)", taken)
	assert.ErrorIs(t, err, errFacadeBindFailed, "bind failure was not tagged retryable")

	assert.Nil(t, p.facadeFor(failing.Name))
	assert.NotNil(t, p.facadeFor(surviving.Name))
	assert.Len(t, p.enabledFacades(), 1, "enabled facades")
}

// A facade-scoped request reaches only the engine it names, and an engine with
// no facade is refused rather than served by whichever one happens to exist.
func TestAddressedRequestReachesOnlyItsOwnFacade(t *testing.T) {
	p := twoFacadeProxy(t)

	all := engines.All()
	first, second := all[0], all[1]

	// Give each facade a distinct manual node, then confirm nodes/list on one
	// engine never reports the other's.
	for i, e := range []engines.Engine{first, second} {
		node := Node{
			ID:        e.Name + "-node",
			Port:      9000 + i,
			Addresses: []string{"127.0.0.1"},
		}
		p.facadeFor(e.Name).discovery.AddManual(node)
	}

	for _, e := range []engines.Engine{first, second} {
		f := p.facadeFor(e.Name)
		nodes := f.discovery.Nodes()
		require.Len(t, nodes, 1)
		assert.Equal(t, e.Name+"-node", nodes[0].ID)
	}

	// An engine with no facade resolves to nothing rather than to a sibling.
	assert.Nil(t, p.facadeFor("vllm"), "unknown engine resolved to the")
	assert.Nil(t, p.facadeFor(""), "an unaddressed message resolved to the")
}

// The scheduler baseline and the reservation map are process-wide, so a
// dispatch through one facade is visible to the other. Two facades bursting at
// once are competing for the same node's GPU; per-facade state would let both
// pick the same idle node in the same instant, which is the whole reason for
// hosting them together.
func TestFacadesShareSchedulerStateAndReservations(t *testing.T) {
	p := twoFacadeProxy(t)

	all := engines.All()
	first, second := all[0], all[1]

	p.SetPrioritySnapshot(schedulerwire.Priority{
		Generation: 1,
		Nodes:      []string{"x", "y"},
		Ranks:      []schedulerwire.NodeRank{{ID: "x"}, {ID: "y"}},
	})

	// One dispatch through each facade. The second must see the first's claim.
	_, firstRes := p.reserveCandidate(p.facadeFor(first.Name), reservationCandidates("x", "y"))
	_, secondRes := p.reserveCandidate(p.facadeFor(second.Name), reservationCandidates("x", "y"))

	require.True(t, firstRes.held, "a facade failed to take a reservation from the shared snapshot")
	require.True(t, secondRes.held, "a facade failed to take a reservation from the shared snapshot")
	require.NotEqual(t, secondRes.nodeID, firstRes.nodeID, "both facades dispatched to")

	// Releasing through one facade's request does not disturb the other's.
	p.releaseReservation(firstRes)
	assert.Equal(t, 1, reservationCount(p, secondRes.nodeID), "")
}

// A panic while handling one engine's request must not end the process, because
// the process now holds every engine's listener and the inference streaming
// through it.
//
// Driven through the real dispatch with a payload that panics inside a handler:
// node/select reads the facade's discovery, so a nil discovery panics there
// rather than at the boundary, which is what makes this a test of containment
// and not of argument validation.
func TestPanicHandlingOneEngineLeavesTheOtherServing(t *testing.T) {
	p := twoFacadeProxy(t)

	all := engines.All()
	victim, bystander := all[0], all[1]

	// Take the victim facade's discovery out from under its handler.
	p.facadeFor(victim.Name).discovery = nil

	params, err := json.Marshal(map[string]string{"id": "some-node"})
	require.NoError(t, err)
	id := json.RawMessage(`7`)
	msg := &Message{
		Method: engines.AddressMethod(victim.Name, "node/select"),
		Params: params,
		ID:     &id,
	}

	// The bare call: if the panic is not contained this test binary dies here,
	// which is the failure mode being guarded against.
	p.handleMessage(msg)

	// The bystander facade is untouched and still answers.
	require.NotNil(t, p.facadeFor(bystander.Name))
	bystanderMsg := &Message{
		Method: engines.AddressMethod(bystander.Name, "nodes/list"),
		ID:     &id,
	}
	p.handleMessage(bystanderMsg)

	assert.Len(t, p.enabledFacades(), len(all), "enabled facades")
}

// A panic during bring-up withdraws that facade and releases its port, while
// every other facade keeps serving.
//
// This path deliberately does not run on the error return — it is a deferred
// rollback, because a panic skips the error return entirely. Without a test,
// dropping the defer (or the listener close inside start) leaves a published
// facade holding a port with no serving loop, and CI stays green.
func TestPanicDuringEnableWithdrawsOnlyThatFacade(t *testing.T) {
	redirectConfigDir(t)

	all := engines.All()
	if len(all) < 2 {
		t.Skip("needs at least two engines")
	}
	surviving, panicking := all[0], all[1]

	p := NewProxy(NewCodec(rwNop{}))
	p.serveCtx = t.Context()
	t.Cleanup(func() { p.shutdown(t.Context()) })

	enableOnFreePort(t, p, surviving.Name)

	// Panic inside bring-up, after the facade is published and its listener is
	// bound: nil the host codec so the ready notification dereferences it.
	port := freeTCPPort(t)
	codec := p.codec
	p.codec = nil
	assert.Panics(t, func() {
		_, _ = p.enableFacade(enableFacadeParams{
			Engine: panicking.Name, Port: port, IgnorePersistedPort: true,
		})
	}, "enable did not panic, so the rollback path was not exercised")
	p.codec = codec

	assert.Nil(t, p.facadeFor(panicking.Name))
	assert.NotNil(t, p.facadeFor(surviving.Name))

	// The port is bindable again, which is the part the deferred rollback is
	// for: a listener left holding it would make the next enable read as a lost
	// bind race and strand that engine on a fallback for the process life.
	//
	// Re-enabling is the check rather than a bare net.Listen, because it
	// distinguishes the two reasons a bind can fail here. If this process still
	// held the port the rollback leaked it; if something else took it in the
	// meantime that is an unrelated race, and skipping says so instead of
	// blaming the rollback.
	if _, err := p.enableFacade(enableFacadeParams{
		Engine: panicking.Name, Port: port, IgnorePersistedPort: true,
	}); err != nil {
		if stderrors.Is(err, errFacadeBindFailed) {
			t.Skipf("port %d was taken by something else between the rollback and the re-enable: %v", port, err)
		}
		require.FailNowf(t, "re-enable after the rollback failed for a non-bind reason", "%v", err)
	}
	assert.NotNil(t, p.facadeFor(panicking.Name), " (%v)", port)
}

// Alias validation runs on the surface the broker actually uses. The addresses
// arrive in facade/enable now rather than on argv, and the alias listener is a
// plaintext one, so a weakened check here would put a LAN plaintext listener in
// front of the router.
func TestEnableFacadeRejectsUnsafeAliasAddresses(t *testing.T) {
	redirectConfigDir(t)

	var aliasEngine, plainEngine engines.Engine
	for _, e := range engines.All() {
		p, ok := profileFor(e.Name)
		require.True(t, ok, "no profile for")
		if p.SupportsHostAlias {
			aliasEngine = e
		} else {
			plainEngine = e
		}
	}
	if aliasEngine.Name == "" || plainEngine.Name == "" {
		t.Skip("needs one alias-capable and one alias-incapable engine")
	}

	for _, tc := range []struct {
		name   string
		engine string
		alias  string
	}{
		{"a LAN address", aliasEngine.Name, "192.168.1.20:11433"},
		{"the wildcard address", aliasEngine.Name, "0.0.0.0:11433"},
		{"an engine with no inherited host variable", plainEngine.Name, "127.0.0.1:11433"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewProxy(NewCodec(rwNop{}))
			p.serveCtx = t.Context()
			t.Cleanup(func() { p.shutdown(t.Context()) })

			_, err := p.enableFacade(enableFacadeParams{
				Engine:              tc.engine,
				Port:                freeTCPPort(t),
				AliasAddresses:      []string{tc.alias},
				IgnorePersistedPort: true,
			})
			require.Error(t, err, "enable accepted alias")
			// Rejected outright, not enabled-then-partially-configured.
			assert.Nil(t, p.facadeFor(tc.engine), "a rejected alias still left a")
		})
	}
}

// Facade notifications are addressed, so the broker can attribute a bare
// "ready" to an engine when several share the stream. Without the address the
// two facades' events are indistinguishable on the wire.
func TestBothFacadesAddressTheirNotifications(t *testing.T) {
	redirectConfigDir(t)

	recorded := &recordingWriter{}
	p := NewProxy(NewCodec(recorded))
	p.serveCtx = t.Context()
	t.Cleanup(func() { p.shutdown(t.Context()) })

	for _, e := range engines.All() {
		enableOnFreePort(t, p, e.Name)
	}

	readyFor := map[string]bool{}
	for _, line := range recorded.lines() {
		var msg struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		engine, bare := engines.SplitAddressedMethod(msg.Method)
		if bare != "ready" {
			continue
		}
		if engine == "" {
			assert.Failf(t, "facade notification was not addressed", "method %q", msg.Method)
			continue
		}
		readyFor[engine] = true
	}

	for _, e := range engines.All() {
		assert.Contains(t, readyFor, e.Name, "no addressed ready notification")
	}
}
