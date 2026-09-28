// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func vllmTestBool(value bool) *bool { return &value }
func vllmTestInt(value int) *int    { return &value }

func TestQwenTwoNodeBindsTheEffectiveMergedNICDefault(t *testing.T) {
	facts := make([]vllmGroupFacts, 2)
	selection := vllmGroupSelection{NodeIDs: []string{"node-a", "node-b"}}
	if err := bindQwen38FabricFacts(selection, facts, []string{"principal-a", "principal-b"}, strings.Repeat("a", 32), strings.Repeat("b", 64), directFabricTestEndpoints()); err != nil {
		t.Fatal(err)
	}
	transport := facts[0].fabricTransport
	if transport == nil || transport.MergeNICs == nil || !*transport.MergeNICs || transport.SubnetAwareRouting == nil || *transport.SubnetAwareRouting || transport.SubnetPrefixLength == nil || *transport.SubnetPrefixLength != 0 {
		t.Fatalf("two-node transport does not bind NCCL's effective merge default: %+v", transport)
	}
	plan := vllmGroupPlan{Transport: transport}
	if err := validateVLLMGroupTransport(plan); err != nil {
		t.Fatalf("the reviewed two-node transport was refused: %v", err)
	}
	for name, change := range map[string]func(*vllmGroupTransport){
		"subnet routing": func(v *vllmGroupTransport) { v.SubnetAwareRouting = vllmTestBool(true) },
		"subnet prefix":  func(v *vllmGroupTransport) { v.SubnetPrefixLength = vllmTestInt(31) },
		"unmerged HCAs":  func(v *vllmGroupTransport) { v.MergeNICs = vllmTestBool(false) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := *transport
			change(&changed)
			plan.Transport = &changed
			if err := validateVLLMGroupTransport(plan); err == nil {
				t.Fatal("an unreviewed NCCL routing policy was accepted")
			}
		})
	}
}

func TestQwenRankOwnerExportsTheMergedNICRoute(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil && runtime.GOOS == "windows" {
		python, err = exec.LookPath("python")
	}
	if err != nil {
		t.Skip("python3 is unavailable")
	}
	driver := `import json,sys
ns={"__name__":"pair_fixed_rank_worker"}
exec(compile(sys.stdin.read(),"<owner>","exec"),ns)
base={"NCCL_NET":"Socket","NCCL_IB_DISABLE":"1"}
bad=""
try: ns["bind_qwen_roce"](base,["rocep1s0f0:1","rocep1s0f1:1"],{3,4})
except ns["Unavailable"] as exc: bad=str(exc)
print(json.dumps({"env":ns["bind_qwen_roce"](base,["rocep1s0f0:1","rocep1s0f1:1"],{3}),"bad":bad}))
`
	cmd := exec.Command(python, "-I", "-c", driver)
	cmd.Stdin = strings.NewReader(vllmRankSystemWorkerSource)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("owner driver failed: %v\n%s", err, out)
	}
	var got struct {
		Env map[string]string
		Bad string
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("owner driver output: %v\n%s", err, out)
	}
	if got.Env["NCCL_NET"] != "IB" || got.Env["NCCL_IB_HCA"] != "=rocep1s0f0:1,rocep1s0f1:1" || got.Env["NCCL_IB_MERGE_NICS"] != "1" || got.Env["NCCL_CROSS_NIC"] != "1" {
		t.Fatalf("RoCE route is not exact: %+v", got.Env)
	}
	if _, ok := got.Env["NCCL_IB_SUBNET_AWARE_ROUTING"]; ok || got.Env["NCCL_IB_SUBNET_PREFIX_LEN"] != "" || got.Bad != "qualified_roce_gid_changed" {
		t.Fatalf("subnet routing or a mismatched GID policy reached the rank: %+v", got)
	}
}

func TestQwenStartRequiresMergedNICPolicyReadback(t *testing.T) {
	value := func(n int) *int { return &n }
	plan := vllmRankSystemPlan{Model: vllmQwen38ModelID, Peers: []string{"10.0.0.1", "10.0.0.2"}, RDMALanes: []vllmGroupRDMALane{{RDMADevice: "rocep1s0f0", GIDPort: 1}, {RDMADevice: "rocep1s0f1", GIDPort: 1}}}
	policy := vllmSystemRankPolicy{Transport: vllmQwen38Transport, HCAs: []string{"rocep1s0f0:1", "rocep1s0f1:1"}, NetGDRLevel: value(0), NetGDRC2C: value(0), NetGDRRead: value(0), NetPlugin: "none", EnvPlugin: "none", GINPlugin: "none", MergeNICs: value(1)}
	policy.EffectivePrograms.Ingress, policy.EffectivePrograms.Egress = []uint32{1}, []uint32{2}
	policy.IngressAllowed, policy.EgressDenied = true, true
	result := vllmSystemRankResult{State: "started", EffectsApplied: true, Supervisor: &vllmRankSystemIdentity{PID: 2}, Policy: &policy}
	if !systemRankPolicyPassed(plan, result) {
		t.Fatal("the explicit two-node merged-NIC policy was refused")
	}
	for name, change := range map[string]func(*vllmSystemRankPolicy){
		"subnet routing": func(p *vllmSystemRankPolicy) { p.SubnetAwareRouting = value(1) },
		"subnet prefix":  func(p *vllmSystemRankPolicy) { p.SubnetPrefixLength = value(31) },
		"unmerged HCAs":  func(p *vllmSystemRankPolicy) { p.MergeNICs = value(0) },
		"merge omitted":  func(p *vllmSystemRankPolicy) { p.MergeNICs = nil },
	} {
		t.Run(name, func(t *testing.T) {
			changed := policy
			change(&changed)
			result.Policy = &changed
			if systemRankPolicyPassed(plan, result) {
				t.Fatal("a rank policy outside the reviewed RoCE route passed Start")
			}
		})
	}
}
