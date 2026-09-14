<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
SPDX-License-Identifier: Apache-2.0
-->

# PAIR — MLX fork

A fork of [NVIDIA Personal AI Router](https://github.com/NVIDIA/Personal-AI-Router)
that adds Apple [MLX](https://github.com/ml-explore/mlx-lm) as a third inference
engine on `darwin/arm64`. Ollama and LM Studio are untouched.

![Two Macs paired in one PAIR cluster, MLX running on both. The second node's model
list offers every model held only by the first, each with a Get from button.](assets/mlx-two-nodes.png)

For what PAIR is, how to install it, and everything not listed below, read
[upstream's README](https://github.com/NVIDIA/Personal-AI-Router#readme). This
file covers only what the fork changes.

## Quick start

```bash
make mlx          # install the MLX engine (fetches uv, builds a venv, installs mlx-lm)
make mlx-pull     # download a model
make run          # open the desktop app

make mlx-serve    # or stay in the terminal: start the router
make mlx-ask      # and send a request through it
```

[docs/mlx.mdx](docs/mlx.mdx) is the full guide.

## What the fork adds

**An MLX engine.** `mlx_lm.server` holds exactly one model and has no unload, so
the fork runs `mlx-pool` in front of it: one model per child process, least
recently used evicted, and ending a child *is* the unload. A model serving a
request is never evicted.

**Routing that prefers residency.** Because a node holds one model at a time,
sending a request to a node that must reload first is the expensive mistake.
`mlx-proxy` routes to an owner that already has the model resident and falls
back to on-disk owners only when nobody does. Measured against upstream's
load-only behaviour: **100.0% resident hits vs 31.8%**, against a **7.2 s**
reload — `go test -run TestRoutingPolicyAB` runs both arms back to back.

**Model transfer between nodes.** A model you quantized yourself has no repo id
and is addressed by path, which the cache transfer could not carry. Directory
models now transfer over the cluster's mTLS link, each file verified against a
digest before it lands, resumable, and over several connections.

**A benchmark.** [`tests/ab_bench.py`](tests/README.md) measures one node, the
other, then both — time to first token, decode rate, end-to-end latency and
context — attributing every response to a node by its `system_fingerprint`.

## mlx-lm

The engine installs `mlx-lm` from PyPI at a pinned version. Upstream's
OpenAI-compatible server has known gaps in tool calling and thinking-mode
handling; if you carry a fork that fixes them, point the engine at it with a user
manifest rather than editing the bundled one — see
[Running a different mlx-lm](docs/mlx.mdx).

## Scope

MLX is Apple Silicon only. PAIR routes each independent request to one node; it
does not pool GPU memory, shard a model across machines, or split an in-flight
request. Nothing here changes that.

## License

Apache-2.0, as upstream. See [LICENSE](LICENSE) and
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). Files inherited from upstream
keep NVIDIA's copyright notice; files added by this fork carry their author's.
