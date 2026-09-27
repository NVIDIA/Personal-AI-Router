# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Fixed two- and three-Spark MPI contracts and pure plan compiler.

The separate mpi_socket_native module implements effects behind the admitted
Go profile and rank lease. This compiler's describe action reports only its
specification and integration requirements, never live execution availability.
"""

import base64
from datetime import datetime, timezone
import hashlib
import ipaddress
import json
import re
import shlex
import struct
import sys
from typing import Protocol

RECIPE = "pair-two-spark-nccl-socket-smoke-v1"
NCCL_ARGS = ("-b", "8", "-e", "8388608", "-f", "2", "-g", "1", "-t", "1", "-n", "10", "-w", "1", "-c", "1", "-N", "1", "-T", "10", "-d", "float", "-o", "sum")
QUICK_RECIPE = "pair-two-spark-nccl-socket-correctness-v2"
TRIPLE_RECIPE = "pair-three-spark-nccl-socket-correctness-v3"
QUICK_NCCL_ARGS = ("-b", "8", "-e", "65536", "-f", "2", "-g", "1", "-t", "1", "-n", "3", "-w", "1", "-c", "1", "-N", "1", "-T", "10", "-d", "float", "-o", "sum")
LIMITS = {"ranks": 2, "leaseSeconds": 120, "mpiSeconds": 90, "unitSeconds": 105,
          "stopSeconds": 10, "agentSeconds": 105, "maxOutputBytes": 1048576,
          "maxAuthorizedKeysBytes": 1048576}
BLOCKERS = (
    "managed_profile_and_lease_must_bind_agent_public_identity_and_runtime_libraries",
    "rank_wrapper_must_apply_verified_library_path_and_all_socket_only_settings",
    "retain_verified_ssh_host_public_key_bytes_for_strict_knownhosts",
    "peer_user_unit_must_own_orted_and_ranks_and_report_exact_cgroup_cleanup",
    "qualify_exact_openmpi41_generated_daemon_command_dialect_without_shell_evaluation",
)
TOKEN = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}\Z")
ID = re.compile(r"[a-f0-9]{32}\Z")
HASH = re.compile(r"[a-f0-9]{64}\Z")
HOME = re.compile(r"/[A-Za-z0-9_./-]+\Z")
INTERFACE = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,14}\Z")
MAC = re.compile(r"[0-9a-f]{2}(?::[0-9a-f]{2}){5}\Z")
SYSTEM_TOOLS = {name: "/usr/bin/" + name for name in ("mpirun", "orted", "ssh", "ssh-agent", "ssh-add", "python3", "systemd-run", "systemctl", "busctl", "nvidia-smi")}
# A fabric plan moves only NCCL Socket: a direct fabric lane is one /30, and each
# routed ring member advertises its address on a different /31 cable.
FABRIC_RECIPES = {"spark-two-node-temporary-addresses-v1": (2, 30), "spark-three-node-ring-routed-v2": (3, 31)}
PRIVATE_NETWORKS = tuple(ipaddress.IPv4Network(value) for value in ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"))


class ContractError(Exception):
    pass


def recipe_ranks(recipe_id):
    if recipe_id in (RECIPE, QUICK_RECIPE):
        return 2
    if recipe_id == TRIPLE_RECIPE:
        return 3
    raise ContractError("fixed_plan_mismatch")


def plan_roster(plan):
    ranks = recipe_ranks(plan["recipeId"])
    members = plan["members"]
    if not isinstance(members, list) or len(members) != ranks or plan["limits"] != {**LIMITS, "ranks": ranks}:
        raise ContractError("recipe_participant_count_mismatch")
    ids = [m["nodeId"] for m in members]
    if len(set(ids)) != ranks or plan["ownerNodeId"] not in ids:
        raise ContractError("participant_identity_mismatch")
    if ranks == 3 and (ids[0] != plan["ownerNodeId"] or ids != sorted(ids)):
        raise ContractError("triple_participant_order_changed")
    owner = members[ids.index(plan["ownerNodeId"])]
    return owner, [m for m in members if m is not owner]


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True).encode()


def digest(value):
    return hashlib.sha256(canonical(value)).hexdigest()


def exact(value, fields):
    if not isinstance(value, dict) or set(value) != set(fields):
        raise ContractError("unexpected_or_missing_fields")


def bounded_int(value, low, high):
    if type(value) is not int or not low <= value <= high:
        raise ContractError("integer_out_of_range")


def ipv4(value):
    if not isinstance(value, str):
        raise ContractError("invalid_ipv4")
    try:
        address = ipaddress.IPv4Address(value)
    except ValueError:
        raise ContractError("invalid_ipv4") from None
    if str(address) != value or address.is_loopback or address.is_multicast or address.is_unspecified or int(address) == 0xffffffff:
        raise ContractError("invalid_ipv4")
    return address


def public_key(value, operation=False):
    exact(value, ("algorithm", "blob", "fingerprint"))
    allowed = ("ssh-ed25519",) if operation else ("ssh-ed25519", "ecdsa-sha2-nistp256", "ssh-rsa")
    if value["algorithm"] not in allowed or not isinstance(value["blob"], str) or len(value["blob"]) > 8192:
        raise ContractError("unsupported_public_key")
    try:
        raw = base64.b64decode(value["blob"], validate=True)
        length = struct.unpack("!I", raw[:4])[0]
        algorithm = raw[4:4 + length].decode("ascii")
    except (ValueError, UnicodeError, struct.error):
        raise ContractError("invalid_public_key") from None
    if algorithm != value["algorithm"] or length > 64 or len(raw) <= length + 8:
        raise ContractError("invalid_public_key")
    fingerprint = "SHA256:" + base64.b64encode(hashlib.sha256(raw).digest()).decode().rstrip("=")
    if fingerprint != value["fingerprint"]:
        raise ContractError("public_key_fingerprint_mismatch")
    if operation and (len(raw) != 51 or raw[15:19] != struct.pack("!I", 32)):
        raise ContractError("invalid_ed25519_public_key")
    return value["algorithm"] + " " + value["blob"]


def artifact(value, path=None):
    exact(value, ("path", "sha256", "size"))
    if not isinstance(value["path"], str) or not value["path"].startswith("/") or any(c in value["path"] for c in "\x00\r\n") or ".." in value["path"].split("/") or not HASH.fullmatch(str(value["sha256"])):
        raise ContractError("invalid_artifact")
    bounded_int(value["size"], 1, 2 * 1024**3)
    if path is not None and value["path"] != path:
        raise ContractError("artifact_outside_fixed_layout")


def fabric_prefix(binding, ranks):
    exact(binding, ("operationId", "qualificationDigest", "recipeId", "ownerNodeId", "ownerPrincipal"))
    recipe = FABRIC_RECIPES.get(str(binding["recipeId"]))
    if recipe is None or recipe[0] != ranks or not ID.fullmatch(str(binding["operationId"])) or not HASH.fullmatch(str(binding["qualificationDigest"])) \
            or not TOKEN.fullmatch(str(binding["ownerNodeId"])) or not TOKEN.fullmatch(str(binding["ownerPrincipal"])):
        raise ContractError("invalid_fabric_binding")
    return recipe[1]


def fabric_socket(member, subnet, prefix):
    """Return the member's fabric cable; OpenMPI's management subnet must not select it."""
    socket = member["fabric"]
    exact(socket, ("interface", "index", "mac", "address", "prefix"))
    address = ipv4(socket["address"])
    bounded_int(socket["index"], 1, 2**31 - 1)
    if not INTERFACE.fullmatch(str(socket["interface"])) or socket["interface"] == member["interface"] or not MAC.fullmatch(str(socket["mac"])) \
            or type(socket["prefix"]) is not int or socket["prefix"] != prefix or not any(address in network for network in PRIVATE_NETWORKS) or address in subnet:
        raise ContractError("invalid_fabric_socket")
    return ipaddress.IPv4Interface(f"{address}/{prefix}").network


def validate_plan(plan, now_ms):
    fabric = isinstance(plan, dict) and "fabric" in plan
    exact(plan, ("schemaVersion", "recipeId", "operationId", "groupId", "profileDigest", "ownerNodeId",
                 "createdAt", "expiresAt", "subnet", "sshSourceIPv4", "operationPublicKey", "members", "limits", "planDigest")
          + (("fabric",) if fabric else ()))
    body = dict(plan)
    stated = body.pop("planDigest")
    ranks = recipe_ranks(plan["recipeId"])
    if stated != digest(body) or plan["schemaVersion"] != 1 or plan["limits"] != {**LIMITS, "ranks": ranks}:
        raise ContractError("fixed_plan_mismatch")
    if not ID.fullmatch(str(plan["operationId"])) or not TOKEN.fullmatch(str(plan["groupId"])) or not HASH.fullmatch(str(plan["profileDigest"])):
        raise ContractError("invalid_operation_binding")
    bounded_int(plan["createdAt"], 1, now_ms)
    bounded_int(plan["expiresAt"], now_ms + 1, plan["createdAt"] + 120000)
    public_key(plan["operationPublicKey"], operation=True)
    ipv4(plan["sshSourceIPv4"])
    try:
        subnet = ipaddress.IPv4Network(plan["subnet"], strict=True)
    except (ValueError, TypeError):
        raise ContractError("invalid_collective_subnet") from None
    if str(subnet) != plan["subnet"] or not 1 <= subnet.prefixlen <= 30:
        raise ContractError("invalid_collective_subnet")
    if not isinstance(plan["members"], list) or len(plan["members"]) != ranks:
        raise ContractError("recipe_participant_count_mismatch")
    if fabric and plan["recipeId"] == RECIPE:
        raise ContractError("invalid_fabric_binding")
    prefix = fabric_prefix(plan["fabric"], ranks) if fabric else None
    seen, addresses, sockets, cables = set(), set(), set(), set()
    for member in plan["members"]:
        exact(member, ("nodeId", "principal", "uid", "user", "home", "sshAddress", "sshPort", "collectiveAddress",
                       "interface", "gpuUUID", "hostKey", "buildOperationId", "buildAttempt", "buildPlanDigest",
                       "manager", "binary", "ncclLibrary", "cudaLibrary", "mpiLibrary", "tools") + (("fabric",) if fabric else ()))
        if member["nodeId"] != member["principal"] or not TOKEN.fullmatch(str(member["nodeId"])) or member["nodeId"] in seen:
            raise ContractError("participant_identity_mismatch")
        seen.add(member["nodeId"])
        bounded_int(member["uid"], 1, 2**31 - 1)
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_-]{0,31}", str(member["user"])) or not HOME.fullmatch(str(member["home"])) or ".." in member["home"].split("/") or member["home"].endswith("/"):
            raise ContractError("unsupported_account")
        ipv4(member["sshAddress"])
        bounded_int(member["sshPort"], 1, 65535)
        collective = ipv4(member["collectiveAddress"])
        if collective not in subnet or collective in (subnet.network_address, subnet.broadcast_address) or collective in addresses:
            raise ContractError("collective_address_mismatch")
        addresses.add(collective)
        if not INTERFACE.fullmatch(str(member["interface"])) or not re.fullmatch(r"GPU-[A-Za-z0-9-]{1,80}", str(member["gpuUUID"])):
            raise ContractError("invalid_gpu_or_interface")
        if fabric:
            cables.add(fabric_socket(member, subnet, prefix))
            sockets.add(member["fabric"]["address"])
        public_key(member["hostKey"])
        if not ID.fullmatch(str(member["buildOperationId"])) or not HASH.fullmatch(str(member["buildPlanDigest"])):
            raise ContractError("build_binding_missing")
        bounded_int(member["buildAttempt"], 1, 3)
        runtime = f"{member['home']}/.local/share/pair-nccl-build-v1/{member['buildOperationId']}/attempt-{member['buildAttempt']:04d}/runtime"
        artifact(member["binary"], runtime + "/bin/all_reduce_perf")
        artifact(member["ncclLibrary"], runtime + "/lib/libnccl.so.2")
        artifact(member["manager"])
        if member["manager"]["path"].rsplit("/", 1)[-1] != "nvpair-engine-manager":
            raise ContractError("manager_identity_missing")
        artifact(member["cudaLibrary"])
        artifact(member["mpiLibrary"])
        if not re.fullmatch(r"/usr/local/cuda-13\.0/(?:lib64|targets/(?:aarch64|sbsa)-linux/lib)/libcudart\.so\.13(?:\.[0-9]+)*", member["cudaLibrary"]["path"]) or not re.fullmatch(r"/usr/lib/aarch64-linux-gnu/libmpi\.so\.40(?:\.[0-9]+)*", member["mpiLibrary"]["path"]):
            raise ContractError("runtime_library_layout_mismatch")
        exact(member["tools"], SYSTEM_TOOLS)
        for name, expected_path in SYSTEM_TOOLS.items():
            artifact(member["tools"][name], expected_path)
    if plan["ownerNodeId"] not in seen:
        raise ContractError("coordinator_not_admitted")
    if fabric and (len(sockets) != ranks or len(cables) != (1 if prefix == 30 else ranks)):
        raise ContractError("fabric_socket_topology_mismatch")
    plan_roster(plan)
    return plan


def owned_paths(plan, member):
    root = member["home"] + "/.local/share/pair-nccl-smoke-v1/" + plan["operationId"]
    return {"root": root, "agentSocket": root + "/agent.sock", "publicIdentity": root + "/identity.pub",
            "knownHosts": root + "/known_hosts", "sshAdapter": root + "/ssh_adapter",
            "peerAdapter": root + "/peer_entry", "rankAdapter": root + "/rank_entry", "mpiApp": root + "/mpi.app",
            "unit": "pair-nccl-smoke-" + plan["operationId"] + ".service"}


def rank_environment(member, recipe_id=RECIPE):
    recipe_ranks(recipe_id)
    library = member["ncclLibrary"]["path"].rsplit("/", 1)[0]
    socket = member["fabric"]["interface"] if "fabric" in member else member["interface"]
    environment = {"PATH": "/usr/bin:/bin", "HOME": member["home"], "CUDA_VISIBLE_DEVICES": member["gpuUUID"],
            "LD_LIBRARY_PATH": library + ":" + member["cudaLibrary"]["path"].rsplit("/", 1)[0],
            "NCCL_NET": "Socket", "NCCL_NET_PLUGIN": "none", "NCCL_IB_DISABLE": "1", "NCCL_RAS_ENABLE": "0",
            "NCCL_SOCKET_IFNAME": "=" + socket, "NCCL_SOCKET_NTHREADS": "1",
            "NCCL_NSOCKS_PERTHREAD": "1", "NCCL_DEBUG": "WARN"}
    if recipe_id in (QUICK_RECIPE, TRIPLE_RECIPE):
        environment["UCX_LOG_FILE"] = "stderr"
    return environment


def verify_interface_choice(plan, node_id, observations):
    """Check trusted native observations; a CIDR include alone may fall back."""
    member = next((m for m in plan["members"] if m["nodeId"] == node_id), None)
    if member is None or not isinstance(observations, list) or len(observations) > 128:
        raise ContractError("interface_observation_invalid")
    subnet = ipaddress.IPv4Network(plan["subnet"])
    matches = {}
    for row in observations:
        exact(row, ("name", "up", "addresses"))
        if not INTERFACE.fullmatch(str(row["name"])) or type(row["up"]) is not bool or not isinstance(row["addresses"], list) or len(row["addresses"]) > 32:
            raise ContractError("interface_observation_invalid")
        for address in row["addresses"]:
            try:
                interface = ipaddress.IPv4Interface(address)
            except (ValueError, TypeError):
                raise ContractError("interface_observation_invalid") from None
            if row["up"] and interface.ip in subnet:
                matches.setdefault(row["name"], set()).add(str(interface.ip))
    if set(matches) != {member["interface"]} or member["collectiveAddress"] not in matches[member["interface"]]:
        raise ContractError("collective_subnet_does_not_select_exact_interface")
    return {"nodeId": node_id, "interface": member["interface"], "subnet": plan["subnet"], "address": member["collectiveAddress"]}


def verify_fabric_choice(plan, node_id, observations, identity):
    """Check the reviewed NCCL interface's native index, MAC and sole IPv4 address."""
    member = next((m for m in plan["members"] if m["nodeId"] == node_id), None)
    if member is None or "fabric" not in member or not isinstance(observations, list) or len(observations) > 128:
        raise ContractError("fabric_observation_invalid")
    socket = member["fabric"]
    exact(identity, ("index", "mac"))
    rows = []
    for row in observations:
        exact(row, ("name", "up", "addresses"))
        if row["name"] == socket["interface"]:
            rows.append(row)
    if identity != {"index": socket["index"], "mac": socket["mac"]} or len(rows) != 1 or rows[0]["up"] is not True \
            or rows[0]["addresses"] != [socket["address"] + "/" + str(socket["prefix"])]:
        raise ContractError("fabric_socket_interface_changed")
    return {"nodeId": node_id, "interface": socket["interface"], "address": socket["address"]}


def known_hosts(plan):
    # Public bytes must already have passed the existing pinned callback.
    lines = []
    for member in plan["members"]:
        host = member["sshAddress"] if member["sshPort"] == 22 else f"[{member['sshAddress']}]:{member['sshPort']}"
        lines.append(host + " " + public_key(member["hostKey"]))
    return ("\n".join(lines) + "\n").encode()


def authorization_entry(plan, peer):
    command = owned_paths(plan, peer)["peerAdapter"] + " " + plan["operationId"] + " " + plan["planDigest"]
    expiry = datetime.fromtimestamp(plan["expiresAt"] / 1000, timezone.utc).strftime("%Y%m%d%H%M%SZ")
    options = f'restrict,from="{plan["sshSourceIPv4"]}",expiry-time="{expiry}",command="{command}"'
    marker = f"pair-nccl-smoke:{plan['operationId']}:{plan['planDigest']}"
    return (options + " " + public_key(plan["operationPublicKey"], operation=True) + " " + marker + "\n").encode()


def authorized_keys_edit(before, entry, remove=False):
    """Pure exact-byte edit; the native adapter must supply locking/CAS/readback."""
    if type(before) is not bytes or type(entry) is not bytes or len(before) > LIMITS["maxAuthorizedKeysBytes"] or b"\0" in before or not entry.endswith(b"\n") or entry.count(b"\n") != 1:
        raise ContractError("authorized_keys_input_invalid")
    # Reject ambiguous ownership rather than deleting by a broad key/comment match.
    marker = entry.rstrip(b"\n").rsplit(b" ", 1)[-1]
    lines = before.splitlines(keepends=True)
    matches = [line for line in lines if marker in line]
    if matches and matches != [entry]:
        raise ContractError("authorization_ownership_changed")
    if remove:
        after = b"".join(line for line in lines if line != entry)
    elif matches:
        after = before
    else:
        if before and not before.endswith(b"\n"):
            # Inserting a delimiter would complicate exact reversible ownership.
            raise ContractError("authorized_keys_missing_final_newline")
        after = before + entry
    return {"beforeSha256": hashlib.sha256(before).hexdigest(), "afterSha256": hashlib.sha256(after).hexdigest(),
            "entrySha256": hashlib.sha256(entry).hexdigest(), "bytes": after}


def compile_spec(plan):
    owner, peers = plan_roster(plan)
    quick = plan["recipeId"] != RECIPE
    peer_specs = {peer["nodeId"]: {"nodeId": peer["nodeId"], "paths": owned_paths(plan, peer),
                                 "authorizationEntry": authorization_entry(plan, peer).decode()} for peer in peers}
    paths = owned_paths(plan, owner)
    ssh = [owner["tools"]["ssh"]["path"], "-F", "/dev/null", "-o", "BatchMode=yes",
           "-o", "BindAddress=" + plan["sshSourceIPv4"],
           "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + paths["knownHosts"],
           "-o", "GlobalKnownHostsFile=/dev/null", "-o", "IdentityAgent=" + paths["agentSocket"],
           "-o", "IdentityFile=" + paths["publicIdentity"], "-o", "IdentitiesOnly=yes",
           "-o", "PasswordAuthentication=no", "-o", "KbdInteractiveAuthentication=no",
           "-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes", "-o", "RequestTTY=no",
           "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ConnectTimeout=5"]
    mca = {"pml": "ob1", "btl": "self,tcp", "btl_tcp_if_include": plan["subnet"],
           "oob_tcp_if_include": plan["subnet"], "plm_rsh_no_tree_spawn": "1",
           "plm_base_node_regex_threshold": "0",
           "orte_leave_session_attached": "1", "orte_abort_on_non_zero_status": "1",
           "orte_launch_agent": "/usr/bin/orted", "plm_rsh_pass_environ_mca_params": "0",
           "mca_base_param_files": "/dev/null"}
    mpi = [owner["tools"]["mpirun"]["path"], "--noprefix", "--timeout", "90", "--mca", "plm_rsh_agent", paths["sshAdapter"]]
    for name, value in mca.items():
        mpi += ["--mca", name, value]
    mpi += ["--app", paths["mpiApp"]]
    app = "".join(f"--noprefix -np 1 -host {member['collectiveAddress']} {owned_paths(plan, member)['rankAdapter']} {plan['groupId']} {plan['operationId']}\n" for member in plan["members"])
    return {"schemaVersion": 1, "operationId": plan["operationId"], "planDigest": plan["planDigest"],
            "nativeExecutable": False, "integrationRequired": list(BLOCKERS),
            "coordinator": {"nodeId": owner["nodeId"], "paths": paths,
                            "sshArgv": ssh, "mpiArgv": mpi, "mpiApp": app,
                            "mpiEnvironment": {"PATH": "/usr/bin:/bin", "HOME": owner["home"], "SSH_AUTH_SOCK": paths["agentSocket"]},
                            "agentArgv": ["/usr/bin/ssh-agent", "-D", "-a", paths["agentSocket"], "-t", "105"],
                            "addKeyArgv": ["/usr/bin/ssh-add", "-t", "105", "-"],
                            "agentEnvironment": {"SSH_AUTH_SOCK": paths["agentSocket"], "SSH_ASKPASS_REQUIRE": "never", "DISPLAY": ""}},
            **({"peers": peer_specs} if len(peers) == 2 else {"peer": peer_specs[peers[0]["nodeId"]]}),
            "rankArgv": list(QUICK_NCCL_ARGS if quick else NCCL_ARGS),
            "rankEnvironments": {m["nodeId"]: rank_environment(m, plan["recipeId"]) for m in plan["members"]},
            "outputContract": {"existingParser": "parseDiagnosticOutput", "rows": 14 if quick else 21, "minimumBytes": 8,
                               "maximumBytes": 65536 if quick else 8388608, "bothWrongCounts": 0, "datatype": "float", "operation": "sum"},
            "fixedMCA": mca, "knownHosts": known_hosts(plan).decode(),
            "unitPolicy": {"Type": "exec", "ExitType": "cgroup", "RuntimeMaxSec": 105, "TimeoutStopSec": 10,
                           "KillMode": "control-group", "Restart": "no", "LimitCORE": 0, "Slice": "app.slice"},
            "trustBoundary": "Temporary normal-account MPI launch within the owned " + ("triple" if len(peers) == 2 else "pair") + "; stock orted is not a fixed-workload sandbox",
            "runtimeValidated": False, "mpiExecuted": False, "gpuExecuted": False}


def parse_daemon_command(command, plan, peer):
    """Strict proposed direct-orted dialect, not a universal OpenMPI shell parser."""
    owner, peers = plan_roster(plan)
    if peer not in peers:
        raise ContractError("mpi_daemon_identity_changed")
    ranks = recipe_ranks(plan["recipeId"])
    ordinal = plan["members"].index(peer) if ranks == 3 else 1
    if not isinstance(command, str) or len(command) > 8192 or any(c in command for c in "\0\r\n"):
        raise ContractError("unsupported_mpi_daemon_command")
    lexer = shlex.shlex(command, posix=True, punctuation_chars=";&|<>")
    lexer.whitespace_split, lexer.commenters = True, ""
    try:
        words = list(lexer)
    except ValueError:
        raise ContractError("unsupported_mpi_daemon_command") from None
    if not words or words[0] != peer["tools"]["orted"]["path"] or len(words) > 64 or (len(words) - 1) % 3:
        raise ContractError("unsupported_mpi_daemon_command")
    supplied = {}
    for position in range(1, len(words), 3):
        flag, key, value = words[position:position + 3]
        if flag not in ("-mca", "--mca") or key in supplied:
            raise ContractError("unsupported_mpi_daemon_command")
        supplied[key] = value
    core = {"ess", "ess_base_jobid", "ess_base_vpid", "ess_base_num_procs", "orte_hnp_uri"}
    fixed = {**compile_spec(plan)["fixedMCA"], "plm": "rsh"}
    if "orte_node_regex" in supplied:
        raise ContractError("unsupported_mpi_node_map")
    if not core <= set(supplied) or set(supplied) - core - set(fixed) - {"plm_rsh_agent"}:
        raise ContractError("unsupported_mpi_daemon_command")
    if "plm_rsh_agent" in supplied:
        if supplied.pop("plm_rsh_agent") != compile_spec(plan)["coordinator"]["paths"]["sshAdapter"]:
            raise ContractError("mpi_daemon_policy_changed")
    for key, value in fixed.items():
        if key in supplied and supplied[key] != value:
            raise ContractError("mpi_daemon_policy_changed")
    if supplied["ess"] != "env" or supplied["ess_base_num_procs"] != str(ranks) or supplied["ess_base_vpid"] != str(ordinal) or not re.fullmatch(r"[0-9]{1,10}", supplied["ess_base_jobid"]):
        raise ContractError("mpi_daemon_identity_changed")
    uri = re.fullmatch(r"([0-9]{1,10})\.0;tcp://([0-9.]+):([0-9]{1,5})", supplied["orte_hnp_uri"])
    if not uri or uri[1] != supplied["ess_base_jobid"] or uri[2] != owner["collectiveAddress"] or not 1 <= int(uri[3]) <= 65535:
        raise ContractError("mpi_coordinator_contact_changed")
    # Reconstruct argv from validated fields; never pass shell text onward.
    supplied.update(fixed)
    return [peer["tools"]["orted"]["path"], *[part for key in sorted(supplied) for part in ("-mca", key, supplied[key])]]


def ssh_launch_argv(plan, requested_target, generated_daemon_command):
    """Internal MPI callback adapter; never a public command-taking endpoint."""
    _, peers = plan_roster(plan)
    matches = [m for m in peers if requested_target == m["collectiveAddress"]]
    if len(matches) != 1:
        raise ContractError("mpi_ssh_target_not_the_admitted_peer")
    peer = matches[0]
    daemon = parse_daemon_command(generated_daemon_command, plan, peer)
    return [*compile_spec(plan)["coordinator"]["sshArgv"], "-p", str(peer["sshPort"]),
            peer["user"] + "@" + peer["sshAddress"], shlex.join(daemon)]


class VolatileKey:
    """Transport-owned byte buffer; never a JSON/journal/argv field."""
    def __init__(self, value):
        if type(value) is not bytearray or not 1 <= len(value) <= 16384:
            raise ContractError("invalid_volatile_key_buffer")
        self._value = value

    def __repr__(self):
        return "<VolatileKey redacted>"

    def __getstate__(self):
        raise TypeError("volatile keys cannot be persisted")

    def write_agent_stdin(self, pipe):
        try:
            offset = 0
            while offset < len(self._value):
                written = pipe.write(memoryview(self._value)[offset:])
                if type(written) is not int or not 1 <= written <= len(self._value) - offset:
                    raise ContractError("agent_key_transfer_incomplete")
                offset += written
            pipe.flush()
        finally:
            self._value[:] = b"\0" * len(self._value)


class AdmittedNativePort(Protocol):
    """Future Go adapter obligations. No implementation or authority is supplied."""
    def assert_live_lease(self, plan: dict, node_id: str) -> None: ...
    def prepare_peer(self, plan: dict) -> dict: ...
    def start_coordinator(self, plan: dict, private_key: VolatileKey) -> dict: ...
    def observe(self, plan: dict) -> dict: ...
    def cancel(self, plan: dict) -> dict: ...
    def cleanup(self, plan: dict) -> dict: ...


def cleanup_confirmed(plan, receipt):
    plan_roster(plan)
    exact(receipt, ("operationId", "planDigest", "profileDigest", "nodes", "authorizationRemoved", "agentGone", "publicArtifactsRemoved"))
    if any(receipt[key] != plan[key] for key in ("operationId", "planDigest", "profileDigest")):
        raise ContractError("cleanup_binding_changed")
    if not isinstance(receipt["nodes"], list) or len(receipt["nodes"]) != recipe_ranks(plan["recipeId"]):
        raise ContractError("cleanup_participants_missing")
    seen, clean = set(), True
    for row in receipt["nodes"]:
        exact(row, ("nodeId", "unitOwned", "unitProcessesGone", "rankCleanupConfirmed", "unitMetadataRemoved"))
        if row["nodeId"] not in {m["nodeId"] for m in plan["members"]} or row["nodeId"] in seen:
            raise ContractError("cleanup_participant_changed")
        seen.add(row["nodeId"])
        clean = clean and all(row[field] is True for field in ("unitOwned", "unitProcessesGone", "rankCleanupConfirmed", "unitMetadataRemoved"))
    return clean and all(receipt[field] is True for field in ("authorizationRemoved", "agentGone", "publicArtifactsRemoved"))


def describe(request, now_ms):
    exact(request, ("action", "plan"))
    if request["action"] != "describe":
        raise ContractError("native_adapter_not_integrated")
    plan = validate_plan(request["plan"], now_ms)
    return {"state": "integration-required", "nativeActionTaken": False, "spec": compile_spec(plan)}


def main():
    try:
        raw = sys.stdin.buffer.read(128 * 1024 + 1)
        if len(raw) > 128 * 1024:
            raise ContractError("request_too_large")
        request = json.loads(raw)
        result = describe(request, int(datetime.now(timezone.utc).timestamp() * 1000))
    except (ContractError, ValueError, TypeError, KeyError):
        result = {"state": "integration-required", "nativeActionTaken": False, "blockers": list(BLOCKERS)}
    sys.stdout.write(json.dumps(result, separators=(",", ":")) + "\n")


if __name__ == "__main__":
    main()
