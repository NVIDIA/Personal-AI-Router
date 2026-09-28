// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func bootstrapProfileFixture(t *testing.T) diagnosticProfile {
	t.Helper()
	// Syntactic public fixture only; no private key is generated or persisted.
	key, err := ssh.NewPublicKey(ed25519.PublicKey(make([]byte, ed25519.PublicKeySize)))
	if err != nil {
		t.Fatal(err)
	}
	tool := func(path string) diagnosticTool { return diagnosticTool{Path: path, SHA256: strings.Repeat("a", 64)} }
	p := diagnosticProfile{Transport: "socket", GroupID: "pair-smoke-" + strings.Repeat("b", 32), OwnerNodeID: "node-a", DedicatedTestWindow: true, IdentityFile: "/home/fixture/identity.pub",
		MPI: tool("/usr/bin/mpirun"), SSH: tool("/usr/bin/ssh"), KnownHosts: tool("/home/fixture/known_hosts"),
		Bootstrap: diagnosticBootstrapProfile{OperationID: strings.Repeat("b", 32), PublicKey: diagnosticBootstrapPublicKey{Algorithm: key.Type(), Blob: base64.StdEncoding.EncodeToString(key.Marshal()), Fingerprint: ssh.FingerprintSHA256(key)},
			AgentSocket: "/run/user/1000/pair/agent.sock", PublicIdentity: "/home/fixture/identity.pub", Subnet: "10.50.0.0/24", SSHSourceIPv4: "192.0.2.1"}}
	for i, id := range []string{"node-a", "node-b"} {
		root := "/home/fixture/.local/share/pair-nccl-build-v1/" + strings.Repeat("c", 32) + "/attempt-0001/runtime"
		m := diagnosticMember{NodeID: id, Principal: id, ClusterPinSHA256: strings.Repeat("f", 64), Host: []string{"192.0.2.1", "192.0.2.2"}[i], User: "fixture", GPU: "GPU-" + id, Interface: "enp1",
			Manager: tool("/home/fixture/.local/share/Nvidia Corporation/Personal AI Router/nvpair-engine-manager"), NCCL: tool(root + "/bin/all_reduce_perf"), SMI: tool("/usr/bin/nvidia-smi"),
			Runtime: diagnosticMemberRuntime{BuildOperationID: strings.Repeat("c", 32), BuildPlanDigest: strings.Repeat("d", 64), BuildAttempt: 1, UID: 1000, Home: "/home/fixture",
				NCCLLibrary: tool(root + "/lib/libnccl.so.2"), CUDALibrary: tool("/usr/local/cuda-13.0/targets/aarch64-linux/lib/libcudart.so.13.0.88"), MPILibrary: tool("/usr/lib/aarch64-linux-gnu/libmpi.so.40.30.4")}}
		p.Members = append(p.Members, m)
	}
	return p
}

func TestDiagnosticBootstrapValueFieldsPreserveLegacyEncodingAndMemberEquality(t *testing.T) {
	legacy := diagnosticTestProfile()
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"runtime"`)) || bytes.Contains(raw, []byte(`"bootstrap"`)) || bytes.Contains(raw, []byte(`"clusterPinSha256"`)) {
		t.Fatal("zero managed fields changed legacy profile bytes")
	}
	if err := legacy.validate(); err != nil {
		t.Fatal(err)
	}
	managed := bootstrapProfileFixture(t)
	raw, err = json.Marshal(managed)
	if err != nil {
		t.Fatal(err)
	}
	var restored diagnosticProfile
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Members[0] != managed.Members[0] || restored.Bootstrap != managed.Bootstrap {
		t.Fatal("value binding did not survive JSON decode")
	}
	if err := restored.validate(); err != nil {
		t.Fatal(err)
	}
	legacy.Members[0].Manager.Path = "/opt/Personal AI Router/manager"
	if legacy.validate() == nil {
		t.Fatal("managed path support broadened legacy path admission")
	}
}

func TestDiagnosticBootstrapRawProfileDigestAndOperationBinding(t *testing.T) {
	p := bootstrapProfileFixture(t)
	base := t.TempDir()
	dir := filepath.Join(base, "diagnostic-runs", p.Bootstrap.OperationID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	filename := filepath.Join(dir, "profile.json")
	if err := os.WriteFile(filename, raw, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := loadDiagnosticBootstrapProfile(base, p.GroupID, p.Bootstrap.OperationID, digest)
	if err != nil || !reflect.DeepEqual(got, p) {
		t.Fatalf("raw profile load failed: %v", err)
	}
	indented, _ := json.MarshalIndent(p, "", "  ")
	if err := os.WriteFile(filename, indented, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDiagnosticBootstrapProfile(base, p.GroupID, p.Bootstrap.OperationID, digest); err == nil {
		t.Fatal("reformatted bytes satisfied the original raw digest")
	}
	if err := os.WriteFile(filename, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDiagnosticBootstrapProfile(base, "other", p.Bootstrap.OperationID, digest); err == nil {
		t.Fatal("foreign group accepted")
	}
	if _, err := loadDiagnosticBootstrapProfile(base, p.GroupID, "../escape", digest); err == nil {
		t.Fatal("unsafe operation selector accepted")
	}
	for _, body := range [][]byte{append(append([]byte(nil), raw...), []byte(`{}`)...), bytes.Repeat([]byte(" "), (128<<10)+1)} {
		sum := sha256.Sum256(body)
		if err := os.WriteFile(filename, body, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadDiagnosticBootstrapProfile(base, p.GroupID, p.Bootstrap.OperationID, hex.EncodeToString(sum[:])); err == nil {
			t.Fatal("trailing/oversized profile accepted")
		}
	}
}

func TestDiagnosticBootstrapRejectsPartialIdentityAndUnboundRuntime(t *testing.T) {
	mutations := []func(*diagnosticProfile){
		func(p *diagnosticProfile) { p.Members[0].ClusterPinSHA256 = "" },
		func(p *diagnosticProfile) { p.Bootstrap.PublicKey.Fingerprint = "SHA256:other" },
		func(p *diagnosticProfile) { p.Members[1].NodeID = p.Members[0].NodeID },
		func(p *diagnosticProfile) { p.Members[0].Runtime.UID = 0 },
		func(p *diagnosticProfile) { p.Members[0].Runtime.BuildAttempt = 2 },
		func(p *diagnosticProfile) { p.Members[0].Runtime.NCCLLibrary.Path = "/tmp/libnccl.so.2" },
		func(p *diagnosticProfile) { p.Members[0].Runtime.CUDALibrary.SHA256 = "missing" },
		func(p *diagnosticProfile) { p.Members[0].Manager.Path = "/opt/../tmp/manager" },
		func(p *diagnosticProfile) { p.Members[0].Manager.Path = "/opt/manager\nother" },
		func(p *diagnosticProfile) { p.Bootstrap.Subnet = "0.0.0.0/0" },
		func(p *diagnosticProfile) { p.Bootstrap.SSHSourceIPv4 = "127.0.0.1" },
		func(p *diagnosticProfile) { p.DedicatedTestWindow = false },
	}
	for i, mutate := range mutations {
		p := bootstrapProfileFixture(t)
		mutate(&p)
		if p.validate() == nil {
			t.Fatalf("mutation %d was accepted", i)
		}
	}
}

func TestDiagnosticBootstrapMemberChecksActualAccountAndEveryPinnedFile(t *testing.T) {
	m := bootstrapProfileFixture(t).Members[0]
	var checked []diagnosticTool
	verify := func(tool diagnosticTool) error { checked = append(checked, tool); return nil }
	if err := verifyDiagnosticManagedMember(m, 1000, "/home/fixture", verify); err != nil {
		t.Fatal(err)
	}
	want := []diagnosticTool{m.Manager, m.NCCL, m.SMI, m.Runtime.NCCLLibrary, m.Runtime.CUDALibrary, m.Runtime.MPILibrary}
	if !reflect.DeepEqual(checked, want) {
		t.Fatal("a pinned runtime file was omitted")
	}
	for _, account := range []struct {
		uid  int
		home string
	}{{0, "/home/fixture"}, {1001, "/home/fixture"}, {1000, "/home/other"}} {
		checked = nil
		if verifyDiagnosticManagedMember(m, account.uid, account.home, verify) == nil || len(checked) != 0 {
			t.Fatal("wrong actual account reached file checks")
		}
	}
	for _, bad := range want {
		if verifyDiagnosticManagedMember(m, 1000, "/home/fixture", func(tool diagnosticTool) error {
			if tool == bad {
				return errors.New("fixture changed bytes")
			}
			return nil
		}) == nil {
			t.Fatal("a changed pinned file was ignored")
		}
	}
}

func TestDiagnosticBootstrapRankEnvironmentKeepsRendezvousAndForcesSocketPolicy(t *testing.T) {
	p := bootstrapProfileFixture(t)
	inherited := []string{"OMPI_COMM_WORLD_RANK=1", "PMIX_NAMESPACE=1234", "PMIX_SERVER_URI41=1234.0;tcp://10.50.0.1:40000", "PMI_FD=9", "OMPI_MCA_orte_local_daemon_uri=local", "OMPI_MCA_ess_base_jobid=1234",
		"OMPI_MCA_pml=ucx", "OMPI_MCA_mca_base_component_path=/tmp/foreign", "OMPI_MCA_mca_base_env_list=LD_PRELOAD=/tmp/foreign", "OMPI_MCA_ess_base_arbitrary=/tmp/foreign", "PMIX_MCA_mca_base_component_path=/tmp/foreign",
		"LD_PRELOAD=/tmp/foreign", "LD_LIBRARY_PATH=/tmp/foreign", "NCCL_NET=IB", "NCCL_NET_PLUGIN=foreign", "NCCL_RAS_ENABLE=1", "CUDA_VISIBLE_DEVICES=other", "HOME=/tmp/foreign", "PATH=/tmp/foreign"}
	result, err := diagnosticManagedRankEnvironment(p, p.Members[0], inherited)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, entry := range result {
		k, v, _ := strings.Cut(entry, "=")
		if _, exists := env[k]; exists {
			t.Fatal("duplicate environment key")
		}
		env[k] = v
	}
	for _, name := range []string{"OMPI_COMM_WORLD_RANK", "PMIX_NAMESPACE", "PMIX_SERVER_URI41", "PMI_FD", "OMPI_MCA_orte_local_daemon_uri", "OMPI_MCA_ess_base_jobid"} {
		if env[name] == "" {
			t.Fatalf("lost MPI rendezvous %s", name)
		}
	}
	for _, name := range []string{"LD_PRELOAD", "OMPI_MCA_mca_base_component_path", "OMPI_MCA_mca_base_env_list", "OMPI_MCA_ess_base_arbitrary", "PMIX_MCA_mca_base_component_path"} {
		if _, ok := env[name]; ok {
			t.Fatalf("inherited unsafe policy %s", name)
		}
	}
	for k, v := range map[string]string{"NCCL_NET": "Socket", "NCCL_NET_PLUGIN": "none", "NCCL_IB_DISABLE": "1", "NCCL_RAS_ENABLE": "0", "NCCL_SOCKET_IFNAME": "=enp1", "NCCL_SOCKET_NTHREADS": "1", "NCCL_NSOCKS_PERTHREAD": "1", "OMPI_MCA_pml": "ob1", "OMPI_MCA_ess": "pmi", "OMPI_MCA_pmix": "^s1,s2,cray", "PMIX_MCA_mca_base_param_files": "none", "OMPI_MCA_btl_tcp_if_include": "10.50.0.0/24", "OMPI_MCA_oob_tcp_if_include": "10.50.0.0/24", "HOME": "/home/fixture"} {
		if env[k] != v {
			t.Fatalf("%s=%q want %q", k, env[k], v)
		}
	}
	if strings.Contains(env["LD_LIBRARY_PATH"], "/tmp/foreign") || !strings.HasPrefix(env["LD_LIBRARY_PATH"], strings.TrimSuffix(p.Members[0].Runtime.NCCLLibrary.Path, "/libnccl.so.2")) {
		t.Fatal("verified runtime library directory was not selected")
	}
	member := p.Members[0]
	member.GPU = "GPU-other"
	if _, err := diagnosticManagedRankEnvironment(p, member, inherited); err == nil {
		t.Fatal("substituted rank member accepted")
	}
}
