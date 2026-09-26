# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Temporary filesystem and mocked process checks; no SSH/services/GPU run."""

import contextlib
import copy
import io
import json
import os
from pathlib import Path, PurePosixPath
import stat
import tempfile
import types
import unittest
from unittest.mock import patch

import mpi_socket_adapter as adapter
import mpi_socket_native as port
from test_mpi_socket_adapter import fixture, triple_fixture, NOW, resign


class SandboxFiles(port.Files):
    def __init__(self, root):
        self.root = Path(root)
        self.locks = set()
        self.special = {}
        self.owners = {}
        self.links = {}
        self.before_atomic = None

    def path(self, path):
        return self.root.joinpath(*PurePosixPath(path).parts[1:])

    def info(self, path):
        original = self.path(path).lstat()
        mode = self.special.get(path, stat.S_IFDIR | 0o700 if self.path(path).is_dir() else stat.S_IFREG | 0o600)
        uid = self.owners.get(path, 0 if path == "/" or path.startswith(("/usr", "/etc")) else 1000)
        return types.SimpleNamespace(st_uid=uid, st_mode=mode, st_size=original.st_size,
                                     st_dev=original.st_dev, st_ino=original.st_ino, st_nlink=1)

    def trusted(self, path, uid, directory=False, private=False, system=False):
        logical = PurePosixPath(path)
        if not logical.is_absolute() or ".." in logical.parts:
            raise port.NativeError("unsafe_owned_path")
        info = self.info(path)
        if stat.S_ISLNK(info.st_mode) or directory and not stat.S_ISDIR(info.st_mode) or not directory and not stat.S_ISREG(info.st_mode):
            raise port.NativeError("unsafe_path_component")
        if info.st_mode & 0o022 or private and info.st_mode & 0o077:
            raise port.NativeError("path_ownership_changed")
        return info

    def sync_dir(self, path):
        pass  # Windows fixture replaces only directory fsync, not file bytes/CAS.

    def link_target(self, path):
        return self.links[path]

    @contextlib.contextmanager
    def lock(self, path, uid, deadline=None):
        if path in self.locks:
            raise AssertionError("recursive lease/auth lock")
        self.locks.add(path)
        try:
            yield
        finally:
            self.locks.remove(path)

    def atomic(self, path, data, uid, expected=None, mode=0o600):
        if self.before_atomic:
            callback, self.before_atomic = self.before_atomic, None
            callback(path)
        super().atomic(path, data, uid, expected, mode)

    def put(self, path, data):
        self.path(path).parent.mkdir(parents=True, exist_ok=True)
        self.path(path).write_bytes(data)


class FakeProcess:
    def __init__(self, pid, argv):
        self.pid, self.argv, self.code = pid, argv, None
        self.stdout = io.BytesIO()

    def poll(self):
        return self.code

    def wait(self, timeout=None):
        return self.code


class FakeSystem:
    def __init__(self, files, member):
        self.fs, self.member = files, member
        self.native = None
        self.calls, self.processes = [], {}
        self.active, self.jobs, self.populated = None, [], False
        self.clock = NOW
        self.monotonic_clock = 1000.0
        self.worker_callback = None
        self.key_load_length = None
        self.change_unit = None

    def account(self):
        return {k: self.member[k] for k in ("uid", "user", "home")}

    def now(self):
        return self.clock

    def monotonic(self):
        return self.monotonic_clock

    def advance(self, seconds):
        self.monotonic_clock += seconds
        self.clock += int(seconds * 1000)

    def interfaces(self):
        return [{"name": self.member["interface"], "up": True, "addresses": [self.member["collectiveAddress"] + "/24"]},
                {"name": "eth0", "up": True, "addresses": [self.member["sshAddress"] + "/24"]}]

    def cgroup_state(self, populated):
        self.populated = populated
        self.fs.put("/sys/fs/cgroup" + self.native.cgroup() + "/cgroup.events", b"populated " + (b"1" if populated else b"0") + b"\n")

    def spawn(self, argv, env, input_pipe=False, separate_stderr=False):
        self.calls.append({"argv": argv, "env": env, "inputPipe": input_pipe, "separateStderr": separate_stderr})
        proc = FakeProcess(10000 + len(self.processes), argv)
        self.processes[proc.pid] = proc
        if argv[0] == "/usr/bin/systemd-run":
            j = self.native.journal()
            self.active = {"Id": self.native.paths["unit"], "LoadState": "loaded", "Description": port.OWNER + ":" + self.native.plan["operationId"] + ":" + self.native.plan["planDigest"] + ":" + self.member["nodeId"],
                           "Transient": "yes", "Type": "exec", "ExitType": "cgroup", "KillMode": "control-group", "Restart": "no", "RemainAfterExit": "yes",
                           "ActiveState": "active", "SubState": "running", "MainPID": str(os.getpid()), "ControlPID": "0", "ControlGroup": self.native.cgroup(),
                           "InvocationID": "e" * 32, "RuntimeMaxUSec": str(j["unitSeconds"]) + "s", "TimeoutStopUSec": "10s", "LimitCORE": "0", "Slice": "app.slice", "ExecMainCode": "0", "ExecMainStatus": "0"}
            self.cgroup_state(True)
            self.fs.put(f"/proc/{os.getpid()}/cgroup", ("0::" + self.native.cgroup() + "\n").encode())
        elif argv[0] == "/usr/bin/ssh-agent":
            self.fs.put(self.native.paths["agentSocket"], b"")
            self.fs.special[self.native.paths["agentSocket"]] = stat.S_IFSOCK | 0o600
        elif argv[0] not in ("/usr/bin/ssh-add", "/usr/bin/mpirun", "/usr/bin/orted"):
            raise AssertionError("unexpected spawned command")
        return proc

    def wait(self, proc, seconds, data=None, progress=None, deadline=None, stderr=None):
        if proc.argv[0] != "/usr/bin/systemd-run":
            result = self.run(proc.argv, {}, seconds, data, progress, deadline, stderr=stderr)
            proc.code = result[0]
            return result
        if self.worker_callback:
            self.worker_callback()
        else:
            stream = types.SimpleNamespace(buffer=io.BytesIO(bytes(data or b"")))
            with patch.object(port.sys, "stdin", stream), patch.dict(os.environ, {"INVOCATION_ID": "e" * 32}):
                self.native.worker("coordinator" if self.member["nodeId"] == self.native.plan["ownerNodeId"] else "peer")
        if data is not None:
            data[:] = b"\0" * len(data)
        if self.active:
            self.active.update(MainPID="0", SubState="exited", ExecMainCode="1", ExecMainStatus="0", ControlGroup="")
        self.cgroup_state(False)
        if progress:
            progress()
        proc.code = 0
        return 0, b""

    def run(self, argv, env, seconds=5, data=None, progress=None, deadline=None, stderr=None):
        self.calls.append({"argv": argv, "env": env, "inputLength": len(data) if data is not None else None})
        if argv[0] == "/usr/bin/systemctl":
            if "show" in argv:
                values = self.active or {"Id": self.native.paths["unit"], "LoadState": "not-found"}
                values = {**values, **(self.change_unit or {})}
                return 0, "".join(k + "=" + v + "\n" for k, v in values.items()).encode()
            if "stop" in argv or "reset-failed" in argv:
                self.active = None
                self.cgroup_state(False)
                return 0, b""
        if argv[0] == "/usr/bin/busctl":
            if argv[-1] == "ListJobs":
                return 0, json.dumps({"type": "a(usssoo)", "data": [self.jobs]}).encode()
            if argv[-1] == "ExecStart":
                args = self.native.journal()["unitArgv"]
                return 0, json.dumps({"type": "a(sasbttttuii)", "data": [["/usr/bin/python3", args, False, 0, 0, 0, 0, 0, 0, 0]]}).encode()
        if argv[0] == "/usr/bin/ssh-add":
            if argv[-1] == "-":
                self.key_load_length = len(data)
                data[:] = b"\0" * len(data)
                return 0, b""
            return 0, (adapter.public_key(self.native.plan["operationPublicKey"], True) + " fixture\n").encode()
        if argv[0] in ("/usr/bin/mpirun", "/usr/bin/orted"):
            if progress:
                progress()
            if stderr is not None:
                stderr.extend(b"UCX fixture warning on stderr\n")
            return 0, b"# mocked bounded collective\n"
        if argv[0] == "/usr/bin/ssh":
            return 0, b""
        raise AssertionError("unexpected native tool")

    def ticks(self, pid):
        proc = self.processes.get(pid)
        return "12345" if proc is not None and proc.code is None else None

    def process_gone(self, pid, ticks):
        return self.ticks(pid) != ticks

    def terminate(self, proc):
        proc.code = 0


def profile_for(plan):
    owner = next(m for m in plan["members"] if m["nodeId"] == plan["ownerNodeId"])
    paths = adapter.owned_paths(plan, owner)
    tool = lambda value: {k: value[k] for k in ("path", "sha256")}
    members = []
    for member in plan["members"]:
        runtime = {k: member[k] for k in ("buildOperationId", "buildPlanDigest", "buildAttempt", "uid", "home")}
        runtime.update({k: tool(member[k]) for k in ("ncclLibrary", "cudaLibrary", "mpiLibrary")})
        members.append({"nodeId": member["nodeId"], "principal": member["principal"], "clusterPinSha256": "a" * 64, "host": member["sshAddress"], "user": member["user"],
                        "gpu": member["gpuUUID"], "interface": member["interface"], "manager": tool(member["manager"]), "nccl": tool(member["binary"]), "smi": tool(member["tools"]["nvidia-smi"]), "runtime": runtime})
    return {"transport": "socket", "groupId": plan["groupId"], "label": "fixture", "ownerNodeId": plan["ownerNodeId"], "members": members,
            "mpi": tool(owner["tools"]["mpirun"]), "ssh": tool(owner["tools"]["ssh"]),
            "knownHosts": {"path": paths["knownHosts"], "sha256": port.fingerprint(adapter.known_hosts(plan))}, "identityFile": paths["publicIdentity"],
            "dedicatedTestWindow": True, "bootstrap": {"operationId": plan["operationId"], "publicKey": plan["operationPublicKey"],
                 "agentSocket": paths["agentSocket"], "publicIdentity": paths["publicIdentity"], "subnet": plan["subnet"], "sshSourceIPv4": plan["sshSourceIPv4"]}}


class NativeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.fs = SandboxFiles(self.temp.name)
        self.no_process = patch("subprocess.Popen", side_effect=AssertionError("live processes forbidden"))
        self.no_process.start()
        self.plan = fixture()
        self.install_plan()

    def install_plan(self):
        for member in self.plan["members"]:
            for artifact in [member[k] for k in ("manager", "binary", "ncclLibrary", "cudaLibrary", "mpiLibrary")] + list(member["tools"].values()):
                raw = (artifact["path"] + "\n").encode()
                self.fs.put(artifact["path"], raw)
                artifact.update(size=len(raw), sha256=port.fingerprint(raw))
        self.profile = profile_for(self.plan)
        self.profile_raw = json.dumps(self.profile, separators=(",", ":")).encode()
        self.plan["profileDigest"] = port.fingerprint(self.profile_raw)
        resign(self.plan)
        for member in self.plan["members"]:
            base = member["home"] + "/.config/Nvidia Corporation/Personal AI Router"
            lease_dir = base + "/engine-bin/diagnostic-runs/" + self.plan["operationId"]
            request = {k: self.plan[k] for k in ("groupId", "operationId", "profileDigest", "expiresAt")}
            request["bootstrapPlanDigest"] = self.plan["planDigest"]
            request["executionDeadlineAt"] = min(NOW + 90000, self.plan["expiresAt"])
            self.fs.put(lease_dir + "/lease.json", adapter.canonical({"request": request, "member": next(m for m in self.profile["members"] if m["nodeId"] == member["nodeId"])}))
            self.fs.put(lease_dir + "/profile.json", self.profile_raw)
            self.fs.put(lease_dir + "/bootstrap-plan.json", adapter.canonical(self.plan))
            self.fs.put(base + "/cluster/identity.json", adapter.canonical({"node_uuid": member["principal"]}))

    def tearDown(self):
        self.no_process.stop()
        self.temp.cleanup()

    def native(self, index):
        system = FakeSystem(self.fs, self.plan["members"][index])
        native = port.Native(system, self.fs, Path(port.__file__).read_bytes())
        system.native = native
        return native

    def reset_fixture(self, name):
        self.fs = SandboxFiles(Path(self.temp.name) / name)
        self.plan = fixture()
        self.install_plan()

    def retained_clean_native(self, index=0):
        native = self.native(index)
        if index == 0:
            result = native.start_coordinator(self.plan, adapter.VolatileKey(bytearray(b"private-fixture")))
        else:
            native.prepare_peer(self.plan)
            result = native.cleanup(self.plan)
        self.assertTrue(result["cleanupConfirmed"], result)
        self.assertTrue(native.journal()["cancelled"])
        self.assertTrue(native.journal()["unitCollected"])
        native.system.calls.clear()
        return native

    def test_live_lease_profile_and_both_member_projection_are_required_before_effects(self):
        native = self.native(1)
        native.bind(self.plan)
        root = native.paths["root"]
        for change in (lambda p: p["members"][0].update(interface="other"), lambda p: p["bootstrap"].update(agentSocket="/tmp/foreign")):
            bad = copy.deepcopy(self.profile)
            change(bad)
            self.fs.put(native.lease_dir + "/profile.json", adapter.canonical(bad))
            with self.assertRaises(port.NativeError):
                native.prepare_peer(self.plan)
            self.assertFalse(self.fs.exists(root))
            self.assertEqual(native.system.calls, [])
        self.fs.put(native.lease_dir + "/profile.json", self.profile_raw + b"\n")
        with self.assertRaises(port.NativeError):
            native.prepare_peer(self.plan)

    def test_peer_prepare_and_exact_cleanup_preserve_foreign_authorization(self):
        native = self.native(1)
        original = b"ssh-ed25519 foreign-existing-entry\n"
        auth = self.plan["members"][1]["home"] + "/.ssh/authorized_keys"
        self.fs.put(auth, original)
        prepared = native.prepare_peer(self.plan)
        self.assertEqual(prepared["state"], "prepared")
        self.assertFalse(prepared["cleanupConfirmed"])
        entry = adapter.authorization_entry(self.plan, self.plan["members"][1])
        self.assertEqual(self.fs.path(auth).read_bytes(), original + entry)
        native.prepare_peer(self.plan)
        self.assertEqual(self.fs.path(auth).read_bytes(), original + entry)
        native.system.clock = self.plan["expiresAt"] + 1000
        cleaned = native.cleanup(self.plan)
        self.assertTrue(cleaned["cleanupConfirmed"])
        self.assertEqual(self.fs.path(auth).read_bytes(), original)
        self.assertTrue(self.fs.exists(native.lease_dir + "/cancelled"))
        self.assertTrue(native.cleanup(self.plan)["cleanupConfirmed"])

    def test_retained_cleanup_accepts_only_exact_system_tool_artifact_drift(self):
        for code in sorted(port.SYSTEM_TOOL_ARTIFACT_DRIFT):
            with self.subTest(code=code):
                self.reset_fixture(code)
                native = self.retained_clean_native()
                tool = native.member["tools"]["systemctl"]
                raw = self.fs.path(tool["path"]).read_bytes()
                identity = contextlib.nullcontext()
                if code == "artifact_size_changed":
                    self.fs.put(tool["path"], raw + b"x")
                elif code == "artifact_bytes_changed":
                    self.fs.put(tool["path"], bytes([raw[0] ^ 1]) + raw[1:])
                else:
                    trusted = self.fs.trusted
                    def changed_identity(path, uid, directory=False, private=False, system=False):
                        info = trusted(path, uid, directory, private, system)
                        if path == tool["path"]:
                            return types.SimpleNamespace(**{**vars(info), "st_ino": info.st_ino + 1})
                        return info
                    identity = patch.object(self.fs, "trusted", side_effect=changed_identity)
                seen = []
                fallback = native.cleanup_after_system_tool_drift
                def observe_fallback(error):
                    seen.append(error.args[0])
                    return fallback(error)
                with identity, patch.object(native, "cleanup_after_system_tool_drift", side_effect=observe_fallback):
                    result = native.cleanup(self.plan)
                self.assertEqual(seen, [code])
                self.assertEqual((result["action"], result["state"]), ("cleanup", "cancelled"))
                self.assertTrue(result["cleanupConfirmed"], result)
                self.assertTrue(all(result[field] for field in ("unitOwned", "unitProcessesGone", "unitMetadataRemoved", "rankCleanupConfirmed", "authorizationRemoved", "agentGone", "publicArtifactsRemoved")))
                self.assertFalse(any(call["argv"][0] in ("/usr/bin/systemctl", "/usr/bin/busctl") for call in native.system.calls))

    def test_retained_cleanup_removes_exact_cancelled_peer_artifacts_after_tool_drift(self):
        native = self.native(1)
        native.prepare_peer(self.plan)
        cancelled = native.cancel(self.plan)
        self.assertTrue(cancelled["unitProcessesGone"])
        self.assertFalse(cancelled["publicArtifactsRemoved"])
        self.assertTrue(all(self.fs.exists(native.paths["root"] + "/" + name) for name in port.PUBLIC_ARTIFACTS))
        native.system.calls.clear()
        tool = native.member["tools"]["systemctl"]
        raw = self.fs.path(tool["path"]).read_bytes()
        self.fs.put(tool["path"], bytes([raw[0] ^ 1]) + raw[1:])
        result = native.cleanup(self.plan)
        self.assertTrue(result["cleanupConfirmed"], result)
        self.assertTrue(all(not self.fs.exists(native.paths["root"] + "/" + name) for name in port.PUBLIC_ARTIFACTS))
        self.assertFalse(any(call["argv"][0] in ("/usr/bin/systemctl", "/usr/bin/busctl") for call in native.system.calls))

    def test_retained_cleanup_refuses_partial_or_tampered_proof(self):
        scenarios = ("cancelled", "unit-collected", "lease-tombstone", "native-tombstone", "cgroup-populated", "cgroup-malformed",
                     "rank-active", "agent-active", "agent-malformed", "authorization-present", "public-bytes", "unjournaled-public", "foreign-socket", "files-missing")
        for scenario in scenarios:
            with self.subTest(scenario=scenario):
                self.reset_fixture(scenario)
                native = self.retained_clean_native(1 if scenario == "authorization-present" else 0)
                journal = native.journal()
                if scenario == "cancelled":
                    journal["cancelled"] = False
                    native.save(journal)
                elif scenario == "unit-collected":
                    journal["unitCollected"] = False
                    native.save(journal)
                elif scenario == "lease-tombstone":
                    self.fs.path(native.lease_dir + "/cancelled").unlink()
                elif scenario == "native-tombstone":
                    self.fs.path(native.paths["root"] + "/cancelled").unlink()
                elif scenario == "cgroup-populated":
                    native.system.cgroup_state(True)
                elif scenario == "cgroup-malformed":
                    self.fs.put("/sys/fs/cgroup" + native.cgroup() + "/cgroup.events", b"malformed\n")
                elif scenario == "rank-active":
                    process = FakeProcess(4242, ["rank"])
                    native.system.processes[process.pid] = process
                    self.fs.put(native.lease_dir + "/rank.json", adapter.canonical({"pid": process.pid, "startTicks": "12345", "done": False, "clean": False}))
                elif scenario == "agent-active":
                    native.system.processes[journal["agent"]["pid"]].code = None
                elif scenario == "agent-malformed":
                    journal["agent"]["startTicks"] = "changed"
                    native.save(journal)
                elif scenario == "authorization-present":
                    path = native.member["home"] + "/.ssh/authorized_keys"
                    self.fs.put(path, adapter.authorization_entry(self.plan, native.member))
                elif scenario == "public-bytes":
                    self.fs.put(native.paths["root"] + "/mpi_socket_native.py", b"changed")
                elif scenario == "unjournaled-public":
                    journal["files"].pop("known_hosts")
                    native.save(journal)
                    self.fs.put(native.paths["root"] + "/known_hosts", adapter.compile_spec(self.plan)["knownHosts"].encode())
                elif scenario == "foreign-socket":
                    self.fs.put(native.paths["agentSocket"], b"not-a-socket")
                    self.fs.special[native.paths["agentSocket"]] = stat.S_IFREG | 0o600
                else:
                    journal.pop("files")
                    native.save(journal)
                with self.assertRaises((port.NativeError, ValueError)):
                    native.cleanup_after_system_tool_drift(port.NativeError("artifact_bytes_changed"))
                if scenario == "public-bytes":
                    self.assertEqual(self.fs.path(native.paths["root"] + "/mpi_socket_native.py").read_bytes(), b"changed")

    def test_retained_cleanup_does_not_widen_status_prepare_or_other_errors(self):
        self.reset_fixture("status")
        native = self.retained_clean_native()
        tool = native.member["tools"]["systemctl"]
        raw = self.fs.path(tool["path"]).read_bytes()
        self.fs.put(tool["path"], bytes([raw[0] ^ 1]) + raw[1:])
        with self.assertRaisesRegex(port.NativeError, "artifact_bytes_changed"):
            native.observe(self.plan)

        for code in ("system_tool_owner_changed", "system_tool_path_changed", "system_tool_link_limit", "file_size_limit", "file_changed_during_read", "unit_observation_unknown"):
            with self.subTest(code=code):
                self.reset_fixture(code)
                native = self.retained_clean_native()
                with patch.object(native, "collect_unit", side_effect=port.NativeError(code)), self.assertRaisesRegex(port.NativeError, code):
                    native.cleanup(self.plan)
        self.reset_fixture("os-error")
        native = self.retained_clean_native()
        with patch.object(native, "collect_unit", side_effect=OSError("fixture")), self.assertRaisesRegex(OSError, "fixture"):
            native.cleanup(self.plan)

        self.reset_fixture("prepare")
        peer = self.native(1)
        peer.bind(self.plan)
        tool = peer.member["tools"]["systemctl"]
        raw = self.fs.path(tool["path"]).read_bytes()
        self.fs.put(tool["path"], bytes([raw[0] ^ 1]) + raw[1:])
        with self.assertRaisesRegex(port.NativeError, "artifact_bytes_changed"):
            peer.prepare_peer(self.plan)
        self.assertFalse(self.fs.exists(peer.paths["root"]))

    def test_authorization_compare_and_swap_detects_foreign_edit(self):
        native = self.native(1)
        native.bind(self.plan)
        with native.lock():
            native.stage()
        path = native.member["home"] + "/.ssh/authorized_keys"
        self.fs.put(path, b"original\n")
        original_atomic = self.fs.atomic
        def changing(target, data, uid, expected=None, mode=0o600):
            if target == path:
                self.fs.put(path, b"foreign update\n")
            return original_atomic(target, data, uid, expected, mode)
        with patch.object(self.fs, "atomic", side_effect=changing), self.assertRaises(port.NativeError):
            native.authorization()
        self.assertEqual(self.fs.path(path).read_bytes(), b"foreign update\n")

    def test_cancel_before_native_prepare_fences_late_launch(self):
        native = self.native(1)
        result = native.cleanup(self.plan)
        self.assertTrue(result["cleanupConfirmed"])
        self.assertEqual(native.journal()["files"], {})
        with self.assertRaises(port.NativeError):
            native.prepare_peer(self.plan)
        self.assertFalse(any(c["argv"][0] == "/usr/bin/systemd-run" for c in native.system.calls))

    def test_expired_lease_rejects_prepare_and_coordinator_but_allows_cleanup(self):
        native = self.native(0)
        native.system.clock = self.plan["expiresAt"] + 1
        key = adapter.VolatileKey(bytearray(b"private-fixture"))
        with self.assertRaises(adapter.ContractError):
            native.start_coordinator(self.plan, key)
        self.assertEqual(key._value, bytearray(len(key._value)))
        self.assertTrue(native.cleanup(self.plan)["cleanupConfirmed"])

    def test_coordinator_private_key_flows_only_through_stdin_and_all_resources_clean(self):
        native = self.native(0)
        secret = b"private-key-fixture-never-persist"
        raw = bytearray(secret)
        result = native.start_coordinator(self.plan, adapter.VolatileKey(raw))
        self.assertEqual(result["state"], "completed", result)
        self.assertEqual(result["exitCode"], 0)
        self.assertTrue(result["cleanupConfirmed"], result)
        self.assertEqual(native.system.key_load_length, len(secret))
        self.assertEqual(raw, bytearray(len(secret)))
        self.assertNotIn(secret.decode(), json.dumps(native.system.calls))
        for path in self.fs.root.rglob("*"):
            if path.is_file():
                self.assertNotIn(secret, path.read_bytes())
        unit = next(c["argv"] for c in native.system.calls if c["argv"][0] == "/usr/bin/systemd-run")
        self.assertIn("--pipe", unit)
        self.assertIn("--property=KillMode=control-group", unit)
        self.assertIn("--property=ConditionPathExists=!" + native.lease_dir + "/cancelled", unit)
        self.assertFalse(any("sudo" in " ".join(c["argv"]) for c in native.system.calls))

    def test_triple_local_roles_bind_both_peers_and_preserve_split_output_and_cleanup(self):
        self.plan = triple_fixture()
        self.install_plan()
        receipts = []
        for ordinal in (1, 2):
            peer = self.native(ordinal)
            auth_path = peer.account["home"] + "/.ssh/authorized_keys"
            self.fs.put(auth_path, b"# unrelated authorization\n")
            peer.prepare_peer(self.plan)
            daemon = (f"/usr/bin/orted -mca ess env -mca ess_base_jobid 123 -mca ess_base_vpid {ordinal} "
                      "-mca ess_base_num_procs 3 -mca orte_hnp_uri '123.0;tcp://10.50.0.1:4555'")
            stdout, stderr = io.StringIO(), io.StringIO()
            connection = f"192.0.2.1 44444 {peer.member['sshAddress']} {peer.member['sshPort']}"
            with patch.dict(os.environ, {"SSH_CONNECTION": connection, "SSH_ORIGINAL_COMMAND": daemon}), \
                 patch.object(port.sys, "stdout", stdout), patch.object(port.sys, "stderr", stderr):
                self.assertEqual(peer.internal("peer", [self.plan["operationId"], self.plan["planDigest"]] * 2), 0)
            self.assertEqual(stdout.getvalue(), "# mocked bounded collective\n")
            self.assertEqual(stderr.getvalue(), "UCX fixture warning on stderr\n")
            launched = next(c for c in peer.system.calls if c["argv"][0] == "/usr/bin/orted" and "separateStderr" in c)
            self.assertTrue(launched["separateStderr"])
            self.assertEqual(launched["argv"][launched["argv"].index("ess_base_vpid") + 1], str(ordinal))
            receipts.append(peer.cleanup(self.plan))
            self.assertEqual(self.fs.read(auth_path, port.MAX_OUTPUT, 1000), b"# unrelated authorization\n")
        owner = self.native(0)
        secret = bytearray(b"triple-private-fixture")
        receipts.append(owner.start_coordinator(self.plan, adapter.VolatileKey(secret)))
        self.assertEqual(secret, bytes(len(secret)))
        self.assertEqual(receipts[-1]["stderr"], "UCX fixture warning on stderr\n")
        self.assertTrue(all(row["cleanupConfirmed"] for row in receipts))
        aggregate = {k: self.plan[k] for k in ("operationId", "planDigest", "profileDigest")}
        aggregate.update(authorizationRemoved=True, agentGone=True, publicArtifactsRemoved=True,
                         nodes=[{k: row[k] for k in ("nodeId", "unitOwned", "unitProcessesGone", "rankCleanupConfirmed", "unitMetadataRemoved")} for row in receipts])
        self.assertTrue(adapter.cleanup_confirmed(self.plan, aggregate))
        aggregate["nodes"][1]["rankCleanupConfirmed"] = False
        self.assertFalse(adapter.cleanup_confirmed(self.plan, aggregate))

    def test_triple_third_runtime_projection_is_checked_on_every_local_node(self):
        self.plan = triple_fixture()
        self.install_plan()
        bad = copy.deepcopy(self.profile)
        bad["members"][2]["runtime"]["ncclLibrary"]["sha256"] = "f" * 64
        # Rebind all raw profile digests/leases: exercise the member comparison,
        # not merely a stale outer profile checksum.
        with patch(__name__ + ".profile_for", return_value=bad):
            self.install_plan()
        for index in range(3):
            native = self.native(index)
            with self.subTest(index=index), self.assertRaisesRegex(port.NativeError, "go_runtime_projection_changed"):
                native.bind(self.plan)
            self.assertEqual(native.system.calls, [])
            self.assertFalse(self.fs.exists(adapter.owned_paths(self.plan, self.plan["members"][index])["root"]))

    def test_triple_second_peer_rejects_cross_peer_ordinal_and_late_cancelled_callback(self):
        self.plan = triple_fixture()
        self.install_plan()
        peer = self.native(2)
        peer.prepare_peer(self.plan)
        command = "/usr/bin/orted -mca ess env -mca ess_base_jobid 123 -mca ess_base_vpid 1 -mca ess_base_num_procs 3 -mca orte_hnp_uri '123.0;tcp://10.50.0.1:4555'"
        args = [self.plan["operationId"], self.plan["planDigest"]] * 2
        with patch.dict(os.environ, {"SSH_CONNECTION": "192.0.2.1 44444 192.0.2.3 22", "SSH_ORIGINAL_COMMAND": command}):
            with self.assertRaisesRegex(adapter.ContractError, "mpi_daemon_identity_changed"):
                peer.internal("peer", args)
        self.assertEqual(peer.cancel(self.plan)["state"], "cancelled")
        self.assertTrue(peer.cleanup(self.plan)["cleanupConfirmed"])
        with patch.dict(os.environ, {"SSH_CONNECTION": "192.0.2.1 44444 192.0.2.3 22", "SSH_ORIGINAL_COMMAND": command.replace("ess_base_vpid 1", "ess_base_vpid 2")}):
            with self.assertRaisesRegex(port.NativeError, "operation_cancelled"):
                peer.internal("peer", args)
        self.assertFalse(any(c["argv"][0] in ("/usr/bin/systemd-run", "/usr/bin/orted") for c in peer.system.calls))

    def test_quick_worker_keeps_stderr_separate_through_result_status_and_cleanup(self):
        self.plan["recipeId"] = "pair-two-spark-nccl-socket-correctness-v2"
        self.install_plan()
        native = self.native(0)
        result = native.start_coordinator(self.plan, adapter.VolatileKey(bytearray(b"private-fixture")))
        self.assertEqual(result["exitCode"], 0, result)
        self.assertEqual(result["output"], "# mocked bounded collective\n")
        self.assertEqual(result["stderr"], "UCX fixture warning on stderr\n")
        self.assertTrue(result["cleanupConfirmed"])
        self.assertEqual(native.observe(self.plan)["stderr"], result["stderr"])
        self.assertEqual(native.worker_result()["stderr"], result["stderr"])
        spawned = [c for c in native.system.calls if "separateStderr" in c]
        self.assertTrue(next(c for c in spawned if c["argv"][0] == "/usr/bin/mpirun")["separateStderr"])
        self.assertTrue(all(not c["separateStderr"] for c in spawned if c["argv"][0] != "/usr/bin/mpirun"))
        self.assertNotIn("private-fixture", json.dumps(result))

    def test_system_wait_separates_streams_and_caps_the_combined_output(self):
        for outcome in ("success", "overflow", "timeout"):
            with self.subTest(outcome=outcome):
                system = port.System()
                clock = [100.0]
                class Pipe(io.BytesIO):
                    def __init__(self, fd): super().__init__(); self.fd = fd
                    def fileno(self): return self.fd
                proc = FakeProcess(9876, ["/usr/bin/mpirun"])
                proc.stdout, proc.stderr, proc.stdin = Pipe(23), Pipe(24), None
                chunks = {23: [b"valid stdout\n", b""], 24: [b"e" * port.MAX_OUTPUT if outcome == "overflow" else b"warning stderr\n", b""]}
                class Selector:
                    def __init__(self): self.streams = []
                    def register(self, stream, _event): self.streams.append(stream)
                    def unregister(self, stream): self.streams.remove(stream)
                    def get_map(self): return {s.fd: True for s in self.streams}
                    def select(self, _seconds):
                        if outcome == "timeout": clock[0] += 2
                        if all(not chunks[s.fd] for s in self.streams): proc.code = 0
                        return [(types.SimpleNamespace(fileobj=s), 1) for s in list(self.streams)]
                    def close(self): pass
                def read(fd, _size):
                    chunk = chunks[fd].pop(0)
                    if all(not values for values in chunks.values()): proc.code = 0
                    return chunk
                def killed(pid, sig):
                    self.assertEqual((pid, sig), (9876, 9)); proc.code = -9
                stderr = bytearray()
                with patch.object(port.selectors, "DefaultSelector", side_effect=Selector), patch.object(port.os, "set_blocking"), \
                     patch.object(port.os, "read", side_effect=read), patch.object(port.os, "killpg", side_effect=killed, create=True), \
                     patch.object(port.signal, "SIGKILL", 9, create=True), patch.object(port.time, "monotonic", side_effect=lambda: clock[0]):
                    if outcome != "success":
                        with self.assertRaises(port.NativeError) as caught:
                            system.wait(proc, 1, stderr=stderr)
                        self.assertEqual(caught.exception.args[0], "owned_output_limit" if outcome == "overflow" else "owned_process_timeout")
                        self.assertLessEqual(len(caught.exception.output) + len(caught.exception.stderr), port.MAX_OUTPUT)
                        self.assertEqual(caught.exception.output, b"valid stdout\n")
                        if outcome == "timeout":
                            self.assertEqual(caught.exception.stderr, b"warning stderr\n")
                    else:
                        self.assertEqual(system.wait(proc, 1, stderr=stderr), (0, b"valid stdout\n"))
                        self.assertEqual(stderr, b"warning stderr\n")
                self.assertTrue(proc.stdout.closed and proc.stderr.closed)

    def test_quick_worker_failure_preserves_command_streams_but_never_private_setup(self):
        for operation, fail_tool in (("b" * 32, "/usr/bin/mpirun"), ("c" * 32, "/usr/bin/ssh-add")):
            with self.subTest(tool=fail_tool):
                self.plan = fixture()
                self.plan.update(recipeId="pair-two-spark-nccl-socket-correctness-v2", operationId=operation)
                self.install_plan()
                native = self.native(0)
                original = native.system.wait
                def wait(proc, seconds, data=None, progress=None, deadline=None, stderr=None):
                    if proc.argv[0] == fail_tool:
                        raise port.NativeError("owned_process_timeout", b"retained stdout", b"retained stderr")
                    return original(proc, seconds, data, progress, deadline, stderr=stderr)
                with patch.object(native.system, "wait", side_effect=wait):
                    result = native.start_coordinator(self.plan, adapter.VolatileKey(bytearray(b"private-fixture")))
                self.assertEqual(result["exitCode"], 1)
                self.assertEqual(result["errorCode"], "owned_process_timeout")
                self.assertEqual(result["output"], "retained stdout" if fail_tool.endswith("mpirun") else "")
                self.assertEqual(result["stderr"], "retained stderr" if fail_tool.endswith("mpirun") else "")
                self.assertTrue(result["cleanupConfirmed"])

    def test_quick_peer_and_ssh_forward_stdout_and_stderr_to_distinct_channels(self):
        self.plan["recipeId"] = "pair-two-spark-nccl-socket-correctness-v2"
        self.install_plan()
        daemon = "/usr/bin/orted -mca ess env -mca ess_base_jobid 123 -mca ess_base_vpid 1 -mca ess_base_num_procs 2 -mca orte_hnp_uri '123.0;tcp://10.50.0.1:4555'"
        peer = self.native(1)
        peer.prepare_peer(self.plan)
        stdout, stderr = io.StringIO(), io.StringIO()
        with patch.dict(os.environ, {"SSH_CONNECTION": "192.0.2.1 44444 192.0.2.2 22", "SSH_ORIGINAL_COMMAND": daemon}), \
             patch.object(port.sys, "stdout", stdout), patch.object(port.sys, "stderr", stderr):
            self.assertEqual(peer.internal("peer", [self.plan["operationId"], self.plan["planDigest"]] * 2), 0)
        self.assertEqual(stdout.getvalue(), "# mocked bounded collective\n")
        self.assertEqual(stderr.getvalue(), "UCX fixture warning on stderr\n")
        self.assertTrue(next(c for c in peer.system.calls if c["argv"][0] == "/usr/bin/orted" and "separateStderr" in c)["separateStderr"])
        self.assertTrue(peer.cleanup(self.plan)["cleanupConfirmed"])
        owner = self.native(0)
        self.staged_unit(owner, "coordinator")
        for failing in (False, True):
            stdout, stderr = types.SimpleNamespace(buffer=io.BytesIO()), types.SimpleNamespace(buffer=io.BytesIO())
            original = owner.system.run
            def run(argv, env, seconds=5, data=None, progress=None, deadline=None, stderr=None):
                if argv[0] == "/usr/bin/ssh":
                    self.assertIsInstance(stderr, bytearray)
                    if failing:
                        raise port.NativeError("owned_process_timeout", b"ssh stdout", b"ssh stderr")
                    stderr.extend(b"ssh stderr")
                    return 0, b"ssh stdout"
                return original(argv, env, seconds, data, progress, deadline, stderr=stderr)
            with patch.object(owner.system, "run", side_effect=run), patch.object(port.sys, "stdout", stdout), patch.object(port.sys, "stderr", stderr):
                args = [self.plan["operationId"], self.plan["planDigest"], "10.50.0.2", daemon]
                if failing:
                    with self.assertRaisesRegex(port.NativeError, "owned_process_timeout"):
                        owner.internal("ssh", args)
                else:
                    self.assertEqual(owner.internal("ssh", args), 0)
            self.assertEqual(stdout.buffer.getvalue(), b"ssh stdout")
            self.assertEqual(stderr.buffer.getvalue(), b"ssh stderr")

    def test_quick_utf8_expansion_cannot_bypass_worker_json_output_cap(self):
        self.plan["recipeId"] = "pair-two-spark-nccl-socket-correctness-v2"
        self.install_plan()
        native = self.native(0)
        original = native.system.run
        def run(argv, env, seconds=5, data=None, progress=None, deadline=None, stderr=None):
            if argv[0] == "/usr/bin/mpirun":
                stderr.extend(b"\xff" * (port.MAX_OUTPUT // 3))
                return 0, b"\xff" * (port.MAX_OUTPUT // 3)
            return original(argv, env, seconds, data, progress, deadline, stderr=stderr)
        with patch.object(native.system, "run", side_effect=run):
            result = native.start_coordinator(self.plan, adapter.VolatileKey(bytearray(b"private-fixture")))
        self.assertEqual((result["exitCode"], result["errorCode"]), (1, "owned_output_limit"))
        self.assertEqual((result["output"], result["stderr"]), ("", ""))
        self.assertLessEqual(len(adapter.canonical(native.worker_result())), port.MAX_OUTPUT)

    def test_system_spawn_separate_stderr_is_opt_in(self):
        with patch.object(port.subprocess, "Popen", return_value=object()) as spawn:
            port.System().spawn(["/usr/bin/mpirun"], {})
            self.assertEqual(spawn.call_args.kwargs["stderr"], port.subprocess.STDOUT)
            port.System().spawn(["/usr/bin/mpirun"], {}, separate_stderr=True)
            self.assertEqual(spawn.call_args.kwargs["stderr"], port.subprocess.PIPE)

    def test_changed_tool_and_foreign_unit_are_not_executed_or_stopped(self):
        native = self.native(1)
        path = native.member["tools"]["systemctl"]["path"] if native.member else self.plan["members"][1]["tools"]["systemctl"]["path"]
        self.fs.put(path, b"replaced")
        with self.assertRaises(port.NativeError):
            native.prepare_peer(self.plan)
        self.assertEqual(native.system.calls, [])

    def test_internal_callbacks_require_owned_unit_and_exact_mpi_dialect(self):
        native = self.native(0)
        native.bind(self.plan)
        with native.lock():
            native.stage()
        with self.assertRaises(port.NativeError):
            native.internal("ssh", [self.plan["operationId"], self.plan["planDigest"], "10.50.0.2", "/bin/sh -c true"])
        peer = self.native(1)
        peer.prepare_peer(self.plan)
        with patch.dict(os.environ, {"SSH_CONNECTION": "192.0.2.1 44444 192.0.2.2 22", "SSH_ORIGINAL_COMMAND": "/bin/sh -c true"}), self.assertRaises(adapter.ContractError):
            peer.internal("peer", [self.plan["operationId"], self.plan["planDigest"]] * 2)
        self.assertFalse(any(c["argv"][0] == "/usr/bin/systemd-run" for c in peer.system.calls))

    def test_callback_failure_reason_is_static_and_unknown_text_is_redacted(self):
        native = self.native(0)
        for error, reason in (
            (adapter.ContractError("mpi_ssh_target_not_the_admitted_peer"), "target"),
            (adapter.ContractError("unsupported_mpi_daemon_command"), "daemon-grammar"),
            (adapter.ContractError("unsupported_mpi_node_map"), "node-map"),
            (port.NativeError("go_lease_binding_changed"), "lease-or-ownership"),
            (port.NativeError("unit_invocation_changed"), "lease-or-ownership"),
            (port.NativeError("unknown fixture: private argv URL password material"), "generic-failure"),
            (ValueError("unknown fixture: private argv URL password material"), "generic-failure"),
            (KeyError("unknown fixture: private argv URL password material"), "generic-failure"),
        ):
            with self.subTest(reason=reason):
                stderr = io.StringIO()
                with patch.object(port, "Native", return_value=native), \
                     patch.object(native, "internal", side_effect=error), \
                     patch.object(port.sys, "argv", ["fixture", "--internal-ssh", self.plan["operationId"], self.plan["planDigest"]]), \
                     patch.object(port.sys, "stderr", stderr), self.assertRaises(SystemExit) as caught:
                    port.main()
                self.assertEqual(caught.exception.code, 1)
                self.assertEqual(stderr.getvalue(), "PAIR MPI callback rejected: " + reason + "\n")

    def test_embedded_modules_work_without_file_paths(self):
        source = Path(port.__file__).read_bytes()
        module = types.ModuleType("native_embedded_fixture")
        module.SHIPPED_SOURCE = source
        exec(compile(source, "<embedded-native>", "exec"), module.__dict__)
        native = module.Native(FakeSystem(self.fs, self.plan["members"][0]), self.fs)
        native.bind(self.plan)
        self.assertEqual(native.source, source)

    def test_same_account_on_both_nodes_selects_the_public_identity(self):
        for member in self.plan["members"]:
            before = member["home"]
            member.update(user="eos", home="/home/eos")
            for value in [member[k] for k in ("manager", "binary", "ncclLibrary", "cudaLibrary", "mpiLibrary")]:
                value["path"] = value["path"].replace(before, member["home"])
        self.install_plan()
        peer = self.native(1)
        self.assertEqual(peer.bind(self.plan)["nodeId"], "spark2")
        self.assertEqual(peer.prepare_peer(self.plan)["nodeId"], "spark2")

    def test_root_owned_system_aliases_resolve_and_owned_files_stay_nofollow(self):
        native = self.native(0)
        native.bind(self.plan)
        for source, target in (("/usr/bin/python3", "python3.12"), ("/usr/bin/mpirun", "/etc/alternatives/mpirun"), ("/etc/alternatives/mpirun", "/usr/bin/mpirun.openmpi")):
            if not self.fs.exists(source):
                self.fs.put(source, b"alias")
            self.fs.special[source] = stat.S_IFLNK | 0o777
            self.fs.links[source] = target
        for name, target in (("python3", "/usr/bin/python3.12"), ("mpirun", "/usr/bin/mpirun.openmpi")):
            self.fs.put(target, ("/usr/bin/" + name + "\n").encode())
            native.verify_artifact(native.member["tools"][name])
        self.fs.owners["/etc/alternatives/mpirun"] = 1000
        with self.assertRaises(port.NativeError):
            native.verify_artifact(native.member["tools"]["mpirun"])
        self.fs.special[native.lease_dir + "/lease.json"] = stat.S_IFLNK | 0o777
        with self.assertRaises(port.NativeError):
            native.bind(self.plan)

    def staged_unit(self, native, role):
        native.bind(self.plan)
        with native.lock():
            journal = native.stage()
            journal.update(launchRequested=True, unitSeconds=100, unitArgv=native.unit_argv(role))
            journal["unitDeadlineMonotonic"] = native.system.monotonic() + 100
            native.save(journal)
            native.system.spawn(native.unit_command(journal), native.environment())

    def test_execstart_property_shape_and_pruned_exited_cgroup(self):
        native = self.native(0)
        self.staged_unit(native, "coordinator")
        self.assertEqual(native.unit()["MainPID"], str(os.getpid()))
        native.system.active.update(MainPID="0", ControlGroup="", SubState="exited")
        native.system.cgroup_state(False)
        self.assertEqual(native.unit()["ControlGroup"], "")
        self.assertTrue(native.collect_unit())

    def test_foreign_or_live_pruned_cgroup_never_gets_stopped(self):
        native = self.native(0)
        self.staged_unit(native, "coordinator")
        for change in ({"Description": "foreign"}, {"ControlGroup": "/foreign"}, {"ControlGroup": ""}):
            native.system.change_unit = change
            with self.assertRaises(port.NativeError):
                native.cancel(self.plan)
            self.assertFalse(any("stop" in c["argv"] for c in native.system.calls))

    def test_peer_entry_runs_only_fixed_daemon_inside_owned_unit(self):
        peer = self.native(1)
        peer.prepare_peer(self.plan)
        daemon = "/usr/bin/orted -mca ess env -mca ess_base_jobid 123 -mca ess_base_vpid 1 -mca ess_base_num_procs 2 -mca orte_hnp_uri '123.0;tcp://10.50.0.1:4555'"
        with patch.dict(os.environ, {"SSH_CONNECTION": "192.0.2.1 44444 192.0.2.2 22", "SSH_ORIGINAL_COMMAND": daemon}), patch.object(port.sys, "stdout", io.StringIO()):
            code = peer.internal("peer", [self.plan["operationId"], self.plan["planDigest"]] * 2)
        self.assertEqual(code, 0)
        self.assertTrue(any(c["argv"][0] == "/usr/bin/orted" for c in peer.system.calls))
        self.assertTrue(peer.cleanup(self.plan)["cleanupConfirmed"])

    def test_rank_entry_executes_existing_go_lease_wrapper_with_fixed_environment(self):
        native = self.native(0)
        self.staged_unit(native, "coordinator")
        class ExecObserved(Exception):
            pass
        with patch.dict(os.environ, {"OMPI_COMM_WORLD_RANK": "0", "NCCL_IB_DISABLE": "0", "LD_LIBRARY_PATH": "/foreign"}), patch.object(port.os, "execve", side_effect=ExecObserved) as execute:
            with self.assertRaises(ExecObserved):
                native.internal("rank", [self.plan["operationId"], self.plan["planDigest"], self.plan["groupId"], self.plan["operationId"]])
        binary, argv, environment = execute.call_args.args
        self.assertEqual(binary, native.member["manager"]["path"])
        self.assertEqual(argv[1:], ["--diagnostic-rank", self.plan["groupId"], self.plan["operationId"]])
        self.assertEqual(environment["NCCL_IB_DISABLE"], "1")
        self.assertEqual(environment["NCCL_NET"], "Socket")
        self.assertNotIn("/foreign", environment["LD_LIBRARY_PATH"])
        self.assertEqual(environment["OMPI_COMM_WORLD_RANK"], "0")

    def test_status_preserves_nonzero_worker_failure(self):
        native = self.native(0)
        native.bind(self.plan)
        with native.lock():
            native.stage()
        result = {"operationId": self.plan["operationId"], "planDigest": self.plan["planDigest"], "nodeId": native.member["nodeId"],
                  "done": True, "exitCode": 1, "output": "", "errorCode": "fixed_worker_failure"}
        self.fs.put(native.paths["root"] + "/worker-result.json", adapter.canonical(result))
        observed = native.observe(self.plan)
        self.assertEqual(observed["state"], "failed")
        self.assertEqual(observed["errorCode"], "fixed_worker_failure")
        self.assertEqual(observed["exitCode"], 1)

    def test_expiry_during_worker_verification_cannot_spawn_or_load_agent(self):
        native = self.native(0)
        self.staged_unit(native, "coordinator")
        def expired(*_args, **_kwargs):
            native.system.clock = self.plan["expiresAt"] + 1
        stream = types.SimpleNamespace(buffer=io.BytesIO(b"private-fixture"))
        with patch.dict(os.environ, {"INVOCATION_ID": "e" * 32}), patch.object(port.sys, "stdin", stream), patch.object(native, "verify_inputs", side_effect=expired):
            native.worker("coordinator")
        self.assertFalse(any(c["argv"][0] in ("/usr/bin/ssh-agent", "/usr/bin/ssh-add", "/usr/bin/mpirun") for c in native.system.calls))

    def test_cancellation_before_key_load_does_not_pass_private_stdin(self):
        native = self.native(0)
        self.staged_unit(native, "coordinator")
        original_spawn = native.system.spawn
        def cancelled_after_agent(argv, env, input_pipe=False):
            proc = original_spawn(argv, env, input_pipe)
            if argv[0] == "/usr/bin/ssh-agent":
                self.fs.put(native.lease_dir + "/cancelled", b"cancelled\n")
            return proc
        stream = types.SimpleNamespace(buffer=io.BytesIO(b"private-fixture"))
        with patch.dict(os.environ, {"INVOCATION_ID": "e" * 32}), patch.object(port.sys, "stdin", stream), patch.object(native.system, "spawn", side_effect=cancelled_after_agent):
            native.worker("coordinator")
        self.assertIsNone(native.system.key_load_length)
        self.assertFalse(any(c["argv"][0] == "/usr/bin/mpirun" for c in native.system.calls))

    def test_aged_lease_and_delayed_startup_leave_result_reserve_on_both_roles(self):
        # Root's retained trial had only 77.634s left at approval. These are
        # mocked clocks/processes; no native timing qualification is claimed.
        self.plan.update(createdAt=NOW - 42366, expiresAt=NOW + 77634)
        for index, role in ((0, "coordinator"), (1, "peer")):
            with self.subTest(role=role):
                self.fs = SandboxFiles(Path(self.temp.name) / role)
                self.install_plan()
                native = self.native(index)
                native.bind(self.plan)
                original_wait, original_spawn = native.system.wait, native.system.spawn
                original_atomic = self.fs.atomic
                hard_deadline, writes, child_waits = [], [], []

                def spawn(argv, env, input_pipe=False):
                    if argv[0] == "/usr/bin/systemd-run":
                        hard_deadline.append(native.system.monotonic() + native.journal()["unitSeconds"])
                    proc = original_spawn(argv, env, input_pipe)
                    if argv[0] in ("/usr/bin/mpirun", "/usr/bin/orted"):
                        native.system.advance(2)  # Spawn time must consume the same budget.
                    return proc

                def wait(proc, seconds, data=None, progress=None, deadline=None):
                    if proc.argv[0] == "/usr/bin/systemd-run":
                        native.system.advance(5)  # Unit/bootstrap startup delay.
                        return original_wait(proc, seconds, data, progress, deadline)
                    if proc.argv[0] in ("/usr/bin/mpirun", "/usr/bin/orted"):
                        effective = min(seconds, deadline - native.system.monotonic()) if deadline is not None else seconds
                        child_waits.append((seconds, deadline, effective))
                        native.system.advance(max(0, effective) + 2)  # Bounded exact-child reap.
                        proc.code = -9
                        error = port.NativeError("owned_process_timeout")
                        error.output = b"# retained partial collective output\n"
                        raise error
                    return original_wait(proc, seconds, data, progress, deadline)

                def atomic(path, data, uid, expected=None, mode=0o600):
                    if path.endswith("/worker-result.json"):
                        native.system.advance(5)  # Receipt lock/write fixture delay.
                        writes.append(native.system.monotonic())
                        self.assertLess(writes[-1], hard_deadline[0], "unit deadline preempted the result writer")
                    return original_atomic(path, data, uid, expected, mode)

                def terminate(proc):
                    native.system.advance(4)  # Existing agent termination upper bound.
                    proc.code = 0

                daemon = adapter.parse_daemon_command('/usr/bin/orted -mca ess env -mca ess_base_jobid 1234 -mca ess_base_vpid 1 -mca ess_base_num_procs 2 -mca orte_hnp_uri "1234.0;tcp://10.50.0.1:45000"', self.plan, self.plan["members"][1])
                key = adapter.VolatileKey(bytearray(b"private-fixture")) if role == "coordinator" else None
                with patch.object(native.system, "spawn", side_effect=spawn), patch.object(native.system, "wait", side_effect=wait), \
                     patch.object(native.system, "terminate", side_effect=terminate), patch.object(self.fs, "atomic", side_effect=atomic):
                    result = native.launch_unit(role, key, daemon if role == "peer" else None)
                self.assertEqual((result["exitCode"], result["errorCode"]), (1, "owned_process_timeout"))
                self.assertIn("retained partial", result["output"])
                self.assertTrue(writes)
                self.assertTrue(all(deadline is not None and 0 < seconds <= 90 for seconds, deadline, _ in child_waits))
                if role == "coordinator":
                    mpi = next(c["argv"] for c in native.system.calls if c["argv"][0] == "/usr/bin/mpirun")
                    self.assertLessEqual(int(mpi[mpi.index("--timeout") + 1]), 90)
                    self.assertEqual(key._value, bytearray(len(key._value)))

    def test_exhausted_reserve_refuses_new_unit_and_child_launches(self):
        native = self.native(0)
        native.bind(self.plan)
        original_save = native.save
        def slow_save(value):
            original_save(value)
            if value.get("launchRequested"):
                native.system.advance(100)
        with patch.object(native, "save", side_effect=slow_save), self.assertRaises(port.NativeError):
            native.launch_unit("coordinator", adapter.VolatileKey(bytearray(b"private-fixture")))
        self.assertFalse(any(c["argv"][0] == "/usr/bin/systemd-run" for c in native.system.calls))

    def test_worker_verification_failure_and_exhaustion_still_publish_bound_result(self):
        for kind in ("verification", "reserve", "wall-expiry"):
            with self.subTest(kind=kind):
                self.plan = fixture()
                self.install_plan()
                native = self.native(0)
                self.staged_unit(native, "coordinator")
                def failed(*_args, **_kwargs):
                    if kind == "verification":
                        raise port.NativeError("artifact_bytes_changed")
                    if kind == "reserve":
                        native.system.advance(90)
                    else:
                        native.system.clock = self.plan["expiresAt"] - 1000
                stream = types.SimpleNamespace(buffer=io.BytesIO(b"private-fixture"))
                with patch.dict(os.environ, {"INVOCATION_ID": "e" * 32}), patch.object(port.sys, "stdin", stream), \
                     patch.object(native, "verify_inputs", side_effect=failed):
                    self.assertEqual(native.worker("coordinator"), 1)
                result = native.worker_result()
                self.assertTrue(result["done"])
                self.assertIsNotNone(result["errorCode"])
                self.assertFalse(any(c["argv"][0] in ("/usr/bin/ssh-agent", "/usr/bin/ssh-add", "/usr/bin/mpirun") for c in native.system.calls))

    def test_old_clean_journal_without_timing_remains_readable_and_cannot_relaunch(self):
        native = self.native(0)
        result = native.start_coordinator(self.plan, adapter.VolatileKey(bytearray(b"private-fixture")))
        self.assertTrue(result["cleanupConfirmed"])
        journal = native.journal()
        journal.pop("unitDeadlineMonotonic", None)
        native.save(journal)
        self.assertTrue(native.observe(self.plan)["cleanupConfirmed"])
        self.assertTrue(native.cleanup(self.plan)["cleanupConfirmed"])
        before = len([c for c in native.system.calls if c["argv"][0] == "/usr/bin/systemd-run"])
        with self.assertRaises((port.NativeError, adapter.ContractError)):
            native.launch_unit("coordinator", adapter.VolatileKey(bytearray(b"private-fixture")))
        self.assertEqual(len([c for c in native.system.calls if c["argv"][0] == "/usr/bin/systemd-run"]), before)

    def test_system_wait_timeout_retains_output_and_reaps_only_its_child(self):
        system = port.System()
        clock = [100.0]
        class Pipe(io.BytesIO):
            def fileno(self): return 23
        proc = FakeProcess(9876, ["/usr/bin/orted"])
        proc.stdout, proc.stdin = Pipe(), None
        class Selector:
            def register(self, *_args): pass
            def unregister(self, *_args): pass
            def get_map(self): return {23: True}
            def select(self, _seconds):
                clock[0] += 1
                return [(types.SimpleNamespace(fileobj=proc.stdout), 1)]
            def close(self): pass
        def killed(pid, sig):
            self.assertEqual((pid, sig), (9876, port.signal.SIGKILL))
            proc.code = -9
        with patch.object(port.time, "monotonic", side_effect=lambda: clock[0]), patch.object(port.selectors, "DefaultSelector", return_value=Selector()), \
             patch.object(port.os, "set_blocking"), patch.object(port.os, "read", return_value=b"partial output\n"), \
             patch.object(port.signal, "SIGKILL", 9, create=True), \
             patch.object(port.os, "killpg", side_effect=killed, create=True), self.assertRaises(port.NativeError) as caught:
            system.wait(proc, 1)
        self.assertEqual(caught.exception.args[0], "owned_process_timeout")
        self.assertEqual(getattr(caught.exception, "output", None), b"partial output\n")
        self.assertEqual(proc.code, -9)

    def test_worker_budget_boundary_and_invalid_deadlines_fail_closed(self):
        native = self.native(0)
        native.bind(self.plan)
        now = native.system.monotonic()
        journal = {"unitSeconds": 100, "unitDeadlineMonotonic": now + 13,
                   "executionDeadlineAt": min(NOW + 90000, self.plan["expiresAt"])}
        self.assertEqual(native.worker_budget(journal)[0], 1)
        for deadline in (None, True, float("nan"), float("inf"), -1, now + 101, now + 12.999):
            with self.subTest(deadline=deadline), self.assertRaises(port.NativeError):
                native.worker_budget({**journal, "unitDeadlineMonotonic": deadline})
        native.system.clock = self.plan["expiresAt"] - 12999
        with self.assertRaises(port.NativeError):
            native.worker_budget(journal)

    def test_absolute_wait_cutoff_exhausted_during_spawn_still_reaps_child(self):
        system = port.System()
        class Pipe(io.BytesIO):
            def fileno(self): return 23
        proc = FakeProcess(9876, ["/usr/bin/orted"])
        proc.stdout, proc.stdin = Pipe(), None
        selector = unittest.mock.MagicMock()
        selector.get_map.return_value = {23: True}
        def killed(pid, sig):
            self.assertEqual((pid, sig), (9876, 9))
            proc.code = -9
        with patch.object(port.time, "monotonic", return_value=102.0), \
             patch.object(port.selectors, "DefaultSelector", return_value=selector), \
             patch.object(port.os, "set_blocking"), patch.object(port.signal, "SIGKILL", 9, create=True), \
             patch.object(port.os, "killpg", side_effect=killed, create=True), self.assertRaises(port.NativeError) as caught:
            system.wait(proc, 90, deadline=101.0)
        self.assertEqual(caught.exception.args[0], "owned_process_timeout")
        self.assertEqual(proc.code, -9)
        selector.select.assert_not_called()

    def test_owned_startup_validation_consumes_one_absolute_budget_before_result_authority(self):
        for unit_seconds in (14, 23, 30):
            with self.subTest(unit_seconds=unit_seconds):
                self.fs = SandboxFiles(Path(self.temp.name) / str(unit_seconds))
                self.plan = fixture()
                self.plan.update(expiresAt=NOW + (unit_seconds + 10) * 1000,
                                 createdAt=NOW + (unit_seconds + 10) * 1000 - 120000)
                self.install_plan()
                native = self.native(0)
                self.staged_unit(native, "coordinator")
                journal = native.journal()
                hard_end = native.system.monotonic() + unit_seconds
                journal.update(unitSeconds=unit_seconds, unitDeadlineMonotonic=hard_end)
                native.save(journal)
                native.system.active["RuntimeMaxUSec"] = str(unit_seconds) + "s"
                original_hash, original_run = native.verify_artifact, native.system.run
                original_lock = self.fs.lock
                deadlines = []

                def consume(duration, seconds, deadline):
                    deadlines.append(deadline)
                    allowed = min(duration, seconds, deadline - native.system.monotonic()) if deadline is not None else min(duration, seconds)
                    native.system.advance(max(0, allowed))
                    if allowed < duration or deadline is not None and native.system.monotonic() >= deadline:
                        raise port.NativeError("insufficient_worker_budget")

                def slow_valid_hash(value, seconds=20, deadline=None):
                    consume(6, seconds, deadline)
                    if deadline is None:
                        return original_hash(value, seconds)
                    return original_hash(value, seconds, deadline=deadline)

                def slow_valid_read(argv, env, seconds=5, data=None, progress=None, deadline=None):
                    if argv[0] in ("/usr/bin/systemctl", "/usr/bin/busctl"):
                        consume(1, seconds, deadline)
                    return original_run(argv, env, seconds, data, progress, deadline)

                @contextlib.contextmanager
                def delayed_lock(path, uid, deadline=None):
                    if unit_seconds == 30:
                        consume(5, 5, deadline)
                    with original_lock(path, uid):
                        yield

                stream = types.SimpleNamespace(buffer=io.BytesIO(b"private-fixture"))
                with patch.dict(os.environ, {"INVOCATION_ID": "e" * 32}), patch.object(port.sys, "stdin", stream), \
                     patch.object(native, "verify_artifact", side_effect=slow_valid_hash), \
                     patch.object(native.system, "run", side_effect=slow_valid_read), \
                     patch.object(self.fs, "lock", side_effect=delayed_lock), self.assertRaises(port.NativeError):
                    native.worker("coordinator")
                self.assertLessEqual(native.system.monotonic(), hard_end - 12)
                self.assertTrue(deadlines and all(value == hard_end - 12 for value in deadlines))
                self.assertIsNone(native.worker_result(), "unverified ownership must not mint a bound result")
                self.assertFalse(any(c["argv"][0] in ("/usr/bin/ssh-agent", "/usr/bin/ssh-add", "/usr/bin/mpirun", "/usr/bin/orted") for c in native.system.calls))

    def test_go_execution_deadline_precedes_fresh_unit_and_lease_deadlines(self):
        self.plan.update(createdAt=NOW, expiresAt=NOW + 120000)
        self.install_plan()
        native = self.native(0)
        native.bind(self.plan)
        original_wait = native.system.wait
        original_atomic = self.fs.atomic
        origin = native.system.monotonic()
        writes = []
        def wait(proc, seconds, data=None, progress=None, deadline=None):
            if proc.argv[0] == "/usr/bin/systemd-run":
                native.system.advance(5)
                return original_wait(proc, seconds, data, progress, deadline)
            if proc.argv[0] == "/usr/bin/mpirun":
                self.assertLessEqual(deadline, origin + 78)
                native.system.advance(deadline - native.system.monotonic() + 2)
                proc.code = -9
                raise port.NativeError("owned_process_timeout", b"parent deadline fixture\n")
            return original_wait(proc, seconds, data, progress, deadline)
        def atomic(path, data, uid, expected=None, mode=0o600):
            if path.endswith("/worker-result.json"):
                native.system.advance(5)
                writes.append(native.system.monotonic())
            return original_atomic(path, data, uid, expected, mode)
        def terminate(proc):
            native.system.advance(4)
            proc.code = 0
        with patch.object(native.system, "wait", side_effect=wait), patch.object(native.system, "terminate", side_effect=terminate), \
             patch.object(self.fs, "atomic", side_effect=atomic):
            result = native.launch_unit("coordinator", adapter.VolatileKey(bytearray(b"private-fixture")))
        journal = native.journal()
        self.assertEqual(journal["unitSeconds"], 105)
        self.assertEqual(journal["executionDeadlineAt"], NOW + 90000)
        self.assertLess(writes[0], origin + 90)
        self.assertEqual(result["errorCode"], "owned_process_timeout")
        self.assertIn("parent deadline fixture", result["output"])

    def test_execution_deadline_cannot_change_or_disappear_after_staging(self):
        native = self.native(0)
        native.bind(self.plan)
        with native.lock():
            native.stage()
        path = native.lease_dir + "/lease.json"
        original = self.fs.read(path, port.MAX_JSON, 1000)
        for change in ("extend", "shorten", "remove"):
            with self.subTest(change=change):
                lease = json.loads(original)
                if change == "remove":
                    lease["request"].pop("executionDeadlineAt")
                else:
                    lease["request"]["executionDeadlineAt"] += 1000 if change == "extend" else -1000
                self.fs.put(path, adapter.canonical(lease))
                fresh = self.native(0)
                with self.assertRaises(port.NativeError):
                    fresh.bind(self.plan, live=False)
                self.assertEqual(fresh.system.calls, [])
                self.fs.put(path, original)

    def test_execution_deadline_validation_and_legacy_cleanup_only(self):
        native = self.native(0)
        native.bind(self.plan)
        path = native.lease_dir + "/lease.json"
        original = self.fs.read(path, port.MAX_JSON, 1000)
        for value in (None, True, 0, 1.5, "90", self.plan["createdAt"], self.plan["expiresAt"] + 1, NOW):
            with self.subTest(value=value):
                lease = json.loads(original)
                lease["request"]["executionDeadlineAt"] = value
                self.fs.put(path, adapter.canonical(lease))
                fresh = self.native(0)
                with self.assertRaises(port.NativeError):
                    fresh.bind(self.plan)
                self.assertEqual(fresh.system.calls, [])
        self.fs.put(path, original)
        result = native.start_coordinator(self.plan, adapter.VolatileKey(bytearray(b"private-fixture")))
        self.assertTrue(result["cleanupConfirmed"])
        lease = json.loads(original)
        lease["request"].pop("executionDeadlineAt")
        self.fs.put(path, adapter.canonical(lease))
        journal = native.journal()
        journal.pop("executionDeadlineAt")
        native.save(journal)
        legacy = self.native(0)
        self.assertTrue(legacy.observe(self.plan)["cleanupConfirmed"])
        self.assertTrue(legacy.cleanup(self.plan)["cleanupConfirmed"])
        with self.assertRaises(port.NativeError):
            legacy.bind(self.plan)
        self.assertFalse(any(c["argv"][0] == "/usr/bin/systemd-run" for c in legacy.system.calls))


if __name__ == "__main__":
    unittest.main()
