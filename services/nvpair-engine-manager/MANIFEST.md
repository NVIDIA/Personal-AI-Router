<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Engine Manifest Reference

An engine manifest is a single JSON file that tells `nvpair-engine-manager`
how to detect, install, run, probe, and act on one inference engine. The
runner is engine-agnostic: **adding an engine is a manifest, not code.**

## Where manifests live

- **Bundled** defaults are compiled into the binary from
  `nvpair-engine-manager/manifests/*.json` (e.g. `ollama.json`).
- **User / third-party** manifests are read at runtime from
  the per-user data dir's `engines/*.json`
  (`%LocalAppData%\Nvidia Corporation\Personal AI Router\engines\` on Windows,
  `~/.config/Nvidia Corporation/Personal AI Router/engines/` on Linux,
  `~/Library/Application Support/Nvidia Corporation/Personal AI Router/engines/` on macOS).

A user manifest **overrides** a bundled one with the same `engine` name,
so a vendor or operator can ship or tweak an engine without rebuilding. The
override is a **deep-merge onto the bundled manifest** (override keys win, the
same key-by-key rules as the shared-defaults merge below), not a wholesale
replace — so a partial file pins only the fields it sets and keeps inheriting
everything else (install URLs, actions, fixes) as the bundled manifest is
upgraded. A user file for an engine with **no** bundled base of the same name
is loaded standalone (it must then be a complete manifest). This is how
`engine:set-port` persists a chosen port: it writes the minimal delta
`{ "engine": "<name>", "runtime": { "port": <n> } }`, which merges onto the
bundled manifest so only `runtime.port` is pinned. (A standalone
`LoadRegistry(dirs…)` load still replaces wholesale; the deep-merge applies to
the per-user `engines/` override layer that the service overlays at startup.)

Port changes merge only the port into an existing override. Returning to the
bundled default removes shared and host-platform port overrides while keeping
arguments, environment and unrelated settings. The file is removed only when
the engine identifier is all that remains. A host-platform port override is
updated too if needed, so it cannot shadow the saved shared port at startup.
An invalid existing override is left intact and the save fails.

## Top-level fields

For editable launch settings, `runtime.editable_launch` declares optional
`fixed_args`, command-mode `start_index` (default zero), and `controls` that
bind an engine's syntax to PAIR's policy fields. The bundled manifests reference
[manifest.schema.json](manifest.schema.json) for editor validation. Engine-manager
also validates bindings at load, including cross-control constraints that JSON
Schema does not express. Unknown properties inside `editable_launch` are errors.

```json
"editable_launch": {
  "fixed_args": ["server", "start"],
  "controls": [
    { "flags": ["--port", "-p"], "value": "{server.port}" },
    { "flags": ["--bind"], "env": ["LMS_SERVER_HOST"], "value": "{server.host}" },
    { "flags": ["--cors"], "value": "{cors.enabled}", "implicit": "true" }
  ]
}
```

| Control property | Meaning |
| --- | --- |
| `flags` | Supported flag spellings. The first is the canonical output name; the rest are aliases. |
| `env` | Supported environment names. All share the same binding and validation. |
| `value` | Format binding policy fields, with literal text between them. For example, `{server.host}:{server.port}` or `tcp://{server.host}:{server.port}`. |
| `implicit` | Value supplied by flag presence, such as `true` for `--cors` or `false` for `--no-cors`. Omit for flags that consume a value. Environment sources always consume an explicit value. |

PAIR has a small, fixed policy surface:

| Field | Policy |
| --- | --- |
| `server.port` | Integer 1–65535, synchronized with the server-port field. |
| `server.host` | Managed loopback host, keeping direct engine access behind the proxy. |
| `cors.origins` | Comma-separated origin list, normalized and validated; changes are local-only. |
| `cors.enabled` | Explicit boolean, normalized to `true` or `false`; changes are local-only. |

The **proxy port belongs to the broker**, which creates that listener and validates
port collisions. It is not an engine launch argument or an engine-manifest binding.
The presence of a CORS field identifies a browser-policy control. Engines without
one still work; proxies follow the engine's actual HTTP CORS response.

The tokenizer reads flag/environment syntax, the binding parser extracts fields,
and the field validators enforce policy. All input sources use the same conflict
checks. Launch construction renders those same bindings, with no engine names,
option-kind switches, or separate host:port parser. For example, Ollama uses
`{"env":["OLLAMA_HOST"],"value":"{server.host}:{server.port}"}`.
Bindings can reorder fields or add literal prefixes/brackets without Go changes.
Templates are trusted manifest syntax; user values are never expanded as templates.

Controls must collectively bind every managed server field. Fields within a value
must be unique, separated by literal text, and share ownership: managed server
fields cannot be combined with editable CORS fields. Unknown fields, malformed
formats, duplicate source names and invalid implicit values fail loading.
Implicit values cannot replace managed server fields. Every captured value still
passes the field validator; a format cannot weaken port ranges or loopback policy.

Managed values set every declared environment alias and, when assembling edited
arguments, emit the canonical flag. This prevents inherited network aliases from
overriding the managed values. Short value options accept separate, attached and
equals forms. Ambiguous bundles containing a declared short control are rejected.
A new engine using this grammar needs bindings and tests; a new CLI grammar needs
a shared grammar extension, not an engine-specific branch.

There is no catalog of unrelated vendor options or editable environment names.
Those values pass through literally. Explicit environment assignments override
manifest defaults; omitted defaults remain in effect. The final effective launch
is validated again before execution, including settings saved by earlier versions.

The editor saves literal extra arguments in
host-platform `runtime.launch_args` and explicit environment assignments in
`runtime.launch_env`, after trusted template expansion. Empty arrays clear the
saved arguments or environment assignments. See [LAUNCH_TEXT.md](LAUNCH_TEXT.md) for validation
and [the broker protocol](../nvpair-ui-broker/ENGINE_SETTINGS.md) for application
and recovery. Editing `args`/`start` directly remains trusted manifest authoring.

| Field | Type | Required | Notes |
|---|---|---|---|
| `engine` | string | yes | Unique id (the key used in `engine:*` calls). |
| `display_name` | string | yes | Human-friendly name. |
| `manifest_version` | int | yes | Must be `1`. A higher value is rejected (asks for behavior this binary lacks). Unknown optional fields within a supported version are ignored, so the schema can grow additively. |
| `platforms` | object | yes | Map of `"<goos>/<goarch>"` → platform block (e.g. `"windows/amd64"`, `"darwin/arm64"`, `"linux/amd64"`). At least one entry. The runner selects the block matching the host. |
| `actions` | object | no | Map of action name → action (see below). |
| `detect` / `install` / `uninstall` / `runtime` | — | no | Optional **shared defaults** inherited by every platform (see below). |

**Shared defaults & per-platform overrides.** The platform-level fields `detect`, `install`, `uninstall`, and `runtime` may also be given once at the top level as shared defaults; each `platforms` entry is then merged onto them. Nested objects (e.g. `runtime`, `runtime.env`) merge key-by-key with the platform value winning, while arrays and scalars are replaced wholesale. So a runtime that's identical across platforms except `cli` is declared once at the top level, and each platform sets only `"runtime": { "cli": "…" }`. Omitting a key inherits the default; setting it (even to a zero value like `"port": 0`) overrides it. A manifest that fully specifies each platform with no top-level defaults behaves exactly as before.

## Platform block

| Field | Type | Required | Notes |
|---|---|---|---|
| `detect` | string[] | no | Paths that, if any exists, mean the engine is already installed. Supports OS env refs (`%VAR%`, `$VAR`), a leading `~`, and the `{install_dir}` placeholder. |
| `install` | object | no | How to obtain the engine (see Install). Omit for engines that are only ever detected/launched. |
| `uninstall` | object | no | `{ "run": [...] }` — argv to remove a user-mode install (the engine's own uninstaller, or `rm -rf {install_dir}`). Backs `engine:uninstall`; placeholders resolved, OS env refs expanded. |
| `runtime` | object | yes | How to launch + probe the engine (see Runtime). |

### Install

| Field | Type | Required | Notes |
|---|---|---|---|
| `fetch.url` | string | when `fetch` present | Download URL — **HTTPS** (plain `http` only from loopback). |
| `fetch.sha256` | string | no | Hex SHA-256. When set, the download is verified against it **before** `run` executes; when omitted, the fetch is HTTPS-only and runs with a loud "unpinned" warning (the same weaker guarantee as `script`). Pin it for any real release. |
| `run` | string[] | no | Argv to execute after download (e.g. run the installer, extract the archive). Placeholders resolved; OS env refs expanded. Requires a `fetch` (the artifact it unpacks). |
| `script` | string[] | no | **Escape hatch** for vendors that only ship a script installer. Runs **without** checksum verification (logged as unpinned) and replaces `fetch`+`run`. Prefer `fetch`+`run` whenever the vendor publishes a script or artifact: download it first, then execute the local file. **Make failures loud:** a piped bootstrap such as `curl … \| bash` can mask a failed fetch, while a separate fetch prevents the run and reports the error. |
| `mode` | string | no | `"user"` (default) or `"admin"`. The runner **refuses** `"admin"` (engine-manager is user-mode only); it is a deliberate, flagged exception, not a default. |

### Runtime

| Field | Type | Required | Notes |
|---|---|---|---|
| `mode` | string | no | `"process"` (default) — the engine is a foreground process this service spawns and **owns** (liveness = process alive) — or `"command"` — a daemon driven by start/stop commands (liveness = the probe). |
| `bin` | string | process mode | Path to the engine binary (required in `process` mode). Placeholders + OS env refs resolved. |
| `args` | string[] | no | Arguments (process mode). |
| `env` | object | no | Extra environment (merged over the inherited env). Use `{host}`/`{port}` for the listen address, e.g. `"OLLAMA_HOST": "{host}:{port}"`. |
| `port` | int | no | Fixed port, or `0` to auto-assign a free loopback port. |
| `bind` | string | no | Listen address, substituted as `{host}`. Empty ⇒ `127.0.0.1` (the safe default); an inference engine that serves the cluster sets `"0.0.0.0"` (Ollama does), overridable per call via `engine:start {bind}`. Probes always target loopback. |
| `start` | string[][] | command mode | Ordered bring-up commands (each an argv) for `command` mode, e.g. `[["{cli}","server","start","--port","{port}"]]`. |
| `cli` | string | no | The engine's control-CLI path for this platform, referenced as `{cli}` by start/stop/actions so the manifest's **global** actions resolve to the correct per-OS binary. |
| `ready` | probe | no | Readiness probe; `start` waits for it before reporting Running. |
| `stop` | object | no | Process mode: `signal` (`"term"` default, or `"kill"`) + `grace_s` (seconds before force-kill). Command mode: `cmd` (argv) to bring the engine down. |
| `health` | probe | no | Periodic liveness probe while Running; `interval_s` between checks. |

A **probe** is `{ "http": "<url>", "status": <int>, "timeout_s": <int>, "interval_s": <int> }`
or `{ "tcp": "<host:port>", ... }`. `status` defaults to `200`. Prefer
loopback URLs/addresses.

### Actions

Each action is a config-declared operation exposed over `engine:action`.
Exactly one of `http`, `cmd`, or `remove_path`:

- **`http`** — call the engine's loopback control API. The caller's
  `params` are sent as the JSON request body; `body_schema` is
  informational. Requires the engine to be running.
- **`cmd`** — run a CLI command (e.g. `lms get`). The caller's `params`
  become placeholders (e.g. `{model}`); stdout is returned (parsed as
  JSON when it is valid JSON). Does **not** require the engine to be
  running.
- **`remove_path`** — delete a filesystem path declared in the manifest.
  `remove_path.root` and `remove_path.path` are templated (including
  action `params` as placeholders); the resolved target must stay under
  the declared root. Symlink escapes and missing targets are rejected.
  Does **not** require the engine to be running.

An optional **`result`** declares how `engine:models` extracts a normalized
name list from the action's JSON response: `array` is the top-level array
field to iterate and `field` is the string field to pull from each element.
It lets one engine-agnostic path turn each engine's `list_models` shape into a
flat `[]string` with no per-engine code — Ollama's `/api/tags`
(`{"models":[{"name":...}]}`) uses `{"array":"models","field":"name"}`, while
LM Studio's native `/api/v1/models` (`{"models":[{"key":...}]}`) uses
`{"array":"models","field":"key"}`. A required array that is present but empty
is an authoritative empty inventory; a missing, null, or wrong-typed array is
unknown, as is a non-empty unfiltered inventory with no usable names.
`engine:models` runs every running engine's `list_models` and unions the
extracted names; it also backs the node's `em` HTTP endpoint
(`GET /v1/models`) that peers fetch during discovery enrichment.

`result` also takes an optional **`match`** row filter that keeps only array
elements that pass the filter. Exactly one of `in` or `nonempty` is required:

- `{"field":...,"in":[...]}` — keep elements whose `field` (a JSON string)
  equals one of `in`.
- `{"field":...,"nonempty":true}` — keep elements whose `field` is a JSON
  array with length > 0.

This lets a `loaded_models` action reuse the same extractor as `list_models`
even when an engine's list endpoint returns *all* models tagged with residency
rather than a presence-only list. The bundled `loaded_models` actions report
the models resident in memory (surfaced as `engine:models`'s `loadedByEngine`
and pushed via `engine:models-changed`): Ollama's `GET /api/ps` is presence-only
so it needs no `match` (`{"array":"models","field":"name"}`), while LM Studio's
native `GET /api/v1/models` uses
`{"array":"models","field":"key","match":{"field":"loaded_instances","nonempty":true}}`.
A row whose `match.field` is missing or the wrong JSON type fails the match (it's
excluded). The filter shape lives in the manifest so it can be updated without a
code change if an engine's API drifts. An engine that declares no `loaded_models`
action simply contributes no `loadedByEngine` key. The bundled LM Studio list
and loaded actions require LM Studio 0.4.0 or newer, where the native
`/api/v1/models` endpoint is available. All of this stays additive:
`manifest_version` is still `1`.

An optional **`model_resolution`** expands or resolves the `{model}`
placeholder before the action runs:

- **`"lms-get"`** (cmd actions only) — try the value as given (so an
  explicit `huggingface.co`/`lmstudio.ai` URL is honored first), then the
  LM Studio Hub artifact id (`owner/name`), then the Hugging Face repo
  URL. This works around `lms get` routing a bare `owner/name` to the Hub
  registry, which 404s the Hugging-Face-hosted community models LM
  Studio's own Discover tab downloads. A **transient** download failure
  (a stalled or timed-out transfer) is *not* a resolution failure:
  rather than advancing to the next source, `lms-get` retries the same
  command in place (a bounded number of times) — `lms get` resumes a
  partial download on re-run — so a momentary network hiccup recovers
  instead of failing the pull. A hard error still surfaces immediately.
- **`"lms-disk-path"`** (remove_path actions only) — map a logical LM
  Studio model id (the OpenAI-compatible `/v1/models` id, an
  `owner/repo` pull key, or an on-disk path) to concrete files under the
  declared `remove_path.root` via `lms ls --json`. When no indexed file
  matches, the runner falls back to `{root}/{model}` if that path exists.

An optional **`restart_after`** restarts the engine once the action succeeds.
Declare it on an action that changes state the engine only reads at startup:
LM Studio's `delete_model` removes the files, but its server answers
`/v1/models` from an index built at startup and exposes no rescan operation
(no CLI command, REST route, or SDK call), so clients keep being offered a
deleted model until it restarts. It is opt-in per action and deliberately rare —
Ollama reflects a deletion immediately, so its `delete_model` omits the flag and
its engine is never bounced.

The rules the runner enforces:

- **A stopped engine stays stopped.** There is no serving state to reconcile,
  and starting one behind the user's back would be a surprise.
- **The user's ON/OFF intent is never rewritten.** The bounce goes through
  `doStop`/`doStart`, not `Restart`, precisely because `Restart` also persists
  desired-enabled — an action must not turn "delete this model" into "and leave
  this engine switched on".
- **The running check and the bounce share `opMu`.** A concurrent `Stop` either
  wins the lock first (the action then sees a stopped engine and leaves it down)
  or waits and stops the engine we just brought back. It can never be silently
  undone.
- **A failed restart fails the action**, because the action's effect is only
  observable once the engine is back. The error names the restart
  (`… failed to restart: …`) so a caller can distinguish "nothing happened" from
  "the destructive half ran and the engine did not return" — the destructive
  half is *not* rolled back, so re-issuing the action is the wrong response.
- **The platform must declare `runtime.ready`.** `Validate` rejects
  `restart_after` without one: readiness is what makes "the effect is visible by
  the time this replies" true rather than a race with the engine's own startup.
- **A remote caller waits on the readiness budget.** Driving such an action on a
  peer means the peer withholds its response headers until the restart is ready,
  so `remoteclient.go` routes that path through the readiness-sized client
  (`waitsForEngineReadiness`) rather than the ordinary 30s one. Cutting the call
  off would cancel the peer's in-flight handler and let `bringUpCommand` tear the
  engine back down — for a delete, files gone and engine left stopped.

```json
"list_models": { "http": { "method": "GET", "path": "/api/v1/models" }, "result": { "array": "models", "field": "key" } },
"pull_model":  { "cmd": ["{cli}", "get", "{model}", "--yes"], "model_resolution": "lms-get" },
"delete_model": {
  "model_resolution": "lms-disk-path",
  "remove_path": { "root": "{models_dir}", "path": "{models_dir}/{model}" },
  "restart_after": true
}
```

Actions target the engine's control plane — **not** inference endpoints
(those stay with the proxy).

## Placeholders

Resolved by the runner at execution time; any other `{token}` fails
validation at load:

| Placeholder | Meaning | Available in |
|---|---|---|
| `{host}` | Listen address from `runtime.bind` (or a per-call `bind`); `127.0.0.1` when unset | runtime args/env |
| `{port}` | The chosen runtime port | runtime args/env, start, probes, action paths |
| `{bin}` | Resolved binary path (process mode) | runtime args/env |
| `{cli}` | The platform's `runtime.cli` path | runtime start/stop, action `cmd` |
| `{download}` | Path of the verified download | `install.run` |
| `{install_dir}` | Per-engine user-scoped install dir | `detect`, `install`, runtime |

A `cmd` action additionally templates the action's own `params` as
placeholders (e.g. `{model}`), resolved at call time. HTTP actions send
`params` as the JSON request **body** — they are not substituted into
`http.path`, which templates only `{port}`.

## Validation

A manifest is rejected at load (with a specific message) when: a required
field is missing, `manifest_version` is unsupported, a platform key isn't
`"<goos>/<goarch>"`, `runtime.bin` is empty in process mode (or
`runtime.start` is empty in command mode), `install.run` has no `fetch`,
`install.script` is combined with `fetch`/`run`, `install.mode` or
`runtime.mode` is invalid, an action sets none or more than one of
`http`/`cmd`/`remove_path`, a `remove_path` action omits `root` or
`path`, a `result` is set without both `array` and `field` (or a
`result.match` without both `match.field` and a non-empty `match.in`), or
a non-action templated string uses an unknown placeholder.

---

## Worked example 1 — Ollama (Linux, user-scoped, no sudo)

A conservative Ollama manifest — user-scoped install (never the official
`curl | sh`, which would `sudo`-install to `/usr` + systemd), checksum
**pinned**, server kept on loopback. (The bundled `ollama.json` is shaped
the same way but ships **unpinned** and defaults `runtime.bind` to
`0.0.0.0`; see the note after the example.)

```json
{
  "engine": "ollama",
  "display_name": "Ollama",
  "manifest_version": 1,
  "platforms": {
    "linux/amd64": {
      "detect": ["{install_dir}/bin/ollama"],
      "install": {
        "fetch": { "url": "https://ollama.com/download/ollama-linux-amd64.tgz", "sha256": "‹release sha256›" },
        "run": ["tar", "-xzf", "{download}", "-C", "{install_dir}"],
        "mode": "user"
      },
      "runtime": {
        "bin": "{install_dir}/bin/ollama",
        "args": ["serve"],
        "env": { "OLLAMA_HOST": "{host}:{port}", "LD_LIBRARY_PATH": "{install_dir}/lib/ollama" },
        "port": 11434,
        "ready":  { "http": "http://127.0.0.1:{port}/", "status": 200, "timeout_s": 30 },
        "stop":   { "signal": "term", "grace_s": 5 },
        "health": { "http": "http://127.0.0.1:{port}/", "status": 200, "interval_s": 5 }
      }
    }
  },
  "actions": {
    "list_models": { "http": { "method": "GET",  "path": "/api/tags" } },
    "pull_model":  { "http": { "method": "POST", "path": "/api/pull", "body_schema": { "name": "string" } } }
  }
}
```

> This example **pins** `sha256` (best practice). The bundled `ollama.json`
> ships **unpinned** — `fetch` has no `sha256`, so install is HTTPS-only and
> logs a loud "unpinned" warning. Pin it to the SHA-256 of a *versioned*
> release URL for reproducible, integrity-verified installs ("latest" URLs
> change checksum per release).

## Worked example 2 — LM Studio (a daemon + control-CLI engine)

LM Studio is shaped very differently from Ollama: its server runs in a
background daemon driven by the `lms` CLI, model downloads are a CLI
command (no HTTP endpoint), and its headless installer is a vendor
script. It is still added with **no code** — using `mode: "command"`, a
`cmd` action, `{cli}`, and a downloaded installer script:

```json
{
  "engine": "lmstudio",
  "display_name": "LM Studio",
  "manifest_version": 1,
  "platforms": {
    "linux/amd64": {
      "detect": ["~/.lmstudio/bin/lms"],
      "install": {
        "fetch": { "url": "https://lmstudio.ai/install.sh" },
        "run": ["bash", "{download}"],
        "mode": "user"
      },
      "uninstall": { "run": ["rm", "-rf", "~/.lmstudio"] },
      "runtime": {
        "mode": "command",
        "cli": "~/.lmstudio/bin/lms",
        "port": 1235,
        "start": [["{cli}", "server", "start", "--port", "{port}"]],
        "stop":  { "cmd": ["{cli}", "server", "stop"] },
        "ready":  { "http": "http://127.0.0.1:{port}/v1/models", "status": 200, "timeout_s": 60 },
        "health": { "http": "http://127.0.0.1:{port}/v1/models", "status": 200, "interval_s": 10 }
      }
    }
  },
  "actions": {
    "list_models": { "http": { "method": "GET", "path": "/api/v1/models" }, "result": { "array": "models", "field": "key" } },
    "pull_model":  { "cmd": ["{cli}", "get", "{model}", "--yes"], "model_resolution": "lms-get" },
    "load_model":  { "cmd": ["{cli}", "load", "{model}"] }
  }
}
```

What each piece does here:
- **`mode: "command"`** — no owned process; liveness is the `/v1/models` probe, and `stop.cmd` (`lms server stop`) brings the daemon down.
- **`cmd` action** — `pull_model` shells out to `lms get {model}` (no HTTP equivalent exists), with `{model}` filled from the call's params.
- **`model_resolution: "lms-get"`** — `pull_model` tries the model as given, then the Hub artifact id, then the Hugging Face repo URL, so a bare `owner/name` for a Hugging-Face-hosted model (most of LM Studio's catalog) downloads instead of 404ing against the Hub registry.
- **`{cli}`** — the manifest-global `pull_model` action resolves to the right per-OS binary (`lms.exe` on Windows, `lms` elsewhere).
- **Installer script** — LM Studio's headless installer is vendor-provided and unpinned. Engine-manager downloads it over HTTPS and executes the saved file as a separate argument, so fetch failures stay visible and paths containing spaces cannot be split by a shell. Windows executes the saved `.ps1` with PowerShell `-File`; it never pipes remote content through `Invoke-Expression`. The vendor script checks the downloaded archive against its published SHA-512 when one is available.

---

## Engine config reference

A condensed map of each engine's manifest-relevant surface, to guide
authoring. The recurring, high-value controls (and the right targets for a
future "declared tunables" layer) are: bind host/port, model dir,
concurrency/parallelism, max-loaded models, context length, keep-alive/TTL,
GPU selection, and auth.

| Engine | Fit | Headless launch | Config surface | Control |
|---|---|---|---|---|
| **Ollama** | strong (env-first) | `ollama serve` (foreground) | env: `OLLAMA_HOST`, `OLLAMA_MODELS`, `OLLAMA_KEEP_ALIVE`, `OLLAMA_NUM_PARALLEL`, `OLLAMA_MAX_LOADED_MODELS`, `OLLAMA_MAX_QUEUE`, `OLLAMA_CONTEXT_LENGTH`, `OLLAMA_FLASH_ATTENTION` | HTTP `/api/tags`, `/api/pull`; CLI `ollama pull/ls/ps/stop` |
| **llama.cpp** | strong (env+flags) — **reference design** | `llama-server --host 127.0.0.1 --port {port}` (foreground) | flags + `LLAMA_ARG_*` (host/port, ctx-size, n-parallel, cont-batching, flash-attn, device, n-gpu-layers, tensor-split, main-gpu, api-key, models-dir/max) | HTTP `/v1/models`, `/models/load`, `/models/unload`, `/health`, `/slots` |
| **LM Studio** | command / daemon | `lms daemon up` → `lms server start --port {port}` | small env (`LMS_SERVER_HOST`, `LM_API_TOKEN`); most config is flags/API/settings (`lms load --context-length/--gpu/--ttl`) | HTTP `/api/v1/models[/download\|load\|unload]`; CLI `lms ls/get/load/unload/ps` |
| **vLLM** | flags-first — **Linux/WSL only** | `vllm serve <model> --host 127.0.0.1 --port {port}` | flags (host/port/api-key); `HF_HOME` for cache. `VLLM_PORT`/`VLLM_HOST_IP` are **not** the API bind | OpenAI `/v1/models`; one model per process (unload = restart) |
| **Jan** | hybrid (on llama.cpp router) | `jan serve <model> --port {port}` (CLI) | forwards `LLAMA_ARG_*`; perf settings are router-preset-driven | `jan serve` auto-downloads HF repos |
| **GPT4All** | weak (GUI app) | none (Local API Server toggled in the GUI, `:4891`) | desktop settings / Python SDK — not env | OpenAI `/v1` once enabled in-app |

Caveats worth encoding when authoring these:
- **Ollama `OLLAMA_MODELS`**: leave it at the default (`~/.ollama`) so models survive an `uninstall` that removes `{install_dir}` — **never** point it inside `{install_dir}`.
- **llama.cpp** ships prebuilt archives with **published SHA-256s**, so it's an ideal checksum-pinned `fetch`+`run` target (no placeholder SHA needed).
- **LM Studio** model dir is settings-controlled (no documented relocation env); surface its config as flags/API, not env.
- **vLLM**: native Windows is unsupported (WSL only); the bind is a flag, not env.
- **GPT4All**: no first-class headless CLI today — out of scope for a config-only manager unless targeting its Python SDK.
