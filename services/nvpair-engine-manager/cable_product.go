// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"nvpair-shared/cableprobe"
	"nvpair-shared/clustertrust"
)

// Launch plans contain public ownership/identity references only. Access secrets
// remain in onboardingService.targets and are re-read only for the same generation.
type cableLaunchPlan struct {
	NodeID            string                   `json:"nodeId"`
	Principal         string                   `json:"principal"`
	Candidate         onboardingCandidate      `json:"candidate"`
	AccessGeneration  string                   `json:"-"`
	Receipt           onboardingInstallReceipt `json:"receipt"`
	Info              onboardingPlatformInfo   `json:"info"`
	WorkerSHA256      string                   `json:"workerSha256"`
	WorkerBytes       int64                    `json:"workerBytes"`
	CertificateSHA256 string                   `json:"certificateSha256"`
	WorkerOrigin      string                   `json:"workerOrigin"`
	Runtime           *cableWorkerBinding      `json:"runtime,omitempty"`
}

type cableProductReviewRequest struct {
	cableprobe.ReviewRequest
	AcceptedHostKeys []struct {
		CandidateID string `json:"candidateId"`
		SHA256      string `json:"sha256"`
	} `json:"acceptedHostKeys,omitempty"`
}

type cableProductReview struct {
	public         cableprobe.Review
	selection      cableprobe.ReviewRequest
	plans          []cableLaunchPlan
	bindings       map[string]*http.Client
	ownerPrincipal string
	expires        time.Time
}

type cableProductRun struct {
	Public         cableprobe.Run    `json:"run"`
	OwnerPrincipal string            `json:"ownerPrincipal"`
	AdminApproved  bool              `json:"adminApproved"`
	Plans          []cableLaunchPlan `json:"plans"`
	bindings       map[string]*http.Client
	cancel         context.CancelFunc
	done           chan struct{}
	windowAt       time.Time
}

type cableProductService struct {
	m               *Manager
	ctx             context.Context
	mu              sync.Mutex
	reviews         map[string]cableProductReview
	runs            map[string]*cableProductRun
	recoveryFailed  bool
	shuttingDown    bool
	cleanupReviews  map[string]cableCleanupReviewBinding
	cleanupRecords  map[string]*cableCleanupRecord
	cleanupActive   string
	cleanupCancel   context.CancelFunc
	cleanupDone     chan struct{}
	cleanupLaunch   func(context.Context, *onboardingSSH, cableLaunchPlan, cableCleanupRequest, string) (*cableCleanupWorker, error)
	testCleanupSave func(*cableCleanupRecord) error
	testSave        func(*cableProductRun) error
	// Process I/O seams for socket-free fixtures, never alternate trust owners.
	inspect       func(context.Context, *onboardingSSH, cableLaunchPlan, *clustertrust.Mesh) (cableLaunchPlan, error)
	launch        func(context.Context, *onboardingSSH, cableLaunchPlan, cableProbeOnceRequest, string) (*cableWorker, error)
	workerBinding func(context.Context, cableprobe.Target) (cableWorkerBinding, error)
	knownWorker   func(context.Context, cableLaunchPlan) (string, error)
}

func newCableProductService(m *Manager) *cableProductService {
	s := &cableProductService{m: m, ctx: context.Background(), reviews: map[string]cableProductReview{}, runs: map[string]*cableProductRun{}, inspect: inspectCableWorker, launch: openCableWorker}
	s.workerBinding = s.readWorkerBinding
	s.knownWorker = s.matchKnownWorker
	s.cleanupReviews = map[string]cableCleanupReviewBinding{}
	s.cleanupRecords = map[string]*cableCleanupRecord{}
	s.cleanupLaunch = openCableCleanupWorker
	s.load()
	s.loadCleanupRecords()
	return s
}

func (s *cableProductService) readWorkerBinding(ctx context.Context, target cableprobe.Target) (cableWorkerBinding, error) {
	var binding cableWorkerBinding
	var err error
	if target.NodeID == s.m.cableLocal.nodeID {
		binding, err = inspectRunningCableWorker(s.m.cableLocal, s.m.mesh)
	} else {
		peer, ok := s.m.peers.lookup(target.NodeID)
		if !ok || peer.clusterUUID != target.Principal {
			return binding, errors.New("paired worker owner is not discoverable")
		}
		client, e := s.m.remoteClient(ctx, peer)
		if e != nil {
			return binding, e
		}
		err = readCableJSON(ctx, client.http, client.base+controlCableWorkerPath, &binding)
	}
	if err != nil {
		var failure *cableFactsError
		if errors.As(err, &failure) && failure.unsupported {
			return binding, &cableFactsError{message: "This paired app does not expose the runnable cable-worker capability.", unsupported: true}
		}
		return binding, err
	}
	if binding.NodeID != target.NodeID || binding.Principal != target.Principal || binding.UID <= 0 || (binding.Protocol != "pair-cable-worker/1" && binding.Protocol != "pair-cable-worker/2" && binding.Protocol != "pair-cable-worker/3" && binding.Protocol != cableWorkerProtocol) ||
		!onboardingSHA.MatchString(binding.WorkerSHA256) || !onboardingSHA.MatchString(binding.CertificateSHA256) || binding.WorkerBytes <= 0 || binding.WorkerBytes > cableWorkerMaxBytes ||
		!path.IsAbs(binding.WorkerPath) || path.Clean(binding.WorkerPath) != binding.WorkerPath || len(binding.WorkerPath) > 2048 || !path.IsAbs(binding.ProfileDir) || path.Clean(binding.ProfileDir) != binding.ProfileDir || len(binding.ProfileDir) > 2048 {
		return cableWorkerBinding{}, errors.New("paired running worker capability or identity is invalid")
	}
	return binding, nil
}

func (s *cableProductService) heldLocked() bool {
	if s.recoveryFailed || s.cleanupActive != "" {
		return true
	}
	for _, r := range s.runs {
		if r.Public.State == "preparing" || r.Public.State == "running" || r.Public.State == "cancelling" || (!r.Public.CleanupConfirmed && !s.cleanupReleasedLocked(r)) {
			return true
		}
	}
	return false
}
func (s *cableProductService) held() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.heldLocked() }
func (s *cableProductService) file(id string) string {
	return filepath.Join(s.m.exec.baseDir, "cable-operations", id+".json")
}
func (s *cableProductService) save(r *cableProductRun) error {
	if !onboardingID.MatchString(r.Public.RunID) {
		return errors.New("invalid owned cable operation")
	}
	if s.testSave != nil {
		if err := s.testSave(r); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.file(r.Public.RunID)), 0700); err != nil {
		return err
	}
	return writeJSONAtomic(s.file(r.Public.RunID), r)
}
func (s *cableProductService) load() {
	entries, err := os.ReadDir(filepath.Join(s.m.exec.baseDir, "cable-operations"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil || len(entries) > 128 {
		s.recoveryFailed = true
		return
	}
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !onboardingID.MatchString(id) || entry.Name() != id+".json" {
			s.recoveryFailed = true
			continue
		}
		data, err := readOnboardingFile(s.file(id), 64<<10)
		var run cableProductRun
		if err != nil || onboardingDecode(data, &run) != nil || run.Public.RunID != id || !validCableProductRecord(run) {
			s.recoveryFailed = true
			continue
		}
		run.done = make(chan struct{})
		close(run.done)
		if run.Public.State == "preparing" || run.Public.State == "running" || run.Public.State == "cancelling" {
			run.Public.State, run.Public.Result, run.Public.CleanupConfirmed = "failed", "incomplete", false
			run.Public.Message = "Coordinator restarted; remote worker cleanup is not confirmed. No replacement run was started."
			run.Public.FinishedAt = max(time.Now().UnixMilli(), run.Public.StartedAt)
			run.Public.Revision++
		}
		run.Public.RemainingMs, run.Public.FreshnessRemainingMs = 0, 0
		for i := range run.Public.Edges {
			run.Public.Edges[i].Fresh = false
		}
		s.runs[id] = &run
	}
}

func validCableProductRecord(record cableProductRun) bool {
	r := record.Public
	if r.CleanupRecovery != nil {
		return false
	} // Recovery is derived from its separate owned receipt.
	if !validCableDiagnostics(r) {
		return false
	}
	if !record.AdminApproved || len(record.Plans) != len(r.Targets) || !onboardingID.MatchString(r.RunID) || !cableIdentifier(r.ReviewID, 128) || !cableIdentifier(r.OwnerNodeID, 128) || !cableIdentifier(record.OwnerPrincipal, 256) || r.Revision == 0 || r.StartedAt <= 0 || r.Directness != "unverified" || r.RemainingMs < 0 || r.RemainingMs > cableprobe.Window.Milliseconds() || r.FreshnessRemainingMs != 0 || len(r.Message) > 2048 || len(r.Targets) < 2 || len(r.Targets) > 3 || len(r.Edges) > 6 {
		return false
	}
	if r.Topology != nil && !reflect.DeepEqual(r.Topology, cableTopologyResult(r)) {
		return false
	}
	for i, plan := range record.Plans {
		if plan.NodeID != r.Targets[i].NodeID || plan.Principal != r.Targets[i].Principal || !onboardingSHA.MatchString(plan.WorkerSHA256) || plan.WorkerBytes <= 0 || plan.WorkerOrigin == "" || len(plan.WorkerOrigin) > 128 || !onboardingSHA.MatchString(plan.CertificateSHA256) {
			return false
		}
	}
	terminal := r.State == "completed" || r.State == "cancelled" || r.State == "failed"
	if !terminal && r.State != "preparing" && r.State != "running" && r.State != "cancelling" {
		return false
	}
	if terminal && (r.FinishedAt < r.StartedAt || r.RemainingMs != 0) {
		return false
	}
	if r.Result != "unavailable" && r.Result != "incomplete" && r.Result != "reciprocal-observations" && r.Result != "ambiguous" {
		return false
	}
	nodes, principals := map[string]bool{}, map[string]bool{}
	ports := map[cableprobe.PortRef]bool{}
	for _, t := range r.Targets {
		if !cableIdentifier(t.NodeID, 128) || !cableIdentifier(t.Principal, 256) || nodes[t.NodeID] || principals[t.Principal] || len(t.Ports) < 1 || len(t.Ports) > 2 {
			return false
		}
		nodes[t.NodeID], principals[t.Principal] = true, true
		indexes, names := map[int]bool{}, map[string]bool{}
		for _, p := range t.Ports {
			ref := cableprobe.PortRef{NodeID: t.NodeID, SwitchID: p.SwitchID, PortName: p.PortName}
			if !cableIdentifier(p.SwitchID, 128) || !cableIdentifier(p.PortName, 128) || ports[ref] || len(p.Interfaces) < 1 || len(p.Interfaces) > 4 {
				return false
			}
			ports[ref] = true
			for _, a := range p.Interfaces {
				if !cableInterfaceValid(a) || indexes[a.Index] || names[a.Name] {
					return false
				}
				indexes[a.Index], names[a.Name] = true, true
			}
		}
	}
	seen := map[[2]cableprobe.PortRef]bool{}
	for _, edge := range r.Edges {
		key := [2]cableprobe.PortRef{edge.Left, edge.Right}
		reverse := [2]cableprobe.PortRef{edge.Right, edge.Left}
		if !ports[edge.Left] || !ports[edge.Right] || edge.Left.NodeID == edge.Right.NodeID || edge.Fresh || edge.AgeMs < 0 || edge.AgeMs > 60000 || seen[key] || seen[reverse] {
			return false
		}
		seen[key] = true
	}
	return true
}

func cloneCableRun(r cableprobe.Run) cableprobe.Run {
	data, _ := json.Marshal(r)
	var out cableprobe.Run
	_ = json.Unmarshal(data, &out)
	return out
}
func (s *cableProductService) snapshot(r *cableProductRun) cableprobe.Run {
	out := cloneCableRun(r.Public)
	if record := s.cleanupRecords[r.Public.RunID]; record != nil && len(record.Attempts) > 0 {
		out.CleanupRecovery = cloneCleanupPublic(record.Attempts[len(record.Attempts)-1].Public)
	}
	if out.State == "running" {
		out.RemainingMs = max(0, min(cableprobe.Window.Milliseconds(), time.Until(r.windowAt.Add(cableprobe.Window)).Milliseconds()))
	}
	return out
}

func (s *cableProductService) ownedReceipt(nodeID string, candidate onboardingCandidate, username string) (onboardingInstallReceipt, onboardingPlatformInfo, bool) {
	s.m.onboarding.mu.Lock()
	defer s.m.onboarding.mu.Unlock()
	var receipt onboardingInstallReceipt
	var info onboardingPlatformInfo
	var newest int64
	for _, run := range s.m.onboarding.operations {
		for _, plan := range run.Plans {
			if plan.Receipt.Installed && plan.Receipt.NodeID == nodeID && plan.Candidate.Address == candidate.Address && plan.Candidate.Port == candidate.Port && plan.Username == username && run.Public.StartedAt >= newest {
				receipt, info, newest = plan.Receipt, plan.Info, run.Public.StartedAt
			}
		}
	}
	return receipt, info, receipt.Installed
}

func (s *cableProductService) access(plan cableLaunchPlan) (onboardingAccess, error) {
	s.m.onboarding.mu.Lock()
	defer s.m.onboarding.mu.Unlock()
	return s.accessLocked(plan)
}
func (s *cableProductService) accessLocked(plan cableLaunchPlan) (onboardingAccess, error) {
	target := s.m.onboarding.targets[plan.Candidate.CandidateID]
	if target == nil || target.changedKey || !target.candidate.AccessAvailable || target.accessGeneration != plan.AccessGeneration ||
		target.candidate.Address != plan.Candidate.Address || target.candidate.Port != plan.Candidate.Port || target.candidate.HostKeySHA256 != plan.Candidate.HostKeySHA256 ||
		target.candidate.AccessID != plan.Candidate.AccessID || !time.Now().Before(target.expiresAt) || target.access.user == "" || target.access.elevationPassword == "" || strings.ContainsAny(target.access.elevationPassword, "\r\n\x00") {
		return onboardingAccess{}, errors.New("reviewed device account or administrator access changed or expired")
	}
	return target.access, nil
}

func (s *cableProductService) trust(owner string, bindings map[string]*http.Client) error {
	s.m.mesh.Refresh()
	if !s.m.mesh.Clustered() || s.m.mesh.NodeUUID() != owner {
		return errors.New("PAIR controller membership changed")
	}
	for principal, prior := range bindings {
		current, ok := s.m.remoteHTTP.Client(principal)
		if !ok || current != prior {
			return errors.New("reviewed PAIR certificate changed")
		}
	}
	return nil
}

func (s *cableProductService) review(ctx context.Context, request cableProductReviewRequest) (cableprobe.Review, error) {
	if len(request.AcceptedHostKeys) > 3 {
		return cableprobe.Review{}, errors.New("at most three reviewed SSH fingerprints may be accepted")
	}
	began := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	public, err := s.m.reviewCables(ctx, request.ReviewRequest)
	if err != nil {
		return public, err
	}
	public.Permission = &cableprobe.PermissionReview{Mode: "one-shot-admin", Targets: make([]cableprobe.PermissionTarget, len(public.Targets)), Effects: []string{
		"Temporarily run the verified PAIR cable worker as administrator on exactly the selected devices.",
		"Observe the selected physical links for 20 seconds after every worker is ready. Send at most 20 fixed LLDP frames per reviewed port; close all owned probe resources afterward.",
		"Worker bytes must match the controller's running image or an existing verified PAIR package; no network settings, membership, services, drivers, models or persistent privileges are changed. Directness and RDMA remain unverified.",
	}}
	owner := s.m.mesh.NodeUUID()
	bindings := map[string]*http.Client{}
	if client, ok := s.m.remoteHTTP.Client(owner); ok {
		bindings[owner] = client
	}
	plans := make([]cableLaunchPlan, len(public.Targets))
	ready := make([]bool, len(plans))
	var wait sync.WaitGroup
	for i, target := range public.Targets {
		client, pinned := s.m.remoteHTTP.Client(target.Principal)
		if pinned {
			bindings[target.Principal] = client
		}
		row := cableprobe.PermissionTarget{NodeID: target.NodeID, Reason: target.Reason}
		address := ""
		if target.NodeID == s.m.cableLocal.nodeID {
			address = "127.0.0.1"
		} else if peer, ok := s.m.peers.lookup(target.NodeID); ok && len(peer.addresses) > 0 {
			address = peer.addresses[0]
		}
		if address != "" {
			if c, e := s.m.onboarding.addTarget(onboardingAddTargetRequest{Address: address, Port: 22, Label: target.NodeID}); e == nil {
				row.CandidateID = c.CandidateID
			}
		}
		public.Permission.Targets[i] = row
		if !pinned || target.Principal == "" || len(target.Ports) == 0 {
			continue
		}
		wait.Add(1)
		go func(i int, target cableprobe.Target, row cableprobe.PermissionTarget) {
			defer wait.Done()
			row.Reason = "Verified device access and separate administrator access are required."
			s.m.onboarding.mu.Lock()
			stored := s.m.onboarding.targets[row.CandidateID]
			var copy onboardingPrivateTarget
			if stored != nil {
				copy = *stored
			}
			s.m.onboarding.mu.Unlock()
			if stored == nil {
				public.Permission.Targets[i] = row
				return
			}
			c := copy.candidate
			row.AccessLabel = c.AccessLabel
			row.HostKeySHA256 = c.HostKeySHA256
			row.HostKeyTrusted = c.HostKeyTrusted && !copy.changedKey
			for _, accepted := range request.AcceptedHostKeys {
				if accepted.CandidateID == c.CandidateID && accepted.SHA256 != "" && accepted.SHA256 == c.HostKeySHA256 && !copy.changedKey {
					row.HostKeyTrusted = true
				}
			}
			row.AccessAvailable = c.AccessAvailable && time.Now().Before(copy.expiresAt)
			row.ElevationAvailable = row.AccessAvailable && copy.access.elevationPassword != "" && !strings.ContainsAny(copy.access.elevationPassword, "\r\n\x00")
			defer func() { public.Permission.Targets[i] = row }()
			if !row.AccessAvailable || !row.HostKeyTrusted || !row.ElevationAvailable {
				return
			}
			c.HostKeyTrusted = true
			receipt, info, owned := s.ownedReceipt(target.NodeID, c, copy.access.user)
			binding, bindingErr := s.workerBinding(ctx, target)
			if bindingErr == nil && binding.Protocol != cableWorkerProtocol {
				row.Reason = "This paired app does not support cable worker protocol v4. Update the selected app before a new check."
				return
			}
			if bindingErr != nil && !owned {
				row.Reason = "The paired app does not currently provide a verified runnable cable worker. " + cableFactsMessage(bindingErr)
				return
			}
			plan := cableLaunchPlan{NodeID: target.NodeID, Principal: target.Principal, Candidate: c, AccessGeneration: copy.accessGeneration, Receipt: receipt, Info: info}
			if bindingErr == nil {
				plan.Runtime = &binding
			}
			client, e := s.m.onboarding.dial(ctx, c, copy.access)
			if e != nil {
				row.Reason = "Reviewed SSH access could not be authenticated; no administrator action was attempted."
				return
			}
			defer client.close()
			plan.Info, e = readOnboardingPlatform(ctx, client)
			if e != nil {
				row.Reason = "The existing native device account could not be verified."
				return
			}
			plan, e = s.inspect(ctx, client, plan, s.m.mesh)
			if e != nil {
				row.Reason = "The owned worker, native account, fixed capability or paired identity could not be verified; no administrator action was attempted."
				return
			}
			plan.WorkerOrigin, e = s.knownWorker(ctx, plan)
			if e != nil {
				row.Reason = "The installed worker bytes do not match an existing trusted controller image or verified PAIR package. No administrator action was attempted."
				return
			}
			if _, e = s.access(plan); e != nil {
				row.Reason = e.Error()
				return
			}
			for _, selected := range public.Targets {
				if selected.NodeID == s.m.cableLocal.nodeID && target.NodeID != selected.NodeID {
					_, published := s.m.peers.lookup(selected.NodeID)
					observed := net.IP(nil)
					if plan.Runtime != nil {
						observed = net.ParseIP(plan.Runtime.RequesterAddress)
					}
					if !published && (observed == nil || observed.IsLoopback() || observed.IsUnspecified() || observed.IsMulticast() || s.m.cableLocal.controlPort <= 0) {
						row.Reason = "The peer-visible control route back to the selected controller is unavailable."
						return
					}
				}
			}
			row.WorkerAvailable = true
			row.WorkerSHA256 = plan.WorkerSHA256
			row.Reason = "Explicit approval is required for this finite administrator action."
			plans[i], ready[i] = plan, true
		}(i, target, row)
	}
	wait.Wait()
	public.Available = true
	seenCandidates := map[string]bool{}
	for i := range ready {
		if !ready[i] {
			public.Available = false
		}
		public.Targets[i].RawPrivilege = "approval-needed"
		row := public.Permission.Targets[i]
		public.Targets[i].Reason = row.Reason
		if row.CandidateID != "" {
			if seenCandidates[row.CandidateID] {
				return cableprobe.Review{}, errors.New("distinct selected devices resolve to the same account-access endpoint")
			}
			seenCandidates[row.CandidateID] = true
		}
	}
	public.RemainingMs = max(0, (cableprobe.ReviewLifetime - time.Since(began)).Milliseconds())
	public.Reason = "Review the selected devices, temporary administrator permission and finite effects before approving."
	if err := s.trust(owner, bindings); err != nil {
		public.Available = false
		public.Reason = err.Error()
	}
	if s.held() {
		public.Available = false
		public.Reason = "Another cable operation is active or has unconfirmed cleanup; inspect that same operation first."
	}
	if public.RemainingMs == 0 {
		public.Available = false
		public.Reason = "The review expired during prerequisite inspection; review again."
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, review := range s.reviews {
		if !time.Now().Before(review.expires) {
			delete(s.reviews, id)
		}
	}
	if len(s.reviews) >= 16 {
		return cableprobe.Review{}, errors.New("too many unexpired cable reviews")
	}
	s.reviews[public.ReviewID] = cableProductReview{public: public, selection: request.ReviewRequest, plans: plans, bindings: bindings, ownerPrincipal: owner, expires: began.Add(cableprobe.ReviewLifetime)}
	return public, nil
}

type cableStartNotStartedError struct {
	result cableprobe.StartNotStarted
}

func (e *cableStartNotStartedError) Error() string {
	return "cable check was not started; review again: " + e.result.Reason
}

// The caller holds diagnosticMu and s.mu. Only a known, unconsumed review can
// establish no effects; retiring it here prevents a later start of that review.
// Recovery uncertainty and accepted history never become a refusal result.
func (s *cableProductService) refuseCableStartLocked(ctx context.Context, review cableProductReview, reason string, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, known := s.reviews[review.public.ReviewID]
	if !known || current.ownerPrincipal != review.ownerPrincipal || !current.expires.Equal(review.expires) ||
		s.shuttingDown || s.m.exec.shuttingDown.Load() || s.heldLocked() {
		return cause
	}
	for _, run := range s.runs {
		if run.Public.ReviewID == review.public.ReviewID {
			return cause
		}
	}
	delete(s.reviews, review.public.ReviewID)
	return &cableStartNotStartedError{result: cableprobe.StartNotStarted{
		Disposition: "not-started", ReviewID: review.public.ReviewID,
		OwnerNodeID: review.public.OwnerNodeID, Reason: reason,
	}}
}

func (s *cableProductService) start(ctx context.Context, request cableprobe.StartRequest) (cableprobe.Run, error) {
	s.m.exec.diagnosticMu.Lock()
	defer s.m.exec.diagnosticMu.Unlock()
	if s.m.exec.shuttingDown.Load() {
		return cableprobe.Run{}, errors.New("cable operation owner is shutting down")
	}
	if s.m.exec.fabric != nil && s.m.exec.fabric.held() {
		return cableprobe.Run{}, errors.New("temporary fabric setup or owned-address cleanup is active")
	}
	if err := ctx.Err(); err != nil {
		return cableprobe.Run{}, err
	}
	if !request.ApproveAdmin {
		return cableprobe.Run{}, errors.New("explicit approval of the reviewed temporary administrator action is required")
	}
	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		return cableprobe.Run{}, errors.New("cable operation owner is shutting down")
	}
	for _, r := range s.runs {
		if r.Public.ReviewID == request.ReviewID {
			result := s.snapshot(r)
			s.mu.Unlock()
			return result, nil
		}
	}
	review, ok := s.reviews[request.ReviewID]
	blocked := s.heldLocked()
	if ok && !blocked && (!review.public.Available || !time.Now().Before(review.expires)) {
		reason := "review-unavailable"
		if !time.Now().Before(review.expires) {
			reason = "review-expired"
		}
		err := s.refuseCableStartLocked(ctx, review, reason, errors.New("cable review is expired or unavailable; review again"))
		s.mu.Unlock()
		return cableprobe.Run{}, err
	}
	s.mu.Unlock()
	if !ok || blocked {
		return cableprobe.Run{}, errors.New("cable review is expired, unavailable or held; review again")
	}
	if err := s.trust(review.ownerPrincipal, review.bindings); err != nil {
		s.mu.Lock()
		refusal := s.refuseCableStartLocked(ctx, review, "trust-changed", err)
		s.mu.Unlock()
		return cableprobe.Run{}, refusal
	}
	for _, plan := range review.plans {
		if _, err := s.access(plan); err != nil {
			s.mu.Lock()
			refusal := s.refuseCableStartLocked(ctx, review, "access-changed", err)
			s.mu.Unlock()
			return cableprobe.Run{}, refusal
		}
	}
	// Consume the immutable reviewed manifest. Every selected worker validates
	// its own native aliases before arming; cached global inventory is not a
	// second approval authority for those same physical-port bindings.
	s.m.onboarding.mu.Lock()
	defer s.m.onboarding.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shuttingDown {
		return cableprobe.Run{}, errors.New("cable operation owner is shutting down")
	}
	for _, r := range s.runs {
		if r.Public.ReviewID == request.ReviewID {
			return s.snapshot(r), nil
		}
	}
	if s.heldLocked() || s.m.onboarding.active != "" || len(s.runs) >= 128 {
		return cableprobe.Run{}, errors.New("another owned operation, expired review or retained-operation limit blocks this approval")
	}
	if !time.Now().Before(review.expires) {
		return cableprobe.Run{}, s.refuseCableStartLocked(ctx, review, "review-expired", errors.New("cable review expired before approval"))
	}
	for _, plan := range review.plans {
		if _, err := s.accessLocked(plan); err != nil {
			return cableprobe.Run{}, s.refuseCableStartLocked(ctx, review, "access-changed", err)
		}
	}
	if err := s.trust(review.ownerPrincipal, review.bindings); err != nil {
		return cableprobe.Run{}, s.refuseCableStartLocked(ctx, review, "trust-changed", err)
	}
	if err := ctx.Err(); err != nil {
		return cableprobe.Run{}, err
	}
	runCtx, cancel := context.WithTimeout(s.ctx, 60*time.Second)
	r := &cableProductRun{Public: cableprobe.Run{RunID: newOpID(), ReviewID: request.ReviewID, OwnerNodeID: review.public.OwnerNodeID, Revision: 1, State: "preparing", Targets: cloneCableRun(cableprobe.Run{Targets: review.public.Targets}).Targets, Edges: []cableprobe.Edge{}, Result: "incomplete", Directness: "unverified", RemainingMs: cableprobe.Window.Milliseconds(), CleanupConfirmed: false, StartedAt: time.Now().UnixMilli(), Message: "Approved temporary administrator cable check; preparing the exact selected participants."}, OwnerPrincipal: review.ownerPrincipal, Plans: review.plans, AdminApproved: true, bindings: review.bindings, cancel: cancel, done: make(chan struct{})}
	r.Public.Diagnostics = &cableprobe.RunDiagnostics{Participants: make([]cableprobe.ParticipantDiagnostic, len(r.Public.Targets))}
	for i := range r.Public.Targets {
		r.Public.Diagnostics.Participants[i] = cableprobe.ParticipantDiagnostic{NodeID: r.Public.Targets[i].NodeID, Phase: "preparing"}
		r.Public.Targets[i].RawPrivilege = "unknown"
		r.Public.Targets[i].Reason = "Administrator approval is recorded; waiting for this finite worker to arm."
	}
	if err := s.save(r); err != nil {
		cancel()
		return cableprobe.Run{}, errors.New("cable approval could not be retained before launch")
	}
	s.runs[r.Public.RunID] = r
	go s.execute(runCtx, r, review)
	return s.snapshot(r), nil
}

func (m *Manager) runCableProduct(ctx context.Context, msg *Message) {
	var result cableprobe.Run
	var err error
	switch msg.Method {
	case "engine:cable-start":
		var request cableprobe.StartRequest
		if onboardingDecode(msg.Params, &request) != nil {
			m.codec.RespondError(msg.ID, -32602, "invalid cable approval")
			return
		}
		result, err = m.cables.start(ctx, request)
		var refusal *cableStartNotStartedError
		if errors.As(err, &refusal) {
			m.codec.Respond(msg.ID, refusal.result)
			return
		}
	case "engine:cable-status":
		var request cableprobe.StatusRequest
		if onboardingDecode(msg.Params, &request) != nil {
			m.codec.RespondError(msg.ID, -32602, "invalid cable status identity")
			return
		}
		result, err = m.cables.status(request)
	case "engine:cable-cancel":
		var request struct {
			RunID string `json:"runId"`
		}
		if onboardingDecode(msg.Params, &request) != nil || !cableIdentifier(request.RunID, 128) {
			m.codec.RespondError(msg.ID, -32602, "invalid cable cancellation identity")
			return
		}
		result, err = m.cables.cancelRun(request.RunID)
	}
	m.respondOrErr(msg, result, err)
}

func (s *cableProductService) status(request cableprobe.StatusRequest) (cableprobe.Run, error) {
	if (request.RunID == "") == (request.ReviewID == "") {
		return cableprobe.Run{}, errors.New("choose one cable run or review identity")
	}
	node, principal, err := s.retainedOwner()
	if err != nil {
		return cableprobe.Run{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, run := range s.runs {
		if (request.RunID != "" && request.RunID == run.Public.RunID) || (request.ReviewID != "" && request.ReviewID == run.Public.ReviewID) {
			if run.OwnerPrincipal != principal || run.Public.OwnerNodeID != node {
				return cableprobe.Run{}, errors.New("retained cable run belongs to another controller identity")
			}
			result := s.snapshot(run)
			currentNode, currentPrincipal, err := s.retainedOwner()
			if err != nil || currentNode != node || currentPrincipal != principal {
				return cableprobe.Run{}, errors.New("cable controller identity changed during retained-run loading")
			}
			return result, nil
		}
	}
	return cableprobe.Run{}, errors.New("cable operation is unknown; no run or cleanup was inferred")
}
func (s *cableProductService) cancelRun(id string) (cableprobe.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[id]
	if r == nil {
		return cableprobe.Run{}, errors.New("cable run is unknown; cleanup is not confirmed")
	}
	if r.cancel != nil && (r.Public.State == "preparing" || r.Public.State == "running") {
		r.Public.State = "cancelling"
		r.Public.Message = "Cancelling the same owned workers and awaiting cleanup acknowledgements."
		r.Public.Revision++
		r.Public.RemainingMs = 0
		if s.save(r) != nil {
			s.recoveryFailed = true
		}
		r.cancel()
	}
	return s.snapshot(r), nil
}
func (s *cableProductService) shutdown() {
	s.mu.Lock()
	s.shuttingDown = true
	var done []chan struct{}
	if s.cleanupCancel != nil {
		s.cleanupCancel()
	}
	if s.cleanupDone != nil {
		done = append(done, s.cleanupDone)
	}
	for _, r := range s.runs {
		if r.cancel != nil {
			r.cancel()
		}
		if r.done != nil {
			done = append(done, r.done)
		}
	}
	s.mu.Unlock()
	for _, ch := range done {
		<-ch
	}
}

func (s *cableProductService) workerRequest(r *cableProductRun, review cableProductReview, plan cableLaunchPlan, marker string) (cableProbeOnceRequest, error) {
	request := cableProbeOnceRequest{Protocol: cableWorkerProtocol, Review: review.public, RunID: r.Public.RunID, Marker: marker}
	request.Review.Available = false
	request.Review.Permission = nil
	request.Review.OwnerNodeID = plan.NodeID
	request.Review.RemainingMs = max(0, min(30000, time.Until(review.expires).Milliseconds()))
	for _, target := range review.public.Targets {
		if target.NodeID == plan.NodeID {
			continue
		}
		peer, ok := s.m.peers.lookup(target.NodeID)
		if target.NodeID == s.m.cableLocal.nodeID && plan.Runtime != nil {
			ip := net.ParseIP(plan.Runtime.RequesterAddress)
			if ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsMulticast() && s.m.cableLocal.controlPort > 0 {
				peer, ok = ecPeer{nodeID: target.NodeID, clusterUUID: target.Principal, addresses: []string{ip.String()}, port: s.m.cableLocal.controlPort}, true
			}
		}
		if !ok || peer.clusterUUID != target.Principal || len(peer.addresses) == 0 {
			return request, errors.New("reviewed participant is no longer discoverable")
		}
		request.Peers = append(request.Peers, struct {
			NodeID      string `json:"nodeId"`
			Principal   string `json:"principal"`
			Address     string `json:"address"`
			ControlPort int    `json:"controlPort"`
		}{target.NodeID, target.Principal, peer.addresses[0], peer.port})
	}
	return request, nil
}

func (s *cableProductService) execute(ctx context.Context, r *cableProductRun, review cableProductReview) {
	defer close(r.done)
	workerCtx, workerCancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer workerCancel()
	type participant struct {
		worker          *cableWorker
		client          *onboardingSSH
		attempted       bool
		prearmFailed    bool
		message         cableWorkerMessage
		err             error
		armExpires      time.Time
		cancelTransport context.CancelFunc
		diagnostic      cableprobe.ParticipantDiagnostic
	}
	participants := make([]participant, len(r.Plans))
	var wait sync.WaitGroup
	nonce := make([]byte, 16)
	_, nonceErr := rand.Read(nonce)
	marker := hex.EncodeToString(nonce)
	for i, plan := range r.Plans {
		wait.Add(1)
		go func(i int, plan cableLaunchPlan) {
			defer wait.Done()
			p := &participants[i]
			p.diagnostic = cableprobe.ParticipantDiagnostic{NodeID: plan.NodeID, Phase: "preparing"}
			if p.err = ctx.Err(); p.err != nil {
				latchCableFailure(&p.diagnostic, "preparing", "cancelled", p.err)
				return
			}
			if nonceErr != nil {
				p.err = nonceErr
				latchCableFailure(&p.diagnostic, "preparing", "random-source-unavailable", p.err)
				return
			}
			p.diagnostic.Phase = "access"
			access, err := s.access(plan)
			if err != nil {
				p.err = err
				latchCableFailure(&p.diagnostic, "access", "access-unavailable", err)
				return
			}
			p.diagnostic.Phase = "routing"
			request, err := s.workerRequest(r, review, plan, marker)
			if err != nil {
				p.err = err
				latchCableFailure(&p.diagnostic, "routing", "participant-unavailable", err)
				return
			}
			p.diagnostic.Phase = "trust"
			if err = s.trust(review.ownerPrincipal, review.bindings); err != nil {
				p.err = err
				latchCableFailure(&p.diagnostic, "trust", "trust-changed", err)
				return
			}
			transportCtx, transportCancel := context.WithCancel(workerCtx)
			p.cancelTransport = transportCancel
			stopPending := context.AfterFunc(ctx, transportCancel)
			p.diagnostic.Phase = "ssh"
			p.client, p.err = s.m.onboarding.dial(transportCtx, plan.Candidate, access)
			stopPending()
			if p.err != nil {
				latchCableFailure(&p.diagnostic, "ssh", "ssh-unavailable", p.err)
				return
			}
			if p.err = ctx.Err(); p.err != nil {
				latchCableFailure(&p.diagnostic, "ssh", "cancelled", p.err)
				return
			}
			beganLaunch := time.Now()
			p.attempted = true // Includes a lost launch/armed response in cleanup accounting.
			p.diagnostic.Phase = "launch"
			stopPending = context.AfterFunc(ctx, transportCancel)
			p.worker, p.err = s.launch(transportCtx, p.client, plan, request, access.elevationPassword)
			stopPending()
			access = onboardingAccess{}
			if p.err != nil {
				latchCableFailure(&p.diagnostic, "launch", "launch-failed", p.err)
				return
			}
			p.diagnostic.Phase = "arm"
			p.message, p.err = p.worker.read(ctx)
			if p.err == nil && validCablePrearmWorkerMessage(p.message, r.Public.RunID, r.Public.ReviewID, r.Public.Targets) {
				p.prearmFailed = true
				captureCablePrearm(&p.diagnostic, p.message)
				p.err = errors.New("participant preparation failed before arm")
				return
			}
			if p.err == nil && (p.message.State != "armed" || p.message.RunID != r.Public.RunID || p.message.ReviewID != r.Public.ReviewID || p.message.RemainingMs <= 0 || p.message.RemainingMs > 30000) {
				p.err = errors.New("participant did not acknowledge this unexpired arm")
			}
			if p.err != nil {
				latchCableFailure(&p.diagnostic, "arm", "arm-invalid", p.err)
			}
			p.armExpires = beganLaunch.Add(time.Duration(p.message.RemainingMs) * time.Millisecond)
		}(i, plan)
	}
	wait.Wait()
	var operationErr error
	var operationFailure *cableprobe.Failure
	recordFailure := func(phase, code string, err error) {
		if err != nil && operationFailure == nil {
			operationFailure = cableFailure(phase, code, err)
		}
	}
	for _, p := range participants {
		operationErr = errors.Join(operationErr, p.err)
		recordFailure(p.diagnostic.Phase, p.diagnostic.Code, p.err)
	}
	if operationErr == nil {
		operationErr = s.trust(review.ownerPrincipal, review.bindings)
		recordFailure("trust", "trust-changed", operationErr)
	}
	if operationErr == nil {
		operationErr = ctx.Err()
		recordFailure("barrier", "cancelled", operationErr)
	}
	armCurrent := func() error {
		if err := ctx.Err(); err != nil {
			recordFailure("barrier", "cancelled", err)
			return err
		}
		if !time.Now().Before(review.expires) {
			err := errors.New("review expired before all starts")
			recordFailure("barrier", "review-expired", err)
			return err
		}
		for i := range participants {
			p := &participants[i]
			if !time.Now().Before(p.armExpires) {
				err := errors.New("a participant arm expired before the barrier completed")
				latchCableFailure(&p.diagnostic, "barrier", "arm-expired", err)
				recordFailure("barrier", "arm-expired", err)
				return err
			}
		}
		for i, plan := range r.Plans {
			if _, err := s.access(plan); err != nil {
				latchCableFailure(&participants[i].diagnostic, "access", "access-unavailable", err)
				recordFailure("access", "access-unavailable", err)
				return err
			}
		}
		err := s.trust(review.ownerPrincipal, review.bindings)
		recordFailure("trust", "trust-changed", err)
		return err
	}
	if operationErr == nil {
		operationErr = armCurrent()
	}
	if operationErr == nil {
		s.mu.Lock()
		if ctx.Err() != nil || r.Public.State == "cancelling" {
			operationErr = context.Canceled
			recordFailure("barrier", "cancelled", operationErr)
		} else {
			r.Public.State = "running"
			r.Public.Revision++
			r.windowAt = time.Now()
			r.Public.Message = "All reviewed participants armed; observing the finite current run."
			for i := range r.Public.Targets {
				r.Public.Diagnostics.Participants[i] = participants[i].diagnostic
				r.Public.Targets[i].RawPrivilege = "present"
				r.Public.Targets[i].Reason = "The owned finite worker is armed under this approved run."
			}
			operationErr = s.save(r)
			recordFailure("retention", "receipt-write-failed", operationErr)
		}
		s.mu.Unlock()
		if operationErr == nil {
			for i := range participants {
				if err := armCurrent(); err != nil {
					operationErr = err
					break
				}
				if err := participants[i].worker.send(cableProbeCommand{RunID: r.Public.RunID, Command: "start"}); err != nil {
					operationErr = err
					latchCableFailure(&participants[i].diagnostic, "start", "start-unconfirmed", err)
					recordFailure("start", "start-unconfirmed", err)
					break
				}
				participants[i].diagnostic.Phase = "observation"
			}
		}
	}
	if operationErr == nil {
		// Revoked access or pairing cancels this operation's workers promptly.
		// Transport contexts stay alive for the separate cleanup acknowledgement.
		monitorStop, monitorDone := make(chan struct{}), make(chan struct{})
		type policyResult struct {
			err     error
			failure *cableprobe.Failure
		}
		policyFailure := make(chan policyResult, 1)
		cancelOperation := r.cancel
		go func() {
			defer close(monitorDone)
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-monitorStop:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					phase, code := "trust", "trust-changed"
					err := s.trust(review.ownerPrincipal, review.bindings)
					if err == nil {
						for _, plan := range r.Plans {
							if _, err = s.access(plan); err != nil {
								phase, code = "access", "access-unavailable"
								break
							}
						}
					}
					if err != nil {
						policyFailure <- policyResult{err: err, failure: cableFailure(phase, code, err)}
						cancelOperation()
						return
					}
				}
			}
		}()
		for i := range participants {
			wait.Add(1)
			go func(i int) {
				defer wait.Done()
				participants[i].message, participants[i].err = participants[i].worker.read(ctx)
				if participants[i].err != nil {
					latchCableFailure(&participants[i].diagnostic, "observation", "worker-output-invalid", participants[i].err)
				} else {
					captureCableTerminal(&participants[i].diagnostic, participants[i].message, r.Public.RunID, r.Public.ReviewID)
				}
			}(i)
		}
		wait.Wait()
		close(monitorStop)
		<-monitorDone
		select {
		case policyErr := <-policyFailure:
			operationErr = policyErr.err
			recordFailure(policyErr.failure.Phase, policyErr.failure.Code, policyErr.err)
		default:
		}
		for _, p := range participants {
			operationErr = errors.Join(operationErr, p.err)
			recordFailure(p.diagnostic.Phase, p.diagnostic.Code, p.err)
			if p.err == nil && p.message.State != "completed" {
				err := errors.New("a cable participant did not complete")
				operationErr = errors.Join(operationErr, err)
				recordFailure(p.diagnostic.Phase, p.diagnostic.Code, err)
			}
		}
	}
	clean := true
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cleanupCancel()
	for i := range participants {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			p := &participants[i]
			if p.worker != nil {
				if !p.prearmFailed && (p.message.State == "armed" || p.err != nil) {
					_ = p.worker.send(cableProbeCommand{RunID: r.Public.RunID, Command: "cancel"})
					_ = p.worker.closeInput()
					p.message, p.err = p.worker.read(cleanupCtx)
				}
				p.err = errors.Join(p.err, p.worker.closeInput(), p.worker.wait(cleanupCtx))
				if p.err != nil {
					latchCableFailure(&p.diagnostic, "cleanup", "cleanup-unconfirmed", p.err)
				}
				p.worker.close()
			}
			if p.client != nil {
				p.client.close()
			}
			if p.cancelTransport != nil {
				p.cancelTransport()
			}
		}(i)
	}
	wait.Wait()
	for i := range participants {
		p := &participants[i]
		p.diagnostic.CleanupConfirmed = !p.attempted || (p.worker != nil && p.err == nil && p.message.RunID == r.Public.RunID && p.message.ReviewID == r.Public.ReviewID && p.message.Directness == "unverified" && p.message.CleanupConfirmed && (p.message.State == "completed" || p.message.State == "cancelled" || p.message.State == "failed"))
		if !p.diagnostic.CleanupConfirmed {
			clean = false
			latchCableFailure(&p.diagnostic, "cleanup", "cleanup-unconfirmed", nil)
		}
		operationErr = errors.Join(operationErr, p.err)
		recordFailure(p.diagnostic.Phase, p.diagnostic.Code, p.err)
	}
	if err := s.trust(review.ownerPrincipal, review.bindings); err != nil {
		operationErr = errors.Join(operationErr, err)
		recordFailure("final-validation", "trust-changed", err)
	}
	policyChanged := false
	for i, plan := range r.Plans {
		if _, err := s.access(plan); err != nil {
			operationErr = errors.Join(operationErr, err)
			latchCableFailure(&participants[i].diagnostic, "final-validation", "access-unavailable", err)
			recordFailure("final-validation", "access-unavailable", err)
			policyChanged = true
		}
	}
	if s.trust(review.ownerPrincipal, review.bindings) != nil {
		policyChanged = true
	}
	edges := []cableprobe.Edge{}
	verdict := "incomplete"
	if operationErr == nil && clean {
		messages := make([]cableWorkerMessage, len(participants))
		for i, p := range participants {
			messages[i] = p.message
		}
		edges, verdict = reciprocalCableResult(review.public.Targets, messages)
	}
	s.mu.Lock()
	cancelled := !policyChanged && (r.Public.State == "cancelling" || errors.Is(ctx.Err(), context.Canceled))
	r.Public.State = "completed"
	r.Public.Result = verdict
	r.Public.Edges = edges
	r.Public.Message = "Finite cable check completed; observations are historical and directness/RDMA remain unverified."
	if verdict == "reciprocal-observations" {
		r.Public.Message = "The approved finite check observed reciprocal traffic on every reviewed physical port and confirmed cleanup. This is historical cable evidence; directness and RDMA remain unverified."
	}
	if verdict == "incomplete" {
		r.Public.Message = "The finite check ended without reciprocal observations for every reviewed physical port. No complete cable match is claimed."
	}
	if verdict == "ambiguous" {
		r.Public.Message = "The finite check observed conflicting physical-port partners. The result is ambiguous; no cable match is claimed."
	}
	if operationErr != nil {
		r.Public.State = "failed"
		r.Public.Message = "A reviewed participant, launch, trust check or bounded worker result failed; no reciprocal pass is claimed."
	}
	if cancelled {
		r.Public.State = "cancelled"
		r.Public.Result = "incomplete"
		r.Public.Edges = []cableprobe.Edge{}
		r.Public.Message = "The same owned cable run was cancelled."
	}
	if !clean {
		r.Public.State = "failed"
		r.Public.Message = "Owned worker cleanup is not confirmed; another cable operation remains held."
	}
	r.Public.CleanupConfirmed = clean
	r.Public.Topology = cableTopologyResult(r.Public)
	r.Public.RemainingMs = 0
	r.Public.FreshnessRemainingMs = 0
	r.Public.FinishedAt = max(time.Now().UnixMilli(), r.Public.StartedAt)
	r.Public.Revision++
	for i := range r.Public.Targets {
		row := participants[i].diagnostic
		if row.Code == "" && row.WorkerState == "completed" && row.CleanupConfirmed {
			row.Phase = "completed"
		}
		r.Public.Diagnostics.Participants[i] = row
		r.Public.Targets[i].RawPrivilege = "approval-needed"
		r.Public.Targets[i].Reason = "The temporary operation is closed; another run requires fresh reviewed approval."
		if !clean {
			r.Public.Targets[i].RawPrivilege = "unknown"
			r.Public.Targets[i].Reason = "Worker cleanup is not confirmed; another run remains held."
		}
	}
	if operationFailure == nil && !clean {
		operationFailure = &cableprobe.Failure{Phase: "cleanup", Code: "cleanup-unconfirmed"}
	}
	r.Public.Diagnostics.Failure = operationFailure
	if s.save(r) != nil {
		s.recoveryFailed = true
		r.Public.State = "failed"
		r.Public.Topology = cableTopologyResult(r.Public)
		r.Public.Message = "Terminal cable receipt could not be retained; recovery is required."
		if r.Public.Diagnostics.Failure == nil {
			r.Public.Diagnostics.Failure = &cableprobe.Failure{Phase: "retention", Code: "receipt-write-failed"}
		}
	}
	r.cancel = nil
	s.mu.Unlock()
	s.m.onboarding.mu.Lock()
	for _, plan := range r.Plans {
		if t := s.m.onboarding.targets[plan.Candidate.CandidateID]; t != nil && t.accessGeneration == plan.AccessGeneration {
			t.access = onboardingAccess{}
			t.candidate.AccessAvailable = false
			t.candidate.Reason = "Cable operation ended; temporary account access was released."
		}
	}
	s.m.onboarding.mu.Unlock()
}

func reciprocalCableResult(targets []cableprobe.Target, messages []cableWorkerMessage) ([]cableprobe.Edge, string) {
	known := map[cableprobe.PortRef]bool{}
	for _, t := range targets {
		for _, p := range t.Ports {
			known[cableprobe.PortRef{NodeID: t.NodeID, SwitchID: p.SwitchID, PortName: p.PortName}] = true
		}
	}
	type direction struct{ left, right cableprobe.PortRef }
	observed := map[direction]int64{}
	if len(messages) != len(targets) {
		return []cableprobe.Edge{}, "incomplete"
	}
	for i, m := range messages {
		if m.State != "completed" || !m.CleanupConfirmed || m.Directness != "unverified" || m.Sent < 0 || m.Sent > 40 || m.Received < 0 || m.Received > 256 || len(m.Observations) > 6 {
			return []cableprobe.Edge{}, "incomplete"
		}
		for _, o := range m.Observations {
			if o.Local.NodeID != targets[i].NodeID || o.Peer.NodeID == o.Local.NodeID || !known[o.Local] || !known[o.Peer] || o.AgeMs < 0 || o.AgeMs > 60000 || o.Sequence == 0 || o.Sequence > 100 {
				return []cableprobe.Edge{}, "incomplete"
			}
			key := direction{o.Local, o.Peer}
			if _, duplicate := observed[key]; duplicate {
				return []cableprobe.Edge{}, "ambiguous"
			}
			observed[key] = o.AgeMs
		}
	}
	degree := map[cableprobe.PortRef]int{}
	edges := []cableprobe.Edge{}
	partners := map[cableprobe.PortRef]map[cableprobe.PortRef]bool{}
	for key := range observed {
		if partners[key.left] == nil {
			partners[key.left] = map[cableprobe.PortRef]bool{}
		}
		if partners[key.right] == nil {
			partners[key.right] = map[cableprobe.PortRef]bool{}
		}
		partners[key.left][key.right] = true
		partners[key.right][key.left] = true
		if len(partners[key.left]) > 1 || len(partners[key.right]) > 1 {
			return []cableprobe.Edge{}, "ambiguous"
		}
	}
	for key, age := range observed {
		if other, ok := observed[direction{key.right, key.left}]; ok && key.left.NodeID < key.right.NodeID {
			edges = append(edges, cableprobe.Edge{Left: key.left, Right: key.right, AgeMs: max(age, other), Fresh: false})
			degree[key.left]++
			degree[key.right]++
		}
	}
	for port := range known {
		if degree[port] > 1 {
			return []cableprobe.Edge{}, "ambiguous"
		}
		if degree[port] == 0 {
			return edges, "incomplete"
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		return edges[i].Left.NodeID+edges[i].Left.SwitchID+edges[i].Left.PortName < edges[j].Left.NodeID+edges[j].Left.SwitchID+edges[j].Left.PortName
	})
	return edges, "reciprocal-observations"
}

// Completed window observations describe physical partners at the last check.
// A failed collector never becomes evidence of a missing or incorrect cable.
func cableTopologyResult(run cableprobe.Run) *cableprobe.Topology {
	layout, portsPerNode, expectedEdges := "direct", 1, 1
	if len(run.Targets) == 3 {
		layout, portsPerNode, expectedEdges = "ring", 2, 3
	}
	out := &cableprobe.Topology{Layout: layout, Status: "unavailable"}
	if len(run.Targets) < 2 || len(run.Targets) > 3 || run.State != "completed" || !run.CleanupConfirmed {
		return out
	}
	if run.Result == "ambiguous" {
		out.Status = "unexpected"
		return out
	}
	if run.Result != "incomplete" && run.Result != "reciprocal-observations" {
		return out
	}
	for _, target := range run.Targets {
		if len(target.Ports) != portsPerNode {
			return out
		}
	}
	out.Status = "missing"
	pairs := map[[2]string]bool{}
	for _, edge := range run.Edges {
		a, b := edge.Left.NodeID, edge.Right.NodeID
		if a > b {
			a, b = b, a
		}
		pair := [2]string{a, b}
		if a == b || pairs[pair] {
			out.Status = "unexpected"
			return out
		}
		pairs[pair] = true
	}
	if len(run.Edges) > expectedEdges {
		out.Status = "unexpected"
	} else if run.Result == "reciprocal-observations" && len(run.Edges) == expectedEdges {
		out.Status = "matched"
	}
	return out
}
