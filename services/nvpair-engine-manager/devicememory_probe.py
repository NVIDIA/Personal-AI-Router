# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import json

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


def shares_system_memory(total):
    system_total = int(psutil.virtual_memory().total)
    return abs(total - system_total) <= max(1 << 30, system_total // 20)


if torch.cuda.is_available():
    free, total = torch.cuda.mem_get_info(0)
    total = int(total)
    reading = {"total": total, "free": int(free), "shared": shares_system_memory(total)}
elif hasattr(torch, "xpu") and torch.xpu.is_available():
    free, total = torch.xpu.mem_get_info(0)
    total = int(total)
    reading = {"total": total, "free": int(free), "shared": shares_system_memory(total)}
else:
    total = int(current_platform.get_device_total_memory(0))
    available = getattr(current_platform, "get_device_available_memory", None)
    if callable(available):
        free = int(available(0))
        reading = {"total": total, "free": free, "shared": shares_system_memory(total)}
    elif current_platform.is_cpu():
        reading = system_reading(total)
    else:
        if not shares_system_memory(total):
            raise RuntimeError("the selected vLLM backend does not report free memory")
        reading = system_reading(total)

print(json.dumps(reading))
