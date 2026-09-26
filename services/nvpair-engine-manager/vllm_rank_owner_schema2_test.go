// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestManagedVLLMServingSurfacesDisableFlashInferJITPaths(t *testing.T) {
	for _, variable := range []string{"VLLM_USE_FLASHINFER_SAMPLER", "VLLM_ALLREDUCE_USE_FLASHINFER"} {
		if !strings.Contains(vllmRankSystemWorkerSource, `"`+variable+`": "0"`) {
			t.Fatalf("native rank environment does not disable %s", variable)
		}
	}
	env := managedVLLMRuntimeEnv(&engineState{installDir: t.TempDir(), modelDir: t.TempDir()}, t.TempDir())
	for _, variable := range []string{"VLLM_USE_FLASHINFER_SAMPLER=0", "VLLM_ALLREDUCE_USE_FLASHINFER=0"} {
		if !slices.Contains(env, variable) {
			t.Fatalf("managed rank environment does not contain %s", variable)
		}
	}
}

func TestVLLMRankOwnerVerifiesSchema2RuntimeLayout(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fixed native rank owner is a Linux runtime contract")
	}
	pythonExe := "/usr/bin/python3"
	if _, err := os.Stat(pythonExe); err != nil {
		t.Skipf("fixed native Python unavailable: %v", err)
	}
	st := managedVLLMTestState(t)
	id := "v0-rank-owner"
	runtimeDir := writeManagedVLLMTestEnvironment(t, st, id, managedVLLMVersion)
	pythonLink := filepath.Join(runtimeDir, "venv", "bin", "python")
	managedPython := filepath.Join(runtimeDir, "python", "cpython")
	pythonBytes, err := os.ReadFile(pythonLink)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(managedPython), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(managedPython, pythonBytes, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(pythonLink); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join("..", "..", "python", "cpython"), pythonLink); err != nil {
		t.Fatal(err)
	}
	python, cli, receipt, err := validateVLLMRuntimeBinaries(st, id)
	if err != nil {
		t.Fatal(err)
	}

	modelDir := t.TempDir()
	config := []byte(`{"architectures":["FixtureForOwner"]}`)
	weights := []byte("fixture-weights")
	if err = os.WriteFile(filepath.Join(modelDir, "config.json"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(modelDir, "model.safetensors"), weights, 0o600); err != nil {
		t.Fatal(err)
	}
	type modelFile struct {
		Path   string `json:"path"`
		Size   int    `json:"size"`
		SHA256 string `json:"sha256"`
	}
	hash := func(data []byte) string {
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:])
	}
	files := []modelFile{{"config.json", len(config), hash(config)}, {"model.safetensors", len(weights), hash(weights)}}
	filesJSON, _ := json.Marshal(files)
	modelDigest := hash(filesJSON)
	modelID := "fixture/schema2@" + strings.Repeat("a", 40)
	modelReceipt, _ := json.Marshal(map[string]any{"id": modelID, "digest": modelDigest, "files": files})
	if err = os.WriteFile(filepath.Join(modelDir, "pair-model.json"), modelReceipt, 0o600); err != nil {
		t.Fatal(err)
	}
	plan := map[string]any{
		"runtimeDir":    runtimeDir,
		"modelPath":     modelDir,
		"model":         modelID,
		"nodeId":        "node-a",
		"runtimeDigest": vllmRankRuntimeDigest(receipt),
		"modelDigest":   modelDigest,
		"configSha256":  hash(config),
		"resources":     map[string]any{"gpu_memory_utilization": nil, "max_model_len": nil},
	}
	planJSON, _ := json.Marshal(plan)
	wrapper := "import base64,json,sys\nns={'__name__':'pair_schema2_test'}\nsrc=base64.b64decode('" + base64.StdEncoding.EncodeToString([]byte(vllmRankSystemWorkerSource)) + "')\nexec(compile(src,'<pair-fixed-rank-worker>','exec'),ns)\ntry:\n print(json.dumps([str(value) for value in ns['verify_content'](json.load(sys.stdin))],separators=(',',':')))\nexcept Exception as exc:\n print(ns['diagnostic_failure_code'](exc),file=sys.stderr)\n raise\n"
	run := func() ([]byte, error) {
		cmd := exec.Command(pythonExe, "-I", "-S", "-c", wrapper)
		cmd.Stdin = bytes.NewReader(planJSON)
		return cmd.CombinedOutput()
	}
	out, err := run()
	want, _ := json.Marshal([]string{python, cli, filepath.Join(runtimeDir, "venv", "bin"), receipt.Version})
	if err != nil || strings.TrimSpace(string(out)) != string(want) {
		t.Fatalf("schema2 owner paths=%s want=%s err=%v", out, want, err)
	}
	escapedPython := filepath.Join(t.TempDir(), "python")
	if err = os.WriteFile(escapedPython, pythonBytes, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(pythonLink); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(escapedPython, pythonLink); err != nil {
		t.Fatal(err)
	}
	out, err = run()
	if err == nil || !bytes.Contains(out, []byte("redirected_model_runtime_path")) {
		t.Fatalf("escaping schema2 interpreter was accepted: %s err=%v", out, err)
	}
	if err = os.Remove(pythonLink); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join("..", "..", "python", "cpython"), pythonLink); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(runtimeDir, "pip-report.json")
	reportBytes, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(reportPath, []byte(`{"version":"changed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = run()
	if err == nil || !bytes.Contains(out, []byte("runtime_content_changed")) {
		t.Fatalf("changed schema2 dependency report was accepted: %s err=%v", out, err)
	}
	if err = os.WriteFile(reportPath, reportBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cli, []byte("changed-cli"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err = run()
	if err == nil || !bytes.Contains(out, []byte("runtime_content_changed")) {
		t.Fatalf("changed schema2 CLI was accepted: %s err=%v", out, err)
	}
}

func TestVLLMRankOwnerAdmitsPreparedSchema2QwenAndRejectsDrift(t *testing.T) {
	linuxPath := func(value string) string { return value }
	pythonCommand := func(scriptPath string) *exec.Cmd { return exec.Command("/usr/bin/python3", "-I", "-S", scriptPath) }
	if runtime.GOOS == "windows" {
		if _, err := exec.LookPath("wsl.exe"); err != nil {
			t.Skipf("Linux contract runner unavailable: %v", err)
		}
		linuxPath = func(value string) string {
			volume := filepath.VolumeName(value)
			if len(volume) != 2 || volume[1] != ':' {
				t.Fatalf("translate fixture path: unsupported volume %q", volume)
			}
			relative := strings.TrimPrefix(value, volume+string(filepath.Separator))
			return "/mnt/" + strings.ToLower(volume[:1]) + "/" + strings.ReplaceAll(relative, string(filepath.Separator), "/")
		}
		pythonCommand = func(scriptPath string) *exec.Cmd {
			return exec.Command("wsl.exe", "-d", "Ubuntu", "--", "python3", "-I", "-S", linuxPath(scriptPath))
		}
	} else if runtime.GOOS != "linux" {
		t.Skip("fixed native rank owner is a Linux runtime contract")
	}
	pythonBytes := []byte("schema2-qwen-python-fixture")
	var err error
	st, runtimeDir, receipt, _ := preparedQwen38FactsFixture(t)
	binDir := filepath.Join(runtimeDir, "venv", "bin")
	if err = os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pythonPath, cliPath := filepath.Join(binDir, "python"), filepath.Join(binDir, "vllm")
	if err = os.WriteFile(pythonPath, pythonBytes, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cliPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(runtimeDir, "pip-report.json")
	if err = os.WriteFile(reportPath, []byte(`{"schema":1,"packages":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	hashFile := func(path string) string {
		digest, hashErr := managedVLLMFileHash(runtimeDir, path)
		if hashErr != nil {
			t.Fatal(hashErr)
		}
		return digest
	}
	receipt.PythonSHA256 = hashFile(pythonPath)
	receipt.CLISHA256 = hashFile(cliPath)
	receipt.PipReportSHA256 = hashFile(reportPath)
	receipt.Environment = linuxPath(runtimeDir)
	if err = writeManagedVLLMJSON(st.installDir, filepath.Join(runtimeDir, vllmEnvironmentReceiptFile), receipt); err != nil {
		t.Fatal(err)
	}

	modelDir := t.TempDir()
	config := []byte(`{"architectures":["Qwen4ExpForConditionalGeneration"]}`)
	weights := []byte("qwen-schema2-owner-fixture")
	hash := func(data []byte) string {
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:])
	}
	for name, data := range map[string][]byte{"config.json": config, "model.safetensors": weights} {
		if err = os.WriteFile(filepath.Join(modelDir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files := []vllmModelFile{{Path: "config.json", Size: int64(len(config)), SHA256: hash(config)}, {Path: "model.safetensors", Size: int64(len(weights)), SHA256: hash(weights)}}
	filesJSON, _ := json.Marshal(files)
	modelDigest := hash(filesJSON)
	modelReceipt := vllmModel{ID: vllmQwen38ModelID, Digest: modelDigest, Files: files}
	if err = writeManagedVLLMJSON(modelDir, filepath.Join(modelDir, "pair-model.json"), modelReceipt); err != nil {
		t.Fatal(err)
	}

	off := false
	plan := vllmRankSystemPlan{
		Owner: vllmSystemRankOwner, RunID: strings.Repeat("a", 32), Generation: 1, Rank: 0,
		PlanDigest: strings.Repeat("b", 64), NodeID: "node-a", Model: vllmQwen38ModelID,
		ModelDigest: modelDigest, RuntimeDigest: vllmRankRuntimeDigest(receipt), ConfigSHA256: hash(config),
		UID: 1000, Manager: vllmRankSystemIdentity{PID: 2, StartTicks: "1", UID: 1000}, RuntimeDir: linuxPath(runtimeDir), ModelPath: linuxPath(modelDir),
		GPUUUID: "GPU-11111111-1111-1111-1111-111111111111", Peers: []string{"10.0.0.1", "10.0.0.2"},
		LocalAddress: "10.0.0.1", CoordinatorAddress: "10.0.0.1", APIPort: 8000, MasterPort: 29500,
		Topology:  vllmRankSystemTopology{TP: 2, PP: 1, DP: 1, EP: 2, ContextLength: 32768, MaxSequences: 2, KVCacheMemoryBytes: 8 << 30, MTP: &off, DFlash: &off, FlashInferAutotune: &off},
		Transport: &vllmGroupTransport{Mode: vllmQwen38Transport, OperationID: strings.Repeat("c", 32), QualificationSHA256: strings.Repeat("d", 64), NetPlugin: "none", EnvPlugin: "none", GINPlugin: "none", SubnetAwareRouting: vllmTestBool(false), SubnetPrefixLength: vllmTestInt(0), MergeNICs: vllmTestBool(true)},
		RDMALanes: []vllmGroupRDMALane{
			{PeerNodeID: "node-b", LocalAddress: "10.253.0.1", PeerAddress: "10.253.0.2", InterfaceName: "enp1s0f0np0", InterfaceIndex: 1, MAC: "02:00:00:00:00:01", SwitchID: "switch-a", PortName: "p0", RDMADevice: "mlx5_0", GIDPort: 1, GIDIndex: 3, GIDType: "RoCE v2"},
			{PeerNodeID: "node-b", LocalAddress: "10.253.0.5", PeerAddress: "10.253.0.6", InterfaceName: "enP2p1s0f0np0", InterfaceIndex: 2, MAC: "02:00:00:00:00:02", SwitchID: "switch-a", PortName: "p0", RDMADevice: "mlx5_1", GIDPort: 1, GIDIndex: 3, GIDType: "RoCE v2"},
		},
		Devices: []string{"/dev/nvidiactl", "/dev/nvidia-modeset", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools", "/dev/nvidia0", "/dev/infiniband/uverbs0", "/dev/infiniband/uverbs1"},
		Limits:  vllmRankSystemLimits{RuntimeSeconds: 3600, MemoryMaxBytes: 112 << 30, TasksMax: 512},
	}
	wrapper := "import base64,json,sys\nns={'__name__':'pair_schema2_qwen_test'}\nsrc=base64.b64decode('" + base64.StdEncoding.EncodeToString([]byte(vllmRankSystemWorkerSource)) + "')\nexec(compile(src,'<pair-fixed-rank-worker>','exec'),ns)\nplan=json.load(sys.stdin)\ntry:\n ns['check_plan'](plan)\n ns['current_root_record']=lambda value:(None,None)\n ns['identity']=lambda value:plan['manager']\n def owner_reached(value): raise ns['Unavailable']('owner_reached_after_content')\n ns['show_unit']=owner_reached\n ns['start_rank'](plan,b'fixed-worker')\nexcept Exception as exc:\n if str(exc)=='owner_reached_after_content': print('owner-reached')\n else:\n  print(ns['diagnostic_failure_code'](exc),file=sys.stderr)\n  raise\n"
	wrapperPath := filepath.Join(st.installDir, "qwen-schema2-owner-test.py")
	if err = os.WriteFile(wrapperPath, []byte(wrapper), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(candidate vllmRankSystemPlan) ([]byte, error) {
		raw, marshalErr := json.Marshal(candidate)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		cmd := pythonCommand(wrapperPath)
		cmd.Stdin = bytes.NewReader(raw)
		return cmd.CombinedOutput()
	}
	if out, runErr := run(plan); runErr != nil || strings.TrimSpace(string(out)) != "owner-reached" {
		t.Fatalf("prepared schema-2 Qwen did not reach the fixed owner: %s err=%v", out, runErr)
	}

	changedLayout := plan
	changedLayout.Topology.TP = 3
	if out, runErr := run(changedLayout); runErr == nil || !bytes.Contains(out, []byte("unsupported_fixed_topology")) {
		t.Fatalf("changed Qwen layout was accepted: %s err=%v", out, runErr)
	}
	threeSparks := plan
	threeSparks.Peers = []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	if out, runErr := run(threeSparks); runErr == nil || !bytes.Contains(out, []byte("unsupported_fixed_topology")) {
		t.Fatalf("a three-Spark Qwen rank was accepted: %s err=%v", out, runErr)
	}
	receiptPath := filepath.Join(runtimeDir, vllmEnvironmentReceiptFile)
	receipt.Schema = vllmLegacyReceiptSchema
	if err = writeManagedVLLMJSON(st.installDir, receiptPath, receipt); err != nil {
		t.Fatal(err)
	}
	if out, runErr := run(plan); runErr == nil || !bytes.Contains(out, []byte("managed_runtime_receipt_changed")) {
		t.Fatalf("changed Qwen schema was accepted: %s err=%v", out, runErr)
	}
	receipt.Schema = vllmEnvironmentReceiptSchema
	if err = writeManagedVLLMJSON(st.installDir, receiptPath, receipt); err != nil {
		t.Fatal(err)
	}
	launchPath := filepath.Join(runtimeDir, vllmQwen38LaunchReceiptFile)
	if err = os.WriteFile(launchPath, []byte(`{"schema":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, runErr := run(plan); runErr == nil || !bytes.Contains(out, []byte("managed_runtime_receipt_changed")) {
		t.Fatalf("changed prepared Qwen launch receipt was accepted: %s err=%v", out, runErr)
	}
}
