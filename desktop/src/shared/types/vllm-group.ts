// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { VllmGroupPlan } from '@/shared/types/vllm-group-status'

// Closed wire values from the Engine Manager's rank start-failure boundary.
export const VLLM_GROUP_START_FAILURE_STAGES = [
    'admission',
    'helper',
    'native-return',
    'peer-return',
    'peer-transport',
    'journal'
] as const
export const VLLM_GROUP_START_FAILURE_CODES = [
    'absent_unit_has_members',
    'absent_unit_requires_reviewed_root_fence',
    'admitted_local_interface_missing',
    'admitted_private_ipv4_required',
    'approved_system_manager_worker_required',
    'binding_mismatch',
    'bounded_json_limit',
    'bounded_port_required',
    'bpf_egress_deny_not_enforced',
    'bpf_ingress_policy_not_enforced',
    'bpf_query_architecture_unqualified',
    'cancelled',
    'cgroup_owner_mismatch',
    'content_binding_invalid',
    'credential_binding_invalid',
    'deadline',
    'effective_address_families_changed',
    'effective_cgroup_bpf_egress_unconfirmed',
    'effective_cgroup_bpf_ingress_unconfirmed',
    'effective_device_policy_changed',
    'effective_ip_policy_changed',
    'effective_runtime_bound_unconfirmed',
    'effective_unit_owner_or_policy_changed',
    'fixed_collective_port_required',
    'fixed_device_policy_required',
    'fixed_embedded_worker_bytes_required',
    'fixed_owner_plan_required',
    'fixed_rank_probe_invalid',
    'foreign_uid_in_rank_cgroup',
    'http_rejected',
    'invalid_json',
    'invalid_response',
    'invocation_invalid',
    'managed_runtime_receipt_changed',
    'manager_ended_before_launch',
    'manager_ended_before_model_launch',
    'manager_identity_invalid',
    'manager_owner_changed',
    'manager_uid_mismatch',
    'membership_changed',
    'memory_fraction_invalid',
    'model_config_changed',
    'model_content_changed',
    'model_file_manifest_changed',
    'model_length_invalid',
    'model_manifest_changed',
    'model_node_gpu_binding_invalid',
    'model_path_not_supported_by_fixed_owner',
    'model_runtime_file_not_bounded_regular',
    'native_enforcement_release_missing',
    'noncanonical_path',
    'normal_stop_cgroup_exit_unconfirmed',
    'normal_stop_supervisor_binding_changed',
    'normal_uid_pidfd_stop_required',
    'normal_uid_required',
    'operation_binding_invalid',
    'operation_busy_fence_unconfirmed',
    'operation_lock_owner_unconfirmed',
    'operation_reservation_binding_changed',
    'operation_reservation_write_unconfirmed',
    'owner_directory_not_root_controlled',
    'owner_not_prepared',
    'placement_rank_mismatch',
    'policy_probe_not_owned_unit_main',
    'policy_record_write_failed',
    'policy_unconfirmed',
    'prepare_owner_busy',
    'preunit_absence_unconfirmed_tombstone_retained',
    'preunit_fence_or_absence_unconfirmed',
    'probe_binding_invalid',
    'probe_nonce_changed',
    'process_failed',
    'qualified_direct_socket_lane_changed',
    'qualified_direct_socket_required',
    'qualified_roce_gid_changed',
    'qualified_roce_device_changed',
    'qualified_roce_lane_changed',
    'qualified_roce_lane_required',
    'qualified_roce_transport_required',
    'rank_cgroup_cleanup_unconfirmed',
    'rank_datapath_policy_unconfirmed',
    'rank_device_access_unavailable',
    'rank_not_normal_uid',
    'rank_unit_already_exists',
    'redirected_model_runtime_path',
    'release_send_failed',
    'request_failed',
    'retained_rank_owner_changed',
    'reviewed_gpu_missing',
    'runtime_binding_changed',
    'runtime_content_changed',
    'saved_local_gpu_binding_changed',
    'saved_resource_settings_changed',
    'start_withdrawn',
    'supervisor_identity_invalid',
    'supervisor_identity_missing',
    'supervisor_identity_unavailable',
    'system_manager_action_unconfirmed',
    'system_manager_readback_incomplete',
    'two_or_three_exact_peers_required',
    'unclassified',
    'unified_cgroup_v2_required',
    'unit_missing_but_cgroup_occupied',
    'unit_normal_owner_unconfirmed',
    'unit_state_unknown',
    'unknown_fixed_rank_action',
    'unknown_resource_setting',
    'unsupported_fixed_topology',
    'write_failed'
] as const
export const VLLM_GROUP_START_STDERR_CODES = [
    'none',
    'unclassified',
    'worker_rejected',
    'sudo_authentication_required',
    'sudo_not_permitted'
] as const
export const VLLM_GROUP_READBACK_PROPERTIES = [
    'Id',
    'LoadState',
    'ActiveState',
    'SubState',
    'MainPID',
    'ControlGroup',
    'Transient',
    'User',
    'Group',
    'KillMode',
    'SendSIGKILL',
    'Restart',
    'Delegate',
    'ProtectControlGroups',
    'NoNewPrivileges',
    'PrivateDevices',
    'DevicePolicy',
    'DeviceAllow',
    'RestrictAddressFamilies',
    'IPAccounting',
    'IPAddressAllow',
    'IPAddressDeny',
    'Description',
    'RuntimeMaxUSec',
    'MemoryMax',
    'TasksMax'
] as const

export interface VllmGroupStartFailure {
    stage: (typeof VLLM_GROUP_START_FAILURE_STAGES)[number]
    code: (typeof VLLM_GROUP_START_FAILURE_CODES)[number]
    exit: number
    stdoutBytes: number
    stderrCode: (typeof VLLM_GROUP_START_STDERR_CODES)[number]
    missingProperties?: (typeof VLLM_GROUP_READBACK_PROPERTIES)[number][]
}

/**
 * A typed product request built only from current cluster node identities and
 * one exact downloaded model. It never carries commands, paths, credentials or
 * any caller-asserted authority; PAIR builds the plan.
 */
export interface VllmGroupSelection {
    nodeIds: string[]
    model: string
    /** Omitted preserves the deployed two-node TP2 / three-node PP3 defaults. */
    parallelism?: 'tensor' | 'pipeline'
}

/** Exact retained operation identity for stop and reconcile. */
export interface VllmGroupOperation {
    runId: string
    generation: number
}

export interface VllmGroupElevation {
    nodeId: string
    elevationPassword?: string
    nonInteractive?: true
}

export interface VllmGroupReviewRequest {
    reviewId: string
}

/** Start consumes exactly one unexpired review plus one-use administrator access. */
export interface VllmGroupStartRequest extends VllmGroupReviewRequest {
    elevation?: VllmGroupElevation[]
}

export interface VllmGroupReconcileRequest extends VllmGroupOperation {
    elevation?: VllmGroupElevation[]
}

export interface VllmGroupReview {
    reviewId: string
    planDigest: string
    plan: VllmGroupPlan
    expiresAt: number
    /** True only when PAIR reports an admitted native owner; review is never execution. */
    activationEnabled: boolean
    reason: string
}

export interface VllmGroupCheckParticipant {
    nodeId: string
    state: string
    activationEnabled: boolean
    reason: string
}

export interface VllmGroupCheck {
    reviewId: string
    activationEnabled: boolean
    participants: VllmGroupCheckParticipant[]
}
