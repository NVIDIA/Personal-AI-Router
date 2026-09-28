<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Fresh cable observation primitive

Development-only, pure byte parsing and reviewed-port resolution. This package
does not open sockets, send traffic, elevate privileges, run a coordinator or
provide a callable product feature. Its synthetic tests are not cable evidence.

The decoder accepts only the proposed fixed diagnostic LLDP profile. The run
marker correlates observations; it is not authentication. Ingress resolution
requires caller-supplied kernel metadata and an immutable authenticated review.
It cannot infer packet age, reject cross-call replay, prove a reciprocal edge,
exclude nonconformant bridging or confirm cleanup on its own.

Still required: bounded receive/transmit adapter; replay/freshness state; pinned
participant/coordinator control; idempotent review consumption; deadline/cancel
and cleanup handling; product-owned privilege approval; native current-packet
and unplug/replug tests. Missing privilege must block before effects. Existing
cached observations remain separately labelled and do not satisfy these gates.
