// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grandcat/zeroconf"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

// testPortBase is the six-port service block this test process allocated, and
// every supervised broker starts with --port-base testPortBase. Its workers
// therefore bind nothing in the canonical 14318-14323 block, so the suite
// runs beside a live PAIR instead of skipping when PAIR holds those ports
// (issue #226). TestMain allocates it once per process.
var testPortBase int

// allocatedBases records every base this process has handed out. Blocks are
// released once probed (the broker's own bind must be free to land), and the
// OS happily reissues a just-released ephemeral port to the next ":0" probe —
// so without this ledger, a second newPortBlock can be handed a block
// overlapping the first. Ledger entries keep distinct callers distinct.
var allocatedBases struct {
	mu    sync.Mutex
	bases []int
}

// reservePortBlock reports whether the block at base overlaps no previously
// handed-out block, recording it when it does not.
func reservePortBlock(base int) bool {
	allocatedBases.mu.Lock()
	defer allocatedBases.mu.Unlock()
	top := base + noderec.PortBlockSize
	for _, other := range allocatedBases.bases {
		if base < other+noderec.PortBlockSize && other < top {
			return false
		}
	}
	allocatedBases.bases = append(allocatedBases.bases, base)
	return true
}

// allocatePortBlock finds a base where all six block ports bind cleanly. It
// probes the wildcard socket shape ("tcp", ":N") the workers themselves bind,
// not 127.0.0.1 only: an IPv4 squatter that fools a loopback probe cannot
// pass this. The returned block is released before the caller uses it, so the
// broker's own bind is still the authoritative claim against anything outside
// this process; reservePortBlock is what keeps intra-process allocations
// apart after that release.
func allocatePortBlock() (int, error) {
	for range 20 {
		probe, err := net.Listen("tcp", ":0")
		if err != nil {
			return 0, fmt.Errorf("candidate probe listen: %w", err)
		}
		base := probe.Addr().(*net.TCPAddr).Port
		probe.Close()
		if err := noderec.ValidatePortBase(base); err != nil {
			continue // an ephemeral pick too near the port-space edge
		}
		if !reservePortBlock(base) {
			continue // already promised to another caller of this process
		}
		listeners, err := claimBlock(base)
		for _, ln := range listeners {
			ln.Close()
		}
		if err == nil {
			return base, nil
		}
	}
	return 0, fmt.Errorf("no free %d-port block after 20 candidate bases", noderec.PortBlockSize)
}

// claimBlock binds every port of the block at base. On failure it returns the
// listeners it did manage to bind, so the caller can release them.
func claimBlock(base int) ([]net.Listener, error) {
	listeners := make([]net.Listener, 0, noderec.PortBlockSize)
	for offset := range noderec.PortBlockSize {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", base+offset))
		if err != nil {
			return listeners, fmt.Errorf("bind %d: %w", base+offset, err)
		}
		listeners = append(listeners, ln)
	}
	return listeners, nil
}

// newPortBlock allocates a block for a caller that needs a broker on its own
// ports while another block-using broker is running (two-broker coexistence
// tests). Like testPortBase, the claim is released before use.
func newPortBlock(t *testing.T) int {
	t.Helper()
	base, err := allocatePortBlock()
	require.NoError(t, err, "allocate a free service port block")
	t.Logf("allocated port block %d-%d", base, base+noderec.PortBlockSize-1)
	return base
}

// blockPort returns the port of key inside the block at base, failing the test
// for a key that is not part of the block.
func blockPort(t *testing.T, base int, key noderec.ServiceKey) int {
	t.Helper()
	port, ok := noderec.ServicePort(base, key)
	require.True(t, ok, "service %s is not in the port block", key)
	return port
}

// blockHostPort renders the block port of key as a dialable 127.0.0.1 host:port.
func blockHostPort(t *testing.T, key noderec.ServiceKey) string {
	t.Helper()
	return net.JoinHostPort("127.0.0.1", blockPortText(t, key))
}

// blockPortText renders the block port of key as a string, for JoinHostPort
// against a non-loopback address.
func blockPortText(t *testing.T, key noderec.ServiceKey) string {
	t.Helper()
	return strconv.Itoa(blockPort(t, testPortBase, key))
}

// blockDataPlaneURLs renders the block's wl and er endpoints as host:port/path
// for the inter-node data plane. TestMain uses it before any *testing.T exists.
func blockDataPlaneURLs(base int) (workloadEvents, errorsSync string) {
	wl, _ := noderec.ServicePort(base, noderec.ServiceWorkload)
	er, _ := noderec.ServicePort(base, noderec.ServiceErrors)
	return fmt.Sprintf("127.0.0.1:%d/v1/workloads/events", wl),
		fmt.Sprintf("127.0.0.1:%d/v1/errors", er)
}

// appendPortBase adds --port-base base unless args already set one, so a test
// that wants an explicit block (newPortBlock) overrides the process-wide one
// instead of colliding with it.
func appendPortBase(args []string, base int) []string {
	for _, arg := range args {
		if arg == "--port-base" {
			return args
		}
	}
	return append(args, "--port-base", strconv.Itoa(base))
}

// browseForTXT browses service for records whose instance is this host and
// whose TXT records all contain want, returning that TXT. want doubles as the
// discriminator between this run's advertisement and a live PAIR's sharing
// the hostname-named instance (#226). Returns nil if none arrives in timeout.
//
// The browse runs as repeated short passes, each with a fresh resolver: this
// host's own mDNS responders (a live PAIR's scanner and this run's) both
// answer the same instance name with different TXTs, and a resolver caches
// one name's TXT for its whole life. One long pass can latch onto the other
// responder's TXT and never look again; fresh passes re-query and the answers
// alternate until one carries want.
func browseForTXT(t *testing.T, service, instance string, timeout time.Duration, want string) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		pass := min(3*time.Second, remaining)
		if txt := browseForTXTOnce(t, service, instance, pass, want); txt != nil {
			return txt
		}
	}
}

// browseForTXTOnce runs one resolver lifetime: browse service, accept an
// entry named instance whose joined TXT contains want, and return that TXT.
// Returns nil if the pass window closes first.
func browseForTXTOnce(t *testing.T, service, instance string, timeout time.Duration, want string) []string {
	t.Helper()
	resolver, err := zeroconf.NewResolver(nil)
	require.NoError(t, err, "resolver")

	entries := make(chan *zeroconf.ServiceEntry)
	found := make(chan []string, 1)
	go func() {
		for e := range entries {
			if e.Instance == instance && strings.Contains(strings.Join(e.Text, ";"), want) {
				select {
				case found <- e.Text:
				default:
				}
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	require.NoError(t, resolver.Browse(ctx, service, testDomain, entries), "browse")

	select {
	case text := <-found:
		return text
	case <-ctx.Done():
		return nil
	}
}
