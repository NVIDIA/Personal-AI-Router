# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Review/start/status/cancel for one fixed user-owned NCCL build operation.

PAIR authenticates the transport and retains approval authority. This helper
accepts a complete reviewed plan, never shell text or arbitrary destinations.
It emits an inert runtime candidate, not an adopted diagnostic profile.
"""

import json
from contextlib import contextmanager
from datetime import datetime, timezone
import os
from pathlib import Path
import platform
import re
import secrets
import stat
import sys
import time

import diagnostic_tools_remote as inspection
import diagnostic_runtime_build_worker as worker

OWNER = worker.CONTROLLER_OWNER
TOKEN = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}\Z")
HEX32 = re.compile(r"[a-f0-9]{32}\Z")
HEX64 = re.compile(r"[a-f0-9]{64}\Z")
MAX_JSON = 128 * 1024
PROPERTIES = ("Id", "Description", "Transient", "Type", "ExitType", "KillMode", "Restart", "RemainAfterExit",
              "RuntimeMaxUSec", "TimeoutStartUSec", "TimeoutStopUSec", "MainPID", "ControlPID", "InvocationID",
              "ControlGroup", "ActiveState", "SubState", "Result", "ExecMainCode", "ExecMainStatus", "LoadState",
              "CPUQuotaPerSecUSec", "MemoryMax", "TasksMax", "NoNewPrivileges", "UMask", "Slice")


def strict_json(raw):
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise worker.BuildError("duplicate_json_field")
            result[key] = value
        return result
    if len(raw) > MAX_JSON:
        raise worker.BuildError("input_limit")
    try:
        return json.loads(raw, object_pairs_hook=pairs)
    except (ValueError, UnicodeError, RecursionError):
        raise worker.BuildError("invalid_json") from None


def validate(request):
    if not isinstance(request, dict):
        raise worker.BuildError("invalid_request")
    action = request.get("action")
    keys = {"action", "nodeId", "principal", "operationId"}
    keys |= {"approvedPlan"} if action in ("build", "retry") else {"expectedPlanDigest"} if action in ("status", "cancel") else set()
    if action == "retry":
        keys.add("expectedAttempt")
    if action not in ("review", "build", "retry", "status", "cancel") or set(request) != keys:
        raise worker.BuildError("invalid_request")
    if any(not isinstance(request[key], str) or not TOKEN.fullmatch(request[key]) for key in ("nodeId", "principal")):
        raise worker.BuildError("invalid_identity")
    if request["nodeId"] != request["principal"] or not HEX32.fullmatch(str(request["operationId"])):
        raise worker.BuildError("invalid_identity")
    if action in ("status", "cancel") and not HEX64.fullmatch(str(request["expectedPlanDigest"])):
        raise worker.BuildError("invalid_plan_digest")
    if action == "retry" and (type(request["expectedAttempt"]) is not int or not 1 <= request["expectedAttempt"] < worker.LIMITS["maxAttempts"]):
        raise worker.BuildError("invalid_expected_attempt")


def worker_source(native):
    source = getattr(native, "worker_source_bytes", None)
    if source is None:
        source = worker.SHIPPED_SOURCE
    if source is None:
        source = Path(worker.__file__).read_bytes()  # Standalone source package only.
    if type(source) is not bytes or not source or len(source) > MAX_JSON:
        raise worker.BuildError("shipped_worker_source_invalid")
    return source


@contextmanager
def operation_lock(root):
    import fcntl
    fd = os.open(root / "controller.lock", os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0), 0o600)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or (os.name == "posix" and (info.st_uid != os.geteuid() or info.st_mode & 0o077)):
            raise worker.BuildError("operation_lock_invalid")
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield
    finally:
        os.close(fd)


def mkdir_private(path, uid):
    path = Path(path)
    if not path.exists():
        path.mkdir(mode=0o700)
    worker.owned(path, uid, directory=True, private=True)
    return path


def root_path(account, operation, create=False):
    home = Path(account["home"])
    if account["uid"] <= 0 or not re.fullmatch(r"/[A-Za-z0-9_./-]+", str(home)) or ".." in home.parts:
        raise worker.BuildError("unsupported_home")
    worker.owned(home, account["uid"], directory=True)
    current = home
    for part in (".local", "share"):
        current /= part
        if create and not current.exists():
            current.mkdir(mode=0o700)
        if current.exists():
            worker.owned(current, account["uid"], directory=True)
    current /= "pair-nccl-build-v1"
    for path in (current, current / operation):
        if create:
            mkdir_private(path, account["uid"])
        elif path.exists():
            worker.owned(path, account["uid"], directory=True, private=True)
    return current / operation


class Native(inspection.Native):
    def __init__(self, worker_source_bytes=None):
        super().__init__()
        self.deadline = time.monotonic() + 90
        self.worker_source_bytes = worker_source_bytes

    def command(self, argv):
        code, output = self.run(argv)
        if code:
            raise worker.BuildError("prerequisite_command_failed")
        return output

    def tool(self, name, preferred=()):
        return worker.system_hash(self.resolve(name, preferred))

    def user(self, argv, account):
        runtime = Path("/run/user") / str(account["uid"])
        worker.owned(runtime, account["uid"], directory=True, private=True)
        bus = runtime / "bus"
        info = bus.lstat()
        if not stat.S_ISSOCK(info.st_mode) or info.st_uid != account["uid"]:
            raise worker.BuildError("user_manager_unavailable")
        env = self.resolve("env")
        return self.command([env, "XDG_RUNTIME_DIR=" + str(runtime), "DBUS_SESSION_BUS_ADDRESS=unix:path=" + str(bus), *argv])

    def identity(self, request):
        account = self.account()
        root_path(account, request["operationId"])
        public_raw = self.read(str(Path(account["home"]) / ".config/Nvidia Corporation/Personal AI Router/cluster/identity.json"), 8192)
        public = strict_json(public_raw)
        if not isinstance(public, dict) or public.get("node_uuid") != request["principal"]:
            raise worker.BuildError("public_identity_changed")
        return {**account, "nodeId": request["nodeId"], "principal": request["principal"],
                "publicIdentitySha256": worker.hashlib.sha256(public_raw.encode()).hexdigest()}

    def preflight(self, request):
        if platform.system() != "Linux" or platform.machine() != "aarch64":
            raise worker.BuildError("platform_unsupported")
        if inspection.parse_os_release(self.os_release()) != {"ID": "ubuntu", "VERSION_ID": "24.04"}:
            raise worker.BuildError("platform_unsupported")
        identity = self.identity(request)
        root = root_path(identity, request["operationId"])
        tools = {name: self.tool(name) for name in ("git", "make", "g++", "gcc", "ar", "ld", "readelf", "python3", "dpkg-query", "systemd-run", "systemctl", "busctl")}
        tools["nvcc"] = self.tool("nvcc", (worker.CUDA + "/bin/nvcc",))
        if tools["nvcc"]["path"] != str(Path(worker.CUDA + "/bin/nvcc").resolve(strict=True)):
            raise worker.BuildError("explicit_cuda_missing")
        version = self.command([tools["nvcc"]["path"], "--version"])
        codes = self.command([tools["nvcc"]["path"], "--list-gpu-code"]).split()
        if len(re.findall(r"\brelease 13\.0,", version)) != 1 or not codes or not all(re.fullmatch(r"sm_[0-9]+[a-z]?", code) for code in codes) or "sm_121" not in codes:
            raise worker.BuildError("cuda13_sm121_not_confirmed")
        packages = []
        for name in inspection.MPI_PACKAGES:
            output = self.command([tools["dpkg-query"]["path"], "-W", "-f=${Status}\t${Version}\t${Architecture}", name])
            fields = output.split("\t")
            if len(fields) != 3 or fields[0] != "install ok installed" or fields[2] not in ("arm64", "all") or not re.fullmatch(r"(?:[0-9]+:)?4\.1\.[A-Za-z0-9.+:~_-]+", fields[1]):
                raise worker.BuildError("openmpi41_not_installed")
            packages.append({"name": name, "version": fields[1], "architecture": fields[2]})
        if len({item["version"] for item in packages}) != 1:
            raise worker.BuildError("mpi_family_mismatch")
        # Resolve the compiler wrapper for ownership, execute its public name:
        # OpenMPI selects wrapper behavior using argv[0].
        wrapper = Path("/usr/bin/mpicxx")
        worker.trusted(wrapper)
        worker.trusted(Path(worker.MPI_HOME) / "include/mpi.h")
        worker.trusted(Path(worker.MPI_HOME) / "lib/libmpi.so")
        wrapper_show = self.command([str(wrapper), "--showme:version"])
        if "Open MPI 4.1." not in wrapper_show:
            raise worker.BuildError("mpi_wrapper_mismatch")
        libraries = {name: worker.system_hash(path) for name, path in {
            "libmpi.so.40": "/usr/lib/aarch64-linux-gnu/libmpi.so.40",
            "libcudart.so.13": worker.CUDA_LIB + "/libcudart.so.13"}.items()}
        for item in libraries.values():
            worker.elf_header(item["path"])
        self.user([tools["systemctl"]["path"], "--user", "show", "--property=Version", "--value"], identity)
        if self.disk(identity["home"]) < worker.LIMITS["minimumFreeBytes"]:
            raise worker.BuildError("insufficient_build_space")
        return {"identity": identity, "root": str(root), "tools": tools, "mpiPackages": packages,
                "prerequisiteLibraries": libraries, "cudaVersion": version}

    def verify_runtime(self, plan, attempt):
        if worker.system_hash(plan["tools"]["readelf"]["path"]) != plan["tools"]["readelf"]:
            raise worker.BuildError("verification_tool_changed")
        return worker.verify_runtime(worker.Runner(plan, attempt))

    def unit(self, journal):
        plan = journal["plan"]
        ctl = plan["tools"]["systemctl"]["path"]
        output = self.user([ctl, "--user", "show", journal["unit"], "--no-pager", "--property=" + ",".join(PROPERTIES)], plan["identity"])
        info = {}
        for line in output.splitlines():
            key, separator, value = line.partition("=")
            if not separator or key in info or key not in PROPERTIES:
                raise worker.BuildError("unit_observation_malformed")
            info[key] = value
        if info.get("LoadState") == "not-found":
            return None
        bus = plan["tools"]["busctl"]["path"]
        obj = strict_json(self.user([bus, "--user", "--json=short", "call", "org.freedesktop.systemd1",
                                     "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", "GetUnit", "s", journal["unit"]], plan["identity"]))
        if obj.get("type") != "o" or not isinstance(obj.get("data"), list) or len(obj["data"]) != 1:
            raise worker.BuildError("unit_object_unknown")
        address = obj["data"][0]
        if not isinstance(address, str) or not address.startswith("/org/freedesktop/systemd1/unit/"):
            raise worker.BuildError("unit_object_unknown")
        executable = strict_json(self.user([bus, "--user", "--json=short", "get-property", "org.freedesktop.systemd1", address,
                                            "org.freedesktop.systemd1.Service", "ExecStart"], plan["identity"]))
        entries = executable.get("data")
        # get-property renders the variant value directly; method-call replies
        # have a separate outer argument array which is not present here.
        if executable.get("type") != "a(sasbttttuii)" or not isinstance(entries, list) or len(entries) != 1:
            raise worker.BuildError("unit_exec_unknown")
        entry = entries[0]
        if not isinstance(entry, list) or len(entry) != 10 or entry[0] != journal["argv"][0] or entry[1] != journal["argv"] or entry[2] is not False:
            raise worker.BuildError("unit_exec_changed")
        return info

    def absence(self, request, identity):
        """Observe only the three fixed user units; the caller owns the fence lock."""
        operation = request.get("operationId")
        if not isinstance(operation, str) or not HEX32.fullmatch(operation) or type(identity.get("uid")) is not int or identity["uid"] <= 0:
            raise worker.BuildError("absence_identity_invalid")
        names = [f"pair-nccl-build-{operation}-a{number}.service" for number in range(1, 4)]
        try:
            bus = str(worker.trusted(self.resolve("busctl")))
            ctl = str(worker.trusted(self.resolve("systemctl")))
        except (OSError, ValueError):
            raise worker.BuildError("absence_tools_unknown") from None
        prefix = [bus, "--user", "--json=short", "call", "org.freedesktop.systemd1",
                  "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager"]

        def observation(argv):
            try:
                output = self.user(argv, identity)
                if not isinstance(output, str):
                    raise worker.BuildError("absence_observation_malformed")
                return output
            except (OSError, UnicodeError, ValueError):
                raise worker.BuildError("absence_observation_unknown") from None

        def rows(method, signature, maximum, arguments=()):
            result = strict_json(observation([*prefix, method, *arguments]))
            if not isinstance(result, dict) or set(result) != {"type", "data"} or result["type"] != signature:
                raise worker.BuildError("absence_dbus_malformed")
            data = result["data"]
            if not isinstance(data, list) or len(data) != 1 or not isinstance(data[0], list) or len(data[0]) > maximum:
                raise worker.BuildError("absence_dbus_malformed")
            for row in data[0]:
                integer = 7 if method == "ListUnitsByPatterns" else 0
                width = 10 if method == "ListUnitsByPatterns" else 6
                if not isinstance(row, list) or len(row) != width or type(row[integer]) is not int or not 0 <= row[integer] <= 0xffffffff:
                    raise worker.BuildError("absence_dbus_malformed")
                if any(not isinstance(value, str) or len(value) > 4096 or "\x00" in value for index, value in enumerate(row) if index != integer):
                    raise worker.BuildError("absence_dbus_malformed")
                object_fields = (6, 9) if method == "ListUnitsByPatterns" else (4, 5)
                if any(not re.fullmatch(r"/(?:[A-Za-z0-9_]+(?:/[A-Za-z0-9_]+)*)?", row[index]) for index in object_fields):
                    raise worker.BuildError("absence_dbus_malformed")
            return data[0]

        # ListUnitsByNames loads metadata; literal patterns enumerate only the manager's existing unit map.
        units = rows("ListUnitsByPatterns", "a(ssssssouso)", 3, ("asas", "0", "3", *names))
        if any(row[0] not in names for row in units) or len({row[0] for row in units}) != len(units):
            raise worker.BuildError("absence_dbus_malformed")
        jobs = rows("ListJobs", "a(usssoo)", 4096)
        if len({row[0] for row in jobs}) != len(jobs):
            raise worker.BuildError("absence_dbus_malformed")
        raw_group = observation([ctl, "--user", "show", "--property=ControlGroup", "--value", "--", "-.slice"])
        group = raw_group.removesuffix("\n")
        if len(group) > 2048 or not re.fullmatch(r"/[A-Za-z0-9_./:@-]+", group) or len(group.split("/")) > 33 or any(part in ("", ".", "..") for part in group[1:].split("/")):
            raise worker.BuildError("absence_cgroup_invalid")

        directory_flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
        descriptors = []

        def directory(path, parent=None):
            fd = os.open(path, directory_flags, dir_fd=parent)
            descriptors.append(fd)
            if not stat.S_ISDIR(os.fstat(fd).st_mode):
                raise worker.BuildError("absence_cgroup_invalid")
            return fd

        def empty_events(parent):
            fd = os.open("cgroup.events", os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0), dir_fd=parent)
            try:
                if not stat.S_ISREG(os.fstat(fd).st_mode):
                    raise worker.BuildError("absence_cgroup_invalid")
                raw = os.read(fd, 4097)
            finally:
                os.close(fd)
            if len(raw) > 4096:
                raise worker.BuildError("absence_cgroup_invalid")
            entries = [line.split() for line in raw.decode("ascii").splitlines()]
            if any(len(entry) != 2 or not re.fullmatch(r"[a-z_]+", entry[0]) or not re.fullmatch(r"[0-9]{1,20}", entry[1]) for entry in entries):
                raise worker.BuildError("absence_cgroup_invalid")
            values = dict(entries)
            if len(values) != len(entries) or values.get("populated") not in ("0", "1"):
                raise worker.BuildError("absence_cgroup_invalid")
            return values["populated"] == "0"

        try:
            current = directory("/sys/fs/cgroup")
            for part in group[1:].split("/"):
                current = directory(part, current)
            empty_events(current)  # Validate a readable cgroup-v2 manager root before accepting ENOENT below it.
            clear = True
            try:
                app = directory("app.slice", current)
            except FileNotFoundError:
                app = None
            if app is not None:
                for name in names:
                    try:
                        unit = directory(name, app)
                    except FileNotFoundError:
                        continue
                    clear = empty_events(unit) and clear
        except (OSError, UnicodeError, ValueError):
            raise worker.BuildError("absence_cgroup_unknown") from None
        finally:
            for fd in reversed(descriptors):
                os.close(fd)
        return {"unitsAbsent": not units, "jobsAbsent": not any(row[1] in names for row in jobs),
                "cgroupsEmptyOrAbsent": clear, "unitNames": names,
                "userManagerCgroup": group, "observedAt": time.time()}

    def empty(self, info, allow_missing=False):
        group = info.get("ControlGroup", "")
        if not re.fullmatch(r"/[A-Za-z0-9_./:@-]+", group) or ".." in Path(group).parts:
            raise worker.BuildError("cgroup_unknown")
        try:
            fd = os.open("/sys/fs/cgroup" + group + "/cgroup.events", os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
            with os.fdopen(fd, "rb") as stream:
                if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
                    raise worker.BuildError("cleanup_unknown")
                raw = stream.read(4097)
            if len(raw) > 4096:
                raise worker.BuildError("cleanup_unknown")
            entries = [line.split(" ") for line in raw.decode("ascii").splitlines()]
            if any(len(entry) != 2 for entry in entries) or len({entry[0] for entry in entries}) != len(entries):
                raise worker.BuildError("cleanup_unknown")
            populated = dict(entries).get("populated")
            if populated not in ("0", "1"):
                raise worker.BuildError("cleanup_unknown")
            return populated == "0"
        except FileNotFoundError:
            # A completed unit may have removed its cgroup; the exact unit is
            # still observed, with both owned service/control PIDs absent.
            if allow_missing or (info.get("ActiveState") in ("inactive", "failed") and info.get("MainPID") == "0" and info.get("ControlPID") == "0"):
                return True
            raise worker.BuildError("cleanup_unknown") from None
        except (OSError, UnicodeError, ValueError):
            raise worker.BuildError("cleanup_unknown") from None


def plan_for(request, native, source=None):
    facts = native.preflight(request)
    plan = {"schemaVersion": 1, "recipeId": worker.RECIPE, "operationId": request["operationId"], **facts,
            "sources": worker.SOURCES, "vendorPlaybookCommit": "4663a75d67f129eb121b5ae9ddba21d55dee12bf",
            "sourcePairStatus": "PAIR-pinned-not-vendor-validated", "cudaHome": worker.CUDA,
            "mpiHome": worker.MPI_HOME, "gencode": worker.GENCODE, "limits": worker.LIMITS,
            "workerSha256": worker.hashlib.sha256(worker_source(native) if source is None else source).hexdigest(),
            "effects": ["fetch-two-fixed-public-source-commits", "compile-with-two-cpu-jobs",
                        "private-user-owned-attempt-directories", "bounded-transient-user-systemd-unit",
                        "inert-hash-bound-runtime-candidate-descriptor"],
            "unitPolicy": {"Type": "exec", "ExitType": "cgroup", "RuntimeMaxSec": 1800,
                           "TimeoutStartSec": 10, "TimeoutStopSec": 10, "KillMode": "control-group",
                           "Restart": "no", "RemainAfterExit": "yes", "CPUQuota": "200%",
                           "MemoryMax": worker.LIMITS["memoryMaxBytes"], "TasksMax": 256, "Slice": "app.slice"},
            "gpuExecuted": False, "mpiExecuted": False, "managerAdopted": False, "runtimeValidated": False}
    plan["planDigest"] = worker.digest(plan)
    return worker.check_plan(plan)


def owned_unit(journal, info):
    if info is None:
        raise worker.BuildError("unit_missing_cleanup_unknown")
    if not isinstance(info, dict):
        raise worker.BuildError("unit_observation_malformed")
    expected = {"Id": journal["unit"], "Description": journal["description"], "Transient": "yes",
                "Type": "exec", "ExitType": "cgroup", "KillMode": "control-group", "Restart": "no",
                "RemainAfterExit": "yes", "RuntimeMaxUSec": "30min", "TimeoutStartUSec": "10s", "TimeoutStopUSec": "10s"}
    expected.update(CPUQuotaPerSecUSec="2s", MemoryMax=str(worker.LIMITS["memoryMaxBytes"]),
                    TasksMax="256", NoNewPrivileges="yes", UMask="0077", Slice="app.slice")
    if any(info.get(key) != value for key, value in expected.items()) or not HEX32.fullmatch(info.get("InvocationID", "")):
        raise worker.BuildError("unit_ownership_changed")
    if journal.get("invocationId") and info["InvocationID"] != journal["invocationId"]:
        raise worker.BuildError("unit_invocation_changed")
    return info


def save(journal):
    worker.atomic_json(Path(journal["plan"]["root"]) / f"attempt-{journal['attempt']:04d}" / "controller-receipt.json", journal)
    worker.atomic_json(Path(journal["plan"]["root"]) / "operation.json", journal)


def load(request, native):
    account = native.account()
    root = root_path(account, request["operationId"])
    path = worker.owned(root / "operation.json", account["uid"], private=True)
    journal = strict_json(path.read_bytes())
    plan = worker.check_plan(journal.get("plan"))
    if journal.get("owner") != OWNER or plan["operationId"] != request["operationId"] or plan["planDigest"] != request["expectedPlanDigest"]:
        raise worker.BuildError("operation_binding_mismatch")
    if plan["identity"]["uid"] != account["uid"] or plan["identity"]["home"] != account["home"] or plan["identity"]["principal"] != request["principal"]:
        raise worker.BuildError("operation_identity_changed")
    number = journal.get("attempt")
    if type(number) is not int or not 1 <= number <= worker.LIMITS["maxAttempts"] or type(journal.get("cleanupConfirmed")) is not bool:
        raise worker.BuildError("operation_journal_invalid")
    attempt = root / f"attempt-{number:04d}"
    expected_unit = "pair-nccl-build-" + request["operationId"] + f"-a{number}.service"
    expected_description = OWNER + ":" + request["operationId"] + ":" + plan["planDigest"] + ":" + str(account["uid"])
    if journal.get("unit") != expected_unit or journal.get("description") != expected_description or not HEX32.fullmatch(str(journal.get("invocationToken", ""))):
        raise worker.BuildError("operation_journal_invalid")
    argv = [plan["tools"]["python3"]["path"], "-I", str(attempt / "worker.py"), str(attempt / "plan.json"), journal["invocationToken"]]
    if journal.get("argv") != argv or journal.get("state") not in ("building", "built", "failed", "cancelled", "cleanup-unknown"):
        raise worker.BuildError("operation_journal_invalid")
    intent = journal.get("cleanupIntent")
    if intent is not None:
        if not isinstance(intent, dict) or set(intent) != {"unitObservation", "cancelRequested"} or type(intent["cancelRequested"]) is not bool:
            raise worker.BuildError("operation_journal_invalid")
        owned_unit(journal, intent["unitObservation"])
    if journal.get("launchObservation") is not None:
        owned_unit(journal, journal["launchObservation"])
    return journal


def public(journal, action):
    return {"schemaVersion": 1, "action": action, "operationId": journal["plan"]["operationId"],
            "planDigest": journal["plan"]["planDigest"], "state": journal["state"], "attempt": journal["attempt"],
            "cleanupConfirmed": journal["cleanupConfirmed"], "effectsApplied": True,
            "runtimeValidated": False, "managerAdopted": False,
            "artifactsValidated": journal.get("artifactsValidated", False), "artifactObservedAt": journal.get("artifactObservedAt"),
            "cancelRequested": journal.get("cancelRequested", False), "cancellationDurable": journal.get("cancellationDurable", False),
            "absence": journal.get("absence"),
            "registration": journal.get("registration") if journal.get("artifactsValidated") else None,
            "errorCode": journal.get("errorCode")}


def cancellation_record(root, request, identity, digest):
    path = root / "cancellation.json"
    try:
        worker.owned(path, identity["uid"], private=True)
    except FileNotFoundError:
        return None
    value = strict_json(path.read_bytes())
    if (not isinstance(value, dict) or value.get("owner") != OWNER or value.get("schemaVersion") != 1 or
            value.get("operationId") != request["operationId"] or value.get("planDigest") != digest or
            value.get("identity") != identity or value.get("cancelRequested") is not True or
            type(value.get("closedThroughAttempt")) is not int or not 1 <= value["closedThroughAttempt"] <= worker.LIMITS["maxAttempts"] or
            type(value.get("operationClosed")) is not bool):
        raise worker.BuildError("cancellation_fence_invalid")
    return value


def close_launch(root, request, identity, digest, attempt=None):
    prior = cancellation_record(root, request, identity, digest)
    through = worker.LIMITS["maxAttempts"] if attempt is None else attempt
    record = {"schemaVersion": 1, "owner": OWNER, "operationId": request["operationId"], "planDigest": digest,
              "identity": identity, "cancelRequested": True, "closedThroughAttempt": max(through, prior["closedThroughAttempt"] if prior else 0),
              "operationClosed": attempt is None or bool(prior and prior["operationClosed"]), "absence": None}
    # This durable record is checked by both the controller and fixed worker.
    # Individual condition files also prevent delayed systemd jobs from spawning.
    worker.atomic_json(root / "cancellation.json", record)
    for number in range(1, record["closedThroughAttempt"] + 1):
        worker.atomic_json(root / f"cancelled-a{number}.json", record)
    return record


def absent(proof):
    return all(proof.get(key) is True for key in ("unitsAbsent", "jobsAbsent", "cgroupsEmptyOrAbsent"))


def closed_without_journal(root, request, native, identity, record, action):
    proof = native.absence(request, identity)
    # No-journal attempt files contradict a clean never-launched state. Keep
    # the launch fence, but do not manufacture a terminal build/cancel receipt.
    contradictory = any((root / f"attempt-{n:04d}").exists() for n in range(1, worker.LIMITS["maxAttempts"] + 1))
    clean = absent(proof) and not contradictory
    effects_unknown = contradictory or not absent(proof)
    return {"schemaVersion": 1, "action": action, "operationId": request["operationId"], "planDigest": record["planDigest"],
            "state": "cancelled" if clean else "cleanup-unknown", "attempt": 0, "cleanupConfirmed": clean,
            "cancelRequested": True, "cancellationDurable": True, "operationClosed": True,
            "effectsApplied": None if effects_unknown else False, "effectsUnknown": effects_unknown,
            "artifactsValidated": False, "artifactObservedAt": None, "runtimeValidated": False, "managerAdopted": False,
            "registration": None, "absence": proof,
            "errorCode": None if clean else "operation_journal_missing_with_attempts" if contradictory else "absence_not_confirmed"}


def validate_built(journal, native):
    if journal["state"] != "built":
        return journal
    journal["artifactsValidated"] = False
    journal["artifactObservedAt"] = datetime.now(timezone.utc).isoformat()
    try:
        attempt = Path(journal["plan"]["root"]) / f"attempt-{journal['attempt']:04d}"
        candidate = native.verify_runtime(journal["plan"], attempt)
        candidate["attempt"], candidate["invocationId"] = journal["attempt"], journal["invocationId"]
        if candidate != journal.get("registration"):
            raise worker.BuildError("stored_runtime_changed")
        journal["artifactsValidated"] = True
    except (worker.BuildError, OSError, ValueError) as error:
        journal["state"], journal["errorCode"] = "failed", getattr(error, "code", "stored_runtime_unavailable")
        journal["rejectedRegistration"] = journal.get("registration")
        journal["registration"] = None
    save(journal)
    return journal


def original_cgroup_empty(journal, native):
    if not journal.get("invocationId"):
        return True  # Pre-unit closure uses the exact unit/job/app-slice fence.
    groups = set()
    for original in (journal.get("launchObservation"), journal.get("cleanupIntent", {}).get("unitObservation")):
        if original is None:
            continue
        owned_unit(journal, original)
        group = original.get("ControlGroup")
        if not group:
            if not terminal_unit(original):
                raise worker.BuildError("cgroup_unknown")
            continue  # A terminal snapshot must not hide the original launch group.
        if group not in groups and not native.empty(original, allow_missing=True):
            return False
        groups.add(group)
    return bool(groups)


def terminal_unit(info):
    terminal = info.get("ActiveState") in ("inactive", "failed") or (info.get("ActiveState"), info.get("SubState")) == ("active", "exited")
    return terminal and info.get("MainPID") == "0" and info.get("ControlPID") == "0"


def worker_result(journal):
    """Retain the original bound failure before systemd clears failed metadata."""
    filename = Path(journal["plan"]["root"]) / f"attempt-{journal['attempt']:04d}" / "worker-result.json"
    if not filename.exists():
        return None, None
    try:
        worker.owned(filename, journal["plan"]["identity"]["uid"], private=True)
        with open(filename, "rb") as stream:
            result = strict_json(stream.read(MAX_JSON + 1))
        if not isinstance(result, dict) or result.get("operationId") != journal["plan"]["operationId"] or result.get("planDigest") != journal["plan"]["planDigest"] or result.get("invocationToken") != journal["invocationToken"]:
            raise worker.BuildError("worker_result_unbound")
        if result.get("state") == "failed" and isinstance(result.get("errorCode"), str) and re.fullmatch(r"[a-z][a-z0-9_]{0,127}", result["errorCode"]):
            journal["workerFailure"] = {key: result[key] for key in ("operationId", "planDigest", "invocationToken", "errorCode")}
        return result, None
    except (worker.BuildError, OSError, ValueError) as error:
        return None, getattr(error, "code", "worker_result_invalid")


def cancelled_outcome(journal):
    failure = journal.get("workerFailure", {})
    if (isinstance(failure, dict) and failure.get("operationId") == journal["plan"]["operationId"] and
            failure.get("planDigest") == journal["plan"]["planDigest"] and failure.get("invocationToken") == journal["invocationToken"] and
            isinstance(failure.get("errorCode"), str) and re.fullmatch(r"[a-z][a-z0-9_]{0,127}", failure["errorCode"])):
        journal["state"], journal["errorCode"] = "failed", failure["errorCode"]
    else:
        journal["state"], journal["errorCode"] = "cancelled", None


def observe(journal, native, cancel=False):
    explicit_cancel = cancel
    info = native.unit(journal)
    intent = journal.get("cleanupIntent")
    if info is None:
        if not intent:
            raise worker.BuildError("unit_missing_cleanup_unknown")
        # Stop intent carries the exact observed invocation and cgroup across a
        # lost SSH reply or controller death. Never infer cleanup from PID loss.
        info = owned_unit(journal, intent["unitObservation"])
        unit_present = False
    else:
        info = owned_unit(journal, info)
        unit_present = True
    if not journal.get("invocationId"):
        journal["invocationId"] = info["InvocationID"]
        journal["launchObservation"] = info
        save(journal)
    cancel = cancel or journal.get("cancelRequested", False) or bool(intent and intent["cancelRequested"])
    finished = bool(intent) or info.get("ActiveState") in ("inactive", "failed") or (info.get("ActiveState"), info.get("SubState")) == ("active", "exited")
    if not cancel and not finished:
        return journal
    if unit_present and not explicit_cancel:
        # Status can retire only completed, empty service metadata. A saved
        # cancellation intent never authorizes a status request to stop work.
        if not terminal_unit(info):
            return journal
        if not (native.empty(info) if info.get("ControlGroup") else original_cgroup_empty(journal, native)):
            return journal
        proof = native.absence({"operationId": journal["plan"]["operationId"]}, journal["plan"]["identity"])
        if proof.get("jobsAbsent") is not True:
            return journal
    attempt = Path(journal["plan"]["root"]) / f"attempt-{journal['attempt']:04d}"
    result, result_error = worker_result(journal)
    journal["cleanupConfirmed"] = False
    if "jobResult" not in journal:
        recorded = intent["unitObservation"] if intent else info
        journal["jobResult"] = {key: recorded.get(key) for key in ("Result", "ExecMainCode", "ExecMainStatus")}
    if not intent:
        journal["cleanupIntent"] = {"unitObservation": info, "cancelRequested": cancel}
    elif cancel:
        journal["cleanupIntent"]["cancelRequested"] = True
    save(journal)  # Recovery evidence precedes the stop/collection boundary.
    if unit_present:
        native.user([journal["plan"]["tools"]["systemctl"]["path"], "--user", "stop", journal["unit"]], journal["plan"]["identity"])
    after = native.unit(journal) if unit_present else None
    if after is not None:
        owned_unit(journal, after)
        if after.get("ActiveState") == "failed" and terminal_unit(after):
            proof = native.absence({"operationId": journal["plan"]["operationId"]}, journal["plan"]["identity"])
            current_clear = native.empty(after) if after.get("ControlGroup") else original_cgroup_empty(journal, native)
            if proof.get("jobsAbsent") is True and proof.get("cgroupsEmptyOrAbsent") is True and current_clear and original_cgroup_empty(journal, native):
                # CollectMode=inactive retains FAILED units after stop. Reset
                # only this freshly verified invocation, after saving its error.
                refreshed = owned_unit(journal, native.unit(journal))
                if refreshed != after:
                    raise worker.BuildError("unit_changed_before_collection")
                native.user([journal["plan"]["tools"]["systemctl"]["path"], "--user", "reset-failed", journal["unit"]], journal["plan"]["identity"])
                after = native.unit(journal)
                if after is not None:
                    owned_unit(journal, after)
    proof = native.absence({"operationId": journal["plan"]["operationId"]}, journal["plan"]["identity"])
    journal["absence"] = proof
    journal["cleanupConfirmed"] = after is None and absent(proof) and original_cgroup_empty(journal, native)
    if not journal["cleanupConfirmed"]:
        journal["state"], journal["errorCode"] = "cleanup-unknown", "cleanup_unknown"
    elif cancel:
        cancelled_outcome(journal)
    elif result and result.get("state") == "built" and journal["jobResult"] == {"Result": "success", "ExecMainCode": "1", "ExecMainStatus": "0"}:
        # Re-hash and re-inspect every ELF after the owned cgroup is empty.
        try:
            registration = native.verify_runtime(journal["plan"], attempt)
            if registration != result.get("registration"):
                raise worker.BuildError("runtime_changed_after_build")
            registration["attempt"] = journal["attempt"]
            registration["invocationId"] = journal["invocationId"]
            worker.atomic_json(attempt / "runtime-registration.json", registration)
            journal["registration"], journal["state"], journal["errorCode"] = registration, "built", None
            journal["artifactsValidated"], journal["artifactObservedAt"] = True, datetime.now(timezone.utc).isoformat()
        except (worker.BuildError, OSError, ValueError) as error:
            journal["state"], journal["errorCode"] = "failed", getattr(error, "code", "runtime_verification_failed")
    else:
        journal["state"], journal["errorCode"] = "failed", result.get("errorCode") if result else result_error or "worker_result_missing"
    save(journal)
    return journal


def start(request, native):
    approved = worker.check_plan(request["approvedPlan"])
    account = native.account()
    if (approved["operationId"] != request["operationId"] or approved["identity"]["principal"] != request["principal"] or
            any(approved["identity"][key] != account[key] for key in ("uid", "home"))):
        raise worker.BuildError("operation_identity_changed")
    if native.identity(request) != approved["identity"]:
        raise worker.BuildError("operation_identity_changed")
    root = root_path(account, request["operationId"], create=True)
    action = request["action"]
    with operation_lock(root):
        fence = cancellation_record(root, request, approved["identity"], approved["planDigest"])
        prior = None
        if (root / "operation.json").exists():
            prior = load({**request, "expectedPlanDigest": approved["planDigest"]}, native)
            if action == "build":
                if not prior["cleanupConfirmed"]:
                    prior = observe(prior, native)
                return public(validate_built(prior, native), action)
            if request["expectedAttempt"] != prior["attempt"]:
                raise worker.BuildError("expected_attempt_changed")
            if prior["state"] not in ("failed", "cancelled") or not prior["cleanupConfirmed"]:
                raise worker.BuildError("attempt_not_retryable")
        elif action == "retry":
            raise worker.BuildError("operation_journal_missing")
        elif fence:
            return closed_without_journal(root, request, native, approved["identity"], fence, action)
        number = 1 if prior is None else prior["attempt"] + 1
        if number > worker.LIMITS["maxAttempts"]:
            raise worker.BuildError("attempt_limit")
        if fence and (fence["operationClosed"] or fence["closedThroughAttempt"] >= number):
            raise worker.BuildError("operation_launch_closed")
        source = worker_source(native)
        if plan_for(request, native, source=source) != approved or worker.hashlib.sha256(source).hexdigest() != approved["workerSha256"]:
            raise worker.BuildError("reviewed_prerequisites_changed")
        proof = native.absence(request, approved["identity"])
        if not absent(proof):
            raise worker.BuildError("launch_absence_not_confirmed")
        attempt = mkdir_private(root / f"attempt-{number:04d}", approved["identity"]["uid"])
        if list(attempt.iterdir()):
            raise worker.BuildError("attempt_not_empty")
        worker.atomic_json(attempt / "plan.json", approved)
        fd = os.open(attempt / "worker.py", os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
        with os.fdopen(fd, "wb") as stream:
            stream.write(source)
            stream.flush()
            os.fsync(stream.fileno())
        token = secrets.token_hex(16)
        argv = [approved["tools"]["python3"]["path"], "-I", str(attempt / "worker.py"), str(attempt / "plan.json"), token]
        journal = {"schemaVersion": 1, "owner": OWNER, "plan": approved, "state": "building", "attempt": number,
                   "cleanupConfirmed": False, "invocationToken": token, "invocationId": None,
                   "unit": "pair-nccl-build-" + request["operationId"] + f"-a{number}.service",
                   "description": OWNER + ":" + request["operationId"] + ":" + approved["planDigest"] + ":" + str(approved["identity"]["uid"]),
                   "argv": argv, "registration": None, "errorCode": None, "artifactsValidated": False,
                   "cancelRequested": False, "cancellationDurable": False, "launchAbsence": proof}
        save(journal)  # Durable ownership intent precedes service start.
        command = [approved["tools"]["systemd-run"]["path"], "--user", "--unit=" + journal["unit"], "--description=" + journal["description"],
                   "--service-type=exec", "--property=StandardInput=null", "--property=StandardOutput=null", "--property=StandardError=null",
                   "--property=UMask=0077", "--property=NoNewPrivileges=yes", "--property=KillSignal=SIGTERM",
                   "--property=SendSIGKILL=yes", "--property=CollectMode=inactive",
                   "--property=ConditionPathExists=!" + str(root / f"cancelled-a{number}.json")]
        command.extend("--property=" + key + "=" + str(value) for key, value in approved["unitPolicy"].items() if key != "Type")
        command.extend(["--", *argv])
        native.user(command, approved["identity"])
        info = owned_unit(journal, native.unit(journal))
        journal["invocationId"] = info["InvocationID"]
        journal["launchObservation"] = info
        save(journal)
        return public(journal, action)


def cancel_owned(request, native, root, identity, journal):
    digest = request["expectedPlanDigest"]
    if journal and identity != journal["plan"]["identity"]:
        raise worker.BuildError("operation_identity_changed")
    fence = close_launch(root, request, identity, digest, journal["attempt"] if journal else None)
    if journal is None:
        return closed_without_journal(root, request, native, identity, fence, "cancel")
    journal["cancelRequested"], journal["cancellationDurable"] = True, True
    journal["artifactsValidated"] = False
    worker_result(journal)
    save(journal)
    proof = native.absence(request, identity)
    if not absent(proof):
        try:
            journal = observe(journal, native, cancel=True)
        except (worker.BuildError, inspection.ProbeError, OSError, ValueError) as error:
            journal["cleanupConfirmed"], journal["state"] = False, "cleanup-unknown"
            journal["errorCode"] = getattr(error, "code", "owned_cancel_unknown")
        proof = native.absence(request, identity)
    try:
        original_clear = original_cgroup_empty(journal, native)
    except (worker.BuildError, inspection.ProbeError, OSError, ValueError) as error:
        original_clear = False
        journal["errorCode"] = journal.get("errorCode") or getattr(error, "code", "original_cgroup_unknown")
    journal["absence"], journal["cleanupConfirmed"] = proof, absent(proof) and original_clear
    if journal["cleanupConfirmed"]:
        cancelled_outcome(journal)
        journal["registration"] = None
    else:
        journal["state"] = "cleanup-unknown"
        journal["errorCode"] = journal.get("errorCode") or "absence_not_confirmed"
    save(journal)
    return public(journal, "cancel")


def execute(request, native=None):
    validate(request)
    native = native or Native()
    action = request["action"]
    if action == "review":
        return {"schemaVersion": 1, "action": action, "state": "reviewed", "effectsApplied": False,
                "canBuild": True, "runtimeValidated": False, "plan": plan_for(request, native)}
    if action in ("build", "retry"):
        return start(request, native)
    account = native.account()
    root = root_path(account, request["operationId"], create=action == "cancel")
    if not root.exists():
        raise worker.BuildError("operation_journal_missing")
    with operation_lock(root):
        journal = load(request, native) if (root / "operation.json").exists() else None
        identity = native.identity(request)
        if action == "cancel":
            return cancel_owned(request, native, root, identity, journal)
        if journal is not None and identity != journal["plan"]["identity"]:
            raise worker.BuildError("operation_identity_changed")
        fence = cancellation_record(root, request, identity, request["expectedPlanDigest"])
        if journal is None:
            if fence:
                return closed_without_journal(root, request, native, identity, fence, action)
            raise worker.BuildError("operation_journal_missing")
        if fence and fence["closedThroughAttempt"] >= journal["attempt"]:
            proof = native.absence(request, identity)
            journal["absence"], journal["cancelRequested"], journal["cancellationDurable"] = proof, True, True
            worker_result(journal)
            if not absent(proof):
                try:
                    journal = observe(journal, native)
                except (worker.BuildError, inspection.ProbeError, OSError, ValueError) as error:
                    journal["cleanupConfirmed"], journal["state"] = False, "cleanup-unknown"
                    journal["errorCode"] = getattr(error, "code", "owned_cancel_unknown")
                proof = native.absence(request, identity)
                journal["absence"] = proof
            try:
                journal["cleanupConfirmed"] = absent(proof) and original_cgroup_empty(journal, native)
            except (worker.BuildError, inspection.ProbeError, OSError, ValueError):
                journal["cleanupConfirmed"] = False
            if journal["cleanupConfirmed"]:
                cancelled_outcome(journal)
            else:
                journal["state"] = "cleanup-unknown"
            journal["registration"], journal["artifactsValidated"] = None, False
            if not journal["cleanupConfirmed"]:
                journal["errorCode"] = journal.get("errorCode") or "absence_not_confirmed"
            save(journal)
            return public(journal, action)
        if journal["cleanupConfirmed"] and journal["state"] in ("built", "cancelled", "failed"):
            return public(validate_built(journal, native), action)
        return public(observe(journal, native), action)


def main():
    request = {}
    try:
        request = strict_json(sys.stdin.buffer.read(MAX_JSON + 1))
        result = execute(request)
    except (worker.BuildError, inspection.ProbeError, OSError, ValueError, KeyError, TypeError) as error:
        action = request.get("action") if isinstance(request, dict) else None
        if not isinstance(action, str) or action not in ("review", "build", "retry", "status", "cancel"):
            action = None
        operation = request.get("operationId") if isinstance(request, dict) else None
        if not isinstance(operation, str) or not HEX32.fullmatch(operation):
            operation = None
        result = {"schemaVersion": 1, "action": action,
                  "operationId": operation,
                  "state": "blocked" if action == "review" else "unknown", "cleanupConfirmed": False, "runtimeValidated": False,
                  "effectsUnknown": action != "review",
                  "cancelRequested": action == "cancel", "cancellationDurable": None,
                  "artifactsValidated": False, "artifactObservedAt": None, "registration": None, "absence": None,
                  "errorCode": getattr(error, "code", "native_observation_failed")}
    sys.stdout.write(json.dumps(result, separators=(",", ":")) + "\n")


if __name__ == "__main__":
    main()
