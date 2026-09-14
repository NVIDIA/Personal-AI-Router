<!--
SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
SPDX-License-Identifier: Apache-2.0
-->

# A/B benchmark: one node, the other node, both

`ab_bench.py` measures MLX inference on each cluster node in turn and then on
both at once, so "is the second machine worth it" has an answer instead of an
opinion. Standard library only — it runs on either laptop with nothing installed.

## Run it

```bash
# 1. one node, then the other, then both (the default)
python3 tests/ab_bench.py \
  --model-a ~/models/<model> \
  --model-b /Users/<peer>/models/<model> \
  --name-a "$(scutil --get LocalHostName)" --name-b peer

# 2. just one side while you change something
python3 tests/ab_bench.py --mode a  --model-a … --model-b …

# 3. heavier sample, keep the raw numbers
python3 tests/ab_bench.py --repeat 5 --max-tokens 256 \
  --prompt-sizes 8 500 2000 --json run.json --model-a … --model-b …

# 4. how much context each node can actually serve
python3 tests/ab_bench.py --context-probe --model-a … --model-b …
```

`make ab` runs the default sweep with the ids currently loaded.

## What it reports

| column | meaning |
|---|---|
| `ttft p50/p95` | ms to the first token **with content** — an empty role-only delta is protocol overhead, not a token |
| `decode/s` | tokens/s while generating, excluding the wait for token one — the model's steady-state rate |
| `e2e/s` | completion tokens ÷ total request time — what a caller actually experiences |
| `total p50/p95` | full request duration, ms |
| `aggregate` | tokens produced per **wall-clock** second for the whole run |

Prompt sizes appear as `500w/528t` — words fed in, and the token count **the
server itself reported**. Throughput is never computed from a client-side token
estimate.

## Two things worth knowing before you read a result

**Node attribution is measured, not assumed.** Every response carries
`system_fingerprint`, which embeds the OS version and GPU — `macOS-26.4/applegpu_g17s`
against `macOS-15.0.1/applegpu_g13g`. The tool reports the fingerprint of each
individual response, so a `both` run *proves* the work was split rather than
claiming it.

**The model id selects the node here.** `mlx-pool` binds loopback only, so a
peer's engine isn't directly reachable; everything goes through the router
(`:8090`). A locally built model is advertised by its absolute path, and that
path contains the **owner's** home directory — so `/Users/<a>/models/X` and
`/Users/<b>/models/X` are the same weights on two different machines.

That also means the two nodes serve the **same weights under different ids**, so
the router cannot load-balance one id across both. `both` therefore drives the
two ids concurrently and measures aggregate cluster throughput.

One-id balancing needs the same id on each node — which needs **no download**,
because the weights are already there. A directory model's id *is* its absolute
path, and the paths differ only by the home directory, so the cheap fix is to put
the model at a home-independent path on both machines:

```bash
# on each node, same path on both — an APFS clone, so no extra disk
sudo mkdir -p /Users/Shared/models
cp -c -R ~/models/<model> /Users/Shared/models/
# then point the engine at it (manifest runtime env, or MLX_MODELS_DIRS)
```

Both then advertise `/Users/Shared/models/Qwen3-VL-8B-Instruct-4bit`. Note a
symlink will **not** do: `scanModelsDir` lists with `os.ReadDir` and skips entries
that are not directories, and a symlink reports as a link.

**But check whether you want this at all.** On this pair the benchmark says
`both` is ~20% *slower* than the fast node alone, so letting the router split one
id evenly across a fast and a 3× slower node is the thing that caused the
regression. One-id balancing pays off when the nodes are comparable; when they
are not, pinning the model to the fast node is the better configuration.

## Interpreting `both`

The closing table compares aggregate throughput, because per-request speed
necessarily drops when a slower node takes half the work — comparing decode
rates would make a faster cluster look worse.

Measured on one such pair (M5 Max vs M1 16 GB, Qwen3-VL-8B-Instruct-4bit,
4 requests each; node A is the M5 Max):

```
node A             27.4 tok/s
node B             10.2 tok/s
both (concurrent)  21.3 tok/s
-> -22% vs node A alone
```

Two nodes were **slower than one**. Work is split evenly while the nodes are
~3× apart in speed, so wall time is set by the slower one and the fast node
finishes early and idles. An even split across unequal nodes is a scheduling
choice that costs throughput; the fix is a split weighted by measured
throughput, or keeping the model on the fast node and using the second machine
for different work.

This is the number to re-check after any routing change.

## Caveats

- `prefill/s` (in the JSON, not the table) divides prompt tokens by time-to-first-token,
  which also contains queueing and template rendering. Fine for comparing nodes
  on identical prompts; not an absolute prefill rate.
- `temperature: 0` so repeated runs compare like with like. Sampling speed is
  unaffected, but generated text will be repetitive — that is expected.
- The context probe reports what the node **served**, which can be far below the
  architecture's declared `max_position_embeddings` (262144 for this model): a
  KV-cache budget or simply free memory can cap it first.
- A run where every request failed exits non-zero rather than printing zeros.
