// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// ManifestSchemaVersion is the highest manifest_version this binary
// understands. A manifest declaring a higher version is rejected (the
// author is asking for fields/behavior we don't have); unknown
// optional fields within a supported version are ignored so additive
// schema growth stays backward compatible.
const ManifestSchemaVersion = 1

const (
	actionHTTPParamsBody  = "body"
	actionHTTPParamsQuery = "query"
)

// allowedPlaceholders is the set of `{token}`s the runner can resolve
// at execution time. Validation rejects any other token so a typo in
// a manifest fails at load with a clear message rather than at run
// time with a mangled command line.
var allowedPlaceholders = map[string]bool{
	"bin":         true,
	"cli":         true,
	"host":        true,
	"port":        true,
	"download":    true,
	"install_dir": true,
	"models_dir":  true,
}

var placeholderRe = regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)
var resultMatchFieldPathRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_-]*(?:\.[a-zA-Z_][a-zA-Z0-9_-]*)*$`)
var artifactNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
var sha256Re = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

// engineNameRe restricts engine names to a safe charset — the name is
// used as a filesystem path component (the per-engine install dir), so
// it must not contain separators (and "."/".." are rejected separately).
var engineNameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// Manifest is one engine's declarative definition. Behavior lives in
// JSON; the engine-agnostic runner executes it. The bundled manifests
// under manifests/ are the authoring reference.
type Manifest struct {
	Engine          string              `json:"engine"`
	DisplayName     string              `json:"display_name"`
	ManifestVersion int                 `json:"manifest_version"`
	Platforms       map[string]Platform `json:"platforms"`
	Actions         map[string]Action   `json:"actions,omitempty"`
}

// Platform is the per-`<goos>/<goarch>` block. Variance lives here
// first; OS-specific Go code is the fallback only for primitives a
// manifest cannot express (process termination, console hiding).
type Platform struct {
	Detect    []string   `json:"detect,omitempty"`
	Install   *Install   `json:"install,omitempty"`
	Uninstall *Uninstall `json:"uninstall,omitempty"`
	Runtime   Runtime    `json:"runtime"`
	// ModelsDir is the engine's on-disk model store, referenced elsewhere as
	// {models_dir}. Nothing this service removes may delete it: Uninstall skips
	// it, and the app-level data purge never reaches it because validation
	// rejects a models_dir that is a placeholder (and therefore cannot be
	// derived from the install directory, which lives under the app data root).
	// Declare the engine's real default — the path it would use on its own —
	// so PAIR and a user's own copy of the engine share one library.
	ModelsDir string `json:"models_dir,omitempty"`
}

// Install describes how to obtain the engine in user mode. Fetch may be
// unpinned, but every member of Artifacts is checksum-verified before `run`
// executes. All downloads are HTTPS-only outside loopback.
type Install struct {
	Fetch     *Fetch            `json:"fetch,omitempty"`
	Artifacts []InstallArtifact `json:"artifacts,omitempty"`
	Run       []string          `json:"run,omitempty"`
	// Script is an escape hatch for vendors that only ship a script
	// installer. It runs without checksum verification — strictly opt-in
	// and logged as unpinned. Prefer fetch+run whenever the vendor publishes
	// a script or artifact so remote content is downloaded before execution.
	Script []string `json:"script,omitempty"`
	// Mode defaults to "user" (user-scoped, no elevation). "admin" is
	// an explicit, rarely-used exception the runner surfaces loudly
	// rather than silently escalating.
	Mode string `json:"mode,omitempty"`
}

// Uninstall removes a user-mode install. Run is for work only a command can do
// — stopping the engine's own daemon, or invoking a vendor uninstaller. Remove
// lists the trees this service deletes itself, which is how model weights
// survive: the runner always skips the platform's ModelsDir, so a Remove entry
// that contains the model store takes everything except the models. Prefer
// Remove over an `rm -rf` in Run; a shell string cannot be checked.
type Uninstall struct {
	Run    []string `json:"run,omitempty"`
	Remove []string `json:"remove,omitempty"`
}

// Fetch is an engine download. SHA256, when set, pins it (verified
// before run); when empty the download is HTTPS-only with a warning.
type Fetch struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// InstallArtifact is one checksum-pinned member of a multi-file install.
// Its download is available to install.run as {download_<name>}.
type InstallArtifact struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Runtime is how to launch + probe the engine. By default the launched
// engine binds loopback; an engine may set Bind (substituted as {host})
// to listen elsewhere — inference engines use "0.0.0.0" to serve the
// cluster — and a per-call bind (engine:start) overrides it. Readiness
// and health probes always target 127.0.0.1, which a 0.0.0.0 listener
// also answers. The open default is an interim exception to loopback-only and
// should be narrowed once authenticated inference transport is available.
//
// Mode selects the lifecycle model:
//   - "process" (default): the engine is a foreground process this
//     service spawns and owns; liveness = the process is alive.
//   - "command": the engine is a daemon brought up/down by commands
//     (e.g. LM Studio's `lms`); liveness = the readiness/health probe,
//     and Stop.Cmd brings it down.
type Runtime struct {
	EditableLaunch *EditableLaunch   `json:"editable_launch,omitempty"`
	LaunchArgs     *[]string         `json:"launch_args,omitempty"` // literal arguments after managed fields
	LaunchEnv      *[]string         `json:"launch_env,omitempty"`  // complete literal explicit environment
	Mode           string            `json:"mode,omitempty"`
	Bin            string            `json:"bin,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Port           int               `json:"port"`            // 0 => auto-assign a free loopback port
	Bind           string            `json:"bind,omitempty"`  // listen addr, substituted as {host}; "" => 127.0.0.1
	Start          [][]string        `json:"start,omitempty"` // command mode: ordered bring-up commands
	// CLI is the engine's control-CLI path for this platform, referenced
	// elsewhere as {cli}. It lets the manifest's global actions resolve
	// to the correct per-OS binary (e.g. lms.exe vs lms).
	CLI    string    `json:"cli,omitempty"`
	Ready  *Probe    `json:"ready,omitempty"`
	Stop   *StopSpec `json:"stop,omitempty"`
	Health *Probe    `json:"health,omitempty"`
}

type EditableLaunch struct {
	FixedArgs  []string        `json:"fixed_args"`
	Controls   []LaunchControl `json:"controls"`
	StartIndex int             `json:"start_index,omitempty"`
}

// LaunchControl binds CLI/environment syntax to PAIR policy fields. Value is a
// format such as "{server.port}" or "{server.host}:{server.port}". Implicit is
// the value supplied by flag presence; environment sources always take a value.
type LaunchControl struct {
	Flags    []string `json:"flags,omitempty"`
	Env      []string `json:"env,omitempty"`
	Value    string   `json:"value"`
	Implicit *string  `json:"implicit,omitempty"`
}

func (r *Runtime) modeOrDefault() string {
	if r.Mode == "" {
		return "process"
	}
	return r.Mode
}

// hasCustomLaunch reports whether the user supplied any literal argument or
// environment assignment. A saved override writes both keys, so the untouched
// one persists as an empty — not absent — slice. Emptiness, not nil, is what
// separates a default launch from a customized one.
func (r *Runtime) hasCustomLaunch() bool {
	return (r.LaunchArgs != nil && len(*r.LaunchArgs) > 0) ||
		(r.LaunchEnv != nil && len(*r.LaunchEnv) > 0)
}

// ProbeJSONMatch optionally identifies an HTTP service by a string field in
// its JSON response body. Field supports the same validated dotted object path
// syntax as action result filters.
type ProbeJSONMatch struct {
	Field string `json:"field"`
	Value string `json:"value"`
}

// Probe is an HTTP or TCP reachability check. Exactly one of HTTP/TCP should be
// set; HTTP wins if both are. JSONMatch is valid only for HTTP probes.
type Probe struct {
	HTTP      string          `json:"http,omitempty"`   // url template, e.g. "http://127.0.0.1:{port}/"
	TCP       string          `json:"tcp,omitempty"`    // host:port template, e.g. "127.0.0.1:{port}"
	Status    int             `json:"status,omitempty"` // expected HTTP status (default 200)
	TimeoutS  int             `json:"timeout_s,omitempty"`
	IntervalS int             `json:"interval_s,omitempty"`
	JSONMatch *ProbeJSONMatch `json:"json_match,omitempty"`
}

// StopSpec is how to terminate the engine. Default is a graceful
// signal followed by a forced kill after grace.
type StopSpec struct {
	Signal string   `json:"signal,omitempty"` // "term" (default) | "kill"
	Cmd    []string `json:"cmd,omitempty"`    // optional explicit stop command
	GraceS int      `json:"grace_s,omitempty"`
}

// Action is a config-declared operation exposed over engine:action.
// Exactly one of HTTP (call the engine's loopback control API), Cmd
// (run a CLI command, e.g. `lms get`), or RemovePath (guarded filesystem
// delete) is set. Only a Cmd action templates the action's params as
// placeholders (e.g. {model}); an HTTP action sends params in its declared
// body or query location.
type Action struct {
	Description string            `json:"description,omitempty"`
	HTTP        *ActionHTTP       `json:"http,omitempty"`
	Cmd         []string          `json:"cmd,omitempty"`
	RemovePath  *ActionRemovePath `json:"remove_path,omitempty"`
	// ProgressProtocol selects a narrowly defined streaming adapter for an
	// asynchronous pull HTTP action. Empty keeps the ordinary response-stream
	// behavior; named protocols are validated so a typo cannot silently fall
	// back to the wrong completion semantics.
	ProgressProtocol string `json:"progress_protocol,omitempty"`
	// ModelResolution, when set, expands or resolves the model param:
	//   - "lms-get" on Cmd actions: try as-given → Hub id → Hugging Face URL.
	//   - "lms-disk-path" on RemovePath actions: map logical ids to on-disk
	//     files via `lms ls --json`.
	ModelResolution string `json:"model_resolution,omitempty"`
	// Result, when set, declares how to extract a normalized string list from
	// this action's JSON response — the top-level array field to iterate and the
	// string field to pull from each element. engine:models uses it to turn
	// engine-specific list_models shapes (Ollama {models:[{name}]}, LM Studio
	// {models:[{key}]}) into a flat []string with no per-engine code in the runner.
	Result *ActionResult `json:"result,omitempty"`
	// RestartAfter restarts the engine once this action succeeds, for an action
	// that changes state the engine only reads at startup. LM Studio's
	// delete_model removes the files, but its server keeps serving the model
	// from an index it builds at startup and exposes no rescan operation, so
	// only a restart makes the deletion visible to clients. A stopped engine is
	// left stopped; a restart failure fails the action.
	RestartAfter bool `json:"restart_after,omitempty"`
}

// ActionResult is the list-extraction spec on an Action (see Action.Result).
type ActionResult struct {
	Array string `json:"array"` // top-level array field, e.g. "models" / "data"
	Field string `json:"field"` // string field per element, e.g. "name" / "id"
	// Match, when set, keeps only array elements that pass the ResultMatch
	// filter. It lets loaded_models reuse the same extractor as list_models
	// across engines whose list endpoint tags residency (LM Studio's
	// /api/v1/models models[].loaded_instances nonempty, or a string state
	// field) rather than returning a presence-only list (Ollama's /api/ps,
	// every row of which is resident and so needs no Match).
	Match *ResultMatch `json:"match,omitempty"`
}

// ResultMatch is the optional row filter on an ActionResult. Exactly one of
// In or Nonempty must be set:
//   - In: keep the element when the dot-separated object path Field (decoded as
//     a JSON string) equals one of In.
//   - Nonempty: keep the element when Field resolves to a JSON array with length
//     > 0 (LM Studio's /api/v1/models models[].loaded_instances).
type ResultMatch struct {
	Field    string   `json:"field"`              // element field to test, e.g. "state" / "loaded_instances"
	In       []string `json:"in,omitempty"`       // accepted string values, e.g. ["loaded"]
	Nonempty bool     `json:"nonempty,omitempty"` // true → Field must be a nonempty JSON array
}

// ActionRemovePath deletes a filesystem path declared in the manifest.
// Path and Root are templated; the resolved target must stay under Root.
type ActionRemovePath struct {
	Path string `json:"path"`
	Root string `json:"root"`
}

// ActionHTTP is a templated call against the engine's loopback base URL.
// ParamsIn selects whether the caller's params are sent as the JSON request
// body (the default) or as URL-encoded query parameters. BodySchema is
// informational only.
type ActionHTTP struct {
	Method     string          `json:"method"`
	Path       string          `json:"path"`
	ParamsIn   string          `json:"params_in,omitempty"`
	BodySchema json.RawMessage `json:"body_schema,omitempty"`
}

// ModeOrDefault returns the effective install mode ("user" when unset).
func (i *Install) ModeOrDefault() string {
	if i == nil || i.Mode == "" {
		return "user"
	}
	return i.Mode
}

// Registry holds validated manifests keyed by engine name.
type Registry struct {
	engines map[string]*Manifest
	// bundledRaw is the raw JSON of each bundled manifest (pre-merge),
	// keyed by engine name. LoadOverrideDir deep-merges a per-user
	// override onto this base so a partial override file pins only the
	// fields it sets (e.g. runtime.port) while inheriting everything else.
	bundledRaw map[string][]byte
}

// Get returns the manifest for an engine name.
func (r *Registry) Get(name string) (*Manifest, bool) {
	m, ok := r.engines[name]
	return m, ok
}

// Names returns all known engine names, sorted for stable output.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.engines))
	for n := range r.engines {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// NewRegistry returns an empty registry ready for LoadDir / LoadFS.
func NewRegistry() *Registry {
	return &Registry{
		engines:    make(map[string]*Manifest),
		bundledRaw: make(map[string][]byte),
	}
}

// LoadRegistry reads *.json manifests from each filesystem dir in
// order; a later dir overrides an earlier one by engine name (so a
// user manifest shadows a bundled one). Missing dirs are skipped. An
// invalid manifest is a hard error naming the file.
func LoadRegistry(dirs ...string) (*Registry, error) {
	r := NewRegistry()
	for _, dir := range dirs {
		if err := r.LoadDir(dir); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// LoadDir merges all *.json manifests from a filesystem directory.
// A missing directory is not an error (skipped). Manifests already
// loaded are overridden by same-engine entries found here.
func (r *Registry) LoadDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read manifest dir %s: %w", dir, err)
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].Name() < entries[b].Name() })
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read manifest %s: %w", p, err)
		}
		if _, err := r.addManifest(p, data); err != nil {
			return err
		}
	}
	return nil
}

// LoadFS merges all *.json manifests under root within an fs.FS — used
// for the manifests embedded in the binary via go:embed.
func (r *Registry) LoadFS(fsys fs.FS, root string) error {
	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		return fmt.Errorf("read embedded manifests %s: %w", root, err)
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].Name() < entries[b].Name() })
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(path.Ext(e.Name()), ".json") {
			continue
		}
		name := path.Join(root, e.Name())
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("read embedded manifest %s: %w", name, err)
		}
		eng, err := r.addManifest(name, data)
		if err != nil {
			return err
		}
		// Remember the raw bundled JSON so LoadOverrideDir can deep-merge
		// a per-user override onto it later.
		r.bundledRaw[eng] = append([]byte(nil), data...)
	}
	return nil
}

func (r *Registry) addManifest(src string, data []byte) (string, error) {
	merged, err := applyPlatformDefaults(data)
	if err != nil {
		return "", fmt.Errorf("%s: %w", src, err)
	}
	var m Manifest
	if err := json.Unmarshal(merged, &m); err != nil {
		return "", fmt.Errorf("%s: invalid JSON: %w", src, err)
	}
	if err := m.Validate(); err != nil {
		return "", fmt.Errorf("%s: %w", src, err)
	}
	r.engines[m.Engine] = &m
	return m.Engine, nil
}

// LoadOverrideDir overlays per-user manifests from a config directory onto
// the bundled manifests already loaded via LoadFS. Unlike LoadDir's
// wholesale replace, each file is deep-merged onto the bundled manifest of
// the same engine (override keys win), so a partial override — e.g. just a
// changed runtime.port — pins only that field and keeps inheriting every
// other bundled value (install URLs, actions, fixes) across upgrades. A file
// for an engine with no bundled base is loaded standalone. A missing dir is
// not an error; a single bad file is skipped (logged) so it can't brick
// startup.
func (r *Registry) LoadOverrideDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read override dir %s: %w", dir, err)
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].Name() < entries[b].Name() })
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			slog.Warn("skipping unreadable override manifest", "path", p, "err", err)
			continue
		}
		merged, err := r.mergeOntoBundled(data)
		if err != nil {
			slog.Warn("skipping invalid override manifest", "path", p, "err", err)
			continue
		}
		if _, err := r.addManifest(p, merged); err != nil {
			slog.Warn("skipping invalid override manifest", "path", p, "err", err)
			continue
		}
	}
	return nil
}

// applyLocationOverrides takes from each user override only where the engine's
// things are: its port and its model store. The uninstaller runs with
// privileges the override's author may not have, so the commands it runs and
// the paths it deletes stay bundled. A port decides where it looks for the
// engine still running, and a model store decides what removal keeps — the
// same store the running engine manager, which reads the whole override,
// already keeps. An override that would leave the platform invalid is ignored,
// as it is at startup.
func (r *Registry) applyLocationOverrides(dir string) {
	if dir == "" {
		return
	}
	host := runtime.GOOS + "/" + runtime.GOARCH
	for name, m := range r.engines {
		data, err := os.ReadFile(filepath.Join(dir, name+".json"))
		if err != nil {
			continue
		}
		type portOnly struct {
			Port int `json:"port"`
		}
		var override struct {
			Engine    string   `json:"engine"`
			ModelsDir string   `json:"models_dir"`
			Runtime   portOnly `json:"runtime"`
			Platforms map[string]struct {
				ModelsDir string   `json:"models_dir"`
				Runtime   portOnly `json:"runtime"`
			} `json:"platforms"`
		}
		if err := json.Unmarshal(data, &override); err != nil || override.Engine != name {
			continue
		}
		platform, ok := m.Platforms[host]
		if !ok {
			continue
		}
		hostOverride := override.Platforms[host]
		port := override.Runtime.Port
		if hostOverride.Runtime.Port != 0 {
			port = hostOverride.Runtime.Port
		}
		if port > 0 && port <= 65535 {
			platform.Runtime.Port = port
		}
		store := override.ModelsDir
		if hostOverride.ModelsDir != "" {
			store = hostOverride.ModelsDir
		}
		if store != "" {
			platform.ModelsDir = store
		}
		if err := platform.validate(host); err != nil {
			slog.Warn("ignoring an override's port and model store", "engine", name, "err", err)
			continue
		}
		m.Platforms[host] = platform
	}
}

// bundledDefaultPort returns the host-platform runtime.port from the bundled
// (un-overridden) manifest for an engine, used to decide whether a chosen
// port is back at the default (so its override file can be dropped).
func (r *Registry) bundledDefaultPort(engine string) (int, bool) {
	raw, ok := r.bundledRaw[engine]
	if !ok {
		return 0, false
	}
	merged, err := applyPlatformDefaults(raw)
	if err != nil {
		return 0, false
	}
	var m Manifest
	if err := json.Unmarshal(merged, &m); err != nil {
		return 0, false
	}
	p, ok := m.HostPlatform()
	if !ok {
		return 0, false
	}
	return p.Runtime.Port, true
}

// mergeOntoBundled deep-merges an override manifest's raw JSON onto the
// bundled manifest of the same engine (override wins). When the override
// declares no engine, or no bundled base exists for it, the override is
// returned unchanged to be loaded as a standalone manifest.
func (r *Registry) mergeOntoBundled(override []byte) ([]byte, error) {
	var om map[string]any
	if err := json.Unmarshal(override, &om); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	eng, _ := om["engine"].(string)
	base, ok := r.bundledRaw[eng]
	if eng == "" || !ok {
		return override, nil
	}
	var bm map[string]any
	if err := json.Unmarshal(base, &bm); err != nil {
		return nil, fmt.Errorf("bundled manifest for %q: %w", eng, err)
	}
	return json.Marshal(deepMerge(bm, om))
}

// applyPlatformDefaults implements manifest-level shared defaults: a
// top-level "detect" / "install" / "uninstall" / "runtime" / "models_dir"
// block is merged into every entry under "platforms", with the per-platform block
// winning key-by-key. Nested objects (runtime, runtime.env, …) merge
// recursively; arrays and scalars are replaced wholesale. The merge runs
// at the JSON level before unmarshalling, so an omitted key truly
// inherits the default while an explicit zero value (e.g. "port": 0)
// still overrides — a distinction a Go zero value can't express. A
// manifest with no top-level defaults is returned unchanged, so fully
// per-platform manifests keep loading exactly as before.
func applyPlatformDefaults(data []byte) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	base := map[string]any{}
	for _, k := range []string{"detect", "install", "uninstall", "runtime", "models_dir"} {
		raw, ok := top[k]
		if !ok {
			continue
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("top-level %s: %w", k, err)
		}
		base[k] = v
		delete(top, k) // these now live inside each platform after the merge
	}
	if len(base) == 0 {
		return data, nil // no shared defaults — nothing to merge
	}
	raw, ok := top["platforms"]
	if !ok {
		return data, nil // Validate() will report the missing platforms
	}
	var platforms map[string]json.RawMessage
	if err := json.Unmarshal(raw, &platforms); err != nil {
		return nil, fmt.Errorf("platforms: %w", err)
	}
	merged := make(map[string]any, len(platforms))
	for key, praw := range platforms {
		var pv map[string]any
		if err := json.Unmarshal(praw, &pv); err != nil {
			return nil, fmt.Errorf("platform %q: %w", key, err)
		}
		merged[key] = deepMerge(base, pv)
	}
	mb, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	top["platforms"] = mb
	return json.Marshal(top)
}

// deepMerge overlays override onto base and returns the result without
// mutating either: nested JSON objects merge recursively (override keys
// win); arrays and scalars are replaced by override.
func deepMerge(base, override map[string]any) map[string]any {
	out := make(map[string]any, len(base))
	for k, v := range base {
		out[k] = v
	}
	for k, ov := range override {
		if bv, ok := out[k]; ok {
			if bm, ok1 := bv.(map[string]any); ok1 {
				if om, ok2 := ov.(map[string]any); ok2 {
					out[k] = deepMerge(bm, om)
					continue
				}
			}
		}
		out[k] = ov
	}
	return out
}

// Validate enforces the schema rules. Errors are specific and
// human-readable so a manifest author can fix them without reading
// the code.
func (m *Manifest) Validate() error {
	if strings.TrimSpace(m.Engine) == "" {
		return errors.New("engine is required")
	}
	if !engineNameRe.MatchString(m.Engine) || m.Engine == "." || m.Engine == ".." {
		return fmt.Errorf("engine %q must match [A-Za-z0-9._-]+ (it is used as a filesystem path component)", m.Engine)
	}
	if strings.TrimSpace(m.DisplayName) == "" {
		return errors.New("display_name is required")
	}
	if m.ManifestVersion <= 0 {
		return errors.New("manifest_version must be >= 1")
	}
	if m.ManifestVersion > ManifestSchemaVersion {
		return fmt.Errorf("manifest_version %d is newer than supported %d", m.ManifestVersion, ManifestSchemaVersion)
	}
	if len(m.Platforms) == 0 {
		return errors.New("at least one platforms entry is required")
	}
	for key, p := range m.Platforms {
		if !validPlatformKey(key) {
			return fmt.Errorf("platform key %q must be \"<goos>/<goarch>\"", key)
		}
		if err := p.validate(key); err != nil {
			return err
		}
	}
	for name, a := range m.Actions {
		if err := a.validate(name); err != nil {
			return err
		}
		// restart_after promises the caller that the action's effect is visible
		// by the time it replies, and the runner keeps that promise by not
		// replying until the engine passes its readiness probe. Without a
		// runtime.ready the probe is a no-op: doStart returns the moment the
		// process is spawned, the reply races the engine's own startup, and the
		// caller can still be handed the pre-restart state. Fail at load rather
		// than ship a guarantee the manifest cannot keep.
		if a.RestartAfter {
			for key, p := range m.Platforms {
				if p.Runtime.Ready == nil {
					return fmt.Errorf("action %q: restart_after requires platform %q to declare runtime.ready (the restart is only observable once the engine is ready again)", name, key)
				}
			}
		}
	}
	return m.validatePlaceholders()
}

func (p *Platform) validate(key string) error {
	if policy := p.Runtime.EditableLaunch; policy != nil {
		if policy.StartIndex < 0 || (p.Runtime.modeOrDefault() == "command" && policy.StartIndex >= len(p.Runtime.Start)) {
			return fmt.Errorf("platform %q: invalid editable launch definition", key)
		}
		if _, err := formatLaunchText(policy.FixedArgs); err != nil {
			return fmt.Errorf("platform %q: invalid fixed launch arguments", key)
		}
		if err := policy.validateControls(); err != nil {
			return fmt.Errorf("platform %q: %w", key, err)
		}
	}
	if args := p.Runtime.LaunchArgs; args != nil {
		if p.Runtime.EditableLaunch == nil {
			return fmt.Errorf("platform %q: launch_args requires editable_launch", key)
		}
		if _, err := formatLaunchText(append([]string{"arguments"}, (*args)...)); err != nil {
			return fmt.Errorf("platform %q: invalid literal launch arguments", key)
		}
	}
	if env := p.Runtime.LaunchEnv; env != nil {
		if p.Runtime.EditableLaunch == nil {
			return fmt.Errorf("platform %q: launch_env requires editable_launch", key)
		}
		if _, err := literalEnvironment(*env); err != nil {
			return fmt.Errorf("platform %q: invalid literal launch environment", key)
		}
	}
	switch p.Runtime.modeOrDefault() {
	case "process":
		if strings.TrimSpace(p.Runtime.Bin) == "" {
			return fmt.Errorf("platform %q: runtime.bin is required in process mode", key)
		}
	case "command":
		if len(p.Runtime.Start) == 0 {
			return fmt.Errorf("platform %q: runtime.start is required in command mode", key)
		}
	default:
		return fmt.Errorf("platform %q: runtime.mode %q invalid (want \"process\" or \"command\")", key, p.Runtime.Mode)
	}
	if p.Install != nil {
		hasArtifacts := len(p.Install.Artifacts) > 0
		if len(p.Install.Script) > 0 && (p.Install.Fetch != nil || hasArtifacts || len(p.Install.Run) > 0) {
			return fmt.Errorf("platform %q: install.script is mutually exclusive with fetch/artifacts/run (a script install cannot also be checksum-pinned)", key)
		}
		if p.Install.Fetch != nil && hasArtifacts {
			return fmt.Errorf("platform %q: install.fetch and install.artifacts are mutually exclusive", key)
		}
		if (p.Install.Fetch != nil || hasArtifacts) && len(p.Install.Run) == 0 {
			return fmt.Errorf("platform %q: install.run is required when install.fetch or install.artifacts is present", key)
		}
		if len(p.Install.Run) > 0 && p.Install.Fetch == nil && !hasArtifacts {
			return fmt.Errorf("platform %q: install.run requires a fetch or artifacts (the downloads the run command uses)", key)
		}
		if p.Install.Fetch != nil && strings.TrimSpace(p.Install.Fetch.URL) == "" {
			return fmt.Errorf("platform %q: install.fetch.url is required when fetch is present", key)
		}
		names := make(map[string]bool, len(p.Install.Artifacts))
		for index, artifact := range p.Install.Artifacts {
			if !artifactNameRe.MatchString(artifact.Name) {
				return fmt.Errorf("platform %q: install.artifacts[%d].name %q must match [a-z][a-z0-9_]{0,31}", key, index, artifact.Name)
			}
			if names[artifact.Name] {
				return fmt.Errorf("platform %q: duplicate install artifact name %q", key, artifact.Name)
			}
			names[artifact.Name] = true
			if err := validateDownloadURL(artifact.URL); err != nil {
				return fmt.Errorf("platform %q: install artifact %q: %w", key, artifact.Name, err)
			}
			if !sha256Re.MatchString(strings.TrimSpace(artifact.SHA256)) {
				return fmt.Errorf("platform %q: install artifact %q requires a 64-character hexadecimal sha256", key, artifact.Name)
			}
		}
		switch p.Install.Mode {
		case "", "user", "admin":
		default:
			return fmt.Errorf("platform %q: install.mode %q invalid (want \"user\" or \"admin\")", key, p.Install.Mode)
		}
	}
	// A models_dir derived from {install_dir} is the mistake this field exists to
	// prevent: the install directory lives under the app data root, so the
	// app-level "remove all data" uninstall would take the weights with it even
	// though no engine uninstall ever touched them. Keeping it literal means the
	// only way to point the model store at the data root is to type that path
	// out, which review will catch.
	if strings.Contains(p.ModelsDir, "{") {
		return fmt.Errorf("platform %q: models_dir %q must be a literal path, not a template — the model store must not be derived from the install directory", key, p.ModelsDir)
	}
	if p.Uninstall != nil {
		if len(p.Uninstall.Run) == 0 && len(p.Uninstall.Remove) == 0 {
			return fmt.Errorf("platform %q: uninstall needs run, remove, or both when present", key)
		}
		// Without a model store there is nothing for the runner to preserve, so
		// a removal would be the unconditional delete this field exists to
		// prevent. Requiring it here is what makes the guarantee hold for a
		// hand-written manifest and not only for the bundled ones.
		if len(p.Uninstall.Remove) > 0 && strings.TrimSpace(p.ModelsDir) == "" {
			return fmt.Errorf("platform %q: uninstall.remove requires models_dir, so the removal knows what to keep", key)
		}
		for _, target := range p.Uninstall.Remove {
			// {models_dir} resolves only at uninstall time, so a templated
			// target would pass this check and fail every retry instead. Reject
			// the token outright: every spelling of it names the store or a part
			// of it, and neither may be removed.
			if strings.Contains(target, "{models_dir}") {
				return fmt.Errorf("platform %q: uninstall.remove %q names the model store — uninstall removes the engine, not the user's models", key, target)
			}
			resolved := filepath.Clean(expandPath(target))
			store := filepath.Clean(expandPath(p.ModelsDir))
			// At the store, or inside it. Removing part of a model library is
			// as destructive as removing all of it. Compared without case: one
			// manifest runs on filesystems of both kinds, and wherever case is
			// ignored a second spelling is the store.
			if pathWithinRoot(strings.ToLower(store), strings.ToLower(resolved)) {
				return fmt.Errorf("platform %q: uninstall.remove %q is at or inside the model store %q — uninstall removes the engine, not the user's models", key, target, p.ModelsDir)
			}
		}
	}
	if err := validateProbe(key, "ready", p.Runtime.Ready); err != nil {
		return err
	}
	if err := validateProbe(key, "health", p.Runtime.Health); err != nil {
		return err
	}
	return nil
}

// validateProbe rejects a present-but-empty probe. probe() treats a
// non-nil probe with neither http nor tcp set as "instantly ready", which
// would silently disable the readiness/health check the manifest author
// declared — so fail at load instead.
func validateProbe(key, which string, p *Probe) error {
	if p == nil {
		return nil
	}
	if strings.TrimSpace(p.HTTP) == "" && strings.TrimSpace(p.TCP) == "" {
		return fmt.Errorf("platform %q: runtime.%s must set either http or tcp", key, which)
	}
	if p.JSONMatch != nil {
		if strings.TrimSpace(p.HTTP) == "" {
			return fmt.Errorf("platform %q: runtime.%s json_match requires http", key, which)
		}
		if !resultMatchFieldPathRe.MatchString(p.JSONMatch.Field) {
			return fmt.Errorf("platform %q: runtime.%s json_match.field %q is not a valid object path", key, which, p.JSONMatch.Field)
		}
		if strings.TrimSpace(p.JSONMatch.Value) == "" {
			return fmt.Errorf("platform %q: runtime.%s json_match.value is required", key, which)
		}
	}
	return nil
}

func (a *Action) validate(name string) error {
	hasHTTP := a.HTTP != nil
	hasCmd := len(a.Cmd) > 0
	hasRemovePath := a.RemovePath != nil
	kinds := 0
	if hasHTTP {
		kinds++
	}
	if hasCmd {
		kinds++
	}
	if hasRemovePath {
		kinds++
	}
	if kinds != 1 {
		return fmt.Errorf("action %q: exactly one of http, cmd, or remove_path is required", name)
	}
	if hasRemovePath {
		if strings.TrimSpace(a.RemovePath.Path) == "" || strings.TrimSpace(a.RemovePath.Root) == "" {
			return fmt.Errorf("action %q: remove_path.path and remove_path.root are required", name)
		}
	}
	if hasHTTP && (strings.TrimSpace(a.HTTP.Method) == "" || strings.TrimSpace(a.HTTP.Path) == "") {
		return fmt.Errorf("action %q: http.method and http.path are required", name)
	}
	if hasHTTP {
		switch a.HTTP.ParamsIn {
		case "", actionHTTPParamsBody, actionHTTPParamsQuery:
		default:
			return fmt.Errorf("action %q: http.params_in %q invalid (want %q or %q)", name, a.HTTP.ParamsIn, actionHTTPParamsBody, actionHTTPParamsQuery)
		}
	}
	if a.ProgressProtocol != "" {
		if name != pullModelAction || !hasHTTP {
			return fmt.Errorf("action %q: progress_protocol requires the HTTP pull_model action", name)
		}
		if a.ProgressProtocol != pullProgressProtocolLlamaCPPModelsSSE {
			return fmt.Errorf("action %q: unsupported progress_protocol %q", name, a.ProgressProtocol)
		}
	}
	if a.Result != nil && (strings.TrimSpace(a.Result.Array) == "" || strings.TrimSpace(a.Result.Field) == "") {
		return fmt.Errorf("action %q: result.array and result.field are required when result is set", name)
	}
	if a.Result != nil && a.Result.Match != nil {
		m := a.Result.Match
		if strings.TrimSpace(m.Field) == "" {
			return fmt.Errorf("action %q: result.match.field is required when result.match is set", name)
		}
		if !resultMatchFieldPathRe.MatchString(m.Field) {
			return fmt.Errorf("action %q: result.match.field %q is not a valid object path", name, m.Field)
		}
		hasIn := len(m.In) > 0
		if hasIn == m.Nonempty {
			return fmt.Errorf("action %q: result.match requires exactly one of a non-empty in or nonempty=true", name)
		}
	}
	if a.ModelResolution != "" {
		switch a.ModelResolution {
		case modelResolutionLMSGet:
			if !hasCmd {
				return fmt.Errorf("action %q: model_resolution requires a cmd action", name)
			}
			if !cmdReferencesModel(a.Cmd) {
				return fmt.Errorf("action %q: model_resolution requires the cmd to template {model}", name)
			}
		case modelResolutionLMSDiskPath:
			if !hasRemovePath {
				return fmt.Errorf("action %q: model_resolution %q requires remove_path", name, a.ModelResolution)
			}
		default:
			return fmt.Errorf("action %q: model_resolution %q invalid (want %q or %q)", name, a.ModelResolution, modelResolutionLMSGet, modelResolutionLMSDiskPath)
		}
	}
	return nil
}

// validatePlaceholders rejects any `{token}` outside the placeholders
// available to the platform or manifest-global action that contains it.
func (m *Manifest) validatePlaceholders() error {
	for key, platform := range m.Platforms {
		allowed := make(map[string]bool, len(allowedPlaceholders))
		for name := range allowedPlaceholders {
			allowed[name] = true
		}
		if platform.Install != nil {
			for _, artifact := range platform.Install.Artifacts {
				allowed["download_"+artifact.Name] = true
			}
		}
		if err := validatePlaceholderStrings(platform.templatedStrings(), allowed); err != nil {
			return fmt.Errorf("platform %q: %w", key, err)
		}
	}
	return validatePlaceholderStrings(m.actionTemplatedStrings(), allowedPlaceholders)
}

func validatePlaceholderStrings(templates []string, allowed map[string]bool) error {
	for _, template := range templates {
		for _, match := range placeholderRe.FindAllStringSubmatch(template, -1) {
			if !allowed[match[1]] {
				return fmt.Errorf("unknown placeholder {%s} (allowed: %s)", match[1], strings.Join(placeholderList(allowed), ", "))
			}
		}
	}
	return nil
}

func placeholderList(placeholders map[string]bool) []string {
	out := make([]string, 0, len(placeholders))
	for k := range placeholders {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// templatedStrings collects every platform-local string the runner resolves
// placeholders in, so artifact placeholders stay scoped to their platform.
func (p Platform) templatedStrings() []string {
	var out []string
	out = append(out, p.Detect...)
	if p.Install != nil {
		out = append(out, p.Install.Run...)
		out = append(out, p.Install.Script...)
	}
	if p.Uninstall != nil {
		out = append(out, p.Uninstall.Run...)
		out = append(out, p.Uninstall.Remove...)
	}
	out = append(out, p.Runtime.Bin)
	out = append(out, p.Runtime.Args...)
	for _, cmd := range p.Runtime.Start {
		out = append(out, cmd...)
	}
	for _, v := range p.Runtime.Env {
		out = append(out, v)
	}
	out = append(out, probeStrings(p.Runtime.Ready)...)
	out = append(out, probeStrings(p.Runtime.Health)...)
	if p.Runtime.Stop != nil {
		out = append(out, p.Runtime.Stop.Cmd...)
	}
	return out
}

func (m *Manifest) actionTemplatedStrings() []string {
	var out []string
	for _, act := range m.Actions {
		if act.RemovePath != nil {
			out = append(out, act.RemovePath.Root)
			// Path may template caller params (e.g. {model}); validated at call time.
		}
	}
	// Action http.path / cmd are resolved from runtime params (e.g.
	// {model}), so they're validated at call time, not statically here.
	return out
}

func probeStrings(p *Probe) []string {
	if p == nil {
		return nil
	}
	return []string{p.HTTP, p.TCP}
}

// PlatformFor returns the platform block for the given goos/goarch.
func (m *Manifest) PlatformFor(goos, goarch string) (*Platform, bool) {
	p, ok := m.Platforms[goos+"/"+goarch]
	if !ok {
		return nil, false
	}
	return &p, true
}

// HostPlatform returns the platform block for the running host.
func (m *Manifest) HostPlatform() (*Platform, bool) {
	return m.PlatformFor(runtime.GOOS, runtime.GOARCH)
}

func validPlatformKey(k string) bool {
	parts := strings.Split(k, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

// resolvePlaceholders substitutes {token}s using vars and errors on
// any token not provided. Used at execution time once concrete values
// (bin path, chosen port, download path) are known.
func resolvePlaceholders(s string, vars map[string]string) (string, error) {
	var missing string
	out := placeholderRe.ReplaceAllStringFunc(s, func(tok string) string {
		name := tok[1 : len(tok)-1]
		if v, ok := vars[name]; ok {
			return v
		}
		if missing == "" {
			missing = name
		}
		return tok
	})
	if missing != "" {
		return "", fmt.Errorf("unresolved placeholder {%s}", missing)
	}
	return out, nil
}

// resolveArgs resolves placeholders across a slice (argv / args).
func resolveArgs(in []string, vars map[string]string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		r, err := resolvePlaceholders(s, vars)
		if err != nil {
			return nil, err
		}
		out[i] = r
	}
	return out, nil
}
