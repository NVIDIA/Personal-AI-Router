// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"path"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Internal trusted producer input only. No renderer request decodes these types.
type diagnosticMPIArtifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type diagnosticMPIInterfaceObservation struct {
	Name      string
	Up        bool
	Addresses []string // Observed IPv4 CIDRs, not requested assignments.
}

type diagnosticMPIParticipantFacts struct {
	NodeID               string
	Principal            string
	ClusterPinSHA256     string
	ObservedAt           int64 // Unix milliseconds; fresh producer observation.
	UID                  int
	User                 string
	Home                 string
	PublicIdentitySHA256 string
	SSHAddress           string
	SSHPort              int
	SSHHostKey           diagnosticBootstrapPublicKey     // Bytes retained after verified callback.
	Manager              diagnosticMPIArtifact            // Current os.Executable path and actual hash/size.
	Tools                map[string]diagnosticMPIArtifact // Exact named installed system tools.
	Interface            string
	CollectiveAddress    string
	GPUUUID              string
	Interfaces           []diagnosticMPIInterfaceObservation
	Registration         []byte // Fresh fixed-runtime status registration, before projection.
}

type diagnosticMPISelection struct {
	OperationID         string
	OwnerNodeID         string
	Subnet              string
	SSHSourceIPv4       string
	DedicatedTestWindow bool
}

func diagnosticMPISystemToolPaths() map[string]string {
	result := map[string]string{}
	for _, name := range []string{"mpirun", "orted", "ssh", "ssh-agent", "ssh-add", "python3", "systemd-run", "systemctl", "busctl", "nvidia-smi"} {
		result[name] = "/usr/bin/" + name
	}
	return result
}

type diagnosticMPIRegistration struct {
	SchemaVersion int    `json:"schemaVersion"`
	Kind          string `json:"kind"`
	RecipeID      string `json:"recipeId"`
	OperationID   string `json:"operationId"`
	PlanDigest    string `json:"planDigest"`
	Attempt       int    `json:"attempt"`
	Identity      struct {
		NodeID               string `json:"nodeId"`
		Principal            string `json:"principal"`
		UID                  int    `json:"uid"`
		User                 string `json:"user"`
		Home                 string `json:"home"`
		PublicIdentitySHA256 string `json:"publicIdentitySha256"`
	} `json:"identity"`
	Sources map[string]struct {
		URL    string `json:"url"`
		Commit string `json:"commit"`
		Tag    string `json:"tag"`
	} `json:"sources"`
	Binary           diagnosticMPIArtifact            `json:"binary"`
	NCCLLibrary      diagnosticMPIArtifact            `json:"ncclLibrary"`
	Dependencies     map[string]diagnosticMPIArtifact `json:"dependencies"`
	ManagerAdopted   *bool                            `json:"managerAdopted"`
	RuntimeValidated *bool                            `json:"runtimeValidated"`
	GPUExecuted      *bool                            `json:"gpuExecuted"`
	MPIExecuted      *bool                            `json:"mpiExecuted"`
	LinkValidation   string                           `json:"linkValidation"`
}

func validMPIArtifact(file diagnosticMPIArtifact) bool {
	return diagnosticManagedToolValid(diagnosticTool{file.Path, file.SHA256}) && file.Size > 0 && file.Size <= 2<<30
}
func mpiTool(file diagnosticMPIArtifact) diagnosticTool {
	return diagnosticTool{file.Path, file.SHA256}
}

func validateMPIPublicKey(value diagnosticBootstrapPublicKey) error {
	if value.Algorithm != ssh.KeyAlgoED25519 && value.Algorithm != ssh.KeyAlgoECDSA256 && value.Algorithm != ssh.KeyAlgoRSA {
		return errors.New("unsupported verified SSH public key algorithm")
	}
	if len(value.Blob) > 8192 {
		return errors.New("SSH public key exceeds its bound")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(value.Blob)
	if err != nil {
		return errors.New("SSH public key encoding is invalid")
	}
	key, err := ssh.ParsePublicKey(raw)
	if err != nil || key.Type() != value.Algorithm || ssh.FingerprintSHA256(key) != value.Fingerprint {
		return errors.New("verified SSH host public key bytes changed")
	}
	return nil
}

func mpiInterfaceChoice(f diagnosticMPIParticipantFacts, subnet *net.IPNet) error {
	if len(f.Interfaces) == 0 || len(f.Interfaces) > 128 {
		return errors.New("fresh interface observations are required")
	}
	seen := map[string]bool{}
	matched := map[string]bool{}
	haveAddress := false
	for _, iface := range f.Interfaces {
		if !diagnosticToken.MatchString(iface.Name) || len(iface.Name) > 15 || seen[iface.Name] || len(iface.Addresses) > 32 {
			return errors.New("interface observations are malformed or ambiguous")
		}
		seen[iface.Name] = true
		for _, raw := range iface.Addresses {
			ip, _, err := net.ParseCIDR(raw)
			if err != nil {
				return errors.New("interface address observation is malformed")
			}
			if ip.To4() == nil {
				continue
			}
			if iface.Up && subnet.Contains(ip) {
				matched[iface.Name] = true
				if iface.Name == f.Interface && ip.String() == f.CollectiveAddress {
					haveAddress = true
				}
			}
		}
	}
	if len(matched) != 1 || !matched[f.Interface] || !haveAddress {
		return errors.New("fabric subnet does not uniquely select the intended up interface and address")
	}
	return nil
}

func mpiRegistration(target diagnosticManagedTarget, record diagnosticManagedRecord, f diagnosticMPIParticipantFacts) (diagnosticMPIRegistration, error) {
	var r diagnosticMPIRegistration
	if len(target.Registration) == 0 || len(target.Registration) > 192<<10 || len(f.Registration) > 192<<10 || json.Unmarshal(target.Registration, &r) != nil || !sameManagedJSON(target.Registration, f.Registration) {
		return r, errors.New("fresh runtime artifacts differ from the adopted registration")
	}
	if r.SchemaVersion != 1 || r.Kind != "pair-nccl-runtime-candidate-v1" || r.RecipeID != diagnosticRuntimeRecipe || r.OperationID != record.OperationID || r.PlanDigest != target.PlanDigest || r.Attempt != target.Attempt || r.Attempt < 1 || r.Attempt > 3 || r.LinkValidation != "static-elf-resolution-only" {
		return r, errors.New("adopted runtime registration binding is invalid")
	}
	for _, flag := range []*bool{r.ManagerAdopted, r.RuntimeValidated, r.GPUExecuted, r.MPIExecuted} {
		if flag == nil || *flag {
			return r, errors.New("static adoption evidence was relabelled as runtime proof")
		}
	}
	if r.Identity.NodeID != target.NodeID || r.Identity.Principal != target.Principal || r.Identity.UID != f.UID || r.Identity.Home != f.Home || r.Identity.User != f.User || r.Identity.PublicIdentitySHA256 != f.PublicIdentitySHA256 || !diagnosticDigest.MatchString(f.PublicIdentitySHA256) {
		return r, errors.New("fresh account identity differs from the adopted account")
	}
	if len(r.Sources) != 2 || r.Sources["nccl"].URL != "https://github.com/NVIDIA/nccl.git" || r.Sources["nccl"].Commit != "73cf112295c33aee2b895f329f592f2a9b4b0f97" || r.Sources["nccl"].Tag != "v2.30.7-1" || r.Sources["nccl-tests"].URL != "https://github.com/NVIDIA/nccl-tests.git" || r.Sources["nccl-tests"].Commit != "b4d5beebca8a76cf01335f724d154b9b9d394d96" || r.Sources["nccl-tests"].Tag != "v2.20.0" {
		return r, errors.New("runtime source pins differ from the fixed recipe")
	}
	for _, file := range []diagnosticMPIArtifact{r.Binary, r.NCCLLibrary, r.Dependencies["libnccl.so.2"], r.Dependencies["libmpi.so.40"], r.Dependencies["libcudart.so.13"]} {
		if !validMPIArtifact(file) {
			return r, errors.New("adopted binary/library pin is missing or invalid")
		}
	}
	if r.NCCLLibrary != r.Dependencies["libnccl.so.2"] {
		return r, errors.New("NCCL dependency differs from its adopted library")
	}
	switch path.Dir(r.Dependencies["libcudart.so.13"].Path) {
	case "/usr/local/cuda-13.0/lib64", "/usr/local/cuda-13.0/targets/aarch64-linux/lib", "/usr/local/cuda-13.0/targets/sbsa-linux/lib":
	default:
		return r, errors.New("CUDA library differs from the fixed MPI adapter layout")
	}
	return r, nil
}

func mpiCanonical(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(decoded); err != nil {
		return nil, err
	}
	result := bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'})
	// The pinned Python adapter uses ensure_ascii. All fixed fields and current
	// managed layout are ASCII; reject an unsupported spelling, never hash two
	// different encodings as though they were the same public plan.
	for _, b := range result {
		if b >= 128 {
			return nil, errors.New("MPI adapter paths and identifiers require ASCII spelling")
		}
	}
	return result, nil
}

// Pure compilation: no file, service, interface or network effects. Key bytes
// are returned separately and must be consumed once by Root's private stdin
// path, then cleared; never marshal this return tuple or regenerate on replay.
func compileDiagnosticMPIPlan(record diagnosticManagedRecord, facts []diagnosticMPIParticipantFacts, selection diagnosticMPISelection, now time.Time) (profile diagnosticProfile, planJSON json.RawMessage, privatePEM []byte, err error) {
	defer func() {
		if err != nil {
			clear(privatePEM)
			privatePEM = nil
		}
	}()
	if record.SchemaVersion != 1 || record.Owner != diagnosticManagedOwner || !record.Adopted || record.RuntimeValidated || record.RunAvailable || !onboardingID.MatchString(record.OperationID) || !validDiagnosticParticipantCount(len(record.Targets)) || len(facts) != len(record.Targets) || !onboardingID.MatchString(selection.OperationID) || !selection.DedicatedTestWindow {
		return profile, nil, nil, errors.New("MPI compilation requires two or three participants from one adopted build and an explicit test-window decision")
	}
	if selection.OwnerNodeID != record.Targets[0].NodeID {
		return profile, nil, nil, errors.New("MPI coordinator must be the first selected participant")
	}
	address, subnet, parseErr := net.ParseCIDR(selection.Subnet)
	if parseErr != nil || address.To4() == nil || subnet.String() != selection.Subnet {
		return profile, nil, nil, errors.New("fabric subnet must be canonical IPv4 CIDR")
	}
	ones, bits := subnet.Mask.Size()
	if bits != 32 || ones < 1 || ones > 30 || !diagnosticConcreteIPv4(selection.SSHSourceIPv4) {
		return profile, nil, nil, errors.New("unsupported fabric subnet or SSH source")
	}
	byNode := map[string]diagnosticMPIParticipantFacts{}
	for _, f := range facts {
		if byNode[f.NodeID].NodeID != "" {
			return profile, nil, nil, errors.New("duplicate participant facts")
		}
		byNode[f.NodeID] = f
	}
	tools := diagnosticMPISystemToolPaths()
	members := make([]map[string]any, 0, len(record.Targets))
	var owner diagnosticMPIParticipantFacts
	collectives := map[string]bool{}
	userName := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,31}$`)
	gpuName := regexp.MustCompile(`^GPU-[A-Za-z0-9-]{1,80}$`)
	for index, target := range record.Targets {
		f, ok := byNode[target.NodeID]
		if !ok || target.NodeID != target.Principal || f.NodeID != target.NodeID || f.Principal != target.Principal || !diagnosticToken.MatchString(target.NodeID) || (index > 0 && record.Targets[index-1].NodeID >= target.NodeID) || !diagnosticDigest.MatchString(target.ClusterPinSHA256) || f.ClusterPinSHA256 != target.ClusterPinSHA256 || f.SSHAddress != target.Address {
			return profile, nil, nil, errors.New("fresh participant/pin/management address differs from adopted pair")
		}
		if f.ObservedAt <= 0 || f.ObservedAt > now.UnixMilli() || now.UnixMilli()-f.ObservedAt > 30000 || f.UID <= 0 || !userName.MatchString(f.User) || !diagnosticPath.MatchString(f.Home) || f.Home == "/" || !diagnosticManagedPath(f.Home) || !diagnosticConcreteIPv4(f.SSHAddress) || f.SSHPort < 1 || f.SSHPort > 65535 {
			return profile, nil, nil, errors.New("fresh participant account/SSH facts are missing, stale or invalid")
		}
		if err = validateMPIPublicKey(f.SSHHostKey); err != nil {
			return profile, nil, nil, err
		}
		if !target.Local && f.SSHHostKey.Fingerprint != target.SSHHostKeySHA256 {
			return profile, nil, nil, errors.New("SSH host public key does not match the retained pin")
		}
		collective := net.ParseIP(f.CollectiveAddress).To4()
		broadcast := append(net.IP(nil), subnet.IP.To4()...)
		for i := range broadcast {
			broadcast[i] |= ^subnet.Mask[i]
		}
		if !diagnosticConcreteIPv4(f.CollectiveAddress) || !subnet.Contains(collective) || collective.Equal(subnet.IP) || collective.Equal(broadcast) || collectives[f.CollectiveAddress] || !diagnosticToken.MatchString(f.Interface) || len(f.Interface) > 15 || strings.ContainsAny(f.Interface, ":=") || !gpuName.MatchString(f.GPUUUID) {
			return profile, nil, nil, errors.New("selected fabric/GPU facts are invalid or duplicated")
		}
		collectives[f.CollectiveAddress] = true
		if err = mpiInterfaceChoice(f, subnet); err != nil {
			return profile, nil, nil, err
		}
		if !validMPIArtifact(f.Manager) || path.Base(f.Manager.Path) != "nvpair-engine-manager" || len(f.Tools) != len(tools) {
			return profile, nil, nil, errors.New("installed manager and fixed tool facts are required")
		}
		for name, expected := range tools {
			file, ok := f.Tools[name]
			if !ok || file.Path != expected || !validMPIArtifact(file) {
				return profile, nil, nil, fmt.Errorf("fixed installed tool fact is missing or changed: %s", name)
			}
		}
		r, parseErr := mpiRegistration(target, record, f)
		if parseErr != nil {
			return profile, nil, nil, parseErr
		}
		runtime := diagnosticMemberRuntime{BuildOperationID: r.OperationID, BuildPlanDigest: r.PlanDigest, BuildAttempt: r.Attempt, UID: f.UID, Home: f.Home, NCCLLibrary: mpiTool(r.NCCLLibrary), CUDALibrary: mpiTool(r.Dependencies["libcudart.so.13"]), MPILibrary: mpiTool(r.Dependencies["libmpi.so.40"])}
		profile.Members = append(profile.Members, diagnosticMember{NodeID: f.NodeID, Principal: f.Principal, ClusterPinSHA256: target.ClusterPinSHA256, Host: f.SSHAddress, User: f.User, GPU: f.GPUUUID, Interface: f.Interface, Manager: mpiTool(f.Manager), NCCL: mpiTool(r.Binary), SMI: mpiTool(f.Tools["nvidia-smi"]), Runtime: runtime})
		members = append(members, map[string]any{"nodeId": f.NodeID, "principal": f.Principal, "uid": f.UID, "user": f.User, "home": f.Home, "sshAddress": f.SSHAddress, "sshPort": f.SSHPort, "collectiveAddress": f.CollectiveAddress, "interface": f.Interface, "gpuUUID": f.GPUUUID, "hostKey": f.SSHHostKey, "buildOperationId": r.OperationID, "buildAttempt": r.Attempt, "buildPlanDigest": r.PlanDigest, "manager": f.Manager, "binary": r.Binary, "ncclLibrary": r.NCCLLibrary, "cudaLibrary": r.Dependencies["libcudart.so.13"], "mpiLibrary": r.Dependencies["libmpi.so.40"], "tools": f.Tools})
		if f.NodeID == selection.OwnerNodeID {
			owner = f
		}
	}
	if owner.NodeID == "" {
		return profile, nil, nil, errors.New("MPI coordinator must be one adopted participant")
	}
	sourcePresent := false
	for _, iface := range owner.Interfaces {
		if iface.Up {
			for _, raw := range iface.Addresses {
				ip, _, e := net.ParseCIDR(raw)
				if e == nil && ip.String() == selection.SSHSourceIPv4 {
					sourcePresent = true
				}
			}
		}
	}
	if !sourcePresent {
		return profile, nil, nil, errors.New("reviewed SSH source is not an observed up local address")
	}
	public, private, keyErr := ed25519.GenerateKey(rand.Reader)
	if keyErr != nil {
		return profile, nil, nil, keyErr
	}
	defer clear(private)
	key, keyErr := ssh.NewPublicKey(public)
	if keyErr != nil {
		return profile, nil, nil, keyErr
	}
	block, keyErr := ssh.MarshalPrivateKey(private, "pair-nccl-smoke:"+selection.OperationID)
	if keyErr != nil {
		return profile, nil, nil, keyErr
	}
	defer clear(block.Bytes)
	privatePEM = pem.EncodeToMemory(block)
	publicKey := diagnosticBootstrapPublicKey{Algorithm: key.Type(), Blob: base64.StdEncoding.EncodeToString(key.Marshal()), Fingerprint: ssh.FingerprintSHA256(key)}
	root := path.Join(owner.Home, ".local/share/pair-nccl-smoke-v1", selection.OperationID)
	var knownHosts strings.Builder
	for _, target := range record.Targets {
		f := byNode[target.NodeID]
		host := f.SSHAddress
		if f.SSHPort != 22 {
			host = fmt.Sprintf("[%s]:%d", host, f.SSHPort)
		}
		fmt.Fprintf(&knownHosts, "%s %s %s\n", host, f.SSHHostKey.Algorithm, f.SSHHostKey.Blob)
	}
	knownSum := sha256.Sum256([]byte(knownHosts.String()))
	profile.Transport = "socket"
	profile.GroupID = "pair-smoke-" + selection.OperationID
	profile.Label = "Managed NCCL Socket baseline"
	profile.OwnerNodeID = selection.OwnerNodeID
	profile.DedicatedTestWindow = selection.DedicatedTestWindow
	profile.MPI = mpiTool(owner.Tools["mpirun"])
	profile.SSH = mpiTool(owner.Tools["ssh"])
	profile.IdentityFile = root + "/identity.pub"
	profile.KnownHosts = diagnosticTool{Path: root + "/known_hosts", SHA256: hex.EncodeToString(knownSum[:])}
	profile.Bootstrap = diagnosticBootstrapProfile{OperationID: selection.OperationID, PublicKey: publicKey, AgentSocket: root + "/agent.sock", PublicIdentity: profile.IdentityFile, Subnet: selection.Subnet, SSHSourceIPv4: selection.SSHSourceIPv4}
	if err = validateDiagnosticBootstrapProfile(profile); err != nil {
		return profile, nil, privatePEM, err
	}
	profileRaw, marshalErr := json.Marshal(profile)
	if marshalErr != nil {
		return profile, nil, privatePEM, marshalErr
	}
	profileSum := sha256.Sum256(profileRaw)
	recipe := diagnosticMPIQuickRecipe
	if len(record.Targets) == 3 {
		recipe = diagnosticMPITripleRecipe
	}
	body := map[string]any{"schemaVersion": 1, "recipeId": recipe, "operationId": selection.OperationID, "groupId": profile.GroupID, "profileDigest": hex.EncodeToString(profileSum[:]), "ownerNodeId": selection.OwnerNodeID, "createdAt": now.UnixMilli(), "expiresAt": now.Add(diagnosticLease).UnixMilli(), "subnet": selection.Subnet, "sshSourceIPv4": selection.SSHSourceIPv4, "operationPublicKey": publicKey, "members": members,
		"limits": map[string]int{"ranks": len(record.Targets), "leaseSeconds": 120, "mpiSeconds": 90, "unitSeconds": 105, "stopSeconds": 10, "agentSeconds": 105, "maxOutputBytes": 1048576, "maxAuthorizedKeysBytes": 1048576}}
	canonical, canonicalErr := mpiCanonical(body)
	if canonicalErr != nil {
		return profile, nil, privatePEM, canonicalErr
	}
	sum := sha256.Sum256(canonical)
	body["planDigest"] = hex.EncodeToString(sum[:])
	raw, marshalErr := mpiCanonical(body)
	if marshalErr != nil || len(raw) > 96<<10 {
		if marshalErr == nil {
			marshalErr = errors.New("fixed MPI plan exceeds its bound")
		}
		return profile, nil, privatePEM, marshalErr
	}
	planJSON = raw
	r := diagnosticParticipantRequest{GroupID: profile.GroupID, OperationID: selection.OperationID, ProfileDigest: hex.EncodeToString(profileSum[:]), ExpiresAt: now.Add(diagnosticLease).UnixMilli(), BootstrapPlanDigest: hex.EncodeToString(sum[:])}
	if _, err = validateDiagnosticMPIBinding(profile, planJSON, r, false); err != nil {
		return profile, nil, privatePEM, err
	}
	return profile, planJSON, privatePEM, nil
}
