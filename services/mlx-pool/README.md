<!--
SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
SPDX-License-Identifier: Apache-2.0
-->

# mlx-pool

An OpenAI-compatible front end that keeps **up to N** `mlx_lm.server` processes
alive and routes each request to the one holding the requested model, evicting
the least recently used when a new model is needed.

## Why it exists

`mlx_lm.server` holds exactly one model. Asking it for another unloads the
weights and loads the new ones — measured at **7.2 s between two small models**,
and far worse for large ones. PAIR's router works around that by preferring a
node that already holds the model, but on a single machine, or when every node
is busy with something else, someone still has to pay the swap.

This moves that decision from "whoever asks last wins" to a bounded cache: the
models you actually alternate between stay resident, and only the (N+1)th one
costs a load.

## Why a separate binary rather than logic in engine-manager

`nvpair-engine-manager`'s contract is that adding an engine is a JSON manifest,
not code. A pool that spawns and evicts child processes is engine-specific by
nature, so putting it there would be the first exception to that rule.

Instead PAIR starts *this* as the engine: one process, one port, the same
`/health` and `/v1/models` and `/v1/chat/completions` surface `mlx_lm.server`
offers. Nothing upstream — the manifest schema, `mlx-proxy`, the broker, the
desktop app — knows the difference.

## How many models

`--max-models`, defaulting to `$MLX_MAX_MODELS`, defaulting to 2. The MLX
manifest sets it in `runtime.env`, so a per-user override is a two-line file:

```json
{ "engine": "mlx", "runtime": { "env": { "MLX_MAX_MODELS": "3" } } }
```

dropped in the per-user `engines/` directory, where it deep-merges onto the
bundled manifest.

**The cap is a count, not a memory budget.** Two 27B models will exhaust a
machine that three 4B models would not. Size it for the models you actually
run; there is no accounting here that can save you from setting it to 4 on a
16 GB Mac.

## Surface

| Route | Behaviour |
|---|---|
| `GET /health` | `{"status":"ok","models":[{"id":…}]}` — every resident model, most recently used first. Answers immediately, with an empty list, before any child exists. |
| `GET /v1/models` | The downloadable catalogue, scanned from the Hugging Face cache directly so it works with no child running. |
| `POST /v1/chat/completions`, `/v1/completions` | Routed to the child holding `model`, spawning or evicting as needed. |
| anything else | 404 |

## What it costs at `--max-models 1`

The shipped default is **1**, and at 1 this component is *slower* than the thing
it wraps. Measured on an M5 Max, alternating two small models, 8 switches
(`bench/switch_ab.py`):

| arm | median switch | min | max |
|---|---|---|---|
| `mlx-pool --max-models 1` | **1.62 s** | 1.24 s | 1.80 s |
| plain `mlx_lm.server` (in-process swap) | **0.61 s** | 0.42 s | 0.79 s |

**+1.01 s per switch.** That is a Python interpreter start, imports and server
bootstrap that an in-process unload/reload does not pay.

Two things to read alongside it. The overhead is **fixed**, not proportional:
these are 1B and 0.5B models chosen precisely because they make the pool look
worst — the load itself is under a second, so startup dominates. Against the
3-bit 27B, whose load is ~10 s, the same overhead is under 10%.

And at N=1 the pool buys one real thing: **deterministic reclamation**. Evicting
kills the process, so the weights are returned to the OS rather than left to
Python and MLX to release. On a box already at the edge of its memory that is
the difference between a swap and an OOM.

But the honest summary is that N=1 is a *safety* setting, not a performance one.
The component earns its keep at **N ≥ 2**, where a request for an already-hot
model costs nothing at all instead of a full reload — which is the case the
router's residency preference is trying to create in the first place.
