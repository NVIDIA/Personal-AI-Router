// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"nvpair-shared/cableprobe"
)

type cableCleanupReviewRequest struct {
	RunID            string `json:"runId"`
	AcceptedHostKeys []struct {
		CandidateID string `json:"candidateId"`
		SHA256      string `json:"sha256"`
	} `json:"acceptedHostKeys,omitempty"`
}
type cableCleanupReviewBinding struct {
	public                     cableprobe.CleanupReview
	runID, originalHash, owner string
	originalRevision           uint64
	expires                    time.Time
	plans                      []cableLaunchPlan
	bindings                   map[string]*http.Client
}
type cableCleanupProof struct {
	Ready  cableCleanupMessage `json:"ready"`
	Closed cableCleanupMessage `json:"closed"`
	Joined bool                `json:"joined"`
}
type cableCleanupAttempt struct {
	Public           cableprobe.CleanupRecovery `json:"public"`
	Approved         bool                       `json:"approved"`
	Challenge        string                     `json:"challenge"`
	Owner            string                     `json:"owner"`
	OriginalRevision uint64                     `json:"originalRevision"`
	Plans            []cableLaunchPlan          `json:"plans"`
	Proofs           []cableCleanupProof        `json:"proofs"`
}
type cableCleanupRecord struct {
	RunID          string                 `json:"runId"`
	OriginalSHA256 string                 `json:"originalSha256"`
	Attempts       []*cableCleanupAttempt `json:"attempts"`
}

func (s *cableProductService) cleanupFile(id string) string {
	return filepath.Join(s.m.exec.baseDir, "cable-recoveries", id+".json")
}
func (s *cableProductService) originalCableHash(id string) (string, error) {
	data, err := readOnboardingFile(s.file(id), 64<<10)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
func (s *cableProductService) saveCleanup(record *cableCleanupRecord) error {
	if !onboardingID.MatchString(record.RunID) || len(record.Attempts) == 0 || len(record.Attempts) > 4 {
		return errors.New("invalid bounded cleanup recovery record")
	}
	if len(onboardingMarshal(record)) > 64<<10 {
		return errors.New("cleanup recovery record exceeds its bound")
	}
	if s.testCleanupSave != nil {
		if err := s.testCleanupSave(record); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.cleanupFile(record.RunID)), 0700); err != nil {
		return err
	}
	return writeJSONAtomic(s.cleanupFile(record.RunID), record)
}
func unresolvedCableTargets(r *cableProductRun) []int {
	var result []int
	for i := range r.Public.Targets {
		if r.Public.Diagnostics != nil && len(r.Public.Diagnostics.Participants) == len(r.Public.Targets) && r.Public.Diagnostics.Participants[i].CleanupConfirmed {
			continue
		}
		result = append(result, i)
	}
	return result
}
func validCleanupReady(message cableCleanupMessage, request cableCleanupRequest) bool {
	if !cableCleanupRequestValid(request) {
		return false
	}
	return message.Protocol == cableCleanupProtocol && message.RunID == request.RunID && message.ReviewID == request.ReviewID && message.AttemptID == request.AttemptID && message.Challenge == request.Challenge && message.NodeID == request.NodeID && message.Principal == request.Principal && message.State == "ready" && message.Code == "clear" && !message.CleanupConfirmed && message.Scope == request.Scope && message.LockInode > 0 && message.Passes == 2 && message.Processes >= 0 && message.Processes <= 8192 && message.Candidates == 0 && message.RemainingMs > 0 && message.RemainingMs <= 10000
}
func validCleanupClosed(message cableCleanupMessage, ready cableCleanupMessage) bool {
	return message.Protocol == ready.Protocol && message.RunID == ready.RunID && message.ReviewID == ready.ReviewID && message.AttemptID == ready.AttemptID && message.Challenge == ready.Challenge && message.NodeID == ready.NodeID && message.Principal == ready.Principal && message.State == "closed" && message.Code == "released" && message.CleanupConfirmed && message.Scope == ready.Scope && message.LockDevice == ready.LockDevice && message.LockInode == ready.LockInode
}

func cleanupReadyFresh(began time.Time, ready cableCleanupMessage) bool {
	return !began.IsZero() && ready.RemainingMs > 0 && time.Since(began) < min(5*time.Second, time.Duration(ready.RemainingMs)*time.Millisecond)
}
func cleanupRequestFor(r *cableProductRun, attempt *cableCleanupAttempt, plan cableLaunchPlan) cableCleanupRequest {
	original := ""
	for _, old := range r.Plans {
		if old.NodeID == plan.NodeID {
			original = old.WorkerSHA256
		}
	}
	return cableCleanupRequest{Protocol: cableCleanupProtocol, RunID: r.Public.RunID, ReviewID: attempt.Public.ReviewID, AttemptID: attempt.Public.AttemptID, Challenge: attempt.Challenge, NodeID: plan.NodeID, Principal: plan.Principal, UID: plan.Info.UID, Scope: *plan.Runtime.CleanupScope, WorkerSHA256: plan.WorkerSHA256, OriginalWorkerSHA256: original}
}
func validCleanupRecord(record *cableCleanupRecord, r *cableProductRun) bool {
	if record.RunID != r.Public.RunID || !onboardingSHA.MatchString(record.OriginalSHA256) || len(record.Attempts) == 0 || len(record.Attempts) > 4 {
		return false
	}
	seen := map[string]bool{}
	for index, a := range record.Attempts {
		if a == nil || !a.Approved || !onboardingID.MatchString(a.Public.AttemptID) || seen[a.Public.AttemptID] || !onboardingID.MatchString(a.Challenge) || !cableIdentifier(a.Public.ReviewID, 128) || a.Owner != r.OwnerPrincipal || a.OriginalRevision != r.Public.Revision || a.Public.Revision == 0 || len(a.Public.Targets) != len(r.Public.Targets) || len(a.Plans) != len(unresolvedCableTargets(r)) || len(a.Proofs) > len(a.Plans) {
			return false
		}
		seen[a.Public.AttemptID] = true
		if !cleanupSummaryCodeValid(a.Public.Code) || (index < len(record.Attempts)-1 && a.Public.State != "failed" && a.Public.State != "cancelled") {
			return false
		}
		for i, t := range a.Public.Targets {
			if t.NodeID != r.Public.Targets[i].NodeID || (t.State != "pending" && t.State != "verified" && t.State != "blocked") || len(t.Reason) > 2048 {
				return false
			}
		}
		if a.Public.State != "verifying" && a.Public.State != "release-pending" && a.Public.State != "released" && a.Public.State != "failed" && a.Public.State != "cancelled" {
			return false
		}
		if a.Public.HoldReleased != (a.Public.State == "released") || a.Public.StartedAt <= 0 || len(a.Public.Message) > 2048 {
			return false
		}
		ids := map[string]bool{}
		for _, p := range a.Plans {
			if p.Runtime == nil || p.Runtime.CleanupScope == nil || !cableCleanupScopeValid(*p.Runtime.CleanupScope) || !cableRuntimePlan(p) || p.Runtime.CleanupProtocol != cableCleanupProtocol || ids[p.NodeID] {
				return false
			}
			ids[p.NodeID] = true
		}
		for _, i := range unresolvedCableTargets(r) {
			if !ids[r.Public.Targets[i].NodeID] {
				return false
			}
		}
		if a.Public.State == "released" {
			if len(a.Proofs) != len(a.Plans) || a.Public.Code != "released" || a.Public.FinishedAt < a.Public.StartedAt {
				return false
			}
			for i, p := range a.Plans {
				request := cleanupRequestFor(r, a, p)
				proof := a.Proofs[i]
				if !validCleanupReady(proof.Ready, request) || !validCleanupClosed(proof.Closed, proof.Ready) || !proof.Joined {
					return false
				}
			}
			for _, t := range a.Public.Targets {
				if t.State != "verified" {
					return false
				}
			}
		}
	}
	return true
}

func cleanupSummaryCodeValid(code string) bool {
	switch code {
	case "verifying", "release-pending", "released", "cancelled", "unsupported-scope", "access-unavailable", "trust-changed", "inspector-unavailable", "inspection-blocked", "inspection-timeout", "inspection-invalid", "cleanup-unconfirmed", "receipt-write-failed", "interrupted":
		return true
	}
	return false
}

func cleanupInspectionReason(code string) string {
	switch code {
	case "candidate-active":
		return "A fixed cable worker or loader is still present; the original hold remains."
	case "candidate-ambiguous", "candidate-unreadable":
		return "A possible cable worker could not be completely classified; the original hold remains."
	case "lock-busy":
		return "The existing cable reservation is busy; the original hold remains."
	case "lock-unavailable", "lock-changed":
		return "The protected cable reservation could not be verified; the original hold remains."
	case "unsupported", "scope-mismatch", "normal-process-changed":
		return "The current native host-SSH deployment scope could not be verified; the original hold remains."
	case "inspection-limit", "deadline-exceeded":
		return "The bounded resource inspection did not complete; the original hold remains."
	}
	return "Cleanup verification did not establish complete resource absence; the original hold remains."
}

func cleanupHistoricalPlanSupported(plan cableLaunchPlan) bool {
	if !cableRuntimePlan(plan) || !strings.EqualFold(plan.WorkerSHA256, plan.Runtime.WorkerSHA256) || plan.WorkerBytes != plan.Runtime.WorkerBytes {
		return false
	}
	return plan.WorkerOrigin == "controller-running-image:"+strings.ToLower(plan.WorkerSHA256) || (strings.HasPrefix(plan.WorkerOrigin, "verified-package:") && onboardingSHA.MatchString(strings.TrimPrefix(plan.WorkerOrigin, "verified-package:")))
}
func (s *cableProductService) cleanupReleasedLocked(r *cableProductRun) bool {
	record := s.cleanupRecords[r.Public.RunID]
	return record != nil && len(record.Attempts) > 0 && record.Attempts[len(record.Attempts)-1].Public.HoldReleased && validCleanupRecord(record, r)
}
func (s *cableProductService) loadCleanupRecords() {
	entries, err := os.ReadDir(filepath.Join(s.m.exec.baseDir, "cable-recoveries"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil || len(entries) > 128 {
		s.recoveryFailed = true
		return
	}
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		r := s.runs[id]
		if entry.IsDir() || !onboardingID.MatchString(id) || entry.Name() != id+".json" || r == nil {
			s.recoveryFailed = true
			continue
		}
		data, e := readOnboardingFile(s.cleanupFile(id), 64<<10)
		var record cableCleanupRecord
		hash, hashErr := s.originalCableHash(id)
		if e != nil || hashErr != nil || onboardingDecode(data, &record) != nil || record.OriginalSHA256 != hash || !validCleanupRecord(&record, r) {
			s.recoveryFailed = true
			continue
		}
		last := record.Attempts[len(record.Attempts)-1]
		if last.Public.State == "verifying" || last.Public.State == "release-pending" {
			last.Public.State = "failed"
			last.Public.Code = "interrupted"
			last.Public.Message = "Cleanup verification was interrupted; the original hold remains."
			last.Public.HoldReleased = false
			last.Public.Revision++
			last.Public.FinishedAt = time.Now().UnixMilli()
		}
		s.cleanupRecords[id] = &record
	}
}

func (s *cableProductService) cleanupAccessAllowed(ids []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shuttingDown || s.recoveryFailed || s.cleanupActive != "" || len(ids) == 0 {
		return false
	}
	for _, review := range s.cleanupReviews {
		if !time.Now().Before(review.expires) {
			continue
		}
		r := s.runs[review.runID]
		if r == nil || r.Public.State != "failed" || r.Public.CleanupConfirmed || s.cleanupReleasedLocked(r) {
			continue
		}
		allowed := map[string]bool{}
		for _, t := range review.public.Targets {
			allowed[t.CandidateID] = t.CandidateID != ""
		}
		ok := true
		for _, id := range ids {
			if !allowed[id] {
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func (s *cableProductService) reviewCleanup(ctx context.Context, request cableCleanupReviewRequest) (cableprobe.CleanupReview, error) {
	if !onboardingID.MatchString(request.RunID) || len(request.AcceptedHostKeys) > 3 {
		return cableprobe.CleanupReview{}, errors.New("invalid cleanup review identity")
	}
	s.mu.Lock()
	r := s.runs[request.RunID]
	if r == nil || r.Public.State != "failed" || r.Public.CleanupConfirmed || s.shuttingDown || s.recoveryFailed || s.cleanupActive != "" || s.cleanupReleasedLocked(r) {
		s.mu.Unlock()
		return cableprobe.CleanupReview{}, errors.New("cleanup verification is unavailable for this operation")
	}
	select {
	case <-r.done:
	default:
		s.mu.Unlock()
		return cableprobe.CleanupReview{}, errors.New("the original operation is still finalizing")
	}
	indexes := unresolvedCableTargets(r)
	original := cloneCableRun(r.Public)
	owner := r.OwnerPrincipal
	for _, index := range indexes {
		if !cleanupHistoricalPlanSupported(r.Plans[index]) {
			s.mu.Unlock()
			return cableprobe.CleanupReview{}, errors.New("the original worker lacks the supported native runtime ownership binding")
		}
	}
	s.mu.Unlock()
	if len(indexes) == 0 {
		return cableprobe.CleanupReview{}, errors.New("no unresolved participant is available for inspection")
	}
	hash, err := s.originalCableHash(request.RunID)
	if err != nil {
		return cableprobe.CleanupReview{}, errors.New("original operation evidence is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	began := time.Now()
	review := cableprobe.CleanupReview{ReviewID: "cable-cleanup-review-" + newOpID(), RunID: request.RunID, Available: true, Effects: []string{"Inspect only the unresolved participant's fixed PAIR cable worker and reservation under fresh administrator approval. No packets, process signals, file deletion, or network changes.", "Release only this operation's scheduling hold if complete current verification and inspector cleanup succeed. The original failed result and missing cleanup acknowledgement remain unchanged."}, Targets: []cableprobe.CleanupReviewTarget{}}
	binding := cableCleanupReviewBinding{runID: request.RunID, originalHash: hash, owner: owner, originalRevision: original.Revision, expires: began.Add(30 * time.Second), bindings: map[string]*http.Client{}}
	if owner != s.m.mesh.NodeUUID() || original.OwnerNodeID != s.m.cableLocal.nodeID {
		return review, errors.New("the original controller identity is not current")
	}
	if client, ok := s.m.remoteHTTP.Client(owner); ok {
		binding.bindings[owner] = client
	} else {
		return review, errors.New("current controller pairing is unavailable")
	}
	for _, index := range indexes {
		target := original.Targets[index]
		row := cableprobe.CleanupReviewTarget{NodeID: target.NodeID}
		peer, ok := s.m.peers.lookup(target.NodeID)
		address := ""
		if target.NodeID == s.m.cableLocal.nodeID {
			address = "127.0.0.1"
		} else if ok && peer.clusterUUID == target.Principal && len(peer.addresses) > 0 {
			address = peer.addresses[0]
		}
		if client, pinned := s.m.remoteHTTP.Client(target.Principal); pinned {
			binding.bindings[target.Principal] = client
		} else {
			address = ""
		}
		if address == "" {
			row.Reason = "The original participant is not currently paired and reachable."
			review.Targets = append(review.Targets, row)
			review.Available = false
			continue
		}
		candidate, e := s.m.onboarding.addTarget(onboardingAddTargetRequest{Address: address, Port: 22, Label: target.NodeID})
		if e != nil {
			return review, errors.New("cleanup access candidate is unavailable")
		}
		row.CandidateID = candidate.CandidateID
		s.m.onboarding.mu.Lock()
		stored := s.m.onboarding.targets[candidate.CandidateID]
		var current onboardingPrivateTarget
		if stored != nil {
			current = *stored
		}
		s.m.onboarding.mu.Unlock()
		candidate = current.candidate
		row.AccessLabel = candidate.AccessLabel
		row.HostKeySHA256 = candidate.HostKeySHA256
		row.HostKeyTrusted = candidate.HostKeyTrusted && !current.changedKey
		for _, key := range request.AcceptedHostKeys {
			if key.CandidateID == candidate.CandidateID && key.SHA256 != "" && key.SHA256 == candidate.HostKeySHA256 && !current.changedKey {
				row.HostKeyTrusted = true
			}
		}
		row.AccessAvailable = candidate.AccessAvailable && time.Now().Before(current.expiresAt)
		row.ElevationAvailable = row.AccessAvailable && current.access.elevationPassword != "" && !strings.ContainsAny(current.access.elevationPassword, "\r\n\x00")
		row.Reason = "Fresh device access, verified SSH identity and separate administrator approval are required."
		if row.AccessAvailable && row.HostKeyTrusted && row.ElevationAvailable {
			candidate.HostKeyTrusted = true
			plan := cableLaunchPlan{NodeID: target.NodeID, Principal: target.Principal, Candidate: candidate, AccessGeneration: current.accessGeneration}
			runtimeBinding, e := s.workerBinding(ctx, target)
			if e == nil && runtimeBinding.CleanupProtocol == cableCleanupProtocol && runtimeBinding.CleanupScope != nil && cableCleanupScopeValid(*runtimeBinding.CleanupScope) {
				plan.Runtime = &runtimeBinding
				client, dialErr := s.m.onboarding.dial(ctx, candidate, current.access)
				if dialErr == nil {
					plan.Info, e = readOnboardingPlatform(ctx, client)
					if e == nil {
						plan, e = s.inspect(ctx, client, plan, s.m.mesh)
					}
					if e == nil {
						plan.WorkerOrigin, e = s.knownWorker(ctx, plan)
					}
					client.close()
					if e == nil {
						row.InspectorAvailable = true
						row.Reason = "The current native Linux host-SSH inspector is available for explicit approval."
						binding.plans = append(binding.plans, plan)
					}
				}
			}
			if !row.InspectorAvailable {
				row.Reason = "This installation or deployment scope does not provide a verified native Linux host-SSH cleanup inspector."
			}
		}
		if !row.InspectorAvailable {
			review.Available = false
		}
		review.Targets = append(review.Targets, row)
	}
	review.RemainingMs = max(0, time.Until(binding.expires).Milliseconds())
	if review.RemainingMs == 0 {
		review.Available = false
	}
	if !review.Available {
		review.Reason = "Resolve the stated prerequisites and review again. No administrator inspection was attempted."
	}
	binding.public = review
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, old := range s.cleanupReviews {
		if !time.Now().Before(old.expires) {
			delete(s.cleanupReviews, id)
		}
	}
	if len(s.cleanupReviews) >= 16 {
		return review, errors.New("too many live cleanup reviews")
	}
	s.cleanupReviews[review.ReviewID] = binding
	return review, nil
}

func (s *cableProductService) verifyCleanup(ctx context.Context, reviewID string, approved bool) (cableprobe.Run, error) {
	s.m.onboarding.mu.Lock()
	defer s.m.onboarding.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.verifyCleanupLocked(ctx, reviewID, approved)
}

// A refusal is definitive only while admission is serialized, accepted history
// has been checked, and the refused review can no longer launch. Transport
// errors and missing/corrupt operation history never imply this disposition.
func (s *cableProductService) verifyCleanupAdmission(ctx context.Context, runID, reviewID string, approved bool) (cableprobe.CleanupVerifyResult, error) {
	invalid := errors.New("cleanup approval does not match a retained run and review")
	if !approved || !onboardingID.MatchString(runID) || !cableIdentifier(reviewID, 128) {
		return cableprobe.CleanupVerifyResult{}, invalid
	}
	s.m.onboarding.mu.Lock()
	defer s.m.onboarding.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[runID]
	if r == nil || s.recoveryFailed {
		return cableprobe.CleanupVerifyResult{}, invalid
	}
	for _, record := range s.cleanupRecords {
		for _, attempt := range record.Attempts {
			if attempt.Public.ReviewID == reviewID {
				if record.RunID != runID {
					return cableprobe.CleanupVerifyResult{}, invalid
				}
				return cableprobe.CleanupVerifyResult{Disposition: "accepted", ReviewID: reviewID, Run: s.snapshot(r)}, nil
			}
		}
	}
	if binding, ok := s.cleanupReviews[reviewID]; ok && binding.runID != runID {
		return cableprobe.CleanupVerifyResult{}, invalid
	}
	if err := ctx.Err(); err != nil {
		return cableprobe.CleanupVerifyResult{}, err
	}
	run, err := s.verifyCleanupLocked(ctx, reviewID, approved)
	if err == nil {
		return cableprobe.CleanupVerifyResult{Disposition: "accepted", ReviewID: reviewID, Run: run}, nil
	}
	// All errors from the locked core precede the execute goroutine. Retiring
	// under these same locks also makes a lost refusal reply safe to retry.
	delete(s.cleanupReviews, reviewID)
	return cableprobe.CleanupVerifyResult{Disposition: "not-started", ReviewID: reviewID, Run: s.snapshot(r)}, nil
}

func (s *cableProductService) verifyCleanupLocked(ctx context.Context, reviewID string, approved bool) (cableprobe.Run, error) {
	if !approved {
		return cableprobe.Run{}, errors.New("explicit approval of cleanup inspection is required")
	}
	if err := ctx.Err(); err != nil {
		return cableprobe.Run{}, err
	}
	for _, record := range s.cleanupRecords {
		for _, a := range record.Attempts {
			if a.Public.ReviewID == reviewID {
				return s.snapshot(s.runs[record.RunID]), nil
			}
		}
	}
	binding, ok := s.cleanupReviews[reviewID]
	r := s.runs[binding.runID]
	if !ok || r == nil || !binding.public.Available || !time.Now().Before(binding.expires) || s.shuttingDown || s.recoveryFailed || s.cleanupActive != "" || s.m.onboarding.active != "" || r.Public.State != "failed" || r.Public.CleanupConfirmed || r.Public.Revision != binding.originalRevision || s.cleanupReleasedLocked(r) {
		return cableprobe.Run{}, errors.New("cleanup review expired, changed or is held by another operation")
	}
	hash, err := s.originalCableHash(r.Public.RunID)
	if err != nil || hash != binding.originalHash {
		return cableprobe.Run{}, errors.New("original operation evidence changed")
	}
	if err = s.trust(binding.owner, binding.bindings); err != nil {
		return cableprobe.Run{}, errors.New("reviewed pairing changed")
	}
	for _, plan := range binding.plans {
		if _, err = s.accessLocked(plan); err != nil {
			return cableprobe.Run{}, errors.New("reviewed cleanup access changed")
		}
	}
	record := s.cleanupRecords[r.Public.RunID]
	if record == nil {
		record = &cableCleanupRecord{RunID: r.Public.RunID, OriginalSHA256: hash, Attempts: []*cableCleanupAttempt{}}
	}
	if len(record.Attempts) >= 4 {
		return cableprobe.Run{}, errors.New("bounded cleanup verification attempt limit reached")
	}
	attempt := &cableCleanupAttempt{Approved: true, Challenge: newOpID(), Owner: binding.owner, OriginalRevision: r.Public.Revision, Plans: binding.plans, Proofs: []cableCleanupProof{}, Public: cableprobe.CleanupRecovery{AttemptID: newOpID(), ReviewID: reviewID, Revision: 1, State: "verifying", Code: "verifying", Message: "Verifying only the unresolved fixed cable resources; the original cleanup acknowledgement remains unavailable.", StartedAt: time.Now().UnixMilli(), Targets: []cableprobe.CleanupRecoveryTarget{}}}
	for i, t := range r.Public.Targets {
		state, reason := "pending", "Current inspection is pending."
		if r.Public.Diagnostics != nil && r.Public.Diagnostics.Participants[i].CleanupConfirmed {
			state, reason = "verified", "The original worker acknowledged cleanup; no new inspection is needed."
		}
		attempt.Public.Targets = append(attempt.Public.Targets, cableprobe.CleanupRecoveryTarget{NodeID: t.NodeID, State: state, Reason: reason})
	}
	next := *record
	next.Attempts = append(append([]*cableCleanupAttempt(nil), record.Attempts...), attempt)
	if err = s.saveCleanup(&next); err != nil {
		return cableprobe.Run{}, errors.New("cleanup approval could not be retained before inspection")
	}
	s.cleanupRecords[r.Public.RunID] = &next
	operationCtx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
	s.cleanupActive = attempt.Public.AttemptID
	s.cleanupCancel = cancel
	s.cleanupDone = make(chan struct{})
	go s.executeCleanup(operationCtx, r, binding, &next, attempt)
	return s.snapshot(r), nil
}

func (s *cableProductService) cancelCleanup(runID string) (cableprobe.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[runID]
	if r == nil {
		return cableprobe.Run{}, errors.New("cleanup operation is unknown")
	}
	if record := s.cleanupRecords[runID]; record != nil && len(record.Attempts) > 0 && record.Attempts[len(record.Attempts)-1].Public.AttemptID == s.cleanupActive && s.cleanupCancel != nil {
		s.cleanupCancel()
	}
	return s.snapshot(r), nil
}

func (s *cableProductService) executeCleanup(ctx context.Context, r *cableProductRun, binding cableCleanupReviewBinding, record *cableCleanupRecord, attempt *cableCleanupAttempt) {
	defer func() {
		s.mu.Lock()
		s.cleanupActive = ""
		if s.cleanupCancel != nil {
			s.cleanupCancel()
		}
		s.cleanupCancel = nil
		close(s.cleanupDone)
		s.cleanupDone = nil
		s.mu.Unlock()
	}()
	type participant struct {
		worker *cableCleanupWorker
		client *onboardingSSH
		ready  cableCleanupMessage
		at     time.Time
		err    error
	}
	participants := make([]participant, len(attempt.Plans))
	code, message := "inspection-blocked", "Cleanup verification did not establish complete resource absence; the hold remains."
	var failure error
	defer func() {
		for i := range participants {
			if participants[i].worker != nil {
				participants[i].worker.close()
			}
			if participants[i].client != nil {
				participants[i].client.close()
			}
		}
	}()
	for i, plan := range attempt.Plans {
		p := &participants[i]
		access, err := s.access(plan)
		if err != nil {
			failure = err
			code = "access-unavailable"
			break
		}
		if err = s.trust(binding.owner, binding.bindings); err != nil {
			failure = err
			code = "trust-changed"
			break
		}
		p.client, err = s.m.onboarding.dial(ctx, plan.Candidate, access)
		if err != nil {
			failure = err
			code = "access-unavailable"
			break
		}
		request := cleanupRequestFor(r, attempt, plan)
		p.at = time.Now()
		p.worker, err = s.cleanupLaunch(ctx, p.client, plan, request, access.elevationPassword)
		access = onboardingAccess{}
		if err != nil {
			failure = err
			code = "inspector-unavailable"
			break
		}
		p.ready, err = p.worker.read(ctx)
		if err != nil || !validCleanupReady(p.ready, request) {
			failure = errors.New("cleanup inspector did not establish the reviewed proof")
			code = "inspection-blocked"
			message = cleanupInspectionReason(p.ready.Code)
			break
		}
	}
	if failure == nil {
		failure = ctx.Err()
	}
	if failure == nil {
		failure = s.trust(binding.owner, binding.bindings)
		if failure != nil {
			code = "trust-changed"
		}
	}
	if failure == nil {
		s.mu.Lock()
		attempt.Proofs = make([]cableCleanupProof, len(participants))
		for i, p := range participants {
			attempt.Proofs[i].Ready = p.ready
		}
		attempt.Public.State = "release-pending"
		attempt.Public.Code = "release-pending"
		attempt.Public.Message = "Current absence was observed; waiting for the same inspectors to close before releasing this hold."
		attempt.Public.Revision++
		failure = s.saveCleanup(record)
		s.mu.Unlock()
		if failure != nil {
			code = "receipt-write-failed"
		}
	}
	if failure == nil {
		for i := range participants {
			p := &participants[i]
			if !cleanupReadyFresh(p.at, p.ready) || ctx.Err() != nil {
				failure = errors.New("cleanup proof expired before close")
				code = "inspection-timeout"
				break
			}
			closed, err := p.worker.finish(ctx, cableCleanupCommand{AttemptID: attempt.Public.AttemptID, Challenge: attempt.Challenge, Command: "release"})
			if err != nil || !validCleanupClosed(closed, p.ready) {
				failure = errors.New("inspector cleanup is unconfirmed")
				code = "cleanup-unconfirmed"
				break
			}
			s.mu.Lock()
			attempt.Proofs[i].Closed = closed
			attempt.Proofs[i].Joined = true
			s.mu.Unlock()
		}
	}
	// A restart or scope change after inspection cannot consume a proof bound
	// to the previous normal worker, even when its certificate is unchanged.
	if failure == nil {
		for i, plan := range attempt.Plans {
			if !cleanupReadyFresh(participants[i].at, participants[i].ready) {
				failure = errors.New("cleanup proof expired")
				code = "inspection-timeout"
				break
			}
			current, err := s.workerBinding(ctx, cableprobe.Target{NodeID: plan.NodeID, Principal: plan.Principal})
			if err != nil || !reflect.DeepEqual(current.CleanupScope, plan.Runtime.CleanupScope) || current.CleanupProtocol != plan.Runtime.CleanupProtocol || current.WorkerSHA256 != plan.Runtime.WorkerSHA256 || current.CertificateSHA256 != plan.Runtime.CertificateSHA256 {
				failure = errors.New("reviewed cleanup deployment changed")
				code = "unsupported-scope"
				break
			}
		}
	}
	s.m.onboarding.mu.Lock()
	s.mu.Lock()
	if failure == nil {
		if ctx.Err() != nil || s.shuttingDown || s.cleanupActive != attempt.Public.AttemptID {
			failure = errors.New("cleanup release cancelled")
			code = "cancelled"
		}
		if failure == nil {
			if err := s.trust(binding.owner, binding.bindings); err != nil {
				failure = err
				code = "trust-changed"
			}
		}
		for _, plan := range attempt.Plans {
			if failure != nil {
				break
			}
			if _, err := s.accessLocked(plan); err != nil {
				failure = err
				code = "access-unavailable"
			}
		}
		hash, err := s.originalCableHash(r.Public.RunID)
		if failure == nil && (err != nil || hash != binding.originalHash || r.Public.Revision != binding.originalRevision) {
			failure = errors.New("original run changed")
			code = "inspection-invalid"
		}
		if failure == nil {
			for _, p := range participants {
				if !cleanupReadyFresh(p.at, p.ready) {
					failure = errors.New("cleanup proof expired at final release gate")
					code = "inspection-timeout"
					break
				}
			}
		}
	}
	attempt.Public.FinishedAt = time.Now().UnixMilli()
	attempt.Public.Revision++
	if failure == nil {
		attempt.Public.State = "released"
		attempt.Public.HoldReleased = true
		attempt.Public.Code = "released"
		attempt.Public.Message = "This operation's hold was separately released after current resource verification. Its original cleanup acknowledgement remains unavailable."
		for i := range attempt.Public.Targets {
			attempt.Public.Targets[i].State = "verified"
			attempt.Public.Targets[i].Reason = "This participant no longer holds the operation's scheduling boundary."
		}
		if !validCleanupRecord(record, r) || s.saveCleanup(record) != nil {
			failure = errors.New("cleanup release could not be retained")
			code = "receipt-write-failed"
		}
	}
	if failure != nil {
		attempt.Public.State = "failed"
		attempt.Public.HoldReleased = false
		if errors.Is(ctx.Err(), context.Canceled) {
			attempt.Public.State = "cancelled"
			code = "cancelled"
			message = "Cleanup verification was cancelled; the original hold remains."
		} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code = "inspection-timeout"
		}
		attempt.Public.Code = code
		attempt.Public.Message = message
		for i := range attempt.Public.Targets {
			if attempt.Public.Targets[i].State == "pending" {
				attempt.Public.Targets[i].State = "blocked"
				attempt.Public.Targets[i].Reason = "Current verification or inspector cleanup was not confirmed."
			}
		}
		if s.saveCleanup(record) != nil {
			s.recoveryFailed = true
		}
	}
	for _, plan := range attempt.Plans {
		if t := s.m.onboarding.targets[plan.Candidate.CandidateID]; t != nil && t.accessGeneration == plan.AccessGeneration {
			t.access = onboardingAccess{}
			t.candidate.AccessAvailable = false
			t.candidate.Reason = "Cleanup verification ended; temporary account access was released."
		}
	}
	s.mu.Unlock()
	s.m.onboarding.mu.Unlock()
}

func (m *Manager) runCableCleanup(ctx context.Context, msg *Message) {
	switch msg.Method {
	case "engine:cable-cleanup-review":
		var request cableCleanupReviewRequest
		if onboardingDecode(msg.Params, &request) != nil {
			m.codec.RespondError(msg.ID, -32602, "invalid cleanup review")
			return
		}
		result, err := m.cables.reviewCleanup(ctx, request)
		m.respondOrErr(msg, result, err)
	case "engine:cable-cleanup-verify":
		var request struct {
			RunID        string `json:"runId"`
			ReviewID     string `json:"reviewId"`
			ApproveAdmin bool   `json:"approveAdmin"`
		}
		if onboardingDecode(msg.Params, &request) != nil {
			m.codec.RespondError(msg.ID, -32602, "invalid cleanup approval")
			return
		}
		result, err := m.cables.verifyCleanupAdmission(ctx, request.RunID, request.ReviewID, request.ApproveAdmin)
		m.respondOrErr(msg, result, err)
	case "engine:cable-cleanup-cancel":
		var request struct {
			RunID string `json:"runId"`
		}
		if onboardingDecode(msg.Params, &request) != nil {
			m.codec.RespondError(msg.ID, -32602, "invalid cleanup cancellation")
			return
		}
		result, err := m.cables.cancelCleanup(request.RunID)
		m.respondOrErr(msg, result, err)
	}
}

func cloneCleanupPublic(value cableprobe.CleanupRecovery) *cableprobe.CleanupRecovery {
	var out cableprobe.CleanupRecovery
	_ = onboardingDecode(onboardingMarshal(value), &out)
	return &out
}
