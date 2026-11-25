// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Test-side mDNS advertisements.
//
// A stub node used to be advertised with zeroconf.Register, which defaults the
// advertised host to the machine's own hostname, so every test run announced
// "A <machine-hostname>.local -> 127.0.0.1" (and a conflicting SRV target) to
// every real node on the LAN. A real PAIR node then replaced its correct entry
// with a dead one pointing at the test machine (issue #229).
//
// registerTestAdvertisement below replaces every such call site. It refuses to
// run when the host label it was given is the machine's name, so a future edit
// cannot reintroduce the conflict by passing hostname again, and it pins the
// records it assembles itself — the SRV target and the A and AAAA records — to
// a synthetic label and the loopback address, so no machine-chosen address can
// appear in them. What it does not cover is the TXT payload, which every call
// site passes through untouched: the cluster-stub sites still publish their
// real routable address in their ip=/ips= keys (159 such records in the
// post-conversion capture against 167 at baseline — the same pre-existing
// channel, deliberately left alone by this conversion). Closing that TXT
// channel is a known, separate follow-up.
package tests

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/grandcat/zeroconf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The testHost* constants are the synthetic host labels test advertisements
// use instead of the machine hostname. They carry no dot, and the guard below
// treats one as a claim of the machine name only when the machine is literally
// named like it, so they are safe on this machine and on CI runners.
const (
	testHostOne   = "nvpair-test-1"
	testHostTwo   = "nvpair-test-2"
	testHostThree = "nvpair-test-3"
	testHostFour  = "nvpair-test-4"
	testHostFive  = "nvpair-test-5"
	testHostSix   = "nvpair-test-6"
	testHostSeven = "nvpair-test-7"

	// testHostBrokerTwo / testHostBrokerThree cover the second and third
	// concurrent stubs of the one cross-process broker test, which has three
	// simultaneous advertisements and therefore needs two labels beyond the
	// seven single-site labels.
	testHostBrokerTwo   = "nvpair-test-8"
	testHostBrokerThree = "nvpair-test-9"

	// testHostLoopPrefix is the label prefix for a site that registers several
	// advertisements in one loop; each iteration gets its own number so the
	// labels remain distinct within the run.
	testHostLoopPrefix = "nvpair-test-loop"

	// testHostLoopCount is the number of loop-variant labels the guard tests
	// cover; keep it in step with the largest loop in the converted sites.
	testHostLoopCount = 3
)

// testHostLabels lists every single-stub host label the converted call sites
// use, plus the loop variants, so the guard test can assert none of them ever
// collides with this machine's name.
func testHostLabels() []string {
	labels := []string{
		testHostOne, testHostTwo, testHostThree, testHostFour,
		testHostFive, testHostSix, testHostSeven,
		testHostBrokerTwo, testHostBrokerThree,
	}
	for i := 1; i <= testHostLoopCount; i++ {
		labels = append(labels, fmt.Sprintf("%s-%d", testHostLoopPrefix, i))
	}
	return labels
}

// claimsMachineHostname reports whether a host label claims the machine's own
// mDNS name: the test label and the machine hostname agree on their first DNS
// label, which is the part that resolves under .local.
//
// It compares first labels exactly, case-insensitively. Substring containment
// would be wrong — a machine named "test" must not reject the innocuous label
// "nvpair-test-1", and a label like "nothome-server" is not the machine name —
// so the comparison is equality of the first label only, never Contains.
func claimsMachineHostname(hostLabel, hostname string) bool {
	return strings.EqualFold(firstLabel(hostLabel), firstLabel(hostname))
}

func firstLabel(s string) string {
	return strings.SplitN(s, ".", 2)[0]
}

// shutdownDeadline bounds one grandcat teardown, with room to spare: a healthy
// host finishes unregistering in single-digit milliseconds, so anything still
// running this long after the request is parked rather than merely slow.
const shutdownDeadline = 5 * time.Second

// boundedShutdown unregisters a test advertisement and reports whether grandcat
// finished within shutdownDeadline.
//
// The bound is needed because the teardown cannot be interrupted from here:
// Server.Shutdown takes its shutdown lock, then sends the goodbye on every
// multicast-capable interface the host reports — loopback, VPN tunnels and
// peer-to-peer interfaces included — before it closes any socket, and grandcat
// sets no write deadline on that socket at all. An interface that answers
// "network is unreachable" is harmless, because grandcat discards write errors;
// one that accepts the packet into a queue that never drains leaves the write
// waiting for the socket to become writable, and nothing ever wakes it. On such
// a host every advertisement in this package would be torn down by a parked
// goroutine, and because cleanups run one test at a time the whole package
// stalls until go test's own timeout dumps it.
//
// What this bounds is the wait, not the write: after a timeout the parked
// goroutine keeps the socket until this binary exits, and the advertisement is
// retracted only when its records reach their TTL. That residue is always a
// synthetic label pointing at 127.0.0.1 and never this machine's hostname, so
// what is left behind is a dead entry a peer drops by itself, not a live node's
// entry being replaced.
func boundedShutdown(srv *zeroconf.Server) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Shutdown()
	}()
	select {
	case <-done:
		return true
	case <-time.After(shutdownDeadline):
		return false
	}
}

// logStuckTeardown reports an advertisement that outlived shutdownDeadline. It
// is a report and not a failure: a host that cannot send a goodbye is an
// environment this suite cannot fully exercise, and that is worth stating once
// per advertisement rather than turning every test that advertised anything into
// a failure. Call it with the test's own *testing.T — a throwaway one discards
// everything written to it.
func logStuckTeardown(t *testing.T, what string) {
	t.Helper()
	t.Logf("%s did not finish unregistering within %s: a multicast interface on this host accepts the goodbye "+
		"write into a queue it never drains, so its records linger until their TTL and grandcat's unregister "+
		"goroutine stays parked until this binary exits", what, shutdownDeadline)
}

// registerTestAdvertisement publishes one service advertisement for a test stub
// node, under a synthetic host label and a loopback A record instead of the
// machine hostname and a machine-chosen address.
//
// The host label is passed to RegisterProxy as a bare label. grandcat trims
// dots from the label and then re-appends the domain; the branch meant to
// spare a label that already ends with the domain compares a dot-trimmed
// label against a domain still spelled "local." (testDomain, trailing dot
// included), so that check can never succeed and the domain is always
// re-appended. Pass bare labels, as every converted site does: "nvpair-test-1"
// advertises as "nvpair-test-1.local.", while a pre-suffixed
// "nvpair-test-1.local" would advertise as "nvpair-test-1.local.local." —
// verified on the wire against the vendored grandcat v1.0.0.
//
// It fails the test when hostLabel claims the machine hostname, so a future
// call site cannot reintroduce the conflict by passing os.Hostname(); every
// converted site passes one of the synthetic labels above and can never reach
// that check. ifaces stays nil, so the announcement reaches every interface and
// same-process discovery keeps working exactly as before the conversion. The
// cleanup it registers bounds grandcat's teardown, so an interface that will not
// take the goodbye cannot stall the rest of the package — see boundedShutdown.
func registerTestAdvertisement(t *testing.T, instance, service, hostLabel string, port int, txt []string) *zeroconf.Server {
	t.Helper()

	hostname, err := os.Hostname()
	require.NoError(t, err, "read machine hostname")
	require.Falsef(t, claimsMachineHostname(hostLabel, hostname),
		"test advertisement must not claim the machine hostname: host label %q resolves as %q under .local, "+
			"the same name as this machine (%q); issue #229 — such announcements tell real nodes the services are "+
			"on this machine and kill the live entries when the test exits",
		hostLabel, hostLabel+".local.", hostname)

	srv, err := zeroconf.RegisterProxy(instance, service, testDomain, port, hostLabel, []string{"127.0.0.1"}, txt, nil)
	require.NoErrorf(t, err, "register test advertisement %s under %s", instance, hostLabel)
	t.Cleanup(func() {
		if !boundedShutdown(srv) {
			logStuckTeardown(t, "the test advertisement "+instance+" under "+hostLabel)
		}
	})
	return srv
}

// TestClaimsMachineHostname_FlagsOnlyTheMachineName pins the guard's boundary:
// it flags every spelling of the machine's own name and refuses to flag the
// synthetic labels the converted call sites use, including against a machine
// whose name is a substring or superstring of a test label.
func TestClaimsMachineHostname_FlagsOnlyTheMachineName(t *testing.T) {
	tests := []struct {
		name      string
		hostLabel string
		hostname  string
		want      bool
	}{
		{"same name", "home-server", "home-server", true},
		{"machine FQDN against a label", "home-server", "home-server.lab", true},
		{"test label against a machine FQDN", testHostOne, "nvpair-test-1.lab", true},
		{"differ only by case", "Home-Server", "home-server", true},
		{"machine name with a domain suffix", "home-server", "home-server.example.com", true},
		{"label with a domain suffix", "home-server.local", "home-server", true},
		{"dotted claim of a machine FQDN", "home-server.local", "home-server.lab", true},
		{"a different name", "indigo", "home-server", false},
		{"every converted test label", testHostTwo, "nvpair-test-9", false},
		{"loop label against the seven", testHostLoopPrefix + "-1", testHostOne, false},
		{"machine named test against a containment trap", testHostOne, "test", false},
		{"machine is a superstring of the label", "nvpair-test-10", testHostOne, false},
		{"label is a superstring of the machine name", "notnvpair-test-1", "nvpair-test-1", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := claimsMachineHostname(tc.hostLabel, tc.hostname); got != tc.want {
				t.Fatalf("claimsMachineHostname(%q, %q) = %v, want %v", tc.hostLabel, tc.hostname, got, tc.want)
			}
		})
	}

	t.Run("the actual machine name is always a claim", func(t *testing.T) {
		hostname, err := os.Hostname()
		require.NoError(t, err)
		require.Truef(t, claimsMachineHostname(hostname, hostname),
			"a test label equal to this machine's hostname (%q) must count as a claim", hostname)
		for _, label := range testHostLabels() {
			// The loop labels can never collide with a real hostname here on
			// purpose; this machine's first label differs from every one of
			// them (checked, not assumed, below).
			// A synthetic label must never collide with this machine's real
			// name — checked here, not assumed from the label family.
			if claimsMachineHostname(label, hostname) {
				t.Fatalf("synthetic label %q collides with this machine's hostname %q", label, hostname)
			}
		}
	})
}

// normalizeHostLabel reduces an mDNS name — a bare label, an FQDN with a
// trailing dot as it appears on the wire, or an FQDN without one — to the same
// bare first label claimsMachineHostname compares, so a wire assertion and the
// guard speak one language.
func normalizeHostLabel(s string) string {
	return firstLabel(strings.TrimSuffix(s, "."))
}

// callOutcome records what happened to a helper call made on a throwaway
// *testing.T inside its own goroutine. A require failure on that throwaway T
// runs FailNow, which unwinds with runtime.Goexit and dies inside the goroutine
// only, so the deferred function here can report how far the helper got.
type callOutcome struct {
	reachedEnd bool
	crash      any
}

// runOnThrowawayT runs fn with an unwatched *testing.T in a private goroutine
// and reports whether the helper finished or aborted. A helper that calls
// t.FailNow cannot be observed any other way: failing the caller's own *testing.T
// would take the whole test binary with it.
func runOnThrowawayT(t *testing.T, fn func(ft *testing.T)) callOutcome {
	t.Helper()

	out := callOutcome{}
	done := make(chan struct{})
	go func() {
		defer func() {
			out.crash = recover()
			close(done)
		}()
		fn(new(testing.T))
		out.reachedEnd = true
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("the helper never returned: it must either abort or register")
	}
	return out
}

// TestRegisterTestAdvertisement_RejectsAClaimOfTheMachineName drives the guard
// through the helper a real call site uses: handing the helper the machine's own
// hostname must abort it before it publishes anything, so no converted site can
// silently go back to announcing this machine (issue #229).
func TestRegisterTestAdvertisement_RejectsAClaimOfTheMachineName(t *testing.T) {
	hostname, err := os.Hostname()
	require.NoError(t, err, "read machine hostname")
	require.Falsef(t, claimsMachineHostname(testHostOne, hostname),
		"this machine is named %q, which collides with synthetic label %q; a differently named machine is needed to run this",
		hostname, testHostOne)

	claimed := runOnThrowawayT(t, func(ft *testing.T) {
		registerTestAdvertisement(ft, "nvpair-guard-claim-probe", "_t5guard._tcp", hostname, 59911, nil)
	})
	require.Nilf(t, claimed.crash, "the helper crashed instead of failing the test: %v", claimed.crash)
	require.Falsef(t, claimed.reachedEnd,
		"the helper published an advertisement whose host label is this machine's hostname %q instead of refusing it; "+
			"that is the exact announcement issue #229 is about", hostname)

	// The control keeps the assertion above from passing vacuously: with a
	// synthetic label the same helper does reach the wire, so the abort above is
	// the guard refusing the claim and not a broken helper.
	//
	// controlUnregistered reports whether that control left the wire. It is set
	// from the goroutine runOnThrowawayT owns and read only once that goroutine
	// has reported itself finished, and it is reported through t rather than ft
	// because a throwaway *testing.T discards everything written to it.
	controlUnregistered := true
	control := runOnThrowawayT(t, func(ft *testing.T) {
		srv := registerTestAdvertisement(ft, "nvpair-guard-control-probe", "_t5guard._tcp", testHostOne, 59911, nil)
		if srv != nil {
			// The throwaway *testing.T never runs its cleanups, so the
			// helper's bounded teardown can never fire: unregister the
			// server on this goroutine instead, deferred right where it is
			// created so it leaves the wire even if the closure unwinds
			// before returning normally.
			defer func() { controlUnregistered = boundedShutdown(srv) }()
		}
	})
	require.Nilf(t, control.crash, "the helper crashed on a synthetic label: %v", control.crash)
	require.Truef(t, control.reachedEnd,
		"the helper refused the synthetic label %q too, so the refusal of %q proves nothing", testHostOne, hostname)
	if !controlUnregistered {
		logStuckTeardown(t, "the control advertisement of "+testHostOne)
	}
}

// TestZeroconfAdvertisements_AllRoutedThroughTheHelper pins the conversion
// itself. A call site that went back to plain zeroconf.Register would open the
// machine-hostname conflict again (issue #229) and no behavioral test here would
// notice, because every consumer asserts on instance IDs and TXT keys rather than
// on the advertised host — so this is the gate that keeps it from coming back.
//
// Its threat model is the accidental regression, not sabotage: it catches a
// call site reverting to the plain constructor and cites the offender as
// file:line:code, but a line spelled differently sails past the needle —
// `var reg = zc.Register` called as reg(...), an aliased import, or a call
// from any non-test file all keep this gate green. Read it as a hair trigger
// against a careless edit, never as a barrier against a determined one; the
// wire read-back test is the witness that the helper itself behaves.
func TestZeroconfAdvertisements_AllRoutedThroughTheHelper(t *testing.T) {
	files, err := os.ReadDir(".")
	require.NoError(t, err, "read test package directory")

	var offenders []string
	for _, de := range files {
		if de.IsDir() || !strings.HasSuffix(de.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(de.Name())
		require.NoErrorf(t, err, "read %s", de.Name())
		// Built by concatenation so this file's own source cannot match the
		// needle it searches for.
		const callNeedle = "zeroconf." + "Register" + "("
		for i, line := range strings.Split(string(src), "\n") {
			// The plain constructor's name followed by an open parenthesis:
			// no RegisterProxy call can match, so prose mentions of the old
			// name never do either.
			if strings.Contains(line, callNeedle) {
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", de.Name(), i+1, strings.TrimSpace(line)))
			}
		}
	}
	require.Emptyf(t, offenders,
		"test advertisements must go through registerTestAdvertisement, which refuses a host label that claims the "+
			"machine hostname; plain zeroconf.Register defaults that label to os.Hostname():\n%s",
		strings.Join(offenders, "\n"))
}

// browseOnce resolves one service through a resolver it owns alone, for up to
// window, and returns the first entry whose instance matches; nil, nil when
// the window passes without seeing it. Every browse gets its own resolver: one
// grandcat client shares a single multicast socket between the read loops of
// all the browses made through it, so two live browses on one resolver would
// each silently swallow part of the other's responses.
func browseOnce(service, instance string, window time.Duration) (*zeroconf.ServiceEntry, error) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()

	entries := make(chan *zeroconf.ServiceEntry)
	found := make(chan *zeroconf.ServiceEntry, 1)
	go func() {
		for e := range entries {
			if e.Instance == instance {
				select {
				case found <- e:
				default:
				}
			}
		}
	}()

	if err := resolver.Browse(ctx, service, testDomain, entries); err != nil {
		return nil, err
	}
	select {
	case e := <-found:
		return e, nil
	case <-ctx.Done():
		return nil, nil
	}
}

// discoverProbeWithMulticastControl decides what a browse that never saw the
// probe's advertisement actually means, because "never discovered" has two
// opposite causes and only one of them makes a skip honest. It publishes one
// control advertisement directly through zeroconf.RegisterProxy — deliberately
// NOT through registerTestAdvertisement, which is the very thing this suite
// pins — and browses the probe's service and the control's service at the same
// time:
//
//   - control seen, probe not seen: multicast works here, so the helper's own
//     registration is the broken one. t.Fatalf, never a skip: the host and
//     address assertions that follow the caller's browse are the suite's only
//     witnesses to what issue #229 is about, and skipping would retire them in
//     silence.
//   - probe seen after all: the first browse was merely slow; the entry is
//     returned so the caller's assertions still run.
//   - neither seen: multicast genuinely is unavailable here, and only now is a
//     skip telling the truth.
//
// The control carries a host label outside the nvpair-test-* family — checked
// against the real hostname with the same first-label guard the helper applies
// — and an explicit 127.0.0.1, and it is unregistered by a bounded defer on
// every exit path, including the fatal and skip ones, so the diagnosis leaves
// nothing announced behind and a host that will not take the goodbye cannot
// stall the package here either.
func discoverProbeWithMulticastControl(t *testing.T, probeService, probeInstance string) *zeroconf.ServiceEntry {
	t.Helper()

	const (
		window          = 8 * time.Second
		controlService  = "_t5ctl._tcp"
		controlInstance = "nvpair-mcast-control"
		controlHost     = "nvpair-mcast-host"
		controlPort     = 59913
	)

	hostname, err := os.Hostname()
	require.NoError(t, err, "read machine hostname")
	require.Falsef(t, claimsMachineHostname(controlHost, hostname),
		"the multicast control label %q collides with this machine's hostname %q; a differently named machine is needed to run this",
		controlHost, hostname)

	srv, err := zeroconf.RegisterProxy(controlInstance, controlService, testDomain, controlPort, controlHost, []string{"127.0.0.1"}, nil, nil)
	if err != nil {
		t.Skipf("the probe stayed unseen and even a control published straight through grandcat could not be registered (%v): this environment does not support the mDNS this test needs", err)
	}
	defer func() {
		if !boundedShutdown(srv) {
			logStuckTeardown(t, "the multicast control "+controlInstance)
		}
	}()

	type browseOutcome struct {
		entry *zeroconf.ServiceEntry
		err   error
	}
	probeCh := make(chan browseOutcome, 1)
	controlCh := make(chan browseOutcome, 1)
	go func() {
		e, err := browseOnce(probeService, probeInstance, window)
		probeCh <- browseOutcome{e, err}
	}()
	go func() {
		e, err := browseOnce(controlService, controlInstance, window)
		controlCh <- browseOutcome{e, err}
	}()
	probe, control := <-probeCh, <-controlCh

	switch {
	case probe.entry != nil:
		return probe.entry
	case control.entry != nil && probe.err == nil:
		t.Fatalf("multicast works in this environment — the control advertisement was discovered while the helper's %q never once appeared under %s — registerTestAdvertisement's registration is what is broken here, and this test is the suite's only witness to the host and address claims issue #229 is about", probeInstance, probeService)
	case control.entry != nil:
		t.Skipf("the control was discovered but re-browsing the probe's service itself failed (%v): a broken helper cannot be told apart from a resolver fault here", probe.err)
	case probe.err != nil || control.err != nil:
		t.Skipf("neither advertisement was discovered and the diagnosis hit resolver faults (probe: %v, control: %v): multicast is unavailable in this environment", probe.err, control.err)
	default:
		t.Skipf("neither the probe nor the directly-published control appeared within %s of browsing: multicast is genuinely unavailable in this environment, so this machine's announcements cannot be observed — or pinned — here", window)
	}
	return nil // t.Fatalf and t.Skipf unwind with runtime.Goexit; reached by no honest path
}

// TestRegisterTestAdvertisement_AdvertisesOnlyItsOwnHostAndLoopback reads back
// one advertisement through the same resolver production discovery uses, and
// asserts what issue #229 is about: the SRV target names a synthetic host, the
// A record says 127.0.0.1, there is no AAAA record at all — the helper passes
// only the loopback IPv4, so any IPv6 on the wire is a machine-chosen address —
// and neither address class carries this machine's hostname. If the helper ever
// went back to Register's defaults, or passed a machine-chosen address in
// either family, this is what fails.
func TestRegisterTestAdvertisement_AdvertisesOnlyItsOwnHostAndLoopback(t *testing.T) {
	const (
		service  = "_t5wire._tcp"
		instance = "nvpair-guard-wire"
		host     = testHostTwo
		port     = 59912
	)
	hostname, err := os.Hostname()
	require.NoError(t, err, "read machine hostname")
	require.Falsef(t, claimsMachineHostname(host, hostname),
		"this machine is named %q, which collides with synthetic label %q; a different machine is needed to run this",
		hostname, host)

	registerTestAdvertisement(t, instance, service, host, port, []string{"probe=guard"})

	resolver, err := zeroconf.NewResolver(nil)
	require.NoError(t, err, "resolver")
	entries := make(chan *zeroconf.ServiceEntry)
	found := make(chan *zeroconf.ServiceEntry, 1)
	go func() {
		for e := range entries {
			if e.Instance == instance {
				select {
				case found <- e:
				default:
				}
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	require.NoError(t, resolver.Browse(ctx, service, testDomain, entries), "browse")

	var entry *zeroconf.ServiceEntry
	select {
	case entry = <-found:
	case <-ctx.Done():
		// Skipping straight away would state the environment's condition as
		// fact without checking it: a helper publishing under a name nobody
		// browses skips in exactly the same way, silently taking every
		// assertion below with it. Diagnose against a directly-published
		// control first — see discoverProbeWithMulticastControl.
		entry = discoverProbeWithMulticastControl(t, service, instance)
	}

	assert.Equal(t, host+".local.", entry.HostName, "SRV target / advertised host")
	require.Len(t, entry.AddrIPv4, 1, "A records for the advertised host: %v", entry.AddrIPv4)
	assert.Equal(t, "127.0.0.1", entry.AddrIPv4[0].String(), "A record address")
	assert.Emptyf(t, entry.AddrIPv6,
		"AAAA records for the advertised host: %v — registerTestAdvertisement hands grandcat only 127.0.0.1, so any IPv6 on the wire is a machine-chosen address leaking out of the announcement", entry.AddrIPv6)
	assert.NotEqualf(t, normalizeHostLabel(hostname), normalizeHostLabel(entry.HostName),
		"the advertisement carries this machine's hostname (%q): test nodes are telling the LAN they are this machine",
		entry.HostName)
}
