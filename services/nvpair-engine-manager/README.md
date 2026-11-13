<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-engine-manager

A config-driven control plane for local inference engines. The bundled
manifests support Ollama, LM Studio, llama.cpp, and vLLM. It manages everything about
an engine **except serving inference**: detect, user-mode install,
start/stop/restart, health, and config-declared actions. Adding an engine
is a JSON manifest, not code.

The bundled manifests under `manifests/` are the working reference for manifest
authoring.

### llama.cpp backend checkpoint

The `llamacpp` manifest runs `llama-server` in router mode on loopback port
`8081`. It can list, download, load, unload, and delete exact model ids such as
`owner/repository:Q4_K_M`. Downloads use `/models/sse` for progress; deletion
uses the router's native `DELETE /models` cache operation and does not restart
the router. `LLAMA_CACHE` points at the manifest's `models_dir`, `~/.llamacpp`,
which no removal this service performs will delete.
The llama.cpp `modelSource` setting selects Hugging Face (default) or ModelScope
for both its catalogue and downloads. The manager translates ModelScope metadata
through a loopback-only adapter and sets the router's `MODEL_ENDPOINT` only for
ModelScope; Hugging Face preserves existing endpoint and mirror settings.
llama.cpp still owns GGUF selection, caching, progress and cancellation.
ModelScope serves public, ungated models without forwarding Hugging Face
credentials. The adapter requires its listener's exact Host and a random private
path token. Downloads are bounded by the accepted file metadata and verified
against its SHA-256. The adapter advertises `Accept-Ranges: none` and returns
complete files, so interrupted downloads restart from zero and cached partial
data cannot bypass verification.
`engine:catalog` accepts an optional `modelSource` (`huggingface` or `modelscope`)
for llama.cpp. Clients browsing for a peer pass that peer's saved choice;
omitting it uses this node's setting. Queries cache each source separately.
Repositories that reach ModelScope's 3,000-entry file-list limit are excluded
rather than offered with incomplete GGUF shard metadata.
Readiness requires `/props` to report `role:"router"`, so model-selection
arguments that switch `llama-server` to single-model mode and incompatible
listeners already occupying the port are rejected rather than adopted.
After five minutes without inference work, a loaded model enters llama.cpp sleep
mode and releases its model and KV-cache memory; the next request wakes it. The
router child remains alive and can retain a residual backend GPU context.

Windows and Linux installs download checksum-pinned server and CUDA-runtime
archive pairs (CUDA 12.x for x64 and CUDA 13.4 for arm64). Apple silicon Macs use
the standard Metal-enabled archive; Intel Macs use the upstream x64 archive,
which is built without Metal and runs on the CPU. These on-demand downloads are
roughly 0.3–0.7 GiB for the CUDA archive pairs and about 11 MiB on macOS, and do
not enlarge the PAIR installer. llama.cpp keeps its `auto` GPU-layer policy:
supported NVIDIA/Metal devices can accelerate, while the dynamic CPU backend
remains the fallback. Hardware acceptance, not `/health` alone, is required to
claim GPU activation.

### vLLM

The `vllm` manifest covers Linux (`linux/amd64` and `linux/arm64`, NVIDIA GPU
with a CUDA 13 driver) and Apple Silicon macOS (`darwin/arm64`, macOS 15 or newer,
through the vLLM-Metal plugin). There is no native Windows path, so on Windows
`engine:get-installed` leaves vLLM out and PAIR does not offer it. The
macOS install builds a managed Python 3.12 venv with a sha256-pinned `uv` and
installs the pinned vLLM core and vLLM-Metal wheels, each checked against a
digest, so no compiler is needed. Every platform also pins `mistral_common==1.12.0`:
vLLM 0.32 asks only for 1.11.6 or newer, but 1.13 made an argument of the
tool-call grammar builder required that vLLM 0.32 does not pass, so with the
Mistral tool-call parser every request to a Mistral model failed. vLLM 0.33
makes the same call, so the pin changes only with an upgrade to a vLLM release
that itself requires `mistral_common` 1.13. `vllm serve` listens on loopback `8001`, and
the proxy facade owns `8000`. vLLM serves **one model per process** and has no
load, unload, pull or delete API of its own, so the manifest provides them as
manager actions: `load_model`, `unload_model`, `pull_model` and `delete_model`,
described below. PAIR passes requests straight to vLLM and does not load or
unload models on its own: a model is loaded with Load and released with Eject.

The default launch serves `Qwen/Qwen3.5-4B` through `--model <hugging-face-id>`.
The model thinks before it answers, and with no `--reasoning-parser` in the launch
that reasoning arrives in the reply ahead of the answer; adding
`--reasoning-parser qwen3` in the launch settings moves it to
`reasoning_content`. The model is about 9.3 GB of weights, so the default needs a GPU with at least 16 GB of
memory; on a smaller GPU, download and load a smaller model before starting.
Edit the `--model` argument, or use `load_model`, to serve another model. Only
`--host` and `--port` are managed controls, and there is no CORS control.
The context window and GPU share are chosen automatically at every start
(`auto_launch`, kind `hf_context`), not pinned in the saved launch. The manager
reads the model's `config.json` from the Hugging Face cache, at the revision
vLLM loads (`--revision`, or `main` through `refs/main`, the way
`huggingface_hub` resolves it; another cached revision is never read in its
place), and the free GPU
memory (`nvidia-smi`; on a unified-memory machine such as DGX Spark, where it
reports `[N/A]`, the total and available system memory from `/proc/meminfo`),
then adds: `--max-model-len` of 64K (PAIR's agent-ready default; a
model with a larger window is capped there, and a smaller one is shrunk to what
fits), a YaRN `--hf-overrides` when the model's own window is below 64K (for
example Qwen3-8B, which stops at 40,960), and a `--gpu-memory-utilization` just
large enough for the weights, that KV cache and overhead, instead of vLLM's
92% default. The weights are the files vLLM loads, not everything in the
snapshot: an unused PyTorch copy beside the safetensors is not counted. The
share is at least 15% of the GPU but never more than is free less 1 GiB, which
wins when the two disagree. A model with its own rope scaling is never overridden. Anything
the user saves in the launch arguments wins, and a tensor- or pipeline-parallel
launch is left untouched. A model that is not yet downloaded, such as the
default on a fresh install, first has its small files (config, tokenizer, chat
template; everything but the weights) fetched into the cache with the `hf` tool
beside `vllm`, at the revision vLLM loads, and its weight sizes read from
Hugging Face. It is then sized, and its tool-call parser chosen, exactly as a
cached model is, and vLLM finds those files already there. The fetch and every
other Hugging Face request PAIR makes for vLLM (pull, delete, size lookup) run
with the engine's own environment, read as `huggingface_hub` reads it, so its
launch settings apply to them too: `HF_HUB_OFFLINE`, else `TRANSFORMERS_OFFLINE`;
the token (`HF_TOKEN`, `HUGGING_FACE_HUB_TOKEN`, or the one `hf auth login`
stored under `HF_HOME` or `HF_TOKEN_PATH`), none of which is sent when
`HF_HUB_DISABLE_IMPLICIT_TOKEN` is set; a mirror in `HF_ENDPOINT`; and the cache folder (`HF_HUB_CACHE`,
else `HF_HOME/hub`), or `--download-dir` when the launch arguments set one, since vLLM
keeps a model's weights, config and tokenizer there. (vLLM's Transformers config lookup
also caches a few MB of config and tokenizer files in the default cache; Delete leaves
that shared cache alone, since other Hugging Face tools may use it, and no weights go
there.) The model list, Pull, Delete and
start-time sizing all use the cache vLLM itself will use. If Hugging Face cannot answer (offline,
gated without a token, unreachable), the model gets the free GPU memory less
1 GiB and no parser. Every start also adds `--max-num-seqs 32`: vLLM's default of 256
concurrent requests does not fit a hybrid model such as Qwen3.5-4B on a 16 GB
GPU, and a desktop does not need it. The added arguments appear in the engine-manager log
(`engine launch adjusted for the model and GPU`) and not in the saved settings,
so switching models re-sizes for the new one.
It also enables tool calling, which agent clients need on every request
(vLLM answers HTTP 400 to `tool_choice: "auto"` otherwise). The parser is found
from the model, not from a list of families: `hf` when the tokenizer ships
`response_template` metadata, otherwise the parser of the installed vLLM that
reads the model's own tool-call format. To find it, `toolparser_probe.py` runs
with vLLM's Python, renders the model's chat template with one example tool call,
and offers that text to every parser vLLM provides; among parsers that read it
equally, `hermes`, `qwen3_coder`, `llama3_json`, `mistral` and `pythonic` are
preferred in that order. The probe downloads nothing, runs no code from the model
repository, takes a few seconds, and is done once per model and vLLM while the
service runs. It runs under the start, so cancelling the start (Stop, a caller
giving up, shutdown) stops it. It reads the chat format vLLM will use: the
tokenizer `--tokenizer` and `--tokenizer-revision` name, if set, and a
`--chat-template` file or inline template. When that tokenizer is not cached,
or the template is a relative path vLLM would resolve against its own working
directory, no parser is chosen rather than one read from the wrong template. The chosen parser appears in the log
(`vLLM tool-call parser chosen from the model's chat template`).
When no parser reads the model's format (Nemotron Nano v2's `<TOOLCALL>`, or a
template with no tool support), nothing is added, and a request that asks for
tool calls gets vLLM's own error (HTTP 400). A parser named in the launch
arguments is always used as given.
The environment sets `VLLM_USE_FLASHINFER_SAMPLER=0`. vLLM's FlashInfer sampler
compiles GPU kernels on first start, and that build cannot handle the spaces in
PAIR's app-data folder (`Nvidia Corporation/Personal AI Router`), so the
built-in sampler is used instead. As a side effect no CUDA toolkit or C++
compiler is needed. The same limit means anything that forces a FlashInfer
kernel build (for example choosing a FlashInfer attention backend) fails until
the install can live on a path without spaces.
Readiness polls `/health` for up to eight minutes while weights load.

The model list is the Hugging Face cache (`~/.cache/huggingface/hub`, or the
one `HF_HUB_CACHE` or `HF_HOME` in vLLM's launch settings names), not what
vLLM happens to be serving: `list_models` is an `hf_cache` action listing the
complete, text-model-shaped downloads (see `MANIFEST.md`), and it works while
vLLM is stopped. Peers and the proxy, though, are only told about the model being
served (`advertised_models: loaded`, read from vLLM's `GET /v1/models`), because
the proxy routes a request to any node advertising its model and vLLM cannot
answer for the others. A stopped vLLM advertises nothing. `load_model` switches
the served model: it writes `--model <id>` into the saved launch arguments and
restarts vLLM on it, restoring the previous model if the new one does not start
(for example it does not fit in GPU memory).

`unload_model` is a `stop_engine` action: vLLM cannot drop its model while running, so ejecting stops the engine, which frees all of its GPU memory and keeps the saved model choice; a model the engine reports it is not serving is refused. Eject and `engine:stop` cancel a start that is still in flight, as shutdown does, instead of waiting out its readiness window: a Load cut short this way puts the previous model back in the launch arguments without starting it and replies `{model, restarted: false, stopped: true}` rather than an error, since the user asked for it, and a start stopped by the user is not reported as a failed start. Likewise `engine:start` (including `engine:install` with `start`), `engine:restart` and a peer's remote start that a Stop cut short reply with the engine's status, stopped, rather than an error. The broker relays an `engine:stop` without waiting for the lock a Start or Restart holds, so it reaches the engine-manager while that start is still waiting for readiness. `load_model` on a stopped engine saves the choice and starts it, restoring the previous choice if it does not start.

`delete_model` removes a model from the hub cache with the `hf cache rm` command of the `hf` tool beside `vllm` (an `hf_cache` action with `delete` and `tool`). Hugging Face's own tool decides which files go: it keeps any file in the cache's shared `blobs/` store that another model, dataset or Space still links to, and takes that store's locks, so a delete cannot remove a file another program is using or linking at the same moment. The cache is the user's standard Hugging Face cache, shared with every other Hugging Face tool on the machine. The model a running vLLM is serving is refused; which model that is is asked of vLLM itself (`loaded_models`), because an adopted vLLM may serve a model other than the one its saved arguments name, and the delete is refused when vLLM cannot say. Deleting the model an ejected or stopped vLLM is set to serve also takes `--model` out of its launch arguments, and a start with no `--model` is refused with an error rather than letting vLLM download a default model, until `load_model` selects one. The engine's operation lock is held from that check through the removal and any rollback, so a settings save or start made meanwhile waits and is not undone by a failed delete. `load_model`, `unload_model` and `delete_model` share that one launch-change path.

`pull_model` is an `hf_pull` action: it asks Hugging Face for the repo's file list
and sizes, picks the files vLLM reads (safetensors, config and tokenizer; not
GGUF, ONNX, pickle copies of weights that also exist as safetensors, or
`original/` checkpoints), and runs the `hf` tool
that ships beside `vllm` in the engine's virtualenv to download them into the hub
cache. Like the other engines' pulls (one Ollama tag, one llama.cpp
quantization), it downloads what the engine loads, not the whole repository: the
same set vLLM fetches when it downloads a model itself. A Mistral repository
ships each model in two formats of the same size; vLLM's default
`load_format auto` loads Mistral's own `consolidated*.safetensors` with
`params.json` and `tekken.json` whenever a repository has them, so only those
weights are downloaded and the Hugging Face-format `model-*.safetensors` are
skipped. A repository that ships its weights both as indexed shards and as one
file (`model.safetensors` beside `model-0000N-of-…` and
`model.safetensors.index.json`) has its index downloaded first, as vLLM's own
download does, and only the shards it lists are fetched; vLLM loads only those
(`filter_duplicate_safetensors_files`). Pull, the first-start size lookup and
the cache listing choose weights with one rule, so a first start and a cached
start size a model alike, and launch sizing counts only the indexed shards.
The cache listing judges such a model complete on those files. Other formats in the
repository are not fetched; a full `hf download` adds them to the same cache
without downloading the rest again. Progress is the
share of those bytes that has landed in the cache, a download that makes no
progress for the pull timeout is abandoned, and a cancelled one is stopped. It
needs no running engine. A gated repo needs its terms accepted and `HF_TOKEN`
(or a prior `hf auth login`), in the service's environment or the engine's launch
settings.

On Apple Silicon, vLLM-Metal stores the KV cache in the model's dtype and
refuses an FP8 KV cache, and its loader has no FP8 path for most model families.
The `darwin/arm64` platform therefore declares `unsupported_weights` (see
`MANIFEST.md`): a start or download of an FP8 checkpoint is refused with that
reason before anything is loaded or downloaded, a failed Load keeps the model
that was serving, and the Mac catalog leaves FP8 repositories out. Qwen3.5 and
Qwen3.6, which vLLM-Metal dequantizes itself, are allowed. Linux, DGX Spark
included, loads FP8 as before.

Install is detect-first. `{install_dir}/venv/bin/vllm` (the managed install) and
`~/.local/bin/vllm` are checked in that order, and the first one found is
launched, so an existing install is adopted and nothing is downloaded. Otherwise
the install downloads a sha256-pinned `uv` release, lets `uv` fetch a managed
Python 3.12, creates `{install_dir}/venv` and installs a pinned `vllm` wheel from
PyPI. No source build happens. The pinned wheel is a CUDA 13 build, so the install
first checks that `nvidia-smi` exists, that the driver supports CUDA 13 or newer
(it reads either the `CUDA UMD Version` or the older `CUDA Version` field), and
that at least 25 GB of disk is free, enough for the engine and its default model. It fails with a clear message otherwise,
before downloading anything. The
download is several GB, because `torch` and the CUDA libraries come with it.
Model weights download from Hugging Face on first start into `~/.cache/huggingface`
(`HF_HOME`), which no removal this service performs will delete.

## Communication

Bidirectional newline-delimited JSON-RPC 2.0 — the same conventions as
every other NVPAIR subprocess. Stdio by default; `--ipc <path>` dials a Unix
domain socket or Windows named pipe instead. The service is
**parent-agnostic**: it speaks to whatever owns its pipe (in practice
`nvpair-ui-broker`) and carries no front-end dependency, so the
same binary runs under any supervisor with zero code change. The `engine:*`
namespace is the surface the orchestrator/broker forwards from the UI.

## JSON-RPC surface

Requests (caller → service):

| Method | Params | Result |
|---|---|---|
| `engine:get-installed` | — | `{ engines: [EngineStatus] }` for every engine this host can run; an engine whose manifest has no block for the host's OS and CPU is left out |
| `engine:describe` | `{ engine }` | the engine's manifest |
| `engine:status` | `{ engine, port? }` | `EngineStatus`; an error for an engine this host cannot run |
| `engine:install` | `{ engine, start?, port?, bind? }` | `EngineStatus` (after install; also starts it if `start:true`) |
| `engine:uninstall` | `{ engine }` | `EngineStatus` (after removal) |
| `engine:uninstall-managed` | — | `{ engines: [{ engine, removed, error? }] }` — removes every engine this service installed, selected by its install marker, each through the `engine:uninstall` path. Backs "reset all data" in both clients; downloaded models are kept. Neither `removed` nor `error` means the engine was left alone as not ours |
| `engine:start` | `{ engine, port?, bind? }` | `EngineStatus` (after readiness) |
| `engine:stop` | `{ engine }` | `EngineStatus` |
| `engine:restart` | `{ engine }` | `EngineStatus` |
| `engine:set-port` | `{ engine, port }` | `EngineStatus` (after rebind) |
| `engine:action` | `{ engine, action, params }` | the engine's raw response. `action:"pull_model"` is streamed: it emits live `engine:pull-progress` notifications and returns the pull's terminal result (see below). An action whose manifest declares `restart_after` (LM Studio's `delete_model`) restarts a running engine before replying, so the response also means the engine is back and healthy |
| `engine:logs` | `{ engine }` | `{ lines: [LogLine] }` |
| `engine:errors` | — | `{ errors: [ServiceError] }` |
| `engine:catalog` | `{ engine, platform?, arch?, query?, modelSource? }` | `{ models: [{ id, name, author, url, size?, downloads, likes, updatedAt, tags, family?, parameterSize?, appleOnly? }], source, platform?, arch?, fetchedAt?, searchable?, query? }` — the models an engine can **download**, as opposed to `engine:models`, which reports what is already installed. Ollama, LM Studio, and vLLM have fixed sources; llama.cpp uses its selected source. Ollama's has no public API, so it is a locked list compiled into this binary from `catalog/ollama-models.json`, regenerated on demand by a developer (`npm run scrape:ollama-models` in `desktop/`) who reviews the diff; serving it needs no network. LM Studio's catalog *is* the `lmstudio-community` Hugging Face org, whose repo ids are exactly what `lms get` accepts, so it is fetched live and cached for six hours, with concurrent callers coalesced onto one request, a failed attempt — with or without a list to fall back on — followed by a backoff before the next. vLLM serves Hugging Face repos by their repo id: its browse list is the open, text-generating safetensors repos of `Qwen`, `nvidia`, `mistralai`, `microsoft` and `ibm-granite` (both `text-generation` and `image-text-to-text`), with parameter count and size, and, like llama.cpp, a `query` searches all of Hugging Face under the same rules. llama.cpp downloads any GGUF repo as `repo:quantization`; its Hugging Face browse list is the GGUF repos of `ggml-org`, `bartowski`, and `unsloth`, fetched and cached the same way, keeping only open, text-generating repos that ship a `Q4_K_M` file, each offered as `repo:Q4_K_M`. Both llama.cpp and vLLM are `searchable`. llama.cpp queries use its selected Hugging Face or ModelScope source; vLLM queries use Hugging Face. Each query is cached separately (the 20 most recently used are kept), and the reply echoes it. A query longer than 100 characters is refused rather than shortened, since a shortened one searches for something the operator did not type. Other sources ignore a query and return their whole list for the client to filter. `platform` and `arch` are the GOOS and GOARCH the models will be **installed on**, which is not always this host — a client driving a peer should name that peer. Omitting both means this host; naming a platform without an arch leaves the arch unknown rather than borrowing this host's. MLX quantizations only install on Apple Silicon, so they are marked `appleOnly` and kept only for a `darwin`/`arm64` target — an Intel Mac does not get them; the reply echoes the `platform` and `arch` it filtered for. `name` is pull-ready: it can be handed to `pull_model` verbatim. An engine with no curated source is an error, not an empty list. The Ollama reply is a single multi-megabyte frame, so every hop on its path must allow `jsonrpc.WorkerFrameBytes`. |
| `engine:models` | — | `{ models: [string], modelsByEngine: { <engine>: [string] }, loadedByEngine: { <engine>: [string] }, downloadedByEngine: { <engine>: [string] } }` — the flat de-duplicated union of every running engine's models, the per-engine breakdown keyed by engine name, and the per-engine set of models currently **loaded in memory** (all normalized from each engine's `list_models` / `loaded_models` action `result` spec). `modelsByEngine` carries a key for every running engine whose inventory was successfully queried, including an empty list = "running, no models available"; a missing key means not running / not queryable / invalid response. `loadedByEngine` uses the same known-empty distinction for residency and also omits engines with no loaded endpoint. For an engine with `advertised_models: "loaded"` (vLLM), `modelsByEngine` holds only the served model's name, which is what a request may be routed to, and `downloadedByEngine` holds its `list_models` inventory, read whenever it is installed, running or not, so a model manager keeps its rows after an Eject. Its `loadedByEngine` names the served model by its `--model` (the `root` of vLLM's `/v1/models`), not a `--served-model-name` alias, so it matches the inventory, and is `[]` while it is stopped. A missing `downloadedByEngine` key means the inventory was not read. The `/v1/models` HTTP surface returns the same shape. |
| `engine:remote-get-installed` | `{ node }` | `{ engines: [EngineStatus] }` fetched from the remote node over `ec` mTLS |
| `engine:remote-install` | `{ node, engine, start? }` | `{ opId, status: EngineStatus }` after the remote install (live progress via `engine:remote-progress`) |
| `engine:remote-pull-model` | `{ node, engine, model?, params? }` | `{ opId, result }` after the remote pull (live progress via `engine:remote-progress`) |
| `engine:remote-start` | `{ node, engine, port? }` | `EngineStatus` from the remote node (always the manifest's `runtime.bind`; no per-call bind override on the remote path) |
| `engine:remote-stop` | `{ node, engine }` | `EngineStatus` from the remote node |
| `shutdown` | — | `null` |
| `log/set-level` | `{ level }` | `{ level }` |

`GET /v1/models` has a five-second limit covering waiting for engine operations,
checking engine status, and fetching model lists. It returns 503 if the request
expires or a status check cannot complete, allowing other devices to keep their
previous model lists. Individual model-list fetch failures can still produce a
successful response containing results from other engines.

`EngineStatus` = `{ engine, display_name, installed, running, starting?, healthy, port }`.
`starting` is true while a start launches the engine and waits for it to be
ready, whoever began it (a Start, install-then-start, a settings restart, a
Load, or the restore at launch); `engine:state-changed` is pushed when it
begins and ends. A start refused before launch never reports it.

Notifications (service → caller): `engine:ready{version}`,
`engine:state-changed{EngineStatus}`,
`engine:models-changed{engine, models}` — pushed when one of an engine's
model lists changes: what it serves (`modelsByEngine`), what is loaded in
memory (`loadedByEngine`: explicit load/unload, JIT auto-load, or TTL/idle
eviction) or what it has downloaded (`downloadedByEngine`), so a pull or delete
is pushed too; `models` is the full `engine:models` shape so a consumer swaps
its whole snapshot,
`engine:install-progress{engine, stage, percent?, error?}` (`error` on the
terminal `failed` frame),
`engine:pull-progress{engine, op, stage, percent?, message}` (live progress for a
local model pull driven via `engine:action{action:"pull_model"}` — the local
counterpart of `engine:remote-progress`; frames are coalesced to changes in
stage/percent, the engine's terminal success surfaces as `stage:"success"`, and
a failed pull emits a terminal `stage:"error", percent:-1, message` frame so a
UI converges even if its synchronous call already timed out),
`engine:remote-progress{opId, node, engine, op, stage, percent?, message}`
(relayed live progress for a remote install/pull), and — for the error
pipeline — `errors:report` / `errors:clear` (consumed by `nvpair-errors`
via the broker; see below).

Streaming HTTP pulls have a 30-minute **inactivity** watchdog. Each increase in
an Ollama layer's or llama.cpp file's completed byte count refreshes that
watchdog, so an active download may run longer than 30 minutes; duplicate
progress frames and heartbeats do not extend a stalled pull. CLI-driven pulls
without structured byte progress retain the fixed 30-minute action timeout.

For llama.cpp, an accepted pull that ends before a matching `download_finished`
or `download_failed` event stops the active download. This includes caller
cancellation, remote caller disconnect, inactivity timeout, and premature SSE
termination. Cleanup checks `GET /models` and sends `POST /models/unload` only
when the exact model is still `downloading`; cached files are retained, and
models that have already completed are left alone.

The initial `POST /models` handshake has a separate 30-second total timeout and
continues through caller cancellation so its acceptance can still be read.
The pull then waits for cleanup, which has a separate five-second budget for
the inventory check and stop request together. A failed cleanup reports that
the download could not be confirmed stopped alongside the original pull error.
If the start acknowledgement is lost or unreadable, acceptance and cancellation
are reported as unconfirmed without unloading an unowned download. The monitored
model must match `params.model`; mismatches are rejected before subscribing.

`percent` on all three progress notifications is present only when the step is
measurable (download bytes, byte-progress pulls) or terminal (`100` on install
`done` / `already-installed`, `-1` on install `failed` and pull `error`). An
indeterminate step omits it — the install `verified` and `installing` stages
always do — so a UI renders the stage alone.

The `engine:remote-*` methods are the client half of remote engine
management: engine-manager resolves the target `node` in an `ec` peer
directory (fed by its own `discovery:subscribe{services:[ec]}` to the broker
relay), dials that peer's `ec` surface over pin-based cluster mTLS, and — for
`remote-install` / `remote-pull-model` — mints an `opId`, relays each streamed
progress frame up as `engine:remote-progress`, and settles the request with the
terminal result. Each NDJSON progress or terminal frame uses the shared
worker-path 8 MiB limit, accommodating terminal model-pull results containing
accumulated CLI output. The limit applies per frame so a long operation can
report progress throughout its lifetime. They fail if this node isn't clustered
or the target isn't a pinned cluster peer. See "Remote engine management" below.

`engine:install`, `engine:start`, `engine:stop`, `engine:restart`,
`engine:set-port`, and `engine:action` run in their own goroutine on the
service side, so the read loop never blocks and their responses arrive when
the op finishes.

`engine:set-port` is the **persistent** port setter (distinct from the
one-shot `engine:start {port}` override, which reverts on the next restart).
It validates `1-65535`, persists the choice as a manifest override (a
`{ engine, runtime: { port } }` delta written
to the per-user `engines/` dir that deep-merges onto the bundled manifest, so
`runtime.port` becomes the single source of truth and the port is restored on
the next start with no separate store), and applies it — bouncing the engine
onto the new port if it was running. Setting the port back to the bundled
default removes only the shared and host-platform port overrides; the file is
removed only when no other overrides remain. Arguments, environment, install
settings and other platform overrides are preserved. A host-platform port is
updated when necessary so it cannot shadow the saved value on restart.
Malformed override files fail the save instead of being replaced.
Because the chosen port lives in the
effective manifest, restore is automatic: a normal `engine:start` (no explicit
port) and the adopt-on-fixed-port path both come up on the retained port.
Moving a **running, adopted** engine is **refused** with an error (nothing is
persisted) — NVPAIR can't relocate a process it didn't start; see Adoption below.

## Lifecycle

Combined launch/server/proxy edits use the broker's
[engine settings protocol](../nvpair-ui-broker/ENGINE_SETTINGS.md). The worker
checks basic argument syntax and adapter-declared networking/CORS controls
using [`pair-arguments-v1`](LAUNCH_TEXT.md), persists host-platform `launch_args` and `launch_env`
with the port, and holds its operation lock through stop/rebind/start. The
paired engine-control surface relays settings to its local broker and streams
full authoritative snapshots to pinned peers.

```
NotInstalled --engine:install--> (HTTPS download + verify-if-pinned + user-mode run) --> Stopped
Stopped      --engine:start----> (adopt if already serving the port, else spawn) --> Running --health--> Running
Running      --engine:stop-----> (stop signal, bounded grace, force if needed) --> Stopped
```

Detect uses the manifest's `detect` paths. Install is one-shot and
user-mode — an HTTPS download, verified against the manifest's `sha256`
when one is pinned (an unpinned fetch runs with a loud warning). Start
waits for the readiness probe, then runs a periodic health probe; an
unexpected exit is reported. The bundled Ollama manifest allows up to ten
minutes for startup because GPU discovery can exceed the previous 30-second
allowance on supported Windows systems. The deadline remains finite: if Ollama
never serves its readiness endpoint, engine-manager stops the owned process and
reports the failed start. On Unix, stopping an owned process sends SIGTERM to
its process group, waits the manifest's `stop.grace_s` (five seconds by
default), then escalates to SIGKILL, which also takes any helper processes the
engine forked (vLLM's `EngineCore` holds the GPU) that outlived it; `signal:"kill"`
skips the grace. Failed startup cleanup uses the same policy. While the whole
service is shutting down the grace is capped at three seconds, whatever the
manifest says: the parent allows engine-manager only a few seconds to stop its
engines before it kills it, and engine-manager cannot escalate once killed, so a
longer grace would leave the engine orphaned (an engine still loading its model
does not answer a polite signal at all). On Windows, the windowless engines we
spawn cannot receive a graceful (non-`/F`) close, so stopping uses immediate
`taskkill /T /F`.

### Adoption — start may attach to an engine it didn't launch

Before spawning, `engine:start` **probes the chosen port's readiness
endpoint**, including any manifest-declared JSON identity. If a compatible
service answers there — the engine's own desktop
app (e.g. the Ollama tray app on `11434`), or an instance left running from a
previous session — the service **adopts** that instance: it marks the engine
`running` without launching its own, rather than spawning a duplicate that
would only collide on the port. Consequences worth knowing:

- **An adopted engine has no child process the service owns**, so `engine:stop`
  (and `engine:restart`'s stop phase, and shutdown's cleanup) resolves the PID
  bound to the engine's port and terminates it **only when that process is
  running the very binary NVPAIR manages for the engine** (for an engine
  installed as a Python package, such as vLLM, that is the interpreter running the
  engine's script, read from the process's command line on Linux) — reclaiming an orphan
  a prior run left on our own managed port (e.g. an `ollama serve` on `11435`
  whose handle was lost after a crash). This is precise to the port, so a
  genuine third-party listener on a *different* port (Ollama's own desktop app
  on `11434` while NVPAIR manages `11435`) is never touched. A listener whose
  image is **not** our managed binary is declined with an actionable error
  naming the offending PID and image path — the user / desktop app owns that
  process, and NVPAIR won't terminate it out from under them.
- **`engine:stop` may return an error while still saving OFF.** When stop
  declines a foreign listener it returns an actionable error, but the user's
  OFF choice is persisted anyway — UI layers should treat the saved desired
  state as authoritative (the engine will not restore on restart) and surface
  the error as guidance, not as proof the OFF intent was lost. The same applies
  to cluster `POST /v1/engines/stop`, which may answer HTTP 500 even though OFF
  was recorded.
- **`engine:set-port` on a running adopted engine is refused** (returns an
  error, persists nothing). Moving it would mean killing the old listener and
  spawning a new one; since NVPAIR can't kill what it didn't start, it errors
  rather than leaving a duplicate serving the new port while the original keeps
  serving the old one. Stop the engine in its own app first, then set the port.
- **`engine:install` short-circuits the same way.** `detect` honors existing
  system installs (Ollama's detect paths include `/Applications/Ollama.app`,
  `%LOCALAPPDATA%\Programs\Ollama`, etc.), so "installing" an engine that's
  already present downloads nothing and reports `installed: true` — and a
  following `start:true` then adopts the running instance.
- **Liveness reconciliation uses the same probe.** A fixed-port engine found
  already serving is reported `running: true` even though NVPAIR never started it.

`engine:status` and `engine:get-installed` check engines without holding the lock
used by start, stop, and port changes. Overlapping requests for the same engine
and port share one check while the engine's state remains unchanged. Each shared
check has a four-second limit. A result is discarded if the engine's state
changes before the check finishes. Reading, previewing, or validating launch
settings does not invalidate an ongoing status check; applying settings does.

During an engine operation, ordinary status requests return the current recorded
state. A status request that explicitly selects a port waits for the operation
to finish before checking that port. Model-list requests also wait, with their
five-second limit covering both the wait and the work that follows.

To get a **NVPAIR-owned, stoppable** instance, start it on a port nothing is
already serving — the probe misses, so the service spawns and owns the child
(tracked in `st.proc`), and a later `engine:stop` / `engine:restart` actually
terminates it. `engine:set-port` to a free port does exactly this; quitting the
external app first and re-starting on its usual port works too. Auto-assigned
ports (manifest `runtime.port: 0`) never adopt — there's no fixed address to
probe — so they always spawn an owned process.

## Remote engine management

When the parent passes `--control-port`, engine-manager serves the **`ec`
surface** — a cluster-scoped remote-control endpoint over **pin-based mutual
TLS** (`nvpair-shared/clustertrust`, the same identity + per-peer pins
`nvpair-cluster-manager` mints). It presents this node's cluster leaf, requires a
client cert, and rejects any caller that isn't a byte-for-byte pinned cluster
peer with a `403`. It differs from `em` (`--http-port`) only in what it permits:
`ec` performs privileged operations, while `em` is a read-only inventory. Both are
locked to pinned cluster peers on the LAN — a node's model list tells a caller
which models that machine holds, which is cluster data like any other. `em` also
keeps a plaintext personality on loopback, because this node's own scanner reads
its own inventory that way and must be able to while unclustered.

Membership is evaluated **live**, per handshake and per request, from
`--cluster-dir`. While this node is not a cluster member it presents no leaf, so
every handshake is refused and nothing privileged is reachable; the moment it
becomes a member the same listener serves pinned peers. The listener is therefore
bound for the life of the process rather than only when clustered at startup — a
node joins and leaves a cluster while engine-manager runs, and a surface chosen
at bind time would stay dark until the process was restarted.

Endpoints (all under `/v1`):

| Route | Shape | Purpose |
|---|---|---|
| `GET /v1/engines` | JSON | remote `engine:get-installed` |
| `POST /v1/engines/install` | NDJSON stream | remote install (+ optional start) with live progress |
| `POST /v1/models/pull` | NDJSON stream | remote model pull with live progress |
| `POST /v1/engines/start` | JSON | remote start → `EngineStatus` |
| `POST /v1/engines/stop` | JSON | remote stop → `EngineStatus` |

The streaming routes emit zero or more `{"type":"progress",...}` frames followed
by exactly one terminal `{"type":"result",...}` or `{"type":"error",...}` frame.
The initiating node's engine-manager consumes that stream, relays each progress
frame up as `engine:remote-progress`, and settles the originating
`engine:remote-*` request on the terminal frame — so a UI gets one synchronous
response plus a live progress feed keyed by `opId`.

The broker wires both directions: it passes `--control-port`/`--cluster-dir`,
registers `ec` with the discovery daemon whenever a cluster dir is configured, and
relays engine-manager's `discovery:subscribe{services:[ec]}` into the relay
directory so the peer directory stays current. It does **not** restart
engine-manager on `cluster:identity-changed` — the surface follows membership on
its own.

## CLI flags

| Flag | Default | Description |
|---|---|---|
| `--ipc <path>` | _(stdio)_ | IPC endpoint: Unix socket or Windows named pipe |
| `--http-port <port>` | `0` (off) | Serve the plain LAN model-list surface (`GET /v1/models`, the `em` service) on this port; the broker passes `:14322` |
| `--control-port <port>` | `0` (off) | Serve the cluster-scoped mTLS remote-control surface (the `ec` service) on this port; callers are admitted only while this node is a cluster member. The broker passes `:14323` |
| `--reserved-port <port>` | `0` (off) | Refuse local or remote engine starts and persisted port changes on a parent-owned proxy alias; the broker configures this from `OLLAMA_HOST` |
| `--cluster-dir <dir>` | _(none)_ | Cluster identity/pin directory; gates the `ec` surface on and supplies the leaf/pins used to serve it and to dial peers |
| `--loaded-poll-interval <sec>` | `5` | Seconds between loaded-model polls that drive `engine:models-changed`; `0` disables the watcher |
| `--log-level <level>` | _(env `NVPAIR_LOG_LEVEL` or `info`)_ | `debug` \| `info` \| `warn` \| `error` |
| `--uninstall-managed` | | Remove every engine PAIR installed, then exit; for the platform uninstallers, which have no broker to call `engine:uninstall-managed` through. Same selection and safety path as that method, loading only the manifests compiled into this binary. Always exits 0 so an uninstaller cannot stall |
| `--version` | | Print version and exit |

Logs go to **stderr** via the shared `nvpair-shared/applog` format; stdout is
reserved for JSON-RPC frames in stdio mode.

## Logging & errors

Each managed engine's stdout/stderr is captured into a bounded ring
(queryable via `engine:logs`). Operational failures (install/start/health)
are recorded and surfaced as `errors:report` / `errors:clear`
notifications using the `nvpair-shared/errors` wire shape. The broker
(`nvpair-ui-broker`) forwards them to the `nvpair-errors` registry.
Ids follow `engine-manager:<class>:<engine>`.

## Security posture

Runs **user mode only** — no admin/sudo at runtime (privilege escalation
is reserved for NVPAIR's own install time). It has two optional LAN listeners, and
**both are pin-based cluster mutual TLS with a live membership check** — every
caller is authorized against a per-peer pin, a non-member is refused with a `403`,
and while this node belongs to no cluster it presents no leaf so no handshake
completes:

- **`--http-port`** — the model-list surface (`em`, `GET /v1/models`; the broker
  passes `:14322`). A node's model inventory is cluster data, so LAN callers must
  be pinned peers. This port additionally serves **plaintext on loopback only**,
  which is how this node's own scanner enriches its own card — including when the
  node belongs to no cluster, so a standalone machine still shows its own models.
- **`--control-port`** — the remote-control surface (`ec`, the `engine:remote-*`
  targets; the broker passes `:14323`). mTLS only, no plaintext personality, since
  every route performs a privileged operation.

Unlike the rest of NVPAIR, engine-manager therefore **does terminate inter-node
mTLS itself** (and dials peers' `ec` surfaces with the same pinned identity),
matching how `nvpair-errors` and `nvpair-workload-manager` handle their own
cluster traffic. Managed engines bind **loopback by default**, but a manifest's
`runtime.bind` may open an inference engine to the LAN — Ollama ships
`0.0.0.0` to serve the cluster — overridable per call via
`engine:start {bind}`; readiness/health probes always target loopback.
Downloads are **HTTPS-only** (plain `http` only from loopback) and verified
against the manifest's `sha256` when one is pinned; an unpinned fetch runs
with a loud warning.

## Cross-platform

One binary compiles and runs on Windows, Linux, and macOS × amd64/arm64.
Per-OS variance lives in the manifest first; OS primitives (process
termination, console hiding) are the only build-tagged Go
(`proc_windows.go` / `proc_unix.go`).

## Shutdown

Shuts down on stdin EOF (parent closed the pipe), `SIGINT`/`SIGTERM`, or a
`shutdown` JSON-RPC request — stopping any running engines first so none
are orphaned. A signal shuts down at once even while stdin is idle and an engine
is still starting; the start is cancelled and its process group force-killed
within the three-second shutdown grace (see Process-mode stops above).

That engine sweep (`StopAll`) runs **exactly once per process**, and every
caller returns only once it has finished. An ordinary quit asks for it twice:
the broker sends `engine:prepare-shutdown` from its own teardown, and then
closing stdin reaches the EOF path. Each is the right trigger for a different
way of being shut down, so both stay.

Later callers **wait** for the sweep in flight rather than returning early:
returning early would report engines stopped before they were, and the broker
would close stdin and exit while they were still running. They do not repeat
the sweep, which would re-pay the readiness probe for every adopted engine whose
stop is declined. They do retry any engine whose stop failed through a process
handle or a stop command, such as an `lms server stop` that errored, because
another attempt can succeed where a declined stop cannot.

`engine:status` and `engine:get-installed` never wait for a lifecycle operation
(a start, stop, install or model switch can hold an engine for minutes): while
one runs they report what is already known and the operation reports its own
result.
