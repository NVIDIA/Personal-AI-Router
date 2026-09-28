// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"nvpair-shared/cableprobe"
)

const cableProbeReceiveLimit = 256
const cableProbeSendInterval = time.Second
const cableProbePollInterval = 50 * time.Millisecond

var errCableProbePermission = errors.New("raw cable capability unavailable; an approved one-shot privileged launch is required")
var errCableProbeIdle = errors.New("no cable frame ready")

type cableProbePacket struct {
	Data       []byte
	Index      int
	Kind       uint8
	ReceivedAt time.Time // Actual kernel timestamp, never a cached LLDP timestamp.
	Truncated  bool
}

// Native calls are a narrow I/O boundary so cleanup and bounds run in socket-free
// tests on Windows. Only the Linux implementation can acquire raw descriptors.
type cableProbeIO struct {
	validate  func(alias cableprobe.Interface, port cableprobe.PortRef) error
	lock      func() (int, error)
	open      func() (int, error)
	configure func(fd int, alias cableprobe.Interface, port cableprobe.PortRef) error
	read      func(fds []int, wait time.Duration) (cableProbePacket, error)
	write     func(fd int, data []byte) error
	close     func(fd int) error
}

type cableProbeObservation struct {
	Local    cableprobe.PortRef `json:"local"`
	Peer     cableprobe.PortRef `json:"peer"`
	Sequence uint32             `json:"sequence"`
	AgeMs    int64              `json:"ageMs"`
}

type cableProbeResult struct {
	State               string                  `json:"state"`
	Directness          string                  `json:"directness"`
	Observations        []cableProbeObservation `json:"observations"`
	Sent                int                     `json:"sent"`
	Received            int                     `json:"received"`
	CleanupConfirmed    bool                    `json:"cleanupConfirmed"`
	Message             string                  `json:"message"`
	FinalValidationCode string                  `json:"finalValidationCode,omitempty"`
}

// Finalization can make a result fail without replacing its first failure.
// Cleanup and final validation retain their separate outcomes.
func (r *cableProbeResult) fail(message string) {
	if r.State != "failed" {
		r.Message = message
	}
	r.State = "failed"
}

type cableProbeSession struct {
	io      cableProbeIO
	local   string
	marker  string
	targets []cableprobe.Target
	fds     []int
	senders []struct {
		fd  int
		mac string
	}
	lockFD     int
	current    func(context.Context) error
	closeOnce  sync.Once
	closeErr   error
	mu         sync.Mutex
	started    bool
	closed     bool
	finished   bool
	cancel     context.CancelFunc
	done       chan struct{}
	admitUntil time.Time
	expiry     *time.Timer
}

// Preparation reserves the node and arms only reviewed native interfaces. It
// sends nothing. The privileged-launch decision cannot arrive in a wire payload.
func prepareCableProbe(ctx context.Context, approvedLaunch bool, local, marker string, targets []cableprobe.Target, io cableProbeIO, current func(context.Context) error) (*cableProbeSession, error) {
	if !approvedLaunch {
		return nil, errCableProbePermission
	}
	if len(targets) < 2 || len(targets) > 3 || current == nil {
		return nil, newCablePreparationError("prepare-scope-invalid", "not-attempted", errors.New("incomplete reviewed cable participants"))
	}
	if _, err := cableprobe.EncodeFrame("02:00:00:00:00:01", marker, 1); err != nil {
		return nil, newCablePreparationError("prepare-scope-invalid", "not-attempted", err)
	}
	var localPorts []cableprobe.Port
	nodes, principals := map[string]bool{}, map[string]bool{}
	cloned := make([]cableprobe.Target, len(targets))
	for i, target := range targets {
		if !cableIdentifier(target.NodeID, 128) || !cableIdentifier(target.Principal, 256) || nodes[target.NodeID] || principals[target.Principal] || len(target.Ports) < 1 || len(target.Ports) > 2 {
			return nil, newCablePreparationError("prepare-scope-invalid", "not-attempted", errors.New("invalid reviewed cable identity or port count"))
		}
		nodes[target.NodeID], principals[target.Principal] = true, true
		cloned[i] = target
		cloned[i].Ports = make([]cableprobe.Port, len(target.Ports))
		indexes, names, ports := map[int]bool{}, map[string]bool{}, map[[2]string]bool{}
		for j, port := range target.Ports {
			key := [2]string{port.SwitchID, port.PortName}
			if !cableIdentifier(port.SwitchID, 128) || !cableIdentifier(port.PortName, 128) || ports[key] || len(port.Interfaces) < 1 || len(port.Interfaces) > 4 {
				return nil, newCablePreparationError("prepare-scope-invalid", "not-attempted", errors.New("invalid reviewed native port"))
			}
			ports[key] = true
			cloned[i].Ports[j] = port
			cloned[i].Ports[j].Interfaces = append([]cableprobe.Interface(nil), port.Interfaces...)
			for _, alias := range port.Interfaces {
				if !cableInterfaceValid(alias) || indexes[alias.Index] || names[alias.Name] {
					return nil, newCablePreparationError("prepare-scope-invalid", "not-attempted", errors.New("ambiguous reviewed native alias"))
				}
				indexes[alias.Index], names[alias.Name] = true, true
			}
		}
		if target.NodeID == local {
			localPorts = cloned[i].Ports
		}
	}
	if len(localPorts) == 0 {
		return nil, newCablePreparationError("prepare-scope-invalid", "not-attempted", errors.New("local node is outside the reviewed selection"))
	}
	if err := ctx.Err(); err != nil {
		return nil, newCablePreparationError("prepare-admission-expired", "not-attempted", err)
	}
	if err := current(ctx); err != nil {
		return nil, newCablePreparationError("prepare-current-invalid", "not-attempted", err)
	}
	s := &cableProbeSession{io: io, local: local, marker: marker, targets: cloned, lockFD: -1, current: current, done: make(chan struct{})}
	s.admitUntil = time.Now().Add(cableprobe.ReviewLifetime)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(s.admitUntil) {
		s.admitUntil = deadline
	}
	lockFD, err := io.lock()
	if err != nil {
		// lock may have opened a descriptor before its internal failure/close.
		return nil, newCablePreparationError("prepare-reservation-failed", "unknown", err)
	}
	s.lockFD = lockFD
	failureCode := "prepare-unknown"
	for _, port := range localPorts {
		for i, alias := range port.Interfaces {
			if err = ctx.Err(); err != nil {
				failureCode = "prepare-admission-expired"
				break
			}
			var fd int
			fd, err = io.open()
			if err != nil {
				failureCode = "prepare-open-failed"
				break
			}
			s.fds = append(s.fds, fd)
			if err = io.configure(fd, alias, cableprobe.PortRef{NodeID: local, SwitchID: port.SwitchID, PortName: port.PortName}); err != nil {
				failureCode = "prepare-configure-failed"
				break
			}
			// Receive on every reviewed alias; transmit once per physical port.
			if i == 0 {
				s.senders = append(s.senders, struct {
					fd  int
					mac string
				}{fd, alias.MAC})
			}
		}
		if err != nil {
			break
		}
	}
	if err == nil {
		err = ctx.Err()
		failureCode = "prepare-admission-expired"
	}
	if err == nil {
		err = current(ctx)
		failureCode = "prepare-current-invalid"
	}
	if err != nil {
		// Classify the first cause before rollback; cleanup cannot replace it.
		failure := newCablePreparationError(failureCode, "rollback-confirmed", err)
		if s.Close() != nil {
			failure.ResourceOutcome = "rollback-unconfirmed"
		}
		return nil, failure
	}
	s.expiry = time.AfterFunc(max(0, time.Until(s.admitUntil)), func() {
		s.mu.Lock()
		expired := !s.started
		if expired {
			s.closed = true
		}
		s.mu.Unlock()
		if expired {
			_ = s.release()
		}
	})
	return s, nil
}

func (s *cableProbeSession) Close() error {
	s.mu.Lock()
	s.closed = true
	if s.expiry != nil {
		s.expiry.Stop()
	}
	cancel, running := s.cancel, s.started && !s.finished
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if running {
		<-s.done
	}
	return s.release()
}

func (s *cableProbeSession) limitAdmission(deadline time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started && !s.closed && deadline.Before(s.admitUntil) {
		s.admitUntil = deadline
		if s.expiry != nil {
			s.expiry.Reset(max(0, time.Until(deadline)))
		}
	}
}

func (s *cableProbeSession) release() error {
	s.closeOnce.Do(func() {
		for _, fd := range s.fds {
			s.closeErr = errors.Join(s.closeErr, s.io.close(fd))
		}
		if s.lockFD >= 0 {
			s.closeErr = errors.Join(s.closeErr, s.io.close(s.lockFD))
		}
	})
	return s.closeErr
}

// Run is deliberately local evidence only. Reciprocal/coordinator verdicts must
// be assembled through authenticated participants; this adapter cannot infer them.
func (s *cableProbeSession) Run(ctx context.Context) (result cableProbeResult) {
	result = cableProbeResult{State: "failed", Directness: "unverified", Observations: []cableProbeObservation{}, Message: "Cable observation did not start."}
	s.mu.Lock()
	if s.started || s.closed {
		preparedClosed := s.closed && !s.started
		s.mu.Unlock()
		result.Message = "Cable session is already used or closed."
		if preparedClosed {
			result.CleanupConfirmed = s.release() == nil
		}
		return
	}
	if !time.Now().Before(s.admitUntil) {
		s.mu.Unlock()
		result.Message = "Cable admission expired before start."
		result.CleanupConfirmed = s.Close() == nil
		return
	}
	s.started = true
	if s.expiry != nil {
		s.expiry.Stop()
	}
	started := time.Now()
	ctx, cancel := context.WithDeadline(ctx, started.Add(cableprobe.Window))
	s.cancel = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		result.CleanupConfirmed = s.release() == nil
		if !result.CleanupConfirmed {
			result.fail("Cable descriptor cleanup is not confirmed.")
		}
		s.mu.Lock()
		s.finished = true
		close(s.done)
		s.mu.Unlock()
	}()
	nextSend := started
	sequence := uint32(1)
	type seenKey struct {
		local, peer cableprobe.PortRef
		sequence    uint32
	}
	seen := map[seenKey]bool{}
	type observed struct {
		value    cableProbeObservation
		received time.Time
	}
	latest := map[[2]cableprobe.PortRef]observed{}
	for {
		if time.Since(started) >= cableprobe.Window {
			result.State, result.Message = "completed", "Finite local cable observation ended; reciprocity and directness remain unverified."
			break
		}
		if result.Received >= cableProbeReceiveLimit {
			result.Message = "Cable receive budget exhausted."
			break
		}
		if err := ctx.Err(); err != nil {
			result.State, result.Message = "cancelled", "Cable observation cancelled."
			break
		}
		if err := s.current(ctx); err != nil {
			if time.Since(started) >= cableprobe.Window && errors.Is(err, context.DeadlineExceeded) {
				result.State, result.Message = "completed", "Finite local cable observation ended."
			} else if errors.Is(err, context.Canceled) {
				result.State, result.Message = "cancelled", "Cable observation cancelled."
			} else if errors.Is(err, errCableFactsStale) {
				result.Message = "Reviewed native port observations remained stale."
			} else {
				result.Message = "Reviewed identity or native ports changed during the cable observation."
			}
			break
		}
		if !time.Now().Before(nextSend) && sequence <= 20 {
			for _, sender := range s.senders {
				if ctx.Err() != nil || time.Since(started) >= cableprobe.Window {
					break
				}
				frame, err := cableprobe.EncodeFrame(sender.mac, s.marker, sequence)
				if err != nil || s.io.write(sender.fd, frame) != nil {
					result.Message = "Fixed-profile cable transmission failed."
					return
				}
				result.Sent++
			}
			sequence++
			nextSend = time.Now().Add(cableProbeSendInterval)
		}
		packet, err := s.io.read(s.fds, min(cableProbePollInterval, max(0, cableprobe.Window-time.Since(started))))
		if errors.Is(err, errCableProbeIdle) {
			continue
		}
		if err != nil {
			result.Message = "Cable receive failed."
			break
		}
		result.Received++
		now := time.Now()
		age := now.Sub(packet.ReceivedAt)
		if packet.Truncated || packet.ReceivedAt.Before(started) || age < 0 || age >= cableprobe.Freshness {
			continue
		}
		frame, err := cableprobe.DecodeFrame(packet.Data)
		if err != nil {
			continue
		}
		match, err := cableprobe.ResolveIngress(frame, s.marker, s.local, packet.Index, packet.Kind, s.targets)
		if err != nil {
			continue
		}
		key := seenKey{match.Local, match.Peer, match.Sequence}
		if seen[key] {
			continue
		}
		seen[key] = true
		edge := [2]cableprobe.PortRef{match.Local, match.Peer}
		if _, exists := latest[edge]; !exists && len(latest) == 6 {
			result.Message = "Cable observation is ambiguous within its edge bound."
			break
		}
		// Project wall-clock kernel age onto a monotonic receipt clock.
		latest[edge] = observed{cableProbeObservation{Local: match.Local, Peer: match.Peer, Sequence: match.Sequence}, now.Add(-age)}
	}
	// Close before final trust revalidation so a slow owner cannot retain raw FDs.
	if s.release() != nil {
		result.fail("Cable descriptor cleanup is not confirmed.")
	}
	finalCtx, finalCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer finalCancel()
	if err := s.current(finalCtx); err != nil {
		latest = nil
		result.FinalValidationCode = "final-revalidation-failed"
		message := "Final reviewed identity or ports changed; observations discarded."
		if errors.Is(err, errCableFactsStale) {
			result.FinalValidationCode = "facts-stale"
			message = "Final native port observations remained stale; observations discarded."
		}
		result.fail(message)
	}
	for _, item := range latest {
		item.value.AgeMs = max(0, time.Since(item.received).Milliseconds())
		result.Observations = append(result.Observations, item.value)
	}
	sort.Slice(result.Observations, func(i, j int) bool {
		a, b := result.Observations[i], result.Observations[j]
		return fmt.Sprint(a.Local, a.Peer) < fmt.Sprint(b.Local, b.Peer)
	})
	return
}
