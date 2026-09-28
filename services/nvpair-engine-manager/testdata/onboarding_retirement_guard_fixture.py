# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Temporary-file fixture support; kernel/systemd observations are mocked.

The Go test injects the actual production prelude and retirement body. Nothing
here executes an ELF, a systemd command, an installed helper, or a peer action.
"""
import builtins
import base64
import contextlib
import ctypes
import errno
import hashlib
import io
import json
import os
import stat
import sys
import shutil
import subprocess
import tempfile
import types

home = os.path.abspath(os.environ["PAIR_RETIREMENT_TEST_ROOT"])
sys.modules["pwd"] = types.SimpleNamespace(getpwuid=lambda uid: types.SimpleNamespace(pw_dir=home))
os.geteuid = lambda: 1000
os.getuid = lambda: 1000
for flag, bit in (("O_NOFOLLOW", 1 << 24), ("O_CLOEXEC", 1 << 25), ("O_DIRECTORY", 1 << 26)):
    if not hasattr(os, flag):
        setattr(os, flag, bit)
if not hasattr(os, "O_ACCMODE"):
    os.O_ACCMODE = os.O_WRONLY | os.O_RDWR

real_open, real_close, real_fstat = os.open, os.close, os.fstat
real_stat, real_lstat, real_listdir, real_os_readlink = os.stat, os.lstat, os.listdir, os.readlink
real_unlink, real_rename, real_replace, real_fsync, real_write, real_chmod = os.unlink, os.rename, os.replace, os.fsync, os.write, os.chmod
real_builtin_open = builtins.open
modes, held, fd_paths, virtual = {}, {}, {}, {}
owners, redirected = {}, set()
opened, deletions, identity_checks = [], [], []
scenario = "success"
injected = False
identity_changed = False
delete_after = None
interrupt_proof = None
expected_guards = 0
next_virtual_fd = 100000
op = "a" * 32
digest = "b" * 64
root = os.path.join(home, ".local", "share", "Nvidia Corporation", "Personal AI Router")
stage = os.path.join(root, ".onboarding", op)
bundle = os.path.join(root, "bundles", digest)
old_bundle = os.path.join(home, "old-application", "resources", "cli-bin")
tui = os.path.join(bundle, "bin", "nvpair-tui")
config = os.path.join(home, ".config")
unitpath = os.path.join(config, "systemd", "user", "nvidia-pair-headless.service")
names = ["nvpair-ui-broker", "nvpair-node-scanner", "nvpair-node-info", "nvpair-cluster-manager", "nvpair-engine-manager", "nvpair-node-settings", "nvpair-errors", "nvpair-manual-nodes", "nvpair-workload-manager", "nvpair-job-scheduler", "nvpair-tui", "ollama-proxy", "lmstudio-proxy"]
old_paths = [os.path.join(old_bundle, name) for name in names]
retired_suffix = ".pair-retired-" + op


def write_fixture(filename, data, mode=0o600):
    os.makedirs(os.path.dirname(filename), exist_ok=True)
    with real_builtin_open(filename, "wb") as stream:
        stream.write(data)
    modes[filename] = mode


for directory in (stage, old_bundle, os.path.dirname(tui), os.path.dirname(unitpath)):
    os.makedirs(directory, exist_ok=True)
image = bytearray(64)
image[:6] = b"\x7fELF\x02\x01"
image[16:18] = (2).to_bytes(2, "little")
image[18:20] = (183).to_bytes(2, "little")
for filename in old_paths:
    write_fixture(filename, bytes(image), 0o755)
write_fixture(tui, b"new-tui-fixture", 0o755)
old_manifest_data = {
    "source": "services-build", "sourceFingerprint": "f" * 64, "services": "0.91.7", "platform": "linux", "arch": "arm64",
    "components": {name: "0.1.0" for name in names},
    "files": [{"fileName": name, "size": len(image), "sha256": hashlib.sha256(image).hexdigest()} for name in names],
    "builtAt": "2026-09-12T00:00:00Z",
}
write_fixture(os.path.join(old_bundle, "manifest.json"), json.dumps(old_manifest_data).encode())
old_manifest = os.path.join(old_bundle, "manifest.json")
manifest_hash = hashlib.sha256(real_builtin_open(old_manifest, "rb").read()).hexdigest()
marker = {"owner": "nvidia-pair-onboarding-v1", "operationId": op, "sha256": digest, "manifestSha256": "c" * 64, "startupLifetime": "session"}
write_fixture(os.path.join(stage, ".pair-onboarding.json"), json.dumps(marker).encode())
existing = {
    "nodeId": "peer", "clusterId": "cluster", "certFingerprint": "sha256:" + "d" * 64,
    "uid": 1000, "home": home, "configHome": config, "unit": "nvidia-pair-headless.service",
    "unitPath": unitpath, "unitSha256": "e" * 64, "manifestSha256": manifest_hash,
    "bundle": old_bundle, "launcherPath": os.path.join(home, ".local", "bin", "nvpair"),
    "launcherPresent": False, "launcherBody": "", "launcherSha256": "",
    "components": [{"name": name, "bytes": len(image), "sha256": hashlib.sha256(image).hexdigest()} for name in names],
}
r = {"stagePath": stage, "bundlePath": bundle, "tuiPath": tui, "artifactSha256": digest, "manifestSha256": "c" * 64, "startupLifetime": "session"}
h = {"receipt": r, "existing": existing, "retiredProcessSuffix": retired_suffix}


def old_original(filename):
    filename = os.fspath(filename)
    original = filename[:-len(retired_suffix)] if filename.endswith(retired_suffix) else filename
    return original if original in old_paths else None


class FileInfo:
    def __init__(self, info, filename):
        self._info = info
        self.st_uid = owners.get(filename, 1000)
        self.st_mode = stat.S_IFMT(info.st_mode) | modes.get(filename, 0o700 if stat.S_ISDIR(info.st_mode) else stat.S_IMODE(info.st_mode))
        self.st_nlink = 2 if scenario == "hardlink" and old_original(filename) == old_paths[0] else info.st_nlink
        if scenario == "redirection" and old_original(filename) == old_paths[0]:
            self.st_mode = stat.S_IFLNK | 0o755
        if filename in redirected:
            self.st_mode = stat.S_IFLNK | 0o755
        if scenario == "owner-race" and injected and old_original(filename) == old_paths[0]:
            self.st_uid = 1001

    def __getattr__(self, name):
        return getattr(self._info, name)


def fixture_stat(filename, *args, **kwargs):
    if isinstance(filename, int):
        return fixture_fstat(filename)
    if isinstance(filename, str) and filename == "/proc/123/exe":
        return FileInfo(real_stat(tui), tui)
    return FileInfo(real_stat(filename, *args, **kwargs), os.fspath(filename))


def fixture_lstat(filename, *args, **kwargs):
    return FileInfo(real_lstat(filename, *args, **kwargs), os.fspath(filename))


# posixpath.realpath follows a link that lstat reports, so a faked link must
# also resolve somewhere else.
def fixture_readlink(filename, *args, **kwargs):
    name = os.fspath(filename)
    if not stat.S_ISLNK(real_lstat(name).st_mode) and stat.S_ISLNK(fixture_lstat(name).st_mode):
        return os.path.join(os.path.dirname(name), "redirected-target")
    return real_os_readlink(filename, *args, **kwargs)


def fixture_open(filename, flags, mode=0o777, *args, **kwargs):
    global injected, identity_changed, next_virtual_fd
    filename = os.fspath(filename)
    original = old_original(filename)
    write_guard = original is not None and flags & os.O_ACCMODE != os.O_RDONLY
    if write_guard:
        assert flags & os.O_ACCMODE == os.O_WRONLY
        assert flags & os.O_NOFOLLOW and flags & os.O_CLOEXEC
        assert not flags & (os.O_TRUNC | os.O_CREAT | os.O_EXCL)
        opened.append(filename)
        if scenario in ("busy", "permission") and original == old_paths[2]:
            # This is the Linux remote script: Windows maps ETXTBSY to 139.
            raise OSError(26 if scenario == "busy" else errno.EACCES, "injected guard refusal")
        if not injected and original == old_paths[0] and scenario in ("inode-race", "hash-race", "mode-race", "owner-race"):
            injected = True
            if scenario == "inode-race":
                replacement = filename + ".external-replacement"
                write_fixture(replacement, real_builtin_open(filename, "rb").read(), 0o755)
                real_replace(replacement, filename)
            elif scenario == "hash-race":
                with real_builtin_open(filename, "r+b") as stream:
                    stream.seek(63)
                    stream.write(b"x")
            elif scenario == "mode-race":
                modes[filename] = 0o775
    # The real Linux ETXTBSY contract is tested separately on the host. Virtual
    # descriptors model its lifetime here; Windows denies unlink of open files.
    if flags & os.O_DIRECTORY or original is not None or filename in (old_manifest, old_manifest + retired_suffix):
        next_virtual_fd += 1
        fd = next_virtual_fd
        virtual[fd] = FileInfo(real_stat(filename), filename)
    else:
        native_flags = flags & ~(os.O_NOFOLLOW | os.O_CLOEXEC | os.O_DIRECTORY)
        fd = real_open(filename, native_flags, mode, *args, **kwargs)
    fd_paths[fd] = filename
    if flags & os.O_CREAT:
        modes[filename] = mode
    if write_guard:
        held[fd] = filename
        if scenario == "identity-race" and len(held) == len(old_paths):
            identity_changed = True
    return fd


def fixture_close(fd):
    held.pop(fd, None)
    fd_paths.pop(fd, None)
    if fd in virtual:
        del virtual[fd]
        return
    real_close(fd)


def fixture_fstat(fd):
    filename = fd_paths.get(fd, "")
    return virtual[fd] if fd in virtual else FileInfo(real_fstat(fd), filename)


def fixture_unlink(filename, *args, **kwargs):
    original = old_original(filename)
    if original is not None:
        assert len(held) == expected_guards and expected_guards > 0, "complete old-ELF guard set was not retained through deletion"
    real_unlink(filename, *args, **kwargs)
    for fd, observed in virtual.items():
        if fd_paths.get(fd) == os.fspath(filename):
            observed.st_nlink = 1 if scenario == "hardlink-after-unlink" and original == old_paths[0] else 0
    if original is not None:
        deletions.append(os.fspath(filename))
        if delete_after is not None and len(deletions) == delete_after:
            raise RuntimeError("injected interruption after an old ELF was retired")


def fixture_listdir(directory):
    if os.fspath(directory) == "/proc":
        raise PermissionError(errno.EACCES, "unrelated process visibility is denied")
    return real_listdir(directory)


def fixture_rename(first_fd, first, second_fd, second, flags):
    first, second = os.fsdecode(first), os.fsdecode(second)
    if flags == 1:
        if os.path.lexists(second):
            ctypes.set_errno(errno.EEXIST)
            return -1
        real_rename(first, second)
        for fd, filename in list(fd_paths.items()):
            if filename == first:
                fd_paths[fd] = second
    elif flags == 2:
        temporary = first + ".fixture-exchange"
        real_rename(first, temporary)
        real_rename(second, first)
        real_rename(temporary, second)
        for fd, filename in list(fd_paths.items()):
            if filename == first:
                fd_paths[fd] = second
            elif filename == second:
                fd_paths[fd] = first
    else:
        raise AssertionError("unexpected rename flags")
    if flags == 2:
        modes[first], modes[second] = modes.get(second, 0o600), modes.get(first, 0o600)
    elif first in modes:
        modes[second] = modes.pop(first)
    return 0


os.stat, os.lstat, os.open, os.close, os.fstat = fixture_stat, fixture_lstat, fixture_open, fixture_close, fixture_fstat
os.unlink, os.listdir, os.readlink = fixture_unlink, fixture_listdir, fixture_readlink
ctypes.CDLL = lambda *args, **kwargs: types.SimpleNamespace(renameat2=fixture_rename)


def fixture_fsync(fd):
    global interrupt_proof
    if fd not in virtual:
        return real_fsync(fd)
    if fd_paths.get(fd) == stage and interrupt_proof is not None:
        with real_builtin_open(os.path.join(stage, ".pair-retirement-proof.json"), "rb") as stream:
            proof = json.load(stream)
        completed = proof.get("retired") == [names[0]] and proof.get("pendingUnlink") is None
        pending = proof.get("retired") == [] and proof.get("pendingUnlink") == names[0]
        initial = proof.get("retired") == [] and proof.get("pendingUnlink") is None
        if interrupt_proof == "completed" and completed or interrupt_proof == "pending" and pending or interrupt_proof == "initial" and initial:
            interrupt_proof = None
            raise RuntimeError("injected lost reply after durable proof publication")


os.fsync = fixture_fsync


def fixture_write(fd, data):
    assert fd not in held, "retirement wrote to an old ELF guard"
    return real_write(fd, data)


def fixture_chmod(filename, mode, *args, **kwargs):
    modes[os.fspath(filename)] = mode
    return real_chmod(filename, mode, *args, **kwargs)


os.write, os.chmod = fixture_write, fixture_chmod


def fixture_marked(directory):
    try:
        with real_builtin_open(os.path.join(directory, ".pair-onboarding.json")) as stream:
            return json.load(stream) == marker
    except FileNotFoundError:
        return False


marked = fixture_marked
verify_tui = lambda: None


def forbid_product_execution(*args, **kwargs):
    raise AssertionError("fixture attempted to execute a native product or system command")


subprocess.run = subprocess.Popen = subprocess.check_output = forbid_product_execution


def fixture_identity(*args, **kwargs):
    identity_checks.append(len(held))
    if identity_changed:
        raise RuntimeError("injected identity changed during guard acquisition")


verify_upgrade_identity = fixture_identity


def invoke_retirement(body):
    global expected_guards
    expected_guards = sum(os.path.lexists(p) + os.path.lexists(p + retired_suffix) for p in old_paths)
    output = io.StringIO()
    error = None
    try:
        with contextlib.redirect_stdout(output):
            exec(body, globals())
    except BaseException as failure:
        error = type(failure).__name__ + ": " + str(failure)
    text = output.getvalue().strip()
    return {"error": error, "wire": json.loads(text) if text else None, "held": len(held), "deletions": len(deletions)}


def ready_fixture():
    write_fixture(unitpath, upgrade_unit(tui, os.path.join(bundle, "bin", "nvpair-ui-broker"), config, 145).encode())
    globals()["upgrade_manager"] = lambda account: {
        "LoadState": "loaded", "ActiveState": "active", "SubState": "running", "FragmentPath": unitpath,
        "DropInPaths": "", "Job": "", "MainPID": "123",
    }


def current_inventory():
    for name in ("llamacpp-proxy", "vllm-proxy"):
        filename = os.path.join(old_bundle, name)
        names.append(name)
        old_paths.append(filename)
        write_fixture(filename, bytes(image), 0o755)
        component = {"name": name, "bytes": len(image), "sha256": hashlib.sha256(image).hexdigest()}
        existing["components"].append(component)
        old_manifest_data["components"][name] = "0.1.0"
        old_manifest_data["files"].append({"fileName": name, "size": len(image), "sha256": component["sha256"]})
    write_fixture(old_manifest, json.dumps(old_manifest_data).encode())
    existing["manifestSha256"] = hashlib.sha256(real_builtin_open(old_manifest, "rb").read()).hexdigest()


def assert_held(result, before=0):
    assert result["error"] is not None and result["held"] == 0, result
    assert result["error"].startswith(("RuntimeError:", "PermissionError:")), result
    assert result["deletions"] == before, result
    assert result["wire"] is None, result
    assert not virtual, "a directory or manifest guard leaked"


def assert_proof_held(result, before=0):
    assert result["error"] is None and result["held"] == 0 and result["deletions"] == before, result
    assert result["wire"] == {"retired": False, "retiredEntries": 0, "launcher": "unconfirmed", "scope": "bound-cli-only", "failureCode": "retirement-proof-unconfirmed"}, result
    assert not virtual, "a proof ownership refusal leaked descriptors"


def assert_retired(result):
    assert result["error"] is None and result["held"] == 0, result
    assert result["wire"] == {"retired": True, "retiredEntries": len(names) + 1, "launcher": "absent", "scope": "bound-cli-only"}, result
    assert len(deletions) == len(names), deletions
    assert not any(os.path.lexists(p) or os.path.lexists(p + retired_suffix) for p in old_paths + [old_manifest])
    assert not os.path.lexists(existing["launcherPath"])
    assert not virtual, "a directory or manifest guard leaked"


def private_ancestry():
    local = os.path.join(home, ".local")
    corporation = os.path.join(local, "share", "Nvidia Corporation")
    modes.update({home: 0o755, local: 0o700, os.path.join(local, "share"): 0o700, corporation: 0o775, root: 0o700, os.path.dirname(stage): 0o700, stage: 0o700})
    return local, corporation


def ancestry_case(case, body):
    global interrupt_proof, delete_after
    local, corporation = private_ancestry()
    # The native descriptor orders the original thirteen entries; lmstudio is
    # the first alias retained by the interrupted operation.
    names.sort()
    old_paths[:] = [os.path.join(old_bundle, name) for name in names]
    existing["components"].sort(key=lambda entry: entry["name"])
    if case == "ancestry-private-anchor":
        # Owned directory descendants below a private boundary are readable;
        # writable binary/identity files themselves remain refused.
        identity = os.path.join(config, "Nvidia Corporation", "Personal AI Router", "cluster", "identity.json")
        write_fixture(identity, b"{}")
        for filename, directory in ((old_paths[0], old_bundle), (identity, os.path.dirname(identity))):
            modes[directory] = 0o775
            assert upgrade_private(filename, home, 65536)[0]
            prior_mode = modes[filename]
            modes[filename] = 0o664
            try:
                upgrade_private(filename, home, 65536)
            except RuntimeError as error:
                assert "file ownership" in str(error)
            else:
                raise AssertionError("the private directory boundary admitted a writable installed file")
            modes[filename] = prior_mode
            modes[directory] = 0o700
        assert_retired(invoke_retirement(body))
    elif case in ("ancestry-initial-alias", "ancestry-proof-mode"):
        interrupt_proof = "initial"
        assert_held(invoke_retirement(body))
        proofpath = os.path.join(stage, ".pair-retirement-proof.json")
        proof = json.loads(real_builtin_open(proofpath, "rb").read())
        assert proof["retired"] == [] and proof["pendingUnlink"] is None
        if case == "ancestry-initial-alias":
            assert names[0] == "lmstudio-proxy"
            real_rename(old_paths[0], old_paths[0] + retired_suffix)
            modes[old_paths[0] + retired_suffix] = modes.pop(old_paths[0])
            assert_retired(invoke_retirement(body))
        else:
            modes[proofpath] = 0o640
            assert_held(invoke_retirement(body))
    elif case == "ancestry-pending-present":
        interrupt_proof = "pending"
        assert_held(invoke_retirement(body))
        assert os.path.lexists(old_paths[0] + retired_suffix)
        assert_retired(invoke_retirement(body))
    elif case == "ancestry-pending-missing":
        delete_after = 1
        assert_held(invoke_retirement(body), before=1)
        delete_after = None
        assert_held(invoke_retirement(body), before=1)
    else:
        if case == "ancestry-writable-before-anchor":
            modes[local] = 0o775
        elif case == "ancestry-foreign-descendant":
            owners[corporation] = 1001
        elif case == "ancestry-redirect":
            redirected.add(corporation)
        elif case == "ancestry-stage-mode":
            modes[stage] = 0o750
        else:
            raise AssertionError("unknown ancestry fixture " + case)
        assert_proof_held(invoke_retirement(body))
        assert not opened and not os.path.lexists(os.path.join(stage, ".pair-retirement-proof.json"))
    assert modes[corporation] == 0o775, "retirement changed an existing ancestor's permissions"


def managed_inspection_case(case):
    global tui, bundle
    private_ancestry()
    modes[os.path.join(root, "bundles")] = 0o700
    modes[bundle] = 0o775
    modes[os.path.join(bundle, "bin")] = 0o700
    tui = os.path.join(bundle, "bin", "nvpair-tui")
    installed_names = names + ["llamacpp-proxy", "vllm-proxy"]
    files = []
    for name in installed_names:
        filename = os.path.join(bundle, "bin", name)
        write_fixture(filename, bytes(image), 0o755)
        files.append({"fileName": name, "size": len(image), "sha256": hashlib.sha256(image).hexdigest()})
    installed_manifest = {"source": "services-build", "sourceFingerprint": "f" * 64, "services": "0.92.0", "platform": "linux", "arch": "arm64", "components": {name: "0.1.0" for name in installed_names}, "files": files}
    manifest_path = os.path.join(bundle, "bin", "manifest.json")
    write_fixture(manifest_path, json.dumps(installed_manifest).encode(), 0o644)
    identityroot = os.path.join(config, "Nvidia Corporation", "Personal AI Router")
    write_fixture(os.path.join(identityroot, "cluster", "identity.json"), json.dumps({"node_uuid": "peer"}).encode())
    write_fixture(os.path.join(identityroot, "node-id.json"), json.dumps({"nodeId": "peer"}).encode())
    certificate = b"fixture-public-certificate"
    pem = b"-----BEGIN CERTIFICATE-----\n" + base64.b64encode(certificate) + b"\n-----END CERTIFICATE-----\n"
    write_fixture(os.path.join(identityroot, "cluster", "node.crt"), pem)
    fingerprint = "sha256:" + hashlib.sha256(certificate).hexdigest()
    ready_fixture()
    if case == "managed-inspect-foreign-owner":
        owners[tui] = 1001
    elif case == "managed-inspect-redirect":
        redirected.add(os.path.dirname(tui))
    elif case == "managed-inspect-writable-file":
        modes[tui] = 0o664
    elif case == "managed-inspect-unanchored-parent":
        modes[os.path.join(home, ".local")] = 0o775
    platform.system = lambda: "Linux"
    platform.machine = lambda: "aarch64"
    real_readlink = os.readlink
    os.readlink = lambda filename, *args, **kwargs: tui if filename == "/proc/123/exe" else real_readlink(filename, *args, **kwargs)
    def read_fixture(filename, *args, **kwargs):
        if filename == "/proc/123/stat":
            return io.StringIO("123 (nvpair-tui) " + " ".join(["0"] * 19 + ["999"]))
        return real_builtin_open(filename, *args, **kwargs)
    builtins.open = read_fixture
    controls = []
    def control_fixture(argv, *args, **kwargs):
        if argv == [tui, "--control"]:
            assert kwargs["input"] == b'{"method":"cluster:get-node-id"}'
            controls.append("identity")
            return types.SimpleNamespace(returncode=0, stdout=json.dumps({"result": {"nodeUuid": "peer", "clusterId": "cluster", "certFingerprint": fingerprint}}).encode())
        if argv == ["/usr/bin/loginctl", "show-user", "1000", "-p", "Linger", "--value"]:
            return types.SimpleNamespace(returncode=0, stdout=b"no\n")
        raise AssertionError("inspection attempted an unmocked command")
    subprocess.run = control_fixture
    if case == "managed-inspect-next-upgrade":
        result = inspect_existing()
        assert result["nodeId"] == "peer" and result["clusterId"] == "cluster" and result["certFingerprint"] == fingerprint
        assert result["bundle"] == os.path.dirname(tui) and result["executable"] == tui
        assert result["version"] == "0.92.0" and len(result["components"]) == 15
        assert result["manifestSha256"] == hashlib.sha256(real_builtin_open(manifest_path, "rb").read()).hexdigest()
        assert controls == ["identity"]
    else:
        try:
            inspect_existing()
        except RuntimeError as error:
            assert any(word in str(error) for word in ("ancestry", "namespace", "bundle", "owner", "redirect", "private")), str(error)
        else:
            raise AssertionError("unsafe installed-bundle ownership was admitted")
        assert controls == [], "refused inspection reached the old product control helper"
    assert not deletions and not held and not virtual


def run_case(case, body, finish_body):
    global scenario, delete_after, injected, identity_changed, interrupt_proof
    proofpath = os.path.join(stage, ".pair-retirement-proof.json")
    if case == "current-fifteen":
        current_inventory()
    if case.startswith("managed-inspect-"):
        managed_inspection_case(case)
    elif case.startswith("ancestry-"):
        ancestry_case(case, body)
    elif case in ("legacy-thirteen", "current-fifteen", "lost-reply-and-cleanup"):
        # The unchanged native request has only these three pre-existing fields.
        assert set(h) == {"receipt", "existing", "retiredProcessSuffix"}
        result = invoke_retirement(body)
        assert_retired(result)
        assert os.path.lexists(proofpath)
        if case == "lost-reply-and-cleanup":
            again = invoke_retirement(body)
            assert_retired(again)
            h.clear()
            h["receipt"] = r
            finished = invoke_retirement(finish_body)
            assert finished["error"] is None and finished["wire"] == {"stagingCleaned": True}, finished
            assert not os.path.lexists(stage) and os.path.lexists(tui)
    elif case == "hardlink-after-unlink":
        scenario = case
        first = invoke_retirement(body)
        assert_held(first, before=1)
        assert os.path.lexists(stage), "ambiguous retirement discarded its evidence"
        scenario = "success"
        assert_held(invoke_retirement(body), before=1)
    elif case == "pending-present":
        interrupt_proof = "pending"
        assert_held(invoke_retirement(body))
        assert os.path.lexists(old_paths[0] + retired_suffix)
        assert_retired(invoke_retirement(body))
    elif case == "pending-missing":
        delete_after = 1
        first = invoke_retirement(body)
        assert_held(first, before=1)
        delete_after = None
        assert_held(invoke_retirement(body), before=1)
    elif case in ("busy", "permission", "hardlink", "redirection", "inode-race", "hash-race", "mode-race", "owner-race", "identity-race"):
        scenario = case
        result = invoke_retirement(body)
        if case == "busy":
            assert result["error"] is None and result["held"] == 0 and result["deletions"] == 0, result
            assert result["wire"] == {"retired": False, "retiredEntries": 0, "launcher": "unconfirmed", "scope": "bound-cli-only", "failureCode": "old-executable-busy"}, result
            assert len(opened) == 3, opened
            scenario = "success"
            assert_retired(invoke_retirement(body))
        else:
            assert_held(result)
            assert not os.path.lexists(proofpath), "refused acquisition published a retirement proof"
    elif case in ("missing-legacy-elf", "missing-legacy-manifest", "duplicate-alias"):
        if case == "missing-legacy-elf":
            real_unlink(old_paths[0])
        elif case == "missing-legacy-manifest":
            real_unlink(old_manifest)
        else:
            write_fixture(old_paths[0] + retired_suffix, bytes(image), 0o755)
        assert_held(invoke_retirement(body))
        assert not os.path.lexists(proofpath)
    elif case.startswith("partial-"):
        interrupt_proof = "completed"
        first = invoke_retirement(body)
        assert first["error"] and first["held"] == 0 and first["deletions"] == 1, first
        assert os.path.lexists(proofpath) and os.path.lexists(old_manifest)
        proof = json.loads(real_builtin_open(proofpath, "rb").read())
        assert set(proof["inodes"]) == set(names + ["manifest.json"])
        assert proof["retired"] == [names[0]] and proof["pendingUnlink"] is None
        if case in ("partial-valid", "partial-alias"):
            if case == "partial-alias":
                real_rename(old_paths[1], old_paths[1] + retired_suffix)
                modes[old_paths[1] + retired_suffix] = modes.pop(old_paths[1])
            assert_retired(invoke_retirement(body))
        else:
            if case == "partial-missing-proof":
                real_unlink(proofpath)
            elif case == "partial-tampered-binding":
                proof["binding"]["owner"]["operationId"] = "c" * 32
                write_fixture(proofpath, json.dumps(proof).encode())
            elif case == "partial-tampered-inode":
                proof["inodes"][names[1]]["ino"] += 1
                write_fixture(proofpath, json.dumps(proof).encode())
            elif case == "partial-replaced-inode":
                replacement = old_paths[1] + ".foreign"
                write_fixture(replacement, bytes(image), 0o755)
                real_replace(replacement, old_paths[1])
            elif case == "partial-proof-mode":
                modes[proofpath] = 0o644
            elif case == "partial-prefix-tamper":
                proof["retired"] = [names[1]]
                write_fixture(proofpath, json.dumps(proof).encode())
            elif case == "partial-pending-tamper":
                proof["pendingUnlink"] = names[2]
                write_fixture(proofpath, json.dumps(proof).encode())
            elif case == "partial-publication-uncertain":
                write_fixture(os.path.join(stage, ".pair-retirement-proof-uncertain"), b"retained evidence")
            elif case == "partial-wrong-stage-owner":
                write_fixture(os.path.join(stage, ".pair-onboarding.json"), b"{}")
            else:
                raise AssertionError("unknown partial fixture " + case)
            result = invoke_retirement(body)
            if case == "partial-wrong-stage-owner":
                assert_proof_held(result, before=1)
            else:
                assert_held(result, before=1)
    else:
        raise AssertionError("unknown fixture " + case)
    return {"case": case, "passed": True, "remainingGuards": len(held), "oldELFWrites": 0}
