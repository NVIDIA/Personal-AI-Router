// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"nvpair-shared/cableprobe"
)

type fabricReviewRecord struct {
	public      fabricReview
	access      cableProductReview
	inventories []fabricInventory
	expires     time.Time
}
type fabricRunRecord struct {
	AdministratorApproved     bool                 `json:"administratorApproved"`
	SelectedPortPauseApproved bool                 `json:"selectedPortPauseApproved,omitempty"`
	Public                    fabricOperation      `json:"operation"`
	OwnerPrincipal            string               `json:"ownerPrincipal"`
	Plans                     []cableLaunchPlan    `json:"plans"`
	Attempted                 []bool               `json:"attempted"`
	Reserved                  []bool               `json:"reserved"`
	Failure                   string               `json:"failure,omitempty"`
	ConsumerLease             *fabricConsumerLease `json:"consumerLease,omitempty"`
	// TransferHolds maps controller-minted attempt IDs to exact copy identities.
	TransferHolds   map[string]fabricTransferHold `json:"transferHolds,omitempty"`
	cancel          context.CancelFunc
	done            chan struct{}
	rollbackWaiters int
}

// fabricRollbackFailure is rendered into the existing bounded public Message.
// No raw provider error or new durable wire field crosses that boundary.
type fabricRollbackFailure struct {
	NodeID string
	Stage  string
	Code   string
}
type fabricReservation struct {
	OperationID    string       `json:"operationId"`
	OwnerPrincipal string       `json:"ownerPrincipal"`
	Target         fabricTarget `json:"target"`
}

const (
	fabricRollbackAccessUnavailable = "Rollback access is unavailable or expired; refresh temporary account access and explicitly review cleanup again. Applied addresses were preserved."
	fabricRollbackAdmissionRefused  = "Rollback was refused because a participant is busy or its availability is unknown; applied addresses were preserved."
	fabricRollbackAdmissionChecking = "Checking every participant is idle before exact owned-address rollback."
	maxFabricRollbackCauseScan      = 2048
	maxFabricRollbackFailureMessage = 512
	maxFabricRecordBytes            = 64 << 10
	fabricRollbackReleaseTimeout    = 15 * time.Second
)

func validFabricRollbackFailureCode(stage, code string) bool {
	switch code {
	case "cancelled", "deadline-exceeded", "identity-unavailable", "response-invalid":
		return stage == "reserve-rollback" || stage == "release"
	case "admission-unavailable", "provider-unavailable":
		return stage == "reserve-rollback"
	case "release-unconfirmed":
		return stage == "release"
	}
	return false
}

func fabricRollbackFailureCode(stage string, err error) string {
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline-exceeded"
	}
	detail := err.Error()
	if len(detail) > maxFabricRollbackCauseScan {
		detail = detail[:maxFabricRollbackCauseScan]
	}
	detail = strings.ToLower(detail)
	if strings.Contains(detail, "pinned") || strings.Contains(detail, "identity changed") || strings.Contains(detail, "target identity mismatch") || strings.Contains(detail, "paired caller") {
		return "identity-unavailable"
	}
	if strings.Contains(detail, "response binding mismatch") || strings.Contains(detail, "invalid exact fabric reservation") {
		return "response-invalid"
	}
	if stage == "release" {
		return "release-unconfirmed"
	}
	if strings.Contains(detail, "admission") || strings.Contains(detail, "reservation") || strings.Contains(detail, "managed ") || strings.Contains(detail, "workload") || strings.Contains(detail, "busy") || strings.Contains(detail, "active or unconfirmed") || strings.Contains(detail, "requires cleanup") || strings.Contains(detail, "http 409") {
		return "admission-unavailable"
	}
	return "provider-unavailable"
}

func fabricRollbackFailureMessage(fabricFailure fabricRollbackFailure) string {
	return "Rollback " + fabricFailure.Stage + " failed on participant " + fabricFailure.NodeID + " (" + fabricFailure.Code + "); owned addresses were preserved. Reconcile this same operation."
}

func parseFabricRollbackFailureMessage(message string, targets []fabricTarget) (fabricRollbackFailure, bool) {
	if len(message) > maxFabricRollbackFailureMessage {
		return fabricRollbackFailure{}, false
	}
	rest, ok := strings.CutPrefix(message, "Rollback ")
	if !ok {
		return fabricRollbackFailure{}, false
	}
	stage, rest, ok := strings.Cut(rest, " failed on participant ")
	if !ok {
		return fabricRollbackFailure{}, false
	}
	nodeID, rest, ok := strings.Cut(rest, " (")
	if !ok {
		return fabricRollbackFailure{}, false
	}
	code, ok := strings.CutSuffix(rest, "); owned addresses were preserved. Reconcile this same operation.")
	if !ok || !validFabricRollbackFailureCode(stage, code) {
		return fabricRollbackFailure{}, false
	}
	matched := false
	for _, target := range targets {
		matched = matched || target.NodeID == nodeID
	}
	failure := fabricRollbackFailure{NodeID: nodeID, Stage: stage, Code: code}
	return failure, matched && fabricRollbackFailureMessage(failure) == message
}

func fabricRollbackBlockedMessage(message string) bool {
	return message == fabricRollbackAccessUnavailable || message == fabricRollbackAdmissionRefused
}

type fabricService struct {
	m              *Manager
	ctx            context.Context
	mu             sync.Mutex
	reviews        map[string]fabricReviewRecord
	runs           map[string]*fabricRunRecord
	qualified      map[string]string // process-local proof; intentionally empty after restart
	reservation    *fabricReservation
	recoveryFailed bool
	shuttingDown   bool
	// These seams replace only bounded transport/native I/O in tests.
	inventory     func(context.Context, string) (fabricInventory, error)
	worker        func(context.Context, cableLaunchPlan, fabricWorkerRequest) (fabricWorkerResult, error)
	control       func(context.Context, fabricTarget, fabricControlRequest) (fabricControlResult, error)
	refreshAccess func(context.Context, *fabricRunRecord) ([]cableLaunchPlan, error)
	testSave      func(*fabricRunRecord) error
}

func newFabricService(m *Manager) *fabricService {
	s := &fabricService{m: m, ctx: context.Background(), reviews: map[string]fabricReviewRecord{}, runs: map[string]*fabricRunRecord{}, qualified: map[string]string{}}
	s.inventory = s.readInventory
	s.worker = s.runWorker
	s.control = s.callControl
	s.refreshAccess = s.refreshRollbackPlans
	s.load()
	return s
}
func (s *fabricService) held() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.heldLocked()
}
func (s *fabricService) heldLocked() bool {
	if s.shuttingDown || s.recoveryFailed || s.reservation != nil {
		return true
	}
	for _, r := range s.runs {
		if !r.Public.CleanupConfirmed && r.Public.State != "active" {
			return true
		}
	}
	return false
}

func (s *fabricService) closeAdmission() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.shuttingDown = true
	s.mu.Unlock()
}

func (s *fabricService) file(id string) string {
	return filepath.Join(s.m.exec.baseDir, "fabric-operations", id+".json")
}
func (s *fabricService) save(r *fabricRunRecord) error {
	if !onboardingID.MatchString(r.Public.OperationID) {
		return errors.New("invalid fabric operation identity")
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil || len(raw) > maxFabricRecordBytes {
		return errors.New("fabric operation record exceeds retained decode limit")
	}
	if s.testSave != nil {
		if err := s.testSave(r); err != nil {
			return err
		}
	}
	if e := os.MkdirAll(filepath.Dir(s.file(r.Public.OperationID)), 0700); e != nil {
		return e
	}
	return writeJSONAtomic(s.file(r.Public.OperationID), r)
}
func (s *fabricService) reservationFile() string {
	return filepath.Join(s.m.exec.baseDir, "fabric-reservation.json")
}
func (s *fabricService) load() {
	if _, e := os.Lstat(s.reservationFile()); e == nil {
		raw, e := readOnboardingFile(s.reservationFile(), 64<<10)
		var r fabricReservation
		if e != nil || onboardingDecode(raw, &r) != nil || !onboardingID.MatchString(r.OperationID) || !cableIdentifier(r.OwnerPrincipal, 256) || len(r.Target.Interfaces) != 2 {
			s.recoveryFailed = true
		} else {
			s.reservation = &r
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		s.recoveryFailed = true
	}
	entries, e := os.ReadDir(filepath.Join(s.m.exec.baseDir, "fabric-operations"))
	if errors.Is(e, os.ErrNotExist) {
		return
	}
	if e != nil || len(entries) > 128 {
		s.recoveryFailed = true
		return
	}
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !onboardingID.MatchString(id) || entry.Name() != id+".json" {
			s.recoveryFailed = true
			continue
		}
		raw, e := readOnboardingFile(s.file(id), 128<<10)
		var r fabricRunRecord
		if e != nil || onboardingDecode(raw, &r) != nil || r.Public.OperationID != id || !validFabricRecord(r) {
			s.recoveryFailed = true
			continue
		}
		if !r.Public.CleanupConfirmed && (r.Public.State != "active" || r.Public.EffectsUnconfirmed || slices.Contains(r.Reserved, true)) {
			r.Public.State = "recovery-required"
			r.Public.CandidateIPs = nil
			r.Public.QualifiedAt = 0
			r.Public.QualificationDigest = ""
			if _, retained := parseFabricRollbackFailureMessage(r.Public.Message, r.Public.Targets); !retained {
				r.Public.Message = "PAIR restarted; confirm the same operation's exact owned-address cleanup before another setup."
			}
		}
		s.runs[id] = &r
	}
}

func validFabricRecord(r fabricRunRecord) bool {
	op := r.Public
	leaseState := op.State == "active" || op.State == "recovery-required"
	if r.ConsumerLease != nil && (!validFabricConsumerLease(*r.ConsumerLease) || !leaseState || op.RecipeID != fabricRecipe && op.RecipeID != fabricRingRecipe) {
		return false
	}
	ring := op.RecipeID == fabricRingRecipe
	participants := 2
	prefixBits := 30
	if ring {
		participants, prefixBits = 3, 31
		if !onboardingID.MatchString(op.CableRunID) {
			return false
		}
	} else if op.RecipeID != "" && op.RecipeID != fabricRecipe || op.CableRunID != "" {
		return false
	}
	if failure := op.Failure; failure != nil {
		matched := -1
		for i, target := range op.Targets {
			if target.NodeID == failure.NodeID {
				matched = i
			}
		}
		if matched < 0 || op.State == "not-started" || op.State == "active" {
			return false
		}
		switch failure.Phase {
		case "inspect":
			if !validFabricFailureCode(failure.Code) || op.EffectsApplied || op.EffectsUnconfirmed {
				return false
			}
			for _, attempted := range r.Attempted {
				if attempted {
					return false
				}
			}
		case "apply":
			if !validFabricApplyFailureCode(failure.Code) || matched >= len(r.Attempted) || !r.Attempted[matched] && failure.Code != "deadline-exceeded" && failure.Code != "cancelled" {
				return false
			}
		case "qualify":
			if !validFabricQualificationFailureCode(failure.Code) || !op.EffectsApplied || slices.Contains(r.Attempted, false) {
				return false
			}
		default:
			return false
		}
	}
	if op.SchemaVersion != 1 || !onboardingID.MatchString(op.OperationID) || op.ReviewID != op.OperationID || op.CreatedAt <= 0 || op.ExpiresAt != 0 || len(op.Targets) != participants || !cableIdentifier(r.OwnerPrincipal, 256) || !cableIdentifier(op.OwnerNodeID, 128) {
		return false
	}
	refused := op.State == "not-started"
	if refused {
		if !op.CleanupConfirmed || op.EffectsApplied || op.EffectsUnconfirmed || len(r.Plans) != 0 || len(r.Attempted) != 0 || len(r.Reserved) != 0 {
			return false
		}
	} else if !r.AdministratorApproved || len(r.Plans) != participants || len(r.Attempted) != participants || len(r.Reserved) != participants {
		return false
	}
	switch op.State {
	case "applying", "active", "rolling-back", "cancelled", "failed", "recovery-required", "not-started":
	default:
		return false
	}
	if op.CleanupConfirmed && (op.State != "cancelled" && op.State != "failed" && !refused) {
		return false
	}
	if op.State == "active" && (!op.EffectsApplied || slices.Contains(r.Attempted, false)) {
		return false
	}
	nodes, principals := map[string]bool{}, map[string]bool{}
	for _, target := range op.Targets {
		if nodes[target.NodeID] || principals[target.Principal] {
			return false
		}
		nodes[target.NodeID], principals[target.Principal] = true, true
	}
	if len(r.TransferHolds) > maxFabricTransferHolds || len(r.TransferHolds) > 0 && (!leaseState || op.CleanupConfirmed) {
		return false
	}
	owners := map[fabricTransferHold]bool{}
	for holdID, owner := range r.TransferHolds {
		if !validFabricTransferHold(holdID, owner, nodes) || owners[owner] {
			return false
		}
		owners[owner] = true
	}
	for i, target := range op.Targets {
		if (!refused && (target.NodeID != r.Plans[i].NodeID || target.Principal != r.Plans[i].Principal)) || !cableIdentifier(target.NodeID, 128) || !cableIdentifier(target.Principal, 256) || len(target.Interfaces) != 2 {
			return false
		}
		for _, iface := range target.Interfaces {
			prefix, err := fabricNativePrefix(iface)
			portBound := iface.PhysicalPort.SwitchID == target.SwitchID && iface.PhysicalPort.PortName == target.PortName
			if ring {
				portBound = slices.Contains(target.Ports, fabricTargetPort{SwitchID: iface.PhysicalPort.SwitchID, PortName: iface.PhysicalPort.PortName})
			}
			if err != nil || prefix.Bits() != prefixBits || fabricNativeIdentity(iface, iface) != nil || !portBound || !fabricLinkLocalBaseline(iface.Addresses) || (len(iface.Addresses) != 0 && iface.GeneratedDefault == nil) {
				return false
			}
			if iface.GeneratedDefault != nil && (!validFabricGeneratedDefault(iface.GeneratedDefault, iface) || (!refused && !r.SelectedPortPauseApproved)) {
				return false
			}
		}
		if target.Interfaces[0].Index == target.Interfaces[1].Index || target.Interfaces[0].Name == target.Interfaces[1].Name || target.Interfaces[0].MAC == target.Interfaces[1].MAC {
			return false
		}
		if ring && (target.SwitchID != "" || target.PortName != "" || len(target.Ports) != 2 || target.Ports[0].PortName != "p0" || target.Ports[1].PortName != "p1" || target.Ports[0].SwitchID != target.Ports[1].SwitchID) {
			return false
		}
	}
	if op.RecipeID == fabricRecipe || ring {
		candidates, err := fabricCandidates(op.Targets)
		_, digestErr := hex.DecodeString(op.QualificationDigest)
		qualified := op.QualifiedAt > 0 || len(op.CandidateIPs) != 0 || op.QualificationDigest != ""
		complete := op.QualifiedAt > 0 && fabricCandidatesQualified(candidates, op.CandidateIPs) && len(op.QualificationDigest) == 64 && digestErr == nil && op.QualificationDigest == fabricQualificationDigest(op.OperationID, op.RecipeID, op.Targets, op.CandidateIPs)
		if err != nil || qualified != complete || (op.State == "active" && !qualified) || (op.State != "active" && op.State != "applying" && qualified) {
			return false
		}
	} else if op.QualifiedAt != 0 || len(op.CandidateIPs) != 0 || op.QualificationDigest != "" {
		return false
	}
	return true
}

func (s *fabricService) review(ctx context.Context, request cableProductReviewRequest, inspectSelectedProfiles ...bool) (fabricReview, error) {
	public := fabricReview{SchemaVersion: 1, ReviewID: newOpID(), RecipeID: fabricRecipe, Persistence: "until-reboot", State: "blocked", Targets: []fabricTarget{}, Blockers: []string{}}
	ring := len(request.NodeIDs) == 3
	var ringReceipt cableprobe.Run
	if ring {
		var roles []string
		var err error
		ringReceipt, roles, err = s.m.cables.freshRingReceipt(request.ReviewRequest)
		if err != nil {
			return public, err
		}
		public.RecipeID, public.CableRunID = fabricRingRecipe, ringReceipt.RunID
		request.NodeIDs = roles
		sort.Slice(request.Ports, func(i, j int) bool {
			role := func(nodeID string) int { return slices.Index(roles, nodeID) }
			left, right := role(request.Ports[i].NodeID), role(request.Ports[j].NodeID)
			return left < right || left == right && request.Ports[i].PortName < request.Ports[j].PortName
		})
	} else if len(request.NodeIDs) != 2 || len(request.Ports) != 2 || !validateCableSelection(request.ReviewRequest) {
		return public, errors.New("select exactly two nodes with one port each, or three nodes with p0 and p1 each")
	} else if request.NodeIDs[0] > request.NodeIDs[1] {
		request.NodeIDs[0], request.NodeIDs[1] = request.NodeIDs[1], request.NodeIDs[0]
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 28*time.Second)
	defer cancel()
	// Reuse verified access and executable inspection only. The cable grant is
	// never approved; this review describes its own exact network-purpose effects.
	accessPublic, e := s.m.cables.review(ctx, request)
	if e != nil {
		return public, e
	}
	s.m.cables.mu.Lock()
	access := s.m.cables.reviews[accessPublic.ReviewID]
	s.m.cables.mu.Unlock()
	public.OwnerNodeID = accessPublic.OwnerNodeID
	if ring && ringReceipt.OwnerNodeID != accessPublic.OwnerNodeID {
		return public, errors.New("fresh cable receipt belongs to a different fabric owner")
	}
	if accessPublic.Permission != nil {
		encoded, _ := json.Marshal(accessPublic.Permission)
		_ = json.Unmarshal(encoded, &public.Permission)
	}
	if public.Permission != nil {
		public.Permission.Effects = []string{
			"Inspect management routes, DNS, native identity and existing network configuration as administrator on only these two devices; stop before changes if any guard fails.",
			"On only the selected NetworkManager devices, record the current Autoconnect policy and pause it while PAIR owns the runtime fabric. Preserve existing saved profiles; wait at most 90 seconds for an addressless pending attempt to settle. Do not disconnect a foreign usable connection. Restore the recorded policy after exact owned-profile cleanup; original automatic profiles may then resume.",
			"NetworkManager may retire or regenerate its own generated, unsaved default placeholders during this workflow. Their temporary object IDs are not preserved. PAIR does not edit or delete existing user or system saved profiles.",
			"Add only the four exact reviewed IPv4 addresses, one independent /30 lane per PCI function. For disconnected NetworkManager devices, create and activate only new operation-owned in-memory profiles; use exact owned native addresses only on unmanaged interfaces. No timed expiry or persistent network plan is created. Reboot or NetworkManager restart can remove runtime configuration.",
			"Cancel or recover by removing only addresses carrying this operation's private ownership labels. Preserve every other device, port, address, route, DNS setting and existing network plan. No cable, RDMA, NCCL or inference success is claimed.",
		}
		if ring {
			public.Permission.Effects = []string{
				"Revalidate the fresh completed reciprocal p0/p1 ring receipt, exact participant identities, management routes, DNS and existing network configuration before any write.",
				"Pause Autoconnect only on the six selected primary functions and create operation-owned, unsaved, never-default/no-DNS /31 profiles. Preserve Wi-Fi, its default route, saved profiles, policy rules and the six sibling functions.",
				"Prove all six source/interface-constrained routes and all six certificate-pinned peer identity reads before publishing peer-specific CandidateIPs. This qualifies a fast control route only; it does not claim RDMA, RoCE, NCCL, bandwidth or directness.",
				"Cancel or recover by withdrawing only this operation's CandidateIPs and removing only its owned profiles and addresses, then restore the recorded Autoconnect policy.",
			}
		}
	}
	if !accessPublic.Available {
		public.Blockers = append(public.Blockers, accessPublic.Reason)
	}
	if accessPublic.Available {
		for _, plan := range access.plans {
			if err := s.inspectWorkerCapability(ctx, plan); err != nil {
				public.Blockers = append(public.Blockers, "This selected worker does not expose the current fabric address capability.")
			}
		}
	}
	inventories := []fabricInventory{}
	for _, node := range request.NodeIDs {
		facts, e := s.inventory(ctx, node)
		if e != nil {
			public.Blockers = append(public.Blockers, "Current complete fabric inventory unavailable for "+node)
			continue
		}
		refs := []cableprobe.PortRef{}
		for _, p := range request.Ports {
			if p.NodeID == node {
				refs = append(refs, p)
			}
		}
		var target fabricTarget
		if ring {
			target, e = fabricSelectedRingTarget(facts, refs)
		} else if len(refs) == 1 {
			target, e = fabricSelectedTarget(facts, refs[0])
		} else {
			e = errors.New("selected physical port binding is incomplete")
		}
		if e != nil {
			public.Blockers = append(public.Blockers, e.Error())
			continue
		}
		public.Targets = append(public.Targets, target)
		inventories = append(inventories, facts)
		if ring && !fabricRingReceiptMatchesInventory(ringReceipt, target, facts) {
			public.Blockers = append(public.Blockers, "Fresh cable receipt interface identities changed for "+node)
		}
	}
	participants := 2
	if ring {
		participants = 3
	}
	if len(public.Targets) == participants {
		allocate := fabricAllocate
		if ring {
			allocate = fabricAllocateRing
		}
		if e := allocate(public.Targets, inventories); e != nil {
			public.Blockers = append(public.Blockers, e.Error())
		}
	}
	if s.held() {
		public.Blockers = append(public.Blockers, "Another fabric operation or unconfirmed cleanup holds network setup.")
	}
	public.InspectionAvailable = accessPublic.Available && len(access.plans) == participants && len(public.Targets) == participants && len(public.Blockers) == 0
	for _, target := range public.Targets {
		for _, iface := range target.Interfaces {
			public.InspectionRequired = public.InspectionRequired || len(iface.Addresses) != 0
		}
	}
	// NetworkManager keeps a generated default profile on an unconfigured port
	// between DHCP attempts, and only this inspection can bind it for consent.
	public.InspectionRequired = public.InspectionRequired || public.InspectionAvailable
	inspected := len(inspectSelectedProfiles) == 1 && inspectSelectedProfiles[0] && public.InspectionAvailable
	if inspected {
		// Explicit read-only administrator inspection is separate from mutation
		// approval. It creates no operation, reservation, profile or policy effect.
		results := make([]fabricWorkerResult, participants)
		failures := make([]error, participants)
		var reads sync.WaitGroup
		for i, target := range public.Targets {
			reads.Go(func() {
				results[i], failures[i] = s.worker(ctx, access.plans[i], fabricWorkerRequest{Protocol: fabricWorkerProtocol, Method: "inspect", OperationID: public.ReviewID, Target: target})
			})
		}
		reads.Wait()
		for i := range results {
			bound := results[i].Protocol == fabricWorkerProtocol && results[i].OperationID == public.ReviewID && results[i].NodeID == public.Targets[i].NodeID && results[i].Principal == public.Targets[i].Principal && results[i].Method == "inspect"
			if failures[i] != nil || !bound || results[i].FailureCode != "" || results[i].CleanupConfirmed || bindFabricGeneratedDefaults(&public.Targets[i], results[i].Facts) != nil {
				code := "native-inspect-failed"
				var failure *fabricInspectError
				if errors.As(failures[i], &failure) && validFabricFailureCode(failure.Code) {
					code = failure.Code
				} else if bound && validFabricFailureCode(results[i].FailureCode) {
					code = results[i].FailureCode
				} else if bound && len(results[i].Facts.Blockers) != 0 {
					code = "configuration-blocked"
				}
				public.Blockers = append(public.Blockers, public.Targets[i].NodeID+": selected native profile inspection failed ("+code+"); no configuration was applied.")
			}
		}
		if len(public.Blockers) == 0 {
			public.InspectionRequired = false
		}
	}
	if public.InspectionRequired && !inspected {
		public.Blockers = append(public.Blockers, "Inspect the selected network profiles as administrator before reviewing link-local interruption permission.")
	}
	if ctx.Err() != nil {
		public.Blockers = append(public.Blockers, "Selected network review was interrupted; inspect again.")
	}
	if fabricTargetsNeedPause(public.Targets) && public.Permission != nil {
		public.Permission.Effects = []string{
			"Use only the listed dedicated cable ports during the approved administration window; no other operator or tool may configure them until cleanup or explicit recovery handoff.",
			"Pause automatic connection selection on only the selected devices. If a listed generated default profile has a pending DHCP attempt, PAIR may stop only the current attempt of that exact reviewed profile after current identity, address, route and DNS checks. Link-local communication may be interrupted, including if that attempt finishes during the unavoidable check-to-dispatch gap.",
			"Create and activate only the reviewed operation-owned in-memory IPv4 profiles. Existing user and system saved profiles are neither edited nor deleted. Generated default profiles and link-local addresses may retire or regenerate.",
			"Rollback removes only PAIR-owned effects and restores the recorded device Autoconnect policy. DHCP success and identical generated profile IDs or link-local addresses are not promised. Preserve management Wi-Fi and all unselected ports.",
		}
	}
	public.RemainingMs = max(0, (30*time.Second - time.Since(started)).Milliseconds())
	if public.RemainingMs == 0 {
		public.Blockers = append(public.Blockers, "Review expired; inspect again.")
	}
	public.Executable = len(public.Blockers) == 0 && len(public.Targets) == participants
	if public.Executable {
		public.State = "ready"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, r := range s.reviews {
		if !time.Now().Before(r.expires) {
			delete(s.reviews, id)
		}
	}
	if len(s.reviews) >= 16 {
		return public, errors.New("too many pending fabric reviews")
	}
	if err := s.retainReviewLocked(public, access.ownerPrincipal); err != nil {
		return public, err
	}
	s.reviews[public.ReviewID] = fabricReviewRecord{public: public, access: access, inventories: inventories, expires: started.Add(30 * time.Second)}
	return public, nil
}

func fabricTargetsNeedPause(targets []fabricTarget) bool {
	for _, target := range targets {
		for _, iface := range target.Interfaces {
			if iface.GeneratedDefault != nil {
				return true
			}
		}
	}
	return false
}

func bindFabricGeneratedDefaults(target *fabricTarget, facts fabricNativeFacts) error {
	_, digestErr := hex.DecodeString(facts.Digest)
	if len(facts.Digest) != 64 || digestErr != nil || len(facts.Blockers) != 0 || len(facts.GeneratedDefaults) > len(target.Interfaces) {
		return errors.New("complete native inspection did not qualify the selected profiles")
	}
	bound := make([]*fabricGeneratedDefault, len(target.Interfaces))
	for _, candidate := range facts.GeneratedDefaults {
		found := false
		for i, iface := range target.Interfaces {
			if candidate.Index == iface.Index && candidate.InterfaceName == iface.Name {
				if bound[i] != nil || !validFabricGeneratedDefault(&candidate, iface) {
					return errors.New("ambiguous generated profile binding")
				}
				copy := candidate
				bound[i] = &copy
				found = true
			}
		}
		if !found {
			return errors.New("native inspection returned an unselected profile")
		}
	}
	for i, iface := range target.Interfaces {
		if len(iface.Addresses) != 0 && bound[i] == nil {
			return errors.New("link-local baseline has no qualified generated profile")
		}
	}
	for i := range target.Interfaces {
		target.Interfaces[i].GeneratedDefault = bound[i]
	}
	return nil
}

func rebindFabricGeneratedDefaults(target *fabricTarget, facts fabricNativeFacts, approved bool) (bool, error) {
	next := *target
	next.Interfaces = slices.Clone(target.Interfaces)
	if err := bindFabricGeneratedDefaults(&next, facts); err != nil {
		return false, err
	}
	changed := false
	for i := range target.Interfaces {
		before, after := target.Interfaces[i].GeneratedDefault, next.Interfaces[i].GeneratedDefault
		if before == nil || after == nil {
			if before != nil || after != nil {
				return false, errors.New("generated profile appeared or disappeared after review")
			}
			continue
		}
		if !fabricNMGeneratedStableSame(before, after) {
			return false, errors.New("generated profile stable binding changed after review")
		}
		if before.ActivePath != after.ActivePath {
			if !approved {
				return false, errors.New("generated activation changed without selected-port approval")
			}
			changed = true
		}
	}
	if changed {
		target.Interfaces = next.Interfaces
	}
	return changed, nil
}

func (s *fabricService) approve(ctx context.Context, id string, admin bool, selectedPortPauseApproved ...bool) (fabricOperation, error) {
	if !admin || !onboardingID.MatchString(id) {
		return fabricOperation{}, errors.New("explicit approval of this exact fabric address review is required")
	}
	// Serialize the entire approval (including read-only preflight) with terminal
	// refusal/status. Lock order remains canonical admission before service locks.
	s.m.exec.diagnosticMu.Lock()
	defer s.m.exec.diagnosticMu.Unlock()
	s.mu.Lock()
	if r := s.runs[id]; r != nil {
		out, err := s.recordedOutcomeLocked(r)
		s.mu.Unlock()
		return out, err
	}
	if s.shuttingDown {
		out, err := s.refuseLocked(id, "Fabric shutdown closed this approval. No setup started; review again after restart.")
		s.mu.Unlock()
		return out, err
	}
	review, ok := s.reviews[id]
	s.mu.Unlock()
	if !ok || !review.public.Executable || !time.Now().Before(review.expires) {
		return s.refuse(id, "This review is unavailable or expired. No setup started; review again.")
	}
	pauseApproved := len(selectedPortPauseApproved) == 1 && selectedPortPauseApproved[0]
	if fabricTargetsNeedPause(review.public.Targets) && !pauseApproved {
		return s.refuse(id, "Explicit selected-port interruption and administration-window approval is required. No setup started; review again.")
	}
	if e := s.m.cables.trust(review.access.ownerPrincipal, review.access.bindings); e != nil {
		return s.refuse(id, "Pairing changed before approval. No setup started; review again.")
	}
	for _, plan := range review.access.plans {
		if _, e := s.m.cables.access(plan); e != nil {
			return s.refuse(id, "Reviewed access changed or expired. No setup started; review again.")
		}
	}
	if review.public.RecipeID == fabricRingRecipe {
		selection := cableprobe.ReviewRequest{NodeIDs: []string{}, Ports: []cableprobe.PortRef{}}
		for _, target := range review.public.Targets {
			selection.NodeIDs = append(selection.NodeIDs, target.NodeID)
			selection.Ports = append(selection.Ports, fabricTargetPortRefs(target)...)
		}
		receipt, roles, err := s.m.cables.freshRingReceipt(selection)
		if err != nil || receipt.RunID != review.public.CableRunID || !slices.Equal(roles, selection.NodeIDs) {
			return s.refuse(id, "The bound reciprocal ring cable receipt is no longer fresh or exact. No setup started; run the cable check again.")
		}
	}
	for i, target := range review.public.Targets {
		facts, e := s.inventory(ctx, target.NodeID)
		if e != nil || facts.Digest != review.inventories[i].Digest || !fabricSameTarget(target, facts) {
			return s.refuse(id, "Reviewed identity, addresses or routes changed. No setup started; review again.")
		}
	}
	if s.otherBusy() {
		return s.refuse(id, "Another operation or cleanup hold prevents this setup. No setup started for this review.")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.runs[id]; r != nil {
		return s.recordedOutcomeLocked(r)
	}
	if s.heldLocked() || len(s.runs) >= 128 || !time.Now().Before(review.expires) {
		return s.refuseLocked(id, "Fabric admission changed. No setup started; review again.")
	}
	now := time.Now()
	runCtx, cancel := context.WithCancel(s.ctx)
	participants := len(review.public.Targets)
	r := &fabricRunRecord{Public: fabricOperation{SchemaVersion: 1, OperationID: id, ReviewID: id, OwnerNodeID: review.public.OwnerNodeID, RecipeID: review.public.RecipeID, CableRunID: review.public.CableRunID, State: "applying", Targets: review.public.Targets, Permission: review.public.Permission, CreatedAt: now.UnixMilli(), ExpiresAt: 0, Message: "Approved exact fabric addresses until rollback or reboot; checking every participant before adding addresses."}, OwnerPrincipal: review.access.ownerPrincipal, Plans: review.access.plans, Attempted: make([]bool, participants), Reserved: make([]bool, participants), cancel: cancel, done: make(chan struct{})}
	r.AdministratorApproved = true
	r.SelectedPortPauseApproved = pauseApproved
	if e := createFabricRecord(s.file(id), r); e != nil {
		cancel()
		s.recoveryFailed = true
		return fabricOperation{}, errors.New("fabric approval could not be retained before effects")
	}
	s.runs[id] = r
	go s.execute(runCtx, r)
	return cloneFabricOperation(r.Public), nil
}

func (s *fabricService) otherBusy() bool {
	if s.m.cables != nil && s.m.cables.held() {
		return true
	}
	d := s.m.exec.diagnostics
	if d != nil {
		d.mu.Lock()
		busy := d.packageAdmissionClosed || d.ctx.Err() != nil || d.packageActive != "" || d.runtimeActive != "" || d.packageRecoveryFailed || d.runtimeRecoveryFailed || d.recoveryFailed || d.reservation != nil || len(d.cancels) > 0
		d.mu.Unlock()
		if busy {
			return true
		}
	}
	return false
}
func cloneFabricOperation(op fabricOperation) fabricOperation {
	raw, _ := json.Marshal(op)
	var out fabricOperation
	_ = json.Unmarshal(raw, &out)
	return out
}

// Apply reports the same fixed inspection codes as preflight, so a refusal on a
// participant stays diagnosable without exposing native detail.
func validFabricApplyFailureCode(code string) bool {
	return code == "state-retain-failed" || code == "provider-failed" || validFabricFailureCode(code)
}

func fabricApplyFailureCode(err error) string {
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline-exceeded"
	}
	var failure *fabricInspectError
	if errors.As(err, &failure) && validFabricApplyFailureCode(failure.Code) {
		return failure.Code
	}
	return "provider-failed"
}

func (s *fabricService) retainPhaseFailure(r *fabricRunRecord, target fabricTarget, phase, code, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Public.Failure = &fabricFailure{NodeID: target.NodeID, Phase: phase, Code: code}
	r.Failure = message
	if s.save(r) != nil {
		s.recoveryFailed = true
	}
}

func (s *fabricService) state(r *fabricRunRecord, state, message string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Public.State = state
	r.Public.Message = message
	if state != "active" && state != "applying" {
		delete(s.qualified, r.Public.OperationID)
		r.Public.CandidateIPs = nil
		r.Public.QualifiedAt = 0
		r.Public.QualificationDigest = ""
	}
	if s.save(r) != nil {
		s.recoveryFailed = true
		r.Public.State = "recovery-required"
		r.Public.CleanupConfirmed = false
		return false
	}
	return true
}

func (s *fabricService) retainRollbackControlFailure(r *fabricRunRecord, target fabricTarget, stage string, err error) {
	failure := fabricRollbackFailure{NodeID: target.NodeID, Stage: stage, Code: fabricRollbackFailureCode(stage, err)}
	s.state(r, "recovery-required", fabricRollbackFailureMessage(failure))
}

func (s *fabricService) execute(ctx context.Context, r *fabricRunRecord) {
	defer close(r.done)
	defer r.cancel()
	prepared := true
	for i, target := range r.Public.Targets {
		request := fabricControlRequest{Method: "reserve", OperationID: r.Public.OperationID, Target: target}
		s.mu.Lock()
		r.Reserved[i] = true
		e := s.save(r)
		s.mu.Unlock()
		if e != nil {
			prepared = false
			break
		}
		if _, e = s.control(ctx, target, request); e != nil {
			prepared = false
			break
		}
	}
	// Inspect BOTH participants before the first address mutation.
	if prepared {
		for i, target := range r.Public.Targets {
			result, e := s.worker(ctx, r.Plans[i], fabricWorkerRequest{Protocol: fabricWorkerProtocol, Method: "inspect", OperationID: r.Public.OperationID, Target: target, SelectedPortPauseApproved: r.SelectedPortPauseApproved})
			// The production provider validates the wire before returning. Keep
			// even a substituted provider from treating a failed result as facts.
			if e == nil && result.FailureCode != "" {
				code := result.FailureCode
				if !validFabricFailureCode(code) || result.Protocol != fabricWorkerProtocol || result.OperationID != r.Public.OperationID || result.NodeID != target.NodeID || result.Principal != target.Principal || result.Method != "inspect" || result.CleanupConfirmed || result.Facts.Digest != "" || len(result.Facts.Routes) != 0 || len(result.Facts.Blockers) != 0 || len(result.Facts.GeneratedDefaults) != 0 {
					code = "result-invalid"
				}
				e = fabricInspectionError(code, errors.New("fabric inspection reported failure"))
			}
			if e == nil && len(result.Facts.Blockers) == 0 && (fabricTargetsNeedPause([]fabricTarget{target}) || len(result.Facts.GeneratedDefaults) != 0) {
				rebound := target
				changed, bindErr := rebindFabricGeneratedDefaults(&rebound, result.Facts, r.SelectedPortPauseApproved)
				if bindErr != nil {
					e = fabricInspectionError("network-manager-generation-changed", bindErr)
				} else if changed {
					s.mu.Lock()
					r.Public.Targets[i] = rebound
					if saveErr := s.save(r); saveErr != nil {
						e = fabricInspectionError("configuration-unavailable", saveErr)
					}
					s.mu.Unlock()
				}
			}
			if e != nil || len(result.Facts.Blockers) != 0 {
				s.mu.Lock()
				if r.Failure == "" {
					r.Failure = "Fabric administrator preflight failed before address configuration."
				}
				if e != nil && r.Public.Failure == nil {
					code := "native-inspect-failed"
					var cause *fabricInspectError
					if errors.As(e, &cause) && validFabricFailureCode(cause.Code) {
						code = cause.Code
					} else if errors.Is(e, context.Canceled) {
						code = "cancelled"
					} else if errors.Is(e, context.DeadlineExceeded) {
						code = "deadline-exceeded"
					}
					r.Public.Failure = &fabricFailure{NodeID: target.NodeID, Phase: "inspect", Code: code}
				}
				if e == nil && len(result.Facts.Blockers) > 0 {
					if r.Public.Failure == nil {
						r.Public.Failure = &fabricFailure{NodeID: target.NodeID, Phase: "inspect", Code: "configuration-blocked"}
					}
					r.Failure = "Fabric preflight blocked on " + target.NodeID + ": existing or ambiguous network configuration requires review."
				}
				s.mu.Unlock()
				prepared = false
				break
			}
		}
	}
	if prepared {
		for i, target := range r.Public.Targets {
			if ctx.Err() != nil {
				s.retainPhaseFailure(r, target, "apply", fabricApplyFailureCode(ctx.Err()), "Fabric apply failed on "+target.NodeID+": the reviewed participant action did not complete.")
				prepared = false
				break
			}
			s.mu.Lock()
			r.Attempted[i] = true
			r.Public.EffectsUnconfirmed = true
			e := s.save(r)
			if e != nil {
				r.Public.Failure = &fabricFailure{NodeID: target.NodeID, Phase: "apply", Code: "state-retain-failed"}
				r.Failure = "Fabric apply failed on " + target.NodeID + ": the operation state could not be retained before the participant action."
				_ = s.save(r)
			}
			s.mu.Unlock()
			if e != nil {
				prepared = false
				break
			}
			result, workerErr := s.worker(ctx, r.Plans[i], fabricWorkerRequest{Protocol: fabricWorkerProtocol, Method: "apply", OperationID: r.Public.OperationID, Target: target, SelectedPortPauseApproved: r.SelectedPortPauseApproved})
			if workerErr == nil && result.FailureCode != "" {
				code := result.FailureCode
				if !validFabricApplyFailureCode(code) {
					code = "result-invalid"
				}
				workerErr = fabricInspectionError(code, errors.New("fabric apply provider reported failure"))
			}
			if workerErr != nil {
				e = workerErr
				s.retainPhaseFailure(r, target, "apply", fabricApplyFailureCode(e), "Fabric apply failed on "+target.NodeID+": the reviewed participant action did not complete.")
				prepared = false
				break
			}
			s.mu.Lock()
			r.Public.EffectsApplied = true
			r.Public.EffectsUnconfirmed = false
			s.mu.Unlock()
		}
	}
	if prepared && (r.Public.RecipeID == fabricRecipe || r.Public.RecipeID == fabricRingRecipe) {
		if err := s.qualifyFabric(ctx, r); err != nil {
			target := r.Public.Targets[0]
			code := fabricQualificationFailureCode(err, "result-invalid")
			var failure *fabricProofFailure
			if errors.As(err, &failure) {
				code = failure.Code
				for _, candidate := range r.Public.Targets {
					if candidate.NodeID == failure.NodeID {
						target = candidate
					}
				}
			}
			s.retainPhaseFailure(r, target, "qualify", code, "Fast fabric qualification failed on "+target.NodeID+": no candidate endpoints were published.")
			prepared = false
		}
	}
	if prepared && ctx.Err() == nil {
		// Commit before releasing admission. No automatic teardown after success:
		// a workload may begin using this stable configuration once released.
		message := "Fabric addresses are configured until explicit rollback or reboot. Connectivity, RDMA and workloads remain unverified."
		if r.Public.QualifiedAt > 0 {
			message = "The fast control route is qualified and peer-specific CandidateIPs are available. RDMA, RoCE, NCCL, bandwidth and inference remain unverified."
		}
		if s.state(r, "active", message) {
			for i, target := range r.Public.Targets {
				if _, err := s.control(ctx, target, fabricControlRequest{Method: "release", OperationID: r.Public.OperationID, Target: target}); err != nil {
					s.state(r, "recovery-required", "Addresses were applied, but admission release is unconfirmed; reconcile this operation before further mutation.")
					return
				}
				s.mu.Lock()
				r.Reserved[i] = false
				s.mu.Unlock()
			}
			s.mu.Lock()
			if s.save(r) != nil {
				s.recoveryFailed = true
				r.Public.State = "recovery-required"
			}
			s.mu.Unlock()
			return
		}
	}
	if !prepared && ctx.Err() == nil {
		s.mu.Lock()
		if r.Failure == "" {
			r.Failure = "Fabric preparation or apply failed; no connectivity result was established."
		}
		s.mu.Unlock()
	}
	s.rollback(r)
}

func (s *fabricService) rollback(r *fabricRunRecord) {
	if !s.beginFabricRollback(r, "Confirming removal of only this operation's owned addresses on every selected device.") {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	clean := true
	for i := len(r.Public.Targets) - 1; i >= 0; i-- {
		target := r.Public.Targets[i]
		if r.Attempted[i] {
			result, e := s.worker(ctx, r.Plans[i], fabricWorkerRequest{Protocol: fabricWorkerProtocol, Method: "rollback", OperationID: r.Public.OperationID, Target: target})
			if e != nil || !result.CleanupConfirmed {
				clean = false
				continue
			}
		}
		if r.Reserved[i] {
			if _, e := s.control(ctx, target, fabricControlRequest{Method: "release", OperationID: r.Public.OperationID, Target: target}); e != nil {
				clean = false
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Public.CleanupConfirmed = clean
	if clean {
		r.Public.EffectsUnconfirmed = false
		r.Public.State = "cancelled"
		r.Public.Message = "Exact operation-owned address cleanup confirmed; other network configuration was preserved."
		if r.Failure != "" {
			r.Public.State = "failed"
			r.Public.Message = r.Failure + " Exact owned-address cleanup is confirmed."
		}
	} else {
		r.Public.State = "recovery-required"
		r.Public.Message = "Owned address cleanup is unconfirmed. Keep this operation and use its explicit administrator recovery action."
	}
	if s.save(r) != nil {
		s.recoveryFailed = true
		r.Public.State = "recovery-required"
		r.Public.CleanupConfirmed = false
	}
}

func (s *fabricService) beginFabricRollback(r *fabricRunRecord, message string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.qualified, r.Public.OperationID)
	r.Public.CandidateIPs = nil
	r.Public.QualifiedAt = 0
	r.Public.QualificationDigest = ""
	r.Public.State = "rolling-back"
	r.Public.Message = message
	if s.save(r) != nil {
		s.recoveryFailed = true
		r.Public.State = "recovery-required"
		return false
	}
	return true
}

type fabricRetainedOperations struct {
	Operations []fabricOperation `json:"operations"`
}

// Discovery reads only validated retained records, so a client that restarted
// or did not start the operation can find every operation whose owned
// addresses still await confirmed cleanup, newest first.
func (s *fabricService) retainedOperations() (fabricRetainedOperations, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recoveryFailed {
		return fabricRetainedOperations{}, errors.New("retained fabric history is incomplete; no cleanup state inferred")
	}
	out := fabricRetainedOperations{Operations: []fabricOperation{}}
	for _, r := range s.runs {
		if r.Public.CleanupConfirmed || r.Public.State == "not-started" {
			continue
		}
		op, err := s.recordedOutcomeLocked(r)
		if err != nil {
			return fabricRetainedOperations{}, err
		}
		out.Operations = append(out.Operations, op)
	}
	sort.Slice(out.Operations, func(i, j int) bool { return out.Operations[i].CreatedAt > out.Operations[j].CreatedAt })
	return out, nil
}

func (s *fabricService) status(id string) (fabricOperation, error) {
	s.mu.Lock()
	if r := s.runs[id]; r != nil {
		out, err := s.recordedOutcomeLocked(r)
		s.mu.Unlock()
		return out, err
	}
	s.mu.Unlock()
	s.m.exec.diagnosticMu.Lock()
	defer s.m.exec.diagnosticMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refuseLocked(id, "This issued review did not start an operation and is now closed. No effects were applied; review again.")
}

func (s *fabricService) currentStatus(ctx context.Context, id string) (fabricOperation, error) {
	op, err := s.status(id)
	if err != nil || op.State != "active" || fabricRollbackBlockedMessage(op.Message) {
		return op, err
	}
	// Claim the shared fabric worker slot before any re-proof I/O. Rollback admission
	// uses the same slot, so status and rollback cannot pass separate check-then-act
	// windows and overwrite each other's durable outcome.
	s.mu.Lock()
	r := s.runs[id]
	if r == nil {
		s.mu.Unlock()
		return s.status(id)
	}
	op, err = s.recordedOutcomeLocked(r)
	if err != nil || op.State != "active" || fabricRollbackBlockedMessage(op.Message) {
		s.mu.Unlock()
		return op, err
	}
	if r.rollbackWaiters > 0 {
		s.mu.Unlock()
		return op, nil
	}
	if r.done != nil {
		select {
		case <-r.done:
		default:
			s.mu.Unlock()
			return op, nil
		}
	}
	leasedQualified := r.ConsumerLease != nil && s.qualified[id] == r.Public.QualificationDigest
	proofDone := make(chan struct{})
	r.done = proofDone
	// A leased group may keep using its last fresh process-local proof while this
	// read-only proof runs. An unleased fabric is withdrawn until proof succeeds.
	if !leasedQualified {
		delete(s.qualified, id)
	}
	s.mu.Unlock()
	defer close(proofDone)
	ctx, cancel := context.WithTimeout(ctx, 28*time.Second)
	defer cancel()
	valid := true
	for _, target := range op.Targets {
		matched := false
		for attempt := 0; attempt < 5; attempt++ {
			facts, err := s.inventory(ctx, target.NodeID)
			if err != nil {
				break
			}
			matched = fabricSameIdentity(target, facts)
			for _, expected := range target.Interfaces {
				present := false
				for _, actual := range facts.Interfaces {
					if actual.Index == expected.Index {
						for _, address := range actual.Addresses {
							present = present || address == expected.Address
						}
					}
				}
				matched = matched && present
			}
			if matched {
				break
			}
			// node-info has a two-second passive cache. Bound one refresh window
			// rather than treating its pre-apply snapshot as current removal proof.
			timer := time.NewTimer(500 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
		valid = valid && matched
	}
	s.mu.Lock()
	r = s.runs[id]
	if !valid && r.Public.State == "active" {
		delete(s.qualified, id)
		r.Public.State = "recovery-required"
		r.Public.CandidateIPs = nil
		r.Public.QualifiedAt = 0
		r.Public.QualificationDigest = ""
		r.Public.Message = "Applied address readback changed or is unavailable; preserve and reconcile this exact configuration."
		if s.save(r) != nil {
			s.recoveryFailed = true
		}
		out := cloneFabricOperation(r.Public)
		s.mu.Unlock()
		return out, nil
	}
	if r.Public.QualificationDigest == "" || len(r.Public.CandidateIPs) == 0 {
		out := cloneFabricOperation(r.Public)
		s.mu.Unlock()
		return out, nil
	}
	s.mu.Unlock()
	if err := s.requalifyActive(ctx, r); err != nil {
		s.mu.Lock()
		if r.Public.State == "active" {
			r.Public.Message = "Applied addresses remain owned, but route, RoCE-v2 GID/HCA, and pinned peer identity are not currently re-proven; group use is held."
		}
		out := cloneFabricOperation(r.Public)
		s.mu.Unlock()
		return out, nil
	}
	s.mu.Lock()
	if r.Public.State == "active" {
		r.Public.Message = "The fast control route is freshly qualified and peer-specific CandidateIPs are available. RDMA payload transfer, NCCL, bandwidth and inference remain unverified."
	}
	out := cloneFabricOperation(r.Public)
	s.mu.Unlock()
	return out, nil
}
func (s *fabricService) cancel(id string, administratorApproved ...bool) (fabricOperation, error) {
	return s.cancelContext(context.Background(), id, administratorApproved...)
}

func (s *fabricService) cancelContext(ctx context.Context, id string, administratorApproved ...bool) (fabricOperation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var waiting *fabricRunRecord
	for {
		s.mu.Lock()
		if waiting != nil {
			if waiting.rollbackWaiters > 0 {
				waiting.rollbackWaiters--
			}
			waiting = nil
		}
		if ctx.Err() != nil {
			s.mu.Unlock()
			return fabricOperation{}, errors.New("fabric rollback admission deadline expired while waiting for current proof")
		}
		r := s.runs[id]
		if r == nil {
			s.mu.Unlock()
			return s.status(id)
		}
		if r.Public.State == "not-started" {
			out, err := s.recordedOutcomeLocked(r)
			s.mu.Unlock()
			return out, err
		}
		if r.Public.State == "active" {
			if s.shuttingDown {
				s.mu.Unlock()
				return fabricOperation{}, errors.New("fabric shutdown closed new rollback admission")
			}
			if r.ConsumerLease != nil {
				s.mu.Unlock()
				return fabricOperation{}, errors.New("a serving group is using this fabric; stop the group and confirm its cleanup before rollback")
			}
			if len(r.TransferHolds) > 0 {
				s.mu.Unlock()
				return fabricOperation{}, errFabricTransferActive
			}
			if len(administratorApproved) != 1 || !administratorApproved[0] {
				s.mu.Unlock()
				return fabricOperation{}, errors.New("explicit administrator consent for exact applied-address rollback is required")
			}
			if r.done != nil {
				select {
				case <-r.done:
				default:
					done := r.done
					r.rollbackWaiters++
					waiting = r
					s.mu.Unlock()
					select {
					case <-done:
						continue
					case <-ctx.Done():
						s.mu.Lock()
						if waiting.rollbackWaiters > 0 {
							waiting.rollbackWaiters--
						}
						s.mu.Unlock()
						return fabricOperation{}, errors.New("fabric rollback admission deadline expired while waiting for current proof")
					}
				}
			}
			rollbackDone := make(chan struct{})
			r.done = rollbackDone
			delete(s.qualified, id)
			out := cloneFabricOperation(r.Public)
			s.mu.Unlock()
			go func() { defer close(rollbackDone); s.explicitRollback(r, true) }()
			return out, nil
		}
		if r.cancel != nil {
			if r.Public.State == "applying" {
				r.Public.State = "rolling-back"
				r.Public.CandidateIPs = nil
				r.Public.QualifiedAt = 0
				r.Public.QualificationDigest = ""
				r.Public.Message = "Cancellation requested; awaiting exact owned profile, policy and address cleanup."
				if s.save(r) != nil {
					s.recoveryFailed = true
				}
			}
			r.cancel()
		}
		out := cloneFabricOperation(r.Public)
		s.mu.Unlock()
		return out, nil
	}
}

func (s *fabricService) shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.shuttingDown = true
	pending := []chan struct{}{}
	for _, r := range s.runs {
		if r.done == nil {
			continue
		}
		select {
		case <-r.done:
			continue
		default:
		}
		if r.Public.State == "applying" && r.cancel != nil {
			r.cancel()
		}
		pending = append(pending, r.done)
	}
	s.mu.Unlock()
	for _, done := range pending {
		select {
		case <-done:
			continue
		default:
		}
		select {
		case <-done:
		case <-ctx.Done():
			return errors.Join(errors.New("fabric shutdown incomplete: owned cleanup is still pending"), ctx.Err())
		}
	}
	return nil
}

func (s *fabricService) recover(ctx context.Context, id string, admin bool) (fabricOperation, error) {
	if !admin {
		return fabricOperation{}, errors.New("fresh approval of exact owned-address rollback requires administrator consent")
	}
	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		return fabricOperation{}, errors.New("fabric shutdown closed recovery admission")
	}
	r := s.runs[id]
	if r == nil || r.Public.State != "recovery-required" {
		s.mu.Unlock()
		return fabricOperation{}, errors.New("this operation does not require recovery")
	}
	if r.ConsumerLease != nil {
		s.mu.Unlock()
		return fabricOperation{}, errors.New("a serving group is using this fabric; stop the group and confirm its cleanup before recovery")
	}
	if len(r.TransferHolds) > 0 {
		s.mu.Unlock()
		return fabricOperation{}, errFabricTransferActive
	}
	if r.done != nil {
		select {
		case <-r.done:
		default:
			s.mu.Unlock()
			return fabricOperation{}, errors.New("the prior fabric worker is still reconciling")
		}
	}
	s.mu.Unlock()
	plans, e := s.refreshAccess(ctx, r)
	if e != nil {
		return fabricOperation{}, e
	}
	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		return fabricOperation{}, errors.New("fabric shutdown closed recovery admission while access was being refreshed")
	}
	if r.Public.State != "recovery-required" {
		s.mu.Unlock()
		return fabricOperation{}, errors.New("recovery already started")
	}
	r.Plans = plans
	r.Public.State = "rolling-back"
	r.Public.Message = fabricRollbackAdmissionChecking
	r.done = make(chan struct{})
	s.mu.Unlock()
	go func() { defer close(r.done); s.explicitRollback(r, false) }()
	return s.status(id)
}

// Reacquire every participant under its canonical admission guard before any
// explicit removal. If a workload is busy/unknown, retain the applied network.
func (s *fabricService) explicitRollback(r *fabricRunRecord, priorActive bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if !priorActive && !s.state(r, "rolling-back", fabricRollbackAdmissionChecking) {
		return
	}
	if priorActive {
		plans, err := s.refreshAccess(ctx, r)
		if err != nil {
			s.state(r, "active", fabricRollbackAccessUnavailable)
			return
		}
		s.mu.Lock()
		r.Plans = plans
		s.mu.Unlock()
	}
	attempted := []int{}
	ready := true
	var reserveTarget fabricTarget
	var reserveErr error
	for i, target := range r.Public.Targets {
		attempted = append(attempted, i)
		if _, err := s.control(ctx, target, fabricControlRequest{Method: "reserve-rollback", OperationID: r.Public.OperationID, Target: target}); err != nil {
			ready = false
			reserveTarget, reserveErr = target, err
			break
		}
		s.mu.Lock()
		r.Reserved[i] = true
		s.mu.Unlock()
	}
	if !ready {
		// A participant that held reserve-rollback until the shared deadline must
		// not also fail every compensating release.
		releaseCtx, cancelRelease := context.WithTimeout(context.Background(), fabricRollbackReleaseTimeout)
		defer cancelRelease()
		released := true
		var releaseTarget fabricTarget
		var releaseErr error
		for _, i := range attempted {
			target := r.Public.Targets[i]
			if _, err := s.control(releaseCtx, target, fabricControlRequest{Method: "release", OperationID: r.Public.OperationID, Target: target}); err != nil {
				released = false
				if releaseErr == nil {
					releaseTarget, releaseErr = target, err
				}
			} else {
				s.mu.Lock()
				r.Reserved[i] = false
				s.mu.Unlock()
			}
		}
		if priorActive && released {
			s.state(r, "active", fabricRollbackAdmissionRefused)
		} else if releaseErr != nil {
			s.retainRollbackControlFailure(r, releaseTarget, "release", releaseErr)
		} else {
			s.retainRollbackControlFailure(r, reserveTarget, "reserve-rollback", reserveErr)
		}
		return
	}
	if !s.beginFabricRollback(r, "Every participant is idle; removing only this operation's owned addresses.") {
		return
	}
	s.rollback(r)
}

func (s *fabricService) refreshRollbackPlans(ctx context.Context, r *fabricRunRecord) ([]cableLaunchPlan, error) {
	s.mu.Lock()
	request := cableProductReviewRequest{ReviewRequest: cableprobe.ReviewRequest{NodeIDs: []string{}, Ports: []cableprobe.PortRef{}}}
	identities := make([][2]string, len(r.Public.Targets))
	retained := make([]onboardingCandidate, len(r.Public.Targets))
	if len(r.Plans) != len(r.Public.Targets) {
		s.mu.Unlock()
		return nil, errors.New("retained rollback participant binding is invalid")
	}
	for i, t := range r.Public.Targets {
		plan := r.Plans[i]
		if plan.NodeID != t.NodeID || plan.Principal != t.Principal || plan.Candidate.Address == "" || plan.Candidate.Port < 1 || plan.Candidate.HostKeySHA256 == "" {
			s.mu.Unlock()
			return nil, errors.New("retained rollback participant binding is invalid")
		}
		identities[i] = [2]string{t.NodeID, t.Principal}
		retained[i] = plan.Candidate
		request.NodeIDs = append(request.NodeIDs, t.NodeID)
		request.Ports = append(request.Ports, fabricTargetPortRefs(t)...)
	}
	s.mu.Unlock()
	// Accepted SSH host keys are consent for one review only, and candidate IDs
	// and management addresses do not survive every Engine Manager or DHCP
	// restart. Resolve each participant's current paired route, then bind only
	// the freshly authorized candidate carrying this operation's approved SSH
	// fingerprint. The cable review below re-proves node, principal, selected
	// ports, worker and current access before rollback.
	s.m.mesh.Refresh()
	currentCandidates := make([]onboardingCandidate, len(retained))
	currentGenerations := make([]string, len(retained))
	for i, identity := range identities {
		address := ""
		if s.m.cableLocal != nil && identity[0] == s.m.cableLocal.nodeID {
			if !s.m.mesh.Clustered() || identity[1] != s.m.mesh.NodeUUID() {
				return nil, errors.New("rollback participant or PAIR identity changed")
			}
			address = "127.0.0.1"
		} else if peer, ok := s.m.peers.lookup(identity[0]); ok && peer.clusterUUID == identity[1] && len(peer.addresses) > 0 {
			address = peer.addresses[0]
		}
		if address == "" {
			return nil, errors.New("rollback participant or PAIR identity changed")
		}
		currentCandidates[i].Address, currentCandidates[i].Port = address, 22
	}
	s.m.onboarding.mu.Lock()
	for i, prior := range retained {
		var current *onboardingPrivateTarget
		for _, candidate := range s.m.onboarding.targets {
			if candidate.candidate.Address != currentCandidates[i].Address || candidate.candidate.Port != currentCandidates[i].Port {
				continue
			}
			if current != nil {
				s.m.onboarding.mu.Unlock()
				return nil, errors.New("current exact rollback access is ambiguous")
			}
			copy := *candidate
			current = &copy
		}
		if current == nil || current.changedKey || !onboardingID.MatchString(current.candidate.CandidateID) || current.candidate.HostKeySHA256 != prior.HostKeySHA256 {
			s.m.onboarding.mu.Unlock()
			return nil, errors.New("rollback participant or SSH identity changed")
		}
		if _, err := s.m.cables.accessLocked(cableLaunchPlan{Candidate: current.candidate, AccessGeneration: current.accessGeneration}); err != nil {
			s.m.onboarding.mu.Unlock()
			return nil, err
		}
		currentCandidates[i] = current.candidate
		currentGenerations[i] = current.accessGeneration
		request.AcceptedHostKeys = append(request.AcceptedHostKeys, struct {
			CandidateID string `json:"candidateId"`
			SHA256      string `json:"sha256"`
		}{current.candidate.CandidateID, prior.HostKeySHA256})
	}
	s.m.onboarding.mu.Unlock()
	public, err := s.m.cables.review(ctx, request)
	if err != nil {
		return nil, err
	}
	s.m.cables.mu.Lock()
	access := s.m.cables.reviews[public.ReviewID]
	s.m.cables.mu.Unlock()
	if len(access.plans) != len(identities) || len(identities) < 2 || len(identities) > 3 {
		return nil, errors.New("current exact rollback access unavailable")
	}
	for i, plan := range access.plans {
		prior := retained[i]
		current := currentCandidates[i]
		if [2]string{plan.NodeID, plan.Principal} != identities[i] || plan.Candidate.CandidateID != current.CandidateID || plan.Candidate.Address != current.Address || plan.Candidate.Port != current.Port || plan.Candidate.AccessID != current.AccessID || plan.AccessGeneration != currentGenerations[i] || plan.Candidate.HostKeySHA256 != prior.HostKeySHA256 {
			return nil, errors.New("rollback participant or SSH identity changed")
		}
		if _, err := s.m.cables.access(plan); err != nil {
			return nil, err
		}
		if err := s.inspectWorkerCapability(ctx, plan); err != nil {
			return nil, err
		}
	}
	return access.plans, nil
}

func (m *Manager) handleFabric(ctx context.Context, msg *Message) {
	var result any
	var err error
	if msg.Method == "engine:fabric-retained-operations" {
		var request struct{}
		if onboardingDecode(msg.Params, &request) != nil {
			m.codec.RespondError(msg.ID, -32602, "invalid retained fabric request")
			return
		}
		result, err = m.exec.fabric.retainedOperations()
	} else if msg.Method == "engine:fabric-review" {
		var request struct {
			cableProductReviewRequest
			InspectSelectedProfiles bool `json:"inspectSelectedProfiles,omitempty"`
		}
		if onboardingDecode(msg.Params, &request) != nil {
			m.codec.RespondError(msg.ID, -32602, "invalid fabric selection")
			return
		}
		result, err = m.exec.fabric.review(ctx, request.cableProductReviewRequest, request.InspectSelectedProfiles)
	} else {
		var request struct {
			ReviewID                  string `json:"reviewId,omitempty"`
			OperationID               string `json:"operationId,omitempty"`
			AdministratorApproved     bool   `json:"administratorApproved,omitempty"`
			SelectedPortPauseApproved bool   `json:"selectedPortPauseApproved,omitempty"`
		}
		if onboardingDecode(msg.Params, &request) != nil {
			m.codec.RespondError(msg.ID, -32602, "invalid fabric operation request")
			return
		}
		switch msg.Method {
		case "engine:fabric-approve":
			result, err = m.exec.fabric.approve(ctx, request.ReviewID, request.AdministratorApproved, request.SelectedPortPauseApproved)
		case "engine:fabric-status":
			result, err = m.exec.fabric.currentStatus(ctx, request.OperationID)
		case "engine:fabric-cancel":
			result, err = m.exec.fabric.cancelContext(ctx, request.OperationID, request.AdministratorApproved)
		case "engine:fabric-recover":
			result, err = m.exec.fabric.recover(ctx, request.OperationID, request.AdministratorApproved)
		}
	}
	m.respondOrErr(msg, result, err)
}

func fabricTargetsEqual(a, b fabricTarget) bool { return reflect.DeepEqual(a, b) }
