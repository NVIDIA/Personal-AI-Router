#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
# SPDX-License-Identifier: Apache-2.0
"""Is mlx-pool at --max-models 1 worse than plain mlx_lm.server?

At N=1 the pool cannot keep a second model warm, so every cross-model request
pays a process teardown, a Python start, imports, server bootstrap and health
polling -- where mlx_lm.server pays only its own unload/reload inside a live
interpreter. That is a real objection and it deserves a number, not an opinion.

Both arms alternate the same two models over the same sequence, back to back.
"""
import json, statistics, subprocess, sys, time, socket, urllib.request

A = "mlx-community/Llama-3.2-1B-Instruct-4bit"
B = "mlx-community/Qwen2.5-0.5B-Instruct-4bit"
ROUNDS = 4


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def ask(port, model, timeout=900):
    body = json.dumps({"model": model, "messages": [{"role": "user", "content": "hi"}],
                       "max_tokens": 1, "temperature": 0}).encode()
    req = urllib.request.Request(f"http://127.0.0.1:{port}/v1/chat/completions", data=body,
                                 headers={"Content-Type": "application/json"})
    t0 = time.time()
    with urllib.request.urlopen(req, timeout=timeout) as r:
        json.load(r)
    return time.time() - t0


def wait_up(port, limit=120):
    for _ in range(limit * 4):
        try:
            urllib.request.urlopen(f"http://127.0.0.1:{port}/health", timeout=2).read()
            return True
        except Exception:
            time.sleep(0.25)
    return False


def run(argv, port):
    proc = subprocess.Popen(argv, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        if not wait_up(port):
            return None
        ask(port, A)  # warm: both arms start holding A, so only switches are timed
        out = []
        for _ in range(ROUNDS):
            out.append(ask(port, B))
            out.append(ask(port, A))
        return out
    finally:
        proc.terminate()
        try:
            proc.wait(60)
        except Exception:
            proc.kill()


pool_bin, server_bin = sys.argv[1], sys.argv[2]
p1, p2 = free_port(), free_port()

pool = run([pool_bin, "--port", str(p1), "--child-port-base", "8300",
            "--max-models", "1", "--server-bin", server_bin], p1)
plain = run([server_bin, "--host", "127.0.0.1", "--port", str(p2), "--log-level", "ERROR"], p2)

print(f"\n{2 * ROUNDS} model switches, same two models, alternating\n")
print(f"{'arm':<40}{'median':>9}{'min':>9}{'max':>9}")
for label, s in (("mlx-pool --max-models 1", pool), ("plain mlx_lm.server (in-process swap)", plain)):
    if not s:
        print(f"{label:<40}   never came up")
        continue
    print(f"{label:<40}{statistics.median(s):>8.2f}s{min(s):>8.2f}s{max(s):>8.2f}s")
if pool and plain:
    d = statistics.median(pool) - statistics.median(plain)
    print(f"\npool costs {d:+.2f}s per switch vs the in-process swap")
