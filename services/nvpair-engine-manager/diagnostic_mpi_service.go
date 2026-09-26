// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"time"
)

type diagnosticMPIRequest struct {
	MemberNodeIDs        []string `json:"memberNodeIds,omitempty"`
	BuildOperationID     string   `json:"buildOperationId,omitempty"`
	OwnerNodeID          string   `json:"ownerNodeId,omitempty"`
	ReviewID             string   `json:"reviewId,omitempty"`
	OperationID          string   `json:"operationId,omitempty"`
	GroupID              string   `json:"groupId,omitempty"`
	Network              string   `json:"network,omitempty"`
	DedicatedTestWindow  bool     `json:"dedicatedTestWindow,omitempty"`
	CloseUnstartedReview bool     `json:"closeUnstartedReview,omitempty"`
}

type diagnosticMPIReviewStatus struct {
	Operation    *diagnosticOperation `json:"operation"`
	ReviewClosed bool                 `json:"reviewClosed"`
}

const diagnosticMPIReviewMarkerOwner = "pair-mpi-review-marker-v1"
const diagnosticMPIReviewMarkerLimit = 64

// Deliberately contains no key, profile, plan, account access or command bytes.
type diagnosticMPIReviewMarker struct {
	MemberSetDigest     string `json:"memberSetDigest,omitempty"`
	SchemaVersion       int    `json:"schemaVersion"`
	Owner               string `json:"owner"`
	ReviewID            string `json:"reviewId"`
	OperationID         string `json:"operationId"`
	GroupID             string `json:"groupId"`
	BuildOperationID    string `json:"buildOperationId"`
	OwnerNodeID         string `json:"ownerNodeId"`
	Controller          string `json:"controller"`
	ControllerPin       string `json:"controllerPin"`
	ProfileDigest       string `json:"profileDigest"`
	BootstrapPlanDigest string `json:"bootstrapPlanDigest"`
	RecipeID            string `json:"recipeId,omitempty"`
	ExpiresAt           int64  `json:"expiresAt"`
	State               string `json:"state"`
}

type diagnosticMPIReviewIO struct {
	read  func(string) ([]byte, error)
	write func(string, diagnosticMPIReviewMarker, bool) error
	sync  func(string) error
}

func (d *diagnosticService) mpiReviewMarkerPath(id string) string {
	return filepath.Join(d.m.exec.baseDir, "diagnostic-mpi-reviews", id+".json")
}
func mpiReviewStorage() diagnosticMPIReviewIO {
	return diagnosticMPIReviewIO{read: readMPIReviewMarkerFile, write: writeMPIReviewMarkerFile, sync: syncMPIReviewMarkerFile}
}

func readMPIReviewMarkerFile(filename string) ([]byte, error) {
	parent, err := os.Lstat(filepath.Dir(filename))
	if err != nil {
		return nil, err
	}
	if !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("MPI review marker directory is not regular storage")
	}
	before, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > 8192 {
		return nil, errors.New("MPI review marker is not a bounded regular file")
	}
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		return nil, errors.New("MPI review marker changed while opening")
	}
	if err := diagnosticProfileOwnership(f); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil || len(raw) > 8192 {
		return nil, errors.New("MPI review marker exceeds its read bound")
	}
	return raw, nil
}
func syncMPIReviewMarkerFile(filename string) error {
	if _, err := readMPIReviewMarkerFile(filename); err != nil {
		return err
	}
	f, err := os.OpenFile(filename, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(filename))
		if err != nil {
			return err
		}
		syncErr = dir.Sync()
		closeErr = dir.Close()
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	}
	return nil
}
func writeMPIReviewMarkerFile(filename string, marker diagnosticMPIReviewMarker, create bool) error {
	dir := filepath.Dir(filename)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("MPI review marker directory is not regular storage")
	}
	if create {
		f, err := os.Open(dir)
		if err != nil {
			return err
		}
		entries, readErr := f.ReadDir(diagnosticMPIReviewMarkerLimit + 1)
		closeErr := f.Close()
		if readErr != nil && readErr != io.EOF || closeErr != nil || len(entries) >= diagnosticMPIReviewMarkerLimit {
			return errors.New("durable MPI review capacity reached; existing markers require reconciliation")
		}
		for _, entry := range entries {
			id := strings.TrimSuffix(entry.Name(), ".json")
			if entry.IsDir() || entry.Name() != id+".json" || !onboardingID.MatchString(id) {
				return errors.New("MPI review marker namespace requires recovery")
			}
		}
		if _, err := os.Lstat(filename); !errors.Is(err, os.ErrNotExist) {
			return errors.New("MPI review marker already exists or cannot be inspected")
		}
	} else {
		if _, err := readMPIReviewMarkerFile(filename); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(marker)
	if err != nil || len(raw) > 8192 {
		return errors.New("MPI review marker exceeds its write bound")
	}
	tmp := filename + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(raw)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		return errors.Join(writeErr, syncErr, closeErr)
	}
	if err := os.Rename(tmp, filename); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncMPIReviewMarkerFile(filename)
}

func validMPIReviewMarker(marker diagnosticMPIReviewMarker) bool {
	if _, _, err := diagnosticMPIRecipeArgs(diagnosticMPIStoredRecipe(marker.RecipeID)); err != nil {
		return false
	}
	if marker.MemberSetDigest != "" && !diagnosticDigest.MatchString(marker.MemberSetDigest) || marker.RecipeID == diagnosticMPITripleRecipe && marker.MemberSetDigest == "" {
		return false
	}
	return marker.SchemaVersion == 1 && marker.Owner == diagnosticMPIReviewMarkerOwner && onboardingID.MatchString(marker.ReviewID) && onboardingID.MatchString(marker.OperationID) && marker.GroupID == "pair-smoke-"+marker.OperationID && onboardingID.MatchString(marker.BuildOperationID) && diagnosticToken.MatchString(marker.OwnerNodeID) && diagnosticToken.MatchString(marker.Controller) && diagnosticDigest.MatchString(marker.ControllerPin) && diagnosticDigest.MatchString(marker.ProfileDigest) && diagnosticDigest.MatchString(marker.BootstrapPlanDigest) && marker.ExpiresAt > 0 && (marker.State == "unconsumed" || marker.State == "consumed" || marker.State == "closed")
}
func (d *diagnosticService) readMPIReviewMarker(id string, storage diagnosticMPIReviewIO) (diagnosticMPIReviewMarker, error) {
	var marker diagnosticMPIReviewMarker
	if !onboardingID.MatchString(id) {
		return marker, errors.New("invalid original MPI review identifier")
	}
	raw, err := storage.read(d.mpiReviewMarkerPath(id))
	if err != nil {
		return marker, err
	}
	if len(raw) > 8192 || strictDiagnosticJSON(raw, &marker) != nil || !validMPIReviewMarker(marker) || marker.ReviewID != id || marker.OwnerNodeID != d.m.mesh.NodeUUID() {
		return marker, errors.New("original MPI review marker is malformed or belongs to another owner")
	}
	return marker, nil
}

// Caller holds d.mu throughout transition, durable sync and exact readback.
func (d *diagnosticService) persistMPIReviewMarker(marker diagnosticMPIReviewMarker, create bool, storage diagnosticMPIReviewIO) error {
	if !validMPIReviewMarker(marker) {
		return errors.New("invalid MPI review marker")
	}
	writeErr := storage.write(d.mpiReviewMarkerPath(marker.ReviewID), marker, create)
	observed, readErr := d.readMPIReviewMarker(marker.ReviewID, storage)
	if readErr != nil || observed != marker {
		return errors.Join(writeErr, readErr, errors.New("MPI review marker transition is unconfirmed"))
	}
	if err := storage.sync(d.mpiReviewMarkerPath(marker.ReviewID)); err != nil {
		return errors.Join(writeErr, err)
	}
	confirmed, err := d.readMPIReviewMarker(marker.ReviewID, storage)
	if err != nil || confirmed != marker {
		return errors.Join(err, errors.New("MPI review marker changed after synchronization"))
	}
	return nil // A lost write acknowledgement is recovered only by this positive proof.
}

func (d *diagnosticService) publishMPIReviewLocked(bound *diagnosticMPIReviewBinding, storage diagnosticMPIReviewIO) error {
	if d.mpiReviews == nil {
		d.mpiReviews = map[string]*diagnosticMPIReviewBinding{}
	}
	if len(d.mpiReviews) >= 16 {
		return errors.New("MPI review capacity reached; existing volatile reviews must settle")
	}
	var header diagnosticMPIHeader
	if json.Unmarshal(bound.Plan, &header) != nil || header.RecipeID != diagnosticMPIStoredRecipe(bound.Public.RecipeID) || header.OperationID != bound.Public.OperationID || header.GroupID != bound.Public.GroupID || header.ProfileDigest != profileDigest(bound.Profile) || bound.Profile.OwnerNodeID != bound.Public.OwnerNodeID {
		return errors.New("compiled MPI review binding changed")
	}
	marker := diagnosticMPIReviewMarker{SchemaVersion: 1, Owner: diagnosticMPIReviewMarkerOwner, ReviewID: bound.Public.ReviewID, OperationID: bound.Public.OperationID, GroupID: bound.Public.GroupID, BuildOperationID: bound.Public.BuildOperationID, OwnerNodeID: bound.Public.OwnerNodeID, Controller: bound.Controller, ControllerPin: bound.ControllerPin, ProfileDigest: header.ProfileDigest, BootstrapPlanDigest: header.PlanDigest, RecipeID: bound.Public.RecipeID, ExpiresAt: bound.Public.ExpiresAt, State: "unconsumed"}
	marker.MemberSetDigest = diagnosticMPIMemberDigest(diagnosticMPIProfileMemberIDs(bound.Profile))
	if marker.MemberSetDigest == "" || len(bound.Profile.Members) != diagnosticMPIRecipeRanks(header.RecipeID) {
		return errors.New("compiled MPI review participant count or order changed")
	}
	if err := d.persistMPIReviewMarker(marker, true, storage); err != nil {
		return err
	}
	d.mpiReviews[bound.Public.ReviewID] = bound
	return nil
}

func (d *diagnosticService) mpiReviewCaller(marker diagnosticMPIReviewMarker, caller, pin string, pinned bool) error {
	if !pinned || !d.m.mesh.Clustered() || marker.Controller != caller || marker.ControllerPin != pin {
		return errors.New("original MPI review controller identity changed")
	}
	return nil
}

func (d *diagnosticService) mpiReviewOperationLocked(marker diagnosticMPIReviewMarker) (*diagnosticOperation, error) {
	if op, ok := d.operations[marker.OperationID]; ok {
		if diagnosticMPIStoredRecipe(op.RecipeID) != diagnosticMPIStoredRecipe(marker.RecipeID) || op.GroupID != marker.GroupID || op.OwnerNodeID != marker.OwnerNodeID || op.OperationID != marker.OperationID || op.profileDigest != "" && op.profileDigest != marker.ProfileDigest || marker.MemberSetDigest != "" && diagnosticMPIMemberDigest(op.MemberNodeIDs) != marker.MemberSetDigest {
			return nil, errors.New("retained MPI operation binding changed")
		}
		// Restart restores public operation fields into this map. Recover the
		// original profile binding from its sidecar instead of treating an empty
		// private field as evidence that any profile is acceptable.
		if op.profileDigest == "" {
			pin, err := readOnboardingFile(d.operationPath(marker.OperationID)+".profile", 64)
			if err != nil || string(pin) != marker.ProfileDigest {
				return nil, errors.New("recovered MPI operation does not match its review profile")
			}
		}
		copy := op
		return &copy, nil
	}
	filename := d.operationPath(marker.OperationID)
	if _, err := os.Lstat(filename); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var op diagnosticOperation
	if err := readDiagnosticJSON(filename, &op); err != nil {
		return nil, err
	}
	pin, err := readOnboardingFile(filename+".profile", 64)
	if err != nil || string(pin) != marker.ProfileDigest || diagnosticMPIStoredRecipe(op.RecipeID) != diagnosticMPIStoredRecipe(marker.RecipeID) || op.OperationID != marker.OperationID || op.GroupID != marker.GroupID || op.OwnerNodeID != marker.OwnerNodeID || marker.MemberSetDigest != "" && diagnosticMPIMemberDigest(op.MemberNodeIDs) != marker.MemberSetDigest {
		return nil, errors.New("retained MPI operation does not match its review marker")
	}
	return &op, nil
}

func (d *diagnosticService) closeUnstartedMPIReview(caller, reviewID string) (diagnosticMPIReviewStatus, error) {
	return d.closeUnstartedMPIReviewWithIO(caller, reviewID, mpiReviewStorage())
}
func (d *diagnosticService) closeUnstartedMPIReviewWithIO(caller, reviewID string, storage diagnosticMPIReviewIO) (diagnosticMPIReviewStatus, error) {
	d.m.mesh.Refresh()
	pin, pinned := d.m.mesh.PinSHA256(caller)
	d.mu.Lock()
	defer d.mu.Unlock()
	result := diagnosticMPIReviewStatus{}
	marker, err := d.readMPIReviewMarker(reviewID, storage)
	if err != nil {
		return result, err
	}
	if err := d.mpiReviewCaller(marker, caller, pin, pinned); err != nil {
		return result, err
	}
	if op, err := d.mpiReviewOperationLocked(marker); err != nil {
		return result, err
	} else if op != nil {
		result.Operation = op
		return result, nil
	}
	bound := d.mpiReviews[reviewID]
	if marker.State == "consumed" || bound != nil && bound.Consumed {
		return result, errors.New("original MPI start was consumed; operation publication must be reconciled")
	}
	if d.cancels[marker.OperationID] != nil {
		return result, errors.New("original MPI operation is still admitted")
	}
	for _, filename := range []string{d.operationPath(marker.OperationID) + ".profile", d.coordinatorBootstrapPath(marker.OperationID), filepath.Join(d.m.exec.baseDir, "diagnostic-runs", marker.OperationID)} {
		if _, err := os.Lstat(filename); !errors.Is(err, os.ErrNotExist) {
			return result, errors.New("retained MPI start evidence prevents unstarted review closure")
		}
	}
	if bound != nil {
		clear(bound.PrivateKey)
		bound.PrivateKey = nil
	}
	marker.State = "closed"
	if err := d.persistMPIReviewMarker(marker, false, storage); err != nil {
		return result, err
	}
	delete(d.mpiReviews, reviewID)
	result.ReviewClosed = true
	return result, nil
}

type diagnosticMPIReviewTarget struct {
	NodeID     string `json:"nodeId"`
	Interface  string `json:"interface"`
	Address    string `json:"address"`
	SSHAddress string `json:"sshAddress"`
}
type diagnosticMPIReview struct {
	ReviewID         string                      `json:"reviewId"`
	BuildOperationID string                      `json:"buildOperationId"`
	OperationID      string                      `json:"operationId"`
	GroupID          string                      `json:"groupId"`
	OwnerNodeID      string                      `json:"ownerNodeId"`
	Network          string                      `json:"network"`
	Transport        string                      `json:"transport"`
	RecipeID         string                      `json:"recipeId,omitempty"`
	ExpiresAt        int64                       `json:"expiresAt"`
	Targets          []diagnosticMPIReviewTarget `json:"targets"`
}
type diagnosticMPIReviewBinding struct {
	Public        diagnosticMPIReview
	Controller    string
	ControllerPin string
	Profile       diagnosticProfile
	Plan          json.RawMessage
	PrivateKey    []byte `json:"-"`
	Consumed      bool
	Operation     *diagnosticOperation
	Error         string
}
type diagnosticMPIReviewControl struct {
	Record              diagnosticManagedRecord     `json:"record"`
	Requests            []diagnosticMPIFactsRequest `json:"requests"`
	DedicatedTestWindow bool                        `json:"dedicatedTestWindow"`
}

func (d *diagnosticService) mpiPeer(ctx context.Context, nodeID string) (*remoteClient, error) {
	d.m.mesh.Refresh()
	peer, ok := d.m.peers.lookup(nodeID)
	if !ok || peer.clusterUUID != nodeID || !d.m.mesh.Clustered() || !d.m.mesh.HasPin(peer.clusterUUID) {
		return nil, errors.New("selected MPI owner is not a current paired participant")
	}
	return d.m.remoteClient(ctx, peer)
}

// A new MPI review may use freshly reauthorized access to the original
// registered accounts. This does not refresh the saved build's approval.
func (d *diagnosticService) mpiReviewAccess(binding diagnosticParticipantBinding, record diagnosticManagedRecord) ([]onboardingPrivateTarget, error) {
	if !validDiagnosticParticipantCount(len(binding.Targets)) || len(record.Targets) != len(binding.Targets) {
		return nil, errors.New("two or three registered MPI access bindings are required")
	}
	accesses, err := d.managedAccess(binding)
	if err != nil {
		return nil, err
	}
	for i, target := range binding.Targets {
		registered, access := record.Targets[i], accesses[i]
		if registered.NodeID != target.NodeID || registered.Principal != target.Principal || registered.Address != target.Address || registered.Local != target.Local {
			return nil, errors.New("registered MPI participant identity changed")
		}
		if target.Local {
			if target.Principal != d.m.mesh.NodeUUID() || target.Candidate != nil || registered.CandidateID != "" || registered.SSHHostKeySHA256 != "" {
				return nil, errors.New("local MPI participant differs from the original controller")
			}
			continue // Native facts revalidate the original registered account.
		}
		if target.Candidate == nil {
			return nil, errors.New("the original controller-to-device SSH binding is required")
		}
		var registration struct {
			Identity struct {
				User string `json:"user"`
			} `json:"identity"`
		}
		if json.Unmarshal(registered.Registration, &registration) != nil || !onboardingToken.MatchString(registration.Identity.User) || access.access.user != registration.Identity.User {
			return nil, errors.New("MPI review requires reauthorization of the registered device account")
		}
		if registered.NodeID != target.NodeID || registered.Principal != target.Principal || registered.Address != target.Address || registered.CandidateID != target.Candidate.CandidateID || registered.SSHHostKeySHA256 != target.Candidate.HostKeySHA256 || access.candidate.CandidateID != target.Candidate.CandidateID || access.candidate.Address != target.Address || access.candidate.Port != target.Candidate.Port || access.candidate.HostKeySHA256 != target.Candidate.HostKeySHA256 || access.accessGeneration == "" {
			return nil, errors.New("registered MPI account or SSH endpoint identity changed")
		}
	}
	return accesses, nil
}

func (d *diagnosticService) reviewMPI(ctx context.Context, request diagnosticMPIRequest) (diagnosticMPIReview, error) {
	if !onboardingID.MatchString(request.BuildOperationID) || request.Network != "management" || !request.DedicatedTestWindow || request.OwnerNodeID != "" || request.ReviewID != "" || request.OperationID != "" || request.GroupID != "" || request.CloseUnstartedReview {
		return diagnosticMPIReview{}, errors.New("review requires the registered build and an explicit management Socket test window")
	}
	d.mu.Lock()
	run := d.runtimeRuns[request.BuildOperationID]
	if run == nil {
		d.mu.Unlock()
		return diagnosticMPIReview{}, errors.New("registered build is unavailable")
	}
	record, err := d.readManaged(run, d.managedIO())
	binding := run.Binding
	d.mu.Unlock()
	if err != nil || record == nil {
		return diagnosticMPIReview{}, errors.New("verified registered participants are required")
	}
	originalRecord := record
	selected, participantBinding, err := selectDiagnosticMPIRecord(*record, binding.diagnosticParticipantBinding, request.MemberNodeIDs)
	if err != nil {
		return diagnosticMPIReview{}, err
	}
	record = &selected
	binding.diagnosticParticipantBinding = participantBinding
	accesses, err := d.mpiReviewAccess(binding.diagnosticParticipantBinding, *record)
	if err != nil {
		return diagnosticMPIReview{}, err
	}
	// Generations are intentionally absent from durable build records. Bind
	// only this new review to the fresh access snapshots; never mutate run.Binding.
	binding.Generations = make(map[string]string, len(accesses))
	for i, target := range binding.Targets {
		binding.Generations[target.NodeID] = accesses[i].accessGeneration
	}
	accessCurrent := func() error {
		current, err := d.mpiReviewAccess(binding.diagnosticParticipantBinding, *record)
		if err != nil {
			return err
		}
		for i, target := range binding.Targets {
			if current[i].accessGeneration != binding.Generations[target.NodeID] {
				return errors.New("MPI review account access changed during verification")
			}
		}
		return d.participantBindingCurrent(binding.diagnosticParticipantBinding)
	}
	if err = accessCurrent(); err != nil {
		return diagnosticMPIReview{}, err
	}
	control := diagnosticMPIReviewControl{Record: *record, DedicatedTestWindow: true}
	for i, target := range binding.Targets {
		if err = accessCurrent(); err != nil {
			return diagnosticMPIReview{}, err
		}
		var hostKey diagnosticBootstrapPublicKey
		var port int
		if target.Local {
			hostKey, port, err = d.localMPIHostIdentity(ctx, record.Targets[i])
			if err != nil {
				return diagnosticMPIReview{}, err
			}
		} else {
			if target.Candidate == nil {
				return diagnosticMPIReview{}, errors.New("reviewed SSH participant binding is unavailable")
			}
			access := accesses[i]
			client, dialErr := d.m.onboarding.dial(ctx, access.candidate, access.access)
			if dialErr != nil {
				return diagnosticMPIReview{}, dialErr
			}
			key, keyErr := client.verifiedHostPublicKey(target.Candidate.HostKeySHA256)
			client.close()
			if keyErr != nil {
				return diagnosticMPIReview{}, keyErr
			}
			hostKey = diagnosticBootstrapPublicKey{Algorithm: key.Algorithm, Blob: key.Blob, Fingerprint: key.Fingerprint}
			port = target.Candidate.Port
		}
		peers := make([]string, 0, len(record.Targets)-1)
		for j, other := range record.Targets {
			if i != j {
				peers = append(peers, other.Address)
			}
		}
		control.Requests = append(control.Requests, diagnosticMPIFactsRequest{Network: "management", Target: record.Targets[i], PeerAddress: peers[0], PeerAddresses: peers, SSHAddress: target.Address, SSHPort: port, SSHHostKey: hostKey})
	}
	if err = accessCurrent(); err != nil {
		return diagnosticMPIReview{}, err
	}
	owner := record.Targets[0].NodeID
	var review diagnosticMPIReview
	if owner == d.m.mesh.NodeUUID() {
		review, err = d.reviewMPIAsCoordinator(ctx, d.m.mesh.NodeUUID(), control)
	} else {
		client, clientErr := d.mpiPeer(ctx, owner)
		if clientErr != nil {
			return diagnosticMPIReview{}, clientErr
		}
		var raw []byte
		raw, err = client.postJSON(ctx, diagnosticControlPath, "", diagnosticControlRequest{Method: "mpi-review", MPIReview: &control})
		if err == nil {
			err = strictDiagnosticJSON(raw, &review)
		}
	}
	if err != nil {
		return diagnosticMPIReview{}, err
	}
	if review.OwnerNodeID != owner || review.BuildOperationID != record.OperationID || review.Network != "management" || review.Transport != "socket" || !onboardingID.MatchString(review.ReviewID) || !onboardingID.MatchString(review.OperationID) || review.GroupID != "pair-smoke-"+review.OperationID || len(review.Targets) != len(record.Targets) || diagnosticMPIRecipeRanks(diagnosticMPIStoredRecipe(review.RecipeID)) != len(record.Targets) {
		return diagnosticMPIReview{}, errors.New("native MPI review changed its managed participant binding")
	}
	if _, _, err := diagnosticMPIRecipeArgs(diagnosticMPIStoredRecipe(review.RecipeID)); err != nil {
		return diagnosticMPIReview{}, err
	}
	for i, target := range review.Targets {
		if target.NodeID != record.Targets[i].NodeID || target.SSHAddress != record.Targets[i].Address || !diagnosticConcreteIPv4(target.Address) || !diagnosticToken.MatchString(target.Interface) || len(target.Interface) > 15 {
			return diagnosticMPIReview{}, errors.New("native MPI review changed its selected target or interface")
		}
	}
	d.mu.Lock()
	currentRecord, recordErr := d.readManaged(run, d.managedIO())
	sameRun := d.runtimeRuns[request.BuildOperationID] == run
	d.mu.Unlock()
	if recordErr != nil || !sameRun || currentRecord == nil || !reflect.DeepEqual(*originalRecord, *currentRecord) {
		return diagnosticMPIReview{}, errors.New("registered runtime changed during MPI review")
	}
	if err = accessCurrent(); err != nil {
		return diagnosticMPIReview{}, err
	}
	return review, nil
}

func (d *diagnosticService) reviewMPIAsCoordinator(ctx context.Context, caller string, control diagnosticMPIReviewControl) (diagnosticMPIReview, error) {
	var empty diagnosticMPIReview
	if _, err := diagnosticMPIProgram(); err != nil {
		return empty, err
	}
	d.m.mesh.Refresh()
	pin, pinned := d.m.mesh.PinSHA256(caller)
	count := len(control.Record.Targets)
	if !pinned || !d.m.mesh.Clustered() || !control.DedicatedTestWindow || !control.Record.Adopted || !validDiagnosticParticipantCount(count) || len(control.Requests) != count || control.Record.Targets[0].Principal != d.m.mesh.NodeUUID() {
		return empty, errors.New("current coordinator, registered roster, and explicit test window are required")
	}
	factsBudget := 75 * time.Second
	if count == 3 {
		factsBudget = 110 * time.Second // The fixed inspector reads both peer routes.
	}
	ctx, stop := context.WithTimeout(ctx, factsBudget)
	defer stop()
	type answer struct {
		index int
		reply diagnosticMPIFactsReply
		err   error
	}
	answers := make(chan answer, count)
	for i, request := range control.Requests {
		peers := make([]string, 0, count-1)
		for j, target := range control.Record.Targets {
			if i != j {
				peers = append(peers, target.Address)
			}
		}
		if request.Network != "management" || !reflect.DeepEqual(request.Target, control.Record.Targets[i]) || request.PeerAddress != peers[0] || (count == 3 || request.PeerAddresses != nil) && !slices.Equal(request.PeerAddresses, peers) {
			return empty, errors.New("MPI prerequisite request differs from the registered roster or peer routes")
		}
		go func(index int, request diagnosticMPIFactsRequest) {
			if request.Target.Principal == d.m.mesh.NodeUUID() {
				reply, err := d.localMPIFacts(ctx, request)
				answers <- answer{index, reply, err}
				return
			}
			client, err := d.mpiPeer(ctx, request.Target.NodeID)
			var reply diagnosticMPIFactsReply
			if err == nil {
				var raw []byte
				raw, err = client.postJSON(ctx, diagnosticControlPath, "", diagnosticControlRequest{Method: "mpi-facts", MPIFacts: &request})
				if err == nil {
					err = strictDiagnosticJSON(raw, &reply)
				}
			}
			answers <- answer{index, reply, err}
		}(i, request)
	}
	replies := make([]diagnosticMPIFactsReply, count)
	for range control.Requests {
		select {
		case result := <-answers:
			if result.err != nil {
				return empty, result.err
			}
			replies[result.index] = result.reply
		case <-ctx.Done():
			return empty, ctx.Err()
		}
	}
	facts := make([]diagnosticMPIParticipantFacts, count)
	for i, reply := range replies {
		if replies[0].Address.Subnet == "" || reply.Address.Subnet != replies[0].Address.Subnet {
			return empty, errors.New("the management Socket baseline requires one current shared IPv4 subnet")
		}
		facts[i] = reply.Facts
	}
	selection := diagnosticMPISelection{OperationID: newOpID(), OwnerNodeID: control.Record.Targets[0].NodeID, Subnet: replies[0].Address.Subnet, SSHSourceIPv4: replies[0].SSHSourceIPv4, DedicatedTestWindow: true}
	p, plan, key, err := compileDiagnosticMPIPlan(control.Record, facts, selection, time.Now())
	if err != nil {
		return empty, err
	}
	keep := false
	defer func() {
		if !keep {
			clear(key)
		}
	}()
	var header diagnosticMPIHeader
	if json.Unmarshal(plan, &header) != nil {
		return empty, errors.New("compiled MPI plan is invalid")
	}
	review := diagnosticMPIReview{ReviewID: newOpID(), BuildOperationID: control.Record.OperationID, OperationID: header.OperationID, GroupID: p.GroupID, OwnerNodeID: p.OwnerNodeID, Network: "management", Transport: "socket", RecipeID: header.RecipeID, ExpiresAt: header.ExpiresAt, Targets: []diagnosticMPIReviewTarget{}}
	for _, fact := range facts {
		review.Targets = append(review.Targets, diagnosticMPIReviewTarget{NodeID: fact.NodeID, Interface: fact.Interface, Address: fact.CollectiveAddress, SSHAddress: fact.SSHAddress})
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.packageAdmissionClosed || d.ctx.Err() != nil || d.recoveryFailed || d.runtimeRecoveryFailed || d.packageRecoveryFailed {
		return empty, errors.New("MPI review admission closed")
	}
	bound := &diagnosticMPIReviewBinding{Public: review, Controller: caller, ControllerPin: pin, Profile: p, Plan: plan, PrivateKey: key}
	if err := d.publishMPIReviewLocked(bound, mpiReviewStorage()); err != nil {
		return empty, err
	}
	keep = true
	time.AfterFunc(time.Until(time.UnixMilli(review.ExpiresAt)), func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if current := d.mpiReviews[review.ReviewID]; current == bound {
			clear(current.PrivateKey)
			current.PrivateKey = nil
			delete(d.mpiReviews, review.ReviewID)
		}
	})
	return review, nil
}

func (d *diagnosticService) approveMPI(caller, reviewID string) (diagnosticOperation, error) {
	return d.approveMPIWithIO(caller, reviewID, mpiReviewStorage(), d.startBootstrap)
}

func (d *diagnosticService) approveMPIWithIO(caller, reviewID string, storage diagnosticMPIReviewIO, start func(diagnosticProfile, json.RawMessage, []byte) (diagnosticOperation, error)) (diagnosticOperation, error) {
	d.m.mesh.Refresh()
	pin, pinned := d.m.mesh.PinSHA256(caller)
	d.mu.Lock()
	marker, readErr := d.readMPIReviewMarker(reviewID, storage)
	if readErr != nil {
		d.mu.Unlock()
		return diagnosticOperation{}, readErr
	}
	if err := d.mpiReviewCaller(marker, caller, pin, pinned); err != nil {
		d.mu.Unlock()
		return diagnosticOperation{}, err
	}
	if marker.State == "closed" {
		d.mu.Unlock()
		return diagnosticOperation{}, errors.New("original MPI review was positively closed")
	}
	if op, err := d.mpiReviewOperationLocked(marker); err != nil {
		d.mu.Unlock()
		return diagnosticOperation{}, err
	} else if op != nil {
		d.mu.Unlock()
		return *op, nil
	}
	review := d.mpiReviews[reviewID]
	if marker.State == "consumed" || review != nil && review.Consumed {
		d.mu.Unlock()
		return diagnosticOperation{}, errors.New("original MPI start was consumed; operation publication must be reconciled")
	}
	if review == nil || review.Controller != caller || review.ControllerPin != pin || time.Now().UnixMilli() >= marker.ExpiresAt || len(review.PrivateKey) == 0 {
		d.mu.Unlock()
		return diagnosticOperation{}, errors.New("original MPI review or controller identity is unavailable or expired")
	}
	var header diagnosticMPIHeader
	if diagnosticMPIStoredRecipe(review.Public.RecipeID) != diagnosticMPIStoredRecipe(marker.RecipeID) || review.Public.OperationID != marker.OperationID || review.Public.GroupID != marker.GroupID || review.Public.OwnerNodeID != marker.OwnerNodeID || review.Public.BuildOperationID != marker.BuildOperationID || review.Public.ExpiresAt != marker.ExpiresAt || profileDigest(review.Profile) != marker.ProfileDigest || json.Unmarshal(review.Plan, &header) != nil || header.RecipeID != diagnosticMPIStoredRecipe(marker.RecipeID) || header.PlanDigest != marker.BootstrapPlanDigest || header.OperationID != marker.OperationID || header.GroupID != marker.GroupID || marker.MemberSetDigest != "" && marker.MemberSetDigest != diagnosticMPIMemberDigest(diagnosticMPIProfileMemberIDs(review.Profile)) {
		d.mu.Unlock()
		return diagnosticOperation{}, errors.New("volatile MPI review differs from its durable marker")
	}
	// Reject known admission closure before consuming the marker. A rejection
	// after consumption remains uncertain; it is never inferred unstarted.
	if d.packageAdmissionClosed || d.ctx.Err() != nil || d.packageActive != "" || d.runtimeActive != "" || d.packageRecoveryFailed || d.runtimeRecoveryFailed || d.recoveryFailed || d.reservation != nil || len(d.cancels) != 0 {
		d.mu.Unlock()
		return diagnosticOperation{}, errors.New("MPI start admission is currently held")
	}
	key := review.PrivateKey
	review.PrivateKey = nil
	marker.State = "consumed"
	if err := d.persistMPIReviewMarker(marker, false, storage); err != nil {
		clear(key)
		review.Error = "MPI review consumption is unconfirmed"
		d.mu.Unlock()
		return diagnosticOperation{}, err
	}
	review.Consumed = true
	d.mu.Unlock()
	op, err := start(review.Profile, review.Plan, key)
	d.mu.Lock()
	if err != nil {
		review.Error = diagnosticPublicMessage(err.Error())
	} else {
		if diagnosticMPIStoredRecipe(op.RecipeID) != diagnosticMPIStoredRecipe(marker.RecipeID) || op.OperationID != marker.OperationID || op.GroupID != marker.GroupID || op.OwnerNodeID != marker.OwnerNodeID {
			err = errors.New("MPI start returned an operation outside its consumed review")
			review.Error = err.Error()
		} else {
			review.Operation = &op
		}
	}
	d.mu.Unlock()
	return op, err
}

func (m *Manager) handleDiagnosticMPI(ctx context.Context, msg *Message) {
	var request diagnosticMPIRequest
	if strictDiagnosticJSON(msg.Params, &request) != nil {
		m.codec.RespondError(msg.ID, -32602, "invalid managed MPI request")
		return
	}
	d := m.exec.diagnostics
	budget := 100 * time.Second
	if msg.Method == "engine:diagnostic-mpi-review" {
		count := len(request.MemberNodeIDs)
		if request.MemberNodeIDs == nil {
			d.mu.Lock()
			if run := d.runtimeRuns[request.BuildOperationID]; run != nil {
				count = len(run.Binding.Targets)
			}
			d.mu.Unlock()
		}
		if count == 3 {
			budget = 135 * time.Second
		}
	}
	ctx, stop := context.WithTimeout(ctx, budget)
	defer stop()
	if msg.Method == "engine:diagnostic-mpi-reconcile" {
		value, err := d.reconcileMPI(ctx, request)
		m.respondOrErr(msg, value, err)
		return
	}
	if msg.Method == "engine:diagnostic-mpi-recover" {
		value, err := d.recoverMPI(ctx, request)
		m.respondOrErr(msg, value, err)
		return
	}
	if msg.Method == "engine:diagnostic-mpi-review" {
		value, err := d.reviewMPI(ctx, request)
		m.respondOrErr(msg, value, err)
		return
	}
	if !diagnosticToken.MatchString(request.OwnerNodeID) || request.BuildOperationID != "" || request.Network != "" || request.DedicatedTestWindow || request.MemberNodeIDs != nil {
		m.codec.RespondError(msg.ID, -32602, "only the original native MPI owner may be addressed")
		return
	}
	var body diagnosticControlRequest
	recovery := msg.Method == "engine:diagnostic-mpi-status" && request.CloseUnstartedReview
	if recovery {
		if !onboardingID.MatchString(request.ReviewID) || request.OperationID != "" || request.GroupID != "" {
			m.codec.RespondError(msg.ID, -32602, "review recovery requires only its original owner and review identifier")
			return
		}
		body = diagnosticControlRequest{Method: "mpi-close-review", MPIReviewID: request.ReviewID}
	} else if request.CloseUnstartedReview {
		m.codec.RespondError(msg.ID, -32602, "review closure is available only through MPI status")
		return
	} else if msg.Method == "engine:diagnostic-mpi-approve" {
		if !onboardingID.MatchString(request.ReviewID) || request.OperationID != "" || request.GroupID != "" {
			m.codec.RespondError(msg.ID, -32602, "only the original MPI review may be approved")
			return
		}
		body = diagnosticControlRequest{Method: "mpi-approve", MPIReviewID: request.ReviewID}
	} else if (msg.Method == "engine:diagnostic-mpi-status" || msg.Method == "engine:diagnostic-mpi-cancel") && request.ReviewID == "" && onboardingID.MatchString(request.OperationID) && diagnosticToken.MatchString(request.GroupID) {
		operationMethod := "engine:diagnostic-status"
		if msg.Method == "engine:diagnostic-mpi-cancel" {
			operationMethod = "engine:diagnostic-cancel"
		}
		body = diagnosticControlRequest{Method: operationMethod, Request: diagnosticRequest{OperationID: request.OperationID, GroupID: request.GroupID}}
	} else {
		m.codec.RespondError(msg.ID, -32602, "invalid managed MPI control selector")
		return
	}
	raw, err := d.callOriginalMPIControl(ctx, request.OwnerNodeID, body)
	if recovery {
		var status diagnosticMPIReviewStatus
		if err == nil {
			err = strictDiagnosticJSON(raw, &status)
			if err == nil && ((status.Operation != nil && status.ReviewClosed) || (status.Operation == nil && !status.ReviewClosed)) {
				err = errors.New("MPI review recovery did not provide a positive closure or retained operation")
			}
			if err == nil && status.Operation != nil && (status.Operation.OwnerNodeID != request.OwnerNodeID || !onboardingID.MatchString(status.Operation.OperationID) || status.Operation.GroupID != "pair-smoke-"+status.Operation.OperationID) {
				err = errors.New("MPI review recovery changed its operation owner")
			}
		}
		m.respondOrErr(msg, status, err)
		return
	}
	var op diagnosticOperation
	if err == nil {
		err = strictDiagnosticJSON(raw, &op)
		if err == nil && (op.OwnerNodeID != request.OwnerNodeID || (request.OperationID != "" && op.OperationID != request.OperationID) || (request.GroupID != "" && op.GroupID != request.GroupID)) {
			err = errors.New("MPI operation response changed its owner or selector")
		}
	}
	m.respondOrErr(msg, op, err)
}

// A selected participant can also own the local desktop. Use the same control
// methods directly in that case; local ownership does not require SSH to self.
func (d *diagnosticService) callOriginalMPIControl(ctx context.Context, owner string, body diagnosticControlRequest) ([]byte, error) {
	if owner != d.m.mesh.NodeUUID() {
		client, err := d.mpiPeer(ctx, owner)
		if err != nil {
			return nil, err
		}
		return client.postJSON(ctx, diagnosticControlPath, "", body)
	}
	var value any
	var err error
	switch body.Method {
	case "mpi-approve":
		value, err = d.approveMPI(owner, body.MPIReviewID)
	case "mpi-close-review":
		value, err = d.closeUnstartedMPIReview(owner, body.MPIReviewID)
	case "engine:diagnostic-status", "engine:diagnostic-cancel":
		profile, profileErr := d.operationProfile(body.Method, body.Request)
		if profileErr != nil {
			return nil, profileErr
		}
		participant, ok := profile.member(profile.OwnerNodeID)
		if !ok || profile.OwnerNodeID != owner || participant.Principal != d.m.mesh.NodeUUID() {
			return nil, errors.New("retained MPI operation belongs to another native owner")
		}
		value, err = d.dispatch(ctx, body.Method, body.Request)
	default:
		return nil, errors.New("unknown original MPI control action")
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
