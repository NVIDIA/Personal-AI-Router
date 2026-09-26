// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

// Value fields keep diagnosticMember and its lease comparable after JSON decode.
type diagnosticMemberRuntime struct {
	BuildOperationID string         `json:"buildOperationId"`
	BuildPlanDigest  string         `json:"buildPlanDigest"`
	BuildAttempt     int            `json:"buildAttempt"`
	UID              int            `json:"uid"`
	Home             string         `json:"home"`
	NCCLLibrary      diagnosticTool `json:"ncclLibrary"`
	CUDALibrary      diagnosticTool `json:"cudaLibrary"`
	MPILibrary       diagnosticTool `json:"mpiLibrary"`
}

type diagnosticBootstrapPublicKey struct {
	Algorithm   string `json:"algorithm"`
	Blob        string `json:"blob"`
	Fingerprint string `json:"fingerprint"`
}

type diagnosticBootstrapProfile struct {
	OperationID    string                       `json:"operationId"`
	PublicKey      diagnosticBootstrapPublicKey `json:"publicKey"`
	AgentSocket    string                       `json:"agentSocket"`
	PublicIdentity string                       `json:"publicIdentity"`
	Subnet         string                       `json:"subnet"`
	SSHSourceIPv4  string                       `json:"sshSourceIPv4"`
}

var diagnosticManagedCUDAName = regexp.MustCompile(`^libcudart\.so\.13(?:\.[0-9]+)*$`)
var diagnosticManagedMPIName = regexp.MustCompile(`^libmpi\.so\.40(?:\.[0-9]+)*$`)

// Managed paths are passed only as argv/file paths, never shell source. Keep
// canonical POSIX spelling and reject delimiters that could alter loader search.
func diagnosticManagedPath(value string) bool {
	if len(value) > 4096 || !utf8.ValidString(value) || value == "/" || !path.IsAbs(value) || path.Clean(value) != value || strings.ContainsAny(value, "\\:") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func diagnosticManagedToolValid(tool diagnosticTool) bool {
	return diagnosticManagedPath(tool.Path) && diagnosticDigest.MatchString(tool.SHA256)
}

func diagnosticConcreteIPv4(value string) bool {
	ip := net.ParseIP(value)
	return ip != nil && ip.To4() != nil && ip.String() == value && !ip.IsLoopback() && !ip.IsMulticast() && !ip.IsUnspecified() && value != "255.255.255.255"
}

func validateDiagnosticBootstrapProfile(p diagnosticProfile) error {
	b := p.Bootstrap
	if p.Transport != "socket" || !p.DedicatedTestWindow || !validDiagnosticParticipantCount(len(p.Members)) || !diagnosticToken.MatchString(p.GroupID) || !onboardingID.MatchString(b.OperationID) {
		return errors.New("managed socket smoke requires its exact operation, reserved window and two or three participants")
	}
	if !diagnosticManagedPath(b.AgentSocket) || !diagnosticManagedPath(b.PublicIdentity) || b.AgentSocket == b.PublicIdentity || len(b.AgentSocket) > 107 || !diagnosticConcreteIPv4(b.SSHSourceIPv4) {
		return errors.New("managed SSH bootstrap paths or source are invalid")
	}
	if p.IdentityFile != b.PublicIdentity {
		return errors.New("managed SSH identity must be the declared public agent identity")
	}
	address, subnet, err := net.ParseCIDR(b.Subnet)
	if err != nil || address.To4() == nil || subnet.String() != b.Subnet {
		return errors.New("managed MPI subnet must be canonical IPv4 CIDR")
	}
	ones, bits := subnet.Mask.Size()
	if bits != 32 || ones < 1 || ones > 30 {
		return errors.New("managed MPI subnet prefix is unsupported")
	}
	if b.PublicKey.Algorithm != ssh.KeyAlgoED25519 || len(b.PublicKey.Blob) > 8192 {
		return errors.New("managed bootstrap requires the reviewed Ed25519 public key")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(b.PublicKey.Blob)
	if err != nil {
		return errors.New("managed public key encoding is invalid")
	}
	key, err := ssh.ParsePublicKey(raw)
	if err != nil || key.Type() != b.PublicKey.Algorithm || ssh.FingerprintSHA256(key) != b.PublicKey.Fingerprint {
		return errors.New("managed public key fingerprint changed")
	}
	seen := map[string]bool{}
	for _, m := range p.Members {
		if !diagnosticToken.MatchString(m.NodeID) || m.Principal != m.NodeID || !diagnosticDigest.MatchString(m.ClusterPinSHA256) || seen[m.NodeID] || !diagnosticConcreteIPv4(m.Host) || !diagnosticToken.MatchString(m.User) || !strings.HasPrefix(m.GPU, "GPU-") || !diagnosticToken.MatchString(m.GPU) || !diagnosticToken.MatchString(m.Interface) || len(m.Interface) > 15 || strings.ContainsAny(m.Interface, ":=") {
			return errors.New("managed participant identity, control address, GPU or interface is invalid")
		}
		seen[m.NodeID] = true
		r := m.Runtime
		if !onboardingID.MatchString(r.BuildOperationID) || !diagnosticDigest.MatchString(r.BuildPlanDigest) || r.BuildAttempt < 1 || r.BuildAttempt > 3 || r.UID <= 0 || !diagnosticManagedPath(r.Home) {
			return errors.New("managed runtime build/account binding is incomplete")
		}
		root := path.Join(r.Home, ".local/share/pair-nccl-build-v1", r.BuildOperationID, fmt.Sprintf("attempt-%04d/runtime", r.BuildAttempt))
		if m.NCCL.Path != root+"/bin/all_reduce_perf" || r.NCCLLibrary.Path != root+"/lib/libnccl.so.2" {
			return errors.New("managed NCCL artifacts escaped the adopted build attempt")
		}
		for _, tool := range []diagnosticTool{m.Manager, m.NCCL, m.SMI, r.NCCLLibrary, r.CUDALibrary, r.MPILibrary} {
			if !diagnosticManagedToolValid(tool) {
				return errors.New("managed runtime tools require canonical absolute paths and exact hashes")
			}
		}
		if !strings.HasPrefix(r.CUDALibrary.Path, "/usr/local/cuda-13.0/") || !diagnosticManagedCUDAName.MatchString(path.Base(r.CUDALibrary.Path)) || path.Dir(r.MPILibrary.Path) != "/usr/lib/aarch64-linux-gnu" || !diagnosticManagedMPIName.MatchString(path.Base(r.MPILibrary.Path)) {
			return errors.New("managed CUDA/MPI library family changed")
		}
	}
	if !seen[p.OwnerNodeID] {
		return errors.New("managed coordinator must be an explicit participant")
	}
	for _, tool := range []diagnosticTool{p.MPI, p.SSH, p.KnownHosts} {
		if !diagnosticManagedToolValid(tool) {
			return errors.New("managed launcher and known-hosts bindings are invalid")
		}
	}
	return nil
}

func loadDiagnosticBootstrapProfile(base, group, operationID, profileSHA256 string) (diagnosticProfile, error) {
	var p diagnosticProfile
	if base == "" || !diagnosticToken.MatchString(group) || !onboardingID.MatchString(operationID) || !diagnosticDigest.MatchString(profileSHA256) {
		return p, errors.New("invalid managed profile selector")
	}
	filename := filepath.Join(base, "diagnostic-runs", operationID, "profile.json")
	before, err := os.Lstat(filename)
	if err != nil || !before.Mode().IsRegular() || before.Size() > 128<<10 {
		return p, errors.New("managed profile is not a bounded regular file")
	}
	f, err := os.Open(filename)
	if err != nil {
		return p, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		return p, errors.New("managed profile changed while opening")
	}
	if err = diagnosticProfileOwnership(f); err != nil {
		return p, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, (128<<10)+1))
	if err != nil || len(raw) > 128<<10 {
		return p, errors.New("managed profile exceeds its read bound")
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != profileSHA256 {
		return p, errors.New("managed profile raw bytes differ from the admitted digest")
	}
	if strictDiagnosticJSON(raw, &p) != nil || p.GroupID != group || p.Bootstrap.OperationID != operationID {
		return p, errors.New("managed profile operation binding changed")
	}
	if err = validateDiagnosticBootstrapProfile(p); err != nil {
		return p, err
	}
	return p, nil
}

// Root may reuse this verifier during managed preflight. Legacy tool.verify
// retains its original safe-path restriction and 256 MiB bound.
func verifyDiagnosticManagedTool(tool diagnosticTool) error {
	if !diagnosticManagedToolValid(tool) {
		return errors.New("invalid managed tool binding")
	}
	resolved, before, err := diagnosticManagedTrustedInput(tool.Path)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > 2<<30 {
		return errors.New("managed tool is not a bounded regular file")
	}
	f, err := diagnosticManagedTrustOpen(resolved)
	if err != nil {
		return errDiagnosticManagedTrust
	}
	defer f.Close()
	after, err := f.Stat()
	beforeUID, beforeKnown := diagnosticManagedTrustFileUID(before)
	afterUID, afterKnown := diagnosticManagedTrustFileUID(after)
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Mode().Perm()&0022 != 0 || !beforeKnown || !afterKnown || beforeUID != afterUID {
		return errors.New("managed tool changed while opening")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, (2<<30)+1))
	if err != nil || n > 2<<30 {
		return errors.New("managed tool read failed or exceeded its bound")
	}
	if hex.EncodeToString(h.Sum(nil)) != tool.SHA256 {
		return errors.New("managed tool bytes changed")
	}
	final, err := f.Stat()
	if err != nil || !os.SameFile(before, final) || final.Size() != before.Size() || n != before.Size() {
		return errors.New("managed tool changed while verifying")
	}
	finalPath, finalInfo, err := diagnosticManagedTrustedInput(tool.Path)
	if err != nil {
		return err
	}
	if finalPath != resolved || !os.SameFile(final, finalInfo) {
		return errors.New("managed tool path changed while verifying")
	}
	return nil
}

func verifyDiagnosticManagedMember(m diagnosticMember, uid int, home string, verify func(diagnosticTool) error) error {
	if m.Runtime.UID <= 0 || uid != m.Runtime.UID || home != m.Runtime.Home || verify == nil {
		return errors.New("managed rank requires the admitted UID and account home")
	}
	for _, tool := range []diagnosticTool{m.Manager, m.NCCL, m.SMI, m.Runtime.NCCLLibrary, m.Runtime.CUDALibrary, m.Runtime.MPILibrary} {
		if !diagnosticManagedToolValid(tool) {
			return errors.New("managed rank has an invalid pinned file")
		}
		if err := verify(tool); err != nil {
			return err
		}
	}
	return nil
}

func diagnosticManagedRankEnvironment(p diagnosticProfile, m diagnosticMember, inherited []string) ([]string, error) {
	if err := validateDiagnosticBootstrapProfile(p); err != nil {
		return nil, err
	}
	current, ok := p.member(m.NodeID)
	if !ok || current != m {
		return nil, errors.New("managed rank is not the exact admitted member")
	}
	env := map[string]string{}
	if len(inherited) > 1024 {
		return nil, errors.New("MPI environment exceeds its entry bound")
	}
	total := 0
	for _, entry := range inherited {
		total += len(entry)
		if total > 128<<10 {
			return nil, errors.New("MPI environment exceeds its byte bound")
		}
		name, value, ok := strings.Cut(entry, "=")
		if !ok || len(entry) > 16384 || strings.ContainsRune(value, 0) {
			continue
		}
		// Keep runtime rendezvous/rank variables, never inherited MCA policy,
		// component paths or environment-injection directives.
		if (strings.HasPrefix(name, "OMPI_COMM_WORLD_") || strings.HasPrefix(name, "OMPI_UNIVERSE_") || name == "OMPI_APP_CTX_NUM_PROCS" || name == "OMPI_FIRST_RANKS" || name == "OMPI_FILE_LOCATION" || strings.HasPrefix(name, "PMIX_") || strings.HasPrefix(name, "PMI_")) && !strings.Contains(name, "MCA_") {
			env[name] = value
		}
		if name == "OMPI_MCA_ess_base_jobid" || name == "OMPI_MCA_ess_base_vpid" || name == "OMPI_MCA_orte_hnp_uri" || name == "OMPI_MCA_orte_local_daemon_uri" || name == "OMPI_MCA_orte_ess_node_rank" || name == "OMPI_MCA_orte_ess_jobid" || name == "OMPI_MCA_orte_ess_vpid" || name == "OMPI_MCA_orte_num_nodes" || name == "OMPI_MCA_orte_ess_num_procs" || name == "OMPI_MCA_orte_app_num" || name == "OMPI_NUM_APP_CTX" {
			env[name] = value
		}
	}
	for k, v := range map[string]string{"PATH": "/usr/bin:/bin", "HOME": m.Runtime.Home, "CUDA_VISIBLE_DEVICES": m.GPU, "LD_LIBRARY_PATH": path.Dir(m.Runtime.NCCLLibrary.Path) + ":" + path.Dir(m.Runtime.CUDALibrary.Path), "NCCL_NET": "Socket", "NCCL_NET_PLUGIN": "none", "NCCL_IB_DISABLE": "1", "NCCL_RAS_ENABLE": "0", "NCCL_SOCKET_IFNAME": "=" + m.Interface, "NCCL_SOCKET_NTHREADS": "1", "NCCL_NSOCKS_PERTHREAD": "1", "NCCL_DEBUG": "WARN", "OMPI_MCA_pml": "ob1", "OMPI_MCA_btl": "self,tcp", "OMPI_MCA_btl_tcp_if_include": p.Bootstrap.Subnet, "OMPI_MCA_oob_tcp_if_include": p.Bootstrap.Subnet, "OMPI_MCA_mca_base_param_files": "/dev/null", "OMPI_MCA_ess": "pmi", "OMPI_MCA_pmix": "^s1,s2,cray", "PMIX_MCA_mca_base_param_files": "none"} {
		env[k] = v
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out, nil
}
