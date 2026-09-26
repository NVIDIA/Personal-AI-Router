// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func onboardingDiscoveryFixture(cidr string) onboardingInterfacePrefix {
	return onboardingInterfacePrefix{Name: "lan0", Index: 7, Hardware: "02:00:00:00:00:01", Flags: net.FlagUp | net.FlagBroadcast, Prefix: netip.MustParsePrefix(cidr)}
}

type onboardingBannerFixture struct {
	reader  *bytes.Reader
	block   bool
	closed  chan struct{}
	once    sync.Once
	onClose func()
	writes  atomic.Int32
}

func newOnboardingBanner(text string, block bool) *onboardingBannerFixture {
	return &onboardingBannerFixture{reader: bytes.NewReader([]byte(text)), block: block, closed: make(chan struct{})}
}
func (c *onboardingBannerFixture) Read(p []byte) (int, error) {
	if c.reader.Len() > 0 {
		return c.reader.Read(p)
	}
	if c.block {
		<-c.closed
		return 0, net.ErrClosed
	}
	return 0, io.EOF
}
func (c *onboardingBannerFixture) Write([]byte) (int, error) {
	c.writes.Add(1)
	return 0, errors.New("discovery must never send client protocol data")
}
func (c *onboardingBannerFixture) Close() error {
	c.once.Do(func() {
		close(c.closed)
		if c.onClose != nil {
			c.onClose()
		}
	})
	return nil
}
func (c *onboardingBannerFixture) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *onboardingBannerFixture) RemoteAddr() net.Addr             { return &net.TCPAddr{Port: 22} }
func (c *onboardingBannerFixture) SetDeadline(time.Time) error      { return nil }
func (c *onboardingBannerFixture) SetReadDeadline(time.Time) error  { return nil }
func (c *onboardingBannerFixture) SetWriteDeadline(time.Time) error { return nil }

func TestOnboardingDiscoveryScopesAreStableAndFailClosed(t *testing.T) {
	valid := onboardingDiscoveryFixture("192.168.10.3/24")
	one := onboardingScopesFromFacts([]onboardingInterfacePrefix{valid})
	if len(one) != 1 || !one[0].Eligible || one[0].ScopeID != onboardingScopeID(valid) || one[0].CIDR != "192.168.10.0/24" {
		t.Fatalf("scope=%+v", one)
	}
	if !reflect.DeepEqual(one, onboardingScopesFromFacts([]onboardingInterfacePrefix{valid})) {
		t.Fatal("unchanged scope IDs churned")
	}
	for _, change := range []func(*onboardingInterfacePrefix){func(f *onboardingInterfacePrefix) { f.Prefix = netip.MustParsePrefix("10.0.0.2/16") }, func(f *onboardingInterfacePrefix) { f.Flags = 0 }, func(f *onboardingInterfacePrefix) { f.Flags |= net.FlagPointToPoint }, func(f *onboardingInterfacePrefix) { f.Prefix = netip.MustParsePrefix("192.168.10.3/32") }} {
		fact := valid
		change(&fact)
		scopes := onboardingScopesFromFacts([]onboardingInterfacePrefix{fact})
		if len(scopes) != 1 || scopes[0].Eligible || scopes[0].Reason == "" {
			t.Fatalf("unsupported scope admitted: %+v", scopes)
		}
	}
	other := valid
	other.Index = 8
	other.Name = "wifi0"
	for _, scope := range onboardingScopesFromFacts([]onboardingInterfacePrefix{valid, other}) {
		if scope.Eligible {
			t.Fatal("overlapping interface ambiguity admitted")
		}
	}
	public := valid
	public.Prefix = netip.MustParsePrefix("203.0.113.1/24")
	ipv6 := valid
	ipv6.Prefix = netip.MustParsePrefix("fd00::1/64")
	if got := onboardingScopesFromFacts([]onboardingInterfacePrefix{public, ipv6}); len(got) != 0 {
		t.Fatalf("public/IPv6 scopes escaped restriction: %+v", got)
	}
}

func TestOnboardingDiscoveryAddressSetIsSingleScopeAndExcludesAllSelfAddresses(t *testing.T) {
	fact := onboardingDiscoveryFixture("192.168.10.1/29")
	scope := onboardingScopesFromFacts([]onboardingInterfacePrefix{fact})[0]
	seeds := []onboardingDiscoveredDevice{{Address: "192.168.10.3", Source: "mdns"}, {Address: "10.0.0.9", Source: "mdns"}, {Address: "192.168.10.0"}, {Address: "192.168.10.7"}}
	addresses := onboardingScopeAddresses(scope, []netip.Addr{netip.MustParseAddr("192.168.10.4")}, seeds, netip.MustParseAddr("192.168.10.2"))
	want := []netip.Addr{netip.MustParseAddr("192.168.10.3"), netip.MustParseAddr("192.168.10.4"), netip.MustParseAddr("192.168.10.5"), netip.MustParseAddr("192.168.10.6")}
	if !reflect.DeepEqual(addresses, want) {
		t.Fatalf("addresses=%v", addresses)
	}
	fact = onboardingDiscoveryFixture("10.0.0.0/31")
	scope = onboardingScopesFromFacts([]onboardingInterfacePrefix{fact})[0]
	if got := onboardingScopeAddresses(scope, nil, nil); len(got) != 1 || got[0].String() != "10.0.0.1" {
		t.Fatalf("/31 peer=%v", got)
	}
	fact = onboardingDiscoveryFixture("10.0.0.3/24")
	scope = onboardingScopesFromFacts([]onboardingInterfacePrefix{fact})[0]
	if got := onboardingScopeAddresses(scope, nil, nil); len(got) != 253 {
		t.Fatalf("maximum subnet address count=%d", len(got))
	}
}

func TestOnboardingDiscoveryNeighborFactsDoNotExpandScopeOrAssertReadiness(t *testing.T) {
	text := "IP address HW type Flags HW address Mask Device\n192.168.10.4 0x1 0x2 02:00:00:00:00:04 * lan0\n192.168.10.5 0x1 0x0 00:00:00:00:00:00 * lan0\n10.0.0.9 0x1 0x2 02:00:00:00:00:09 * lan0\n192.168.10.6 0x1 0x2 02:00:00:00:00:06 * other0\n"
	fact := onboardingDiscoveryFixture("192.168.10.1/29")
	scope := onboardingScopesFromFacts([]onboardingInterfacePrefix{fact})[0]
	neighbors := onboardingParseNeighbors(strings.NewReader(text), "lan0")
	addresses := onboardingScopeAddresses(scope, neighbors, nil)
	if len(neighbors) != 2 || len(addresses) != 5 || addresses[0].String() != "192.168.10.4" {
		t.Fatalf("neighbor hint/address scope=%v %v", neighbors, addresses)
	}
}

func TestOnboardingDiscoveryReadOnlyBannerBounds(t *testing.T) {
	for _, test := range []struct {
		text  string
		valid bool
	}{{"SSH-2.0-fixture\r\n", true}, {"notice\r\nSSH-1.99-fixture\r\n", true}, {"SSH-2.0-fixture\n", false}, {"SSH-2.0-\r\n", false}, {"SSH-2.0- bad\r\n", false}, {"SSH-2.0-fixture\x00\r\n", false}, {strings.Repeat("x", 256) + "\r\nSSH-2.0-fixture\r\n", false}, {strings.Repeat("notice\r\n", 8) + "SSH-2.0-fixture\r\n", false}, {"HTTP/1.1 200 OK\r\n", false}} {
		connection := newOnboardingBanner(test.text, false)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		got := onboardingReadSSHBanner(ctx, connection)
		cancel()
		_ = connection.Close()
		if got != test.valid || connection.writes.Load() != 0 {
			t.Fatalf("banner accepted=%v expected=%v writes=%d", got, test.valid, connection.writes.Load())
		}
	}
}

func TestOnboardingDiscoveryUntrustedResultsAndBoundedConnections(t *testing.T) {
	fact := onboardingDiscoveryFixture("192.168.10.1/29")
	scope := onboardingScopesFromFacts([]onboardingInterfacePrefix{fact})[0]
	var active, maxActive, attempts atomic.Int32
	var connectionsMu sync.Mutex
	var connections []*onboardingBannerFixture
	access := onboardingDiscoveryIO{prefixes: func() ([]onboardingInterfacePrefix, error) { return []onboardingInterfacePrefix{fact}, nil }, neighbors: func(onboardingDiscoveryScope) []netip.Addr { return nil }, dial: func(_ context.Context, local, address netip.Addr) (net.Conn, error) {
		attempts.Add(1)
		if local.String() != scope.LocalAddress || !fact.Prefix.Masked().Contains(address) || address == local {
			t.Error("probe escaped selected local network")
		}
		now := active.Add(1)
		for old := maxActive.Load(); now > old && !maxActive.CompareAndSwap(old, now); old = maxActive.Load() {
		}
		banner := "not SSH\r\n"
		if address.String() == "192.168.10.3" {
			banner = "SSH-2.0-software-version-not-exported\r\n"
		}
		connection := newOnboardingBanner(banner, false)
		connection.onClose = func() { active.Add(-1) }
		connectionsMu.Lock()
		connections = append(connections, connection)
		connectionsMu.Unlock()
		return connection, nil
	}}
	devices, err := discoverOnboardingWithIO(context.Background(), scope.ScopeID, []onboardingDiscoveredDevice{{Address: "192.168.10.3", Label: "advertised\nname", Source: "mdns"}}, access)
	if err != nil || len(devices) != 1 || devices[0].Address != "192.168.10.3" || devices[0].Port != 22 || devices[0].Source != "mdns+ssh-banner" || devices[0].BootstrapState != "ssh-ready" || attempts.Load() != 5 || maxActive.Load() > onboardingDiscoveryWorkers || active.Load() != 0 {
		t.Fatalf("devices=%+v attempts=%d max=%d active=%d err=%v", devices, attempts.Load(), maxActive.Load(), active.Load(), err)
	}
	data, _ := json.Marshal(devices)
	if strings.Contains(string(data), "software-version") || strings.Contains(string(data), "trusted") {
		t.Fatal("banner software or trust was invented in observations")
	}
	for _, connection := range connections {
		if connection.writes.Load() != 0 {
			t.Fatal("discovery wrote client protocol data")
		}
	}
}

func TestOnboardingDiscoveryRetainsObservedDeviceWithoutSSH(t *testing.T) {
	fact := onboardingDiscoveryFixture("192.168.10.1/29")
	scope := onboardingScopesFromFacts([]onboardingInterfacePrefix{fact})[0]
	access := onboardingDiscoveryIO{
		prefixes: func() ([]onboardingInterfacePrefix, error) {
			return []onboardingInterfacePrefix{fact}, nil
		},
		neighbors: func(onboardingDiscoveryScope) []netip.Addr {
			return nil
		},
		dial: func(
			context.Context,
			netip.Addr,
			netip.Addr,
		) (net.Conn, error) {
			return nil, errors.New("SSH closed")
		},
	}
	devices, err := discoverOnboardingWithIO(
		context.Background(),
		scope.ScopeID,
		[]onboardingDiscoveredDevice{{
			Address: "192.168.10.3",
			Label:   "Observed host",
			Source:  "mdns",
		}},
		access,
	)
	if err != nil || len(devices) != 1 ||
		devices[0].BootstrapState != "bootstrap-required" ||
		devices[0].Source != "mdns" {
		t.Fatalf("devices=%+v error=%v", devices, err)
	}
}

func TestOnboardingDiscoveryCancellationClosesOwnedConnections(t *testing.T) {
	fact := onboardingDiscoveryFixture("192.168.10.1/24")
	scope := onboardingScopesFromFacts([]onboardingInterfacePrefix{fact})[0]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{}, 1)
	var active atomic.Int32
	access := onboardingDiscoveryIO{prefixes: func() ([]onboardingInterfacePrefix, error) { return []onboardingInterfacePrefix{fact}, nil }, neighbors: func(onboardingDiscoveryScope) []netip.Addr { return nil }, dial: func(ctx context.Context, _, _ netip.Addr) (net.Conn, error) {
		active.Add(1)
		connection := newOnboardingBanner("", true)
		connection.onClose = func() { active.Add(-1) }
		select {
		case entered <- struct{}{}:
		default:
		}
		return connection, nil
	}}
	done := make(chan error, 1)
	go func() { _, err := discoverOnboardingWithIO(ctx, scope.ScopeID, nil, access); done <- err }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled search reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel left owned connections blocked")
	}
	if active.Load() != 0 {
		t.Fatalf("cancel retained%d connections", active.Load())
	}
}

func TestOnboardingDiscoveryScopeChangeAndMissingSelectionNeverProbe(t *testing.T) {
	fact := onboardingDiscoveryFixture("192.168.10.1/29")
	scope := onboardingScopesFromFacts([]onboardingInterfacePrefix{fact})[0]
	for _, missing := range []bool{false, true} {
		var reads, dials atomic.Int32
		access := onboardingDiscoveryIO{prefixes: func() ([]onboardingInterfacePrefix, error) {
			current := fact
			if reads.Add(1) > 1 {
				current.Prefix = netip.MustParsePrefix("192.168.20.1/29")
			}
			return []onboardingInterfacePrefix{current}, nil
		}, neighbors: func(onboardingDiscoveryScope) []netip.Addr { return nil }, dial: func(context.Context, netip.Addr, netip.Addr) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("should not dial")
		}}
		id := scope.ScopeID
		if missing {
			id = "not-selected"
		}
		if _, err := discoverOnboardingWithIO(context.Background(), id, nil, access); err == nil || dials.Load() != 0 {
			t.Fatalf("stale/absent selection probed: %v %d", err, dials.Load())
		}
	}
}

func TestOnboardingDiscoveryCancelledMetadataDoesNotAccumulateOSReads(t *testing.T) {
	blocked := make(chan struct{})
	entered := make(chan struct{})
	returned := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(blocked) }) }
	defer unblock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := onboardingReadMetadata(ctx, func() (int, error) { close(entered); <-blocked; close(returned); return 1, nil })
		done <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled metadata was reported as confirmed")
		}
	case <-time.After(time.Second):
		t.Fatal("metadata cancellation blocked")
	}
	second, cancelSecond := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelSecond()
	var calls atomic.Int32
	if _, err := onboardingReadMetadata(second, func() (int, error) { calls.Add(1); return 2, nil }); err == nil || calls.Load() != 0 {
		t.Fatal("a second blocked OS read was launched")
	}
	unblock()
	<-returned
	final, cancelFinal := context.WithTimeout(context.Background(), time.Second)
	defer cancelFinal()
	if value, err := onboardingReadMetadata(final, func() (int, error) { return 3, nil }); err != nil || value != 3 {
		t.Fatalf("metadata guard did not recover: %d %v", value, err)
	}
}

func TestOnboardingDiscoveryOnlyOneSelectedNetworkCanRun(t *testing.T) {
	onboardingDiscoveryActive <- struct{}{}
	defer func() { <-onboardingDiscoveryActive }()
	if _, err := discoverOnboardingDevices(context.Background(), "unused-scope"); err == nil || !strings.Contains(err.Error(), "another nearby-device search") {
		t.Fatalf("concurrent selection was admitted: %v", err)
	}
}

func TestOnboardingDiscoveryNewLocalAliasIsNeverProbed(t *testing.T) {
	fact := onboardingDiscoveryFixture("192.168.10.1/29")
	scope := onboardingScopesFromFacts([]onboardingInterfacePrefix{fact})[0]
	alias := fact
	alias.Prefix = netip.MustParsePrefix("192.168.10.3/29")
	var reads, dials atomic.Int32
	access := onboardingDiscoveryIO{prefixes: func() ([]onboardingInterfacePrefix, error) {
		if reads.Add(1) == 1 {
			return []onboardingInterfacePrefix{fact}, nil
		}
		return []onboardingInterfacePrefix{fact, alias}, nil
	}, neighbors: func(onboardingDiscoveryScope) []netip.Addr { return nil }, dial: func(_ context.Context, _, address netip.Addr) (net.Conn, error) {
		if address == alias.Prefix.Addr() {
			t.Error("new local alias was probed")
		}
		dials.Add(1)
		return nil, errors.New("no service")
	}}
	if _, err := discoverOnboardingWithIO(context.Background(), scope.ScopeID, nil, access); err != nil || dials.Load() != 4 {
		t.Fatalf("alias exclusion dials=%d err=%v", dials.Load(), err)
	}
}
