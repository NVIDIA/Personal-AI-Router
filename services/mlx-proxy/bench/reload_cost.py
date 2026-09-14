#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
# SPDX-License-Identifier: Apache-2.0
"""Measure what a model switch costs a single mlx_lm.server, in seconds.

This is the physical constant behind mlx-proxy's residency-preferring routing.
TestRoutingPolicyAB (in the parent directory) measures how often each routing
arm lands a request on a node that already holds the model; this measures what
NOT landing on one costs. They multiply:

    latency saved per request = (miss-rate delta) x (reload seconds)

Method: alternate two models against one server. A request for the resident
model is a hit; a request for the other forces a full unload and reload. Both
are one-token completions, so generation time is negligible and the difference
is the swap. Hits and misses are interleaved rather than batched so that thermal
state, page cache, and any background load fall on both equally.

Usage:
    python3 reload_cost.py --server <path to mlx_lm.server> \\
        --model-a mlx-community/Llama-3.2-1B-Instruct-4bit \\
        --model-b mlx-community/Qwen2.5-0.5B-Instruct-4bit
"""

import argparse
import json
import socket
import statistics
import subprocess
import sys
import time
import urllib.error
import urllib.request


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def post(port, model, timeout=600):
    body = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": "hi"}],
        "max_tokens": 1,
        "temperature": 0,
        "stream": False,
    }).encode()
    req = urllib.request.Request(
        f"http://127.0.0.1:{port}/v1/chat/completions",
        data=body, headers={"Content-Type": "application/json"},
    )
    started = time.time()
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        json.load(resp)
    return time.time() - started


def resident(port):
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/health", timeout=10) as resp:
        return json.load(resp).get("model")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--server", required=True, help="path to the mlx_lm.server entry point")
    ap.add_argument("--model-a", required=True)
    ap.add_argument("--model-b", required=True)
    ap.add_argument("--rounds", type=int, default=5)
    args = ap.parse_args()

    port = free_port()
    proc = subprocess.Popen(
        [args.server, "--host", "127.0.0.1", "--port", str(port), "--log-level", "ERROR"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    try:
        for _ in range(120):
            try:
                resident(port)
                break
            except Exception:
                time.sleep(0.5)
        else:
            raise SystemExit("mlx_lm.server never became healthy")

        # Warm up: the first load of each model also pays for reading weights
        # off cold disk, which is not what a steady-state swap costs.
        post(port, args.model_a)
        post(port, args.model_b)
        post(port, args.model_a)

        hits, misses = [], []
        for _ in range(args.rounds):
            # A is resident here: this is the hit.
            hits.append(post(port, args.model_a))
            # B is not: this is the miss, and it leaves B resident.
            misses.append(post(port, args.model_b))
            # Back to A: another miss, and it restores the invariant for the
            # next round. Counted, because it is the same kind of event.
            misses.append(post(port, args.model_a))

        print(f"\nresident model after run: {resident(port)}")
        print(f"{'':<24}{'n':>4}{'median s':>12}{'min s':>10}{'max s':>10}")
        for label, samples in (("hit (already resident)", hits), ("miss (forces reload)", misses)):
            print(f"{label:<24}{len(samples):>4}{statistics.median(samples):>12.3f}"
                  f"{min(samples):>10.3f}{max(samples):>10.3f}")
        cost = statistics.median(misses) - statistics.median(hits)
        print(f"\nreload cost (median miss - median hit): {cost:.3f} s")
        print("Multiply by the miss-rate delta from TestRoutingPolicyAB for the")
        print("per-request latency the residency-preferring policy saves.")
        print("\nThese two models are small on purpose -- the number scales with")
        print("weight size, so a 30B swap costs far more than this figure.")
    finally:
        proc.terminate()
        try:
            proc.wait(20)
        except Exception:
            proc.kill()


if __name__ == "__main__":
    main()
