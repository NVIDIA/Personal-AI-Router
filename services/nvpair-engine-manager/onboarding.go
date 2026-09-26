// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"nvpair-shared/discovery"
	"nvpair-shared/hostbootstrap"
)

type onboardingPrivateTarget struct {
	accessGeneration string
	expiresAt        time.Time
	candidate        onboardingCandidate
	access           onboardingAccess
	lifetime         string
	changedKey       bool
	expectedPeer     string
}
type onboardingReviewBinding struct {
	review   onboardingReview
	info     map[string]onboardingPlatformInfo
	packages map[string]onboardingPackage
	upgrades map[string]onboardingExistingInstallation
}
type onboardingService struct {
	searchCancel                context.CancelFunc
	searchScope                 string
	scopes                      func(context.Context) ([]onboardingDiscoveryScope, error)
	search                      func(context.Context, string, []onboardingDiscoveredDevice) ([]onboardingDiscoveredDevice, error)
	testSave                    func(*onboardingRun) error
	recoveryRequired            bool
	imports                     map[string]onboardingArtifactSource
	dial                        func(context.Context, onboardingCandidate, onboardingAccess) (*onboardingSSH, error)
	observeKey                  func(context.Context, onboardingCandidate, string) (string, bool, bool, error)
	loadBootstrapCatalog        func() (bootstrapCatalog, error)
	loadBootstrapControllerKeys func(context.Context) ([]hostbootstrap.PublicKeyIdentity, error)
	testCluster                 func(context.Context, string, any) (json.RawMessage, error)
	ctx                         context.Context
	operations                  map[string]*onboardingRun
	active                      string
	m                           *Manager
	mu                          sync.Mutex
	targets                     map[string]*onboardingPrivateTarget
	reviews                     map[string]onboardingReviewBinding
	clusterWait                 map[string]chan onboardingClusterReply
	discover                    func(context.Context) []discovery.Node
}
type onboardingClusterReply struct {
	RequestID string          `json:"requestId"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
	ErrorCode int             `json:"errorCode,omitempty"`
}
type onboardingClusterError struct {
	Code    int
	Message string
}

func (e *onboardingClusterError) Error() string { return e.Message }

type onboardingBatchAccess struct {
	Purpose             string   `json:"purpose,omitempty"`
	OperationID         string   `json:"operationId,omitempty"`
	CandidateIDs        []string `json:"candidateIds"`
	Username            string   `json:"username"`
	Auth                string   `json:"auth"`
	KeyPath             string   `json:"keyPath,omitempty"`
	Password            string   `json:"password,omitempty"`
	Passphrase          string   `json:"passphrase,omitempty"`
	PrivateKey          []byte   `json:"privateKeyBase64,omitempty"`
	CredentialProvider  string   `json:"credentialProvider,omitempty"`
	CredentialPurpose   string   `json:"credentialPurpose,omitempty"`
	PublicKeySHA256     string   `json:"publicKeySha256,omitempty"`
	CredentialExpiresAt int64    `json:"credentialExpiresAt,omitempty"`
	ElevationPassword   string   `json:"elevationPassword,omitempty"`
	StartupLifetime     string   `json:"startupLifetime,omitempty"`
}

func newOnboardingService(m *Manager) *onboardingService {
	s := &onboardingService{m: m, imports: map[string]onboardingArtifactSource{}, dial: dialOnboardingSSH, ctx: context.Background(), operations: map[string]*onboardingRun{}, targets: map[string]*onboardingPrivateTarget{}, reviews: map[string]onboardingReviewBinding{}, clusterWait: map[string]chan onboardingClusterReply{}, discover: func(ctx context.Context) []discovery.Node {
		return discovery.New("_ssh._tcp", "local.", discovery.WithScanTimeout(2*time.Second)).Poll(ctx)
	}}
	s.loadOperations()
	s.scopes = onboardingDiscoveryScopes
	s.search = discoverOnboardingDevicesWithSeeds
	s.observeKey = observeOnboardingHostKey
	bootstrapCatalogPath, bootstrapCatalogPathErr :=
		currentBootstrapCatalogResourcePath()
	s.loadBootstrapCatalog = func() (bootstrapCatalog, error) {
		if bootstrapCatalogPathErr != nil {
			return bootstrapCatalog{},
				errors.New("bootstrap catalog resource path is unavailable")
		}
		return readBootstrapCatalog(bootstrapCatalogPath)
	}
	source := defaultBootstrapControllerKeySource()
	s.loadBootstrapControllerKeys = func(
		ctx context.Context,
	) ([]hostbootstrap.PublicKeyIdentity, error) {
		return readBootstrapControllerKeys(ctx, source)
	}
	return s
}
func (s *onboardingService) cluster(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if s.testCluster != nil {
		return s.testCluster(ctx, method, params)
	}
	id := newOpID()
	ch := make(chan onboardingClusterReply, 1)
	s.mu.Lock()
	s.clusterWait[id] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.clusterWait, id); s.mu.Unlock() }()
	if err := s.m.codec.Notify("engine:onboarding-cluster-call", map[string]any{"requestId": id, "method": method, "params": params}); err != nil {
		return nil, errors.New("PAIR cluster manager is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	select {
	case reply := <-ch:
		if reply.Error != "" {
			return nil, &onboardingClusterError{Code: reply.ErrorCode, Message: reply.Error}
		}
		return reply.Result, nil
	case <-ctx.Done():
		return nil, errors.New("PAIR cluster operation did not respond")
	}
}
func (s *onboardingService) clusterReply(raw json.RawMessage) {
	var reply onboardingClusterReply
	if onboardingDecode(raw, &reply) != nil {
		return
	}
	s.mu.Lock()
	ch := s.clusterWait[reply.RequestID]
	s.mu.Unlock()
	if ch != nil {
		select {
		case ch <- reply:
		default:
		}
	}
}
func (s *onboardingService) artifactSources(ctx context.Context) ([]onboardingArtifactSource, error) {
	catalog, err := readOnboardingCatalog(s.m.exec.baseDir)
	if err != nil {
		return nil, err
	}
	if exe, e := os.Executable(); e == nil {
		if self, e := onboardingSelfPackage(s.m.exec.baseDir, filepath.Dir(exe)); e == nil {
			catalog.Artifacts = append([]onboardingArtifactSource{self}, catalog.Artifacts...)
		}
	}
	s.mu.Lock()
	for _, artifact := range s.imports {
		catalog.Artifacts = append(catalog.Artifacts, artifact)
	}
	s.mu.Unlock()
	return catalog.Artifacts, nil
}
func (s *onboardingService) addTarget(request onboardingAddTargetRequest) (onboardingCandidate, error) {
	return s.addCandidate(
		request,
		"manual",
		"bootstrap-required",
	)
}

func (s *onboardingService) addCandidate(
	request onboardingAddTargetRequest,
	bootstrapSource string,
	bootstrapState string,
) (onboardingCandidate, error) {
	request.Address = strings.TrimSpace(request.Address)
	if net.ParseIP(request.Address) == nil {
		return onboardingCandidate{}, errors.New("enter a concrete device IP address")
	}
	if request.Port == 0 {
		request.Port = 22
	}
	if request.Port < 1 || request.Port > 65535 {
		return onboardingCandidate{}, errors.New("SSH port is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, target := range s.targets {
		if target.candidate.Address == request.Address && target.candidate.Port == request.Port {
			if target.candidate.BootstrapState == "" ||
				bootstrapState == "ssh-ready" {
				target.candidate.BootstrapState = bootstrapState
				target.candidate.BootstrapSource = bootstrapSource
			}
			return target.candidate, nil
		}
	}
	if len(s.targets) >= 32 {
		return onboardingCandidate{}, errors.New("at most 32 device candidates are retained in one session")
	}
	id := newOpID()
	label := request.Label
	if label == "" || len(label) > 128 {
		label = request.Address
	}
	c := onboardingCandidate{
		CandidateID:     id,
		Label:           label,
		Address:         request.Address,
		Port:            request.Port,
		BootstrapSource: bootstrapSource,
		BootstrapState:  bootstrapState,
		Reason:          "Select device account access before inspection",
	}
	s.targets[id] = &onboardingPrivateTarget{candidate: c, lifetime: "persistent"}
	return c, nil
}
func (s *onboardingService) candidates(ctx context.Context) (any, error) {
	s.expireAccess(time.Now())
	s.mu.Lock()
	recovery := s.recoveryRequired
	s.mu.Unlock()
	if recovery {
		return nil, errors.New("onboarding recovery is required: retained ownership records are unavailable or invalid; new setup is blocked")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	paired := s.pairedServingAddresses()
	for _, node := range s.discover(ctx) {
		for _, address := range node.Addresses {
			if ip := net.ParseIP(address); ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
				if paired[address] {
					break
				}
				_, _ = s.addCandidate(
					onboardingAddTargetRequest{
						Address: address,
						Port:    node.Port,
						Label:   strings.TrimSuffix(node.Host, "."),
					},
					"ssh-mdns",
					"ssh-ready",
				)
				break
			}
		}
	}
	s.mu.Lock()
	candidates := []onboardingCandidate{}
	for _, target := range s.targets {
		if paired[target.candidate.Address] {
			continue
		}
		candidates = append(candidates, target.candidate)
	}
	s.mu.Unlock()
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].CandidateID < candidates[j].CandidateID })
	artifacts, err := s.artifactSources(context.WithoutCancel(ctx))
	if err != nil {
		return nil, err
	}
	public := []onboardingArtifact{}
	for _, artifact := range artifacts {
		public = append(public, artifact.onboardingArtifact)
	}
	return map[string]any{"candidates": candidates, "artifacts": public}, nil
}

func (s *onboardingService) pairedServingAddresses() map[string]bool {
	addresses := make(map[string]bool)
	if s.m == nil || s.m.peers == nil || s.m.mesh == nil {
		return addresses
	}
	s.m.peers.mu.RLock()
	peers := make([]ecPeer, 0, len(s.m.peers.peers))
	for _, peer := range s.m.peers.peers {
		peers = append(peers, peer)
	}
	s.m.peers.mu.RUnlock()
	for _, peer := range peers {
		if !s.m.mesh.HasPin(peer.clusterUUID) {
			continue
		}
		for _, address := range peer.addresses {
			addresses[address] = true
		}
	}
	return addresses
}

func (s *onboardingService) bindAccess(ctx context.Context, r onboardingBatchAccess) (any, error) {
	defer clear(r.PrivateKey)
	if r.Purpose != "" && r.Purpose != "cable" {
		return nil, errors.New("unsupported access purpose")
	}
	if r.Purpose == "cable" {
		if r.OperationID != "" || (r.StartupLifetime != "" && r.StartupLifetime != "session") {
			return nil, errors.New("cable access is temporary and cannot change an onboarding operation")
		}
		r.StartupLifetime = "session"
		if s.m.cables != nil && s.m.cables.held() && !s.m.cables.cleanupAccessAllowed(r.CandidateIDs) {
			return nil, errors.New("an active or cleanup-unconfirmed cable operation holds this access session")
		}
	}
	if len(r.CandidateIDs) < 1 || len(r.CandidateIDs) > 4 || !onboardingToken.MatchString(r.Username) || (r.Auth != "password" && r.Auth != "existing-key") {
		return nil, errors.New("select one to four devices and explicit device account access")
	}
	if r.StartupLifetime == "" && r.OperationID == "" {
		r.StartupLifetime = "persistent"
	}
	if r.StartupLifetime != "" && r.StartupLifetime != "session" && r.StartupLifetime != "persistent" {
		return nil, errors.New("startup lifetime is invalid")
	}
	now := time.Now()
	access := onboardingAccess{user: r.Username, elevationPassword: r.ElevationPassword}
	providerFields := r.CredentialProvider != "" || r.CredentialPurpose != "" || r.PublicKeySHA256 != "" || r.CredentialExpiresAt != 0
	if len(r.PrivateKey) != 0 || providerFields {
		if r.Auth != "existing-key" || r.KeyPath != "" || r.Password != "" || r.ElevationPassword != "" || len(r.PrivateKey) == 0 || r.CredentialProvider != onboardingControllerCredentialProvider || r.CredentialPurpose != "enrolled-peer-upgrade" || r.PublicKeySHA256 == "" {
			return nil, errors.New("volatile SSH key provider binding is invalid")
		}
		expiresAt := time.UnixMilli(r.CredentialExpiresAt)
		if !expiresAt.After(now) || expiresAt.After(now.Add(30*time.Minute)) {
			return nil, errors.New("volatile SSH key provider binding is stale or exceeds its lifetime")
		}
		signer, err := parseOnboardingVolatileSigner(r.PrivateKey, r.Passphrase)
		clear(r.PrivateKey)
		if err != nil || ssh.FingerprintSHA256(signer.PublicKey()) != r.PublicKeySHA256 {
			return nil, errors.New("volatile SSH key does not match its public fingerprint")
		}
		access.signer = signer
		access.provider = onboardingCredentialBinding{provider: r.CredentialProvider, purpose: r.CredentialPurpose, publicKeySHA256: r.PublicKeySHA256, expiresAt: expiresAt, account: r.Username}
	}
	var recovery *onboardingRun
	var recoveryRevision uint64
	if r.OperationID != "" {
		if !onboardingID.MatchString(r.OperationID) {
			return nil, errors.New("retained operation ID is invalid")
		}
		s.mu.Lock()
		recovery = s.operations[r.OperationID]
		if recovery == nil || recovery.historyOnly || s.active != "" || recovery.Public.State == "running" || s.recoveryRequired {
			s.mu.Unlock()
			return nil, errors.New("retained operation is unavailable or busy; no access was changed")
		}
		recoveryRevision = recovery.Public.Revision
		s.mu.Unlock()
		identity, e := s.localIdentity(ctx)
		if e != nil || identity.NodeUUID != recovery.ControllerNodeID {
			return nil, errors.New("retained operation belongs to a different or unavailable PAIR controller")
		}
	}
	if len(r.Password) > 4096 || len(r.Passphrase) > 4096 || len(r.ElevationPassword) > 4096 {
		return nil, errors.New("access input exceeds its limit")
	}
	if r.Auth == "password" {
		if r.Password == "" {
			return nil, errors.New("enter the device account password")
		}
		access.password = r.Password
	} else if access.signer == nil {
		access.keyPath = r.KeyPath
		access.passphrase = r.Passphrase
		if access.keyPath == "" {
			home, _ := os.UserHomeDir()
			access.keyPath = filepath.Join(home, ".ssh", "id_ed25519")
		}
		if !filepath.IsAbs(access.keyPath) {
			return nil, errors.New("select an existing absolute SSH key path")
		}
	}
	seen := map[string]bool{}
	selected := []*onboardingPrivateTarget{}
	originals := map[string]*onboardingPrivateTarget{}
	s.mu.Lock()
	for _, id := range r.CandidateIDs {
		target := s.targets[id]
		if target == nil || seen[id] {
			s.mu.Unlock()
			return nil, errors.New("device selection changed or contains duplicates")
		}
		seen[id] = true
		clone := *target
		if recovery != nil {
			plan, exists := recovery.Plans[id]
			unfinished := false
			for _, state := range recovery.Public.Targets {
				if state.CandidateID == id && state.Stage != "paired" {
					unfinished = true
					break
				}
			}
			if !exists || !unfinished || plan.Username != r.Username || target.candidate.Address != plan.Candidate.Address || target.candidate.Port != plan.Candidate.Port || (r.StartupLifetime != "" && r.StartupLifetime != plan.Review.StartupLifetime) || (plan.Review.StartupLifetime != "persistent" && plan.Review.StartupLifetime != "session") {
				s.mu.Unlock()
				return nil, errors.New("recovery access must preserve the original unfinished targets, account and startup choices")
			}
			if access.signer != nil && (plan.Review.Action != "upgrade" || plan.ExistingInstallation == nil || plan.Review.StartupLifetime != "persistent") {
				s.mu.Unlock()
				return nil, errors.New("provider-backed recovery is restricted to persistent enrolled-peer upgrades")
			}
			clone.lifetime = plan.Review.StartupLifetime
		}
		originals[id] = target
		selected = append(selected, &clone)
	}
	s.mu.Unlock()
	id := newOpID()
	candidateSet := append([]string(nil), r.CandidateIDs...)
	sort.Strings(candidateSet)
	canonicalCandidates := strings.Join(candidateSet, ",")
	out := []onboardingCandidate{}
	for _, target := range selected {
		lifetime := r.StartupLifetime
		if recovery != nil {
			lifetime = target.lifetime
		}
		if access.signer != nil && lifetime != "persistent" {
			return nil, errors.New("provider-backed access is restricted to persistent enrolled-peer upgrades")
		}
		c := target.candidate
		c.AccessID = id
		if access.signer == nil && target.candidate.AccessID != "" && target.access.user == r.Username && target.lifetime == lifetime && ((r.Auth == "password" && target.access.keyPath == "" && target.access.signer == nil) || (r.Auth == "existing-key" && target.access.keyPath == access.keyPath && target.access.signer == nil)) {
			c.AccessID = target.candidate.AccessID
		}
		c.AccessLabel = r.Username + " (" + r.Auth + ")"
		c.AccessAvailable = true
		c.Reason = ""
		fingerprint, trusted, changed, err := s.observeKey(ctx, c, r.Username)
		if recovery != nil {
			s.mu.Lock()
			plan := recovery.Plans[c.CandidateID]
			s.mu.Unlock()
			if err != nil || changed || fingerprint == "" || fingerprint != plan.Review.HostKeySHA256 {
				return nil, errors.New("recovery SSH identity is unavailable, changed or denied by existing trust; original approval was not replaced")
			}
		}
		c.HostKeySHA256, c.HostKeyTrusted = fingerprint, trusted
		if err != nil {
			c.Reason = err.Error()
		} else if changed {
			c.Reason = "Existing SSH trust rejects this host key; do not approve it as a new device"
		} else {
			c.BootstrapState = "ssh-ready"
			if !trusted {
				c.Reason = "Review this host fingerprint before authorizing inspection"
				if recovery != nil {
					c.Reason = "Original operation SSH fingerprint matched; existing OS trust was not changed"
				}
			}
		}
		targetAccess := access
		if targetAccess.signer != nil {
			targetAccess.generation = id
			targetAccess.provider.candidateID = c.CandidateID
			targetAccess.provider.candidateSet = canonicalCandidates
			targetAccess.provider.lifetime = lifetime
			targetAccess.provider.accessGeneration = id
		}
		target.candidate, target.access, target.lifetime, target.changedKey = c, targetAccess, lifetime, changed
		target.expiresAt = now.Add(30 * time.Minute)
		if targetAccess.signer != nil && targetAccess.provider.expiresAt.Before(target.expiresAt) {
			target.expiresAt = targetAccess.provider.expiresAt
		}
		target.accessGeneration = id
		out = append(out, c)
	}
	if recovery != nil {
		identity, e := s.localIdentity(ctx)
		if e != nil || identity.NodeUUID != recovery.ControllerNodeID {
			return nil, errors.New("PAIR controller changed during recovery access observation")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if recovery != nil && (s.recoveryRequired || recovery.historyOnly || s.operations[r.OperationID] != recovery || s.active != "" || recovery.Public.State == "running" || recovery.Public.Revision != recoveryRevision) {
		return nil, errors.New("retained operation changed while access was being authorized")
	}
	if r.Purpose == "cable" && s.m.cables != nil && s.m.cables.held() && !s.m.cables.cleanupAccessAllowed(r.CandidateIDs) {
		return nil, errors.New("cable or cleanup-verification ownership changed while access was being authorized")
	}
	for _, target := range selected {
		if s.targets[target.candidate.CandidateID] != originals[target.candidate.CandidateID] {
			return nil, errors.New("newer device access superseded this request")
		}
	}
	for _, target := range selected {
		s.targets[target.candidate.CandidateID] = target
	}
	return map[string]any{"candidates": out}, nil
}
func (s *onboardingService) inspect(ctx context.Context, r onboardingInspectRequest) (onboardingReview, error) {
	s.expireAccess(time.Now())
	result := onboardingReview{ReviewID: newOpID(), ExpiresAt: time.Now().Add(10 * time.Minute).UnixMilli(), Targets: []onboardingReviewTarget{}, CanApprove: true}
	if len(r.CandidateIDs) < 1 || len(r.CandidateIDs) > 4 {
		return result, errors.New("select one to four devices")
	}
	var identity struct {
		NodeUUID  string `json:"nodeUuid"`
		ClusterID string `json:"clusterId"`
	}
	raw, err := s.cluster(ctx, "cluster:get-node-id", map[string]any{})
	if err != nil || json.Unmarshal(raw, &identity) != nil || identity.NodeUUID == "" {
		return result, errors.New("local PAIR identity must be available before onboarding")
	}
	result.ControllerNodeID, result.TargetClusterID = identity.NodeUUID, identity.ClusterID
	artifacts, err := s.artifactSources(ctx)
	if err != nil {
		return result, err
	}
	binding := onboardingReviewBinding{info: map[string]onboardingPlatformInfo{}, packages: map[string]onboardingPackage{}, upgrades: map[string]onboardingExistingInstallation{}}
	seen := map[string]bool{}
	fingerprints := map[string]bool{}
	for _, id := range r.CandidateIDs {
		s.mu.Lock()
		target := s.targets[id]
		if target != nil {
			copy := *target
			target = &copy
		}
		s.mu.Unlock()
		if target == nil || seen[id] {
			return result, errors.New("device selection changed or contains duplicates")
		}
		if !target.expiresAt.IsZero() && time.Now().After(target.expiresAt) {
			target.access = onboardingAccess{}
			target.candidate.AccessAvailable = false
		}
		seen[id] = true
		c := target.candidate
		row := onboardingReviewTarget{CandidateID: id, Label: c.Label, Address: c.Address, Port: c.Port, AccessID: c.AccessID, AccessLabel: c.AccessLabel, StartupLifetime: target.lifetime, Status: "blocked"}
		for _, accept := range r.AcceptedHostKeys {
			if accept.CandidateID == id && accept.SHA256 == c.HostKeySHA256 && accept.SHA256 != "" && !target.changedKey {
				c.HostKeyTrusted = true
			}
		}
		if target.changedKey || c.HostKeySHA256 == "" || !c.HostKeyTrusted || !c.AccessAvailable {
			row.Reason = "Verified device access and explicit host-key trust are required"
		} else if fingerprints[c.HostKeySHA256] {
			row.Reason = "Selected devices report the same SSH host identity"
		} else {
			fingerprints[c.HostKeySHA256] = true
			row.HostKeySHA256 = c.HostKeySHA256
			client, e := s.dial(ctx, c, target.access.forPurpose("enrolled-peer-upgrade"))
			if e == nil {
				var info onboardingPlatformInfo
				info, e = readOnboardingPlatform(ctx, client)
				if e == nil && info.ExistingPAIR {
					var existing onboardingExistingInstallation
					elevationAvailable := target.access.elevationPassword != "" && !strings.ContainsAny(target.access.elevationPassword, "\r\n\x00")
					existing, e = inspectOnboardingUpgrade(ctx, client, info, elevationAvailable)
					if e == nil {
						e = s.verifyUpgradePeer(ctx, existing, result.ControllerNodeID, result.TargetClusterID)
					}
					if e == nil && target.expectedPeer != "" && target.expectedPeer != existing.NodeID {
						e = errors.New("selected enrolled peer differs from the authenticated installation")
					}
					if e == nil {
						row.Action = "upgrade"
						summary := existing.Summary()
						row.ExistingInstallation = &summary
						if target.lifetime != existing.StartupLifetime {
							e = errors.New("an enrolled-peer upgrade preserves " + existing.StartupLifetime + " startup; select that lifetime before review")
						} else {
							binding.upgrades[id] = existing
						}
					}
				} else if e == nil {
					e = validateFreshOnboardingPlatform(info)
				}
				if e == nil && target.access.signer != nil && (row.Action != "upgrade" || row.StartupLifetime != "persistent") {
					e = errors.New("provider-backed access is restricted to persistent enrolled-peer upgrades")
				}
				client.close()
				row.Hostname = info.Hostname
				row.Platform, row.Arch, _ = onboardingPlatform(info.OS, info.Arch)
				row.NeedsLinger = row.Action != "upgrade" && target.lifetime == "persistent" && !info.Linger
				if e == nil && row.NeedsLinger && target.access.elevationPassword == "" {
					e = errors.New("persistent startup needs explicit own-account privilege access; enter the separate elevation password or choose session startup")
				}
				if e == nil {
					found := false
					for _, artifact := range artifacts {
						if artifact.Platform == row.Platform && artifact.Arch == row.Arch && (r.ArtifactID == "" || artifact.ArtifactID == r.ArtifactID) {
							pkg, pe := prepareOnboardingPackage(ctx, s.m.exec.baseDir, artifact)
							if pe != nil {
								e = pe
								break
							}
							if existing, upgrading := binding.upgrades[id]; upgrading {
								manifestHash, hashErr := onboardingPackageManifestHash(pkg)
								if hashErr != nil || manifestHash == existing.ManifestSHA256 || !onboardingRetentionAllowsArtifact(existing.Retention, artifact.SHA256) {
									e = errors.New("the selected peer already uses this package manifest or its successor could not be verified")
									break
								}
							}
							row.Artifact = &artifact.onboardingArtifact
							manifest, manifestErr := onboardingArchiveManifest(pkg.file)
							if manifestErr != nil || !onboardingSHA.MatchString(manifest.SourceFingerprint) {
								e = errors.New("verified successor source fingerprint is unavailable")
								break
							}
							row.Artifact.SourceFingerprint = manifest.SourceFingerprint
							binding.packages[id] = pkg
							binding.info[id] = info
							found = true
							break
						}
					}
					if !found && e == nil {
						e = errors.New("no verified PAIR package is available for this device architecture")
					}
				}
			}
			if e != nil {
				row.Reason = e.Error()
			} else {
				row.Status = "ready"
				// Session consent belongs to this review, never the public OS-trust fact.
			}
		}
		if row.Status != "ready" {
			result.CanApprove = false
		}
		result.Targets = append(result.Targets, row)
	}
	rejectDuplicateOnboardingUpgradePeers(&result, binding.upgrades)
	binding.review = result
	s.mu.Lock()
	s.reviews[result.ReviewID] = binding
	s.mu.Unlock()
	return result, nil
}
func (s *onboardingService) expireAccess(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, target := range s.targets {
		if !target.expiresAt.IsZero() && !now.Before(target.expiresAt) {
			target.access = onboardingAccess{}
			target.candidate.AccessAvailable = false
			target.candidate.Reason = "Temporary device access expired; authorize access again"
		}
	}
	for id, review := range s.reviews {
		if now.UnixMilli() > review.review.ExpiresAt {
			delete(s.reviews, id)
		}
	}
}
func (s *onboardingService) watchAccess(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			s.expireAccess(now)
		case <-ctx.Done():
			s.mu.Lock()
			for _, target := range s.targets {
				target.access = onboardingAccess{}
				target.candidate.AccessAvailable = false
			}
			s.mu.Unlock()
			return
		}
	}
}
func (m *Manager) handleOnboarding(ctx context.Context, msg *Message) {
	timeout := 3 * time.Minute
	if strings.HasPrefix(
		msg.Method,
		"engine:onboarding-bootstrap-",
	) {
		timeout = 6 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer clear(msg.Params)
	var result any
	var err error
	s := m.onboarding
	switch msg.Method {
	case "engine:onboarding-scopes":
		var scopes []onboardingDiscoveryScope
		scopes, err = s.scopes(ctx)
		result = map[string]any{"scopes": scopes}
	case "engine:onboarding-discover":
		var request struct {
			ScopeID string `json:"scopeId"`
			Cancel  bool   `json:"cancel,omitempty"`
		}
		if err = onboardingDecode(msg.Params, &request); err == nil {
			result, err = s.searchCandidates(ctx, request.ScopeID, request.Cancel)
		}
	case "engine:onboarding-import-artifact":
		var request struct {
			File string `json:"file"`
		}
		if err = onboardingDecode(msg.Params, &request); err == nil {
			result, err = s.importArtifact(ctx, request.File)
		}
	case "engine:onboarding-bootstrap-catalog":
		var request struct{}
		if err = onboardingDecode(msg.Params, &request); err == nil {
			result, err = s.bootstrapCatalogMetadata()
		}
	case "engine:onboarding-bootstrap-controller-keys":
		var request struct{}
		if err = onboardingDecode(msg.Params, &request); err == nil {
			result, err = s.bootstrapControllerKeyMetadata(ctx)
		}
	case "engine:onboarding-bootstrap-inspect":
		result, err = s.bootstrapInspect(ctx, msg.Params)
	case "engine:onboarding-bootstrap-review":
		result, err = s.bootstrapReview(ctx, msg.Params)
	case "engine:onboarding-bootstrap-apply":
		result, err = s.bootstrapApply(ctx, msg.Params)
	case "engine:onboarding-bootstrap-status":
		result, err = s.bootstrapStatus(ctx, msg.Params)
	case "engine:onboarding-bootstrap-recover":
		result, err = s.bootstrapRecover(ctx, msg.Params)
	case "engine:onboarding-bootstrap-verify":
		result, err = s.bootstrapVerify(ctx, msg.Params)
	case "engine:onboarding-candidates":
		result, err = s.candidates(ctx)
	case "engine:onboarding-add-target":
		var request onboardingAddTargetRequest
		if err = onboardingDecode(msg.Params, &request); err == nil {
			result, err = s.addTarget(request)
		}
	case "engine:onboarding-access":
		var request onboardingBatchAccess
		if err = onboardingDecode(msg.Params, &request); err == nil {
			clear(msg.Params)
			result, err = s.bindAccess(ctx, request)
		}
	case "engine:onboarding-inspect":
		var request onboardingInspectRequest
		if err = onboardingDecode(msg.Params, &request); err == nil {
			result, err = s.inspect(ctx, request)
		}
	case "engine:onboarding-approve":
		var request struct {
			ReviewID string `json:"reviewId"`
		}
		if err = onboardingDecode(msg.Params, &request); err == nil {
			result, err = s.approve(ctx, request.ReviewID)
		}
	case "engine:onboarding-status", "engine:onboarding-cancel", "engine:onboarding-retry":
		var request onboardingOperationRequest
		if err = onboardingDecode(msg.Params, &request); err == nil {
			result, err = s.operationRequest(ctx, msg.Method, request)
		}
	default:
		err = errors.New("onboarding method is unavailable")
	}
	if err != nil {
		_ = m.codec.RespondError(msg.ID, -32000, err.Error())
		return
	}
	_ = m.codec.Respond(msg.ID, result)
}

func (s *onboardingService) searchCandidates(ctx context.Context, scopeID string, cancelRequest bool) (any, error) {
	if !onboardingID.MatchString(scopeID) {
		return nil, errors.New("select an observed local network scope")
	}
	s.mu.Lock()
	if cancelRequest {
		if s.searchScope != scopeID || s.searchCancel == nil {
			s.mu.Unlock()
			return nil, errors.New("no matching device search is active")
		}
		s.searchCancel()
		s.mu.Unlock()
		return map[string]any{"candidates": []onboardingCandidate{}}, nil // Acknowledgement only; the original request settles separately.
	}
	if s.searchCancel != nil {
		s.mu.Unlock()
		return nil, errors.New("a device search is already active")
	}
	ctx, cancel := context.WithTimeout(ctx, onboardingDiscoveryLimit)
	s.searchCancel, s.searchScope = cancel, scopeID
	seeds := []onboardingDiscoveredDevice{}
	for _, target := range s.targets {
		seeds = append(seeds, onboardingDiscoveredDevice{
			Address:        target.candidate.Address,
			Port:           target.candidate.Port,
			Label:          target.candidate.Label,
			Source:         target.candidate.BootstrapSource,
			BootstrapState: target.candidate.BootstrapState,
		})
	}
	s.mu.Unlock()
	defer func() { cancel(); s.mu.Lock(); s.searchCancel = nil; s.searchScope = ""; s.mu.Unlock() }()
	devices, err := s.search(ctx, scopeID, seeds)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, errors.New("device search cancelled or expired; no partial discovery was admitted")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	endpoints := map[string]bool{}
	for _, target := range s.targets {
		endpoints[net.JoinHostPort(target.candidate.Address, fmt.Sprint(target.candidate.Port))] = true
	}
	for _, d := range devices {
		if net.ParseIP(d.Address) == nil ||
			d.Port != 22 ||
			(d.BootstrapState != "ssh-ready" &&
				d.BootstrapState != "bootstrap-required") ||
			d.Source == "" {
			return nil, errors.New("device observation is invalid")
		}
		endpoints[net.JoinHostPort(d.Address, "22")] = true
	}
	if len(endpoints) > 32 {
		return nil, errors.New("search exceeds the 32-device session limit; use a smaller eligible network or enter exact device addresses")
	}
	for _, d := range devices {
		if s.pairedServingAddresses()[d.Address] {
			continue
		}
		found := false
		for _, target := range s.targets {
			if target.candidate.Address == d.Address && target.candidate.Port == d.Port {
				if d.BootstrapState == "ssh-ready" {
					target.candidate.BootstrapState = d.BootstrapState
					target.candidate.BootstrapSource = d.Source
				}
				found = true
				break
			}
		}
		if found {
			continue
		}
		id := newOpID()
		label := d.Label
		if label == "" {
			label = d.Address
		}
		s.targets[id] = &onboardingPrivateTarget{
			candidate: onboardingCandidate{
				CandidateID:     id,
				Address:         d.Address,
				Port:            d.Port,
				Label:           label,
				BootstrapSource: d.Source,
				BootstrapState:  d.BootstrapState,
				Reason:          "Device observed; authorize account access before inspection",
			},
			lifetime: "persistent",
		}
	}
	result := []onboardingCandidate{}
	for _, target := range s.targets {
		result = append(result, target.candidate)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CandidateID < result[j].CandidateID })
	return map[string]any{"candidates": result}, nil
}
