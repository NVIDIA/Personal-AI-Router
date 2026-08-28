// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"sync"
)

// serviceError mirrors nvpair-shared/errors.ServiceError on the wire. Keeping
// the JSON tags identical preserves the shared producer-to-broker contract.
// Engine-manager emits these as errors:report / errors:clear notifications on
// its own stdio; the supervising broker forwards them to nvpair-errors and
// stamps NodeID/Timestamp, so producers leave those zero.
type serviceError struct {
	ID      string `json:"id"`
	Message string `json:"message"`
	// Timestamp has no omitempty: the canonical nvpair-shared/errors tag is
	// `json:"timestamp"` (a required field). Producers emit it as 0 for the
	// broker to stamp, matching the nvpair-manual-nodes producer byte-for-byte.
	Timestamp  int64  `json:"timestamp"`
	NodeID     string `json:"nodeId,omitempty"`
	Severity   string `json:"severity,omitempty"`
	Action     string `json:"action,omitempty"`
	EngineType string `json:"engineType,omitempty"`
	Operation  string `json:"operation,omitempty"`
	ModelName  string `json:"modelName,omitempty"`
}

// clearParams mirrors nvpair-shared/errors.ClearParams. ClearedBy is
// broker-stamped; producers leave it empty, so omitempty keeps the wire
// shape identical to emitting just {"id":...}.
type clearParams struct {
	ID        string `json:"id"`
	ClearedBy string `json:"clearedBy,omitempty"`
}

// maxRecentErrors bounds the in-memory error ring queried via
// engine:errors.
const maxRecentErrors = 256

// Reporter is the single seam every surfaced error flows through. It
// always logs locally (slog → stderr, captured by the parent's debug
// panel), keeps a bounded ring queryable via engine:errors, and emits
// errors:report / errors:clear on the codec for the broker to forward
// to nvpair-errors. There is one Reporter implementation in v1; when a
// second sink is needed it slots in here without touching the
// executor.
type Reporter struct {
	codec *Codec

	mu     sync.Mutex
	recent []serviceError
}

func NewReporter(codec *Codec) *Reporter {
	return &Reporter{codec: codec}
}

// report upserts an error by id: records it locally and emits
// errors:report for the nvpair-errors pipeline.
func (r *Reporter) report(e serviceError) {
	slog.Warn("engine error reported", "id", e.ID, "severity", e.Severity, "message", e.Message)

	r.mu.Lock()
	r.recent = upsertError(r.recent, e)
	if len(r.recent) > maxRecentErrors {
		r.recent = append([]serviceError(nil), r.recent[len(r.recent)-maxRecentErrors:]...)
	}
	r.mu.Unlock()

	if r.codec != nil {
		_ = r.codec.Notify("errors:report", e)
	}
}

// clear removes an error by id and emits errors:clear. A clear for an
// id that isn't present is harmless (nvpair-errors treats it as a no-op).
func (r *Reporter) clear(id string) {
	r.mu.Lock()
	r.recent = removeError(r.recent, id)
	r.mu.Unlock()
	if r.codec != nil {
		_ = r.codec.Notify("errors:clear", clearParams{ID: id})
	}
}

// snapshot returns the current recent-error list (for engine:errors).
func (r *Reporter) snapshot() []serviceError {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]serviceError, len(r.recent))
	copy(out, r.recent)
	return out
}

func upsertError(list []serviceError, e serviceError) []serviceError {
	for i := range list {
		if list[i].ID == e.ID {
			list[i] = e
			return list
		}
	}
	return append(list, e)
}

func removeError(list []serviceError, id string) []serviceError {
	out := make([]serviceError, 0, len(list))
	for _, e := range list {
		if e.ID != id {
			out = append(out, e)
		}
	}
	return out
}
