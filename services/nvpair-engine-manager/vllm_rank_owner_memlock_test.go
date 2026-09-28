// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// A Qwen3.8 rank's NCCL IB transport pins host buffers, so its unit must lock
// memory up to its own ceiling; ordinary socket ranks keep systemd's default.
func TestQwenRankUnitLocksMemoryUpToItsCeiling(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil && runtime.GOOS == "windows" {
		python, err = exec.LookPath("python")
	}
	if err != nil {
		t.Skip("python3 is unavailable")
	}
	driver := `import json, sys
ns = {"__name__": "pair_fixed_rank_worker"}
exec(compile(sys.stdin.read(), "<owner>", "exec"), ns)
ns["pwd"] = type("P", (), {"getpwuid": staticmethod(lambda uid: type("E", (), {"pw_gid": uid})())})
def lane(peer, local, remote, interface, index, device):
    return {"peerNodeId": peer, "localAddress": local, "peerAddress": remote, "interfaceName": interface, "interfaceIndex": index,
        "mac": "02:00:00:00:00:01", "switchId": "switch-a", "portName": "p0", "rdmaDevice": device, "gidPort": 1, "gidIndex": 3, "gidType": "RoCE v2"}
qwen = {"owner": ns["OWNER"], "runId": "a"*32, "generation": 1, "rank": 0, "planDigest": "b"*64, "nodeId": "node-a", "model": ns["QWEN_MODEL"],
    "uid": 1000, "peers": ["10.0.0.1", "10.0.0.2", "10.0.0.3"], "localAddress": "10.0.0.1",
    "rdmaLanes": [lane("node-b", "10.10.1.1", "10.10.1.2", "enp1s0f0np0", 1, "rocep1s0f0"),
                  lane("node-c", "10.10.2.1", "10.10.2.2", "enp1s0f1np1", 2, "rocep1s0f1")],
    "devices": ["/dev/nvidia0", "/dev/infiniband/uverbs0", "/dev/infiniband/uverbs1"], "limits": ns["QWEN_LIMITS"]}
ordinary = dict(qwen, model="example/model@" + "a"*40, rdmaLanes=[], devices=["/dev/nvidia0"], limits=ns["LIMITS"])
def record(plan):
    properties = ns["unit_properties"](plan, "/run/credential.json")
    value = dict(item.split("=", 1) for item in properties if not item.startswith("DeviceAllow="))
    value.update({"Id": ns["unit_name"](plan), "LoadState": "loaded", "ActiveState": "active", "Transient": "yes",
        "ControlGroup": "/system.slice/" + ns["unit_name"](plan), "IPAddressDeny": "0.0.0.0/0 ::/0",
        "DeviceAllow": " ".join(device + " rw" for device in plan["devices"]),
        "RuntimeMaxUSec": {600: "10min", 3600: "1h"}[plan["limits"]["runtimeSeconds"]]})
    if "LimitMEMLOCK" in value:
        value["LimitMEMLOCKSoft"] = value["LimitMEMLOCK"]
    return properties, value
def verdict(plan, change):
    _, value = record(plan)
    change(value)
    try:
        ns["validate_unit"](plan, value)
        return "ok"
    except ns["Unavailable"] as exc:
        return str(exc)
print(json.dumps({
    "qwenLimit": ns["QWEN_LIMITS"]["memoryMaxBytes"],
    "qwenProperties": record(qwen)[0],
    "ordinaryProperties": record(ordinary)[0],
    "qwen": verdict(qwen, lambda value: None),
    "qwenDefaultHard": verdict(qwen, lambda value: value.update(LimitMEMLOCK="8388608")),
    "qwenDefaultSoft": verdict(qwen, lambda value: value.update(LimitMEMLOCKSoft="8388608")),
    "ordinary": verdict(ordinary, lambda value: value.update(LimitMEMLOCK="8388608", LimitMEMLOCKSoft="8388608"))}))
`
	cmd := exec.Command(python, "-I", "-c", driver)
	cmd.Stdin = strings.NewReader(vllmRankSystemWorkerSource)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("owner driver failed: %v\n%s", err, out)
	}
	var got struct {
		QwenLimit                                         int64
		QwenProperties, OrdinaryProperties                []string
		Qwen, QwenDefaultHard, QwenDefaultSoft, Ordinary string
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("owner driver output: %v\n%s", err, out)
	}
	if !slices.Contains(got.QwenProperties, fmt.Sprintf("LimitMEMLOCK=%d", got.QwenLimit)) {
		t.Fatalf("a Qwen3.8 unit keeps systemd's locked-memory default: %v", got.QwenProperties)
	}
	if slices.ContainsFunc(got.OrdinaryProperties, func(property string) bool { return strings.HasPrefix(property, "LimitMEMLOCK=") }) {
		t.Fatalf("an ordinary unit gained locked memory: %v", got.OrdinaryProperties)
	}
	if got.Qwen != "ok" || got.QwenDefaultHard != "effective_unit_owner_or_policy_changed" ||
		got.QwenDefaultSoft != "effective_unit_owner_or_policy_changed" || got.Ordinary != "ok" {
		t.Fatalf("effective locked-memory verification changed: %+v", got)
	}
}
