#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Start the relays, then hand the terminal to nvpair-tui.
#
# The TUI is PID 1's child rather than a background process because it IS the node: it
# supervises the broker, and the broker owns every worker. If it exits, the pod should end and
# be restarted, not linger with a dead node and healthy-looking relays.
set -uo pipefail

: "${PAIR_OPENAI_PORT:=1234}"
: "${PAIR_OLLAMA_PORT:=11434}"
: "${PAIR_RELAY_OPENAI_PORT:=18434}"
: "${PAIR_RELAY_OLLAMA_PORT:=18435}"

# An arbitrary UID has no passwd entry, and some tooling wants one. Best-effort; PAIR itself
# only needs HOME, which is set in the image.
if ! whoami >/dev/null 2>&1 && [ -w /etc/passwd ]; then
  echo "pair:x:$(id -u):0:PAIR node:${HOME}:/sbin/nologin" >> /etc/passwd 2>/dev/null || true
fi

mkdir -p "${XDG_CONFIG_HOME}" "${XDG_CACHE_HOME}" "${XDG_RUNTIME_DIR}" 2>/dev/null || true

python3 /opt/pair/loopback-relay.py --port "${PAIR_RELAY_OPENAI_PORT}" --target-port "${PAIR_OPENAI_PORT}" &
python3 /opt/pair/loopback-relay.py --port "${PAIR_RELAY_OLLAMA_PORT}" --target-port "${PAIR_OLLAMA_PORT}" &

echo "pair: HOME=${HOME} uid=$(id -u) gid=$(id -g)"
echo "pair: data root = ${XDG_CONFIG_HOME}/Nvidia Corporation/Personal AI Router"
exec /opt/pair/bin/nvpair-tui "$@"
