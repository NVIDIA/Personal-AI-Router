// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Only the issued review's public scope is retained, never access credentials
// or executable plans. A restart cannot execute this binding; it can close it.
type fabricReviewBinding struct {
	ReviewID       string         `json:"reviewId"`
	OwnerNodeID    string         `json:"ownerNodeId"`
	OwnerPrincipal string         `json:"ownerPrincipal"`
	RecipeID       string         `json:"recipeId"`
	CableRunID     string         `json:"cableRunId,omitempty"`
	Targets        []fabricTarget `json:"targets"`
	IssuedAt       int64          `json:"issuedAt"`
}

func (s *fabricService) reviewFile(id string) string {
	return filepath.Join(s.m.exec.baseDir, "fabric-reviews", id+".json")
}

// First disposition is exclusive. Partial/uncertain writes remain visible and
// held; neither a later refusal nor a second process may replace acceptance.
func createFabricRecord(filename string, value any) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(value)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

func (b fabricReviewBinding) refusal(message string) *fabricRunRecord {
	return &fabricRunRecord{OwnerPrincipal: b.OwnerPrincipal, Public: fabricOperation{
		SchemaVersion: 1, OperationID: b.ReviewID, ReviewID: b.ReviewID, OwnerNodeID: b.OwnerNodeID,
		RecipeID: b.RecipeID, CableRunID: b.CableRunID, State: "not-started", Targets: b.Targets, CleanupConfirmed: true, Message: message, CreatedAt: time.Now().UnixMilli(), ExpiresAt: 0,
	}}
}

func (s *fabricService) retainReviewLocked(review fabricReview, principal string) error {
	if !review.Executable {
		return nil
	}
	b := fabricReviewBinding{ReviewID: review.ReviewID, OwnerNodeID: review.OwnerNodeID, OwnerPrincipal: principal, RecipeID: review.RecipeID, CableRunID: review.CableRunID, Targets: review.Targets, IssuedAt: time.Now().UnixMilli()}
	if !validFabricRecord(*b.refusal("review binding")) {
		return errors.New("exact fabric review binding unavailable")
	}
	// Admission capacity is reserved before exposing a review ID, so the later
	// definitive refusal never needs to exceed the existing 128-outcome bound.
	known := map[string]bool{}
	for id := range s.runs {
		known[id] = true
	}
	entries, err := os.ReadDir(filepath.Dir(s.reviewFile(b.ReviewID)))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !onboardingID.MatchString(id) || entry.Name() != id+".json" {
			return errors.New("fabric review retention is ambiguous; no new review was issued")
		}
		known[id] = true
	}
	if len(known) >= 128 {
		return errors.New("fabric review retention is full; no new review was issued")
	}
	return createFabricRecord(s.reviewFile(b.ReviewID), b)
}

func (s *fabricService) readReviewBinding(id string) (fabricReviewBinding, error) {
	var b fabricReviewBinding
	if !onboardingID.MatchString(id) {
		return b, errors.New("unknown fabric review")
	}
	raw, err := readOnboardingFile(s.reviewFile(id), 64<<10)
	if err != nil || onboardingDecode(raw, &b) != nil || b.ReviewID != id || b.IssuedAt <= 0 || !validFabricRecord(*b.refusal("review binding")) {
		return b, errors.New("known owner-bound fabric review unavailable; outcome remains unknown")
	}
	return b, nil
}

func (s *fabricService) sameRefusalOwner(node, principal string) bool {
	return s.m.cableLocal != nil && s.m.mesh != nil && s.m.cableLocal.nodeID == node && s.m.mesh.NodeUUID() == principal
}

func (s *fabricService) recordedOutcomeLocked(r *fabricRunRecord) (fabricOperation, error) {
	if r.Public.State == "not-started" && !s.sameRefusalOwner(r.Public.OwnerNodeID, r.OwnerPrincipal) {
		return fabricOperation{}, errors.New("fabric refusal owner changed; outcome remains unknown")
	}
	return cloneFabricOperation(r.Public), nil
}

// Caller holds the canonical admission lock and s.mu. Once this terminal record
// exists, even an approval request queued before status cannot start that ID.
func (s *fabricService) refuseLocked(id, message string) (fabricOperation, error) {
	if !onboardingID.MatchString(id) {
		return fabricOperation{}, errors.New("unknown fabric operation; no effects inferred")
	}
	if r := s.runs[id]; r != nil {
		return s.recordedOutcomeLocked(r)
	}
	if s.recoveryFailed {
		return fabricOperation{}, errors.New("retained fabric outcome is uncertain; no refusal inferred")
	}
	if _, err := os.Lstat(s.file(id)); !errors.Is(err, os.ErrNotExist) {
		return fabricOperation{}, errors.New("fabric outcome file already exists or is unavailable; no refusal inferred")
	}
	b, err := s.readReviewBinding(id)
	if err != nil {
		return fabricOperation{}, err
	}
	if !s.sameRefusalOwner(b.OwnerNodeID, b.OwnerPrincipal) {
		return fabricOperation{}, errors.New("fabric review owner changed; outcome remains unknown")
	}
	r := b.refusal(message)
	if err := createFabricRecord(s.file(id), r); err != nil {
		s.recoveryFailed = true
		return fabricOperation{}, errors.New("fabric refusal could not be confirmed durably; outcome remains unknown")
	}
	s.runs[id] = r
	delete(s.reviews, id)
	return cloneFabricOperation(r.Public), nil
}

// Approval already owns the canonical admission lock during all preflight.
func (s *fabricService) refuse(id, message string) (fabricOperation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refuseLocked(id, message)
}
