# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import json
import platform
import sys

import psutil
import torch
from vllm.platforms import current_platform


def system_reading(total):
    memory = psutil.virtual_memory()
    return {
        "total": total,
        "free": min(int(memory.available), total),
        "shared": True,
    }


if torch.cuda.is_available():
    properties = torch.cuda.get_device_properties(0)
    if getattr(properties, "is_integrated", False):
        reading = system_reading(int(properties.total_memory))
    else:
        free, total = torch.cuda.mem_get_info(0)
        reading = {"total": int(total), "free": int(free), "shared": False}
elif hasattr(torch, "xpu") and torch.xpu.is_available():
    properties = torch.xpu.get_device_properties(0)
    if getattr(properties, "is_integrated_gpu", False):
        reading = system_reading(int(properties.total_memory))
    else:
        free, total = torch.xpu.mem_get_info(0)
        reading = {"total": int(total), "free": int(free), "shared": False}
else:
    total = int(current_platform.get_device_total_memory(0))
    apple_silicon = (
        sys.platform == "darwin"
        and platform.machine() == "arm64"
        and torch.backends.mps.is_available()
    )
    available = getattr(current_platform, "get_device_available_memory", None)
    if callable(available):
        free = int(available(0))
        reading = {"total": total, "free": free, "shared": apple_silicon or current_platform.is_cpu()}
    elif current_platform.is_cpu() or apple_silicon:
        reading = system_reading(total)
    else:
        raise RuntimeError("the selected vLLM backend does not report free memory")

print(json.dumps(reading))
