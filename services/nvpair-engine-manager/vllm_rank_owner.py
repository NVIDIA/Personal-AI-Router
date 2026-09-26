# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Fixed, reviewed PAIR rank lifetime owner. No generic privileged command API.

Root entrypoint is embedded/invoked only by the existing approved admin worker.
The system unit invokes the same root-owned source as the verified normal UID.
Native support is fail-closed: cgroup-v2 BPF readback AND datapath checks precede
model execution. Ordinary groups retain TCP; an ordinary two-node TP2 plan may
bind only its NCCL Socket to one reviewed direct fabric lane while the master
address, VLLM_HOST_IP and Gloo stay on management. The exact Qwen profile
separately requires reviewed host-buffer RoCE bindings and never falls back to
Socket payloads.
"""
from contextlib import contextmanager
import ctypes
import errno
import hashlib
import ipaddress
import json
import os
from pathlib import Path, PurePosixPath
import platform
try:
    import pwd
except ImportError:
    pwd = None
import signal
import re
import select
import socket
import stat
import subprocess
import sys
import time

OWNER = "pair-vllm-rank-system-v1"
ROOT = Path("/run/nvpair-vllm-ranks")
SYSTEMD_RUN = "/usr/bin/systemd-run"
SYSTEMCTL = "/usr/bin/systemctl"
PYTHON = "/usr/bin/python3"
LIMITS = {"runtimeSeconds": 600, "memoryMaxBytes": 16 * 1024**3, "tasksMax": 512}
QWEN_LIMITS = {"runtimeSeconds": 3600, "memoryMaxBytes": 112 * 1024**3, "tasksMax": 512}
HEX = re.compile(r"^[a-f0-9]{64}$")
RUN = re.compile(r"^[a-f0-9]{32}$")
GPU = re.compile(r"^GPU-[a-fA-F0-9-]{36}$")
QWEN_MODEL = "nvidia/Qwen3.8-Flash-Next-NVFP4@fc694b54fb0174e0913e6adf86691ef85a4ead47"
QWEN_RUNTIME = "0.28.1rc1.dev361+gd4d703caf"
QWEN_NCCL = "2.30.7"
# DGX Spark GPU memory is shared with the OS and page cache; vLLM's default 0.92
# startup check fails whenever a few GiB are cached, so the fixed recipe keeps headroom.
QWEN_GPU_MEMORY_UTILIZATION = "0.8"
QWEN_RECIPE = "qwen38-flash-next-nvfp4-d4d703c-spark-a-v4"
QWEN_RECIPE_SHA256 = "5870b327fa252f3ddf27730738449e0a7615256cb9bce5ee29ae816a3b37752e"
QWEN_RUNTIME_COMMIT = "d4d703caf908786416585ceb1f369e2e0363358b"
QWEN_WHEEL_URL = "https://wheels.vllm.ai/d4d703caf908786416585ceb1f369e2e0363358b/vllm-0.28.1rc1.dev361%2Bgd4d703caf-cp38-abi3-manylinux_2_28_aarch64.whl"
QWEN_WHEEL_SHA256 = "f45d026fe1a9c532e89eeb71f888aabd1523a19a4052ff67e3810f02c9fdee67"
QWEN_ARTIFACT_CLOSURE_SHA256 = "47d74f2078e4f3fb393e4f4048bded2a0c0b39e76bb5b6eaeceea8b8a003b8d5"
QWEN_REFERENCE_PROVIDER_CLOSURE = "8396cb056ada51dee9f92eeb11469c4573d967b2861b5c3c1ef5f7b4c1ec3c61"
QWEN_PROVIDER_CLOSURES = {
    QWEN_REFERENCE_PROVIDER_CLOSURE,
    "0265d190e9bd5c90ac4492a1c811056dc4b0ed6814bd797c5d65cac075f5d3c2",
    "eb64849ee23f29a140f33630fb2d4b95e59e6acff8069a5426e27f461ca85329",
}
QWEN_PROVIDER_PROFILES = {
    "spark-a-libc8.6-r580.95.05": QWEN_REFERENCE_PROVIDER_CLOSURE,
    "spark-bc-libc8.8-r580.173.02": "0265d190e9bd5c90ac4492a1c811056dc4b0ed6814bd797c5d65cac075f5d3c2",
    "spark-abc-dgxos7.6-libc8.9-r580.178.04": "eb64849ee23f29a140f33630fb2d4b95e59e6acff8069a5426e27f461ca85329",
}
QWEN_ARTIFACT_COUNT = 196
QWEN_ARTIFACT_BYTES = 3940635938
VLLM_RUNTIME = "0.29.0"
VLLM_RECIPE = "vllm-0.29.0-py312-cu130-uv0.12.17-v1"
DIRECT_SOCKET = "qualified-direct-socket"
DISTRIBUTED_TIMEOUT_SECONDS = 180
UNIT_PROPERTIES = ("Id", "LoadState", "ActiveState", "SubState", "MainPID", "ControlGroup", "Transient", "User", "Group", "KillMode", "SendSIGKILL", "Restart", "Delegate", "ProtectControlGroups", "NoNewPrivileges", "PrivateDevices", "DevicePolicy", "DeviceAllow", "RestrictAddressFamilies", "IPAccounting", "IPAddressAllow", "IPAddressDeny", "Description", "RuntimeMaxUSec", "MemoryMax", "TasksMax", "StandardOutput", "StandardError", "LimitMEMLOCK", "LimitMEMLOCKSoft")

class Unavailable(Exception):
    pass

class SystemManagerReadbackIncomplete(Unavailable):
    def __init__(self, missing=()):
        self.missing = tuple(sorted(missing))
        super().__init__("system_manager_readback_incomplete")

def canonical(value):
    return json.dumps(value, separators=(",", ":"), ensure_ascii=True).encode()

def sha(data):
    return hashlib.sha256(data).hexdigest()

def bounded_json(path, maximum=65536):
    with open(path, "rb") as stream:
        raw = stream.read(maximum + 1)
    if len(raw) > maximum:
        raise Unavailable("bounded_json_limit")
    return json.loads(raw)

def fixed_path(value):
    p = PurePosixPath(value)
    if not p.is_absolute() or ".." in p.parts or str(p) != value or "\x00" in value:
        raise Unavailable("noncanonical_path")
    return Path(value)

def unit_name(plan):
    return f"nvpair-vllm-rank-{plan['runId']}-{plan['generation']}-{plan['rank']}.service"

def identity(proc_id):
    root = Path("/proc") / str(proc_id)
    fields = (root / "stat").read_text().rsplit(")", 1)[1].split()
    uid_line = next(line for line in (root / "status").read_text().splitlines() if line.startswith("Uid:"))
    uids = [int(item) for item in uid_line.split()[1:]]
    if len(set(uids)) != 1:
        raise Unavailable("manager_uid_mismatch")
    return {"pid": proc_id, "startTicks": fields[19], "uid": uids[0]}

def check_plan(plan):
    keys = {"owner", "runId", "generation", "rank", "planDigest", "nodeId", "model", "modelDigest", "runtimeDigest", "configSha256", "uid", "manager", "runtimeDir", "modelPath", "resources", "gpuUuid", "peers", "localAddress", "coordinatorAddress", "apiPort", "masterPort", "topology", "devices", "limits"}
    expected_limits = QWEN_LIMITS if plan.get("model") == QWEN_MODEL else LIMITS
    if not isinstance(plan, dict) or plan.get("owner") != OWNER or plan.get("limits") != expected_limits:
        raise Unavailable("fixed_owner_plan_required")
    qwen = plan.get("model") == QWEN_MODEL
    direct = isinstance(plan, dict) and "directSocket" in plan
    if set(plan) != (keys | ({"transport", "rdmaLanes"} if qwen else set()) | ({"directSocket"} if direct else set())):
        raise Unavailable("fixed_owner_plan_required")
    if not RUN.fullmatch(str(plan["runId"])) or type(plan["generation"]) is not int or not 1 <= plan["generation"] < 2**63:
        raise Unavailable("operation_binding_invalid")
    for key in ("planDigest", "modelDigest", "runtimeDigest", "configSha256"):
        if not HEX.fullmatch(str(plan[key])):
            raise Unavailable("content_binding_invalid")
    if type(plan["uid"]) is not int or plan["uid"] <= 0 or set(plan["manager"]) != {"pid", "startTicks", "uid"} or plan["manager"]["uid"] != plan["uid"]:
        raise Unavailable("normal_uid_required")
    if type(plan["manager"]["pid"]) is not int or plan["manager"]["pid"] <= 1 or not re.fullmatch(r"[0-9]+", str(plan["manager"]["startTicks"])):
        raise Unavailable("manager_identity_invalid")
    peers = plan["peers"]
    if not isinstance(peers, list) or len(peers) not in (2, 3) or len(set(peers)) != len(peers):
        raise Unavailable("two_or_three_exact_peers_required")
    for value in peers:
        address = ipaddress.ip_address(value)
        if address.version != 4 or not address.is_private or address.is_loopback or address.is_unspecified or str(address) != value:
            raise Unavailable("admitted_private_ipv4_required")
    if type(plan["rank"]) is not int or not 0 <= plan["rank"] < len(peers) or plan["localAddress"] != peers[plan["rank"]] or plan["coordinatorAddress"] != peers[0]:
        raise Unavailable("placement_rank_mismatch")
    for key in ("apiPort", "masterPort"):
        if type(plan[key]) is not int or not 1024 <= plan[key] <= 65535:
            raise Unavailable("bounded_port_required")
    if plan["apiPort"] == plan["masterPort"] or plan["masterPort"] != 29500:
        raise Unavailable("fixed_collective_port_required")
    for key in ("runtimeDir", "modelPath"):
        fixed_path(plan[key])
    if not GPU.fullmatch(str(plan["gpuUuid"])) or not re.fullmatch(r"[A-Za-z0-9._:/@-]{1,512}", str(plan["model"])) or not re.fullmatch(r"[A-Za-z0-9._-]{1,128}", str(plan["nodeId"])):
        raise Unavailable("model_node_gpu_binding_invalid")
    resources = plan["resources"]
    if set(resources) - {"gpu_memory_utilization", "max_model_len", "gpu_uuid", "tensor_parallel_size"}:
        raise Unavailable("unknown_resource_setting")
    memory = resources.get("gpu_memory_utilization")
    length = resources.get("max_model_len")
    if memory is not None and (type(memory) not in (int, float) or not 0 < memory <= 1):
        raise Unavailable("memory_fraction_invalid")
    if length is not None and (type(length) is not int or not 1 <= length < 2**31):
        raise Unavailable("model_length_invalid")
    if resources.get("gpu_uuid") not in (None, plan["gpuUuid"]) or resources.get("tensor_parallel_size") not in (None, 1):
        raise Unavailable("saved_local_gpu_binding_changed")
    devices = plan["devices"]
    if not isinstance(devices, list) or len(devices) != (7 if qwen else 5) or len(set(devices)) != len(devices):
        raise Unavailable("fixed_device_policy_required")
    for device in devices:
        if not isinstance(device, str) or not device.startswith("/dev/"):
            raise Unavailable("fixed_device_policy_required")
        fixed_path(device)
    required = {"/dev/nvidiactl", "/dev/nvidia-modeset", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools"}
    gpu_nodes = [device for device in devices if re.fullmatch(r"/dev/nvidia[0-9]{1,3}", device)]
    uverbs = [device for device in devices if re.fullmatch(r"/dev/infiniband/uverbs[0-9]{1,3}", device)]
    if not required.issubset(devices) or len(gpu_nodes) != 1 or len(uverbs) != (2 if qwen else 0):
        raise Unavailable("fixed_device_policy_required")
    topology = plan["topology"]
    ordinary = ({"tensorParallel": len(peers), "pipelineParallel": 1, "dataParallel": 1}, {"tensorParallel": 1, "pipelineParallel": len(peers), "dataParallel": 1})
    qwen = {"tensorParallel": 2, "pipelineParallel": 1, "dataParallel": 1, "expertParallel": 2, "contextLength": 32768, "maxSequences": 2, "kvCacheMemoryBytes": 8 * 1024**3, "mtp": False, "dflash": False, "flashinferAutotune": False}
    if plan["model"] == QWEN_MODEL:
        if len(peers) != 2 or topology != qwen: raise Unavailable("unsupported_fixed_topology")
        transport = plan["transport"]
        if set(transport) != {"mode", "operationId", "qualificationSha256", "netGdrLevel", "netGdrC2c", "netGdrRead", "netPlugin", "envPlugin", "ginPlugin", "subnetAwareRouting", "subnetPrefixLength", "mergeNICs", "socketPayloadFallback"} or transport["mode"] != "host-buffer-roce" or not RUN.fullmatch(str(transport["operationId"])) or not HEX.fullmatch(str(transport["qualificationSha256"])) or transport["netGdrLevel"] != 0 or transport["netGdrC2c"] != 0 or transport["netGdrRead"] != 0 or transport["netPlugin"] != "none" or transport["envPlugin"] != "none" or transport["ginPlugin"] != "none" or transport["subnetAwareRouting"] is not False or transport["subnetPrefixLength"] != 0 or transport["mergeNICs"] is not True or transport["socketPayloadFallback"] is not False:
            raise Unavailable("qualified_roce_transport_required")
        lanes = plan["rdmaLanes"]
        if not isinstance(lanes, list) or len(lanes) != 2:
            raise Unavailable("qualified_roce_transport_required")
        indexes, devices, gid_indexes = set(), set(), set()
        for lane in lanes:
            lane_keys = {"peerNodeId", "localAddress", "peerAddress", "interfaceName", "interfaceIndex", "mac", "switchId", "portName", "rdmaDevice", "gidPort", "gidIndex", "gidType"}
            if not isinstance(lane, dict) or set(lane) != lane_keys or lane["peerNodeId"] == plan["nodeId"] or lane["gidType"] != "RoCE v2":
                raise Unavailable("qualified_roce_lane_required")
            for key in ("localAddress", "peerAddress"):
                address = ipaddress.ip_address(lane[key])
                if address.version != 4 or not address.is_private or address.is_loopback or address.is_unspecified or str(address) != lane[key]: raise Unavailable("qualified_roce_lane_required")
            if ipaddress.ip_network(lane["localAddress"] + "/30", strict=False) != ipaddress.ip_network(lane["peerAddress"] + "/30", strict=False):
                raise Unavailable("qualified_roce_lane_required")
            if not re.fullmatch(r"[A-Za-z0-9_.:-]{1,64}", str(lane["interfaceName"])) or type(lane["interfaceIndex"]) is not int or lane["interfaceIndex"] <= 0 or not re.fullmatch(r"(?:mlx5_[0-9]{1,3}|roce(?:P2)?p[0-9]{1,3}s[0-9]{1,3}f[0-9]{1,3})", str(lane["rdmaDevice"])) or type(lane["gidPort"]) is not int or not 1 <= lane["gidPort"] <= 255 or type(lane["gidIndex"]) is not int or not 0 <= lane["gidIndex"] <= 255:
                raise Unavailable("qualified_roce_lane_required")
            indexes.add(lane["interfaceIndex"]); devices.add(lane["rdmaDevice"]); gid_indexes.add(lane["gidIndex"])
        if len(indexes) != 2 or len(devices) != 2 or len(gid_indexes) != 1:
            raise Unavailable("qualified_roce_lane_required")
    elif topology not in ordinary:
        raise Unavailable("unsupported_fixed_topology")
    if direct:
        lane = plan["directSocket"]
        lane_keys = {"mode", "operationId", "qualificationSha256", "interfaceName", "interfaceIndex", "mac", "localAddress", "peerAddress", "peerNodeId"}
        if plan["model"] == QWEN_MODEL or len(peers) != 2 or topology != ordinary[0] or not isinstance(lane, dict) or set(lane) != lane_keys or lane["mode"] != DIRECT_SOCKET or not RUN.fullmatch(str(lane["operationId"])) or not HEX.fullmatch(str(lane["qualificationSha256"])):
            raise Unavailable("qualified_direct_socket_required")
        if not re.fullmatch(r"[A-Za-z0-9_.:-]{1,15}", str(lane["interfaceName"])) or type(lane["interfaceIndex"]) is not int or lane["interfaceIndex"] <= 0 or not re.fullmatch(r"[0-9a-f]{2}(:[0-9a-f]{2}){5}", str(lane["mac"])) or not re.fullmatch(r"[A-Za-z0-9._-]{1,128}", str(lane["peerNodeId"])) or lane["peerNodeId"] == plan["nodeId"]:
            raise Unavailable("qualified_direct_socket_required")
        for key in ("localAddress", "peerAddress"):
            address = ipaddress.ip_address(lane[key])
            if address.version != 4 or not address.is_private or address.is_loopback or address.is_unspecified or str(address) != lane[key] or lane[key] in peers:
                raise Unavailable("qualified_direct_socket_required")
        if lane["localAddress"] == lane["peerAddress"]:
            raise Unavailable("qualified_direct_socket_required")
    return plan

def model_arguments(plan, cli, runtime_version):
    args = [str(cli), "serve", plan["modelPath"], "--served-model-name", plan["model"], "--distributed-executor-backend", "mp", "--data-parallel-backend", "mp", "--nnodes", str(len(plan["peers"])), "--node-rank", str(plan["rank"]), "--master-addr", plan["coordinatorAddress"], "--master-port", str(plan["masterPort"])]
    # Fixed bounded group recipe: avoid graph capture; trade steady-state throughput for startup.
    args.append("--enforce-eager")
    for flag, key in (("--tensor-parallel-size", "tensorParallel"), ("--pipeline-parallel-size", "pipelineParallel"), ("--data-parallel-size", "dataParallel")):
        args += [flag, str(plan["topology"][key])]
    qwen = plan["model"] == QWEN_MODEL
    if not qwen and runtime_version == VLLM_RUNTIME:
        args += ["--distributed-timeout-seconds", str(DISTRIBUTED_TIMEOUT_SECONDS), "--cpu-distributed-timeout-seconds", str(DISTRIBUTED_TIMEOUT_SECONDS)]
    if qwen:
        args += ["--enable-expert-parallel", "--max-model-len", str(plan["topology"]["contextLength"]), "--max-num-seqs", str(plan["topology"]["maxSequences"]), "--kv-cache-memory-bytes", str(plan["topology"]["kvCacheMemoryBytes"]), "--gpu-memory-utilization", QWEN_GPU_MEMORY_UTILIZATION, "--no-enable-flashinfer-autotune"]
        if plan["topology"].get("eplb"):
            args += ["--enable-eplb", "--eplb-config", canonical({"window_size": 1000, "step_interval": 3000, "num_redundant_experts": plan["topology"]["redundantExperts"], "log_balancedness": False, "use_async": False, "policy": "default", "communicator": "torch_nccl"}).decode()]
    args += ["--host", "127.0.0.1", "--port", str(plan["apiPort"])] if plan["rank"] == 0 else ["--headless"]
    for flag, key in (("--gpu-memory-utilization", "gpu_memory_utilization"), ("--max-model-len", "max_model_len")):
        if qwen and key in ("gpu_memory_utilization", "max_model_len"): continue
        if plan["resources"].get(key) is not None:
            args += [flag, str(plan["resources"][key])]
    return args

def transport_name(plan):
    if plan["model"] == QWEN_MODEL: return "host-buffer-roce"
    return DIRECT_SOCKET if "directSocket" in plan else "tcp-only"

def allowed_addresses(plan):
    allowed = {"127.0.0.1", *plan["peers"]}
    if plan["model"] == QWEN_MODEL:
        for lane in plan["rdmaLanes"]: allowed.update((lane["localAddress"], lane["peerAddress"]))
    if "directSocket" in plan:
        allowed.update((plan["directSocket"]["localAddress"], plan["directSocket"]["peerAddress"]))
    return allowed

def unit_properties(plan, credential_path):
    families = "AF_UNIX AF_INET AF_NETLINK" + (" AF_IB" if plan["model"] == QWEN_MODEL else "")
    limits = plan["limits"]
    allowed = allowed_addresses(plan)
    properties = ["Type=exec", "ExitType=main", "KillMode=control-group", "SendSIGKILL=yes", "TimeoutStopSec=8s", "RuntimeMaxSec=" + str(limits["runtimeSeconds"]) + "s", "Restart=no", "OOMPolicy=stop", "MemoryMax=" + str(limits["memoryMaxBytes"]), "TasksMax=" + str(limits["tasksMax"]), "Delegate=no", "ProtectControlGroups=yes", "RuntimeDirectory=" + runtime_name(plan), "RuntimeDirectoryMode=0700", "RuntimeDirectoryPreserve=no", "User=" + str(plan["uid"]), "Group=" + str(pwd.getpwuid(plan["uid"]).pw_gid), "NoNewPrivileges=yes", "CapabilityBoundingSet=", "AmbientCapabilities=", "PrivateDevices=no", "DevicePolicy=closed", "RestrictAddressFamilies=" + families, "IPAddressDeny=any", "IPAddressAllow=" + " ".join(value + "/32" for value in sorted(allowed)), "IPAccounting=yes", "LoadCredential=plan:" + str(credential_path), "UMask=0077", "StandardOutput=journal", "StandardError=journal", "Description=" + OWNER + ":" + sha(canonical(plan))]
    properties += ["DeviceAllow=" + device + " rw" for device in plan["devices"]]
    if plan["model"] == QWEN_MODEL:
        # NCCL's IB transport pins its host buffers; a system unit otherwise
        # inherits systemd's 8 MiB default and every registration fails.
        properties.append("LimitMEMLOCK=" + str(limits["memoryMaxBytes"]))
    return properties

def run_system(argv, timeout=10):
    result = subprocess.run(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env={"PATH": "/usr/bin:/bin", "LANG": "C", "LC_ALL": "C"}, timeout=timeout, check=False)
    if result.returncode != 0 or len(result.stdout) > 65536:
        raise Unavailable("system_manager_action_unconfirmed")
    return result.stdout.decode("utf-8", "strict")

def atomic_json(path, value):
    temporary = path.with_name(path.name + ".next-" + os.urandom(8).hex())
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(canonical(value)); stream.flush(); os.fsync(stream.fileno())
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try: os.fsync(directory)
        finally: os.close(directory)
    finally:
        if temporary.exists(): temporary.unlink()

def root_directory(path):
    if not path.exists():
        path.mkdir(mode=0o755)
        os.chmod(path, 0o755)
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022:
        raise Unavailable("owner_directory_not_root_controlled")

def file_digest(path, maximum=512 * 1024**3):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_size > maximum:
            raise Unavailable("model_runtime_file_not_bounded_regular")
        result = hashlib.sha256()
        while block := stream.read(1024 * 1024): result.update(block)
    return result.hexdigest()

def runtime_file(runtime_dir, path):
    # uv-managed virtual environments normally expose their interpreter through
    # an in-environment symlink. Match the Go receipt verifier: follow it only
    # when the final regular file remains inside this exact owned environment.
    resolved = path.resolve(strict=True)
    try: resolved.relative_to(runtime_dir)
    except ValueError: raise Unavailable("redirected_model_runtime_path") from None
    return resolved


def verify_prepared_qwen_receipts(runtime_dir, receipt):
    recipe_path = runtime_dir / "pair-qwen38-recipe.json"
    bundle_path = runtime_dir / "pair-qwen38-bundle.json"
    launch_path = runtime_dir / "pair-qwen38-launch.json"
    for path, key in ((recipe_path, "recipeReceiptSha256"), (bundle_path, "bundleReceiptSha256")):
        if not HEX.fullmatch(str(receipt.get(key))) or file_digest(path, 1024**2) != receipt[key]:
            raise Unavailable("managed_runtime_receipt_changed")
    recipe = bounded_json(recipe_path, 1024**2)
    if (recipe.get("Schema") != 2 or recipe.get("LifecycleSchema") != 2 or
            recipe.get("RecipeID") != QWEN_RECIPE or recipe.get("RecipeSHA256") != QWEN_RECIPE_SHA256 or
            recipe.get("RuntimeVersion") != QWEN_RUNTIME or recipe.get("RuntimeCommit") != QWEN_RUNTIME_COMMIT or
            recipe.get("WheelSHA256") != QWEN_WHEEL_SHA256 or recipe.get("ArtifactClosureSHA256") != QWEN_ARTIFACT_CLOSURE_SHA256 or
            recipe.get("ProviderClosureSHA256") not in QWEN_PROVIDER_CLOSURES or
            recipe.get("ReferenceProviderClosureSHA256") != QWEN_REFERENCE_PROVIDER_CLOSURE or
            recipe.get("ArtifactCount") != QWEN_ARTIFACT_COUNT or recipe.get("ArtifactBytes") != QWEN_ARTIFACT_BYTES or
            recipe.get("TimeoutSeconds") != 1800 or recipe.get("MemoryMaxBytes") != 24 * 1024**3 or
            recipe.get("StageMaxBytes") != 24 * 1024**3 or recipe.get("FreeReserveBytes") != 40 * 1024**3 or
            recipe.get("TasksMax") != 512 or recipe.get("WholeCgroupCleanup") is not True or
            recipe.get("Offline") is not True or recipe.get("EmptyCwd") is not True or recipe.get("TelemetryDisabled") is not True or
            recipe.get("StageOwner") != "stageQwen38ManagedVLLM" or recipe.get("ActivateOwner") != "activateManagedVLLM" or
            recipe.get("RollbackOwner") != "restoreManagedVLLM" or recipe.get("CleanupOwner") != "uninstallManagedVLLMWithIO" or
            recipe.get("ActivePointer") != "active-runtime.json"):
        raise Unavailable("managed_runtime_receipt_changed")
    bundle = bounded_json(bundle_path, 1024**2)
    provider = bundle.get("provider")
    artifacts = bundle.get("artifacts")
    allowed_providers = provider.get("allowedClosureSha256") if isinstance(provider, dict) else None
    if (bundle.get("schema") != 1 or bundle.get("phase") != "runtime-prepared" or
            bundle.get("recipeId") != QWEN_RECIPE or bundle.get("recipeSha256") != QWEN_RECIPE_SHA256 or
            bundle.get("artifactClosureSha256") != QWEN_ARTIFACT_CLOSURE_SHA256 or
            bundle.get("artifactCount") != QWEN_ARTIFACT_COUNT or bundle.get("artifactBytes") != QWEN_ARTIFACT_BYTES or
            not isinstance(provider, dict) or provider.get("qualified") is not True or
            QWEN_PROVIDER_PROFILES.get(provider.get("profileId")) != provider.get("observedClosureSha256") or
            provider.get("observedClosureSha256") not in QWEN_PROVIDER_CLOSURES or
            provider.get("expectedClosureSha256") != provider.get("observedClosureSha256") or provider.get("mismatches") not in (None, []) or
            not isinstance(allowed_providers, list) or set(allowed_providers) != QWEN_PROVIDER_CLOSURES or
            not isinstance(artifacts, list) or len(artifacts) != QWEN_ARTIFACT_COUNT or
            not HEX.fullmatch(str(bundle.get("launchReceiptSha256")))):
        raise Unavailable("managed_runtime_receipt_changed")
    if recipe.get("ProviderClosureSHA256") != provider.get("observedClosureSha256"):
        raise Unavailable("managed_runtime_receipt_changed")
    seen = set()
    total = 0
    for artifact in artifacts:
        if (not isinstance(artifact, dict) or set(artifact) != {"filename", "sha256", "bytes"} or
                not isinstance(artifact.get("filename"), str) or Path(artifact["filename"]).name != artifact["filename"] or
                artifact["filename"] in seen or not HEX.fullmatch(str(artifact.get("sha256"))) or
                type(artifact.get("bytes")) is not int or artifact["bytes"] <= 0):
            raise Unavailable("managed_runtime_receipt_changed")
        seen.add(artifact["filename"])
        total += artifact["bytes"]
    if total != QWEN_ARTIFACT_BYTES or file_digest(launch_path, 1024**2) != bundle["launchReceiptSha256"]:
        raise Unavailable("managed_runtime_receipt_changed")
    launch = bounded_json(launch_path, 1024**2)
    if (launch.get("schema") != 1 or launch.get("recipeId") != QWEN_RECIPE or
            launch.get("recipeSha256") != QWEN_RECIPE_SHA256 or launch.get("runtimeVersion") != QWEN_RUNTIME or
            not HEX.fullmatch(str(launch.get("runtimeCompatibilitySha256"))) or
            launch.get("providerClosureSha256") != provider.get("observedClosureSha256") or
            launch.get("python") != "3.12.3" or launch.get("soabi") != "cpython-312-aarch64-linux-gnu" or
            launch.get("glibc") != "2.39" or launch.get("torch") != "2.13.0+cu130" or launch.get("cuda") != "13.0" or launch.get("nccl") != QWEN_NCCL or
            launch.get("transformers") != "5.17.0" or launch.get("compressedTensors") != "0.17.0" or
            launch.get("modelOptClass") != "ModelOptNvFp4Config" or launch.get("qwenClass") != "Qwen4ExpForCausalLM"):
        raise Unavailable("managed_runtime_receipt_changed")

def verify_content(plan):
    runtime_dir = fixed_path(plan["runtimeDir"])
    model_path = fixed_path(plan["modelPath"])
    for path in (runtime_dir, model_path):
        if str(path.resolve(strict=True)) != str(path): raise Unavailable("redirected_model_runtime_path")
    receipt = bounded_json(runtime_dir / "pair-runtime.json", 16384)
    qwen = plan["model"] == QWEN_MODEL
    if receipt.get("environment") != str(runtime_dir):
        raise Unavailable("managed_runtime_receipt_changed")
    schema = receipt.get("schema")
    if schema == 2:
        if qwen:
            if (receipt.get("version") != QWEN_RUNTIME or receipt.get("architecture") != "arm64" or
                    receipt.get("recipeId") != QWEN_RECIPE or receipt.get("recipeSha256") != QWEN_RECIPE_SHA256 or
                    receipt.get("nodeId") != plan["nodeId"] or
                    any(receipt.get(key) for key in ("uvUrl", "uvSha256", "wheelUrl", "wheelSha256", "pythonSource"))):
                raise Unavailable("managed_runtime_receipt_changed")
        elif receipt.get("version") != VLLM_RUNTIME or receipt.get("recipeId") != VLLM_RECIPE or not HEX.fullmatch(str(receipt.get("uvSha256"))):
            raise Unavailable("managed_runtime_receipt_changed")
        bin_dir = runtime_dir / "venv" / "bin"
        python, cli = bin_dir / "python", bin_dir / "vllm"
        python_sha, cli_sha = file_digest(runtime_file(runtime_dir, python), 1024**3), file_digest(runtime_file(runtime_dir, cli), 1024**3)
        pip_sha = file_digest(runtime_dir / "pip-report.json", 16 * 1024**2)
        if python_sha != receipt.get("pythonSha256") or cli_sha != receipt.get("cliSha256") or pip_sha != receipt.get("pipReportSha256"):
            raise Unavailable("runtime_content_changed")
        runtime_identity = [receipt["version"], receipt["architecture"], receipt["recipeId"], receipt["uvSha256"], python_sha, cli_sha, pip_sha]
    elif schema == 1:
        expected_version = QWEN_RUNTIME if qwen else "0.28.0"
        if receipt.get("version") != expected_version:
            raise Unavailable("managed_runtime_receipt_changed")
        if qwen and (receipt.get("wheelUrl") != QWEN_WHEEL_URL or receipt.get("wheelSha256") != QWEN_WHEEL_SHA256 or
                     any(receipt.get(key) for key in ("uvUrl", "uvSha256", "cliSha256"))):
            raise Unavailable("managed_runtime_receipt_changed")
        bin_dir = runtime_dir / "bin"
        python, cli = bin_dir / "python", bin_dir / "vllm"
        python_sha = file_digest(runtime_file(runtime_dir, python), 1024**3)
        pip_sha = file_digest(runtime_dir / "pip-report.json", 16 * 1024**2)
        if python_sha != receipt.get("pythonSha256") or pip_sha != receipt.get("pipReportSha256"):
            raise Unavailable("runtime_content_changed")
        runtime_identity = [receipt["version"], receipt["architecture"], receipt["wheelSha256"], python_sha, pip_sha]
    else:
        raise Unavailable("managed_runtime_receipt_changed")
    if qwen:
        if receipt.get("architecture") != "arm64" or receipt.get("recipeId") != QWEN_RECIPE or receipt.get("recipeSha256") != QWEN_RECIPE_SHA256 or receipt.get("nodeId") != plan["nodeId"]:
            raise Unavailable("managed_runtime_receipt_changed")
        if schema == 2:
            verify_prepared_qwen_receipts(runtime_dir, receipt)
        else:
            for name, key in (("pair-qwen38-recipe.json", "recipeReceiptSha256"), ("pair-qwen38-bundle.json", "bundleReceiptSha256")):
                if not HEX.fullmatch(str(receipt.get(key))) or file_digest(runtime_dir / name, 1024**2) != receipt[key]:
                    raise Unavailable("managed_runtime_receipt_changed")
            runtime_identity += [receipt["recipeId"], receipt["recipeSha256"], receipt["recipeReceiptSha256"], receipt["bundleReceiptSha256"], receipt["nodeId"]]
    elif receipt.get("schema") == 1 and any(receipt.get(key) for key in ("recipeId", "recipeSha256", "recipeReceiptSha256", "bundleReceiptSha256", "nodeId")):
        raise Unavailable("managed_runtime_receipt_changed")
    runtime_hash = sha(canonical(runtime_identity))
    if runtime_hash != plan["runtimeDigest"]: raise Unavailable("runtime_binding_changed")
    model = bounded_json(model_path / "pair-model.json", 8 * 1024**2)
    files = model.get("files", [])
    if model.get("id") != plan["model"] or model.get("digest") != plan["modelDigest"] or not 1 <= len(files) <= 100000:
        raise Unavailable("model_manifest_changed")
    if sha(canonical(files)) != plan["modelDigest"]: raise Unavailable("model_file_manifest_changed")
    for item in files:
        name = item.get("path", "")
        if not re.fullmatch(r"[A-Za-z0-9._/-]+", name) or Path(name).is_absolute() or ".." in Path(name).parts:
            raise Unavailable("model_path_not_supported_by_fixed_owner")
        path = model_path / name
        if str(path.resolve(strict=True)) != str(path) or path.stat().st_size != item.get("size") or file_digest(path) != item.get("sha256"):
            raise Unavailable("model_content_changed")
    if file_digest(model_path / "config.json", 1024**2) != plan["configSha256"]:
        raise Unavailable("model_config_changed")
    try: settings = bounded_json(runtime_dir.parent.parent / "resource-settings.json", 4096)
    except FileNotFoundError: settings = {"gpu_memory_utilization": None, "max_model_len": None}
    if settings != plan["resources"]: raise Unavailable("saved_resource_settings_changed")
    return python, cli, bin_dir, receipt["version"]

def show_unit(plan, policy=True):
    raw = run_system([SYSTEMCTL, "--system", "--no-ask-password", "--no-pager", "show", "--all", unit_name(plan)] + ["--property=" + key for key in UNIT_PROPERTIES])
    result = {}
    malformed = False
    repeatable = {"DeviceAllow", "IPAddressAllow", "IPAddressDeny"}
    for line in raw.splitlines():
        if not line.strip():
            continue
        if "=" not in line:
            malformed = True
            continue
        key, value = line.split("=", 1)
        if key not in UNIT_PROPERTIES:
            malformed = True
            continue
        if key in result:
            if key in repeatable:
                result[key] = " ".join(part for part in (result[key], value) if part)
                continue
            malformed = True
            continue
        result[key] = value
    missing = set(UNIT_PROPERTIES) - set(result)
    # systemd 255 suppresses an empty DeviceAllow for a non-existent unit even
    # when every requested property is otherwise returned. This is absence
    # formatting, not a policy echo: loaded units still require the exact field.
    if result.get("LoadState") == "not-found":
        if not malformed and missing == {"DeviceAllow"}:
            result["DeviceAllow"] = ""
        elif malformed or missing:
            raise SystemManagerReadbackIncomplete(missing)
    elif malformed or policy and missing:
        raise SystemManagerReadbackIncomplete(missing)
    elif not {"Id", "LoadState", "ActiveState", "MainPID", "ControlGroup", "Transient", "User", "Group", "Description"}.issubset(result):
        raise SystemManagerReadbackIncomplete(missing)
    return result

def normalized_networks(text):
    # systemctl's actual textual address array must parse exactly. Unknown
    # serialization is unavailable, never accepted as a configuration echo.
    networks = set()
    for word in text.split():
        if word in ("any", "0.0.0.0/0", "::/0"):
            networks.update(["0.0.0.0/0", "::/0"] if word == "any" else [word])
        else:
            networks.add(str(ipaddress.ip_network(word, strict=True)))
    return networks

def normalized_device_allow(text):
    words = text.split()
    if len(words) % 2: raise Unavailable("effective_device_policy_changed")
    return {(words[index], words[index + 1]) for index in range(0, len(words), 2)}

def validate_unit(plan, record):
    limits = plan["limits"]
    expected = {"Id": unit_name(plan), "LoadState": "loaded", "Transient": "yes", "User": str(plan["uid"]), "Group": str(pwd.getpwuid(plan["uid"]).pw_gid), "KillMode": "control-group", "SendSIGKILL": "yes", "Restart": "no", "Delegate": "no", "ProtectControlGroups": "yes", "NoNewPrivileges": "yes", "PrivateDevices": "no", "DevicePolicy": "closed", "IPAccounting": "yes", "Description": OWNER + ":" + sha(canonical(plan)), "MemoryMax": str(limits["memoryMaxBytes"]), "TasksMax": str(limits["tasksMax"]), "StandardOutput": "journal", "StandardError": "journal"}
    if plan["model"] == QWEN_MODEL:
        expected["LimitMEMLOCK"] = expected["LimitMEMLOCKSoft"] = str(limits["memoryMaxBytes"])
    if any(record.get(key) != value for key, value in expected.items()): raise Unavailable("effective_unit_owner_or_policy_changed")
    runtime_values = ("10min", "10min 0", "600000000", "600s") if limits["runtimeSeconds"] == 600 else ("1h", "1h 0", "3600000000", "3600s")
    if record.get("RuntimeMaxUSec") not in runtime_values:
        raise Unavailable("effective_runtime_bound_unconfirmed")
    # glibc interface enumeration needs NETLINK_ROUTE; no network-admin capability is granted.
    families = {"AF_UNIX", "AF_INET", "AF_NETLINK"} | ({"AF_IB"} if plan["model"] == QWEN_MODEL else set())
    if set(record.get("RestrictAddressFamilies", "").split()) != families:
        raise Unavailable("effective_address_families_changed")
    allowed = allowed_addresses(plan)
    if normalized_networks(record["IPAddressAllow"]) != {value + "/32" for value in allowed} or normalized_networks(record["IPAddressDeny"]) != {"0.0.0.0/0", "::/0"}:
        raise Unavailable("effective_ip_policy_changed")
    if normalized_device_allow(record["DeviceAllow"]) != {(device, "rw") for device in plan["devices"]}:
        raise Unavailable("effective_device_policy_changed")
    expected_group = "/system.slice/" + unit_name(plan)
    if record["ControlGroup"] != expected_group and not (record["ControlGroup"] == "" and record["ActiveState"] in ("inactive", "failed")): raise Unavailable("cgroup_owner_mismatch")
    return Path("/sys/fs/cgroup") / expected_group.lstrip("/")

def cleanup_unit_group(plan, unit):
    expected_group = "/system.slice/" + unit_name(plan)
    expected = {"Id": unit_name(plan), "LoadState": "loaded", "Transient": "yes", "User": str(plan["uid"]), "Group": str(pwd.getpwuid(plan["uid"]).pw_gid), "Description": OWNER + ":" + sha(canonical(plan))}
    if any(unit.get(key) != value for key, value in expected.items()):
        raise Unavailable("effective_unit_owner_or_policy_changed")
    if unit.get("ControlGroup") not in ("", expected_group):
        raise Unavailable("cgroup_owner_mismatch")
    return Path("/sys/fs/cgroup") / expected_group.lstrip("/")

def validate_cleanup_unit(plan, unit, retained):
    """Bind risk-reducing Stop to the exact retained unit, not its failed policy."""
    if retained.get("effectsAttempted") is not True or retained.get("state") not in {"unit-owned-policy-pending", "policy-qualified", "stopping", "cleanup-required"} or not HEX.fullmatch(str(retained.get("workerSha256"))):
        raise Unavailable("retained_rank_owner_changed")
    group = cleanup_unit_group(plan, unit)
    supervisor = retained.get("supervisor")
    if not isinstance(supervisor, dict) or set(supervisor) != {"pid", "startTicks", "uid"}:
        raise Unavailable("normal_stop_supervisor_binding_changed")
    main = int(unit.get("MainPID", "0"))
    procs = cgroup_processes(group)
    if unit.get("ActiveState") in ("inactive", "failed"):
        if main or procs or supervisor.get("pid", 0) <= 1 or supervisor.get("uid") != plan["uid"] or not str(supervisor.get("startTicks", "")):
            raise Unavailable("normal_stop_supervisor_binding_changed")
        return False
    if unit.get("ActiveState") not in ("active", "activating", "deactivating") or unit.get("ControlGroup") != "/system.slice/" + unit_name(plan):
        raise Unavailable("unit_state_unknown")
    if main <= 1 or supervisor != identity(main) or supervisor.get("uid") != plan["uid"] or main not in procs or any(identity(pid)["uid"] != plan["uid"] for pid in procs):
        raise Unavailable("normal_stop_supervisor_binding_changed")
    return True

def observe_cleanup(plan, retained):
    group = Path("/sys/fs/cgroup/system.slice") / unit_name(plan)
    deadline = time.monotonic() + 12
    while time.monotonic() < deadline:
        unit = show_unit(plan, policy=False)
        procs = cgroup_processes(group)
        if unit["LoadState"] == "not-found":
            if procs: raise Unavailable("unit_missing_but_cgroup_occupied")
            return {"state":"stopped","cleanupConfirmed":True,"unit":unit_name(plan),"planHash":retained["planHash"]}
        cleanup_unit_group(plan, unit)
        if unit["ActiveState"] not in ("deactivating", "inactive", "failed"):
            time.sleep(0.1)
            continue
        if int(unit.get("MainPID", "0")) != 0 or procs:
            time.sleep(0.1)
            continue
        time.sleep(0.1)  # --collect should now remove the exact inactive transient unit.
    raise Unavailable("rank_cgroup_cleanup_unconfirmed")

def effective_bpf_programs(cgroup):
    if not Path("/sys/fs/cgroup/cgroup.controllers").exists(): raise Unavailable("unified_cgroup_v2_required")
    number = {"x86_64": 321, "aarch64": 280}.get(platform.machine())
    if number is None: raise Unavailable("bpf_query_architecture_unqualified")
    class Query(ctypes.Structure):
        # The kernel writes query.revision at offset 56 even for EFFECTIVE
        # queries. Allocate the full UAPI output; zero extensions remain valid
        # on older kernels. A short buffer can overwrite adjacent Python data.
        _fields_ = [("target_fd", ctypes.c_uint32), ("attach_type", ctypes.c_uint32), ("query_flags", ctypes.c_uint32), ("attach_flags", ctypes.c_uint32), ("prog_ids", ctypes.c_uint64), ("prog_cnt", ctypes.c_uint32), ("padding", ctypes.c_uint32), ("prog_attach_flags", ctypes.c_uint64), ("link_ids", ctypes.c_uint64), ("link_attach_flags", ctypes.c_uint64), ("revision", ctypes.c_uint64)]
    descriptor = os.open(cgroup, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    libc = ctypes.CDLL(None, use_errno=True)
    result = {}
    try:
        for attach, label in ((0, "ingress"), (1, "egress")):
            ids = (ctypes.c_uint32 * 64)()
            query = Query(descriptor, attach, 1, 0, ctypes.addressof(ids), 64)
            if libc.syscall(number, 16, ctypes.byref(query), ctypes.sizeof(query)) != 0 or not 1 <= query.prog_cnt <= 64:
                raise Unavailable("effective_cgroup_bpf_" + label + "_unconfirmed")
            result[label] = list(ids)[:query.prog_cnt]
    finally: os.close(descriptor)
    return result

def cgroup_processes(cgroup):
    result = set()
    if not cgroup.exists(): return result
    for path in [cgroup, *cgroup.rglob("*")]:
        if path.is_dir():
            result.update(int(value) for value in (path / "cgroup.procs").read_text().split())
    return result

def runtime_name(plan):
    return "nvpair-rank-" + sha(canonical(plan))[:24]

def verify_device_access(plan):
    for device in plan["devices"]:
        try: descriptor = os.open(device, os.O_RDWR | os.O_CLOEXEC | os.O_NOFOLLOW)
        except OSError: raise Unavailable("rank_device_access_unavailable") from None
        try:
            if not stat.S_ISCHR(os.fstat(descriptor).st_mode): raise Unavailable("rank_device_access_unavailable")
        finally: os.close(descriptor)

def observe_direct_socket_lane(lane):
    import fcntl
    name = lane["interfaceName"]
    try:
        if socket.if_nametoindex(name) != lane["interfaceIndex"] or (Path("/sys/class/net") / name / "address").read_text().strip().lower() != lane["mac"]:
            return False
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as probe:
            data = fcntl.ioctl(probe.fileno(), 0x8915, name.encode()[:15].ljust(256, b"\0"))
        return socket.inet_ntoa(data[20:24]) == lane["localAddress"]
    except OSError:
        return False

def bind_direct_socket(env, lane, management_interface, observed):
    # Only NCCL Socket moves to the lane; VLLM_HOST_IP, Gloo and the master
    # address stay on the management interface.
    if not observed or lane["interfaceName"] == management_interface: raise Unavailable("qualified_direct_socket_lane_changed")
    return {**env, "NCCL_SOCKET_IFNAME": "=" + lane["interfaceName"]}

def bind_qwen_roce(env, hcas, gid_indexes):
    if len(gid_indexes) != 1: raise Unavailable("qualified_roce_gid_changed")
    ib_env = {"NCCL_NET": "IB", "NCCL_NET_PLUGIN": "none", "NCCL_ENV_PLUGIN": "none", "NCCL_GIN_PLUGIN": "none", "NCCL_IB_DISABLE": "0", "NCCL_IB_HCA": "=" + ",".join(hcas), "NCCL_IB_GID_INDEX": str(next(iter(gid_indexes))), "NCCL_IB_ROCE_VERSION_NUM": "2", "NCCL_NET_GDR_LEVEL": "LOC", "NCCL_NET_GDR_C2C": "0", "NCCL_NET_GDR_READ": "0", "NCCL_CROSS_NIC": "1", "NCCL_DEBUG": "WARN"}
    ib_env["NCCL_IB_MERGE_NICS"] = "1"
    return {**env, **ib_env}

def model_environment(plan, bin_dir):
    import fcntl
    interface = None
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as probe:
        for _, name in socket.if_nameindex():
            try:
                data = fcntl.ioctl(probe.fileno(), 0x8915, name.encode()[:15].ljust(256, b"\0"))
                if socket.inet_ntoa(data[20:24]) == plan["localAddress"]: interface = name
            except OSError: pass
    if interface is None: raise Unavailable("admitted_local_interface_missing")
    runtime_dir = plan["runtimeDir"]
    env = {"PATH": str(bin_dir) + ":/usr/bin:/bin", "HOME": runtime_dir + "/.pair-home", "XDG_CACHE_HOME": runtime_dir + "/.pair-cache", "XDG_CONFIG_HOME": runtime_dir + "/.pair-home/config", "TMPDIR": "/run/" + runtime_name(plan), "TEMP": "/run/" + runtime_name(plan), "TMP": "/run/" + runtime_name(plan), "VLLM_RPC_BASE_PATH": "/run/" + runtime_name(plan), "HF_HOME": runtime_dir + "/.pair-cache/hf", "TORCH_EXTENSIONS_DIR": runtime_dir + "/.pair-cache/torch", "TRITON_CACHE_DIR": runtime_dir + "/.pair-cache/triton", "CUDA_CACHE_PATH": runtime_dir + "/.pair-cache/cuda", "VLLM_CACHE_ROOT": runtime_dir + "/.pair-cache/vllm", "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8", "PYTHONNOUSERSITE": "1", "PYTHONSAFEPATH": "1", "PYTHONDONTWRITEBYTECODE": "1", "HF_HUB_OFFLINE": "1", "TRANSFORMERS_OFFLINE": "1", "HF_HUB_DISABLE_IMPLICIT_TOKEN": "1", "CUDA_VISIBLE_DEVICES": plan["gpuUuid"], "VLLM_HOST_IP": plan["localAddress"], "VLLM_USE_FLASHINFER_SAMPLER": "0", "VLLM_ALLREDUCE_USE_FLASHINFER": "0", "NCCL_NET": "Socket", "NCCL_IB_DISABLE": "1", "NCCL_SOCKET_FAMILY": "AF_INET", "NCCL_SOCKET_IFNAME": "=" + interface, "GLOO_SOCKET_IFNAME": interface}
    if "directSocket" in plan:
        env = bind_direct_socket(env, plan["directSocket"], interface, observe_direct_socket_lane(plan["directSocket"]))
    if plan["model"] == QWEN_MODEL:
        hcas, gid_indexes = [], set()
        for lane in plan["rdmaLanes"]:
            name = lane["interfaceName"]
            if socket.if_nametoindex(name) != lane["interfaceIndex"] or (Path("/sys/class/net") / name / "address").read_text().strip().lower() != lane["mac"].lower():
                raise Unavailable("qualified_roce_lane_changed")
            with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as fabric_probe:
                data = fcntl.ioctl(fabric_probe.fileno(), 0x8915, name.encode()[:15].ljust(256, b"\0"))
                if socket.inet_ntoa(data[20:24]) != lane["localAddress"]: raise Unavailable("qualified_roce_lane_changed")
            try: verbs = [entry for entry in os.listdir(Path("/sys/class/infiniband") / lane["rdmaDevice"] / "device" / "infiniband_verbs") if re.fullmatch(r"uverbs[0-9]{1,3}", entry)]
            except OSError: raise Unavailable("qualified_roce_device_changed") from None
            if len(verbs) != 1 or "/dev/infiniband/" + verbs[0] not in plan["devices"]: raise Unavailable("qualified_roce_device_changed")
            rdma = Path("/sys/class/infiniband") / lane["rdmaDevice"] / "ports" / str(lane["gidPort"]) / "gid_attrs"
            if (rdma / "ndevs" / str(lane["gidIndex"])).read_text().strip() != name or (rdma / "types" / str(lane["gidIndex"])).read_text().strip() != "RoCE v2":
                raise Unavailable("qualified_roce_gid_changed")
            gid = (rdma.parent / "gids" / str(lane["gidIndex"])).read_text().strip()
            gid_address = ipaddress.ip_address(gid)
            if gid_address.ipv4_mapped != ipaddress.ip_address(lane["localAddress"]): raise Unavailable("qualified_roce_gid_changed")
            hcas.append(lane["rdmaDevice"] + ":" + str(lane["gidPort"])); gid_indexes.add(lane["gidIndex"])
        env = bind_qwen_roce(env, hcas, gid_indexes)
    return env


_GUARD_PHASES = {"waiting_release", "release_received", "content_verified", "environment_ready", "devices_verified", "gpu_verified", "child_started", "child_exit", "manager_exit", "guard_exception"}

def guard_phase(phase, exit_code=None, failure_code=None):
    # Fixed, tiny journal stderr records only. A diagnostic must never replace
    # or introduce a lifecycle failure; no argv/env/model streams are captured.
    try:
        if phase not in _GUARD_PHASES: return
        value={"event":"PAIR_RANK_GUARD","phase":phase}
        if exit_code is not None:
            if type(exit_code) is not int or not -128 <= exit_code <= 255: return
            value["exit"]=exit_code
        if failure_code is not None:
            value["code"]=failure_code if failure_code in _NATIVE_FAILURE_CODES else "unclassified"
        encoded=canonical(value)
        if len(encoded)>512: return
        sys.stderr.write(encoded.decode("ascii")+"\n")
        sys.stderr.flush()
    except Exception:
        pass

def guard_main():
    payload = bounded_json(Path(os.environ["CREDENTIALS_DIRECTORY"]) / "plan")
    if set(payload) != {"plan", "probe"}: raise Unavailable("credential_binding_invalid")
    plan = check_plan(payload["plan"])
    if os.geteuid() != plan["uid"] or os.geteuid() == 0: raise Unavailable("rank_not_normal_uid")
    if identity(plan["manager"]["pid"]) != plan["manager"]: raise Unavailable("manager_owner_changed")
    manager_fd = os.pidfd_open(plan["manager"]["pid"])
    if identity(plan["manager"]["pid"]) != plan["manager"]: raise Unavailable("manager_owner_changed")
    probe = payload["probe"]
    nonce = probe["nonce"]
    if not RUN.fullmatch(nonce) or type(probe["port"]) is not int: raise Unavailable("probe_binding_invalid")
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as channel:
        channel.bind(("127.0.0.1", 0)); channel.settimeout(8)
        denied = False
        try: channel.sendto(nonce.encode(), ("127.0.0.2", probe["port"]))
        except OSError as exc: denied = exc.errno in (errno.EPERM, errno.EACCES)
        if not denied: raise Unavailable("bpf_egress_deny_not_enforced")
        target = ("127.0.0.1", probe["port"])
        channel.sendto(canonical({"nonce": nonce, "stage": "hello", "pid": os.getpid(), "uid": os.geteuid(), "egressDenied": True}), target)
        allowed = False; forbidden = False
        deadline = time.monotonic() + 0.8
        while time.monotonic() < deadline:
            readable, _, _ = select.select([channel, manager_fd], [], [], max(0, deadline - time.monotonic()))
            if manager_fd in readable:
                guard_phase("manager_exit", exit_code=70)
                raise Unavailable("manager_ended_before_launch")
            if channel in readable:
                data, sender = channel.recvfrom(4096)
                message = json.loads(data)
                if message.get("nonce") != nonce: raise Unavailable("probe_nonce_changed")
                if sender[0] != "127.0.0.1" or message.get("stage") == "forbidden": forbidden = True
                elif message.get("stage") == "allowed": allowed = True
        if forbidden or not allowed: raise Unavailable("bpf_ingress_policy_not_enforced")
        channel.sendto(canonical({"nonce": nonce, "stage": "policy", "ingressAllowed": True, "forbiddenIngressReceived": False, "egressDenied": True}), target)
        guard_phase("waiting_release")
        data, sender = channel.recvfrom(4096)
        if sender != target or json.loads(data) != {"nonce": nonce, "stage": "start"}: raise Unavailable("native_enforcement_release_missing")
        guard_phase("release_received")
    if select.select([manager_fd], [], [], 0)[0]:
        guard_phase("manager_exit", exit_code=70)
        raise Unavailable("manager_ended_before_launch")
    python, cli, bin_dir, runtime_version = verify_content(plan)  # Normal UID rechecks immediately before executing its model.
    guard_phase("content_verified")
    env = model_environment(plan, bin_dir)
    guard_phase("environment_ready")
    verify_device_access(plan)
    guard_phase("devices_verified")
    gpu = subprocess.run(["/usr/bin/nvidia-smi", "--query-gpu=uuid", "--format=csv,noheader,nounits"], env=env, capture_output=True, timeout=3, check=False)
    if gpu.returncode or plan["gpuUuid"].lower() not in gpu.stdout.decode().lower().split(): raise Unavailable("reviewed_gpu_missing")
    guard_phase("gpu_verified")
    if select.select([manager_fd], [], [], 0)[0]:
        guard_phase("manager_exit", exit_code=70)
        raise Unavailable("manager_ended_before_model_launch")
    child = subprocess.Popen([str(python), *model_arguments(plan, cli, runtime_version)], stdin=subprocess.DEVNULL, stdout=None, stderr=None, env=env)
    guard_phase("child_started")
    # This process is unit MainPID. Its normal exit makes systemd terminate the
    # entire non-delegated cgroup, including model grandchildren. No daemonization.
    try:
        while child.poll() is None:
            if select.select([manager_fd], [], [], 0.25)[0]:
                guard_phase("manager_exit", exit_code=72)
                return 72
        guard_phase("child_exit", exit_code=child.returncode)
        return child.returncode or 0
    finally: os.close(manager_fd)


def operation_directory(plan):
    return ROOT / str(plan["uid"]) / unit_name(plan).removesuffix(".service")

@contextmanager
def operation_lock(plan):
    """One fixed operation reservation covers record checks through unit effects.

    A busy lock is an unconfirmed cancellation, never an absent/closed result.
    The kernel releases flock on owner exit; the retained hash binds recovery.
    """
    import fcntl
    root_directory(ROOT); root_directory(ROOT / str(plan["uid"]))
    directory=operation_directory(plan);root_directory(directory)
    descriptor=os.open(directory/"operation.lock",os.O_RDWR|os.O_CREAT|os.O_NOFOLLOW,0o600)
    try:
        info=os.fstat(descriptor)
        if not stat.S_ISREG(info.st_mode) or info.st_uid!=0 or info.st_mode&0o077 or info.st_nlink!=1:
            raise Unavailable("operation_lock_owner_unconfirmed")
        try: fcntl.flock(descriptor,fcntl.LOCK_EX|fcntl.LOCK_NB)
        except BlockingIOError: raise Unavailable("operation_busy_fence_unconfirmed") from None
        raw=os.read(descriptor,1025)
        binding={"owner":OWNER,"uid":plan["uid"],"planHash":sha(canonical(plan))}
        if raw:
            if len(raw)>1024 or json.loads(raw)!=binding: raise Unavailable("operation_reservation_binding_changed")
        else:
            encoded=canonical(binding)
            if os.write(descriptor,encoded)!=len(encoded): raise Unavailable("operation_reservation_write_unconfirmed")
            os.fsync(descriptor)
        yield directory
    finally:
        os.close(descriptor) # Releases only this descriptor's flock.


def close_preunit(plan, record=None):
    """Caller holds operation_lock. A tombstone never starts/stops a unit."""
    directory=operation_directory(plan)
    if record is None:
        record={"owner":OWNER,"plan":plan,"planHash":sha(canonical(plan)),"state":"closing","tombstone":True,"effectsAttempted":False,"cleanupConfirmed":False}
        atomic_json(directory/"owner.json",record) # Retain the cancellation before observing absence.
    unit=show_unit(plan)
    group=Path("/sys/fs/cgroup/system.slice")/unit_name(plan)
    if unit["LoadState"]!="not-found" or cgroup_processes(group):
        raise Unavailable("preunit_absence_unconfirmed_tombstone_retained")
    record["state"]="closed";record["cleanupConfirmed"]=True
    atomic_json(directory/"owner.json",record)
    return {"state":"closed","tombstone":True,"startFenced":True,"effectsApplied":False,"cleanupConfirmed":True,"unit":unit_name(plan),"planHash":record["planHash"]}

def current_root_record(plan):
    directory = operation_directory(plan)
    path = directory / "owner.json"
    record = bounded_json(path)
    if record.get("owner") != OWNER or record.get("plan") != plan or record.get("planHash") != sha(canonical(plan)):
        raise Unavailable("retained_rank_owner_changed")
    return directory, record

def read_status(plan):
    directory, record = current_root_record(plan)
    unit = show_unit(plan)
    group = Path("/sys/fs/cgroup/system.slice") / unit_name(plan)
    if record.get("tombstone"):
        if record.get("state")!="closed" or not record.get("cleanupConfirmed") or unit["LoadState"]!="not-found" or cgroup_processes(group):
            raise Unavailable("preunit_fence_or_absence_unconfirmed")
        return {"state":"closed","tombstone":True,"startFenced":True,"effectsApplied":False,"cleanupConfirmed":True,"unit":unit_name(plan),"planHash":record["planHash"]}
    if unit["LoadState"] == "not-found":
        if cgroup_processes(group): raise Unavailable("unit_missing_but_cgroup_occupied")
        return {"state": "stopped", "cleanupConfirmed": True, "unit": unit_name(plan), "planHash": record["planHash"]}
    group = validate_unit(plan, unit)
    procs = cgroup_processes(group)
    if unit["ActiveState"] not in ("active", "activating", "deactivating", "inactive", "failed"):
        raise Unavailable("unit_state_unknown")
    if unit["ActiveState"] in ("active", "activating"):
        main = int(unit["MainPID"])
        if main <= 1 or main not in procs or identity(main)["uid"] != plan["uid"]:
            raise Unavailable("unit_normal_owner_unconfirmed")
        if any(identity(pid)["uid"] != plan["uid"] for pid in procs): raise Unavailable("foreign_uid_in_rank_cgroup")
        bpf = effective_bpf_programs(group)
        return {"state": "running", "cleanupConfirmed": False, "unit": unit_name(plan), "mainPid": main, "ownedPids": sorted(procs), "bpf": bpf, "transport": record["policy"]["transport"], "policy": record["policy"], "planHash": record["planHash"]}
    return {"state": "stopped" if not procs else "cleanup-required", "cleanupConfirmed": not procs, "unit": unit_name(plan), "planHash": record["planHash"]}

def stop_rank(plan):
    try: directory, record = current_root_record(plan)
    except FileNotFoundError: return close_preunit(plan)
    if record.get("tombstone"): return close_preunit(plan,record)
    unit = show_unit(plan, policy=False)
    if unit["LoadState"] != "not-found":
        if validate_cleanup_unit(plan, unit, record):
            record["state"] = "stopping"
            try: atomic_json(directory / "owner.json", record)
            except Exception: pass  # Journal failure must not suppress owned Stop.
            run_system([SYSTEMCTL, "--system", "--no-ask-password", "--no-pager", "stop", unit_name(plan)], 12)
    result = observe_cleanup(plan, record)
    record["state"] = "stopped"; record["cleanupConfirmed"] = True
    atomic_json(directory / "owner.json", record)
    return result


def post_policy_start_result(channel, sender, nonce, hello, plan, record, directory):
    # Preserve write -> release -> identity ordering. These codes diagnose the
    # exact boundary without retaining exception text, streams or paths.
    record["state"] = "policy-qualified"
    try: atomic_json(directory / "owner.json", record)
    except Exception: raise Unavailable("policy_record_write_failed") from None
    try: channel.sendto(canonical({"nonce": nonce, "stage": "start"}), sender)
    except Exception: raise Unavailable("release_send_failed") from None
    try: supervisor = identity(hello["pid"])
    except (FileNotFoundError, ProcessLookupError):
        raise Unavailable("supervisor_identity_missing") from None
    except OSError:
        raise Unavailable("supervisor_identity_unavailable") from None
    except (IndexError, KeyError, StopIteration, TypeError, ValueError):
        raise Unavailable("supervisor_identity_invalid") from None
    if supervisor != record.get("supervisor"):
        raise Unavailable("policy_probe_not_owned_unit_main")
    # The complete identity was retained before release. A post-release identity
    # read remains mandatory: disappearance/change must still fail visibly.
    # This acknowledges owned launch/policy, never model readiness.
    return {"state": "started", "unit": unit_name(plan), "planHash": record["planHash"], "supervisor": supervisor, "effectsApplied": True, "cleanupConfirmed": False, "policy": record["policy"]}


def cleanup_failed_start(plan, record, directory):
    # Cleanup/persistence failures must not overwrite the original classified
    # Start exception. No cleanup success is inferred from this best effort.
    try: stop_rank(plan)
    except Exception:
        record["state"] = "cleanup-required"
        try: atomic_json(directory / "owner.json", record)
        except Exception: pass

def start_rank(plan, shipped_source):
    try: _, retained=current_root_record(plan)
    except FileNotFoundError: retained=None
    if retained is not None:
        raise Unavailable("rank_operation_closed" if retained.get("tombstone") else "rank_operation_already_retained_no_retry")
    if not isinstance(shipped_source, bytes) or len(shipped_source) > 128 * 1024:
        raise Unavailable("fixed_embedded_worker_bytes_required")
    if identity(plan["manager"]["pid"]) != plan["manager"]: raise Unavailable("manager_owner_changed")
    verify_content(plan)
    # Refuse pre-existing unit names; a new run cannot adopt/replace a foreign unit.
    previous = show_unit(plan)
    if previous["LoadState"] != "not-found": raise Unavailable("rank_unit_already_exists")
    directory = operation_directory(plan)
    record = {"owner": OWNER, "plan": plan, "planHash": sha(canonical(plan)), "state": "prepared", "effectsAttempted": False, "cleanupConfirmed": False, "workerSha256": sha(shipped_source)}
    atomic_json(directory / "owner.json", record)  # Retain exact owner before unit effects.
    worker = directory / "worker.py"
    fd = os.open(worker, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o555)
    os.fchmod(fd, 0o555)
    with os.fdopen(fd, "wb") as stream: stream.write(shipped_source); stream.flush(); os.fsync(stream.fileno())
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as channel:
        channel.bind(("127.0.0.1", 0)); channel.settimeout(10)
        nonce = os.urandom(16).hex()
        credential = directory / "credential.json"
        atomic_json(credential, {"plan": plan, "probe": {"nonce": nonce, "port": channel.getsockname()[1]}})
        argv = [SYSTEMD_RUN, "--system", "--no-ask-password", "--quiet", "--collect", "--unit=" + unit_name(plan)]
        argv += ["--property=" + value for value in unit_properties(plan, credential)]
        argv += [PYTHON, "-I", "-S", str(worker), "--rank-guard"]
        record["state"] = "starting"; record["effectsAttempted"] = True
        atomic_json(directory / "owner.json", record)
        try:
            run_system(argv)
            raw, sender = channel.recvfrom(4096)
            hello = json.loads(raw)
            if hello != {"nonce": nonce, "stage": "hello", "pid": hello.get("pid"), "uid": plan["uid"], "egressDenied": True} or sender[0] != "127.0.0.1": raise Unavailable("fixed_rank_probe_invalid")
            identity_unit = show_unit(plan, policy=False)
            validated_supervisor = identity(hello["pid"])
            record["supervisor"] = validated_supervisor
            record["state"] = "unit-owned-policy-pending"
            if not validate_cleanup_unit(plan, identity_unit, record): raise Unavailable("policy_probe_not_owned_unit_main")
            atomic_json(directory / "owner.json", record)
            unit = show_unit(plan); group = validate_unit(plan, unit)
            if int(unit["MainPID"]) != hello["pid"] or validated_supervisor["uid"] != plan["uid"] or hello["pid"] not in cgroup_processes(group): raise Unavailable("policy_probe_not_owned_unit_main")
            bpf = effective_bpf_programs(group)
            with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as forbidden:
                forbidden.bind(("127.0.0.2", 0))
                for _ in range(3): forbidden.sendto(canonical({"nonce": nonce, "stage": "forbidden"}), sender)
            channel.sendto(canonical({"nonce": nonce, "stage": "allowed"}), sender)
            raw, peer = channel.recvfrom(4096)
            policy = json.loads(raw)
            if peer != sender or policy != {"nonce": nonce, "stage": "policy", "ingressAllowed": True, "forbiddenIngressReceived": False, "egressDenied": True}: raise Unavailable("rank_datapath_policy_unconfirmed")
            roce = plan["model"] == QWEN_MODEL
            record["policy"] = {"effectivePrograms": bpf, "ingressAllowed": True, "forbiddenIngressReceived": False, "egressDenied": True, "transport": transport_name(plan), "rdmaDisabled": not roce, "socketPayloadFallback": False, "hcas": [lane["rdmaDevice"] + ":" + str(lane["gidPort"]) for lane in plan.get("rdmaLanes", [])]}
            if roce:
                record["policy"].update(netGdrLevel=0, netGdrC2c=0, netGdrRead=0, netPlugin="none", envPlugin="none", ginPlugin="none")
                record["policy"]["mergeNICs"] = 1
            if "directSocket" in plan: record["policy"]["socketInterface"] = plan["directSocket"]["interfaceName"]
            return post_policy_start_result(channel, sender, nonce, hello, plan, record, directory)
        except Exception:
            cleanup_failed_start(plan, record, directory)
            raise

def control(action, plan, shipped_source=None):
    if os.geteuid() != 0 or platform.system() != "Linux": raise Unavailable("approved_system_manager_worker_required")
    check_plan(plan)
    if action == "status": return read_status(plan)
    if action not in ("start","stop","reconcile"): raise Unavailable("unknown_fixed_rank_action")
    with operation_lock(plan):
        if action == "start": return start_rank(plan, shipped_source)
        return stop_rank(plan)


def normal_status(plan, supervisor):
    """Same-UID live observation with full effective unit-policy readback."""
    check_plan(plan)
    if os.geteuid()!=plan["uid"]: raise Unavailable("normal_uid_required")
    unit=show_unit(plan);group=validate_unit(plan,unit);procs=cgroup_processes(group)
    if unit["LoadState"]!="loaded" or unit["ActiveState"] not in ("active","activating"):
        raise Unavailable("unit_normal_owner_unconfirmed")
    main=int(unit["MainPID"])
    if main<=1 or supervisor!=identity(main) or supervisor["uid"]!=plan["uid"] or main not in procs or any(identity(pid)["uid"]!=plan["uid"] for pid in procs):
        raise Unavailable("normal_stop_supervisor_binding_changed")
    return {"state":"running","cleanupConfirmed":False,"unit":unit_name(plan),"planHash":sha(canonical(plan)),"mainPid":main,"ownedPids":sorted(procs),"transport":transport_name(plan)}


def normal_stop(plan, supervisor):
    """Same-UID Stop signals only the current exact fixed unit supervisor.

    It needs no new administrator prompt. PID/start/UID and the effective unit
    descriptor are rechecked; pidfd pins that identity for the one signal.
    systemd's KillMode then owns descendant cleanup. Missing proof stays held.
    """
    check_plan(plan)
    if os.geteuid()!=plan["uid"] or not hasattr(signal,"pidfd_send_signal"):
        raise Unavailable("normal_uid_pidfd_stop_required")
    unit=show_unit(plan)
    group=Path("/sys/fs/cgroup/system.slice")/unit_name(plan)
    if unit["LoadState"]=="not-found":
        if cgroup_processes(group): raise Unavailable("absent_unit_has_members")
        raise Unavailable("absent_unit_requires_reviewed_root_fence")
    group=validate_unit(plan,unit)
    main=int(unit["MainPID"])
    if main<=1 or supervisor!=identity(main) or supervisor["uid"]!=plan["uid"] or main not in cgroup_processes(group):
        raise Unavailable("normal_stop_supervisor_binding_changed")
    descriptor=os.pidfd_open(main)
    try:
        if identity(main)!=supervisor: raise Unavailable("normal_stop_supervisor_binding_changed")
        signal.pidfd_send_signal(descriptor,signal.SIGTERM)
    finally: os.close(descriptor)
    deadline=time.monotonic()+12
    while time.monotonic()<deadline:
        now=show_unit(plan)
        if now["LoadState"]=="not-found" or now["ActiveState"] in ("inactive","failed"):
            if not cgroup_processes(group): return {"state":"stopped","cleanupConfirmed":True}
        time.sleep(0.1)
    raise Unavailable("normal_stop_cgroup_exit_unconfirmed")

_NATIVE_FAILURE_CODES = {'normal_stop_cgroup_exit_unconfirmed', 'probe_nonce_changed', 'operation_busy_fence_unconfirmed', 'owner_directory_not_root_controlled', 'unified_cgroup_v2_required', 'bpf_ingress_policy_not_enforced', 'rank_not_normal_uid', 'saved_resource_settings_changed', 'unit_missing_but_cgroup_occupied', 'policy_probe_not_owned_unit_main', 'bounded_json_limit', 'model_length_invalid', 'unknown_fixed_rank_action', 'model_config_changed', 'rank_datapath_policy_unconfirmed', 'preunit_fence_or_absence_unconfirmed', 'fixed_owner_plan_required', 'manager_ended_before_model_launch', 'manager_owner_changed', 'effective_ip_policy_changed', 'manager_identity_invalid', 'preunit_absence_unconfirmed_tombstone_retained', 'effective_cgroup_bpf_ingress_unconfirmed', 'normal_uid_pidfd_stop_required', 'unknown_resource_setting', 'operation_reservation_write_unconfirmed', 'content_binding_invalid', 'system_manager_action_unconfirmed', 'model_manifest_changed', 'placement_rank_mismatch', 'effective_runtime_bound_unconfirmed', 'probe_binding_invalid', 'fixed_embedded_worker_bytes_required', 'saved_local_gpu_binding_changed', 'model_content_changed', 'native_enforcement_release_missing', 'bpf_query_architecture_unqualified', 'operation_reservation_binding_changed', 'admitted_private_ipv4_required', 'unsupported_fixed_topology', 'bpf_egress_deny_not_enforced', 'effective_unit_owner_or_policy_changed', 'redirected_model_runtime_path', 'absent_unit_requires_reviewed_root_fence', 'operation_lock_owner_unconfirmed', 'model_runtime_file_not_bounded_regular', 'fixed_collective_port_required', 'manager_ended_before_launch', 'rank_unit_already_exists', 'fixed_rank_probe_invalid', 'normal_uid_required', 'runtime_binding_changed', 'unit_state_unknown', 'normal_stop_supervisor_binding_changed', 'credential_binding_invalid', 'noncanonical_path', 'admitted_local_interface_missing', 'cgroup_owner_mismatch', 'model_path_not_supported_by_fixed_owner', 'manager_uid_mismatch', 'unclassified', 'model_node_gpu_binding_invalid', 'unit_normal_owner_unconfirmed', 'system_manager_readback_incomplete', 'reviewed_gpu_missing', 'managed_runtime_receipt_changed', 'operation_binding_invalid', 'absent_unit_has_members', 'approved_system_manager_worker_required', 'effective_address_families_changed', 'memory_fraction_invalid', 'retained_rank_owner_changed', 'effective_cgroup_bpf_egress_unconfirmed', 'foreign_uid_in_rank_cgroup', 'model_file_manifest_changed', 'two_or_three_exact_peers_required', 'runtime_content_changed', 'rank_cgroup_cleanup_unconfirmed', 'bounded_port_required'}

_NATIVE_FAILURE_CODES.update({'supervisor_identity_invalid', 'supervisor_identity_unavailable', 'policy_record_write_failed', 'release_send_failed', 'supervisor_identity_missing'})
_NATIVE_FAILURE_CODES.update({'qualified_roce_transport_required', 'qualified_roce_lane_required', 'qualified_roce_lane_changed', 'qualified_roce_gid_changed', 'qualified_roce_device_changed'})
_NATIVE_FAILURE_CODES.update({'qualified_direct_socket_required', 'qualified_direct_socket_lane_changed'})
_NATIVE_FAILURE_CODES.update({'fixed_device_policy_required', 'effective_device_policy_changed', 'rank_device_access_unavailable'})

def diagnostic_failure_code(exc):
    code=str(exc) if isinstance(exc,Unavailable) else "unclassified"
    return code if code in _NATIVE_FAILURE_CODES else "unclassified"

def diagnostic_failure_detail(exc):
    if not isinstance(exc, SystemManagerReadbackIncomplete): return ""
    if not exc.missing or any(key not in UNIT_PROPERTIES for key in exc.missing): return ""
    return ",".join(exc.missing)

if __name__ == "__main__":
    # Root controls are invoked by the existing fixed approved worker using the
    # embedded module. No stdin-selected source, shell or generic command exists.
    if sys.argv[1:] != ["--rank-guard"]:
        raise SystemExit("fixed approved worker entrypoint required")
    try: raise SystemExit(guard_main())
    except Exception as exc:
        guard_phase("guard_exception", exit_code=70, failure_code=diagnostic_failure_code(exc))
        raise SystemExit(70)
