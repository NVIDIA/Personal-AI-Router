// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const onboardingDiscoveryLimit = 15 * time.Second
const onboardingDiscoveryProbeLimit = 400 * time.Millisecond
const onboardingDiscoveryWorkers = 8

type onboardingDiscoveryScope struct {
	ScopeID      string `json:"scopeId"`
	Interface    string `json:"interface"`
	LocalAddress string `json:"localAddress"`
	CIDR         string `json:"cidr"`
	Eligible     bool   `json:"eligible"`
	Reason       string `json:"reason,omitempty"`
}

type onboardingDiscoveredDevice struct {
	Address        string `json:"address"`
	Port           int    `json:"port"`
	Label          string `json:"label,omitempty"`
	Source         string `json:"source"`
	BootstrapState string `json:"bootstrapState"`
}

type onboardingInterfacePrefix struct {
	Name     string
	Index    int
	Hardware string
	Flags    net.Flags
	Prefix   netip.Prefix // Retains the actual local host address, not only its network.
}

func onboardingDiscoveryScopes(ctx context.Context) ([]onboardingDiscoveryScope, error) {
	ctx, cancel := context.WithTimeout(ctx, onboardingDiscoveryLimit)
	defer cancel()
	facts, err := onboardingReadMetadata(ctx, onboardingLocalPrefixes)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return onboardingScopesFromFacts(facts), nil
}

func onboardingLocalPrefixes() ([]onboardingInterfacePrefix, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, errors.New("local network interfaces could not be read")
	}
	if len(interfaces) > 128 {
		return nil, errors.New("local interface inventory exceeds the supported bound")
	}
	facts := []onboardingInterfacePrefix{}
	for _, iface := range interfaces {
		addresses, err := iface.Addrs()
		if err != nil {
			return nil, errors.New("a local interface address is unknown; refresh network scope selection")
		}
		if len(addresses) > 128 {
			return nil, errors.New("local interface address inventory exceeds the supported bound")
		}
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err != nil || !prefix.Addr().Is4() || !prefix.Addr().IsPrivate() {
				continue
			}
			facts = append(facts, onboardingInterfacePrefix{Name: iface.Name, Index: iface.Index, Hardware: iface.HardwareAddr.String(), Flags: iface.Flags, Prefix: prefix})
			if len(facts) > 128 {
				return nil, errors.New("local private-address inventory exceeds the supported bound; enter a device address")
			}
		}
	}
	return facts, nil
}

func onboardingScopeID(fact onboardingInterfacePrefix) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s", fact.Name, fact.Index, fact.Hardware, fact.Prefix)))
	return hex.EncodeToString(sum[:16])
}

func onboardingScopesFromFacts(facts []onboardingInterfacePrefix) []onboardingDiscoveryScope {
	scopes := []onboardingDiscoveryScope{}
	seen := map[string]bool{}
	for _, fact := range facts {
		if !fact.Prefix.IsValid() || !fact.Prefix.Addr().Is4() || !fact.Prefix.Addr().IsPrivate() {
			continue
		}
		scope := onboardingDiscoveryScope{ScopeID: onboardingScopeID(fact), Interface: fact.Name, LocalAddress: fact.Prefix.Addr().String(), CIDR: fact.Prefix.Masked().String(), Eligible: true}
		if seen[scope.ScopeID] {
			continue
		}
		seen[scope.ScopeID] = true
		switch {
		case fact.Flags&net.FlagUp == 0:
			scope.Reason = "This interface is down"
		case fact.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0:
			scope.Reason = "Choose a directly connected local network or enter a device address"
		case fact.Prefix.Bits() < 24:
			scope.Reason = "This network is larger than /24; enter a device address instead of expanding the search"
		case fact.Prefix.Bits() > 31:
			scope.Reason = "This address has no other directly connected IPv4 host to search"
		}
		for _, other := range facts {
			if other.Index != fact.Index && other.Flags&net.FlagUp != 0 && other.Prefix.IsValid() && other.Prefix.Addr().Is4() && other.Prefix.Overlaps(fact.Prefix) {
				scope.Reason = "Overlapping interfaces make this network ambiguous; enter a device address"
				break
			}
		}
		scope.Eligible = scope.Reason == ""
		scopes = append(scopes, scope)
	}
	sort.Slice(scopes, func(i, j int) bool {
		if scopes[i].Interface != scopes[j].Interface {
			return scopes[i].Interface < scopes[j].Interface
		}
		return scopes[i].ScopeID < scopes[j].ScopeID
	})
	return scopes
}

func onboardingSelectedScope(facts []onboardingInterfacePrefix, id string) (onboardingDiscoveryScope, error) {
	if len(facts) > 128 {
		return onboardingDiscoveryScope{}, errors.New("local network scope inventory exceeds its supported bound")
	}
	for _, scope := range onboardingScopesFromFacts(facts) {
		if scope.ScopeID == id {
			if !scope.Eligible {
				return scope, errors.New(scope.Reason)
			}
			return scope, nil
		}
	}
	return onboardingDiscoveryScope{}, errors.New("selected network scope changed; refresh and select it again")
}

func onboardingScopeAddresses(scope onboardingDiscoveryScope, neighbors []netip.Addr, seeds []onboardingDiscoveredDevice, localAddresses ...netip.Addr) []netip.Addr {
	prefix, err := netip.ParsePrefix(scope.CIDR)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() < 24 || prefix.Bits() > 31 {
		return nil
	}
	local, err := netip.ParseAddr(scope.LocalAddress)
	if err != nil {
		return nil
	}
	seen := map[netip.Addr]bool{}
	self := map[netip.Addr]bool{local: true}
	for _, address := range localAddresses {
		self[address] = true
	}
	addresses := []netip.Addr{}
	network := prefix.Masked().Addr()
	last := network
	for next := last.Next(); prefix.Contains(next); next = next.Next() {
		last = next
	}
	appendAddress := func(address netip.Addr) {
		if !address.Is4() || !prefix.Contains(address) || self[address] || seen[address] {
			return
		}
		if prefix.Bits() < 31 && (address == network || address == last) {
			return
		}
		seen[address] = true
		addresses = append(addresses, address)
	}
	// Existing mDNS observations and passive neighbor facts only prioritize
	// in-scope addresses. Every returned candidate still needs an SSH banner.
	for _, seed := range seeds {
		if address, err := netip.ParseAddr(seed.Address); err == nil {
			appendAddress(address)
		}
	}
	for _, address := range neighbors {
		appendAddress(address)
	}
	for address := network; prefix.Contains(address); address = address.Next() {
		appendAddress(address)
	}
	return addresses
}

func onboardingNeighborFacts(scope onboardingDiscoveryScope) []netip.Addr {
	if runtime.GOOS != "linux" {
		return nil
	} // Unknown is fine: these are ordering hints, not admission.
	file, err := os.Open("/proc/net/arp")
	if err != nil {
		return nil
	}
	defer file.Close()
	return onboardingParseNeighbors(io.LimitReader(file, 64<<10), scope.Interface)
}

func onboardingParseNeighbors(reader io.Reader, iface string) []netip.Addr {
	addresses := []netip.Addr{}
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 6 || fields[5] != iface {
			continue
		}
		flags, err := strconv.ParseUint(fields[2], 0, 32)
		if err != nil || flags&2 == 0 {
			continue
		}
		address, err := netip.ParseAddr(fields[0])
		if err == nil && address.Is4() {
			addresses = append(addresses, address)
		}
		if len(addresses) >= 256 {
			break
		}
	}
	return addresses
}

type onboardingDiscoveryIO struct {
	prefixes  func() ([]onboardingInterfacePrefix, error)
	neighbors func(onboardingDiscoveryScope) []netip.Addr
	dial      func(context.Context, netip.Addr, netip.Addr) (net.Conn, error)
}

// Only one user-selected network is searched at a time in this process. The
// guard remains held until cancelled workers have really closed their sockets.
var onboardingDiscoveryActive = make(chan struct{}, 1)

// Interface/neighbor APIs have no context parameter. At most one metadata read
// can outlive a cancelled waiter, preventing repeated searches from accumulating
// blocked OS calls. No probe can start from a result delivered after cancellation.
var onboardingMetadataActive = make(chan struct{}, 1)

func onboardingReadMetadata[T any](ctx context.Context, read func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	select {
	case onboardingMetadataActive <- struct{}{}:
	case <-ctx.Done():
		return zero, ctx.Err()
	}
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	go func() {
		defer func() { <-onboardingMetadataActive }()
		value, err := read()
		done <- result{value, err}
	}()
	select {
	case outcome := <-done:
		return outcome.value, outcome.err
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

func discoverOnboardingDevices(ctx context.Context, scopeID string) ([]onboardingDiscoveredDevice, error) {
	return discoverOnboardingDevicesWithSeeds(ctx, scopeID, nil)
}

// Seeds reuse observations from the existing shared mDNS/broker pipeline. This
// selected-scope helper never opens another all-interface mDNS browser and does
// not require the scanner to exist for its SSH-banner fallback.
func discoverOnboardingDevicesWithSeeds(ctx context.Context, scopeID string, seeds []onboardingDiscoveredDevice) ([]onboardingDiscoveredDevice, error) {
	select {
	case onboardingDiscoveryActive <- struct{}{}:
	default:
		return nil, errors.New("another nearby-device search is active or still closing")
	}
	defer func() { <-onboardingDiscoveryActive }()
	return discoverOnboardingWithIO(ctx, scopeID, seeds, onboardingDiscoveryIO{prefixes: onboardingLocalPrefixes, neighbors: onboardingNeighborFacts, dial: func(ctx context.Context, local, address netip.Addr) (net.Conn, error) {
		dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IP(local.AsSlice())}}
		return dialer.DialContext(ctx, "tcp4", net.JoinHostPort(address.String(), "22"))
	}})
}

func discoverOnboardingWithIO(parent context.Context, scopeID string, seeds []onboardingDiscoveredDevice, access onboardingDiscoveryIO) ([]onboardingDiscoveredDevice, error) {
	ctx, cancel := context.WithTimeout(parent, onboardingDiscoveryLimit)
	defer cancel()
	facts, err := onboardingReadMetadata(ctx, access.prefixes)
	if err != nil {
		return nil, err
	}
	scope, err := onboardingSelectedScope(facts, scopeID)
	if err != nil {
		return nil, err
	}
	local, _ := netip.ParseAddr(scope.LocalAddress)
	neighbors, err := onboardingReadMetadata(ctx, func() ([]netip.Addr, error) { return access.neighbors(scope), nil })
	if err != nil {
		return nil, err
	}
	if len(seeds) > 256 {
		return nil, errors.New("mDNS seed inventory exceeds the selected-scope limit")
	}
	locals := []netip.Addr{}
	for _, fact := range facts {
		locals = append(locals, fact.Prefix.Addr())
	}
	addresses := onboardingScopeAddresses(scope, neighbors, seeds, locals...)
	jobs := make(chan netip.Addr, len(addresses))
	for _, address := range addresses {
		jobs <- address
	}
	close(jobs)
	results := make(chan onboardingDiscoveredDevice, len(addresses))
	failures := make(chan error, 1)
	fail := func(err error) {
		select {
		case failures <- err:
		default:
		}
		cancel()
	}
	var workers sync.WaitGroup
	for count := 0; count < onboardingDiscoveryWorkers; count++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for address := range jobs {
				if ctx.Err() != nil {
					return
				}
				current, err := onboardingReadMetadata(ctx, access.prefixes)
				if err != nil {
					fail(err)
					return
				}
				if _, err := onboardingSelectedScope(current, scopeID); err != nil {
					fail(err)
					return
				}
				self := false
				for _, fact := range current {
					if fact.Prefix.Addr() == address {
						self = true
						break
					}
				}
				if self {
					continue
				}
				if ctx.Err() != nil {
					return
				}
				probeCtx, probeCancel := context.WithTimeout(ctx, onboardingDiscoveryProbeLimit)
				connection, err := access.dial(probeCtx, local, address)
				if err == nil {
					if onboardingReadSSHBanner(probeCtx, connection) {
						device := onboardingDiscoveredDevice{
							Address:        address.String(),
							Port:           22,
							Label:          "Nearby SSH device",
							Source:         "ssh-banner",
							BootstrapState: "ssh-ready",
						}
						for _, seed := range seeds {
							if seed.Address == device.Address && seed.Source == "mdns" {
								device.Source = "mdns+ssh-banner"
								device.Label = onboardingDiscoveryLabel(seed.Label)
								break
							}
						}
						results <- device
					}
					_ = connection.Close()
				}
				probeCancel()
			}
		}()
	}
	workers.Wait()
	close(results)
	select {
	case err := <-failures:
		return nil, err
	default:
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	devices := []onboardingDiscoveredDevice{}
	observed := make(map[string]bool)
	for device := range results {
		devices = append(devices, device)
		observed[device.Address] = true
	}
	eligible := make(map[string]bool, len(addresses))
	for _, address := range addresses {
		eligible[address.String()] = true
	}
	for _, seed := range seeds {
		if observed[seed.Address] ||
			!eligible[seed.Address] ||
			seed.Source == "" {
			continue
		}
		devices = append(devices, onboardingDiscoveredDevice{
			Address:        seed.Address,
			Port:           22,
			Label:          onboardingDiscoveryLabel(seed.Label),
			Source:         seed.Source,
			BootstrapState: "bootstrap-required",
		})
		observed[seed.Address] = true
	}
	sort.Slice(devices, func(i, j int) bool {
		return netip.MustParseAddr(devices[i].Address).Less(netip.MustParseAddr(devices[j].Address))
	})
	return devices, nil
}

func onboardingReadSSHBanner(ctx context.Context, connection net.Conn) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return false
	}
	_ = connection.SetReadDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	reader := bufio.NewReaderSize(io.LimitReader(connection, 2048), 256)
	for line := 0; line < 8; line++ {
		text, err := reader.ReadString('\n')
		if err != nil || len(text) > 255 {
			return false
		}
		if strings.HasPrefix(text, "SSH-2.0-") || strings.HasPrefix(text, "SSH-1.99-") {
			if !strings.HasSuffix(text, "\r\n") {
				return false
			}
			identifier := strings.TrimSuffix(text, "\r\n")
			for _, character := range identifier {
				if character < 32 || character > 126 {
					return false
				}
			}
			prefix := "SSH-2.0-"
			if strings.HasPrefix(identifier, "SSH-1.99-") {
				prefix = "SSH-1.99-"
			}
			return len(identifier) > len(prefix) && identifier[len(prefix)] != ' '
		}
	}
	return false
}

func onboardingDiscoveryLabel(label string) string {
	label = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, label))
	if label == "" {
		return "Nearby SSH device"
	}
	runes := []rune(label)
	if len(runes) > 96 {
		label = string(runes[:96])
	}
	return label
}
