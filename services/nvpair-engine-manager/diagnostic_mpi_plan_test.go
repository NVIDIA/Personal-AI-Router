// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func mpiPlanFixture(t *testing.T) (diagnosticManagedRecord, []diagnosticMPIParticipantFacts, diagnosticMPISelection, time.Time) {
	return mpiPlanCountFixture(t, 2)
}

func mpiPlanCountFixture(t *testing.T, count int) (diagnosticManagedRecord, []diagnosticMPIParticipantFacts, diagnosticMPISelection, time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	p := bootstrapProfileFixture(t)
	if count == 3 {
		third := p.Members[1]
		third.NodeID, third.Principal, third.Host, third.GPU = "node-c", "node-c", "192.0.2.3", "GPU-node-c"
		p.Members = append(p.Members, third)
	} else if count != 2 {
		t.Fatal("fixture supports only the two closed participant counts")
	}
	record := diagnosticManagedRecord{SchemaVersion: 1, Owner: diagnosticManagedOwner, OperationID: strings.Repeat("c", 32), ReviewID: strings.Repeat("b", 32), GroupID: "pair-recipe/nccl-fixture", ApprovedRevision: 7, AdoptedAt: now.Add(-time.Minute).UnixMilli(), Adopted: true}
	file := func(v diagnosticTool) diagnosticMPIArtifact {
		return diagnosticMPIArtifact{Path: v.Path, SHA256: v.SHA256, Size: 4096}
	}
	facts := []diagnosticMPIParticipantFacts{}
	for i, m := range p.Members {
		identity := map[string]any{"nodeId": m.NodeID, "principal": m.Principal, "uid": m.Runtime.UID, "user": m.User, "home": m.Runtime.Home, "publicIdentitySha256": strings.Repeat("e", 64)}
		r := map[string]any{"schemaVersion": 1, "kind": "pair-nccl-runtime-candidate-v1", "recipeId": diagnosticRuntimeRecipe, "operationId": record.OperationID, "planDigest": m.Runtime.BuildPlanDigest, "attempt": 1,
			"identity": identity, "sources": map[string]any{"nccl": map[string]string{"url": "https://github.com/NVIDIA/nccl.git", "commit": "73cf112295c33aee2b895f329f592f2a9b4b0f97", "tag": "v2.30.7-1"}, "nccl-tests": map[string]string{"url": "https://github.com/NVIDIA/nccl-tests.git", "commit": "b4d5beebca8a76cf01335f724d154b9b9d394d96", "tag": "v2.20.0"}},
			"binary": file(m.NCCL), "ncclLibrary": file(m.Runtime.NCCLLibrary), "dependencies": map[string]any{"libnccl.so.2": file(m.Runtime.NCCLLibrary), "libcudart.so.13": file(m.Runtime.CUDALibrary), "libmpi.so.40": file(m.Runtime.MPILibrary)},
			"managerAdopted": false, "runtimeValidated": false, "mpiExecuted": false, "gpuExecuted": false, "linkValidation": "static-elf-resolution-only"}
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		target := diagnosticManagedTarget{NodeID: m.NodeID, Principal: m.Principal, Address: m.Host, ClusterPinSHA256: strings.Repeat("f", 64), SSHHostKeySHA256: p.Bootstrap.PublicKey.Fingerprint, PlanDigest: m.Runtime.BuildPlanDigest, Attempt: 1, ArtifactObservedAt: now.Add(-time.Minute).Format(time.RFC3339Nano), Registration: raw}
		record.Targets = append(record.Targets, target)
		tools := map[string]diagnosticMPIArtifact{}
		for name, toolPath := range diagnosticMPISystemToolPaths() {
			tools[name] = diagnosticMPIArtifact{Path: toolPath, SHA256: strings.Repeat("a", 64), Size: 4096}
		}
		iface := []string{"wlan0", "wlP9s9", "enp3"}[i]
		facts = append(facts, diagnosticMPIParticipantFacts{NodeID: m.NodeID, Principal: m.Principal, ClusterPinSHA256: target.ClusterPinSHA256, ObservedAt: now.UnixMilli(), UID: m.Runtime.UID, User: m.User, Home: m.Runtime.Home, PublicIdentitySHA256: strings.Repeat("e", 64),
			SSHAddress: m.Host, SSHPort: 22, SSHHostKey: p.Bootstrap.PublicKey, Manager: file(m.Manager), Tools: tools, Interface: iface, CollectiveAddress: m.Host, GPUUUID: m.GPU,
			Interfaces: []diagnosticMPIInterfaceObservation{{Name: iface, Up: true, Addresses: []string{m.Host + "/24"}}}, Registration: append([]byte(nil), raw...)})
	}
	selection := diagnosticMPISelection{OperationID: strings.Repeat("9", 32), OwnerNodeID: p.OwnerNodeID, Subnet: "192.0.2.0/24", SSHSourceIPv4: "192.0.2.1", DedicatedTestWindow: true}
	return record, facts, selection, now
}

func TestDiagnosticMPIPlanTripleBindsEveryOriginalParticipant(t *testing.T) {
	record, facts, selection, now := mpiPlanCountFixture(t, 3)
	// Fresh observation arrival order must not change the approved rank order.
	facts[0], facts[2] = facts[2], facts[0]
	p, raw, key, err := compileDiagnosticMPIPlan(record, facts, selection, now)
	defer clear(key)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		RecipeID string `json:"recipeId"`
		Members  []struct {
			NodeID string `json:"nodeId"`
		} `json:"members"`
		Limits map[string]int `json:"limits"`
	}
	if json.Unmarshal(raw, &body) != nil || body.RecipeID != diagnosticMPITripleRecipe || body.Limits["ranks"] != 3 || len(body.Members) != 3 || len(p.Members) != 3 {
		t.Fatal("triple compiler lost its recipe/cardinality binding")
	}
	for i, target := range record.Targets {
		if body.Members[i].NodeID != target.NodeID || p.Members[i].NodeID != target.NodeID || p.Members[i].ClusterPinSHA256 != target.ClusterPinSHA256 {
			t.Fatal("triple rank order or original certificate binding changed")
		}
	}
	if p.OwnerNodeID != p.Members[0].NodeID || body.Limits["mpiSeconds"] != 90 || body.Limits["unitSeconds"] != 105 || body.Limits["leaseSeconds"] != 120 || body.Limits["stopSeconds"] != 10 {
		t.Fatal("triple compilation changed coordinator or execution caps")
	}
	for _, failure := range []string{"missing third facts", "changed third pin", "different owner", "four targets"} {
		t.Run(failure, func(t *testing.T) {
			r, f, s, at := mpiPlanCountFixture(t, 3)
			switch failure {
			case "missing third facts":
				f = f[:2]
			case "changed third pin":
				f[2].ClusterPinSHA256 = strings.Repeat("0", 64)
			case "different owner":
				s.OwnerNodeID = "node-c"
			case "four targets":
				r.Targets = append(r.Targets, r.Targets[2])
				f = append(f, f[2])
			}
			_, rejected, private, err := compileDiagnosticMPIPlan(r, f, s, at)
			defer clear(private)
			if err == nil || len(rejected) != 0 || len(private) != 0 {
				t.Fatal("invalid triple observations produced a usable plan or identity")
			}
		})
	}
}

func TestDiagnosticMPIPlanCompilesAdoptedPairWithMemoryOnlyMatchingIdentity(t *testing.T) {
	record, facts, selection, now := mpiPlanFixture(t)
	p, raw, private, err := compileDiagnosticMPIPlan(record, facts, selection, now)
	defer clear(private)
	if err != nil {
		t.Fatal(err)
	}
	if p.Label != "Managed NCCL Socket baseline" || p.GroupID != "pair-smoke-"+selection.OperationID || len(p.Members) != 2 {
		t.Fatal("fixed baseline/profile projection changed")
	}
	if p.Members[0].ClusterPinSHA256 != record.Targets[0].ClusterPinSHA256 || p.Members[1].ClusterPinSHA256 != record.Targets[1].ClusterPinSHA256 {
		t.Fatal("profile dropped the admitted certificate generation")
	}
	if p.Members[0].Host != facts[0].SSHAddress || p.Members[0].NCCL.Path == "" || p.Members[0].Runtime.NCCLLibrary != mpiTool(mustMPIRegistration(t, record, facts, 0).NCCLLibrary) {
		t.Fatal("compiler did not derive runtime pins from adoption")
	}
	signer, err := ssh.ParsePrivateKey(private)
	if err != nil {
		t.Fatal("compiler did not return an OpenSSH private identity")
	}
	if ssh.FingerprintSHA256(signer.PublicKey()) != p.Bootstrap.PublicKey.Fingerprint {
		t.Fatal("public/private operation identity mismatch")
	}
	profileRaw, _ := json.Marshal(p)
	if bytes.Contains(profileRaw, []byte("PRIVATE KEY")) || bytes.Contains(raw, []byte("PRIVATE KEY")) || bytes.Contains(raw, private) {
		t.Fatal("private bytes entered public JSON")
	}
	var header diagnosticMPIHeader
	if json.Unmarshal(raw, &header) != nil {
		t.Fatal("invalid public plan")
	}
	sum := sha256.Sum256(profileRaw)
	if header.RecipeID != diagnosticMPIQuickRecipe || header.ProfileDigest != hex.EncodeToString(sum[:]) || header.ExpiresAt-header.CreatedAt != 120000 {
		t.Fatal("profile raw hash or lease cap changed")
	}
	request := diagnosticParticipantRequest{GroupID: p.GroupID, OperationID: selection.OperationID, ProfileDigest: header.ProfileDigest, ExpiresAt: header.ExpiresAt, BootstrapPlanDigest: header.PlanDigest}
	if _, err := validateDiagnosticMPIBinding(p, raw, request, false); err != nil {
		t.Fatal(err)
	}
	if p.IdentityFile != p.Bootstrap.PublicIdentity || !strings.HasSuffix(p.IdentityFile, "/identity.pub") || path.Dir(p.KnownHosts.Path) != path.Dir(p.Bootstrap.AgentSocket) {
		t.Fatal("managed bootstrap used an alternate identity/storage path")
	}
}

func mustMPIRegistration(t *testing.T, r diagnosticManagedRecord, f []diagnosticMPIParticipantFacts, i int) diagnosticMPIRegistration {
	t.Helper()
	v, err := mpiRegistration(r.Targets[i], r, f[i])
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func setMPIPlanCUDAPath(t *testing.T, record *diagnosticManagedRecord, facts []diagnosticMPIParticipantFacts, index int, libraryPath string) {
	t.Helper()
	var registration map[string]any
	if err := json.Unmarshal(record.Targets[index].Registration, &registration); err != nil {
		t.Fatal(err)
	}
	registration["dependencies"].(map[string]any)["libcudart.so.13"].(map[string]any)["path"] = libraryPath
	raw, err := json.Marshal(registration)
	if err != nil {
		t.Fatal(err)
	}
	record.Targets[index].Registration = raw
	facts[index].Registration = append([]byte(nil), raw...)
}

func TestDiagnosticMPIPlanPreservesCUDAResolvedLayoutAndRankEnvironment(t *testing.T) {
	for _, directory := range []string{
		"/usr/local/cuda-13.0/lib64",
		"/usr/local/cuda-13.0/targets/aarch64-linux/lib",
		"/usr/local/cuda-13.0/targets/sbsa-linux/lib",
	} {
		t.Run(directory, func(t *testing.T) {
			record, facts, selection, now := mpiPlanFixture(t)
			libraryPath := directory + "/libcudart.so.13.0.88"
			setMPIPlanCUDAPath(t, &record, facts, 0, libraryPath)
			p, raw, key, err := compileDiagnosticMPIPlan(record, facts, selection, now)
			defer clear(key)
			if err != nil {
				t.Fatal(err)
			}
			if p.Members[0].Runtime.CUDALibrary.Path != libraryPath || !bytes.Contains(raw, []byte(libraryPath)) {
				t.Fatal("compiler replaced the recorded canonical CUDA path")
			}
			for _, member := range p.Members {
				env, err := diagnosticManagedRankEnvironment(p, member, []string{"LD_LIBRARY_PATH=/tmp/foreign"})
				if err != nil {
					t.Fatal(err)
				}
				want := "LD_LIBRARY_PATH=" + path.Dir(member.Runtime.NCCLLibrary.Path) + ":" + path.Dir(member.Runtime.CUDALibrary.Path)
				found := false
				for _, entry := range env {
					if strings.HasPrefix(entry, "LD_LIBRARY_PATH=") {
						if found || entry != want {
							t.Fatalf("rank loader path = %q, want exactly %q", entry, want)
						}
						found = true
					}
				}
				if !found {
					t.Fatal("rank loader path is missing")
				}
			}
		})
	}
}

func TestDiagnosticMPIPlanRejectsCUDAOutsideFixedLayoutsAndFamily(t *testing.T) {
	for _, libraryPath := range []string{
		"/tmp/libcudart.so.13.0.88",
		"/usr/local/cuda-12.8/lib64/libcudart.so.13.0.88",
		"/usr/local/cuda-13.0/lib/libcudart.so.13.0.88",
		"/usr/local/cuda-13.0/targets/x86_64-linux/lib/libcudart.so.13.0.88",
		"/usr/local/cuda-13.0/lib64/stubs/libcudart.so.13.0.88",
		"/usr/local/cuda-13.0/lib64/libcudart.so.12",
		"/usr/local/cuda-13.0/targets/sbsa-linux/lib/libcudart.so.130",
	} {
		t.Run(libraryPath, func(t *testing.T) {
			record, facts, selection, now := mpiPlanFixture(t)
			setMPIPlanCUDAPath(t, &record, facts, 0, libraryPath)
			_, raw, key, err := compileDiagnosticMPIPlan(record, facts, selection, now)
			defer clear(key)
			if err == nil || len(raw) != 0 {
				t.Fatal("unsupported CUDA layout/family produced a usable MPI plan")
			}
		})
	}
}

func TestDiagnosticMPIPlanKeepsManagementSeparateFromAddressedFabric(t *testing.T) {
	record, facts, selection, now := mpiPlanFixture(t)
	selection.Subnet = "10.50.0.0/24"
	for i := range facts {
		facts[i].CollectiveAddress = []string{"10.50.0.1", "10.50.0.2"}[i]
		facts[i].Interface = []string{"enp1", "enp2"}[i]
		facts[i].Interfaces = append(facts[i].Interfaces, diagnosticMPIInterfaceObservation{Name: facts[i].Interface, Up: true, Addresses: []string{facts[i].CollectiveAddress + "/24"}})
	}
	p, raw, key, err := compileDiagnosticMPIPlan(record, facts, selection, now)
	defer clear(key)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Members []struct {
			SSHAddress        string `json:"sshAddress"`
			CollectiveAddress string `json:"collectiveAddress"`
		} `json:"members"`
	}
	if json.Unmarshal(raw, &body) != nil {
		t.Fatal("bad plan")
	}
	if p.Members[0].Host != "192.0.2.1" || body.Members[0].SSHAddress != "192.0.2.1" || body.Members[0].CollectiveAddress != "10.50.0.1" || p.Bootstrap.SSHSourceIPv4 != "192.0.2.1" {
		t.Fatal("management address was substituted by fabric selection")
	}
}

func TestDiagnosticMPIPlanRejectsChangedFactsPinsAndAmbiguousInterfaces(t *testing.T) {
	for _, kind := range []string{"missing", "stale", "pin", "user", "public-identity", "ssh-key", "tool", "registration", "ambiguous", "source", "window"} {
		t.Run(kind, func(t *testing.T) {
			record, facts, selection, now := mpiPlanFixture(t)
			switch kind {
			case "missing":
				facts = facts[:1]
			case "stale":
				facts[0].ObservedAt = now.Add(-time.Minute).UnixMilli()
			case "pin":
				facts[0].ClusterPinSHA256 = strings.Repeat("0", 64)
			case "user":
				facts[0].User = "other"
			case "public-identity":
				facts[0].PublicIdentitySHA256 = strings.Repeat("0", 64)
			case "ssh-key":
				facts[0].SSHHostKey.Fingerprint = "SHA256:other"
			case "tool":
				delete(facts[0].Tools, "ssh-agent")
			case "registration":
				facts[0].Registration = bytes.ReplaceAll(facts[0].Registration, []byte(strings.Repeat("a", 64)), []byte(strings.Repeat("0", 64)))
			case "ambiguous":
				facts[0].Interfaces = append(facts[0].Interfaces, diagnosticMPIInterfaceObservation{Name: "other", Up: true, Addresses: []string{"192.0.2.44/24"}})
			case "source":
				selection.SSHSourceIPv4 = "198.51.100.9"
			case "window":
				selection.DedicatedTestWindow = false
			}
			_, raw, key, err := compileDiagnosticMPIPlan(record, facts, selection, now)
			if err == nil || len(raw) != 0 || len(key) != 0 {
				clear(key)
				t.Fatal("invalid trusted input produced a usable plan or private key")
			}
		})
	}
}

func TestDiagnosticMPIPlanRejectsMissingLibrariesEvenWhenBothSnapshotsMatch(t *testing.T) {
	record, facts, selection, now := mpiPlanFixture(t)
	var registration map[string]any
	_ = json.Unmarshal(record.Targets[0].Registration, &registration)
	delete(registration["dependencies"].(map[string]any), "libmpi.so.40")
	raw, _ := json.Marshal(registration)
	record.Targets[0].Registration = raw
	facts[0].Registration = raw
	_, _, key, err := compileDiagnosticMPIPlan(record, facts, selection, now)
	defer clear(key)
	if err == nil {
		t.Fatal("missing adopted MPI library was ignored")
	}
}

func TestDiagnosticMPIPlanMatchesPinnedPythonAdapter(t *testing.T) {
	testDiagnosticMPIPlanMatchesPinnedPythonAdapter(t, 2)
}

func TestDiagnosticMPIPlanTripleMatchesPinnedPythonAdapter(t *testing.T) {
	testDiagnosticMPIPlanMatchesPinnedPythonAdapter(t, 3)
}

func testDiagnosticMPIPlanMatchesPinnedPythonAdapter(t *testing.T, count int) {
	t.Helper()
	source, _, err := canonicalDiagnosticMPIPythonSources(diagnosticMPIAdapterPython, diagnosticMPINativePython)
	if err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python")
	if err != nil {
		t.Fatal("Python is required for this pure adapter schema check")
	}
	record, facts, selection, now := mpiPlanCountFixture(t, count)
	p, plan, key, err := compileDiagnosticMPIPlan(record, facts, selection, now)
	defer clear(key)
	if err != nil {
		t.Fatal(err)
	}
	recipe := diagnosticMPIQuickRecipe
	if count == 3 {
		recipe = diagnosticMPITripleRecipe
	}
	args, _, err := diagnosticMPIRecipeArgs(recipe)
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]any{"source": source, "profile": p, "plan": json.RawMessage(plan), "now": now.UnixMilli(), "preset": args, "ranks": count})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	code := "import sys,json,hashlib\nx=json.load(sys.stdin)\ns={'__name__':'mpi_socket_adapter'}\nexec(x['source'],s)\np=s['validate_plan'](x['plan'],x['now'])\nspec=s['compile_spec'](p)\nassert not spec['nativeExecutable']\nassert x['profile']['knownHosts']['sha256']==hashlib.sha256(s['known_hosts'](p)).hexdigest()\nassert x['profile']['identityFile']==spec['coordinator']['paths']['publicIdentity']\nassert x['profile']['bootstrap']['agentSocket']==spec['coordinator']['paths']['agentSocket']\nassert spec['rankArgv']==x['preset']\nassert p['limits']['ranks']==x['ranks']==len(p['members'])==len(spec['coordinator']['mpiApp'].splitlines())\nassert spec['outputContract']['rows']==14\nif x['ranks']==3:\n assert list(spec['peers'])==[m['nodeId'] for m in p['members'][1:]]\n for ordinal,m in enumerate(p['members'][1:],1):\n  command=f\"/usr/bin/orted -mca ess env -mca ess_base_jobid 123 -mca ess_base_vpid {ordinal} -mca ess_base_num_procs 3 -mca orte_hnp_uri '123.0;tcp://{p['members'][0]['collectiveAddress']}:4555'\"\n  argv=s['ssh_launch_argv'](p,m['collectiveAddress'],command)\n  assert argv[-2]==m['user']+'@'+m['sshAddress']\nprint('adapter schema and public profile projection pass')\n"
	cmd := exec.CommandContext(ctx, python, "-I", "-c", code)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "SYSTEMROOT=" + os.Getenv("SYSTEMROOT"), "PYTHONDONTWRITEBYTECODE=1"}
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pure adapter validation failed: %v; %s", err, output)
	}
}
