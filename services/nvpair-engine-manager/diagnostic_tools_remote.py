# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Fixed read-only native NCCL prerequisites. One bounded JSON request on stdin.

PAIR supplies authenticated, host-pinned SSH transport; this helper neither opens
SSH connections nor reads private identities. It does not install or run NCCL.
"""

import csv
import ctypes
import hashlib
import io
import ipaddress
import json
import os
import platform
import re
import selectors
import shutil
import signal
import stat
import subprocess
import sys
import time
from pathlib import Path

MAX_INPUT = 16 * 1024
MAX_OUTPUT = 96 * 1024
MPI_PACKAGES = ("libopenmpi-dev", "openmpi-bin", "libopenmpi3t64", "openmpi-common")
TOKEN = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}\Z")
CUDA_PATHS = ("/usr/local/cuda-13.0/bin/nvcc", "/usr/local/cuda/bin/nvcc")


def probe_parent_death(expected_parent):
    """Linux child hook: cover SIGKILL and parent death during process creation."""
    libc = ctypes.CDLL(None, use_errno=True)
    if libc.prctl(1, signal.SIGKILL, 0, 0, 0) != 0:  # PR_SET_PDEATHSIG
        os._exit(125)
    # prctl cannot retroactively report a death before it was armed.
    if os.getppid() != expected_parent:
        os._exit(125)


class ProbeError(Exception):
    def __init__(self, code, message):
        super().__init__(message)
        self.code = code


def decode(raw):
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise ProbeError("invalid_request", "Duplicate JSON field")
            result[key] = value
        return result

    if len(raw) > MAX_INPUT:
        raise ProbeError("invalid_request", "Request exceeds 16 KiB")
    try:
        return json.loads(raw, object_pairs_hook=pairs)
    except (ValueError, UnicodeError, RecursionError):
        raise ProbeError("invalid_request", "Malformed JSON request") from None


def validate(request):
    allowed = {"action", "operationId", "nodeId", "principal", "peerAddress"}
    if not isinstance(request, dict) or set(request) - allowed:
        raise ProbeError("invalid_request", "Unexpected request fields")
    if request.get("action") != "inspect":
        raise ProbeError("unsupported_action", "This helper supports inspection only; no changes were applied")
    for key in ("nodeId", "principal"):
        if not isinstance(request.get(key), str) or not TOKEN.fullmatch(request[key]):
            raise ProbeError("invalid_request", "Invalid paired identity")
    if request["nodeId"] != request["principal"]:
        raise ProbeError("identity_mismatch", "This recipe requires matching PAIR node and principal identities")
    if "operationId" in request and (not isinstance(request["operationId"], str) or
                                    not re.fullmatch(r"[a-f0-9]{32}", request["operationId"])):
        raise ProbeError("invalid_request", "Invalid operation identifier")
    try:
        if not isinstance(request.get("peerAddress"), str):
            raise ValueError()
        peer = ipaddress.IPv4Address(request["peerAddress"])
        if peer.is_loopback or peer.is_multicast or peer.is_unspecified or int(peer) == 0xFFFFFFFF:
            raise ValueError()
    except (KeyError, ValueError, TypeError, ipaddress.AddressValueError):
        raise ProbeError("invalid_request", "A concrete peer IPv4 address is required") from None


class Native:
    """The small I/O boundary is replaceable by Windows-safe test fixtures."""

    def __init__(self):
        self.deadline = time.monotonic() + 30
        self.cancelled = False

    def account(self):
        import pwd
        uid = os.geteuid()
        user = pwd.getpwuid(uid)
        return {"uid": uid, "home": user.pw_dir, "user": user.pw_name}

    def read(self, path, limit=65536):
        try:
            fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
            with os.fdopen(fd, "rb") as stream:
                if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
                    raise ProbeError("file_unavailable", "Required observation is not a regular file")
                data = stream.read(limit + 1)
            if len(data) > limit:
                raise ProbeError("output_limit", "Required observation exceeds its size limit")
            return data.decode("utf-8", errors="strict")
        except (OSError, UnicodeError):
            raise ProbeError("file_unavailable", "Required observation is missing or unreadable") from None

    def os_release(self):
        """Accept Ubuntu's standard OS metadata link, never arbitrary symlinks."""
        try:
            path = "/etc/os-release"
            info = os.lstat(path)
            parents = ["/", "/etc"]
            if stat.S_ISLNK(info.st_mode):
                if info.st_uid != 0 or os.readlink(path) not in ("../usr/lib/os-release", "/usr/lib/os-release"):
                    raise ProbeError("os_metadata_unknown", "OS metadata link is not the trusted standard location")
                path = "/usr/lib/os-release"
                parents.extend(["/usr", "/usr/lib"])
            for parent in parents:
                info = os.lstat(parent)
                if not stat.S_ISDIR(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022:
                    raise ProbeError("os_metadata_unknown", "OS metadata parent directory is not safely owned")
            fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
            with os.fdopen(fd, "rb") as stream:
                info = os.fstat(stream.fileno())
                if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or stat.S_IMODE(info.st_mode) != 0o644:
                    raise ProbeError("os_metadata_unknown", "OS metadata must be a root-owned regular 0644 file")
                data = stream.read(8193)
            if len(data) > 8192:
                raise ProbeError("os_metadata_unknown", "OS metadata exceeds its size limit")
            return data.decode("utf-8", errors="strict")
        except (OSError, UnicodeError):
            raise ProbeError("os_metadata_unknown", "Trusted OS metadata is unavailable or unreadable") from None

    def resolve(self, name, preferred=()):
        candidates = list(preferred)
        found = shutil.which(name)
        if found:
            candidates.append(found)
        for path in candidates:
            if os.path.isfile(path) and os.access(path, os.X_OK):
                resolved = os.path.realpath(path)
                # Do not execute a user-managed tool or container-provided shim
                # merely because an inherited PATH advertises it.
                if not resolved.startswith(("/usr/", "/bin/", "/sbin/")):
                    continue
                current = Path(resolved)
                trusted = True
                while str(current) != "/":
                    info = current.stat()
                    if info.st_uid != 0 or info.st_mode & 0o022:
                        trusted = False
                        break
                    current = current.parent
                if trusted:
                    return resolved
        raise ProbeError("tool_missing", "Required system tool is missing or not safely owned: " + name)

    def disk(self, home):
        return shutil.disk_usage(home).free

    def run(self, argv):
        if self.cancelled:
            raise ProbeError("inspection_cancelled", "Native inspection was cancelled")
        remaining = min(5, self.deadline - time.monotonic())
        if remaining <= 0:
            raise ProbeError("inspection_timeout", "Native inspection exceeded its overall deadline")
        env = {"PATH": "/usr/bin:/bin", "LANG": "C", "LC_ALL": "C", "HOME": "/nonexistent"}
        previous = {}

        def interrupted(_number, _frame):
            # Do not raise during Popen construction: its child might exist
            # before Popen has returned the process handle to this helper.
            self.cancelled = True

        child = None
        data = bytearray()
        deadline = time.monotonic() + remaining
        completed = False
        try:
            for name in ("SIGTERM", "SIGHUP"):
                number = getattr(signal, name, None)
                if number is not None:
                    previous[number] = signal.getsignal(number)
                    signal.signal(number, interrupted)
            if self.cancelled:
                raise ProbeError("inspection_cancelled", "Native inspection was cancelled")
            parent_pid = os.getpid()
            child = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                     stderr=subprocess.DEVNULL, env=env, start_new_session=True,
                                     preexec_fn=lambda: probe_parent_death(parent_pid))
            with selectors.DefaultSelector() as selector:
                selector.register(child.stdout, selectors.EVENT_READ)
                while selector.get_map():
                    if self.cancelled:
                        raise ProbeError("inspection_cancelled", "Native inspection was cancelled")
                    if time.monotonic() >= deadline:
                        raise ProbeError("probe_timeout", "Native observation timed out")
                    for key, _ in selector.select(0.1):
                        part = os.read(key.fd, 4096)
                        if not part:
                            selector.unregister(key.fileobj)
                        data.extend(part)
                        if len(data) > 16384:
                            raise ProbeError("output_limit", "Native observation exceeded 16 KiB")
            child.wait(timeout=max(0.01, deadline - time.monotonic()))
            if self.cancelled:
                raise ProbeError("inspection_cancelled", "Native inspection was cancelled")
            completed = True
            return child.returncode, data.decode("utf-8", errors="replace").strip()
        except subprocess.TimeoutExpired:
            raise ProbeError("probe_timeout", "Native observation did not exit") from None
        finally:
            try:
                if child is not None:
                    if not completed:
                        # On read/output cancellation the child is unreaped,
                        # retaining its PID while its process group is killed.
                        if child.returncode is None:
                            try:
                                os.killpg(child.pid, signal.SIGKILL)
                            except ProcessLookupError:
                                pass
                        child.wait(timeout=2)
                    child.stdout.close()
            finally:
                for number, handler in previous.items():
                    signal.signal(number, handler)


def parse_os_release(text):
    result = {}
    for line in text.splitlines():
        key, separator, value = line.partition("=")
        if separator and key in ("ID", "VERSION_ID"):
            if key in result:
                raise ProbeError("platform_unknown", "Duplicate platform metadata")
            result[key] = value.strip().strip('"')
    if set(result) != {"ID", "VERSION_ID"} or not all(result.values()):
        raise ProbeError("platform_unknown", "Distribution identity or version metadata is missing")
    return result


def inspect(request, native=None):
    validate(request)
    native = native or Native()
    result = {"schemaVersion": 1, "action": "inspect", "state": "inspected",
              "effectsApplied": False, "executable": False, "identity": {},
              "observations": {}, "errors": [], "plan": None}
    if "operationId" in request:
        result["operationId"] = request["operationId"]

    def observe(phase, fn):
        try:
            return fn()
        except ProbeError as error:
            result["errors"].append({"code": error.code, "phase": phase, "message": str(error)})
        except (OSError, ValueError, KeyError, TypeError, OverflowError):
            result["errors"].append({"code": "observation_failed", "phase": phase,
                                     "message": "Native observation is unavailable or malformed"})
        return None

    def command(name, arguments=(), preferred=()):
        path = native.resolve(name, preferred)
        code, output = native.run([path, *arguments])
        if code:
            raise ProbeError("command_failed", name + " observation exited with status " + str(code))
        return {"path": path, "output": output}

    facts = result["observations"]
    facts["platform"] = {"os": platform.system(), "arch": platform.machine()}
    if facts["platform"] != {"os": "Linux", "arch": "aarch64"}:
        result["errors"].append({"code": "platform_unsupported", "phase": "platform",
                                 "message": "This native recipe requires Linux ARM64"})
        result["state"] = "blocked"
        return result
    distro = observe("platform", lambda: parse_os_release(native.os_release()))
    facts["platform"]["distribution"] = distro
    if distro is not None and distro != {"ID": "ubuntu", "VERSION_ID": "24.04"}:
        result["errors"].append({"code": "platform_unsupported", "phase": "platform",
                                 "message": "This recipe requires observed Ubuntu 24.04"})
    account = observe("identity", native.account)
    if not account or account["uid"] <= 0 or not account["home"].startswith("/") or account["home"] == "/":
        result["errors"].append({"code": "account_unsupported", "phase": "identity",
                                 "message": "Inspection requires the selected normal user account and home"})
        result["state"] = "blocked"
        return result
    result["identity"] = dict(account)
    root = os.path.join(account["home"], ".config", "Nvidia Corporation", "Personal AI Router")

    def identity():
        path = os.path.join(root, "cluster", "identity.json")
        raw = native.read(path, 8192)
        public = decode(raw)
        if not isinstance(public, dict) or public.get("node_uuid") != request["principal"]:
            raise ProbeError("identity_mismatch", "Existing public PAIR identity does not match the selected participant")
        return {"nodeId": public["node_uuid"], "principal": public["node_uuid"],
                "publicIdentitySha256": hashlib.sha256(raw.encode()).hexdigest()}

    public = observe("identity", identity)
    if public:
        result["identity"].update(public)
    facts["debianArchitecture"] = observe("platform", lambda: command("dpkg", ["--print-architecture"]))
    if not facts["debianArchitecture"] or facts["debianArchitecture"]["output"] != "arm64":
        result["errors"].append({"code": "architecture_unknown", "phase": "platform",
                                 "message": "Debian architecture arm64 was not confirmed"})
    facts["cuda"] = observe("cuda", lambda: command("nvcc", ["--version"], CUDA_PATHS))
    if facts["cuda"]:
        codes = observe("cuda", lambda: command("nvcc", ["--list-gpu-code"], CUDA_PATHS))
        facts["cuda"]["gpuCodes"] = codes["output"].splitlines() if codes else None
        tokens = codes["output"].split() if codes else []
        valid_codes = bool(tokens) and all(re.fullmatch(r"sm_[0-9]+[a-z]?", token) for token in tokens)
        facts["cuda"]["sm121"] = ("sm_121" in tokens) if valid_codes else None
        versions = re.findall(r"\brelease ([0-9]+)\.([0-9]+),", facts["cuda"]["output"])
        facts["cuda"]["cuda13"] = (versions[0][0] == "13") if len(versions) == 1 else None
        if codes is not None and not valid_codes:
            result["errors"].append({"code": "cuda_capability_unknown", "phase": "cuda",
                                     "message": "Compiler GPU-code output was empty or unrecognized"})
        if facts["cuda"]["cuda13"] is None:
            result["errors"].append({"code": "cuda_version_unknown", "phase": "cuda",
                                     "message": "CUDA compiler version output was unrecognized"})
        if facts["cuda"]["sm121"] is False or facts["cuda"]["cuda13"] is False:
            result["errors"].append({"code": "cuda_unsupported", "phase": "cuda",
                                     "message": "Observed compiler version or GPU-code support does not meet CUDA 13 and sm_121 requirements"})
    facts["tools"] = {}
    for name, arguments in (("python3", ["--version"]), ("git", ["--version"]),
                            ("make", ["--version"]), ("g++", ["-dumpfullversion"])):
        facts["tools"][name] = observe("tools", lambda n=name, a=arguments: command(n, a))
    facts["mpi"] = {"packages": []}
    for name in MPI_PACKAGES:
        package = {"name": name, "status": "unknown", "version": None, "architecture": None}

        def package_status():
            path = native.resolve("dpkg-query")
            code, output = native.run([path, "-W", "-f=${Status}\t${Version}\t${Architecture}", name])
            if code == 1 and not output:
                return {"status": "missing", "version": None, "architecture": None}
            if code:
                raise ProbeError("package_query_failed", "Package status query failed: " + name)
            fields = output.split("\t")
            if len(fields) != 3 or "\n" in output or not re.fullmatch(
                    r"(?:unknown|install|hold|deinstall|purge) (?:ok|reinstreq) (?:not-installed|config-files|half-installed|unpacked|half-configured|triggers-awaited|triggers-pending|installed)", fields[0]) or not re.fullmatch(
                    r"[0-9][A-Za-z0-9.+:~_-]{0,127}", fields[1]) or not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,31}", fields[2]):
                raise ProbeError("package_status_unknown", "Malformed package status: " + name)
            return {"status": fields[0], "version": fields[1], "architecture": fields[2]}

        observed = observe("mpi", package_status)
        if observed:
            package.update(observed)
        facts["mpi"]["packages"].append(package)
    packages = facts["mpi"]["packages"]
    missing = any(p["status"] == "missing" for p in packages)
    unknown = any(p["status"] == "unknown" for p in packages)
    facts["mpi"]["matchingFamily"] = (False if missing else None if unknown else
        all(p["status"] == "install ok installed" and p["architecture"] in ("arm64", "all") for p in packages)
        and len({p["version"] for p in packages}) == 1)
    if missing:
        result["errors"].append({"code": "mpi_prerequisites_missing", "phase": "mpi",
                                 "message": "At least one required OpenMPI package was reported missing; package acquisition is not inspected"})
    elif facts["mpi"]["matchingFamily"] is False:
        result["errors"].append({"code": "mpi_prerequisites_incompatible", "phase": "mpi",
                                 "message": "Observed MPI package states, versions or architectures do not form the required installed family"})
    facts["mpi"]["runtime"] = observe("mpi", lambda: command("mpirun", ["--version"]))
    facts["mpi"]["info"] = observe("mpi", lambda: command("ompi_info", ["--version"]))

    def gpu_query():
        found = command("nvidia-smi", ["--query-gpu=uuid,name,utilization.gpu", "--format=csv,noheader,nounits"])
        rows = list(csv.reader(io.StringIO(found["output"])))
        if len(rows) != 1 or len(rows[0]) != 3:
            raise ProbeError("gpu_unknown", "Exactly one GPU with UUID, name and utilization must be observed")
        uuid, name, utilization = [x.strip() for x in rows[0]]
        if not uuid.startswith("GPU-") or not TOKEN.fullmatch(uuid) or not name:
            raise ProbeError("gpu_unknown", "GPU identity is malformed")
        value = int(utilization) if utilization.isdigit() else None
        if value is not None and not 0 <= value <= 100:
            raise ProbeError("gpu_unknown", "GPU utilization is invalid")
        return {"path": found["path"], "uuid": uuid, "name": name, "utilizationPercent": value,
                "gb10Observed": "GB10" in name}

    facts["gpu"] = observe("gpu", gpu_query)
    if facts["gpu"]:
        gpu = facts["gpu"]
        if not gpu["gb10Observed"] or gpu["utilizationPercent"] is None:
            result["errors"].append({"code": "gpu_prerequisites_unknown", "phase": "gpu",
                                     "message": "A GB10 GPU and current utilization were not both confirmed"})
        elif gpu["utilizationPercent"] > 0:
            result["errors"].append({"code": "gpu_busy", "phase": "gpu",
                                     "message": "The selected GPU reports active utilization"})

    def gpu_processes():
        found = command("nvidia-smi", ["--query-compute-apps=gpu_uuid,pid,process_name", "--format=csv,noheader,nounits"])
        processes = []
        for row in csv.reader(io.StringIO(found["output"])):
            if len(row) != 3 or not row[0].strip().startswith("GPU-") or not row[1].strip().isdigit():
                raise ProbeError("gpu_processes_unknown", "GPU process inventory is malformed or unsupported")
            processes.append({"gpuUUID": row[0].strip(), "pid": int(row[1].strip()), "name": row[2].strip()})
        return {"path": found["path"], "processes": processes}

    facts["gpuProcesses"] = observe("gpu", gpu_processes)
    if facts["gpuProcesses"] and facts["gpuProcesses"]["processes"]:
        result["errors"].append({"code": "gpu_busy", "phase": "gpu",
                                 "message": "The native GPU process inventory contains active compute processes"})

    def route():
        found = command("ip", ["-j", "-4", "route", "get", request["peerAddress"]])
        routes = json.loads(found["output"])
        if not isinstance(routes, list) or len(routes) != 1 or not isinstance(routes[0], dict):
            raise ProbeError("route_unknown", "A single route to the selected peer was not observed")
        entry = routes[0]
        interface, source = entry.get("dev"), entry.get("prefsrc", entry.get("src"))
        if not isinstance(interface, str) or not re.fullmatch(r"[A-Za-z0-9_.-]{1,15}", interface):
            raise ProbeError("route_unknown", "Selected peer route has no usable interface")
        address = ipaddress.IPv4Address(source)
        if interface == "lo" or address.is_loopback or entry.get("type") in ("local", "blackhole", "unreachable", "prohibit"):
            raise ProbeError("route_unknown", "Selected peer route is not a usable inter-node route")
        return {"peerAddress": request["peerAddress"], "interface": interface, "sourceAddress": str(address)}

    facts["route"] = observe("route", route)

    def resources():
        memory = native.read("/proc/meminfo", 16384)
        values = re.findall(r"^MemAvailable:\s+([0-9]+) kB$", memory, re.MULTILINE)
        if len(values) != 1:
            raise ProbeError("memory_unknown", "Available system memory was not reported")
        return {"freeDiskBytes": native.disk(account["home"]), "availableMemoryBytes": int(values[0]) * 1024}

    facts["resources"] = observe("resources", resources)
    if result["errors"]:
        result["state"] = "blocked"
    return result


def main():
    try:
        request = decode(sys.stdin.buffer.read(MAX_INPUT + 1))
        result = inspect(request)
    except ProbeError as error:
        result = {"schemaVersion": 1, "state": "blocked", "effectsApplied": False,
                  "executable": False, "errors": [{"code": error.code, "phase": "request", "message": str(error)}]}
    except Exception:
        # Never serialize exceptions carrying environment, request or credential data.
        result = {"schemaVersion": 1, "state": "blocked", "effectsApplied": False,
                  "executable": False, "errors": [{"code": "inspection_failed", "phase": "inspect",
                                                      "message": "Native inspection could not complete"}]}
    output = json.dumps(result, separators=(",", ":"), ensure_ascii=True, allow_nan=False)
    if len(output) > MAX_OUTPUT:
        output = '{"schemaVersion":1,"state":"blocked","effectsApplied":false,"executable":false,"errors":[{"code":"output_limit","phase":"inspect","message":"Inspection output limit exceeded"}]}'
    sys.stdout.write(output + "\n")


if __name__ == "__main__":
    main()
