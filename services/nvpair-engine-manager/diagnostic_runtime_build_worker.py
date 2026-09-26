# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Fixed CPU-only build worker. Started only by the reviewed user-unit controller.

No GPU test, MPI launch, sudo, package installation, shell input, or manager
registration occurs here. The unit's cgroup owns this worker and its descendants.
"""

import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import stat
import struct
import subprocess
import sys
import tempfile
import time

RECIPE = "dgx-spark-nccl-cuda13-sm121-user-build-v2"
CONTROLLER_OWNER = "pair-user-nccl-build-controller-v2"
# The owning Go transport may assign its fixed embedded bytes after loading
# this module. No request field can select or replace shipped worker code.
SHIPPED_SOURCE = None
SOURCES = {
    "nccl": {"url": "https://github.com/NVIDIA/nccl.git", "commit": "73cf112295c33aee2b895f329f592f2a9b4b0f97", "tag": "v2.30.7-1"},
    "nccl-tests": {"url": "https://github.com/NVIDIA/nccl-tests.git", "commit": "b4d5beebca8a76cf01335f724d154b9b9d394d96", "tag": "v2.20.0"},
}
CUDA = "/usr/local/cuda-13.0"
CUDA_LIB = CUDA + "/lib64"
MPI_HOME = "/usr/lib/aarch64-linux-gnu/openmpi"
GENCODE = "-gencode=arch=compute_121,code=sm_121"
LIMITS = {"maxAttempts": 3, "maxBuildSeconds": 1800, "maxFetchSeconds": 180,
          "maxAttemptBytes": 8 * 1024**3, "minimumFreeBytes": 24 * 1024**3,
          "maxArtifactBytes": 2 * 1024**3, "maxOutputBytes": 256 * 1024,
          "parallelJobs": 2, "memoryMaxBytes": 8 * 1024**3, "tasksMax": 256}


class BuildError(Exception):
    def __init__(self, code):
        self.code = code
        super().__init__(code)


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True).encode()


def digest(value):
    return hashlib.sha256(canonical(value)).hexdigest()


def file_hash(path, maximum=LIMITS["maxArtifactBytes"], allow_hardlinks=False):
    fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    with os.fdopen(fd, "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_size > maximum or (not allow_hardlinks and info.st_nlink != 1):
            raise BuildError("artifact_not_bounded_regular_file")
        result = hashlib.sha256()
        while block := stream.read(1024 * 1024):
            result.update(block)
    return {"path": str(path), "sha256": result.hexdigest(), "size": info.st_size}


def atomic_json(path, value):
    path = Path(path)
    fd, name = tempfile.mkstemp(prefix=path.name + ".", suffix=".tmp", dir=path.parent)
    temporary = Path(name)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(canonical(value))
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        if os.name == "posix":
            directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
    finally:
        if temporary.exists():
            temporary.unlink()


def owned(path, uid, directory=False, private=False):
    path = Path(path)
    info = path.lstat()
    kind = stat.S_ISDIR if directory else stat.S_ISREG
    if not kind(info.st_mode) or info.st_uid != uid or info.st_mode & (0o077 if private else 0o022) or (not directory and info.st_nlink != 1):
        raise BuildError("unsafe_owned_path")
    return path


def trusted(path):
    path = Path(path).resolve(strict=True)
    if not str(path).startswith(("/usr/", "/lib/", "/bin/")):
        raise BuildError("untrusted_system_path")
    for current in (path, *path.parents):
        info = current.stat()
        if info.st_uid != 0 or info.st_mode & 0o022:
            raise BuildError("untrusted_system_path")
    return path


def system_hash(path):
    # Root ownership and immutable-to-normal-users permissions are the trust
    # boundary. Distro aliases may legitimately hard-link the same inode.
    return file_hash(trusted(path), allow_hardlinks=True)


def launch_allowed(plan, attempt, invocation_token=None):
    root = Path(plan["root"])
    number = int(Path(attempt).name.rsplit("-", 1)[1])
    fence = root / "cancellation.json"
    try:
        owned(fence, plan["identity"]["uid"], private=True)
    except FileNotFoundError:
        record = None
    else:
        with open(fence, "rb") as stream:
            raw = stream.read(16385)
        if len(raw) > 16384:
            raise BuildError("cancellation_fence_invalid")
        record = json.loads(raw)
        if (not isinstance(record, dict) or record.get("owner") != CONTROLLER_OWNER or
                record.get("operationId") != plan["operationId"] or record.get("planDigest") != plan["planDigest"] or
                record.get("identity") != plan["identity"] or type(record.get("closedThroughAttempt")) is not int or
                not 1 <= record["closedThroughAttempt"] <= LIMITS["maxAttempts"]):
            raise BuildError("cancellation_fence_invalid")
        if record["closedThroughAttempt"] >= number:
            raise BuildError("attempt_cancelled")
    if invocation_token is not None:
        journal_path = owned(root / "operation.json", plan["identity"]["uid"], private=True)
        with open(journal_path, "rb") as stream:
            raw = stream.read(128 * 1024 + 1)
        if len(raw) > 128 * 1024:
            raise BuildError("operation_journal_invalid")
        journal = json.loads(raw)
        if (journal.get("owner") != CONTROLLER_OWNER or journal.get("plan") != plan or journal.get("attempt") != number or
                journal.get("invocationToken") != invocation_token or journal.get("state") != "building"):
            raise BuildError("attempt_not_current")


def inside(path, root):
    result = Path(path).resolve(strict=True)
    if not result.is_relative_to(Path(root).resolve(strict=True)):
        raise BuildError("path_escaped_attempt")
    return result


def check_plan(plan):
    if not isinstance(plan, dict):
        raise BuildError("invalid_plan")
    body = dict(plan)
    stated = body.pop("planDigest", None)
    if stated != digest(body) or plan.get("recipeId") != RECIPE or plan.get("sources") != SOURCES or plan.get("limits") != LIMITS:
        raise BuildError("plan_binding_mismatch")
    if not re.fullmatch(r"[a-f0-9]{32}", str(plan.get("operationId", ""))):
        raise BuildError("invalid_operation")
    if plan.get("cudaHome") != CUDA or plan.get("mpiHome") != MPI_HOME or plan.get("gencode") != GENCODE:
        raise BuildError("recipe_changed")
    identity = plan.get("identity", {})
    home = identity.get("home", "")
    if identity.get("uid", 0) <= 0 or not re.fullmatch(r"/[A-Za-z0-9_./-]+", home) or ".." in Path(home).parts:
        raise BuildError("unsupported_home")
    if plan.get("root") != str(Path(home) / ".local/share/pair-nccl-build-v1" / plan["operationId"]):
        raise BuildError("plan_path_mismatch")
    return plan


def elf_header(path):
    with open(path, "rb") as stream:
        raw = stream.read(64)
        if len(raw) != 64 or raw[:7] != b"\x7fELF\x02\x01\x01" or struct.unpack_from("<H", raw, 18)[0] != 183:
            raise BuildError("not_aarch64_elf64")
        if struct.unpack_from("<H", raw, 16)[0] not in (2, 3):
            raise BuildError("elf_type_unsupported")
        offset = struct.unpack_from("<Q", raw, 32)[0]
        size, count = struct.unpack_from("<HH", raw, 54)
        if count > 128 or (count and size != 56):
            raise BuildError("elf_program_headers_invalid")
        interpreter = None
        for index in range(count):
            stream.seek(offset + index * size)
            entry = stream.read(size)
            if len(entry) != size:
                raise BuildError("elf_program_headers_invalid")
            if struct.unpack_from("<I", entry)[0] == 3:  # PT_INTERP
                start, length = struct.unpack_from("<Q", entry, 8)[0], struct.unpack_from("<Q", entry, 32)[0]
                if interpreter is not None or not 2 <= length <= 256:
                    raise BuildError("elf_interpreter_invalid")
                stream.seek(start)
                value = stream.read(length)
                if len(value) != length or not value.endswith(b"\0"):
                    raise BuildError("elf_interpreter_invalid")
                interpreter = value[:-1].decode("ascii")
        return interpreter


class Runner:
    """Fixed worker commands only; tests replace this I/O boundary."""
    def __init__(self, plan, attempt):
        self.plan, self.attempt = plan, Path(attempt)
        self.deadline = time.monotonic() + LIMITS["maxBuildSeconds"]
        self.calls = []
        self.env = {"PATH": CUDA + "/bin:/usr/bin:/bin", "HOME": str(self.attempt / "empty-home"),
                    "LANG": "C", "LC_ALL": "C", "TMPDIR": str(self.attempt / "tmp"),
                    "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null",
                    "GIT_CONFIG_SYSTEM": "/dev/null", "GIT_TERMINAL_PROMPT": "0",
                    "GIT_ALLOW_PROTOCOL": "https", "GIT_OPTIONAL_LOCKS": "0"}

    def run(self, argv, cwd=None, seconds=15):
        launch_allowed(self.plan, self.attempt)
        self.calls.append(argv)
        remaining = min(seconds, self.deadline - time.monotonic())
        if remaining <= 0:
            raise BuildError("build_timeout")
        # The containing user unit enforces the hard deadline and owns all
        # descendants. Output is a regular bounded file, not an unbounded pipe.
        output = self.attempt / "command-output.txt"
        fd = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | getattr(os, "O_NOFOLLOW", 0), 0o600)
        with os.fdopen(fd, "wb") as stream:
            child = subprocess.Popen(argv, cwd=cwd, env=self.env, stdin=subprocess.DEVNULL,
                                     stdout=stream, stderr=subprocess.STDOUT)
            deadline = time.monotonic() + remaining
            next_disk_check = 0.0
            while child.poll() is None:
                if time.monotonic() >= deadline:
                    child.kill()
                    child.wait()
                    raise BuildError("command_timeout")
                if os.fstat(stream.fileno()).st_size > LIMITS["maxOutputBytes"]:
                    child.kill()
                    child.wait()
                    raise BuildError("command_output_limit")
                if time.monotonic() >= next_disk_check:
                    total = 0
                    for directory, _, files in os.walk(self.attempt, followlinks=False):
                        for name in files:
                            info = Path(directory, name).lstat()
                            if stat.S_ISREG(info.st_mode):
                                total += info.st_size
                    if total > LIMITS["maxAttemptBytes"]:
                        child.kill()
                        child.wait()
                        raise BuildError("attempt_disk_limit")
                    next_disk_check = time.monotonic() + 2
                time.sleep(0.1)
        if output.stat().st_size > LIMITS["maxOutputBytes"]:
            raise BuildError("command_output_limit")
        text = output.read_text(errors="replace")
        if child.returncode:
            # Do not return uncontrolled compiler/network text in public IPC.
            raise BuildError("command_failed")
        return text.strip()


def acquire(runner, name):
    source = SOURCES[name]
    directory = runner.attempt / (name + "-src")
    directory.mkdir(mode=0o700)
    git = runner.plan["tools"]["git"]["path"]
    fixed = [git, "-c", "core.hooksPath=/dev/null", "-c", "credential.helper=",
             "-c", "http.followRedirects=false", "-c", "protocol.file.allow=never"]
    runner.run([*fixed, "init", "--template=", str(directory)])
    runner.run([*fixed, "-C", str(directory), "fetch", "--depth=1", "--no-tags", "--no-recurse-submodules",
                source["url"], source["commit"]], seconds=LIMITS["maxFetchSeconds"])
    runner.run([*fixed, "-C", str(directory), "checkout", "--detach", "--force", source["commit"]])
    head = runner.run([*fixed, "-C", str(directory), "rev-parse", "--verify", "HEAD"])
    if head != source["commit"]:
        raise BuildError("source_commit_mismatch")
    tree = runner.run([*fixed, "-C", str(directory), "ls-tree", "-r", "HEAD"])
    if any(line.startswith("160000 ") for line in tree.splitlines()) or (directory / ".gitmodules").exists():
        raise BuildError("source_submodules_unsupported")
    if runner.run([*fixed, "-C", str(directory), "status", "--porcelain", "--untracked-files=all"]):
        raise BuildError("source_tree_dirty")
    for base, dirs, files in os.walk(directory, followlinks=False):
        for entry in (*dirs, *files):
            if Path(base, entry).is_symlink():
                inside(Path(base, entry), directory)
    return directory


def dynamic(runner, path):
    text = runner.run([runner.plan["tools"]["readelf"]["path"], "--wide", "--dynamic", str(path)])
    needed = re.findall(r"\(NEEDED\).*?\[([^]\r\n]+)\]", text)
    soname = re.findall(r"\(SONAME\).*?\[([^]\r\n]+)\]", text)
    paths = re.findall(r"\((?:RUNPATH|RPATH)\).*?\[([^]\r\n]*)\]", text)
    if any(not re.fullmatch(r"[A-Za-z0-9_.+-]+", item) for item in needed + soname):
        raise BuildError("unsafe_elf_dependency")
    allowed_paths = {str(runner.attempt / "runtime/lib"), CUDA_LIB, MPI_HOME + "/lib", "/usr/lib/aarch64-linux-gnu", "/lib/aarch64-linux-gnu"}
    cuda = runner.plan.get("prerequisiteLibraries", {}).get("libcudart.so.13")
    if cuda:
        allowed_paths.add(str(Path(cuda["path"]).parent))
    if any(part not in allowed_paths for value in paths for part in value.split(":")):
        raise BuildError("unreviewed_elf_search_path")
    if any(MPI_HOME + "/lib" in value.split(":") for value in paths):
        mpi_directory = trusted(Path(MPI_HOME) / "lib")
        # The distro RUNPATH may contain aliases to normal system libraries,
        # but must not select different bytes than the static dependency walk.
        for name in needed:
            candidate = mpi_directory / name
            if candidate.exists() and trusted(candidate) != resolve_dependency(name):
                raise BuildError("unreviewed_elf_search_path")
    return {"needed": needed, "soname": soname, "searchPaths": paths}


def resolve_dependency(name):
    for directory in (CUDA_LIB, "/usr/lib/aarch64-linux-gnu", "/lib/aarch64-linux-gnu"):
        candidate = Path(directory, name)
        if candidate.exists():
            return trusted(candidate)
    raise BuildError("dependency_unresolved")


def verify_runtime(runner):
    runtime = runner.attempt / "runtime"
    binary, library = runtime / "bin/all_reduce_perf", runtime / "lib/libnccl.so.2"
    for path in (binary, library):
        owned(path, runner.plan["identity"]["uid"])
        elf_header(path)
    interpreter = elf_header(binary)
    if interpreter != "/lib/ld-linux-aarch64.so.1":
        raise BuildError("binary_interpreter_mismatch")
    executable_dynamic, library_dynamic = dynamic(runner, binary), dynamic(runner, library)
    if not {"libnccl.so.2", "libmpi.so.40", "libcudart.so.13"}.issubset(executable_dynamic["needed"]):
        raise BuildError("required_binary_dependency_missing")
    if library_dynamic["soname"] != ["libnccl.so.2"]:
        raise BuildError("nccl_soname_mismatch")
    records, pending = {}, list(executable_dynamic["needed"] + library_dynamic["needed"])
    while pending:
        name = pending.pop(0)
        if name in records:
            continue
        if len(records) >= 64:
            raise BuildError("dependency_limit")
        if name == "libnccl.so.2":
            resolved = library
        else:
            resolved = resolve_dependency(name)
        elf_header(resolved)
        details = dynamic(runner, resolved)
        if details["soname"] and details["soname"] != [name]:
            raise BuildError("dependency_soname_mismatch")
        records[name] = {**(file_hash(resolved) if name == "libnccl.so.2" else system_hash(resolved)), **details}
        pending.extend(details["needed"])
    for name, approved in runner.plan.get("prerequisiteLibraries", {}).items():
        if not name in records or any(records[name].get(key) != approved[key] for key in ("path", "sha256", "size")):
            raise BuildError("reviewed_link_dependency_changed")
    # Static resolution is intentionally not a claim that the loader or GPU ran.
    return {"schemaVersion": 1, "kind": "pair-nccl-runtime-candidate-v1",
            "recipeId": RECIPE, "operationId": runner.plan["operationId"], "planDigest": runner.plan["planDigest"],
            "identity": runner.plan["identity"], "sources": SOURCES,
            "binary": {**file_hash(binary), **executable_dynamic, "interpreter": interpreter},
            "ncclLibrary": {**file_hash(library), **library_dynamic}, "dependencies": records,
            "requiredEnvironment": {"LD_LIBRARY_PATH": str(runtime / "lib") + ":" + str(Path(records["libcudart.so.13"]["path"]).parent)},
            "linkValidation": "static-elf-resolution-only", "managerAdopted": False,
            "runtimeValidated": False, "gpuExecuted": False, "mpiExecuted": False}


def build(plan, attempt, runner=None):
    check_plan(plan)
    runner = runner or Runner(plan, attempt)
    nccl = acquire(runner, "nccl")
    tests = acquire(runner, "nccl-tests")
    nccl_build, tests_build = runner.attempt / "nccl-build", runner.attempt / "tests-build"
    make = plan["tools"]["make"]["path"]
    runner.run([make, "-C", str(nccl), "-j2", "src.build", "CUDA_HOME=" + CUDA,
                "BUILDDIR=" + str(nccl_build), "NVCC_GENCODE=" + GENCODE], seconds=LIMITS["maxBuildSeconds"])
    runner.run([make, "-C", str(tests / "src"), "-j2", str(tests_build / "all_reduce_perf"),
                "BUILDDIR=" + str(tests_build), "MPI=1", "MPI_HOME=" + MPI_HOME,
                "CUDA_HOME=" + CUDA, "NCCL_HOME=" + str(nccl_build), "NVCC_GENCODE=" + GENCODE],
               seconds=LIMITS["maxBuildSeconds"])
    (runner.attempt / "runtime/bin").mkdir(parents=True, mode=0o700)
    (runner.attempt / "runtime/lib").mkdir(mode=0o700)
    for origin, destination in ((tests_build / "all_reduce_perf", runner.attempt / "runtime/bin/all_reduce_perf"),
                                (nccl_build / "lib/libnccl.so.2", runner.attempt / "runtime/lib/libnccl.so.2")):
        origin = inside(origin, runner.attempt)
        file_hash(origin)
        shutil.copyfile(origin, destination, follow_symlinks=False)
        destination.chmod(0o700 if destination.name == "all_reduce_perf" else 0o600)
    return verify_runtime(runner)


def main(argv):
    if len(argv) != 3 or not re.fullmatch(r"[a-f0-9]{32}", argv[2]):
        raise BuildError("invalid_worker_invocation")
    plan_path = Path(argv[1])
    uid = os.geteuid()
    owned(plan_path, uid, private=True)
    plan = check_plan(json.loads(plan_path.read_bytes()))
    if plan["identity"]["uid"] != uid or uid == 0:
        raise BuildError("identity_changed")
    attempt = owned(plan_path.parent, uid, directory=True, private=True)
    if attempt.parent != Path(plan["root"]) or not re.fullmatch(r"attempt-000[1-3]", attempt.name):
        raise BuildError("attempt_path_mismatch")
    owned(attempt.parent, uid, directory=True, private=True)
    if file_hash(attempt / "worker.py")["sha256"] != plan["workerSha256"]:
        raise BuildError("worker_changed")
    for item in plan["tools"].values():
        if system_hash(item["path"]) != item:
            raise BuildError("toolchain_changed")
    import fcntl
    owned(attempt.parent / "controller.lock", uid, private=True)
    lock_fd = os.open(attempt.parent / "controller.lock", os.O_RDWR | getattr(os, "O_NOFOLLOW", 0))
    try:
        fcntl.flock(lock_fd, fcntl.LOCK_EX)
        launch_allowed(plan, attempt, argv[2])
    finally:
        os.close(lock_fd)
    for name in ("tmp", "empty-home"):
        (attempt / name).mkdir(mode=0o700)
    result = {"schemaVersion": 1, "operationId": plan["operationId"], "planDigest": plan["planDigest"],
              "invocationToken": argv[2], "state": "failed", "registration": None,
              "runtimeValidated": False, "errorCode": None}
    try:
        result["registration"] = build(plan, attempt)
        result["state"] = "built"
    except (BuildError, OSError, ValueError) as error:
        result["errorCode"] = error.code if isinstance(error, BuildError) else "build_io_failed"
    atomic_json(attempt / "worker-result.json", result)
    return 0 if result["state"] == "built" else 1


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv))
    except (BuildError, OSError, ValueError):
        sys.exit(1)
