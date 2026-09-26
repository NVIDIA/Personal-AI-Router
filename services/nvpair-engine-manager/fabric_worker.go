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
	"path"
	"strings"
	"time"

	"nvpair-shared/clustertrust"
)

const fabricWorkerResultMaxBytes = 128 << 10 // Existing onboardingSSH stdout envelope.

type fabricWorkerRequest struct {
	Protocol                  string       `json:"protocol"`
	Method                    string       `json:"method"`
	OperationID               string       `json:"operationId"`
	Target                    fabricTarget `json:"target"`
	SelectedPortPauseApproved bool         `json:"selectedPortPauseApproved,omitempty"`
}
type fabricWorkerResult struct {
	Protocol         string            `json:"protocol"`
	OperationID      string            `json:"operationId"`
	NodeID           string            `json:"nodeId"`
	Principal        string            `json:"principal"`
	Method           string            `json:"method"`
	Facts            fabricNativeFacts `json:"facts"`
	CleanupConfirmed bool              `json:"cleanupConfirmed"`
	FailureCode      string            `json:"failureCode,omitempty"`
}

func (s *fabricService) inspectWorkerCapability(ctx context.Context, plan cableLaunchPlan) error {
	access, err := s.m.cables.access(plan)
	if err != nil {
		return fabricInspectionError("access-unavailable", err)
	}
	client, err := s.m.onboarding.dial(ctx, plan.Candidate, access)
	if err != nil {
		return fabricInspectionError("ssh-unavailable", err)
	}
	defer client.close()
	worker := path.Join(plan.Receipt.BundlePath, "bin", "nvpair-engine-manager")
	if plan.Runtime != nil {
		worker = plan.Runtime.WorkerPath
	}
	body, err := client.run(ctx, onboardingQuote(worker)+" --fabric-address-capabilities-json", nil)
	var capability struct {
		Protocol      string `json:"protocol"`
		Persistence   string `json:"persistence"`
		MaxInterfaces int    `json:"maxInterfaces"`
	}
	if err != nil {
		return fabricInspectionError("transport-unavailable", err)
	}
	if onboardingDecode(body, &capability) != nil || capability.Protocol != fabricWorkerProtocol || capability.Persistence != "until-reboot" || capability.MaxInterfaces != 2 {
		return fabricInspectionError("worker-changed", errors.New("current fabric worker capability unavailable"))
	}
	return nil
}

// This purpose-specific derivative preserves the existing verified, sealed
// executable launcher. No cable start/grant is used and no UI-supplied argv is
// accepted. The only altered action is the fixed fabric-address worker mode.
func fabricRootLaunchScript() string {
	script := strings.ReplaceAll(cableRootLaunchScript, "PAIR-CABLE-LAUNCH/1 ", "PAIR-FABRIC-LAUNCH/1 ")
	script = strings.ReplaceAll(script, "'nvpair-cable-worker'", "'nvpair-fabric-worker'")
	return strings.ReplaceAll(script, "'--cable-probe-once'", "'--fabric-address-once'")
}

func (s *fabricService) runWorker(ctx context.Context, plan cableLaunchPlan, request fabricWorkerRequest) (fabricWorkerResult, error) {
	var result fabricWorkerResult
	if request.Protocol != fabricWorkerProtocol || request.Target.NodeID != plan.NodeID || request.Target.Principal != plan.Principal || !onboardingID.MatchString(request.OperationID) || len(request.Target.Interfaces) != 2 ||
		(request.Method != "inspect" && request.Method != "apply" && request.Method != "rollback") {
		return result, fabricInspectionError("request-invalid", errors.New("invalid exact fabric worker binding"))
	}
	access, e := s.m.cables.access(plan)
	if e != nil {
		return result, fabricInspectionError("access-unavailable", e)
	}
	limit := 35 * time.Second
	if request.Method == "apply" {
		limit = 135 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	client, e := s.m.onboarding.dial(ctx, plan.Candidate, access)
	if e != nil {
		return result, fabricInspectionError("ssh-unavailable", e)
	}
	defer client.close()
	checked, e := s.m.cables.inspect(ctx, client, plan, s.m.mesh)
	if e != nil {
		return result, fabricInspectionError("worker-changed", e)
	}
	if checked.WorkerSHA256 != plan.WorkerSHA256 || checked.CertificateSHA256 != plan.CertificateSHA256 || checked.WorkerBytes != plan.WorkerBytes {
		return result, fabricInspectionError("worker-changed", errors.New("reviewed fabric worker bytes or identity changed"))
	}
	header := map[string]any{"nodeId": plan.NodeID, "principal": plan.Principal, "uid": plan.Info.UID, "home": plan.Info.Home, "workerSha256": plan.WorkerSHA256, "workerBytes": plan.WorkerBytes, "certificateSha256": plan.CertificateSHA256}
	if plan.Runtime != nil {
		header["runtime"] = cableRuntimeLaunchFields(plan.Runtime)
	} else {
		header["receipt"] = plan.Receipt
	}
	h, _ := json.Marshal(header)
	body, _ := json.Marshal(request)
	if len(h) > 16384 || len(body) > 32768 {
		return result, fabricInspectionError("request-invalid", errors.New("fabric worker request exceeds bound"))
	}
	// Volatile administrator secret is only an input line to the fixed sudo
	// launcher. It never enters the review, argv, output or retained journal.
	input := make([]byte, 0, len(access.elevationPassword)+len(h)+len(body)+32)
	input = append(input, access.elevationPassword...)
	input = append(input, '\n')
	input = append(input, "PAIR-FABRIC-LAUNCH/1 "...)
	input = append(input, h...)
	input = append(input, '\n')
	input = append(input, body...)
	input = append(input, '\n')
	defer clear(input)
	raw, e := client.run(ctx, "/usr/bin/sudo -S -p '' -- /usr/bin/python3 -I -c "+onboardingQuote(fabricRootLaunchScript()), bytes.NewReader(input))
	if e != nil {
		return result, fabricInspectionError("transport-unavailable", e)
	}
	fields, e := decodeFabricWorkerObject(raw, &result)
	if e != nil || result.Protocol != fabricWorkerProtocol || result.OperationID != request.OperationID || result.NodeID != plan.NodeID || result.Principal != plan.Principal || result.Method != request.Method {
		return fabricWorkerResult{}, fabricInspectionError("result-invalid", errors.New("fabric worker result does not match its exact operation"))
	}
	if fields["failureCode"] {
		validCode := request.Method == "inspect" && validFabricFailureCode(result.FailureCode) || request.Method == "apply" && validFabricApplyFailureCode(result.FailureCode)
		if !validCode || result.Facts.Digest != "" || len(result.Facts.Routes) != 0 || len(result.Facts.Blockers) != 0 || len(result.Facts.GeneratedDefaults) != 0 || result.CleanupConfirmed || !fields["facts"] || !fields["cleanupConfirmed"] {
			return fabricWorkerResult{}, fabricInspectionError("result-invalid", errors.New("fabric inspection failure has an invalid result shape"))
		}
		if request.Method == "inspect" {
			return result, fabricInspectionError(result.FailureCode, errors.New("fabric inspection reported failure"))
		}
		return result, nil
	}
	return result, nil
}

// Protocol fields are case-sensitive and duplicate keys are rejected before
// ordinary struct decoding. Null failure metadata cannot become empty success.
func decodeFabricWorkerObject(raw []byte, value any) (map[string]bool, error) {
	invalid := errors.New("invalid bounded fabric worker object")
	if len(raw) == 0 || len(raw) > fabricWorkerResultMaxBytes {
		return nil, invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, invalid
	}
	fields := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		switch name {
		case "protocol", "method", "operationId", "target", "nodeId", "principal", "facts", "cleanupConfirmed", "failureCode", "selectedPortPauseApproved":
		default:
			return nil, invalid
		}
		var part json.RawMessage
		if err != nil || !ok || fields[name] || decoder.Decode(&part) != nil || bytes.Equal(bytes.TrimSpace(part), []byte("null")) {
			return nil, invalid
		}
		fields[name] = true
	}
	last, err := decoder.Token()
	// onboardingDecode preserves the existing 64-KiB accepted-result limit;
	// the SSH transport/output envelope is separately 128 KiB.
	if err != nil || last != json.Delim('}') || !fields["protocol"] || onboardingDecode(raw, value) != nil {
		return nil, invalid
	}
	return fields, nil
}

// Preserve the existing fabric success envelope and complete-write checks.
// Successful delivery uses the passed worker context, not a new shorter timer.
func writeFabricWorkerResult(ctx context.Context, output io.Writer, result fabricWorkerResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(result)
	if err != nil || len(data)+1 > fabricWorkerResultMaxBytes {
		return errors.New("fabric worker output bound exceeded")
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
	select {
	case err := <-finished:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Narrow function seams keep worker admission and result handling testable
// without executing native queries, address operations or a listening service.
type fabricWorkerIO struct {
	identity  func(context.Context) (string, error)
	inventory func(context.Context) (fabricInventory, error)
	inspect   func(context.Context, []fabricInterface, bool) (fabricNativeFacts, error)
	add       func(context.Context, fabricInterface, string, int, bool) error
	remove    func(context.Context, fabricInterface, string) error
}

func runFabricAddressOnce(ctx context.Context, input io.Reader, output io.Writer, nodeID string, nodeInfoPort int, profile string) error {
	var mesh *clustertrust.Mesh
	local := newCableLocalFacts(nodeID, nodeInfoPort)
	defer local.http.CloseIdleConnections()
	return runFabricAddressWorker(ctx, input, output, nodeID, fabricWorkerIO{
		identity: func(checkCtx context.Context) (string, error) {
			if err := checkCtx.Err(); err != nil {
				return "", err
			}
			mesh = clustertrust.Open(profile)
			mesh.Refresh()
			principal := mesh.NodeUUID()
			if !mesh.Clustered() || !cableIdentifier(principal, 256) || !mesh.HasPin(principal) {
				return "", errors.New("paired local fabric identity unavailable")
			}
			return principal, nil
		},
		inventory: func(checkCtx context.Context) (fabricInventory, error) {
			return readFabricInventory(checkCtx, local, mesh)
		},
		inspect: fabricNativeInspectRebind,
		add:     fabricNativeAdd,
		remove:  fabricNativeRemove,
	})
}

func runFabricAddressWorker(ctx context.Context, input io.Reader, output io.Writer, nodeID string, workerIO fabricWorkerIO) error {
	// Use the existing finite-worker line protocol: stdin need not remain open
	// or supply EOF to start this one action, and an absent request expires.
	lines := make(chan []byte, 1)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 4096), 32<<10)
		if scanner.Scan() {
			lines <- bytes.Clone(scanner.Bytes())
		}
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var raw []byte
	select {
	case line, ok := <-lines:
		if !ok {
			return errors.New("fabric worker request unavailable")
		}
		raw = line
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errors.New("fabric worker input deadline expired")
	}
	var request fabricWorkerRequest
	_, decodeErr := decodeFabricWorkerObject(raw, &request)
	if len(raw) > 32<<10 || decodeErr != nil || request.Protocol != fabricWorkerProtocol || !onboardingID.MatchString(request.OperationID) || !cableIdentifier(nodeID, 128) || request.Target.NodeID != nodeID || !cableIdentifier(request.Target.Principal, 256) || len(request.Target.Interfaces) != 2 {
		return errors.New("invalid bounded fabric worker request")
	}
	if request.Method != "inspect" && request.Method != "apply" && request.Method != "rollback" {
		return errors.New("unsupported fabric worker action")
	}
	for _, iface := range request.Target.Interfaces {
		if iface.GeneratedDefault != nil && !validFabricGeneratedDefault(iface.GeneratedDefault, iface) {
			return errors.New("invalid reviewed generated-default binding")
		}
		if request.Method == "apply" && ((iface.GeneratedDefault != nil && !request.SelectedPortPauseApproved) || (len(iface.Addresses) != 0 && iface.GeneratedDefault == nil)) {
			return errors.New("exact generated-default inspection and selected-port pause consent required")
		}
	}
	limit := 25 * time.Second
	if request.Method == "apply" {
		limit = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	principal, e := workerIO.identity(ctx)
	if e != nil || principal != request.Target.Principal {
		// An unbound identity cannot echo the caller's operation or principal.
		return errors.New("paired local fabric identity changed")
	}
	result := fabricWorkerResult{Protocol: fabricWorkerProtocol, OperationID: request.OperationID, NodeID: nodeID, Principal: principal, Method: request.Method, Facts: fabricNativeFacts{Routes: []string{}, Blockers: []string{}}}
	inspectionFailure := func(code string, cause error) error {
		if request.Method == "apply" {
			var failure *fabricInspectError
			if errors.As(fabricInspectionError(code, cause), &failure) && validFabricApplyFailureCode(failure.Code) {
				result.FailureCode = failure.Code
				result.Facts = fabricNativeFacts{}
				result.CleanupConfirmed = false
				return writeCableProbeMessage(ctx, output, result)
			}
		}
		if request.Method != "inspect" {
			return errors.New("fabric native identity, configuration or address operation failed; reconcile owned cleanup")
		}
		var failure *fabricInspectError
		if !errors.As(fabricInspectionError(code, cause), &failure) {
			return errors.New("fabric inspection failure unavailable")
		}
		result.FailureCode = failure.Code
		result.Facts = fabricNativeFacts{} // Partial inventory is never usable evidence.
		result.CleanupConfirmed = false
		return writeCableProbeMessage(ctx, output, result)
	}
	facts, e := workerIO.inventory(ctx)
	if e != nil {
		return inspectionFailure("inventory-unavailable", e)
	}
	if facts.NodeID != nodeID || facts.Principal != principal {
		return inspectionFailure("identity-mismatch", errors.New("paired local fabric identity changed"))
	}
	if request.Method != "rollback" && !fabricSameTarget(request.Target, facts) {
		return inspectionFailure("facts-changed", errors.New("reviewed fabric interface facts changed before execution"))
	}
	if request.Method == "inspect" {
		result.Facts, e = workerIO.inspect(ctx, request.Target.Interfaces, request.SelectedPortPauseApproved)
	} else if request.Method == "apply" {
		for _, iface := range request.Target.Interfaces {
			if e = workerIO.add(ctx, iface, request.OperationID, fabricLeaseSeconds, request.SelectedPortPauseApproved); e != nil {
				break
			}
		}
	} else {
		result.CleanupConfirmed = true
		for i := len(request.Target.Interfaces) - 1; i >= 0; i-- {
			if err := workerIO.remove(ctx, request.Target.Interfaces[i], request.OperationID); err != nil {
				result.CleanupConfirmed = false
			}
		}
	}
	if e != nil {
		return inspectionFailure("native-inspect-failed", e)
	}
	return writeFabricWorkerResult(ctx, output, result)
}
