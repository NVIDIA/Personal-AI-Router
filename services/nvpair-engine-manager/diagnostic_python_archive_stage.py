# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Fixed normal-UID product archive staging. Loaded after diagnostic definitions."""
import tempfile

def stage_python_archives(request, native=None):
    native = native or Native()
    if not isinstance(request, dict) or set(request) != {"nodeId", "principal", "archives"} or not isinstance(request["archives"], dict) or set(request["archives"]) != set(PYTHON_SNAPSHOT_FILES):
        raise ProbeError("local_archive_stage_invalid", "Only the closed three-archive product payload is supported")
    binding = identity({**request, "runtimePreparation": True}, native)
    root = Path(binding["home"])
    if str(root.resolve(strict=True)) != str(root):
        raise ProbeError("local_archive_stage_invalid", "Selected home is redirected")
    for part in PYTHON_LOCAL_SUBDIR.split("/"):
        root = root / part
        try:
            root.mkdir(mode=0o700)
        except FileExistsError:
            pass
        info = root.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != binding["uid"] or info.st_mode & 0o022:
            raise ProbeError("local_archive_stage_invalid", "Fixed normal-user cache ownership is unavailable")
    for name, artifact in PYTHON_SNAPSHOT_FILES.items():
        encoded = request["archives"][name]
        if not isinstance(encoded, str) or len(encoded) > (artifact["size"] + 2) // 3 * 4:
            raise ProbeError("local_archive_stage_invalid", "Archive payload size differs from the fixed product bytes")
        data = base64.b64decode(encoded, validate=True)
        if len(data) != artifact["size"] or hashlib.sha256(data).hexdigest() != artifact["sha256"]:
            raise ProbeError("local_archive_stage_invalid", "Archive payload does not match the fixed product hash")
        path = Path(python_local_path(binding, name))
        if os.path.lexists(path):
            info = path.lstat()
            if not stat.S_ISREG(info.st_mode) or info.st_uid != binding["uid"] or info.st_mode & 0o022 or info.st_size != artifact["size"] or path.read_bytes() != data:
                raise ProbeError("local_archive_stage_changed", "An existing archive differs; preserve it for review")
            continue
        fd, temp = tempfile.mkstemp(prefix=".pair-archive-", dir=root)
        try:
            with os.fdopen(fd, "wb") as stream:
                stream.write(data); stream.flush(); os.fsync(stream.fileno())
            os.replace(temp, path)
        finally:
            if os.path.exists(temp): os.unlink(temp)
    python_local_validate_files(native, binding)
    return {"state": "staged", "metadata": "verified_local_archive", "identity": binding,
            "archives": {name: value["sha256"] for name, value in PYTHON_SNAPSHOT_FILES.items()}}

if __name__ == "__main__":
    try:
        raw = sys.stdin.buffer.read((9 << 20) + 1)
        if len(raw) > 9 << 20: raise ProbeError("local_archive_stage_invalid", "Fixed archive staging payload exceeds nine MiB")
        print(json.dumps(stage_python_archives(json.loads(raw)), separators=(",", ":")))
    except Exception as error:
        print(json.dumps({"state": "blocked", "errorCode": getattr(error, "code", "local_archive_stage_failed")}))
