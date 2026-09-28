// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
)

// Embedded immutable product bytes; no request can supply worker source.
//
//go:embed vllm_rank_owner.py
var vllmRankSystemWorkerSource string

type vllmRankSystemIdentity struct {
	PID        int    `json:"pid"`
	StartTicks string `json:"startTicks"`
	UID        int    `json:"uid"`
}
type vllmRankSystemResources struct {
	Memory *float64 `json:"gpu_memory_utilization"`
	Length *int32   `json:"max_model_len"`
	GPU    *string  `json:"gpu_uuid,omitempty"`
	TP     *int32   `json:"tensor_parallel_size,omitempty"`
}
type vllmRankSystemTopology struct {
	TP                 int   `json:"tensorParallel"`
	PP                 int   `json:"pipelineParallel"`
	DP                 int   `json:"dataParallel"`
	EP                 int   `json:"expertParallel,omitempty"`
	EPLB               bool  `json:"eplb,omitempty"`
	RedundantExperts   int   `json:"redundantExperts,omitempty"`
	ContextLength      int   `json:"contextLength,omitempty"`
	MaxSequences       int   `json:"maxSequences,omitempty"`
	KVCacheMemoryBytes int64 `json:"kvCacheMemoryBytes,omitempty"`
	MTP                *bool `json:"mtp,omitempty"`
	DFlash             *bool `json:"dflash,omitempty"`
	FlashInferAutotune *bool `json:"flashinferAutotune,omitempty"`
}
type vllmRankSystemLimits struct {
	RuntimeSeconds int   `json:"runtimeSeconds"`
	MemoryMaxBytes int64 `json:"memoryMaxBytes"`
	TasksMax       int   `json:"tasksMax"`
}
type vllmRankSystemPlan struct {
	Owner              string                  `json:"owner"`
	RunID              string                  `json:"runId"`
	Generation         uint64                  `json:"generation"`
	Rank               int                     `json:"rank"`
	PlanDigest         string                  `json:"planDigest"`
	NodeID             string                  `json:"nodeId"`
	Model              string                  `json:"model"`
	ModelDigest        string                  `json:"modelDigest"`
	RuntimeDigest      string                  `json:"runtimeDigest"`
	ConfigSHA256       string                  `json:"configSha256"`
	UID                int                     `json:"uid"`
	Manager            vllmRankSystemIdentity  `json:"manager"`
	RuntimeDir         string                  `json:"runtimeDir"`
	ModelPath          string                  `json:"modelPath"`
	Resources          vllmRankSystemResources `json:"resources"`
	GPUUUID            string                  `json:"gpuUuid"`
	Peers              []string                `json:"peers"`
	LocalAddress       string                  `json:"localAddress"`
	CoordinatorAddress string                  `json:"coordinatorAddress"`
	APIPort            int                     `json:"apiPort"`
	MasterPort         int                     `json:"masterPort"`
	Topology           vllmRankSystemTopology  `json:"topology"`
	Transport          *vllmGroupTransport     `json:"transport,omitempty"`
	RDMALanes          []vllmGroupRDMALane     `json:"rdmaLanes,omitempty"`
	DirectSocket       *vllmRankDirectSocket   `json:"directSocket,omitempty"`
	RingSocket         *vllmRankRingSocket     `json:"ringSocket,omitempty"`
	Devices            []string                `json:"devices"`
	Limits             vllmRankSystemLimits    `json:"limits"`
}

// The local end of the reviewed direct socket lane, plus its peer address.
type vllmRankDirectSocket struct {
	Mode                string `json:"mode"`
	OperationID         string `json:"operationId"`
	QualificationSHA256 string `json:"qualificationSha256"`
	InterfaceName       string `json:"interfaceName"`
	InterfaceIndex      int    `json:"interfaceIndex"`
	MAC                 string `json:"mac"`
	LocalAddress        string `json:"localAddress"`
	PeerAddress         string `json:"peerAddress"`
	PeerNodeID          string `json:"peerNodeId"`
}

// The interface carrying this member's advertised ring address, plus every
// ring address the rank's NCCL Socket peers may use.
type vllmRankRingSocket struct {
	Mode                string   `json:"mode"`
	OperationID         string   `json:"operationId"`
	QualificationSHA256 string   `json:"qualificationSha256"`
	InterfaceName       string   `json:"interfaceName"`
	InterfaceIndex      int      `json:"interfaceIndex"`
	MAC                 string   `json:"mac"`
	AdvertisedAddress   string   `json:"advertisedAddress"`
	RingAddresses       []string `json:"ringAddresses"`
}

// The caller places this fixed request behind the existing reviewed admin form
// for start/status/reconcile. It does not invoke sudo, create a privileged route,
// or relax approval. Normal Stop runs as the same verified UID using pidfd.
func fixedVLLMSystemRankCommand(action string, plan vllmRankSystemPlan, supervisor *vllmRankSystemIdentity) ([]string, []byte, error) {
	switch action {
	case "start", "status", "stop", "reconcile", "normal-status", "normal-stop":
	default:
		return nil, nil, errors.New("unknown fixed rank action")
	}
	normal := action == "normal-stop" || action == "normal-status"
	if normal != (supervisor != nil) {
		return nil, nil, errors.New("only normal status or Stop carries the exact returned supervisor identity")
	}
	request := struct {
		Action     string                  `json:"action"`
		Plan       vllmRankSystemPlan      `json:"plan"`
		Supervisor *vllmRankSystemIdentity `json:"supervisor,omitempty"`
	}{action, plan, supervisor}
	data, err := json.Marshal(request)
	if err != nil || len(data) > 65536 {
		return nil, nil, errors.New("rank request exceeds fixed envelope")
	}
	script := "import base64,json,sys\nsrc=base64.b64decode('" + base64.StdEncoding.EncodeToString([]byte(vllmRankSystemWorkerSource)) + "')\nns={'__name__':'pair_fixed_rank_worker'}\nexec(compile(src,'<pair-fixed-rank-worker>','exec'),ns)\nraw=sys.stdin.buffer.read(65537)\nif len(raw)>65536: raise SystemExit('bounded request required')\nr=json.loads(raw)\nif set(r)-{'action','plan','supervisor'}: raise SystemExit('fixed request required')\ntry:\n result=ns['normal_stop'](r['plan'],r['supervisor']) if r['action']=='normal-stop' else ns['normal_status'](r['plan'],r['supervisor']) if r['action']=='normal-status' else ns['control'](r['action'],r['plan'],src)\nexcept Exception as exc:\n detail=ns['diagnostic_failure_detail'](exc)\n if detail: print('PAIR_RANK_READBACK_MISSING:'+detail,file=sys.stderr)\n print('PAIR_RANK_FAILURE:'+ns['diagnostic_failure_code'](exc),file=sys.stderr)\n raise SystemExit(70)\nprint(json.dumps(result,separators=(',',':')))\n"
	return []string{"/usr/bin/python3", "-I", "-S", "-c", script}, data, nil
}
