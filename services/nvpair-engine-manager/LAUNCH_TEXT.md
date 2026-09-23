<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# PAIR engine arguments

The Settings editor uses this notation for local and paired-device engine
configuration. Engine-manager assembles the manifest-owned command; the broker
coordinates its saved revision, proxy port and lifecycle as one operation.

## `pair-arguments-v1` notation

The same notation is used on every target OS. It represents tokens passed
directly to the engine, not a command to paste into a system shell.

- ASCII spaces, tabs and line breaks separate unquoted tokens.
- Single quotes enclose literal text. Double quotes enclose literal text with
  only two escapes: `\"` for a quote and `\\` for a backslash. Other backslashes
  remain literal, including those in a path such as `C:\new\tools`.
- Quoted and unquoted segments may form one token: `--name="two words"`.
  Empty quoted strings represent empty arguments. Quoted line breaks are literal.
- `$HOME`, `%USERPROFILE%`, `{port}` and `~` are not expanded by the parser.
  There are no shell comments, pipelines, redirects or command substitution.
  Unquoted `|`, `&`, `;`, `<`, `>`, backticks and `$(` are rejected. Such values
  can be quoted when they are literal engine arguments.
- Input must be valid UTF-8, at most 16 KiB, with at most 256 tokens.
  Control characters other than tab, CR and LF are rejected, including NUL.
  Errors describe the syntax problem without echoing the submitted text.
- Formatting sorts explicitly configured environment assignments by name, then
  writes the arguments. The executable and startup subcommand are never editable.
  Values are quoted deterministically; inherited environment variables are never
  included. Empty text selects managed defaults. Normalized text has the same
  size bound as input.

For example:

```text
OLLAMA_HOST=127.0.0.1:11435 OLLAMA_ORIGINS="https://example.com"
--port 1235 --bind 127.0.0.1 --future-option "two words"
```

The settings validator checks valid environment assignments, declared loopback
bind/CORS controls, managed port options and conflicts before accepting settings.
The executable and optional startup subcommand come from the trusted manifest.
The `--` terminator and vendor flags retain their token positions for that layer.
Unknown vendor option semantics are not decided by the tokenizer.

## Launch resolution boundary

`resolveProcessLaunch` returns the executable, argv and explicit environment
that `bringUpProcess` uses. `resolveCommandLaunch` returns the executable and
argv for each ordered command-mode start step. Existing trusted manifest
placeholder/path expansion is preserved. The builders do not mutate the
caller's resolution context.

User arguments are combined **after** trusted manifest expansion;
storing user text in `runtime.args` or `runtime.start` would interpret literal
placeholders and environment references. Settings persist literal extra tokens
in host-platform `runtime.launch_args` alongside the managed port. Explicit
editable environment assignments are stored as a replacement array in `runtime.launch_env`,
so editing or removing an assignment cannot leave an old value behind through
manifest merging. These values reach both process and command-mode launches
literally, overriding inherited values. An empty array clears prior explicit
editable assignments (inherited OS environment still applies). Environment names
are not allowlisted. Explicit assignments override manifest defaults, including
values such as Linux Ollama's `LD_LIBRARY_PATH`; omitted defaults are restored
from the manifest. Existing trusted `args`,
`start`, `env`, install and other-platform overrides are preserved.

## Managed fields and preview

`runtime.editable_launch` declares fixed startup arguments, an optional
command-mode `start_index`, and field-binding `controls`. A control describes
its flag/environment names, a `value` format and an optional `implicit` value
for flag presence. Binding syntax and policy are separate: the same parser reads
all formats, while four PAIR fields enforce port, loopback and CORS policy.
See [the schema and examples](MANIFEST.md#top-level-fields).

Ollama binds `OLLAMA_HOST` to `{server.host}:{server.port}` and
`OLLAMA_ORIGINS` to `{cors.origins}`. LM Studio binds `--port`/`-p` to
`{server.port}`, `--bind`/`LMS_SERVER_HOST` to `{server.host}`, and `--cors`
to `{cors.enabled}` with an implicit value of `true`. Managed network values
replace every declared environment alias and use the first flag as canonical
output for edited arguments. All CORS fields are local-only. Proxy ports are
owned and validated by the broker, independently of engine argument syntax.
The proxy always follows standard HTTP CORS responses, including absence of
permission, independently of how an engine configures that policy.
Environment assignments such as `OLLAMA_ORIGINS="http://localhost"` can be added,
edited or removed at the beginning of the text, before engine arguments. Names
use letters, digits and underscores and start with a letter or underscore; duplicate names are invalid
(case-insensitive on Windows). Values support spaces, empty strings and the
same literal quoting as arguments. Origin lists are normalized before validation
and storage, including extra surrounding quotes, scheme/host case and whitespace.
Each nonempty origin requires a scheme and specific host; wildcard subdomains
and ports are allowed, but universal origins, credentials, paths and remaining
quotes are rejected. Conflicting repeated origin lists are invalid. Environment values remain
quoted in normalized commands. `OLLAMA_HOST` continues to synchronize with
the server-port field. The editor contains no executable/subcommand. Removing
a managed option reinserts it. Equals-form ports and repeated identical ports
normalize to one managed value; conflicting duplicate ports are invalid.
Declared short aliases accept `-p 1235`, `-p1235` and `-p=1235`. Ambiguous
short-option bundles containing managed controls are rejected without guessing
unknown vendor options' arities. CORS switches accept no attached value; `--cors=false`
is rejected rather than interpreted differently from the engine.
Disagreement with the numeric field returns an explicit conflict unless the
caller supplies `resolution: "server"` or `"launch"`.
The editor supplies that resolution automatically: a changed server-port field
updates the command; otherwise the command's port updates the field. A final
server/proxy collision is rejected with an explicit collision error dialog.

Other vendor options and environment values pass through literally, including
unknown options, short options, configuration-file options and values that the
engine may reject. PAIR maintains no vendor-option catalog and performs no
type, range or compatibility checks for them. Only the adapter-declared networking and CORS controls
receive semantic validation; vendor support is determined at startup. There are
no guessed network aliases or nested-assignment checks. An adapter must identify
the controls PAIR manages; arbitrary vendor options remain opaque.
Changes to declared CORS origin lists, switches and explicit booleans are local to the engine's device; other
environment assignments can be edited through the same local or pinned-peer API.
On startup failure, PAIR returns a bounded tail of the engine output alongside
the exit/readiness failure. It does not classify vendor messages or interpret
options. Supplied positional/assignment values and explicit environment values
are redacted by exact match; engine-transformed values cannot be guaranteed to
match. Do not put secrets in launch values. Custom output is still excluded from
ongoing exported engine logs, and desktop subprocess logs redact launch text,
argument arrays and environment maps. Successful startup discards the capture.

The effective argv and explicit environment are revalidated immediately before
execution, so saved settings from older versions cannot bypass mandatory checks
on a later start.

The wire key remains `launchText`; snapshots identify the new notation with
`format: "pair-arguments-v1"`. Older clients stay read-only for an unknown format.
Existing component `launch_args`/`launch_env` need no migration. The broker
converts saved `pair-launch-v1` journal commands through the internal preview
request before displaying or replaying them, preserving pending arguments and
ports. The legacy executable check exists only on that conversion path.

The broker-facing internal methods are `engine:get-launch`,
`engine:preview-launch`, `engine:configured-ports` and
`engine:configure-launch`. UI/headless clients use the combined broker API
documented in [ENGINE_SETTINGS.md](../nvpair-ui-broker/ENGINE_SETTINGS.md).

Tests cover grammar and normalization, exact argv received by a real isolated
child process, both lifecycle modes, preservation of override fields, reset to
defaults, platform precedence and replacement of an existing settings file.
The existing engine-manager suite also exercises the JSON-RPC stdio path.

## Networking adapter coverage

The bundled engines are Ollama and LM Studio. The shared parser contains no engine
name checks. Their mandatory networking declarations and all platform variants
are pinned by TestBundledNetworkingControls; adding an engine requires extending
that inventory and reviewing its networking alternatives. Other settings have no catalog.

| Engine | Port/bind controls | CORS controls | Vendor reference |
| --- | --- | --- | --- |
| Ollama | OLLAMA_HOST | OLLAMA_ORIGINS, including vendor quote stripping | [Environment source](https://github.com/ollama/ollama/blob/main/envconfig/config.go) |
| LM Studio | --port / -p; --bind / LMS_SERVER_HOST | --cors (no short alias) | [Server command source](https://github.com/lmstudio-ai/lms/blob/main/src/subcommands/server.ts) |

Remote CORS comparisons use normalized policy. The broker derives the internal
preserveCORS preview guard from the authenticated caller and repeats validation
after acquiring its apply lock, before recording a new desired revision. A peer
cannot bypass this check using a stale ingress check or a client-supplied false flag.

Vendor option semantics can change independently of PAIR. New aliases or networking
controls require a manifest/test update; this is the deliberately bounded portion
of engine behavior PAIR owns.
