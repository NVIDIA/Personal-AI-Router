# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Pure mocked adapter checks. No processes, keys, SSH, GPU or network actions."""

import base64
import copy
import hashlib
import io
import json
import pickle
import shlex
import struct
import unittest
from unittest.mock import patch

import mpi_socket_adapter as adapter

NOW = 1800000000000


def public_fixture():
    # Syntactic public blob only. No key generation/private counterpart exists.
    algorithm = b"ssh-ed25519"
    raw = struct.pack("!I", len(algorithm)) + algorithm + struct.pack("!I", 32) + bytes(32)
    return {"algorithm": "ssh-ed25519", "blob": base64.b64encode(raw).decode(),
            "fingerprint": "SHA256:" + base64.b64encode(hashlib.sha256(raw).digest()).decode().rstrip("=")}


def tool(path):
    return {"path": path, "sha256": "b" * 64, "size": 4096}


def fixture(count=2):
    members = []
    for number in range(1, count + 1):
        account = "spark" + str(number)
        home = "/home/" + account
        build_id = str(number) * 32
        runtime = f"{home}/.local/share/pair-nccl-build-v1/{build_id}/attempt-0001/runtime"
        members.append({"nodeId": account, "principal": account, "uid": 1000, "user": account, "home": home,
                        "sshAddress": "192.0.2." + str(number), "sshPort": 22,
                        "collectiveAddress": "10.50.0." + str(number), "interface": "enp" + str(number),
                        "gpuUUID": "GPU-fixture-" + str(number), "hostKey": public_fixture(),
                        "buildOperationId": build_id, "buildAttempt": 1, "buildPlanDigest": "c" * 64,
                        "manager": tool(home + "/.local/share/Nvidia Corporation/Personal AI Router/nvpair-engine-manager"),
                        "binary": tool(runtime + "/bin/all_reduce_perf"), "ncclLibrary": tool(runtime + "/lib/libnccl.so.2"),
                        "cudaLibrary": tool("/usr/local/cuda-13.0/targets/aarch64-linux/lib/libcudart.so.13.0.88"),
                        "mpiLibrary": tool("/usr/lib/aarch64-linux-gnu/libmpi.so.40.30.4"),
                        "tools": {name: tool(path) for name, path in adapter.SYSTEM_TOOLS.items()}})
    result = {"schemaVersion": 1, "recipeId": adapter.RECIPE, "operationId": "a" * 32, "groupId": "pair-smoke-fixture",
              "profileDigest": "d" * 64, "ownerNodeId": "spark1", "createdAt": NOW - 1000, "expiresAt": NOW + 110000,
              "subnet": "10.50.0.0/24", "sshSourceIPv4": "192.0.2.1", "operationPublicKey": public_fixture(),
              "members": members, "limits": adapter.LIMITS}
    return resign(result)


def triple_fixture():
    plan = fixture(3)
    plan.update(recipeId="pair-three-spark-nccl-socket-correctness-v3", limits={**adapter.LIMITS, "ranks": 3})
    return resign(plan)


def resign(plan):
    plan.pop("planDigest", None)
    plan["planDigest"] = adapter.digest(plan)
    return plan


class AdapterTests(unittest.TestCase):
    def setUp(self):
        self.process = patch("subprocess.Popen", side_effect=AssertionError("processes forbidden"))
        self.network = patch("socket.create_connection", side_effect=AssertionError("network forbidden"))
        self.process.start()
        self.network.start()

    def tearDown(self):
        self.process.stop()
        self.network.stop()

    def test_valid_spec_is_explicitly_integration_blocked(self):
        result = adapter.describe({"action": "describe", "plan": fixture()}, NOW)
        self.assertEqual(result["state"], "integration-required")
        self.assertFalse(result["nativeActionTaken"])
        self.assertFalse(result["spec"]["nativeExecutable"])
        self.assertFalse(result["spec"]["mpiExecuted"] or result["spec"]["gpuExecuted"])
        self.assertTrue(result["spec"]["integrationRequired"])

    def test_no_native_action_or_extra_command_field_can_be_admitted(self):
        for action in ("preparePeer", "startCoordinator", "cancel", "cleanup"):
            with self.assertRaises(adapter.ContractError):
                adapter.describe({"action": action, "plan": fixture()}, NOW)
        for key in ("command", "env", "path", "privateKey", "password"):
            with self.assertRaises(adapter.ContractError):
                adapter.describe({"action": "describe", "plan": fixture(), key: "unreviewed"}, NOW)

    def test_exactly_two_distinct_admitted_participants_and_coordinator(self):
        for change in (lambda p: p["members"].pop(), lambda p: p["members"].append(copy.deepcopy(p["members"][0])),
                       lambda p: p["members"][1].update(nodeId="spark1", principal="spark1"),
                       lambda p: p.update(ownerNodeId="foreign")):
            plan = fixture()
            change(plan)
            with self.assertRaises(adapter.ContractError):
                adapter.validate_plan(resign(plan), NOW)

    def test_lease_expiry_and_resource_bounds_cannot_be_extended(self):
        for change in (lambda p: p.update(expiresAt=NOW), lambda p: p.update(expiresAt=NOW + 121000),
                       lambda p: p.update(limits={**adapter.LIMITS, "mpiSeconds": 900})):
            plan = fixture()
            change(plan)
            with self.assertRaises(adapter.ContractError):
                adapter.validate_plan(resign(plan), NOW)

    def test_artifact_and_host_key_substitution_is_rejected(self):
        for change in (lambda p: p["members"][0]["binary"].update(path="/tmp/foreign"),
                       lambda p: p["members"][0]["tools"]["ssh"].update(path="/tmp/ssh"),
                       lambda p: p["members"][0]["hostKey"].update(fingerprint="SHA256:foreign"),
                       lambda p: p["members"][0]["cudaLibrary"].update(path="/usr/local/cuda-12.8/lib/libcudart.so.12")):
            plan = fixture()
            change(plan)
            with self.assertRaises(adapter.ContractError):
                adapter.validate_plan(resign(plan), NOW)

    def test_cuda_resolved_layout_is_preserved_in_each_rank_environment(self):
        for directory in ("/usr/local/cuda-13.0/lib64",
                          "/usr/local/cuda-13.0/targets/aarch64-linux/lib",
                          "/usr/local/cuda-13.0/targets/sbsa-linux/lib"):
            with self.subTest(directory=directory):
                plan = fixture()
                library_path = directory + "/libcudart.so.13.0.88"
                plan["members"][0]["cudaLibrary"]["path"] = library_path
                approved = adapter.validate_plan(resign(plan), NOW)
                self.assertEqual(approved["members"][0]["cudaLibrary"]["path"], library_path)
                spec = adapter.compile_spec(approved)
                for member in approved["members"]:
                    expected = (member["ncclLibrary"]["path"].rsplit("/", 1)[0] + ":" +
                                member["cudaLibrary"]["path"].rsplit("/", 1)[0])
                    self.assertEqual(spec["rankEnvironments"][member["nodeId"]]["LD_LIBRARY_PATH"], expected)

    def test_cuda_outside_fixed_layouts_or_family_is_rejected(self):
        for library_path in ("/tmp/libcudart.so.13.0.88",
                             "/usr/local/cuda-12.8/lib64/libcudart.so.13.0.88",
                             "/usr/local/cuda-13.0/lib/libcudart.so.13.0.88",
                             "/usr/local/cuda-13.0/targets/x86_64-linux/lib/libcudart.so.13.0.88",
                             "/usr/local/cuda-13.0/lib64/stubs/libcudart.so.13.0.88",
                             "/usr/local/cuda-13.0/lib64/libcudart.so.12",
                             "/usr/local/cuda-13.0/targets/sbsa-linux/lib/libcudart.so.130"):
            with self.subTest(path=library_path):
                plan = fixture()
                plan["members"][0]["cudaLibrary"]["path"] = library_path
                with self.assertRaisesRegex(adapter.ContractError, "runtime_library_layout_mismatch"):
                    adapter.validate_plan(resign(plan), NOW)

    def test_subnet_and_local_interface_constraints_are_typed(self):
        for subnet in ("0.0.0.0/0", "10.50.0.1/24", "10.51.0.0/24"):
            plan = fixture()
            plan["subnet"] = subnet
            with self.assertRaises(adapter.ContractError):
                adapter.validate_plan(resign(plan), NOW)
        plan = fixture()
        plan["members"][1]["interface"] = "enp2;other"
        with self.assertRaises(adapter.ContractError):
            adapter.validate_plan(resign(plan), NOW)

    def test_fixed_rank_env_and_correctness_preset_cannot_inherit_overrides(self):
        plan = fixture()
        spec = adapter.compile_spec(plan)
        for member in plan["members"]:
            env = spec["rankEnvironments"][member["nodeId"]]
            self.assertEqual(env["NCCL_NET"], "Socket")
            self.assertEqual(env["NCCL_NET_PLUGIN"], "none")
            self.assertEqual(env["NCCL_IB_DISABLE"], "1")
            self.assertEqual(env["NCCL_RAS_ENABLE"], "0")
            self.assertEqual(env["NCCL_SOCKET_IFNAME"], "=" + member["interface"])
            self.assertIn(member["ncclLibrary"]["path"].rsplit("/", 1)[0], env["LD_LIBRARY_PATH"])
        args = spec["rankArgv"]
        self.assertEqual(args[args.index("-b") + 1], "8")
        self.assertEqual(args[args.index("-e") + 1], "8388608")
        self.assertEqual(args[args.index("-c") + 1], "1")
        self.assertEqual(spec["outputContract"]["rows"], 21)
        self.assertEqual(spec["outputContract"]["bothWrongCounts"], 0)

    def test_quick_recipe_selects_exact_bounded_workload_without_changing_legacy(self):
        legacy = fixture()
        quick = copy.deepcopy(legacy)
        quick["recipeId"] = "pair-two-spark-nccl-socket-correctness-v2"
        spec = adapter.compile_spec(adapter.validate_plan(resign(quick), NOW))
        self.assertEqual(spec["rankArgv"], ["-b", "8", "-e", "65536", "-f", "2", "-g", "1", "-t", "1", "-n", "3", "-w", "1", "-c", "1", "-N", "1", "-T", "10", "-d", "float", "-o", "sum"])
        self.assertEqual(spec["outputContract"], {"existingParser": "parseDiagnosticOutput", "rows": 14,
            "minimumBytes": 8, "maximumBytes": 65536, "bothWrongCounts": 0, "datatype": "float", "operation": "sum"})
        old = adapter.compile_spec(adapter.validate_plan(legacy, NOW))
        self.assertEqual(old["rankArgv"], list(adapter.NCCL_ARGS))
        self.assertEqual(old["outputContract"]["rows"], 21)
        self.assertEqual(spec["fixedMCA"], old["fixedMCA"])
        self.assertEqual(spec["unitPolicy"], old["unitPolicy"])
        for member in quick["members"]:
            node = member["nodeId"]
            self.assertEqual(spec["rankEnvironments"][node], {**old["rankEnvironments"][node], "UCX_LOG_FILE": "stderr"})
        for recipe in ("foreign", "pair-two-spark-nccl-socket-correctness-v3", "", None):
            bad = copy.deepcopy(quick)
            bad["recipeId"] = recipe
            with self.assertRaises(adapter.ContractError):
                adapter.validate_plan(resign(bad), NOW)
            with self.assertRaises(adapter.ContractError):
                adapter.compile_spec(bad)

    def test_triple_recipe_compiles_exact_two_peer_roster_and_three_quick_ranks(self):
        plan = adapter.validate_plan(triple_fixture(), NOW)
        spec = adapter.compile_spec(plan)
        self.assertEqual(spec["rankArgv"], list(adapter.QUICK_NCCL_ARGS))
        self.assertEqual(spec["outputContract"]["rows"], 14)
        self.assertEqual(spec["outputContract"]["maximumBytes"], 65536)
        self.assertNotIn("peer", spec)
        self.assertEqual(list(spec["peers"]), ["spark2", "spark3"])
        self.assertEqual(len(spec["coordinator"]["mpiApp"].splitlines()), 3)
        self.assertEqual(len(spec["knownHosts"].splitlines()), 3)
        for index, member in enumerate(plan["members"]):
            row = shlex.split(spec["coordinator"]["mpiApp"].splitlines()[index])
            self.assertEqual(row[:5], ["--noprefix", "-np", "1", "-host", member["collectiveAddress"]])
            self.assertEqual(spec["rankEnvironments"][member["nodeId"]]["NCCL_NET"], "Socket")
            self.assertEqual(spec["rankEnvironments"][member["nodeId"]]["NCCL_IB_DISABLE"], "1")
            self.assertEqual(spec["rankEnvironments"][member["nodeId"]]["UCX_LOG_FILE"], "stderr")
            if index:
                peer = spec["peers"][member["nodeId"]]
                self.assertEqual(peer["paths"], adapter.owned_paths(plan, member))
                self.assertEqual(peer["authorizationEntry"], adapter.authorization_entry(plan, member).decode())
        self.assertEqual(spec["fixedMCA"], adapter.compile_spec(fixture())["fixedMCA"])
        self.assertEqual({**plan["limits"], "ranks": 2}, adapter.LIMITS)

    def test_recipes_cannot_change_cardinality_or_triple_canonical_order(self):
        changes = (
            lambda p: p.update(recipeId=adapter.RECIPE),
            lambda p: p.update(recipeId=adapter.QUICK_RECIPE),
            lambda p: p.update(limits={**p["limits"], "ranks": 2}),
            lambda p: p["members"].pop(),
            lambda p: p["members"].append(copy.deepcopy(p["members"][-1])),
            lambda p: p.update(ownerNodeId="spark2"),
            lambda p: p.update(members=[p["members"][0], p["members"][2], p["members"][1]]),
            lambda p: p.update(ownerNodeId="spark3", members=[p["members"][2], p["members"][0], p["members"][1]]),
        )
        for change in changes:
            plan = triple_fixture()
            change(plan)
            with self.subTest(plan=plan["recipeId"], count=len(plan["members"])), self.assertRaises(adapter.ContractError):
                adapter.validate_plan(resign(plan), NOW)
            with self.assertRaises(adapter.ContractError):
                adapter.compile_spec(plan)
        for recipe in (adapter.RECIPE, adapter.QUICK_RECIPE):
            plan = fixture()
            plan.update(recipeId=recipe, members=list(reversed(plan["members"])))
            spec = adapter.compile_spec(adapter.validate_plan(resign(plan), NOW))
            self.assertEqual(spec["coordinator"]["nodeId"], "spark1")
            self.assertEqual(spec["peer"]["nodeId"], "spark2")
            self.assertNotIn("peers", spec)

    def test_triple_callback_binds_target_account_and_exact_daemon_ordinal(self):
        plan = triple_fixture()
        plan["members"][2]["sshPort"] = 2222
        adapter.validate_plan(resign(plan), NOW)
        for ordinal, peer in enumerate(plan["members"][1:], 1):
            command = (f"/usr/bin/orted -mca ess env -mca ess_base_jobid 123 -mca ess_base_vpid {ordinal} "
                       "-mca ess_base_num_procs 3 -mca orte_hnp_uri '123.0;tcp://10.50.0.1:4555'")
            argv = adapter.ssh_launch_argv(plan, peer["collectiveAddress"], command)
            self.assertEqual(argv[-4:-1], ["-p", str(peer["sshPort"]), peer["user"] + "@" + peer["sshAddress"]])
            daemon = shlex.split(argv[-1])
            self.assertEqual(daemon[daemon.index("ess_base_vpid") + 1], str(ordinal))
            self.assertEqual(daemon[daemon.index("ess_base_num_procs") + 1], "3")
            for bad in (command.replace(f"ess_base_vpid {ordinal}", f"ess_base_vpid {3 - ordinal}"),
                        command.replace(f"ess_base_vpid {ordinal}", "ess_base_vpid 0"),
                        command.replace("ess_base_num_procs 3", "ess_base_num_procs 2"),
                        command.replace("ess_base_num_procs 3", "ess_base_num_procs 4"),
                        command + " -mca orte_node_regex 'fixture'", command + " -mca plm_rsh_no_tree_spawn 0",
                        command.replace("tcp://10.50.0.1", "tcp://10.50.0.99"), "/bin/sh -c " + shlex.quote(command)):
                with self.subTest(ordinal=ordinal, command=bad), self.assertRaises(adapter.ContractError):
                    adapter.ssh_launch_argv(plan, peer["collectiveAddress"], bad)
            for target in ("10.50.0.1", peer["sshAddress"], peer["user"] + "@" + peer["collectiveAddress"], "foreign", "10.50.0.99", peer["collectiveAddress"] + ":1"):
                with self.assertRaises(adapter.ContractError):
                    adapter.ssh_launch_argv(plan, target, command)
        ambiguous = copy.deepcopy(plan)
        ambiguous["members"][2]["collectiveAddress"] = ambiguous["members"][1]["collectiveAddress"]
        with self.assertRaises(adapter.ContractError):
            adapter.ssh_launch_argv(ambiguous, "10.50.0.2", command)

    def test_triple_cleanup_requires_every_node_and_each_partial_failure_holds(self):
        plan = triple_fixture()
        receipt = {key: plan[key] for key in ("operationId", "planDigest", "profileDigest")}
        receipt.update(authorizationRemoved=True, agentGone=True, publicArtifactsRemoved=True,
                       nodes=[{"nodeId": member["nodeId"], "unitOwned": True, "unitProcessesGone": True,
                               "rankCleanupConfirmed": True, "unitMetadataRemoved": True} for member in plan["members"]])
        self.assertTrue(adapter.cleanup_confirmed(plan, receipt))
        for ordinal in range(3):
            changed = copy.deepcopy(receipt)
            changed["nodes"].pop(ordinal)
            with self.assertRaises(adapter.ContractError):
                adapter.cleanup_confirmed(plan, changed)
            for field in ("unitOwned", "unitProcessesGone", "rankCleanupConfirmed", "unitMetadataRemoved"):
                changed = copy.deepcopy(receipt)
                changed["nodes"][ordinal][field] = False
                self.assertFalse(adapter.cleanup_confirmed(plan, changed))
        receipt["nodes"][2] = copy.deepcopy(receipt["nodes"][1])
        with self.assertRaises(adapter.ContractError):
            adapter.cleanup_confirmed(plan, receipt)

    def test_mpi_uses_both_cidr_filters_and_attached_no_tree_daemon(self):
        plan = fixture()
        spec = adapter.compile_spec(plan)
        mca = spec["fixedMCA"]
        self.assertEqual(mca["btl_tcp_if_include"], "10.50.0.0/24")
        self.assertEqual(mca["oob_tcp_if_include"], "10.50.0.0/24")
        self.assertEqual(mca["plm_rsh_no_tree_spawn"], "1")
        self.assertEqual(mca["orte_leave_session_attached"], "1")
        self.assertEqual(mca["plm_base_node_regex_threshold"], "0")
        self.assertEqual(spec["coordinator"]["mpiArgv"][:2], ["/usr/bin/mpirun", "--noprefix"])
        self.assertEqual(spec["coordinator"]["mpiArgv"].count("--noprefix"), 1)
        self.assertEqual(spec["coordinator"]["mpiApp"].count("-np 1"), 2)
        self.assertNotIn("all_reduce_perf", spec["coordinator"]["mpiApp"])
        self.assertIn("rank_entry", spec["coordinator"]["mpiApp"])
        rows = spec["coordinator"]["mpiApp"].splitlines()
        for row, member in zip(rows, plan["members"]):
            words = shlex.split(row)
            self.assertEqual(words, ["--noprefix", "-np", "1", "-host", member["collectiveAddress"],
                                    adapter.owned_paths(plan, member)["rankAdapter"], plan["groupId"], plan["operationId"]])

    def test_cidr_must_resolve_to_exactly_the_intended_up_interface(self):
        plan = fixture()
        observed = [{"name": "enp1", "up": True, "addresses": ["10.50.0.1/24"]},
                    {"name": "eth0", "up": True, "addresses": ["192.0.2.1/24"]}]
        self.assertEqual(adapter.verify_interface_choice(plan, "spark1", observed)["interface"], "enp1")
        for bad in (observed[1:], [{**observed[0], "up": False}],
                    observed + [{"name": "extra", "up": True, "addresses": ["10.50.0.99/24"]}]):
            with self.assertRaises(adapter.ContractError):
                adapter.verify_interface_choice(plan, "spark1", bad)

    def test_each_appfile_row_disables_automatic_prefix(self):
        rows = adapter.compile_spec(fixture())["coordinator"]["mpiApp"].splitlines()
        self.assertEqual(len(rows), 2)
        for row in rows:
            words = shlex.split(row)
            self.assertEqual(words[0], "--noprefix")
            self.assertEqual(words.count("--noprefix"), 1)

    def test_agent_identity_is_public_and_strict_host_verification_is_fixed(self):
        spec = adapter.compile_spec(fixture())
        owner = spec["coordinator"]
        self.assertEqual(owner["addKeyArgv"], ["/usr/bin/ssh-add", "-t", "105", "-"])
        self.assertIn("-D", owner["agentArgv"])
        args = owner["sshArgv"]
        for value in ("StrictHostKeyChecking=yes", "IdentitiesOnly=yes", "PasswordAuthentication=no", "ForwardAgent=no"):
            self.assertIn(value, args)
        self.assertIn("BindAddress=192.0.2.1", args)
        self.assertIn("IdentityFile=" + owner["paths"]["publicIdentity"], args)
        self.assertTrue(owner["paths"]["publicIdentity"].endswith("identity.pub"))
        self.assertIn("IdentityAgent=" + owner["paths"]["agentSocket"], args)

    def test_authorization_is_source_expiry_operation_and_public_key_bound(self):
        plan = fixture()
        entry = adapter.authorization_entry(plan, plan["members"][1]).decode()
        self.assertTrue(entry.startswith('restrict,from="192.0.2.1",expiry-time="'))
        self.assertIn('Z",command="', entry)
        self.assertIn(plan["operationId"] + " " + plan["planDigest"], entry)
        self.assertIn(plan["operationPublicKey"]["blob"], entry)
        self.assertIn("pair-nccl-smoke:" + plan["operationId"], entry)

    def test_exact_key_insert_remove_preserves_unrelated_bytes_and_rejects_ambiguity(self):
        plan = fixture()
        entry = adapter.authorization_entry(plan, plan["members"][1])
        original = b"# unrelated keys\nssh-ed25519 public-fixture existing\n"
        inserted = adapter.authorized_keys_edit(original, entry)
        self.assertEqual(adapter.authorized_keys_edit(inserted["bytes"], entry)["bytes"], inserted["bytes"])
        removed = adapter.authorized_keys_edit(inserted["bytes"], entry, remove=True)
        self.assertEqual(removed["bytes"], original)
        with self.assertRaises(adapter.ContractError):
            adapter.authorized_keys_edit(original.rstrip(b"\n"), entry)
        with self.assertRaises(adapter.ContractError):
            adapter.authorized_keys_edit(original + entry + entry, entry, remove=True)
        with self.assertRaises(adapter.ContractError):
            adapter.authorized_keys_edit(original + entry.replace(b"restrict,", b""), entry, remove=True)

    def test_knownhosts_uses_verified_public_bytes_and_correct_nondefault_port(self):
        plan = fixture()
        plan["members"][1]["sshPort"] = 2222
        text = adapter.known_hosts(plan).decode()
        self.assertIn("[192.0.2.2]:2222 ssh-ed25519 ", text)
        self.assertEqual(len(text.splitlines()), 2)
        self.assertNotIn("StrictHostKeyChecking=no", text)

    def daemon_command(self):
        return '/usr/bin/orted -mca ess env -mca ess_base_jobid 1234 -mca ess_base_vpid 1 -mca ess_base_num_procs 2 -mca orte_hnp_uri "1234.0;tcp://10.50.0.1:45000"'

    def test_direct_daemon_dialect_is_reconstructed_and_policy_forced(self):
        plan = fixture()
        argv = adapter.parse_daemon_command(self.daemon_command(), plan, plan["members"][1])
        self.assertEqual(argv[0], "/usr/bin/orted")
        self.assertIn("btl_tcp_if_include", argv)
        self.assertIn("oob_tcp_if_include", argv)
        self.assertEqual(argv[argv.index("orte_leave_session_attached") + 1], "1")
        self.assertEqual(argv[argv.index("plm_base_node_regex_threshold") + 1], "0")
        self.assertNotIn("orte_node_regex", argv)

    def test_node_map_omission_threshold_is_fixed_and_forwarded_without_override(self):
        plan = fixture()
        command = self.daemon_command() + " --mca plm_base_node_regex_threshold 0"
        argv = adapter.parse_daemon_command(command, plan, plan["members"][1])
        self.assertEqual(argv.count("plm_base_node_regex_threshold"), 1)
        self.assertEqual(argv[argv.index("plm_base_node_regex_threshold") + 1], "0")
        for value in ("1", "1024", "-1", "00"):
            with self.subTest(value=value), self.assertRaises(adapter.ContractError):
                adapter.parse_daemon_command(self.daemon_command() + " --mca plm_base_node_regex_threshold " + value,
                                             plan, plan["members"][1])
        with self.assertRaises(adapter.ContractError):
            adapter.parse_daemon_command(command + " -mca plm_base_node_regex_threshold 0", plan, plan["members"][1])

    def test_callback_node_maps_are_always_rejected(self):
        plan = fixture()
        for node_map in ("spark1,spark2", "spark1,10.50.0.2@0,1", "spark[1:1-2]@0(2)",
                         "192.0.2.[1:1-2]@0(2)", "", "foreign@0(2)"):
            with self.subTest(node_map=node_map), self.assertRaises(adapter.ContractError):
                adapter.parse_daemon_command(self.daemon_command() + " -mca orte_node_regex " + shlex.quote(node_map),
                                             plan, plan["members"][1])

    def test_forwarded_double_dash_mca_and_exact_coordinator_adapter_are_supported(self):
        plan = fixture()
        selector = adapter.compile_spec(plan)["coordinator"]["paths"]["sshAdapter"]
        command = self.daemon_command().replace("-mca", "--mca") + " --mca plm_rsh_agent " + selector
        result = adapter.parse_daemon_command(command, plan, plan["members"][1])
        self.assertNotIn("plm_rsh_agent", result)
        with self.assertRaises(adapter.ContractError):
            adapter.parse_daemon_command(command.replace(selector, "/tmp/foreign"), plan, plan["members"][1])

    def test_daemon_shell_setup_other_commands_and_wrong_contact_are_rejected(self):
        plan = fixture()
        for command in (self.daemon_command() + " ; id", "PATH=/tmp; " + self.daemon_command(),
                        "PATH=/usr/bin:$PATH ; export PATH ; " + self.daemon_command(),
                        self.daemon_command().replace("10.50.0.1", "10.50.0.99"),
                        self.daemon_command().replace("/usr/bin/orted", "/bin/sh"),
                        self.daemon_command() + " -mca arbitrary_path /tmp/code",
                        self.daemon_command() + " --daemonize",
                        self.daemon_command() + " -mca oob_tcp_if_include eth0"):
            with self.assertRaises(adapter.ContractError):
                adapter.parse_daemon_command(command, plan, plan["members"][1])

    def test_internal_ssh_adapter_maps_only_the_admitted_peer_and_control_port(self):
        plan = fixture()
        plan["members"][1]["sshPort"] = 2222
        argv = adapter.ssh_launch_argv(plan, "10.50.0.2", self.daemon_command())
        self.assertEqual(argv[-4:-1], ["-p", "2222", "spark2@192.0.2.2"])
        self.assertIn("/usr/bin/orted", argv[-1])
        for target in ("10.50.0.1", "192.0.2.2", "spark2", "spark2@10.50.0.2", "foreign@10.50.0.2",
                       "10.50.0.99", "10.50.0.2:1", "10.50.0.2:2222", "10.50.0.2 ",
                       "10.50.0.2 -l foreign", "-l", "-oProxyCommand=id"):
            with self.assertRaises(adapter.ContractError):
                adapter.ssh_launch_argv(plan, target, self.daemon_command())

    def test_volatile_fixture_key_cannot_be_serialized_and_is_zeroed_after_agent_stdin(self):
        value = bytearray(b"NON-KEY memory fixture")
        secret = adapter.VolatileKey(value)
        self.assertNotIn("NON-KEY", repr(secret))
        with self.assertRaises(TypeError):
            json.dumps({"private": secret})
        with self.assertRaises(TypeError):
            pickle.dumps(secret)
        pipe = io.BytesIO()
        secret.write_agent_stdin(pipe)
        self.assertEqual(pipe.getvalue(), b"NON-KEY memory fixture")
        self.assertEqual(value, bytes(len(value)))

    def test_volatile_fixture_key_is_zeroed_when_agent_transfer_fails(self):
        value = bytearray(b"NON-KEY fixture")
        class BrokenPipe:
            def write(self, _data):
                raise OSError("fixture")
        with self.assertRaises(OSError):
            adapter.VolatileKey(value).write_agent_stdin(BrokenPipe())
        self.assertEqual(value, bytes(len(value)))

    def test_volatile_key_transfer_handles_short_writes_and_rejects_no_progress(self):
        class ShortPipe:
            def __init__(self):
                self.received = bytearray()
            def write(self, data):
                self.received.extend(data[:3])
                return min(3, len(data))
            def flush(self):
                pass
        value = bytearray(b"NON-KEY partial fixture")
        expected = bytes(value)
        pipe = ShortPipe()
        adapter.VolatileKey(value).write_agent_stdin(pipe)
        self.assertEqual(pipe.received, expected)
        self.assertEqual(value, bytes(len(value)))
        for count in (0, None):
            value = bytearray(b"NON-KEY fixture")
            pipe = ShortPipe()
            pipe.write = lambda _data, result=count: result
            with self.assertRaises(adapter.ContractError):
                adapter.VolatileKey(value).write_agent_stdin(pipe)
            self.assertEqual(value, bytes(len(value)))

    def test_cleanup_requires_every_resource_and_both_bound_ranks(self):
        plan = fixture()
        receipt = {key: plan[key] for key in ("operationId", "planDigest", "profileDigest")}
        receipt.update(authorizationRemoved=True, agentGone=True, publicArtifactsRemoved=True,
                       nodes=[{"nodeId": member["nodeId"], "unitOwned": True, "unitProcessesGone": True,
                               "rankCleanupConfirmed": True, "unitMetadataRemoved": True} for member in plan["members"]])
        self.assertTrue(adapter.cleanup_confirmed(plan, receipt))
        for field in ("authorizationRemoved", "agentGone", "publicArtifactsRemoved"):
            changed = copy.deepcopy(receipt)
            changed[field] = None
            self.assertFalse(adapter.cleanup_confirmed(plan, changed))
        for field in ("unitOwned", "unitProcessesGone", "rankCleanupConfirmed", "unitMetadataRemoved"):
            changed = copy.deepcopy(receipt)
            changed["nodes"][1][field] = False
            self.assertFalse(adapter.cleanup_confirmed(plan, changed))
        changed = copy.deepcopy(receipt)
        changed["nodes"][1]["nodeId"] = "foreign"
        with self.assertRaises(adapter.ContractError):
            adapter.cleanup_confirmed(plan, changed)


if __name__ == "__main__":
    unittest.main()
