// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"nvpair-shared/cableprobe"
)

// Version 3 keeps the eight mandatory, non-null fields and permits one bounded
// facts difference. Detect duplicate keys before struct decoding.
func decodeCablePrearmLine(line []byte, runID, reviewID string, approved ...[]cableprobe.Target) (cableWorkerMessage, error) {
	invalid := cableWorkerOutputError("cable worker returned an invalid bounded protocol message")
	decoder := json.NewDecoder(bytes.NewReader(line))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return cableWorkerMessage{}, invalid
	}
	seen := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		switch name {
		case "state", "runId", "reviewId", "code", "resourceOutcome", "cleanupConfirmed", "sent", "received", "factsDifference":
		default:
			return cableWorkerMessage{}, invalid
		}
		var value json.RawMessage
		if err != nil || !ok || seen[name] || decoder.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return cableWorkerMessage{}, invalid
		}
		seen[name] = true
		if name == "factsDifference" && !validCableFactsDifferenceJSON(value) {
			return cableWorkerMessage{}, invalid
		}
	}
	end, err := decoder.Token()
	var extra any
	wantFields := 8
	if seen["factsDifference"] {
		wantFields++
	}
	if err != nil || end != json.Delim('}') || len(seen) != wantFields || !errors.Is(decoder.Decode(&extra), io.EOF) {
		return cableWorkerMessage{}, invalid
	}
	var terminal cablePrearmMessage
	if decodeCableProbeLine(line, &terminal) != nil {
		return cableWorkerMessage{}, invalid
	}
	message := cableWorkerMessage{RunID: terminal.RunID, ReviewID: terminal.ReviewID, Prearm: &terminal,
		cableProbeResult: cableProbeResult{State: "prearm-failed"}}
	if !validCablePrearmWorkerMessage(message, runID, reviewID, approved...) {
		return cableWorkerMessage{}, invalid
	}
	return message, nil
}

func validCablePrearmWorkerMessage(message cableWorkerMessage, runID, reviewID string, approved ...[]cableprobe.Target) bool {
	p := message.Prearm
	if p != nil && p.FactsDifference != nil && (p.Code != "prepare-facts-mismatch" || p.ResourceOutcome != "not-attempted" ||
		len(approved) != 1 || !validCableFactsDifference(p.FactsDifference, approved[0])) {
		return false
	}
	return p != nil && message.State == "prearm-failed" && p.State == "prearm-failed" &&
		message.RunID == runID && message.ReviewID == reviewID && p.RunID == runID && p.ReviewID == reviewID &&
		validCablePrearmCode(p.Code) && validCablePrearmResourceOutcome(p.ResourceOutcome) &&
		!p.CleanupConfirmed && p.Sent == 0 && p.Received == 0 && !message.CleanupConfirmed &&
		message.Sent == 0 && message.Received == 0 && message.RemainingMs == 0 && message.FinalValidationCode == "" &&
		message.Message == "" && message.Directness == "" && len(message.Observations) == 0
}

func captureCablePrearm(row *cableprobe.ParticipantDiagnostic, message cableWorkerMessage) {
	row.WorkerState = "failed"
	sent, received := 0, 0
	row.Sent, row.Received = &sent, &received
	row.CleanupConfirmed = false
	row.PreparationResourceOutcome = message.Prearm.ResourceOutcome
	row.FactsDifference = cloneCableFactsDifference(message.Prearm.FactsDifference)
	latchCableFailure(row, "arm", message.Prearm.Code, nil)
}

func validCableFactsDifferenceJSON(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] || (name != "targetIndex" && name != "field" && name != "readStatus") {
			return false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return false
		}
		seen[name] = true
	}
	end, err := decoder.Token()
	var extra any
	if err != nil || end != json.Delim('}') || !seen["targetIndex"] || !seen["field"] || !errors.Is(decoder.Decode(&extra), io.EOF) {
		return false
	}
	var d cableprobe.FactsDifference
	if json.Unmarshal(data, &d) != nil {
		return false
	}
	return seen["readStatus"] == (d.Field == "read")
}
