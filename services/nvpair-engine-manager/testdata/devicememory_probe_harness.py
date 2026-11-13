# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Supply fake runtime modules for the Go device-memory probe tests.

devicememory_probe_test.go embeds this file only in its test binary. Each case
sends one JSON object on stdin and runs this source followed by the production
devicememory_probe.py source in a fresh `python3 -I -c` process. Installing the
fakes in sys.modules makes the production imports use the fixture's device APIs
without requiring PyTorch, vLLM, psutil, or GPU hardware.

The production probe, rather than this harness, prints the memory reading as
JSON. An unavailable or forbidden reading raises an exception on stderr and
exits unsuccessfully, which the Go rejection cases check.

Fixture fields (memory values are GiB, converted to bytes below):
- backend: cuda, rocm, xpu, metal, cpu, or unknown (the default).
- total_gib / free_gib: device capacity and free memory, defaulting to 16 / 4.
- allow_system: permit system-memory reads, false by default. Keeping these
  forbidden for discrete and unknown devices catches unsafe host-RAM fallbacks.
- system_total_gib / system_free_gib: host readings, defaulting to 16 / 12.
- integrated: device reports shared memory, false by default. unknown_topology
  omits that property entirely to model a runtime that cannot report it.
- device_count: visible CUDA/ROCm devices, defaulting to one.
- device_memory_error: make a device free-memory query fail.
- os / arch / mps_available: simulate the platform, defaulting to linux,
  x86_64, and no Metal support.
- has_device_free: expose the vLLM platform's optional free-memory API.

These tests check that the probe chooses the expected memory source and returns
the expected values for each simulated machine.
"""

import json
import sys
import types

sys.excepthook = sys.__excepthook__
fixture = json.load(sys.stdin)
gib = 1 << 30
backend = fixture.get("backend", "unknown")
total = fixture.get("total_gib", 16) * gib
free = fixture.get("free_gib", 4) * gib


def system_memory():
    if not fixture.get("allow_system", False):
        raise RuntimeError("system RAM must not be read for this device")
    return types.SimpleNamespace(
        total=fixture.get("system_total_gib", 16) * gib,
        available=fixture.get("system_free_gib", 12) * gib,
    )


def device_memory(device):
    if fixture.get("device_memory_error", False):
        raise RuntimeError("device memory unavailable")
    return free, total


cuda_properties = types.SimpleNamespace(total_memory=total)
xpu_properties = types.SimpleNamespace(total_memory=total)
if not fixture.get("unknown_topology", False):
    cuda_properties.is_integrated = fixture.get("integrated", False)
    xpu_properties.is_integrated_gpu = fixture.get("integrated", False)

torch = types.SimpleNamespace(
    cuda=types.SimpleNamespace(
        is_available=lambda: backend in ("cuda", "rocm"),
        device_count=lambda: fixture.get("device_count", 1),
        mem_get_info=device_memory,
        get_device_properties=lambda device: cuda_properties,
    ),
    xpu=types.SimpleNamespace(
        is_available=lambda: backend == "xpu",
        mem_get_info=device_memory,
        get_device_properties=lambda device: xpu_properties,
    ),
    backends=types.SimpleNamespace(
        mps=types.SimpleNamespace(
            is_available=lambda: fixture.get("mps_available", False)
        )
    ),
)
platform = types.SimpleNamespace(
    get_device_total_memory=lambda device: total,
    is_cpu=lambda: backend == "cpu",
)
if fixture.get("has_device_free", False):
    platform.get_device_available_memory = lambda device: free

# The appended production source imports these names after the fakes are ready.
sys.modules["torch"] = torch
sys.modules["psutil"] = types.SimpleNamespace(virtual_memory=system_memory)
sys.modules["platform"] = types.SimpleNamespace(
    machine=lambda: fixture.get("arch", "x86_64")
)
sys.modules["vllm.platforms"] = types.SimpleNamespace(current_platform=platform)
sys.platform = fixture.get("os", "linux")
