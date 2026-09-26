// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"nvpair-shared/cableprobe"
	"nvpair-shared/clustertrust"
)

// This process mode is an internal, finite worker for an explicitly approved
// privileged launcher. It never elevates itself or accepts packet/argv overrides.
// Ordinary Engine Manager RPCs cannot enter it and retain unavailable eligibility.
type cableProbeOnceRequest struct {
	Protocol string            `json:"protocol"`
	Review   cableprobe.Review `json:"review"`
	RunID    string            `json:"runId"`
	Marker   string            `json:"marker"`
	Peers    []struct {
		NodeID      string `json:"nodeId"`
		Principal   string `json:"principal"`
		Address     string `json:"address"`
		ControlPort int    `json:"controlPort"`
	} `json:"peers"`
}

type cableProbeCommand struct {
	RunID   string `json:"runId"`
	Command string `json:"command"`
}

func decodeCableProbeLine(data []byte, value any) error {
	if len(data) == 0 || len(data) > 32<<10 {
		return errors.New("invalid cable worker request size")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return errors.New("invalid cable worker request")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("one cable worker object per line is required")
	}
	return nil
}

func bindCableProbeRequest(m *Manager, request cableProbeOnceRequest) (cableprobe.ReviewRequest, error) {
	invalid := errors.New("cable worker requires the exact reviewed paired devices and native ports")
	if request.Protocol != cableWorkerProtocol || m.cableLocal == nil || request.Review.OwnerNodeID != m.cableLocal.nodeID ||
		!cableIdentifier(request.Review.ReviewID, 128) || !cableIdentifier(request.RunID, 128) || request.Review.ConsumedRunID != "" ||
		request.Review.Available || request.Review.RemainingMs <= 0 || request.Review.RemainingMs > 30000 || len(request.Peers) > 2 {
		return cableprobe.ReviewRequest{}, invalid
	}
	if _, err := cableprobe.EncodeFrame("02:00:00:00:00:01", request.Marker, 1); err != nil {
		return cableprobe.ReviewRequest{}, invalid
	}
	selection := cableprobe.ReviewRequest{}
	for _, target := range request.Review.Targets {
		selection.NodeIDs = append(selection.NodeIDs, target.NodeID)
		for _, port := range target.Ports {
			selection.Ports = append(selection.Ports, cableprobe.PortRef{NodeID: target.NodeID, SwitchID: port.SwitchID, PortName: port.PortName})
		}
	}
	if !validateCableSelection(selection) || len(request.Peers) != len(selection.NodeIDs)-1 {
		return cableprobe.ReviewRequest{}, invalid
	}
	seen := map[string]bool{}
	for _, peer := range request.Peers {
		ip := net.ParseIP(peer.Address)
		if !cableIdentifier(peer.NodeID, 128) || !cableIdentifier(peer.Principal, 256) || peer.NodeID == m.cableLocal.nodeID || seen[peer.NodeID] ||
			ip == nil || ip.IsUnspecified() || ip.IsMulticast() || peer.ControlPort < 1 || peer.ControlPort > 65535 {
			return cableprobe.ReviewRequest{}, invalid
		}
		matched := false
		for _, target := range request.Review.Targets {
			if target.NodeID == peer.NodeID && target.Principal == peer.Principal {
				matched = true
			}
		}
		if !matched {
			return cableprobe.ReviewRequest{}, invalid
		}
		seen[peer.NodeID] = true
		// Same trusted-parent directory input used by the normal stdio worker;
		// mTLS plus returned host/principal checks, not this tuple, authenticate it.
		m.peers.peers[peer.NodeID] = ecPeer{nodeID: peer.NodeID, clusterUUID: peer.Principal, addresses: []string{peer.Address}, port: peer.ControlPort}
	}
	return selection, nil
}

func cableProbeTargetFacts(targets []cableprobe.Target) []cableprobe.Target {
	result := make([]cableprobe.Target, len(targets))
	for i, target := range targets {
		result[i] = cableprobe.Target{NodeID: target.NodeID, Principal: target.Principal, Ports: target.Ports}
	}
	return result
}

func (m *Manager) prepareCableProbeOnce(ctx context.Context, request cableProbeOnceRequest, probeIO cableProbeIO) (_ *cableProbeSession, resultErr error) {
	_, err := bindCableProbeRequest(m, request)
	if err != nil {
		return nil, err
	}
	// Only this successful local request binding permits a preparation receipt.
	defer func() {
		if resultErr != nil {
			resultErr = boundCablePreparationError(resultErr)
		}
	}()
	began := time.Now()
	admitCtx, cancel := context.WithTimeout(ctx, min(5*time.Second, time.Duration(request.Review.RemainingMs)*time.Millisecond))
	defer cancel()
	m.mesh.Refresh()
	ownerPrincipal := m.mesh.NodeUUID()
	bindings := map[string]*http.Client{}
	for _, target := range request.Review.Targets {
		client, ok := m.remoteHTTP.Client(target.Principal)
		if !ok || !m.mesh.Clustered() {
			return nil, newCablePreparationError("prepare-pairing-unavailable", "not-attempted", errors.New("reviewed participant is no longer paired"))
		}
		bindings[target.Principal] = client
	}
	// The authenticated controller supplies the peer mapping. Every participant
	// validates its own selected native ports; cached global inventories are not
	// an additional gate on a bounded layer-2 observation.
	if probeIO.validate == nil {
		return nil, newCablePreparationError("prepare-current-invalid", "not-attempted", errors.New("native local interface validation unavailable"))
	}
	var local cableprobe.Target
	for _, target := range request.Review.Targets {
		if target.NodeID == m.cableLocal.nodeID {
			local = target
		}
	}
	if local.Principal != ownerPrincipal {
		return nil, newCablePreparationError("prepare-identity-mismatch", "not-attempted", errors.New("local paired identity is outside the reviewed selection"))
	}
	current := func(checkCtx context.Context) error {
		if err := checkCtx.Err(); err != nil {
			return err
		}
		m.mesh.Refresh()
		if !m.mesh.Clustered() || m.mesh.NodeUUID() != ownerPrincipal {
			return errors.New("cable owner membership changed")
		}
		for principal, bound := range bindings {
			client, ok := m.remoteHTTP.Client(principal)
			if !ok || client != bound {
				return errors.New("cable participant certificate changed")
			}
		}
		for _, port := range local.Ports {
			ref := cableprobe.PortRef{NodeID: local.NodeID, SwitchID: port.SwitchID, PortName: port.PortName}
			for _, alias := range port.Interfaces {
				if err := checkCtx.Err(); err != nil {
					return err
				}
				if err := probeIO.validate(alias, ref); err != nil {
					return errors.New("reviewed local physical ports changed")
				}
			}
		}
		// Native validation may have crossed a certificate replacement.
		m.mesh.Refresh()
		for principal, bound := range bindings {
			client, ok := m.remoteHTTP.Client(principal)
			if !ok || client != bound {
				return errors.New("cable participant certificate changed during facts read")
			}
		}
		return checkCtx.Err()
	}
	if time.Since(began) >= time.Duration(request.Review.RemainingMs)*time.Millisecond {
		return nil, newCablePreparationError("prepare-admission-expired", "not-attempted", errors.New("cable review expired during preparation"))
	}
	return prepareCableProbe(admitCtx, true, local.NodeID, request.Marker, request.Review.Targets, probeIO, current)
}

// No service/listener/engine startup occurs on this branch. The launcher keeps
// stdin open, waits for armed on every participant, then sends a matching start.
// EOF, cancel, a five-second arm wait or the 35-second process budget releases it.
func runCableProbeOnce(ctx context.Context, input io.Reader, output io.Writer, nodeID string, nodeInfoPort int, clusterDir string) error {
	if clusterDir == "" {
		return errors.New("an explicit existing cluster identity directory is required")
	}
	m := newManagerTransport(nil, nil, clustertrust.Open(clusterDir))
	m.cableLocal = newCableLocalFacts(nodeID, nodeInfoPort)
	defer m.remoteHTTP.CloseIdle()
	defer m.readyHTTP.CloseIdle()
	defer m.cableLocal.http.CloseIdleConnections()
	return runCableProbeWorker(ctx, input, output, func(prepareCtx context.Context, request cableProbeOnceRequest) (*cableProbeSession, error) {
		return m.prepareCableProbeOnce(prepareCtx, request, nativeCableProbeIO())
	})
}

func writeCableProbeMessage(ctx context.Context, output io.Writer, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil || len(data) > 16<<10 {
		return errors.New("cable worker output bound exceeded")
	}
	data = append(data, '\n')
	finished := make(chan error, 1)
	go func() {
		n, err := output.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
		finished <- err
	}()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case err := <-finished:
		return err
	case <-timer.C:
		return errors.New("cable worker output deadline expired")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func runCableProbeWorker(ctx context.Context, input io.Reader, output io.Writer, prepare func(context.Context, cableProbeOnceRequest) (*cableProbeSession, error)) error {
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 4096), 32<<10)
		for count := 0; count < 3 && scanner.Scan(); count++ {
			line := bytes.Clone(scanner.Bytes())
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
	}()
	readLine := func(readCtx context.Context) ([]byte, error) {
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case line, ok := <-lines:
			if !ok {
				return nil, errors.New("cable worker owner disconnected")
			}
			return line, nil
		case <-timer.C:
			return nil, errors.New("cable worker input deadline expired")
		case <-readCtx.Done():
			return nil, readCtx.Err()
		}
	}
	line, err := readLine(ctx)
	if err != nil {
		return err
	}
	var request cableProbeOnceRequest
	if err := decodeCableProbeLine(line, &request); err != nil {
		return err
	}
	if request.Protocol != cableWorkerProtocol || !cableIdentifier(request.RunID, 128) || !cableIdentifier(request.Review.ReviewID, 128) {
		return errors.New("cable worker request protocol or identity is invalid")
	}
	session, err := prepare(ctx, request)
	if err != nil {
		var failure *cablePreparationError
		if errors.As(err, &failure) && failure.requestBound && validCablePrearmCode(failure.Code) && validCablePrearmResourceOutcome(failure.ResourceOutcome) {
			if failure.FactsDifference != nil && (failure.Code != "prepare-facts-mismatch" || failure.ResourceOutcome != "not-attempted" || !validCableFactsDifference(failure.FactsDifference, request.Review.Targets)) {
				return err
			}
			return writeCableProbeMessage(ctx, output, cablePrearmMessage{
				State: "prearm-failed", RunID: request.RunID, ReviewID: request.Review.ReviewID,
				Code: failure.Code, ResourceOutcome: failure.ResourceOutcome,
				FactsDifference: cloneCableFactsDifference(failure.FactsDifference),
			})
		}
		return err
	}
	defer session.Close()
	// Advertise exactly the same lease that bounds output, start waiting and
	// prepared descriptor ownership, rather than a longer review lifetime.
	session.limitAdmission(time.Now().Add(5 * time.Second))
	if deadline, ok := ctx.Deadline(); ok {
		// READY must leave the complete observation and its existing final
		// validation/output budgets available. No start can extend this process.
		session.limitAdmission(deadline.Add(-cableprobe.Window - 4*time.Second))
	}
	if !time.Now().Before(session.admitUntil) {
		clean := session.Close() == nil
		outcome := "rollback-confirmed"
		if !clean {
			outcome = "rollback-unconfirmed"
		}
		return writeCableProbeMessage(ctx, output, cablePrearmMessage{State: "prearm-failed", RunID: request.RunID, ReviewID: request.Review.ReviewID, Code: "prepare-admission-expired", ResourceOutcome: outcome})
	}
	armCtx, armCancel := context.WithDeadline(ctx, session.admitUntil)
	defer armCancel()
	if err := writeCableProbeMessage(armCtx, output, map[string]any{"state": "armed", "runId": request.RunID, "reviewId": request.Review.ReviewID, "remainingMs": max(0, time.Until(session.admitUntil).Milliseconds())}); err != nil {
		return err
	}
	line, err = readLine(armCtx)
	if err != nil {
		return err
	}
	if err = armCtx.Err(); err != nil {
		return err
	}
	var command cableProbeCommand
	if decodeCableProbeLine(line, &command) != nil || command.RunID != request.RunID || (command.Command != "start" && command.Command != "cancel") {
		return errors.New("cable worker did not receive its matching start")
	}
	if command.Command == "cancel" {
		clean := session.Close() == nil
		result := cableProbeResult{State: "cancelled", Directness: "unverified", Observations: []cableProbeObservation{}, CleanupConfirmed: clean, Message: "Prepared cable worker cancelled before transmission."}
		if !clean {
			result.State = "failed"
			result.Message = "Prepared cable worker cleanup is not confirmed."
		}
		return writeCableProbeMessage(ctx, output, struct {
			RunID    string `json:"runId"`
			ReviewID string `json:"reviewId"`
			cableProbeResult
		}{request.RunID, request.Review.ReviewID, result})
	}
	armCancel()
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		select {
		case <-lines:
			// The only remaining command is cancellation. Malformed/extra input
			// and EOF also fail closed; none can extend or restart this worker.
			stop()
		case <-runCtx.Done():
		}
	}()
	result := session.Run(runCtx)
	return writeCableProbeMessage(ctx, output, struct {
		RunID    string `json:"runId"`
		ReviewID string `json:"reviewId"`
		cableProbeResult
	}{request.RunID, request.Review.ReviewID, result})
}
