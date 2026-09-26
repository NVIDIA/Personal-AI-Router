// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"strings"
)

// Only closed enumerations and bounded numbers may leave this boundary.
// Neither Error() nor the retained record contains raw process/HTTP output.
type vllmRankStartFailure struct {
	Stage             string   `json:"stage"`
	Code              string   `json:"code"`
	Exit              int      `json:"exit"`
	StdoutBytes       int      `json:"stdoutBytes"`
	StderrCode        string   `json:"stderrCode"`
	MissingProperties []string `json:"missingProperties,omitempty"`
}
type vllmRankStartError struct{ Failure vllmRankStartFailure }

func (e *vllmRankStartError) Error() string {
	if !validVLLMRankStartFailure(&e.Failure) {
		return "vLLM rank operation failed: unclassified"
	}
	return "vLLM rank operation failed: " + e.Failure.Stage + "/" + e.Failure.Code
}

// Preserve lifecycle cancellation across the redacted wire form without
// retaining or unwrapping the subprocess error and its potentially private text.
func (e *vllmRankStartError) Is(target error) bool {
	return e != nil && validVLLMRankStartFailure(&e.Failure) &&
		(target == context.Canceled && e.Failure.Code == "cancelled" ||
			target == context.DeadlineExceeded && e.Failure.Code == "deadline")
}

var vllmRankFailureStages = map[string]bool{"admission": true, "helper": true, "native-return": true, "peer-return": true, "peer-transport": true, "journal": true}
var vllmRankFailureCodes = map[string]bool{"unclassified": true, "invocation_invalid": true, "process_failed": true, "deadline": true, "cancelled": true, "invalid_json": true, "binding_mismatch": true, "policy_unconfirmed": true, "owner_not_prepared": true, "start_withdrawn": true, "write_failed": true, "request_failed": true, "invalid_response": true, "membership_changed": true, "http_rejected": true}
var vllmRankStderrCodes = map[string]bool{"none": true, "unclassified": true, "worker_rejected": true, "sudo_authentication_required": true, "sudo_not_permitted": true}
var vllmRankReadbackProperties = map[string]bool{"Id": true, "LoadState": true, "ActiveState": true, "SubState": true, "MainPID": true, "ControlGroup": true, "Transient": true, "User": true, "Group": true, "KillMode": true, "SendSIGKILL": true, "Restart": true, "Delegate": true, "ProtectControlGroups": true, "NoNewPrivileges": true, "PrivateDevices": true, "DevicePolicy": true, "DeviceAllow": true, "RestrictAddressFamilies": true, "IPAccounting": true, "IPAddressAllow": true, "IPAddressDeny": true, "Description": true, "RuntimeMaxUSec": true, "MemoryMax": true, "TasksMax": true, "StandardOutput": true, "StandardError": true}

func validVLLMRankStartFailure(f *vllmRankStartFailure) bool {
	if f == nil || !vllmRankFailureStages[f.Stage] || !vllmRankFailureCodes[f.Code] || !vllmRankStderrCodes[f.StderrCode] || f.Exit < -1 || f.Exit > 255 || f.StdoutBytes < 0 || f.StdoutBytes > 1<<20 {
		return false
	}
	if f.Code != "system_manager_readback_incomplete" {
		return len(f.MissingProperties) == 0
	}
	previous := ""
	for _, property := range f.MissingProperties {
		if !vllmRankReadbackProperties[property] || property <= previous {
			return false
		}
		previous = property
	}
	return len(f.MissingProperties) <= len(vllmRankReadbackProperties)
}
func cloneVLLMRankStartFailure(f vllmRankStartFailure) vllmRankStartFailure {
	f.MissingProperties = append([]string(nil), f.MissingProperties...)
	return f
}
func rankStartError(stage, code string, err error, stdoutBytes int) *vllmRankStartError {
	var prior *vllmRankStartError
	if errors.As(err, &prior) && validVLLMRankStartFailure(&prior.Failure) {
		return &vllmRankStartError{cloneVLLMRankStartFailure(prior.Failure)}
	}
	f := vllmRankStartFailure{Stage: stage, Code: code, Exit: -1, StdoutBytes: min(max(stdoutBytes, 0), 1<<20), StderrCode: "none"}
	if !vllmRankFailureStages[f.Stage] {
		f.Stage = "peer-return"
	}
	if !vllmRankFailureCodes[f.Code] {
		f.Code = "unclassified"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		f.Code = "deadline"
	} else if errors.Is(err, context.Canceled) {
		f.Code = "cancelled"
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		f.Code = "deadline"
	}
	if stage == "native-return" && err == nil && (code == "invalid_json" || code == "binding_mismatch" || code == "policy_unconfirmed") {
		f.Exit = 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		f.Exit = exit.ExitCode()
	}
	return &vllmRankStartError{f}
}
func classifyVLLMRankProcess(err error, stdoutBytes int, stderr []byte) error {
	failure := rankStartError("helper", "process_failed", err, stdoutBytes)
	text := strings.TrimSpace(string(stderr))
	if text != "" {
		failure.Failure.StderrCode = "unclassified"
	}
	lines := strings.Split(text, "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if strings.HasPrefix(last, "PAIR_RANK_FAILURE:") {
		code := strings.TrimPrefix(last, "PAIR_RANK_FAILURE:")
		if vllmRankFailureCodes[code] {
			failure.Failure.Code = code
			failure.Failure.StderrCode = "worker_rejected"
			const detailPrefix = "PAIR_RANK_READBACK_MISSING:"
			for _, line := range lines[:len(lines)-1] {
				if !strings.HasPrefix(line, detailPrefix) || len(failure.Failure.MissingProperties) != 0 {
					continue
				}
				failure.Failure.MissingProperties = strings.Split(strings.TrimPrefix(line, detailPrefix), ",")
				if !validVLLMRankStartFailure(&failure.Failure) {
					failure.Failure.MissingProperties = nil
				}
			}
		}
	} else if strings.Contains(text, "sudo: a password is required") || strings.Contains(text, "sudo: no password was provided") {
		failure.Failure.StderrCode = "sudo_authentication_required"
	} else if strings.Contains(text, "is not in the sudoers file") || strings.Contains(text, "is not allowed to execute") {
		failure.Failure.StderrCode = "sudo_not_permitted"
	}
	return failure
}
func failedVLLMGroupStart(r vllmGroupPeerRequest, err error) vllmGroupPeerResult {
	failure := rankStartError("peer-return", "unclassified", err, 0)
	result := vllmGroupPeerRefusal(r)
	result.ActivationEnabled = true
	result.State = "failed"
	result.Code = "start_failed"
	result.Reason = ""
	result.EffectsApplied = false
	result.StartFailure = &failure.Failure
	return result
}

// Preparation failures use the same bounded diagnostic schema, but retain the
// requested action and never affirm preparation, native effects or cleanup.
// The coordinator still marks the rank Attempted and reconciles it: an app
// journal write may have been attempted before failure, even without a unit.
func failedVLLMGroupPrepare(r vllmGroupPeerRequest, err error) vllmGroupPeerResult {
	failure := rankStartError("admission", "unclassified", err, 0)
	result := vllmGroupPeerRefusal(r)
	result.ActivationEnabled = true
	result.State, result.Code, result.Reason = "failed", "prepare_failed", ""
	result.EffectsApplied, result.CleanupConfirmed = false, false
	result.StartFailure = &failure.Failure
	return result
}

func init() {
	vllmRankFailureCodes["prepare_owner_busy"] = true
	vllmRankFailureCodes["qualified_direct_socket_required"] = true
	vllmRankFailureCodes["qualified_direct_socket_lane_changed"] = true
}

func init() {
	for _, code := range []string{"absent_unit_has_members", "absent_unit_requires_reviewed_root_fence", "admitted_local_interface_missing", "admitted_private_ipv4_required", "approved_system_manager_worker_required", "bounded_json_limit", "bounded_port_required", "bpf_egress_deny_not_enforced", "bpf_ingress_policy_not_enforced", "bpf_query_architecture_unqualified", "cgroup_owner_mismatch", "content_binding_invalid", "credential_binding_invalid", "effective_address_families_changed", "effective_cgroup_bpf_egress_unconfirmed", "effective_cgroup_bpf_ingress_unconfirmed", "effective_device_policy_changed", "effective_ip_policy_changed", "effective_runtime_bound_unconfirmed", "effective_unit_owner_or_policy_changed", "fixed_collective_port_required", "fixed_device_policy_required", "fixed_embedded_worker_bytes_required", "fixed_owner_plan_required", "fixed_rank_probe_invalid", "foreign_uid_in_rank_cgroup", "managed_runtime_receipt_changed", "manager_ended_before_launch", "manager_ended_before_model_launch", "manager_identity_invalid", "manager_owner_changed", "manager_uid_mismatch", "memory_fraction_invalid", "model_config_changed", "model_content_changed", "model_file_manifest_changed", "model_length_invalid", "model_manifest_changed", "model_node_gpu_binding_invalid", "model_path_not_supported_by_fixed_owner", "model_runtime_file_not_bounded_regular", "native_enforcement_release_missing", "noncanonical_path", "normal_stop_cgroup_exit_unconfirmed", "normal_stop_supervisor_binding_changed", "normal_uid_pidfd_stop_required", "normal_uid_required", "operation_binding_invalid", "operation_busy_fence_unconfirmed", "operation_lock_owner_unconfirmed", "operation_reservation_binding_changed", "operation_reservation_write_unconfirmed", "owner_directory_not_root_controlled", "placement_rank_mismatch", "policy_probe_not_owned_unit_main", "policy_record_write_failed", "preunit_absence_unconfirmed_tombstone_retained", "preunit_fence_or_absence_unconfirmed", "probe_binding_invalid", "probe_nonce_changed", "qualified_roce_device_changed", "qualified_roce_gid_changed", "qualified_roce_lane_changed", "qualified_roce_lane_required", "qualified_roce_transport_required", "rank_cgroup_cleanup_unconfirmed", "rank_datapath_policy_unconfirmed", "rank_device_access_unavailable", "rank_not_normal_uid", "rank_unit_already_exists", "redirected_model_runtime_path", "release_send_failed", "retained_rank_owner_changed", "reviewed_gpu_missing", "runtime_binding_changed", "runtime_content_changed", "saved_local_gpu_binding_changed", "saved_resource_settings_changed", "supervisor_identity_invalid", "supervisor_identity_missing", "supervisor_identity_unavailable", "system_manager_action_unconfirmed", "system_manager_readback_incomplete", "two_or_three_exact_peers_required", "unclassified", "unified_cgroup_v2_required", "unit_missing_but_cgroup_occupied", "unit_normal_owner_unconfirmed", "unit_state_unknown", "unknown_fixed_rank_action", "unknown_resource_setting", "unsupported_fixed_topology"} {
		vllmRankFailureCodes[code] = true
	}
}

func init() {
	for _, code := range []string{"policy_record_write_failed", "release_send_failed", "supervisor_identity_missing", "supervisor_identity_unavailable", "supervisor_identity_invalid"} {
		vllmRankFailureCodes[code] = true
	}
}
