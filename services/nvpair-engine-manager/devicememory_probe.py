# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Measure device memory for PAIR's automatic vLLM launch settings.

The engine manager runs this script with Python from vLLM's environment when
nvidia-smi cannot provide a usable reading. It uses the installed PyTorch and
vLLM libraries to find the selected platform's total and currently available
memory before PAIR chooses a context window and memory budget for the model.

Discrete GPUs use their own memory readings. System RAM is used only for a
verified integrated GPU, Apple Silicon with Metal available, or a CPU platform.
A missing device reading alone is never a reason to substitute system RAM.
CPU readings use vLLM's memory-node view so capacity and available memory
have the same limits.
Metal's MLX worker uses its recommended working-set limit for capacity; its
MPS worker uses physical RAM. These match each worker's utilization calculation.

On success, the final stdout line is JSON containing total and free bytes, a
shared flag indicating whether the device shares system RAM, and an optional
runtime identifier. If memory cannot be measured, the script exits with an error
and the manager leaves memory sizing to vLLM. The Go caller calculates the launch
settings, including the startup reserve and handling runtimes whose resident
weight size differs from the checkpoint size.
"""

import json
import platform
import sys

import psutil
import torch
from vllm.platforms import current_platform

metal_module = sys.modules.get("vllm_metal.platform")


def system_reading(total):
    memory = psutil.virtual_memory()
    return {
        "total": total,
        "free": min(int(memory.available), total),
        "shared": True,
    }


# Follow vLLM's selection: available accelerators may not be the chosen platform.
if current_platform.is_cuda_alike():
    if torch.cuda.device_count() != 1:
        raise RuntimeError("multiple CUDA devices require engine-owned memory sizing")
    properties = torch.cuda.get_device_properties(0)
    if getattr(properties, "is_integrated", False):
        reading = system_reading(int(properties.total_memory))
    else:
        free, total = torch.cuda.mem_get_info(0)
        reading = {"total": int(total), "free": int(free), "shared": False}
elif current_platform.is_xpu():
    if torch.xpu.device_count() != 1:
        raise RuntimeError("multiple XPU devices require engine-owned memory sizing")
    properties = torch.xpu.get_device_properties(0)
    if getattr(properties, "is_integrated_gpu", False):
        reading = system_reading(int(properties.total_memory))
    else:
        free, total = torch.xpu.mem_get_info(0)
        reading = {"total": int(total), "free": int(free), "shared": False}
elif metal_module is not None and isinstance(
    current_platform, metal_module.MetalPlatform
):
    from vllm_metal import envs as metal_envs

    metal_backend = metal_envs.VLLM_METAL_BACKEND
    if metal_backend == "mlx":
        import mlx.core as mx

        total = int(mx.device_info().get("max_recommended_working_set_size", 0))
    else:
        total = int(current_platform.get_device_total_memory(0))
    if total <= 0:
        raise RuntimeError("Metal runtime did not report a usable memory limit")
    available = getattr(current_platform, "get_device_available_memory", None)
    if callable(available):
        reading = {
            "total": total,
            "free": min(int(available(0)), total),
            "shared": True,
        }
    else:
        reading = system_reading(total)
    reading["runtime"] = f"metal_{metal_backend}"
else:
    apple_silicon = (
        sys.platform == "darwin"
        and platform.machine() == "arm64"
        and torch.backends.mps.is_available()
    )
    available = getattr(current_platform, "get_device_available_memory", None)
    if callable(available):
        total = int(current_platform.get_device_total_memory(0))
        free = int(available(0))
        reading = {"total": total, "free": free, "shared": apple_silicon or current_platform.is_cpu()}
    elif current_platform.is_cpu():
        from vllm.utils.cpu_resource_utils import (
            get_memory_node_info,
            get_visible_memory_node,
        )

        # Match the CPU worker's memory node and container limits.
        nodes = get_visible_memory_node()
        if len(nodes) != 1:
            raise RuntimeError("CPU memory sizing requires one visible memory node")
        memory = get_memory_node_info(nodes[0])
        reading = {
            "total": int(memory.total_memory),
            "free": int(memory.available_memory),
            "shared": True,
        }
    elif apple_silicon:
        reading = system_reading(int(current_platform.get_device_total_memory(0)))
    else:
        raise RuntimeError("the selected vLLM backend does not report free memory")

print(json.dumps(reading))
