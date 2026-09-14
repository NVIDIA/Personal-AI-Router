#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
# SPDX-License-Identifier: Apache-2.0
"""A/B benchmark for MLX inference across PAIR cluster nodes.

Measures one node, then the other, then both together, and reports time to first
token, decode and prefill throughput, end-to-end latency and context limits.

WHY IT ROUTES BY MODEL ID
-------------------------
mlx-pool binds loopback only, so a peer's engine is not reachable directly from
here. Everything therefore goes through the router (mlx-proxy, :8090) and the
MODEL ID selects the node: a locally built model is advertised by absolute path,
and that path contains the OWNER's home directory, so `/Users/<a>/models/X` and
`/Users/<b>/models/X` are the same weights on two different machines. That is a
property of how such models are addressed, not a design choice -- see the note
on `both` in `--help`.

HOW A NODE IS ATTRIBUTED
------------------------
Not by which id was asked for -- that would assume the answer. Every response
carries `system_fingerprint`, which embeds the OS version and GPU
(`macOS-26.4-...-applegpu_g17s` vs `macOS-15.0.1-...-applegpu_g13g`). The
fingerprint of each individual response is what this reports, so a `both` run
proves the split actually happened rather than asserting it.

Streaming is used for every request because time to first token cannot be
recovered from a buffered response, and `stream_options.include_usage` returns
the server's own token counts -- so throughput is computed from what the model
actually tokenized, never from a client-side estimate.

Standard library only: it has to run on either laptop with nothing installed.
"""

from __future__ import annotations

import argparse
import json
import re
import statistics
import sys
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field, asdict
from typing import Any, Iterable

DEFAULT_ROUTER = "http://127.0.0.1:8090"

# Prompts are sized in words rather than tokens: the exact token count comes back
# from the server in `usage`, so the only job here is to span a useful range of
# context lengths deterministically.
PROMPT_WORD_SIZES = (8, 200, 1000)

FILLER = (
    "The quick brown fox jumps over the lazy dog while the engineer measures "
    "throughput latency and context window behaviour on a local inference node. "
)


@dataclass
class Sample:
    """One completed request."""

    ok: bool
    node: str  # from system_fingerprint, or "" when unknown
    model: str
    prompt_words: int
    ttft_ms: float | None = None
    total_ms: float | None = None
    prompt_tokens: int | None = None
    completion_tokens: int | None = None
    error: str = ""

    @property
    def decode_tps(self) -> float | None:
        """Tokens per second while generating, excluding the wait for token one.

        Reported separately from the end-to-end rate because they answer
        different questions: decode speed is the model's steady-state generation
        rate, while the end-to-end rate is what a caller actually experiences,
        prefill and queueing included.
        """
        if not self.ok or not self.completion_tokens or self.ttft_ms is None:
            return None
        gen_ms = (self.total_ms or 0) - self.ttft_ms
        if gen_ms <= 0 or self.completion_tokens <= 1:
            return None
        # The first token arrived at ttft, so it is not part of the decode window.
        return (self.completion_tokens - 1) / (gen_ms / 1000.0)

    @property
    def prefill_tps(self) -> float | None:
        """Prompt tokens per second, inferred from time to first token.

        An upper bound on prompt processing rather than a pure prefill number:
        ttft also contains queueing, template rendering and the first decode
        step. Useful for comparing nodes on identical prompts, not as an absolute.
        """
        if not self.ok or not self.prompt_tokens or not self.ttft_ms:
            return None
        return self.prompt_tokens / (self.ttft_ms / 1000.0)

    @property
    def e2e_tps(self) -> float | None:
        if not self.ok or not self.completion_tokens or not self.total_ms:
            return None
        return self.completion_tokens / (self.total_ms / 1000.0)


@dataclass
class Target:
    name: str
    base_url: str
    model: str
    node: str = ""  # discovered fingerprint


@dataclass
class Result:
    label: str
    samples: list[Sample] = field(default_factory=list)

    def ok(self) -> list[Sample]:
        return [s for s in self.samples if s.ok]


def short_node(fingerprint: str) -> str:
    """A readable node tag from mlx-lm's system_fingerprint.

    The fingerprint is `<mlx-lm>-<mlx>-<platform>-<gpu>`; the platform and GPU
    are what differ between machines, so they are what identify one.
    """
    if not fingerprint:
        return "?"
    # The version follows "macOS" as its own hyphen-separated field, so a plain
    # startswith() match returns the bare word and silently drops the number that
    # distinguishes the machines.
    m = re.search(r"macOS-[\d.]+", fingerprint)
    os_part = m.group(0) if m else ""
    idx = fingerprint.find("applegpu")
    gpu = fingerprint[idx:] if idx >= 0 else ""
    tag = "/".join(p for p in (os_part, gpu) if p)
    return tag or fingerprint[:24]


def build_prompt(words: int) -> str:
    filler_words = FILLER.split()
    out: list[str] = []
    while len(out) < words:
        out.extend(filler_words)
    return " ".join(out[:words])


def stream_request(
    target: Target, prompt: str, max_tokens: int, timeout: float, prompt_words: int
) -> Sample:
    """Issue one streaming completion and time it.

    Timing starts before the request is sent, so `ttft` includes connection and
    queueing -- that is the number a caller feels, and excluding them would
    flatter the router.
    """
    body = json.dumps(
        {
            "model": target.model,
            "messages": [{"role": "user", "content": prompt}],
            "max_tokens": max_tokens,
            "stream": True,
            "stream_options": {"include_usage": True},
            # Deterministic so repeated runs compare like with like.
            "temperature": 0.0,
        }
    ).encode()
    req = urllib.request.Request(
        f"{target.base_url}/v1/chat/completions",
        data=body,
        headers={"Content-Type": "application/json"},
    )

    sample = Sample(ok=False, node=target.node, model=target.model, prompt_words=prompt_words)
    start = time.perf_counter()
    first_token_at: float | None = None
    fingerprint = ""
    usage: dict[str, Any] = {}

    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            for raw in resp:
                line = raw.decode("utf-8", "replace").strip()
                # SSE: blank separators and `:` comments (the server's keepalives)
                # carry no payload and must not be mistaken for a token.
                if not line or line.startswith(":"):
                    continue
                if not line.startswith("data:"):
                    continue
                payload = line[5:].strip()
                if payload == "[DONE]":
                    break
                try:
                    chunk = json.loads(payload)
                except json.JSONDecodeError:
                    continue
                fingerprint = chunk.get("system_fingerprint") or fingerprint
                if chunk.get("usage"):
                    usage = chunk["usage"]
                for choice in chunk.get("choices", []):
                    delta = choice.get("delta") or {}
                    # First chunk with actual content: an empty role-only delta is
                    # protocol overhead, not a token, and counting it would report
                    # a time to first token the user never experiences.
                    if delta.get("content") and first_token_at is None:
                        first_token_at = time.perf_counter()
        end = time.perf_counter()
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        sample.error = f"{type(exc).__name__}: {exc}"
        sample.total_ms = (time.perf_counter() - start) * 1000
        return sample

    sample.ok = True
    sample.node = short_node(fingerprint) if fingerprint else target.node
    sample.total_ms = (end - start) * 1000
    sample.ttft_ms = (first_token_at - start) * 1000 if first_token_at else None
    sample.prompt_tokens = usage.get("prompt_tokens")
    sample.completion_tokens = usage.get("completion_tokens")
    return sample


def discover_node(target: Target, timeout: float) -> str:
    """Ask the target who serves it, so runs are labelled by fact not assumption."""
    s = stream_request(target, "hi", max_tokens=1, timeout=timeout, prompt_words=1)
    return s.node if s.ok else ""


def pct(values: list[float], p: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    if len(ordered) == 1:
        return ordered[0]
    k = (len(ordered) - 1) * p
    lo, hi = int(k), min(int(k) + 1, len(ordered) - 1)
    return ordered[lo] + (ordered[hi] - ordered[lo]) * (k - lo)


def summarize(values: Iterable[float | None]) -> dict[str, float | None]:
    vals = [v for v in values if v is not None]
    if not vals:
        return {"n": 0, "mean": None, "p50": None, "p95": None, "min": None, "max": None}
    return {
        "n": len(vals),
        "mean": statistics.fmean(vals),
        "p50": pct(vals, 0.50),
        "p95": pct(vals, 0.95),
        "min": min(vals),
        "max": max(vals),
    }


def run_sequential(target: Target, args: argparse.Namespace) -> Result:
    res = Result(label=target.name)
    # Wall time is recorded for the single-node runs too, not just the concurrent
    # one. Without it there is nothing to compare `both` against: "18 requests in
    # 91s" only means something next to "the same work on one node took N".
    started = time.perf_counter()
    for words in args.prompt_sizes:
        prompt = build_prompt(words)
        for _ in range(args.repeat):
            res.samples.append(
                stream_request(target, prompt, args.max_tokens, args.timeout, words)
            )
    setattr(res, "wall_s", time.perf_counter() - started)
    return res


def run_concurrent(targets: list[Target], args: argparse.Namespace) -> Result:
    """Drive every target at once: the question `both` actually asks.

    Aggregate throughput is the point -- two nodes serving in parallel should beat
    either alone, and the per-node split in the output is what shows whether the
    work really landed on both.
    """
    res = Result(label="both (concurrent)")
    jobs: list[tuple[Target, str, int]] = []
    for words in args.prompt_sizes:
        prompt = build_prompt(words)
        for _ in range(args.repeat):
            for t in targets:
                jobs.append((t, prompt, words))

    started = time.perf_counter()
    with ThreadPoolExecutor(max_workers=max(2, len(targets) * args.concurrency)) as pool:
        futures = [
            pool.submit(stream_request, t, p, args.max_tokens, args.timeout, w)
            for (t, p, w) in jobs
        ]
        res.samples = [f.result() for f in futures]
    res_wall = time.perf_counter() - started
    setattr(res, "wall_s", res_wall)
    return res


def probe_context(target: Target, args: argparse.Namespace) -> dict[str, Any]:
    """Find the largest prompt the node accepts, by binary search on word count.

    Empirical rather than read from config.json: the served limit is what a
    caller can actually use, and it can be lower than the architecture's
    `max_position_embeddings` (a server flag, a KV-cache budget, or free memory
    can all cap it first). Reports the token count the server itself reported for
    the largest accepted prompt, not the word count fed in.
    """
    lo, hi = 0, args.context_max_words
    best: dict[str, Any] = {"accepted_words": 0, "accepted_prompt_tokens": None, "failed_at": None}

    # Grow first, so a node with a small limit is found without paying for the
    # full binary search range.
    #
    # Progress is printed and flushed: a single probe at tens of thousands of
    # tokens takes minutes on a small machine, and a silent tool that long is
    # indistinguishable from a hung one.
    probe = 256
    while probe <= hi:
        print(f"    {target.name}: trying {probe} words...", flush=True)
        s = stream_request(target, build_prompt(probe), 1, args.context_timeout, probe)
        if not s.ok:
            best["failed_at"] = probe
            hi = probe - 1
            break
        best["accepted_words"] = probe
        best["accepted_prompt_tokens"] = s.prompt_tokens
        print(f"      accepted ({s.prompt_tokens} prompt tokens)", flush=True)
        lo = probe
        probe *= 4
    else:
        return best  # never failed within the ceiling

    while lo < hi:
        mid = (lo + hi + 1) // 2
        print(f"    {target.name}: narrowing, trying {mid} words...", flush=True)
        s = stream_request(target, build_prompt(mid), 1, args.context_timeout, mid)
        if s.ok:
            best["accepted_words"] = mid
            best["accepted_prompt_tokens"] = s.prompt_tokens
            lo = mid
        else:
            best["failed_at"] = mid
            hi = mid - 1
    return best


def fmt(v: float | None, digits: int = 1) -> str:
    return "-" if v is None else f"{v:.{digits}f}"


def print_result(res: Result, args: argparse.Namespace) -> None:
    ok = res.ok()
    failed = [s for s in res.samples if not s.ok]
    print(f"\n=== {res.label} ===")
    if not ok:
        print("  no successful requests")
        for s in failed[:3]:
            print(f"    error: {s.error}")
        return

    by_node: dict[str, int] = {}
    for s in ok:
        by_node[s.node] = by_node.get(s.node, 0) + 1
    print(f"  served by: {', '.join(f'{n} x{c}' for n, c in sorted(by_node.items()))}")
    if failed:
        print(f"  failures:  {len(failed)}/{len(res.samples)}  e.g. {failed[0].error[:80]}")

    print(
        f"  {'prompt':<12} {'ttft p50':>9} {'p95':>8} "
        f"{'decode/s':>9} {'p95':>8} {'e2e/s':>8} {'total p50':>10} {'p95':>9}"
    )
    for words in args.prompt_sizes:
        rows = [s for s in ok if s.prompt_words == words]
        if not rows:
            continue
        ttft = summarize(s.ttft_ms for s in rows)
        dec = summarize(s.decode_tps for s in rows)
        e2e = summarize(s.e2e_tps for s in rows)
        tot = summarize(s.total_ms for s in rows)
        ptoks = [s.prompt_tokens for s in rows if s.prompt_tokens]
        label = f"{words}w" + (f"/{ptoks[0]}t" if ptoks else "")
        print(
            f"  {label:<12} {fmt(ttft['p50'],0):>9} {fmt(ttft['p95'],0):>8} "
            f"{fmt(dec['p50']):>9} {fmt(dec['p95']):>8} {fmt(e2e['p50']):>8} "
            f"{fmt(tot['p50'],0):>10} {fmt(tot['p95'],0):>9}"
        )

    wall = getattr(res, "wall_s", None)
    total_completion = sum(s.completion_tokens or 0 for s in ok)
    if wall:
        # The number that decides whether two nodes were worth it: tokens the
        # CLUSTER produced per wall-clock second, not per-request speed.
        print(f"  aggregate: {total_completion} tokens in {wall:.1f}s "
              f"= {total_completion / wall:.1f} tok/s across {len(ok)} requests")


def print_comparison(results: list[Result]) -> None:
    """State whether adding the second node actually bought anything.

    Aggregate throughput is the only fair basis: per-request speed necessarily
    drops when a slower node takes half the work, so comparing decode rates would
    make a faster cluster look worse. What matters is tokens per wall-clock
    second for the same body of work.
    """
    rates: list[tuple[str, float]] = []
    for r in results:
        wall = getattr(r, "wall_s", None)
        toks = sum(s.completion_tokens or 0 for s in r.ok())
        if wall and toks:
            rates.append((r.label, toks / wall))
    if len(rates) < 2:
        return

    print("\n=== aggregate throughput (same work, wall clock) ===")
    best_single = max((r for r in rates if not r[0].startswith("both")), key=lambda x: x[1],
                      default=None)
    for label, rate in rates:
        bar = "#" * max(1, round(rate / max(r[1] for r in rates) * 40))
        print(f"  {label:<22} {rate:7.1f} tok/s  {bar}")
    both = next((r for r in rates if r[0].startswith("both")), None)
    if not (both and best_single):
        return
    delta = (both[1] / best_single[1] - 1) * 100
    if delta > 15:
        verdict = "the second node is carrying real load"
    elif delta >= -15:
        verdict = "no material gain; the fast node finishes early and waits"
    else:
        # Splitting work evenly across unequal nodes means wall time is set by the
        # slower one, so the cluster can finish LATER than the fast node would
        # have alone. That is a scheduling result, not measurement noise, and
        # calling it noise would hide the one thing this benchmark exists to find.
        verdict = (
            "SLOWER than one node: an even split across unequal nodes is bounded "
            "by the slower one. Weight the split by measured throughput, or keep "
            "this model on the fast node only"
        )
    print(f"  -> {delta:+.0f}% vs {best_single[0]} alone -- {verdict}")


def main() -> int:
    ap = argparse.ArgumentParser(
        description="A/B benchmark MLX nodes through the PAIR router.",
        epilog=(
            "NOTE ON `both`: the two nodes here advertise the same weights under "
            "DIFFERENT ids, because a locally built model is addressed by its "
            "absolute path and that path contains the owner's home directory. So "
            "`both` drives the two ids concurrently and measures aggregate cluster "
            "throughput. For the router to load-balance ONE id across both nodes, "
            "the model must be present under the same id on each -- which needs no "
            "download if the weights are already on both: put the directory at a "
            "home-independent path (e.g. /Users/Shared/models/...) on each node, "
            "since a directory model's id IS its absolute path. Check the "
            "aggregate table first, though: when the nodes are far apart in speed, "
            "splitting one id evenly across them is what makes the cluster slower."
        ),
    )
    ap.add_argument("--router", default=DEFAULT_ROUTER, help="mlx-proxy base URL")
    ap.add_argument("--model-a", required=True, help="model id served by node A")
    ap.add_argument("--model-b", required=True, help="model id served by node B")
    ap.add_argument("--name-a", default="node A")
    ap.add_argument("--name-b", default="node B")
    ap.add_argument(
        "--mode",
        default="all",
        choices=["a", "b", "both", "all"],
        help="'all' runs A, then B, then both (default)",
    )
    ap.add_argument("--repeat", type=int, default=3, help="requests per prompt size")
    ap.add_argument("--max-tokens", type=int, default=128)
    ap.add_argument(
        "--prompt-sizes",
        type=int,
        nargs="+",
        default=list(PROMPT_WORD_SIZES),
        help="prompt sizes in words",
    )
    ap.add_argument("--concurrency", type=int, default=1, help="parallel requests per target in 'both'")
    ap.add_argument("--timeout", type=float, default=300.0)
    ap.add_argument("--context-probe", action="store_true", help="find each node's usable context")
    ap.add_argument("--context-max-words", type=int, default=200_000)
    ap.add_argument("--context-timeout", type=float, default=600.0)
    ap.add_argument("--json", dest="json_out", help="write full results here")
    args = ap.parse_args()

    a = Target(args.name_a, args.router.rstrip("/"), args.model_a)
    b = Target(args.name_b, args.router.rstrip("/"), args.model_b)

    print("Discovering which node answers for each model id...")
    for t in (a, b):
        t.node = discover_node(t, args.timeout)
        print(f"  {t.name:<10} {t.model}\n             -> {t.node or 'UNREACHABLE'}")
    if a.node and a.node == b.node:
        print(
            "\n  WARNING: both ids resolved to the SAME node. An A/B across one "
            "machine measures nothing about the cluster; check that the peer's "
            "engine is running and its model is loaded.",
            file=sys.stderr,
        )

    results: list[Result] = []
    if args.mode in ("a", "all"):
        print(f"\nRunning {a.name} alone...")
        results.append(run_sequential(a, args))
    if args.mode in ("b", "all"):
        print(f"Running {b.name} alone...")
        results.append(run_sequential(b, args))
    if args.mode in ("both", "all"):
        print("Running both concurrently...")
        results.append(run_concurrent([a, b], args))

    for res in results:
        print_result(res, args)

    print_comparison(results)

    context: dict[str, Any] = {}
    if args.context_probe:
        print("\n=== usable context (empirical) ===")
        for t in (a, b):
            got = probe_context(t, args)
            context[t.name] = got
            print(
                f"  {t.name:<10} accepted {got['accepted_words']} words "
                f"= {got['accepted_prompt_tokens']} prompt tokens"
                + (f", refused at {got['failed_at']} words" if got["failed_at"] else
                   f" (no limit found below {args.context_max_words} words)")
            )

    if args.json_out:
        payload = {
            "router": args.router,
            "targets": [asdict(t) for t in (a, b)],
            "settings": {
                "repeat": args.repeat,
                "max_tokens": args.max_tokens,
                "prompt_sizes": args.prompt_sizes,
                "concurrency": args.concurrency,
            },
            "runs": [
                {
                    "label": r.label,
                    "wall_s": getattr(r, "wall_s", None),
                    "samples": [
                        {**asdict(s), "decode_tps": s.decode_tps, "e2e_tps": s.e2e_tps,
                         "prefill_tps": s.prefill_tps}
                        for s in r.samples
                    ],
                }
                for r in results
            ],
            "context": context,
        }
        with open(args.json_out, "w") as fh:
            json.dump(payload, fh, indent=2)
        print(f"\nwrote {args.json_out}")

    # A run where nothing succeeded is a failed run, not a report of zeros.
    return 0 if any(r.ok() for r in results) else 1


if __name__ == "__main__":
    sys.exit(main())
