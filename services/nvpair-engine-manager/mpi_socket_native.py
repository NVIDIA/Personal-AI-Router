# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Lease-bound Linux port for the fixed two-member MPI compiler.

Public input selects an action and retained public plan, never executable text.
Internal SSH callbacks are generated files behind one expiring forced key entry.
No native action is performed by importing this module.
"""

import contextlib
import ctypes
import hashlib
import importlib.util
import json
import math
import os
from pathlib import Path, PurePosixPath
import platform
import posixpath
import re
import selectors
import shlex
import signal
import stat
import subprocess
import sys
import time
import types

ADAPTER_SHA256 = "70a1113f78f2bce0e89d852e6f62db6d6b8abd1d3ead215ded9d1a8a533c14d3"
OWNER = "pair-nccl-socket-native-v1"
MAX_JSON = 128 * 1024
MAX_OUTPUT = 1024 * 1024
# Inside the existing unit cap: child reap (2s), agent stop (4s), result lock
# (5s), and write slack. Filesystem/OS stalls are not a hard real-time promise.
RESULT_RESERVE_SECONDS = 12
SHIPPED_SOURCE = globals().get("SHIPPED_SOURCE")
SYSTEM_TOOL_ARTIFACT_DRIFT = frozenset(("artifact_size_changed", "artifact_identity_changed", "artifact_bytes_changed"))
PUBLIC_ARTIFACTS = frozenset(("mpi_socket_native.py", "mpi_socket_adapter.py", "identity.pub", "known_hosts", "mpi.app", "ssh_adapter", "peer_entry", "rank_entry"))


def _adapter():
    module = sys.modules.get("mpi_socket_adapter")
    if module is not None:
        source = getattr(module, "SHIPPED_SOURCE", None)
        if source is None:
            source = Path(module.__file__).read_bytes()
    else:
        source = Path(__file__).with_name("mpi_socket_adapter.py").read_bytes()
    if type(source) is not bytes or hashlib.sha256(source).hexdigest() != ADAPTER_SHA256:
        raise RuntimeError("shipped MPI compiler changed")
    if module is None:
        module = types.ModuleType("mpi_socket_adapter")
        exec(compile(source, "<fixed-mpi-compiler>", "exec"), module.__dict__)
        sys.modules["mpi_socket_adapter"] = module
    module.SHIPPED_SOURCE = source
    return module


adapter = _adapter()


class NativeError(Exception):
    def __init__(self, code, output=b"", stderr=b""):
        super().__init__(code)
        self.output = bytes(output[:MAX_OUTPUT])
        self.stderr = bytes(stderr[:MAX_OUTPUT - len(self.output)])


def callback_failure_reason(error):
    # Only fixed categories may leave the callback; exception text can contain
    # generated commands or other private input and must never be echoed.
    code = error.args[0] if isinstance(error, (NativeError, adapter.ContractError)) and error.args else None
    reasons = {
        "mpi_ssh_target_not_the_admitted_peer": "target",
        "unsupported_mpi_node_map": "node-map",
        "unsupported_mpi_daemon_command": "daemon-grammar",
        "mpi_daemon_policy_changed": "daemon-grammar",
        "mpi_daemon_identity_changed": "daemon-grammar",
        "mpi_coordinator_contact_changed": "daemon-grammar",
        "operation_cancelled": "lease-or-ownership",
        "insufficient_remaining_lease": "lease-or-ownership",
        "go_lease_binding_changed": "lease-or-ownership",
        "admitted_plan_or_profile_changed": "lease-or-ownership",
        "local_account_not_admitted": "lease-or-ownership",
        "foreign_or_changed_unit": "lease-or-ownership",
        "unit_invocation_changed": "lease-or-ownership",
        "ssh_callback_not_in_owned_unit": "lease-or-ownership",
        "rank_not_in_owned_unit": "lease-or-ownership",
        "ssh_peer_connection_changed": "lease-or-ownership",
    }
    return reasons.get(code, "generic-failure") if type(code) is str else "generic-failure"


def strict_json(raw):
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise NativeError("duplicate_json_field")
            result[key] = value
        return result
    return json.loads(raw, object_pairs_hook=pairs)


def fingerprint(raw):
    return hashlib.sha256(raw).hexdigest()


class Files:
    """Logical paths are fixed by the passwd account and the admitted plan."""
    def path(self, path):
        return Path(path)

    def info(self, path):
        return self.path(path).lstat()

    def exists(self, path):
        try:
            self.info(path)
            return True
        except FileNotFoundError:
            return False

    def link_target(self, path):
        return os.readlink(self.path(path))

    def system_tool(self, path):
        if path not in adapter.SYSTEM_TOOLS.values():
            raise NativeError("unsupported_system_tool_path")
        current = path
        for _ in range(20):
            parts = PurePosixPath(current).parts
            resolved, changed = "/", False
            for position, component in enumerate(parts[1:], 1):
                resolved = posixpath.join(resolved, component)
                info = self.info(resolved)
                if info.st_uid != 0:
                    raise NativeError("system_tool_owner_changed")
                if stat.S_ISLNK(info.st_mode):
                    target = self.link_target(resolved)
                    if not target.startswith("/"):
                        target = posixpath.join(posixpath.dirname(resolved), target)
                    current = posixpath.normpath(posixpath.join(target, *parts[position + 1:]))
                    changed = True
                    break
                if info.st_mode & 0o022 or position < len(parts) - 1 and not stat.S_ISDIR(info.st_mode):
                    raise NativeError("system_tool_path_changed")
            if not changed:
                self.trusted(current, 0, system=True)
                return current
        raise NativeError("system_tool_link_limit")

    def trusted(self, path, uid, directory=False, private=False, system=False):
        logical = PurePosixPath(path)
        if not logical.is_absolute() or ".." in logical.parts:
            raise NativeError("unsafe_owned_path")
        chain = list(reversed(logical.parents)) + [logical]
        for part in chain:
            info = self.info(str(part))
            final = part == logical
            if stat.S_ISLNK(info.st_mode) or (not final or directory) and not stat.S_ISDIR(info.st_mode):
                raise NativeError("unsafe_path_component")
            if info.st_uid not in ({0} if system else {0, uid}) or info.st_mode & 0o022:
                raise NativeError("path_ownership_changed")
            if final and not directory and not stat.S_ISREG(info.st_mode):
                raise NativeError("owned_file_not_regular")
            if final and private and (info.st_uid != uid or info.st_mode & 0o077):
                raise NativeError("private_file_permissions_changed")
        return self.info(path)

    def read(self, path, maximum, uid, private=False):
        before = self.trusted(path, uid, private=private)
        if before.st_size < 0 or before.st_size > maximum:
            raise NativeError("file_size_limit")
        fd = os.open(self.path(path), os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        try:
            after = os.fstat(fd)
            if (after.st_dev, after.st_ino) != (before.st_dev, before.st_ino):
                raise NativeError("file_changed_during_read")
            with os.fdopen(fd, "rb", closefd=False) as stream:
                raw = stream.read(maximum + 1)
            if len(raw) > maximum:
                raise NativeError("file_size_limit")
            return raw
        finally:
            os.close(fd)

    def mkdir(self, path, uid):
        if not self.exists(path):
            parent = str(PurePosixPath(path).parent)
            self.trusted(parent, uid, directory=True)
            self.path(path).mkdir(mode=0o700)
        self.trusted(path, uid, directory=True, private=True)

    def sync_dir(self, path):
        fd = os.open(self.path(path), os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(fd)
        finally:
            os.close(fd)

    def atomic(self, path, data, uid, expected=None, mode=0o600):
        self.trusted(str(PurePosixPath(path).parent), uid, directory=True)
        current = self.read(path, MAX_OUTPUT, uid) if self.exists(path) else None
        if current != expected:
            raise NativeError("file_compare_and_swap_failed")
        temp = path + ".new"
        fd = os.open(self.path(temp), os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), mode)
        try:
            with os.fdopen(fd, "wb", closefd=False) as stream:
                stream.write(data)
                stream.flush()
                os.fsync(fd)
            os.close(fd)
            fd = None
            if (self.read(path, MAX_OUTPUT, uid) if self.exists(path) else None) != expected:
                raise NativeError("file_compare_and_swap_failed")
            os.replace(self.path(temp), self.path(path))
            self.sync_dir(str(PurePosixPath(path).parent))
            if self.read(path, MAX_OUTPUT, uid) != data:
                raise NativeError("file_readback_changed")
        finally:
            if fd is not None:
                os.close(fd)
            if self.exists(temp):
                self.path(temp).unlink()

    def remove(self, path, expected_hash, uid):
        if not self.exists(path):
            return
        if fingerprint(self.read(path, MAX_OUTPUT, uid)) != expected_hash:
            raise NativeError("owned_public_file_changed")
        self.path(path).unlink()
        self.sync_dir(str(PurePosixPath(path).parent))

    @contextlib.contextmanager
    def lock(self, path, uid, deadline=None):
        import fcntl
        self.trusted(str(PurePosixPath(path).parent), uid, directory=True)
        fd = os.open(self.path(path), os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0), 0o600)
        try:
            info = os.fstat(fd)
            if not stat.S_ISREG(info.st_mode) or info.st_uid != uid or info.st_mode & 0o077 or info.st_nlink != 1:
                raise NativeError("lock_ownership_changed")
            until = time.monotonic() + 5
            if deadline is not None:
                until = min(until, deadline)
            while True:
                try:
                    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    break
                except BlockingIOError:
                    if time.monotonic() >= until:
                        raise NativeError("owned_lock_busy") from None
                    time.sleep(0.02)
            yield
        finally:
            os.close(fd)


def parent_death(parent):
    def apply():
        libc = ctypes.CDLL(None, use_errno=True)
        if libc.prctl(1, signal.SIGKILL) != 0 or os.getppid() != parent:
            os._exit(125)
    return apply


class System:
    def interfaces(self):
        import socket
        class Sockaddr(ctypes.Structure):
            _fields_ = [("family", ctypes.c_ushort), ("data", ctypes.c_byte * 14)]
        class Address(ctypes.Structure):
            pass
        Address._fields_ = [("next", ctypes.POINTER(Address)), ("name", ctypes.c_char_p),
                            ("flags", ctypes.c_uint), ("address", ctypes.POINTER(Sockaddr)),
                            ("netmask", ctypes.POINTER(Sockaddr)), ("broadcast", ctypes.c_void_p), ("data", ctypes.c_void_p)]
        first = ctypes.POINTER(Address)()
        libc = ctypes.CDLL(None, use_errno=True)
        if libc.getifaddrs(ctypes.byref(first)):
            raise NativeError("interface_observation_failed")
        rows, pointer, count = {}, first, 0
        try:
            while pointer:
                count += 1
                if count > 512:
                    raise NativeError("interface_observation_limit")
                row = pointer.contents
                name = row.name.decode("ascii")
                if row.address and row.netmask and row.address.contents.family == socket.AF_INET:
                    address = socket.inet_ntoa(ctypes.string_at(ctypes.addressof(row.address.contents) + 4, 4))
                    mask = int.from_bytes(ctypes.string_at(ctypes.addressof(row.netmask.contents) + 4, 4), "big")
                    binary = f"{mask:032b}"
                    if "01" in binary:
                        raise NativeError("interface_netmask_invalid")
                    entry = rows.setdefault(name, {"name": name, "up": bool(row.flags & 1), "addresses": []})
                    entry["addresses"].append(address + "/" + str(binary.count("1")))
                pointer = row.next
        finally:
            libc.freeifaddrs(first)
        return list(rows.values())

    def interface_identity(self, name):
        import socket
        try:
            index = socket.if_nametoindex(name)
            mac = (Path("/sys/class/net") / name / "address").read_text().strip().lower()
        except OSError:
            raise NativeError("fabric_interface_unavailable") from None
        return {"index": index, "mac": mac}

    def account(self):
        import pwd
        if platform.system() != "Linux" or platform.machine() != "aarch64" or os.getuid() == 0:
            raise NativeError("native_linux_arm64_account_required")
        row = pwd.getpwuid(os.getuid())
        if os.environ.get("XDG_CONFIG_HOME", row.pw_dir + "/.config") != row.pw_dir + "/.config":
            raise NativeError("nonstandard_pair_base_not_supported")
        return {"uid": row.pw_uid, "user": row.pw_name, "home": row.pw_dir}

    def now(self):
        return int(time.time() * 1000)

    def monotonic(self):
        return time.monotonic()

    def spawn(self, argv, env, input_pipe=False, separate_stderr=False):
        return subprocess.Popen(argv, stdin=subprocess.PIPE if input_pipe else subprocess.DEVNULL,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE if separate_stderr else subprocess.STDOUT, env=env,
                                start_new_session=True, preexec_fn=parent_death(os.getpid()))

    def wait(self, proc, seconds, data=None, progress=None, deadline=None, stderr=None):
        end = self.monotonic() + seconds
        if deadline is not None:
            end = min(end, deadline)
        output, offset = bytearray(), 0
        error_output = stderr if stderr is not None else bytearray()
        selector = None
        try:
            if type(error_output) is not bytearray or error_output:
                raise NativeError("invalid_stderr_buffer")
            selector = selectors.DefaultSelector()
            os.set_blocking(proc.stdout.fileno(), False)
            selector.register(proc.stdout, selectors.EVENT_READ)
            if stderr is not None:
                os.set_blocking(proc.stderr.fileno(), False)
                selector.register(proc.stderr, selectors.EVENT_READ)
            if proc.stdin is not None:
                os.set_blocking(proc.stdin.fileno(), False)
                if data:
                    selector.register(proc.stdin, selectors.EVENT_WRITE)
                else:
                    proc.stdin.close()
            while proc.poll() is None or selector.get_map():
                if self.monotonic() >= end:
                    raise NativeError("owned_process_timeout")
                for key, _ in selector.select(0.1):
                    if key.fileobj is proc.stdout or stderr is not None and key.fileobj is proc.stderr:
                        chunk = os.read(key.fileobj.fileno(), 65536)
                        if not chunk:
                            selector.unregister(key.fileobj)
                        else:
                            destination = output if key.fileobj is proc.stdout else error_output
                            room = MAX_OUTPUT - len(output) - len(error_output)
                            destination.extend(chunk[:room])
                            if len(chunk) > room:
                                raise NativeError("owned_output_limit")
                    else:
                        offset += os.write(proc.stdin.fileno(), memoryview(data)[offset:])
                        if offset == len(data):
                            selector.unregister(proc.stdin)
                            proc.stdin.close()
                if progress:
                    progress()
            return proc.wait(timeout=1), bytes(output)
        except (NativeError, adapter.ContractError, OSError, ValueError) as error:
            code = error.args[0] if isinstance(error, (NativeError, adapter.ContractError)) else "owned_process_failed"
            raise NativeError(code, output, error_output) from None
        finally:
            if selector is not None:
                selector.close()
            if data is not None:
                data[:] = b"\0" * len(data)
            if proc.poll() is None:
                try:
                    os.killpg(proc.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                proc.wait(timeout=2)
            proc.stdout.close()
            if stderr is not None:
                proc.stderr.close()

    def run(self, argv, env, seconds=5, data=None, progress=None, deadline=None, stderr=None):
        process = self.spawn(argv, env, data is not None, **({"separate_stderr": True} if stderr is not None else {}))
        return self.wait(process, seconds, data, progress, deadline, **({"stderr": stderr} if stderr is not None else {}))

    def ticks(self, pid):
        try:
            raw = Path(f"/proc/{pid}/stat").read_text()
        except FileNotFoundError:
            return None
        return raw[raw.rfind(")") + 2:].split()[19]

    def process_gone(self, pid, ticks):
        return self.ticks(pid) != ticks

    def terminate(self, proc):
        if proc.poll() is None:
            os.killpg(proc.pid, signal.SIGTERM)
            try:
                proc.wait(timeout=2)
            except subprocess.TimeoutExpired:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.wait(timeout=2)


class Native:
    def __init__(self, system=None, files=None, source=None):
        self.system, self.fs = system or System(), files or Files()
        self.account = self.system.account()
        self.source = source if source is not None else SHIPPED_SOURCE
        if self.source is None:
            self.source = Path(__file__).read_bytes()
        if type(self.source) is not bytes or not 1 <= len(self.source) <= MAX_JSON:
            raise NativeError("shipped_native_source_unavailable")
        self.base = self.account["home"] + "/.config/Nvidia Corporation/Personal AI Router/engine-bin"
        self.plan = self.member = self.paths = None
        self.execution_deadline = self.execution_binding = None

    def bind(self, plan, live=True):
        now = self.system.now()
        adapter.validate_plan(plan, now if live else min(now, plan["expiresAt"] - 1))
        public_path = self.account["home"] + "/.config/Nvidia Corporation/Personal AI Router/cluster/identity.json"
        principal = strict_json(self.fs.read(public_path, 8192, self.account["uid"])).get("node_uuid")
        member = next((m for m in plan["members"] if m["principal"] == principal and all(m[k] == self.account[k] for k in ("uid", "user", "home"))), None)
        if member is None:
            raise NativeError("local_account_not_admitted")
        self.plan, self.member, self.paths = plan, member, adapter.owned_paths(plan, member)
        self.lease_dir = self.base + "/diagnostic-runs/" + plan["operationId"]
        uid = member["uid"]
        lease = strict_json(self.fs.read(self.lease_dir + "/lease.json", MAX_JSON, uid))
        profile_raw = self.fs.read(self.lease_dir + "/profile.json", MAX_JSON, uid)
        profile = strict_json(profile_raw)
        stored = strict_json(self.fs.read(self.lease_dir + "/bootstrap-plan.json", MAX_JSON, uid))
        if stored != plan or fingerprint(profile_raw) != plan["profileDigest"]:
            raise NativeError("admitted_plan_or_profile_changed")
        expected = {k: plan[k] for k in ("groupId", "operationId", "profileDigest", "expiresAt")}
        expected["bootstrapPlanDigest"] = plan["planDigest"]
        request = lease.get("request")
        execution_deadline = request.get("executionDeadlineAt") if isinstance(request, dict) else None
        if isinstance(request, dict) and "executionDeadlineAt" in request:
            if type(execution_deadline) is not int or not plan["createdAt"] < execution_deadline <= plan["expiresAt"]:
                raise NativeError("go_execution_deadline_invalid")
            expected["executionDeadlineAt"] = execution_deadline
        if live and (execution_deadline is None or execution_deadline <= now):
            raise NativeError("go_execution_deadline_required_or_expired")
        if lease.get("request") != expected:
            raise NativeError("go_lease_binding_changed")
        members = profile.get("members", [])
        selected = next((m for m in members if m.get("nodeId") == member["nodeId"]), None)
        if len(members) != adapter.recipe_ranks(plan["recipeId"]) or selected != lease.get("member") or profile.get("transport") != "socket" or profile.get("groupId") != plan["groupId"] or profile.get("ownerNodeId") != plan["ownerNodeId"] or profile.get("dedicatedTestWindow") is not True:
            raise NativeError("go_profile_member_binding_changed")
        owner = next(m for m in plan["members"] if m["nodeId"] == plan["ownerNodeId"])
        owner_paths = adapter.owned_paths(plan, owner)
        expected_bootstrap = {"operationId": plan["operationId"], "publicKey": plan["operationPublicKey"],
                              "agentSocket": owner_paths["agentSocket"], "publicIdentity": owner_paths["publicIdentity"],
                              "subnet": plan["subnet"], "sshSourceIPv4": plan["sshSourceIPv4"]}
        if profile.get("bootstrap") != expected_bootstrap:
            raise NativeError("go_bootstrap_binding_changed")
        if profile.get("knownHosts") != {"path": owner_paths["knownHosts"], "sha256": fingerprint(adapter.known_hosts(plan))} or profile.get("identityFile") != owner_paths["publicIdentity"]:
            raise NativeError("go_public_ssh_selection_changed")
        if profile.get("fabric") != plan.get("fabric"):
            raise NativeError("go_fabric_binding_changed")
        for row, planned in zip(members, plan["members"]):
            fields = {"nodeId": "nodeId", "principal": "principal", "host": "sshAddress", "user": "user", "gpu": "gpuUUID", "interface": "interface"}
            if not adapter.HASH.fullmatch(str(row.get("clusterPinSha256", ""))) or any(row.get(k) != planned[v] for k, v in fields.items()) or row.get("fabric") != planned.get("fabric"):
                raise NativeError("go_profile_projection_changed")
            for field, value in (("manager", planned["manager"]), ("nccl", planned["binary"]), ("smi", planned["tools"]["nvidia-smi"])):
                if row.get(field) != {k: value[k] for k in ("path", "sha256")}:
                    raise NativeError("go_tool_projection_changed")
            runtime = {k: planned[k] for k in ("buildOperationId", "buildPlanDigest", "buildAttempt", "uid", "home")}
            runtime.update({k: {f: planned[k][f] for f in ("path", "sha256")} for k in ("ncclLibrary", "cudaLibrary", "mpiLibrary")})
            if row.get("runtime") != runtime:
                raise NativeError("go_runtime_projection_changed")
        for field, tool in (("mpi", owner["tools"]["mpirun"]), ("ssh", owner["tools"]["ssh"])):
            if profile.get(field) != {k: tool[k] for k in ("path", "sha256")}:
                raise NativeError("go_coordinator_tool_changed")
        if live and (self.fs.exists(self.lease_dir + "/cancelled") or self.fs.exists(self.paths["root"] + "/cancelled")):
            raise NativeError("operation_cancelled")
        binding = (plan["operationId"], plan["planDigest"], execution_deadline)
        if self.execution_binding is not None and self.execution_binding != binding:
            raise NativeError("go_execution_deadline_changed")
        if self.fs.exists(self.paths["root"] + "/native.json") and self.journal().get("executionDeadlineAt") != execution_deadline:
            raise NativeError("go_execution_deadline_changed")
        self.execution_deadline, self.execution_binding = execution_deadline, binding
        return member

    def assert_live_lease(self, plan, node_id):
        if self.bind(plan)["nodeId"] != node_id:
            raise NativeError("participant_not_local")

    @contextlib.contextmanager
    def lock(self, deadline=None):
        self.remaining_before(deadline)
        options = {"deadline": deadline} if deadline is not None else {}
        with self.fs.lock(self.lease_dir + "/lock", self.member["uid"], **options):
            self.remaining_before(deadline)
            yield

    def remaining_before(self, deadline, maximum=20):
        if deadline is None:
            return maximum
        remaining = min(maximum, deadline - self.system.monotonic())
        if remaining <= 0:
            raise NativeError("insufficient_worker_budget")
        return remaining

    def save(self, value):
        path = self.paths["root"] + "/native.json"
        old = self.fs.read(path, MAX_OUTPUT, self.member["uid"]) if self.fs.exists(path) else None
        self.fs.atomic(path, adapter.canonical(value), self.member["uid"], old)

    def journal(self):
        value = strict_json(self.fs.read(self.paths["root"] + "/native.json", MAX_OUTPUT, self.member["uid"], private=True))
        if any(value.get(k) != self.plan[k] for k in ("operationId", "planDigest", "profileDigest")) or value.get("owner") != OWNER or value.get("nodeId") != self.member["nodeId"]:
            raise NativeError("native_journal_binding_changed")
        return value

    def fixed_file(self, name, data, journal, executable=False):
        path = self.paths["root"] + "/" + name
        prior = self.fs.read(path, MAX_OUTPUT, self.member["uid"]) if self.fs.exists(path) else None
        if prior is not None and prior != data:
            raise NativeError("owned_public_file_changed")
        journal["files"][name] = fingerprint(data)
        self.save(journal)  # Intent precedes every public-file effect.
        if prior is None:
            self.fs.atomic(path, data, self.member["uid"], None, 0o700 if executable else 0o600)

    def stage(self, public_files=True):
        parent = self.member["home"]
        for part in (".local", "share", "pair-nccl-smoke-v1", self.plan["operationId"]):
            parent += "/" + part
            if not self.fs.exists(parent):
                self.fs.mkdir(parent, self.member["uid"])
            else:
                self.fs.trusted(parent, self.member["uid"], directory=True, private=part in ("pair-nccl-smoke-v1", self.plan["operationId"]))
        journal_path = self.paths["root"] + "/native.json"
        if self.fs.exists(journal_path):
            journal = self.journal()
        else:
            journal = {"owner": OWNER, "operationId": self.plan["operationId"], "planDigest": self.plan["planDigest"],
                   "profileDigest": self.plan["profileDigest"], "nodeId": self.member["nodeId"], "files": {},
                   "launchRequested": False, "invocationId": None, "agent": None, "authorization": None,
                   "unitCollected": False, "cancelled": False}
            if self.execution_deadline is not None:
                journal["executionDeadlineAt"] = self.execution_deadline
            self.save(journal)
        if not public_files:
            return journal
        spec = adapter.compile_spec(self.plan)
        for name, data in (("mpi_socket_native.py", self.source), ("mpi_socket_adapter.py", adapter.SHIPPED_SOURCE),
                           ("identity.pub", (adapter.public_key(self.plan["operationPublicKey"], True) + "\n").encode()),
                           ("known_hosts", spec["knownHosts"].encode()), ("mpi.app", spec["coordinator"]["mpiApp"].encode())):
            self.fixed_file(name, data, journal)
        native_path = self.paths["root"] + "/mpi_socket_native.py"
        for filename, mode in (("ssh_adapter", "ssh"), ("peer_entry", "peer"), ("rank_entry", "rank")):
            argv = ["/usr/bin/python3", "-I", native_path, "--internal-" + mode, self.plan["operationId"], self.plan["planDigest"]]
            content = ("#!/usr/bin/python3\nimport os,sys\nos.execv('/usr/bin/python3'," + repr(argv) + "+sys.argv[1:])\n").encode()
            self.fixed_file(filename, content, journal, True)
        return journal

    def verify_artifact(self, value, seconds=20, deadline=None):
        end = self.system.monotonic() + min(20, seconds)
        if deadline is not None:
            end = min(end, deadline)
        self.remaining_before(end)
        path, uid = value["path"], self.member["uid"]
        if path in adapter.SYSTEM_TOOLS.values():
            path = self.fs.system_tool(path)
        before = self.fs.trusted(path, uid)
        if before.st_size != value["size"]:
            raise NativeError("artifact_size_changed")
        self.remaining_before(end)
        digest = hashlib.sha256()
        fd = os.open(self.fs.path(path), os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        try:
            actual = os.fstat(fd)
            if (before.st_dev, before.st_ino) != (actual.st_dev, actual.st_ino):
                raise NativeError("artifact_identity_changed")
            while True:
                self.remaining_before(end)
                chunk = os.read(fd, 1024 * 1024)
                self.remaining_before(end)
                if not chunk:
                    break
                digest.update(chunk)
            if digest.hexdigest() != value["sha256"] or os.fstat(fd).st_size != before.st_size:
                raise NativeError("artifact_bytes_changed")
        finally:
            os.close(fd)

    def verify_inputs(self, budget=None):
        for value in [self.member[k] for k in ("manager", "binary", "ncclLibrary", "cudaLibrary", "mpiLibrary")] + list(self.member["tools"].values()):
            if budget:
                seconds, deadline = budget()
                self.verify_artifact(value, seconds, deadline=deadline)
            else:
                self.verify_artifact(value)
        if budget:
            budget()
        observations = self.system.interfaces()
        adapter.verify_interface_choice(self.plan, self.member["nodeId"], observations)
        if "fabric" in self.member:
            identity = self.system.interface_identity(self.member["fabric"]["interface"])
            adapter.verify_fabric_choice(self.plan, self.member["nodeId"], observations, identity)
        if self.member["nodeId"] == self.plan["ownerNodeId"] and not any(row["up"] and self.plan["sshSourceIPv4"] in [a.split("/")[0] for a in row["addresses"]] for row in observations):
            raise NativeError("ssh_source_not_locally_assigned")

    def environment(self):
        uid = self.member["uid"]
        return {"PATH": "/usr/bin:/bin", "HOME": self.member["home"], "LANG": "C", "LC_ALL": "C",
                "XDG_RUNTIME_DIR": f"/run/user/{uid}", "DBUS_SESSION_BUS_ADDRESS": f"unix:path=/run/user/{uid}/bus"}

    def user(self, args, seconds=5, deadline=None):
        if args[0] not in ("/usr/bin/systemctl", "/usr/bin/busctl"):
            raise NativeError("unsupported_user_manager_command")
        self.verify_artifact(self.member["tools"][args[0].rsplit("/", 1)[1]], deadline=deadline)
        seconds = self.remaining_before(deadline, seconds)
        result = self.system.run(args, self.environment(), seconds, deadline=deadline)
        self.remaining_before(deadline)
        return result

    def unit_argv(self, role):
        return ["/usr/bin/python3", "-I", self.paths["root"] + "/mpi_socket_native.py", "--internal-worker",
                self.plan["operationId"], self.plan["planDigest"], role]

    def cgroup(self):
        uid = self.member["uid"]
        return f"/user.slice/user-{uid}.slice/user@{uid}.service/app.slice/" + self.paths["unit"]

    def cgroup_empty(self, deadline=None):
        self.remaining_before(deadline)
        path = "/sys/fs/cgroup" + self.cgroup() + "/cgroup.events"
        if not self.fs.exists(path):
            return True
        raw = self.fs.read(path, 4096, self.member["uid"]).decode("ascii")
        self.remaining_before(deadline)
        return dict(line.split() for line in raw.splitlines()).get("populated") == "0"

    def job_absent(self, deadline=None):
        code, output = self.user(["/usr/bin/busctl", "--user", "--json=short", "call", "org.freedesktop.systemd1",
                                  "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", "ListJobs"], deadline=deadline)
        if code:
            raise NativeError("user_manager_jobs_unknown")
        value = strict_json(output)
        if value.get("type") != "a(usssoo)" or not isinstance(value.get("data"), list) or len(value["data"]) != 1 or not isinstance(value["data"][0], list) or len(value["data"][0]) > 256:
            raise NativeError("user_manager_jobs_unknown")
        return all(isinstance(row, list) and len(row) == 6 and row[1] != self.paths["unit"] for row in value["data"][0])

    def unit(self, deadline=None):
        properties = "Id,LoadState,Description,Transient,Type,ExitType,KillMode,Restart,RemainAfterExit,ActiveState,SubState,MainPID,ControlPID,ControlGroup,InvocationID,RuntimeMaxUSec,TimeoutStopUSec,LimitCORE,Slice,ExecMainCode,ExecMainStatus"
        code, output = self.user(["/usr/bin/systemctl", "--user", "show", self.paths["unit"], "--no-pager", "--property=" + properties], deadline=deadline)
        fields = {}
        for line in output.decode("utf-8", "strict").splitlines():
            key, separator, value = line.partition("=")
            if not separator or key in fields:
                raise NativeError("unit_observation_malformed")
            fields[key] = value
        if fields.get("LoadState") == "not-found" and self.job_absent(deadline=deadline):
            return None
        if code or fields.get("Id") != self.paths["unit"]:
            raise NativeError("unit_observation_unknown")
        journal = self.journal()
        self.remaining_before(deadline)
        role = "coordinator" if self.member["nodeId"] == self.plan["ownerNodeId"] else "peer"
        if journal.get("launchRequested") is not True or journal.get("unitArgv") != self.unit_argv(role) or type(journal.get("unitSeconds")) is not int or not 1 <= journal["unitSeconds"] <= 105:
            raise NativeError("unit_intent_changed")
        expected = {"Description": OWNER + ":" + self.plan["operationId"] + ":" + self.plan["planDigest"] + ":" + self.member["nodeId"],
                    "Transient": "yes", "Type": "exec", "ExitType": "cgroup", "KillMode": "control-group", "Restart": "no",
                    "RemainAfterExit": "yes", "LimitCORE": "0", "Slice": "app.slice", "TimeoutStopUSec": "10s"}
        if any(fields.get(k) != v for k, v in expected.items()):
            raise NativeError("foreign_or_changed_unit")
        duration = fields.get("RuntimeMaxUSec", "")
        match = re.fullmatch(r"(?:(\d+)min )?(\d+)s", duration)
        if not match or int(match[1] or 0) * 60 + int(match[2]) != journal["unitSeconds"]:
            raise NativeError("unit_lifetime_changed")
        if not adapter.ID.fullmatch(fields.get("InvocationID", "")) or journal.get("invocationId") not in (None, fields["InvocationID"]):
            raise NativeError("unit_invocation_changed")
        object_path = "/org/freedesktop/systemd1/unit/" + "".join(c if c.isalnum() else "_" + format(ord(c), "02x") for c in self.paths["unit"])
        code, raw = self.user(["/usr/bin/busctl", "--user", "--json=short", "get-property", "org.freedesktop.systemd1",
                              object_path, "org.freedesktop.systemd1.Service", "ExecStart"], deadline=deadline)
        decoded = strict_json(raw) if not code else {}
        data = decoded.get("data")
        if decoded.get("type") != "a(sasbttttuii)" or not isinstance(data, list) or len(data) != 1 or not isinstance(data[0], list) or len(data[0]) != 10 or data[0][0:2] != ["/usr/bin/python3", journal["unitArgv"]] or data[0][2] is not False:
            raise NativeError("unit_exec_binding_changed")
        if fields.get("ControlGroup") != self.cgroup():
            terminal = fields.get("ActiveState") in ("active", "inactive", "failed") and fields.get("SubState") in ("exited", "dead", "failed")
            if fields.get("ControlGroup") != "" or not terminal or fields.get("MainPID") != "0" or fields.get("ControlPID") != "0" or not self.job_absent(deadline=deadline) or not self.cgroup_empty(deadline=deadline):
                raise NativeError("unit_cgroup_changed")
        self.remaining_before(deadline)
        return fields

    def authorization(self, remove=False):
        with self.lock():
            self.bind(self.plan, live=not remove)
            return self.authorization_locked(remove)

    def authorization_locked(self, remove=False):
        if self.member["nodeId"] == self.plan["ownerNodeId"]:
            return True
        uid, directory = self.member["uid"], self.member["home"] + "/.ssh"
        entry = adapter.authorization_entry(self.plan, self.member)
        if not self.fs.exists(directory):
            if remove:
                return True
            self.fs.mkdir(directory, uid)
        self.fs.trusted(directory, uid, directory=True, private=True)
        path = directory + "/authorized_keys"
        with self.fs.lock(directory + "/.pair-nccl-authorized.lock", uid):
            if self.fs.exists(path):
                info = self.fs.info(path)
                if info.st_uid != uid or info.st_nlink != 1:
                    raise NativeError("authorization_file_ownership_changed")
            before = self.fs.read(path, adapter.LIMITS["maxAuthorizedKeysBytes"], uid) if self.fs.exists(path) else None
            edited = adapter.authorized_keys_edit(before or b"", entry, remove)
            if edited["bytes"] != (before or b""):
                journal = self.journal()
                journal["authorization"] = {"entrySha256": fingerprint(entry), "beforeSha256": edited["beforeSha256"],
                                            "afterSha256": edited["afterSha256"], "removeRequested": remove}
                self.save(journal)
                self.fs.atomic(path, edited["bytes"], uid, before)
            after = self.fs.read(path, adapter.LIMITS["maxAuthorizedKeysBytes"], uid) if self.fs.exists(path) else b""
            return entry not in after if remove else after.splitlines(keepends=True).count(entry) == 1

    def fence(self):
        with self.lock():
            self.bind(self.plan, live=False)
            journal = self.stage(public_files=False)
            for path in (self.lease_dir + "/cancelled", self.paths["root"] + "/cancelled"):
                if not self.fs.exists(path):
                    self.fs.atomic(path, b"cancelled\n", self.member["uid"])
            journal["cancelled"] = True
            self.save(journal)

    def worker_result(self):
        path = self.paths["root"] + "/worker-result.json"
        if not self.fs.exists(path):
            return None
        value = strict_json(self.fs.read(path, MAX_OUTPUT, self.member["uid"], private=True))
        if any(value.get(k) != self.plan[k] for k in ("operationId", "planDigest")) or value.get("nodeId") != self.member["nodeId"] or type(value.get("exitCode")) is not int or not -64 <= value["exitCode"] <= 255 or value.get("done") is not True or not isinstance(value.get("output"), str):
            raise NativeError("worker_result_binding_changed")
        if not isinstance(value.get("stderr", ""), str) or len((value["output"] + value.get("stderr", "")).encode("utf-8")) > MAX_OUTPUT:
            raise NativeError("worker_result_binding_changed")
        return value

    def collect_unit(self, stop=False):
        fields = self.unit()
        if fields is not None:
            if not stop and (fields["MainPID"] != "0" or fields["ControlPID"] != "0" or not self.cgroup_empty()):
                return False
            code, _ = self.user(["/usr/bin/systemctl", "--user", "stop", self.paths["unit"]], 10)
            if code:
                raise NativeError("owned_unit_stop_failed")
            end = time.monotonic() + 10
            while time.monotonic() < end:
                fields = self.unit()
                if fields is None:
                    break
                if fields["ActiveState"] == "failed" and self.cgroup_empty():
                    self.user(["/usr/bin/systemctl", "--user", "reset-failed", self.paths["unit"]])
                time.sleep(0.1)
        clean = self.unit() is None and self.job_absent() and self.cgroup_empty()
        if clean:
            with self.lock():
                journal = self.journal()
                journal["unitCollected"] = True
                self.save(journal)
        return clean

    def rank_clean(self):
        path = self.lease_dir + "/rank.json"
        if not self.fs.exists(path):
            return self.fs.exists(self.lease_dir + "/cancelled") and self.cgroup_empty()
        rank = strict_json(self.fs.read(path, MAX_JSON, self.member["uid"]))
        if type(rank.get("pid")) is not int or rank["pid"] < 0 or not isinstance(rank.get("startTicks"), str):
            return False
        return (rank["pid"] == 0 and rank.get("done") is True and rank.get("clean") is True) or (rank["pid"] > 0 and rank["startTicks"].isdigit() and self.system.process_gone(rank["pid"], rank["startTicks"]))

    def retained_agent_gone(self, journal):
        agent = journal.get("agent")
        if agent is None:
            return True
        return isinstance(agent, dict) and set(agent) == {"pid", "startTicks", "socket", "fingerprint"} and \
            type(agent.get("pid")) is int and agent["pid"] > 0 and isinstance(agent.get("startTicks"), str) and agent["startTicks"].isdigit() and \
            agent.get("socket") == self.paths["agentSocket"] and agent.get("fingerprint") == self.plan["operationPublicKey"]["fingerprint"] and \
            self.system.process_gone(agent["pid"], agent["startTicks"])

    def authorization_removed(self):
        if self.member["nodeId"] == self.plan["ownerNodeId"]:
            return True
        path = self.member["home"] + "/.ssh/authorized_keys"
        return not self.fs.exists(path) or adapter.authorization_entry(self.plan, self.member) not in self.fs.read(path, MAX_OUTPUT, self.member["uid"])

    def remove_public_artifacts(self, journal):
        files = journal.get("files")
        if not isinstance(files, dict):
            raise NativeError("public_artifact_journal_changed")
        for name, digest in files.items():
            if name not in PUBLIC_ARTIFACTS or not isinstance(digest, str) or not adapter.HASH.fullmatch(digest):
                raise NativeError("foreign_public_artifact")
        socket_path = self.paths["agentSocket"]
        if self.fs.exists(socket_path):
            info = self.fs.info(socket_path)
            if not stat.S_ISSOCK(info.st_mode) or info.st_uid != self.member["uid"]:
                raise NativeError("agent_socket_ownership_changed")
            self.fs.path(socket_path).unlink()
        for name, digest in files.items():
            self.fs.remove(self.paths["root"] + "/" + name, digest, self.member["uid"])
        return not self.fs.exists(socket_path) and all(not self.fs.exists(self.paths["root"] + "/" + name) for name in PUBLIC_ARTIFACTS)

    def retained_cleanup_proofs(self, journal):
        markers = (self.lease_dir + "/cancelled", self.paths["root"] + "/cancelled")
        markers_bound = all(self.fs.exists(path) and self.fs.read(path, 64, self.member["uid"], private=True) == b"cancelled\n" for path in markers)
        return journal.get("cancelled") is True and journal.get("unitCollected") is True and markers_bound and \
            self.cgroup_empty() and self.rank_clean() and self.retained_agent_gone(journal) and self.authorization_removed()

    def cleanup_after_system_tool_drift(self, error):
        code = error.args[0] if isinstance(error, NativeError) and error.args else None
        if code not in SYSTEM_TOOL_ARTIFACT_DRIFT:
            raise error
        with self.lock():
            self.bind(self.plan, live=False)
            journal = self.journal()
            if not self.retained_cleanup_proofs(journal):
                raise NativeError("retained_cleanup_proof_incomplete")
            if not self.remove_public_artifacts(journal) or not self.retained_cleanup_proofs(journal):
                raise NativeError("retained_cleanup_proof_incomplete")
        flags = {"unitOwned": True, "unitProcessesGone": True, "unitMetadataRemoved": True,
                 "rankCleanupConfirmed": True, "authorizationRemoved": True, "agentGone": True, "publicArtifactsRemoved": True}
        return {"schemaVersion": 1, "action": "cleanup", "operationId": self.plan["operationId"], "planDigest": self.plan["planDigest"],
                "profileDigest": self.plan["profileDigest"], "nodeId": self.member["nodeId"], "state": "cancelled",
                "output": "", "exitCode": None, "cleanupConfirmed": True, **flags, "errorCode": None,
                **({"stderr": ""} if self.plan["recipeId"] in (adapter.QUICK_RECIPE, adapter.TRIPLE_RECIPE) else {})}

    def receipt(self, action, state, output="", exit_code=None, removed=False, error=None, stderr=""):
        journal = self.journal()
        unit = self.unit()
        gone = unit is None and self.job_absent() and self.cgroup_empty()
        agent = journal.get("agent")
        agent_gone = gone and (agent is None or self.system.process_gone(agent["pid"], agent["startTicks"]))
        authorization_removed = self.member["nodeId"] == self.plan["ownerNodeId"]
        if not authorization_removed:
            path = self.member["home"] + "/.ssh/authorized_keys"
            authorization_removed = not self.fs.exists(path) or adapter.authorization_entry(self.plan, self.member) not in self.fs.read(path, MAX_OUTPUT, self.member["uid"])
        rank = self.rank_clean()
        flags = {"unitOwned": True, "unitProcessesGone": gone, "unitMetadataRemoved": unit is None,
                 "rankCleanupConfirmed": rank, "authorizationRemoved": authorization_removed,
                 "agentGone": agent_gone, "publicArtifactsRemoved": removed}
        clean = all(flags.values())
        return {"schemaVersion": 1, "action": action, "operationId": self.plan["operationId"], "planDigest": self.plan["planDigest"],
                "profileDigest": self.plan["profileDigest"], "nodeId": self.member["nodeId"], "state": state,
                "output": output, "exitCode": exit_code, "cleanupConfirmed": clean, **flags, "errorCode": error,
                **({"stderr": stderr} if self.plan["recipeId"] in (adapter.QUICK_RECIPE, adapter.TRIPLE_RECIPE) else {})}

    def prepare_peer(self, plan):
        self.bind(plan)
        if self.member["nodeId"] == plan["ownerNodeId"]:
            raise NativeError("peer_prepare_on_coordinator")
        self.verify_inputs()
        with self.lock():
            self.bind(plan)
            self.stage()
        if not self.authorization():
            raise NativeError("authorization_readback_failed")
        return self.receipt("prepare-peer", "prepared")

    def observe(self, plan):
        self.bind(plan, live=False)
        journal = self.journal()
        result = self.worker_result()
        removed = all(not self.fs.exists(self.paths["root"] + "/" + name) for name in journal["files"])
        state = "running" if self.unit() is not None else "completed" if result and result["exitCode"] == 0 else "failed" if result else "prepared"
        return self.receipt("status", state, result.get("output", "") if result else "", result["exitCode"] if result else None, removed,
                            result.get("errorCode") if result else None, stderr=result.get("stderr", "") if result else "")

    def cancel(self, plan):
        self.bind(plan, live=False)
        self.fence()
        removed = self.authorization(remove=True)
        gone = self.collect_unit(stop=True)
        result = self.receipt("cancel", "cancelled" if gone and removed else "cleanup-unknown")
        return result

    def cleanup(self, plan):
        self.bind(plan, live=False)
        self.fence()
        self.authorization(remove=True)
        try:
            if not self.collect_unit(stop=True):
                return self.receipt("cleanup", "cleanup-unknown")
            with self.lock():
                journal = self.journal()
                if not self.rank_clean():
                    return self.receipt("cleanup", "cleanup-unknown")
                removed = self.remove_public_artifacts(journal)
            return self.receipt("cleanup", "cancelled", removed=removed)
        except NativeError as error:
            return self.cleanup_after_system_tool_drift(error)

    def unit_command(self, journal):
        description = OWNER + ":" + self.plan["operationId"] + ":" + self.plan["planDigest"] + ":" + self.member["nodeId"]
        properties = {"Type": "exec", "ExitType": "cgroup", "RuntimeMaxSec": journal["unitSeconds"],
                      "TimeoutStartSec": 10, "TimeoutStopSec": 10, "KillMode": "control-group", "Restart": "no",
                      "RemainAfterExit": "yes", "LimitCORE": 0, "UMask": "0077", "NoNewPrivileges": "yes", "Slice": "app.slice",
                      "ConditionPathExists": "!" + self.lease_dir + "/cancelled"}
        argv = ["/usr/bin/systemd-run", "--user", "--quiet", "--wait", "--pipe", "--unit=" + self.paths["unit"], "--description=" + description]
        for key, value in properties.items():
            argv += ["--property=" + key + "=" + str(value)]
        return argv + ["--", *journal["unitArgv"]]

    def launch_unit(self, role, key=None, daemon=None):
        self.verify_inputs()
        with self.lock():
            self.bind(self.plan)
            journal = self.stage()
            if journal["launchRequested"]:
                raise NativeError("operation_already_started")
            if self.unit() is not None or not self.cgroup_empty():
                raise NativeError("preexisting_unit_or_cgroup")
            remaining = (self.plan["expiresAt"] - self.system.now()) // 1000
            if remaining < 20:
                raise NativeError("insufficient_remaining_lease")
            unit_seconds = min(105, remaining - 10)
            journal.update(launchRequested=True, unitSeconds=unit_seconds, unitArgv=self.unit_argv(role), daemon=daemon,
                           unitDeadlineMonotonic=self.system.monotonic() + unit_seconds)
            self.save(journal)
            self.worker_budget(journal)  # Journal/setup time consumes the same deadline.
            proc = self.system.spawn(self.unit_command(journal), self.environment(), key is not None)
        last = [0.0]
        def progress():
            if time.monotonic() - last[0] < 0.25:
                return
            last[0] = time.monotonic()
            result = self.worker_result()
            if result and result["done"]:
                fields = self.unit()
                if fields and fields.get("MainPID") == "0" and self.cgroup_empty():
                    self.collect_unit(stop=False)
        try:
            self.system.wait(proc, min(115, remaining), key._value if key is not None else None, progress)
        finally:
            if key is not None:
                key._value[:] = b"\0" * len(key._value)
        result = self.worker_result()
        if result is None:
            raise NativeError("owned_worker_result_missing")
        return result

    def start_coordinator(self, plan, private_key):
        try:
            self.bind(plan)
            if self.member["nodeId"] != plan["ownerNodeId"] or not isinstance(private_key, adapter.VolatileKey):
                raise NativeError("coordinator_not_admitted")
        except BaseException:
            if isinstance(private_key, adapter.VolatileKey):
                private_key._value[:] = b"\0" * len(private_key._value)
            raise
        result, failure = None, None
        try:
            result = self.launch_unit("coordinator", private_key)
        except (NativeError, adapter.ContractError, OSError, ValueError) as error:
            failure = error.args[0] if isinstance(error, (NativeError, adapter.ContractError)) else "coordinator_native_failure"
        finally:
            private_key._value[:] = b"\0" * len(private_key._value)
        try:
            cleanup = self.cleanup(plan)
            cleanup.update(action="start-coordinator", state="completed" if result and result["exitCode"] == 0 and cleanup["cleanupConfirmed"] else "failed",
                           output=result.get("output", "") if result else "", exitCode=result["exitCode"] if result else None,
                           errorCode=failure or (result.get("errorCode") if result else "worker_result_unavailable"))
            if self.plan["recipeId"] in (adapter.QUICK_RECIPE, adapter.TRIPLE_RECIPE):
                cleanup["stderr"] = result.get("stderr", "") if result else ""
            return cleanup
        except (NativeError, adapter.ContractError, OSError, ValueError):
            return self.failure("start-coordinator", "cleanup-unknown", "owned_cleanup_unconfirmed", result)

    def failure(self, action, state, error, result=None):
        return {"schemaVersion": 1, "action": action, "operationId": self.plan["operationId"], "planDigest": self.plan["planDigest"],
                "profileDigest": self.plan["profileDigest"], "nodeId": self.member["nodeId"], "state": state,
                "output": result.get("output", "") if result else "", "exitCode": result.get("exitCode") if result else None,
                "cleanupConfirmed": False, "unitOwned": False, "unitProcessesGone": False, "unitMetadataRemoved": False,
                "rankCleanupConfirmed": False, "authorizationRemoved": False, "agentGone": False, "publicArtifactsRemoved": False,
                "errorCode": error,
                **({"stderr": result.get("stderr", "") if result else ""} if self.plan["recipeId"] in (adapter.QUICK_RECIPE, adapter.TRIPLE_RECIPE) else {})}

    def assert_worker(self, role, deadline=None):
        self.remaining_before(deadline)
        self.bind(self.plan)
        self.remaining_before(deadline)
        journal = self.journal()
        self.remaining_before(deadline)
        fields = self.unit(deadline=deadline)
        invocation = os.environ.get("INVOCATION_ID")
        actual_group = self.fs.read(f"/proc/{os.getpid()}/cgroup", 4096, self.member["uid"]).decode().strip()
        self.remaining_before(deadline)
        if not fields or journal.get("unitArgv") != self.unit_argv(role) or fields["MainPID"] != str(os.getpid()) or invocation != fields["InvocationID"] or actual_group != "0::" + self.cgroup():
            raise NativeError("worker_not_in_owned_unit")
        with self.lock(deadline=deadline):
            self.bind(self.plan)
            self.remaining_before(deadline)
            journal = self.journal()
            self.remaining_before(deadline)
            journal["invocationId"] = invocation
            self.save(journal)
            self.remaining_before(deadline)
        return journal

    def worker_budget(self, journal, maximum=90):
        now = self.system.monotonic()
        deadline = journal.get("unitDeadlineMonotonic")
        seconds = journal.get("unitSeconds")
        execution_deadline = journal.get("executionDeadlineAt")
        if type(execution_deadline) is not int or execution_deadline != self.execution_deadline:
            raise NativeError("go_execution_deadline_changed")
        if (type(deadline) not in (int, float) or not math.isfinite(deadline) or deadline <= 0 or
                type(seconds) is not int or not 1 <= seconds <= 105 or deadline > now + seconds):
            raise NativeError("worker_deadline_unknown")
        wall_deadline = min(self.plan["expiresAt"], execution_deadline)
        cutoff = min(deadline, now + (wall_deadline - self.system.now()) / 1000) - RESULT_RESERVE_SECONDS
        usable = min(maximum, cutoff - now)
        if usable < 1:
            raise NativeError("insufficient_worker_budget")
        return usable, cutoff

    def worker(self, role):
        expected = "coordinator" if self.member["nodeId"] == self.plan["ownerNodeId"] else "peer"
        if role != expected:
            raise NativeError("wrong_worker_role")
        initial = self.journal()
        _, deadline = self.worker_budget(initial)
        journal = self.assert_worker(role, deadline=deadline)
        agent, key, command, code, output, error_code = None, None, None, 1, b"", None
        stderr = bytearray() if self.plan["recipeId"] in (adapter.QUICK_RECIPE, adapter.TRIPLE_RECIPE) else None
        try:
            self.verify_inputs(budget=lambda: self.worker_budget(journal))
            if role == "coordinator":
                key = bytearray(sys.stdin.buffer.read(16385))
                if not 1 <= len(key) <= 16384:
                    raise NativeError("invalid_volatile_key_buffer")
                spec = adapter.compile_spec(self.plan)["coordinator"]
                env = {**self.environment(), **spec["agentEnvironment"]}
                with self.lock():
                    self.bind(self.plan)
                    self.worker_budget(journal)
                    agent = self.system.spawn(spec["agentArgv"], env)
                ticks = self.system.ticks(agent.pid)
                if ticks is None:
                    raise NativeError("agent_process_identity_unknown")
                with self.lock():
                    self.bind(self.plan)
                    current = self.journal()
                    current["agent"] = {"pid": agent.pid, "startTicks": ticks, "socket": self.paths["agentSocket"], "fingerprint": self.plan["operationPublicKey"]["fingerprint"]}
                    self.save(current)
                until = self.system.monotonic() + self.worker_budget(journal, 5)[0]
                while not self.fs.exists(self.paths["agentSocket"]):
                    self.bind(self.plan)
                    if agent.poll() is not None or self.system.monotonic() > until:
                        raise NativeError("agent_socket_unavailable")
                    time.sleep(0.02)
                with self.lock():
                    self.bind(self.plan)
                    seconds, cutoff = self.worker_budget(journal, 5)
                    add = self.system.spawn(spec["addKeyArgv"], env, input_pipe=True)
                add_code, _ = self.system.wait(add, seconds, key, deadline=cutoff)
                if add_code:
                    raise NativeError("agent_key_load_failed")
                seconds, cutoff = self.worker_budget(journal, 5)
                list_code, public = self.system.run(["/usr/bin/ssh-add", "-L"], env, seconds, deadline=cutoff)
                lines = public.decode("ascii", "strict").splitlines()
                if list_code or len(lines) != 1 or " ".join(lines[0].split()[:2]) != adapter.public_key(self.plan["operationPublicKey"], True):
                    raise NativeError("agent_public_identity_changed")
                argv = spec["mpiArgv"][:]
                environment = {**spec["mpiEnvironment"], "LANG": "C", "LC_ALL": "C"}
            else:
                argv = journal.get("daemon")
                if not isinstance(argv, list) or argv != adapter.parse_daemon_command(shlex.join(argv), self.plan, self.member):
                    raise NativeError("daemon_request_changed")
                environment = self.environment()
            def watch():
                self.bind(self.plan)
            with self.lock():
                self.bind(self.plan)
                seconds, cutoff = self.worker_budget(journal)
                if role == "coordinator":
                    argv[argv.index("--timeout") + 1] = str(int(seconds))
                command = self.system.spawn(argv, environment, **({"separate_stderr": True} if stderr is not None else {}))
            code, output = self.system.wait(command, seconds, progress=watch, deadline=cutoff,
                                            **({"stderr": stderr} if stderr is not None else {}))
        except (NativeError, adapter.ContractError, OSError, ValueError) as error:
            error_code = error.args[0] if isinstance(error, (NativeError, adapter.ContractError)) else "owned_worker_failed"
            if command is not None and isinstance(error, NativeError):
                output = error.output  # Never retain private ssh-add setup output.
                if stderr is not None:
                    stderr[:] = error.stderr
        finally:
            if key is not None:
                key[:] = b"\0" * len(key)
            if agent is not None:
                try:
                    self.system.terminate(agent)
                except (OSError, subprocess.SubprocessError):
                    error_code = error_code or "agent_cleanup_failed"
                    code = code or 1
                finally:
                    if agent.stdout is not None:
                        agent.stdout.close()
            result = {"operationId": self.plan["operationId"], "planDigest": self.plan["planDigest"], "nodeId": self.member["nodeId"],
                      "done": True, "exitCode": code, "output": output.decode("utf-8", "replace"), "errorCode": error_code}
            if stderr is not None:
                result["stderr"] = stderr.decode("utf-8", "replace")
            if len(adapter.canonical(result)) > MAX_OUTPUT:
                result.update(exitCode=1, output="", errorCode="owned_output_limit")
                if stderr is not None:
                    result["stderr"] = ""
            path = self.paths["root"] + "/worker-result.json"
            with self.lock():
                old = self.fs.read(path, MAX_OUTPUT, self.member["uid"]) if self.fs.exists(path) else None
                self.fs.atomic(path, adapter.canonical(result), self.member["uid"], old)
        return code

    def load_internal(self, operation_id, plan_digest):
        if not adapter.ID.fullmatch(operation_id) or not adapter.HASH.fullmatch(plan_digest):
            raise NativeError("invalid_internal_binding")
        plan = strict_json(self.fs.read(self.base + "/diagnostic-runs/" + operation_id + "/bootstrap-plan.json", MAX_JSON, self.account["uid"]))
        if plan.get("planDigest") != plan_digest:
            raise NativeError("internal_plan_changed")
        self.bind(plan)
        journal = self.journal()
        for name in ("mpi_socket_native.py", "mpi_socket_adapter.py"):
            if journal["files"].get(name) != fingerprint(self.fs.read(self.paths["root"] + "/" + name, MAX_JSON, self.member["uid"], private=True)):
                raise NativeError("internal_adapter_bytes_changed")

    def internal(self, mode, argv):
        if len(argv) < 2:
            raise NativeError("internal_arguments_missing")
        self.load_internal(argv[0], argv[1])
        if mode == "worker" and len(argv) == 3:
            return self.worker(argv[2])
        if mode == "ssh":
            if self.member["nodeId"] != self.plan["ownerNodeId"] or len(argv) < 4:
                raise NativeError("internal_ssh_arguments_changed")
            if self.unit() is None or self.fs.read(f"/proc/{os.getpid()}/cgroup", 4096, self.member["uid"]).decode().strip() != "0::" + self.cgroup():
                raise NativeError("ssh_callback_not_in_owned_unit")
            command = " ".join(argv[3:])
            launch = adapter.ssh_launch_argv(self.plan, argv[2], command)
            with self.lock():
                self.bind(self.plan)
            stderr = bytearray() if self.plan["recipeId"] in (adapter.QUICK_RECIPE, adapter.TRIPLE_RECIPE) else None
            try:
                code, output = self.system.run(launch, self.environment(), min(105, (self.plan["expiresAt"] - self.system.now()) / 1000),
                                               **({"stderr": stderr} if stderr is not None else {}))
            except NativeError as error:
                if stderr is not None:
                    sys.stdout.buffer.write(error.output)
                    sys.stderr.buffer.write(error.stderr)
                raise
            sys.stdout.buffer.write(output)
            if stderr is not None:
                sys.stderr.buffer.write(stderr)
            return code
        if mode == "peer" and len(argv) == 4 and argv[2:] == argv[:2]:
            if self.member["nodeId"] == self.plan["ownerNodeId"]:
                raise NativeError("peer_entry_on_coordinator")
            connection = os.environ.get("SSH_CONNECTION", "").split()
            if len(connection) != 4 or connection[0] != self.plan["sshSourceIPv4"] or connection[2] != self.member["sshAddress"] or connection[3] != str(self.member["sshPort"]):
                raise NativeError("ssh_peer_connection_changed")
            daemon = adapter.parse_daemon_command(os.environ.get("SSH_ORIGINAL_COMMAND", ""), self.plan, self.member)
            result = self.launch_unit("peer", daemon=daemon)
            sys.stdout.write(result.get("output", ""))
            if self.plan["recipeId"] in (adapter.QUICK_RECIPE, adapter.TRIPLE_RECIPE):
                sys.stderr.write(result.get("stderr", ""))
            return result["exitCode"]
        if mode == "rank" and len(argv) == 4 and argv[2:] == [self.plan["groupId"], self.plan["operationId"]]:
            fields = self.unit()
            actual = self.fs.read(f"/proc/{os.getpid()}/cgroup", 4096, self.member["uid"]).decode().strip()
            if not fields or actual != "0::" + self.cgroup():
                raise NativeError("rank_not_in_owned_unit")
            self.verify_artifact(self.member["manager"])
            environment = {key: value for key, value in os.environ.items() if re.fullmatch(r"(?:OMPI|PMI|PMIX)_[A-Za-z0-9_]{1,128}", key) and len(value) <= 4096}
            if len(environment) > 256:
                raise NativeError("mpi_environment_limit")
            environment.update(adapter.rank_environment(self.member, self.plan["recipeId"]))
            with self.lock():
                self.bind(self.plan)
            os.execve(self.member["manager"]["path"], [self.member["manager"]["path"], "--diagnostic-rank", self.plan["groupId"], self.plan["operationId"]], environment)
        raise NativeError("unsupported_internal_entry")


def main():
    native, request, key = None, {}, None
    internal = len(sys.argv) > 1 and sys.argv[1].startswith("--internal-")
    try:
        native = Native()
        if internal:
            code = native.internal(sys.argv[1].removeprefix("--internal-"), sys.argv[2:])
            raise SystemExit(code if 0 <= code <= 255 else 1)
        raw = sys.stdin.buffer.readline(MAX_JSON + 1)
        if len(raw) > MAX_JSON or not raw.endswith(b"\n"):
            raise NativeError("invalid_header_size")
        request = strict_json(raw)
        adapter.exact(request, ("action", "plan"))
        action, plan = request["action"], request["plan"]
        if action == "start-coordinator":
            key = adapter.VolatileKey(bytearray(sys.stdin.buffer.read(16385)))
            result = native.start_coordinator(plan, key)
        elif action in ("prepare-peer", "status", "cancel", "cleanup"):
            if sys.stdin.buffer.read(1):
                raise NativeError("unexpected_trailing_input")
            result = {"prepare-peer": native.prepare_peer, "status": native.observe, "cancel": native.cancel, "cleanup": native.cleanup}[action](plan)
        else:
            raise NativeError("unsupported_native_action")
    except (NativeError, adapter.ContractError, ValueError, TypeError, KeyError, OSError) as error:
        if internal:
            sys.stderr.write("PAIR MPI callback rejected: " + callback_failure_reason(error) + "\n")
            raise SystemExit(1)
        if native is not None and native.plan is not None:
            result = native.failure(request.get("action", "unknown"), "cleanup-unknown", "native_operation_unconfirmed")
        else:
            plan = request.get("plan", {}) if isinstance(request, dict) else {}
            result = {"schemaVersion": 1, "action": request.get("action", "unknown") if isinstance(request, dict) else "unknown",
                      "operationId": plan.get("operationId", ""), "planDigest": plan.get("planDigest", ""), "profileDigest": plan.get("profileDigest", ""),
                      "nodeId": "", "state": "failed", "output": "", "exitCode": None, "cleanupConfirmed": False,
                      "unitOwned": False, "unitProcessesGone": False, "unitMetadataRemoved": False, "rankCleanupConfirmed": False,
                      "authorizationRemoved": False, "agentGone": False, "publicArtifactsRemoved": False, "errorCode": "native_admission_failed"}
    finally:
        if key is not None:
            key._value[:] = b"\0" * len(key._value)
    encoded = adapter.canonical(result)
    if len(encoded) > MAX_OUTPUT:
        result.update(output="", state="failed", errorCode="native_output_limit")
        if "stderr" in result:
            result["stderr"] = ""
        encoded = adapter.canonical(result)
    sys.stdout.buffer.write(encoded + b"\n")


if __name__ == "__main__":
    main()
