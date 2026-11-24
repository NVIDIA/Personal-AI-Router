// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The bodies in this file are measurements, not hand-written examples.
//
// sagittariusModelsBody is the `mtplx models --json` answer captured on the Mac
// that runs these tests: one cache root, /Users/Shared/mtplx/models, and one pack
// in it. A /health answer from the same round of probing reported that pack's
// directory verbatim while calling the served model
// "mtplx-flash-next-optimized-speed", and that pair is what the first test pins:
// the listing's `path` and the served model's `model_path` are one string, and the
// served name matches neither. bellatrixModelsBody is the second Mac's cache,
// re-expressed as a JSON body in the same envelope because that capture is a
// digest of its listing; its values (three packs, all under
// /Users/steven/.mtplx/models, one of them with no runtime contract and
// four-and-a-bit required files missing) are the measured ones. Nothing here is
// read from disk at test time and nothing here reaches a port, so both bodies keep
// working on a machine that never had MTPLX on it — which matters most for the
// path joins, since APFS is case-insensitive by default and a join that leaned on
// the filesystem would pass here for the wrong reason.

const sagittariusModelsBody = `{
  "cache_dir": "/Users/Shared/mtplx/models",
  "model_roots": ["/Users/Shared/mtplx/models"],
  "models": [
    {
      "delete_command": "mtplx remove Youssofal/Qwen3.8-Flash-Next-MTPLX-Optimized-Speed",
      "has_config": true,
      "has_runtime_contract": true,
      "is_primary": true,
      "path": "/Users/Shared/mtplx/models/Youssofal--Qwen3.8-Flash-Next-MTPLX-Optimized-Speed",
      "recommended_profile": "turbo",
      "repo_id": "Youssofal/Qwen3.8-Flash-Next-MTPLX-Optimized-Speed",
      "root": "/Users/Shared/mtplx/models",
      "size_bytes": 115061262324,
      "validation": {
        "contract_present": true,
        "missing_files": [],
        "ok": true
      }
    }
  ]
}`

const bellatrixModelsBody = `{
  "cache_dir": "/Users/steven/.mtplx/models",
  "model_roots": ["/Users/steven/.mtplx/models"],
  "models": [
    {
      "delete_command": "mtplx remove Youssofal/Gemma4-MTPLX-Optimized-Speed",
      "has_runtime_contract": false,
      "path": "/Users/steven/.mtplx/models/Youssofal--Gemma4-MTPLX-Optimized-Speed",
      "repo_id": "Youssofal/Gemma4-MTPLX-Optimized-Speed",
      "validation": {
        "contract_present": false,
        "missing_files": ["config.json", "tokenizer.json", "model.safetensors.index.json", "mtplx_runtime.json", "mtp sidecar"],
        "ok": false
      }
    },
    {
      "delete_command": "mtplx remove Youssofal/Qwen3.5-4B-MTPLX-Optimized-Speed",
      "has_runtime_contract": true,
      "path": "/Users/steven/.mtplx/models/Youssofal--Qwen3.5-4B-MTPLX-Optimized-Speed",
      "recommended_profile": "sustained",
      "repo_id": "Youssofal/Qwen3.5-4B-MTPLX-Optimized-Speed",
      "validation": {"contract_present": true, "missing_files": [], "ok": true}
    },
    {
      "delete_command": "mtplx remove Youssofal/Qwen3.6-35B-A3B-MTPLX-Optimized-Speed-FP16",
      "has_runtime_contract": true,
      "path": "/Users/steven/.mtplx/models/Youssofal--Qwen3.6-35B-A3B-MTPLX-Optimized-Speed-FP16",
      "recommended_profile": "sustained",
      "repo_id": "Youssofal/Qwen3.6-35B-A3B-MTPLX-Optimized-Speed-FP16",
      "validation": {"contract_present": true, "missing_files": [], "ok": true}
    }
  ]
}`

// residencyInventoryPath is where the fixture's inventory read points. It is not
// a path MTPLX serves: the shipped manifest reads the cache with
// `mtplx models --json`, and a test cannot run that, so the same body arrives
// through an HTTP action. What is under test is the join, which does not care how
// the body arrived, only that it arrived whole.
const residencyInventoryPath = "/fixture/models"

// residencyHealthPath and residencyServedPath are where the fixture answers the
// two questions about a running server: which directory it mapped, and what it
// answers to. Both are the paths the shipped manifest names.
const (
	residencyHealthPath = "/health"
	residencyServedPath = "/v1/models"
)

// residencyHealthBody is one MTPLX's /health answer, trimmed to the keys the join
// reads. The measured answer runs to 51,354 bytes, and what is missing here is
// machine telemetry — per-kernel ledgers, thermal history, environment reports —
// that PAIR never reads. Of the three keys, `ok` is how MTPLX says it is itself,
// `model` is the name it answers requests as, and `model_path` is the directory it
// loaded the pack from. Only the last joins to anything, and it joins because it is
// the same string the cache listing carries as a row path: one directory, two
// answers, and a served name that matches neither spelling.
//
// The measured size belongs to mtplxHealthBody in identity_test.go, which these
// cases do not need: what is under test here is which key the join reads, and the
// cases that turn on it differ only in the directory named.
func residencyHealthBody(modelPath string) string {
	return `{"ok":true,"model":"` + mtplxServedModel + `","model_path":"` + modelPath + `"}`
}

// residencyOversizedHealthBody is that same answer wearing a filler field large
// enough that PAIR cannot read it whole. The action machinery lets an 8 MiB HTTP
// body through, so a two-megabyte answer arrives intact at the reader's door: the
// only thing standing between it and a half-parsed pack identity is the cap the
// probe read puts on a body.
func residencyOversizedHealthBody(t *testing.T) string {
	t.Helper()

	pad := strings.Repeat("f", 2*maxProbeJSONBytes)
	body := strings.TrimSuffix(residencyHealthBody(mtplxServedPath), "}") + `,"filler":"` + pad + `"}`
	require.Greater(t, len(body), 2*maxProbeJSONBytes,
		"the case has to be a body larger than PAIR reads, or it proves nothing about the cap")
	return body
}

// residencyServedBody is the same server's answer to its loaded_models read: the
// served name in the array and field the manifest declares. The name is not a pack,
// not a row key and not a path, which is the whole reason residency is declared
// rather than guessed from the answer this body carries.
const residencyServedBody = `{"data":[{"id":"` + mtplxServedModel + `"}]}`

// residencyForeignBody is what a web app with history-api fallback answers every
// path with, /health included: its own bundle. The identity probe already refuses to
// call such a listener this engine; the join has to refuse to read a loaded path out
// of it as well, rather than finding `model_path` wherever a parser might.
const residencyForeignBody = `<!doctype html><html><head><title>MTPLX</title></head><body><div id="root"></div></body></html>`

// residencyPairBundlePath is a directory the server really has mapped and really is
// serving, sitting under a root the cache listing never walks — where a pair bundle
// handed straight to the server lands — and its last element is the folder name of
// the pack the listing does hold. Those two together are why the case is worth
// having: a join that fell back on the served name, on a path prefix, or on "there is
// only one row" answers it with the wrong pack, and every other case here would still
// pass.
const residencyPairBundlePath = "/Users/Shared/mtplx/pairs/Youssofal--Qwen3.8-Flash-Next-MTPLX-Optimized-Speed"

// residencyStubTransport answers the fixture's reads. Anything it was not told
// about is an error rather than a real request, because the machine running these
// tests may have a live MTPLX on some port, and no test may ask it a question and
// then take the answer as a fact about the fixture.
func residencyStubTransport(bodies map[string]string) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			return nil, fmt.Errorf("residency fixture got a request for %s, which it did not declare", r.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})
}

// residencyAsking builds an engine that is running on a port with nothing behind
// it and answers exactly the reads named in bodies. An empty map is how "the
// engine answered nothing" is staged.
func residencyAsking(t *testing.T, manifest *Manifest, bodies map[string]string) *Executor {
	t.Helper()

	ex := newTestExecutor(t, manifest)
	residencyMarkRunning(t, ex, manifest.Engine)
	ex.client.Transport = residencyStubTransport(bodies)
	return ex
}

// residencyMarkRunning says the engine is up. It is separate from residencyAsking
// because two cases set the transport themselves — one because it has to record the
// questions, one because the whole point is that the engine is *not* running — and a
// residency answer that quietly defaulted to "stopped" would make every unknown in
// this file pass for the wrong reason.
func residencyMarkRunning(t *testing.T, ex *Executor, engine string) {
	t.Helper()

	st, err := ex.state(engine)
	require.NoError(t, err)
	st.mu.Lock()
	st.running = true
	st.mu.Unlock()
}

// residencyAskedAbout is residencyAsking plus the record of every question that was
// really asked, for the cases whose answer has to be reached without asking: an
// engine that cannot be asked is answered by not asking it.
func residencyAskedAbout(t *testing.T, manifest *Manifest, bodies map[string]string, asked *[]residencyAsk) *Executor {
	t.Helper()

	ex := residencyAsking(t, manifest, bodies)
	ex.client.Transport = residencyRecordingTransport(bodies, asked)
	return ex
}

// residencyAsk is one question PAIR really put to an engine: the host:port and path
// it asked, how long it gave the answer, and whether the identity-probe marker went
// with it. All three are needed, because each is a mistake that still produces an
// answer that looks right. The target shows a server met on a port PAIR did not
// choose being asked there rather than at the port the manifest would have preferred.
// The marker shows the question went as a probe, which is how PAIR's own facade knows
// to answer for itself instead of forwarding the read to whatever it fronts. And the
// bound is the only part of "this read cannot hang a status refresh" that a stubbed
// answer cannot show on its own: a stub answers instantly, so the request has to
// carry the deadline.
type residencyAsk struct {
	target string
	probe  string
	bound  time.Duration
}

func (a residencyAsk) String() string {
	return fmt.Sprintf("%s probe=%q bound=%s", a.target, a.probe, a.bound)
}

func residencyRecordingTransport(bodies map[string]string, asked *[]residencyAsk) http.RoundTripper {
	inner := residencyStubTransport(bodies)
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		ask := residencyAsk{
			target: r.URL.Host + r.URL.Path,
			probe:  r.Header.Get(engineIdentityProbeHeader),
		}
		if deadline, ok := r.Context().Deadline(); ok {
			ask.bound = time.Until(deadline)
		}
		*asked = append(*asked, ask)
		return inner.RoundTrip(r)
	})
}

// residencyManifest mirrors what manifests/mtplx.json declares about a MTPLX,
// with the two paths the fixture answers. The real manifest reads the cache with
// a CLI action; a hand-built manifest says "an HTTP GET" instead so the test runs
// on a host with no MTPLX, and its port is one nothing is listening on because
// the fixture answers without dialling.
func residencyManifest(t *testing.T) *Manifest {
	t.Helper()

	return &Manifest{
		Engine:           "mtplx",
		DisplayName:      "MTPLX (fixture)",
		ManifestVersion:  ManifestSchemaVersion,
		AdvertisedModels: advertisedModelsLoaded,
		Platforms: map[string]Platform{
			hostPlatformKey(): {
				Detect: []string{os.DevNull},
				Runtime: Runtime{
					// A path need not exist to be a legal manifest, and nothing here
					// launches it. It is set because Manifest.Validate checks the
					// platform before the actions, and a fixture that stops there
					// would never reach the residency rules it is meant to be
					// testing.
					Bin:  filepath.Join(t.TempDir(), "mtplx"+exeExt()),
					Port: portRun(t, 1)[0],
				},
			},
		},
		Actions: map[string]Action{
			"list_models": {
				HTTP:   &ActionHTTP{Method: "GET", Path: residencyInventoryPath},
				Result: &ActionResult{Array: "models", Field: "repo_id"},
			},
			"loaded_models": {
				HTTP: &ActionHTTP{Method: "GET", Path: residencyServedPath},
				Result: &ActionResult{
					Array: "data",
					Field: "id",
					Residency: &ActionResidency{
						Source:       "list_models",
						Read:         &ActionHTTP{Method: "GET", Path: residencyHealthPath},
						PathField:    "model_path",
						RowPathField: "path",
					},
				},
			},
		},
	}
}

// residencyManifestWithRestart is the fixture with the runtime keys that
// restart_after and launch_model each demand on their own: a readiness probe, and
// editable launch settings. The mutating-source test needs them. Without them
// Manifest.Validate fails on the platform first, and a case would pass without ever
// reaching the residency rule it is there to pin down.
func residencyManifestWithRestart(t *testing.T) *Manifest {
	t.Helper()

	manifest := residencyManifest(t)
	platform := manifest.Platforms[hostPlatformKey()]
	platform.Runtime.Ready = &Probe{HTTP: "http://127.0.0.1:{port}/health", Status: 200}
	platform.Runtime.EditableLaunch = &EditableLaunch{
		FixedArgs: []string{"serve"},
		Controls: []LaunchControl{
			{Flags: []string{"--host"}, Value: "{server.host}"},
			{Flags: []string{"--port"}, Value: "{server.port}"},
		},
	}
	manifest.Platforms[hostPlatformKey()] = platform
	return manifest
}

func TestPackInventoryFrom_ReadsRowsFromTheMeasuredModelsListing(t *testing.T) {
	ex := residencyAsking(t, residencyManifest(t), map[string]string{residencyInventoryPath: sagittariusModelsBody})

	rows, ok := ex.packInventoryFrom(t.Context(), "mtplx", inventoryAnswer{})

	require.True(t, ok, "the listing arrived whole, so the inventory is known")
	require.Len(t, rows, 1, "that capture holds one pack")
	assert.Equal(t, "Youssofal/Qwen3.8-Flash-Next-MTPLX-Optimized-Speed", rows[0].repoID, "repo id")
	assert.Equal(t, "/Users/Shared/mtplx/models/Youssofal--Qwen3.8-Flash-Next-MTPLX-Optimized-Speed", rows[0].path, "pack directory")

	const servedPath = "/Users/Shared/mtplx/models/Youssofal--Qwen3.8-Flash-Next-MTPLX-Optimized-Speed"
	row, found := packRowForKey(rows, servedPath)
	require.True(t, found, "the path this MTPLX answered with its /health must join to its own row")
	assert.Equal(t, rows[0].repoID, row.repoID, "a join that lands on the wrong row is worse than no join")

	// The same engine spells one pack three ways: /health says it serves
	// "mtplx-flash-next-optimized-speed", the inventory names the pack
	// "Youssofal/Qwen3.8-Flash-Next-MTPLX-Optimized-Speed", and the directory
	// holds a third spelling. Only the second is a row key and only the third is
	// a path, so the other two have to find nothing here.
	_, found = packRowForKey(rows, "mtplx-flash-next-optimized-speed")
	assert.False(t, found, "a served name is not a pack path")

	// Case only matters because the filesystem under it ignores it: a join that
	// folded case would find this pack on a Mac and then look in the wrong place,
	// which is how a pack that is merely cached starts being reported as the one
	// being served.
	_, found = packRowForKey(rows, strings.ToUpper(servedPath))
	assert.False(t, found, "the join is exact, so an upper-cased spelling finds nothing")
}

func TestPackInventoryFrom_KeepsTwoRootsApartAndNeverGuessesASymlink(t *testing.T) {
	ex := residencyAsking(t, residencyManifest(t), map[string]string{residencyInventoryPath: bellatrixModelsBody})

	rows, ok := ex.packInventoryFrom(t.Context(), "mtplx", inventoryAnswer{})

	require.True(t, ok, "the listing arrived whole, so the inventory is known")
	require.Len(t, rows, 3, "that machine caches three packs")
	require.Equal(t, "/Users/steven/.mtplx/models/Youssofal--Gemma4-MTPLX-Optimized-Speed", rows[0].path, "Gemma4")
	assert.Equal(t, "Youssofal/Gemma4-MTPLX-Optimized-Speed", rows[0].repoID, "Gemma4")

	// Gemma4 is in the listing with no runtime contract, no config.json, no
	// tokenizer.json, no weights index and no MTP sidecar, so loading it fails.
	// It is still a row: residency answers where a pack sits, and a cache entry
	// that fails its own check is a fact about the disk, not a gap in the
	// inventory.
	row, found := packRowForKey(rows, "/Users/steven/.mtplx/models/Youssofal--Gemma4-MTPLX-Optimized-Speed")
	require.True(t, found, "an unloadable pack is still cached, so it is still a row")
	assert.Equal(t, "Youssofal/Gemma4-MTPLX-Optimized-Speed", row.repoID)

	const servedQwen35 = "/Users/steven/.mtplx/models/Youssofal--Qwen3.5-4B-MTPLX-Optimized-Speed"
	_, found = packRowForKey(rows, servedQwen35)
	require.True(t, found, "this is the pack that machine was serving")

	// Two roots, kept apart. The machine that caches these packs keeps everything
	// under the first one and reports the second as holding nothing, so both
	// prefixes look plausible on that host. A join that trimmed the root, or tried
	// "the other root" when a path missed, would answer a query about a directory
	// holding no pack with the pack sitting in the one that does.
	for _, other := range []string{
		"/Users/Shared/mtplx/models/Youssofal--Qwen3.5-4B-MTPLX-Optimized-Speed",
		"/Users/Shared/mtplx/models/Youssofal--Gemma4-MTPLX-Optimized-Speed",
	} {
		_, found := packRowForKey(rows, other)
		assert.False(t, found, "a query under the other root must not land on a pack in this one (%s)", other)
	}

	// macOS answers /var and /tmp under /private as well, so a path can reach
	// PAIR spelled either way. Nothing here resolves that: a query with the
	// prefix is a different string from the row, and guessing that the two name
	// the same file is how one pack gets credited for another.
	_, found = packRowForKey(rows, "/private"+servedQwen35)
	assert.False(t, found, "a leading /private is not trimmed")

	_, found = packRowForKey(rows, servedQwen35+"/")
	assert.False(t, found, "a trailing separator is not trimmed either")
}

func TestPackInventoryFrom_TreatsAnEmptyListingAsKnownAndABlockedOneAsUnknown(t *testing.T) {
	manifest := residencyManifest(t)

	t.Run("an empty cache is an answer", func(t *testing.T) {
		ex := residencyAsking(t, manifest, map[string]string{residencyInventoryPath: `{"models":[]}`})

		rows, ok := ex.packInventoryFrom(t.Context(), "mtplx", inventoryAnswer{})

		require.True(t, ok, "a listing that says nothing cached was still read")
		assert.Empty(t, rows, "nothing cached has to stay distinguishable from nothing read")
	})

	t.Run("a listing with no usable row is not an empty cache", func(t *testing.T) {
		// A non-empty `models` array whose rows name nothing is a listing PAIR
		// cannot read, not a cache full of packs that went missing.
		ex := residencyAsking(t, manifest, map[string]string{residencyInventoryPath: `{"models":[{"foo":1},{"bar":2}]}`})

		rows, ok := ex.packInventoryFrom(t.Context(), "mtplx", inventoryAnswer{})

		assert.False(t, ok, "a malformed inventory must not become authoritative empty")
		assert.Nil(t, rows)
	})

	t.Run("no models array at all was not read", func(t *testing.T) {
		for _, body := range []string{`{}`, `{"models":null}`, `{"models":"Youssofal/Gemma4-MTPLX-Optimized-Speed"}`} {
			ex := residencyAsking(t, manifest, map[string]string{residencyInventoryPath: body})

			rows, ok := ex.packInventoryFrom(t.Context(), "mtplx", inventoryAnswer{})

			assert.False(t, ok, "a body that carries no inventory is unknown (%s)", body)
			assert.Nil(t, rows)
		}
	})

	t.Run("a body larger than PAIR reads is not a partial list", func(t *testing.T) {
		pad := strings.Repeat("f", 4096)
		body := `{"models":[` + strings.Repeat(`{"repo_id":"org/pad","path":"/var/folders/pad","filler":"`+pad+`"},`, 300) + `{"repo_id":"Youssofal/Gemma4-MTPLX-Optimized-Speed","path":"/Users/steven/.mtplx/models/Youssofal--Gemma4-MTPLX-Optimized-Speed"}]}`
		require.Greater(t, len(body), maxProbeJSONBytes, "the fixture has to be past the cap to test it")

		ex := residencyAsking(t, manifest, map[string]string{residencyInventoryPath: body})

		rows, ok := ex.packInventoryFrom(t.Context(), "mtplx", inventoryAnswer{})

		assert.False(t, ok, "a truncated listing must not report the packs it happened to hold")
		assert.Nil(t, rows)
	})

	t.Run("an engine that answers nothing was not read", func(t *testing.T) {
		// The stub errors on any path it was not told about, which is how
		// "the read did not happen" is staged.
		ex := residencyAsking(t, manifest, map[string]string{})

		rows, ok := ex.packInventoryFrom(t.Context(), "mtplx", inventoryAnswer{})

		assert.False(t, ok, "no answer is not an empty cache")
		assert.Nil(t, rows)
	})

	t.Run("a fixture whose inventory read is refused while the engine is stopped", func(t *testing.T) {
		// No running engine means the read is never attempted here: the shared
		// dispatcher refuses an HTTP action while the engine is down, and this
		// fixture carries its inventory read as an HTTP action. That is the fixture
		// talking, not MTPLX: the shipped manifest reads the cache with
		// `mtplx models --json`, a cmd action the dispatcher deliberately does not
		// gate on a running engine, so a stopped MTPLX is expected to answer and a
		// stopped fixture is not. What both agree on is the answer PAIR gives when
		// the read does not happen: unknown, not empty.
		ex := newTestExecutor(t, manifest)

		rows, ok := ex.packInventoryFrom(t.Context(), "mtplx", inventoryAnswer{})

		assert.False(t, ok, "a refused read cannot have produced rows")
		assert.Nil(t, rows)
	})

	t.Run("an engine that declares no join has no rows", func(t *testing.T) {
		withoutResidency := residencyManifest(t)
		loaded := withoutResidency.Actions["loaded_models"]
		loaded.Result.Residency = nil
		withoutResidency.Actions["loaded_models"] = loaded
		ex := residencyAsking(t, withoutResidency, map[string]string{residencyInventoryPath: sagittariusModelsBody})

		rows, ok := ex.packInventoryFrom(t.Context(), "mtplx", inventoryAnswer{})

		assert.False(t, ok, "no declared join means nothing was read, not that nothing is cached")
		assert.Nil(t, rows)
	})
}

func TestResidentJoin_JoinsHealthModelPathWithTheCacheRow(t *testing.T) {
	// One running server, three questions: what does your cache hold, which
	// directory did you map, and what do you answer requests as. The join is the
	// first two meeting, and the third must not be folded in with them: the row is
	// the spelling a cached pack's eject or delete can act on, and the served name
	// is what a request still has to be routed by.
	ex := residencyAsking(t, residencyManifest(t), map[string]string{
		residencyInventoryPath: sagittariusModelsBody,
		residencyHealthPath:    residencyHealthBody(mtplxServedPath),
		residencyServedPath:    residencyServedBody,
	})

	answer := ex.residentJoin(t.Context(), "mtplx", inventoryAnswer{})

	require.True(t, answer.named, "the directory this server mapped is a pack the cache lists, so the resident pack is known")
	assert.Equal(t, "Youssofal/Qwen3.8-Flash-Next-MTPLX-Optimized-Speed", answer.row.repoID,
		"the answer is the cache's own spelling of the pack, not the name the server answers to")
	assert.Equal(t, mtplxServedPath, answer.row.path,
		"the row comes back whole: a caller that has to show or remove the directory needs it")

	names, namesOK := ex.servedNames(t.Context(), "mtplx")

	require.True(t, namesOK, "the same server also answers what it answers to")
	assert.Equal(t, []string{"mtplx-flash-next-optimized-speed"}, names,
		"the wire name survives the join; reporting only the repo id would leave PAIR with a name no request uses")
	require.NotEmpty(t, answer.row.repoID)
	assert.NotEqual(t, answer.row.repoID, names[0],
		"had the two come back the same, one of them would have been guessed from the other rather than read")
}

func TestResidentJoin_AnswersEmptyForFourReasonsAndKeepsEvidenceApart(t *testing.T) {
	// No row comes back for four quite different reasons: nothing to ask, an answer
	// too big to have read, an answer that is not the engine, and an answer that
	// names a pack no row holds. They agree on one thing, which is what these
	// subtests have always pinned — none of them may mark a row loaded. For an
	// engine that serves exactly one pack, the first wrong row is not a miss but a
	// confident claim about a pack that is merely cached, and that is the mistake
	// residency exists to prevent.
	//
	// They are not alike in what they rule out, and that is what complete is for. An
	// eject weighing whether to refuse has to know whether an empty answer is
	// evidence or the absence of any, so each case below says which it is, and the
	// last is the one that is evidence: both halves answered, and the directory the
	// engine named is one no row holds. That distinction is more than a bare `false`
	// could carry, which is why these cases read the join rather than a name.
	manifest := residencyManifest(t)

	t.Run("a stopped engine is answered without asking it anything", func(t *testing.T) {
		// The fixture answers both reads, so a `false` here cannot come from
		// nothing being there to find. It has to come from PAIR knowing a stopped
		// engine answers neither, and asking nothing rather than collecting a
		// refused dial as though it were an answer.
		var asked []residencyAsk
		ex := newTestExecutor(t, manifest)
		ex.client.Transport = residencyRecordingTransport(map[string]string{
			residencyInventoryPath: sagittariusModelsBody,
			residencyHealthPath:    residencyHealthBody(mtplxServedPath),
		}, &asked)

		answer := ex.residentJoin(t.Context(), "mtplx", inventoryAnswer{})

		assert.False(t, answer.named, "nothing is running, so which pack it would have mapped is unknown")
		assert.Equal(t, packRow{}, answer.row, "unknown must not arrive as an empty row that some caller reads as nothing resident")
		assert.False(t, answer.complete, "nothing was asked, so this emptiness rules nothing out")
		assert.Empty(t, asked, "a stopped engine is not asked")
	})

	t.Run("a health answer bigger than PAIR reads was not read", func(t *testing.T) {
		// The action machinery lets an 8 MiB HTTP body through, so a two-megabyte
		// /health answer arrives whole at the reader's door. Reading the first
		// megabyte of it and parsing what happened to land is how a pack that is
		// only partly described starts being reported as the resident one, and
		// there is nothing to crash on either: the answer is simply unknown.
		ex := residencyAsking(t, manifest, map[string]string{
			residencyInventoryPath: sagittariusModelsBody,
			residencyHealthPath:    residencyOversizedHealthBody(t),
		})

		answer := ex.residentJoin(t.Context(), "mtplx", inventoryAnswer{})

		assert.False(t, answer.named, "a truncated health answer identifies no pack")
		assert.Equal(t, packRow{}, answer.row, "no row may be marked loaded from a body that was never read")
		assert.False(t, answer.complete, "the half that names the directory never answered, so what is loaded stays open")

		rows, inventoryOK := ex.packInventoryFrom(t.Context(), "mtplx", inventoryAnswer{})
		require.True(t, inventoryOK, "the cache listing still arrived, so the unknown answer is the health read's own")
		assert.Len(t, rows, 1, "and it is not the unknown that emptied it")
	})

	t.Run("a health answer that is not a JSON object was not read", func(t *testing.T) {
		// `residencyForeignBody` is a web app with history-api fallback answering
		// every path, /health included, with its own bundle. The identity probe
		// already refuses to call such a listener this engine; the join has to
		// refuse to read a loaded path out of it as well, rather than finding
		// `model_path` wherever a parser might.
		ex := residencyAsking(t, manifest, map[string]string{
			residencyInventoryPath: sagittariusModelsBody,
			residencyHealthPath:    residencyForeignBody,
		})

		answer := ex.residentJoin(t.Context(), "mtplx", inventoryAnswer{})

		assert.False(t, answer.named, "an HTML body is not an engine's answer about its model")
		assert.Equal(t, packRow{}, answer.row, "no row may be marked loaded from a body that is not the engine's")
		assert.False(t, answer.complete, "an answer from something that is not this engine leaves what is loaded open")
	})

	t.Run("a loaded path under no cache root matches no row", func(t *testing.T) {
		// `residencyPairBundlePath` is a directory the server really has mapped and
		// really is serving, sitting under a root the listing never walks — a pair
		// bundle handed straight to the server — and its last element is the cached
		// pack's own folder name. That combination is why this is the case no
		// shortcut survives: a join that matched on the served name, on a path
		// prefix, or on "there is only one row" answers here with the pack this
		// machine caches, shown as loaded, and an eject or a delete aimed at it goes
		// after the wrong directory. Unknown costs the user a loaded marker; a guess
		// costs them their other pack.
		ex := residencyAsking(t, manifest, map[string]string{
			residencyInventoryPath: sagittariusModelsBody,
			residencyHealthPath:    residencyHealthBody(residencyPairBundlePath),
		})

		answer := ex.residentJoin(t.Context(), "mtplx", inventoryAnswer{})

		assert.False(t, answer.named, "a path no row holds has no row to return")
		assert.Equal(t, packRow{}, answer.row, "the pack this machine does cache must not stand in for the one it is serving")
		// Where complete earns its keep. Every case above answered this same empty
		// pair, and this one is the opposite of all of them: the engine said where it
		// loaded and the listing said what it holds, so the emptiness is a fact about
		// the pack rather than a failure to look. An eject guard may refuse on this one
		// and may not refuse on any of the others, and no bare `false` said which was
		// which.
		assert.True(t, answer.complete, "both halves answered, so this emptiness is evidence")

		rows, inventoryOK := ex.packInventoryFrom(t.Context(), "mtplx", inventoryAnswer{})
		require.True(t, inventoryOK, "the listing was read, so the miss is the path's and not a failed read")
		require.Len(t, rows, 1, "the one pack it holds is the pack this answer is not about")
	})
}

func TestResidentJoin_JoinsNoRowWhenOnlyATidiedPathWouldMatch(t *testing.T) {
	// The join is exact string equality, and this pins it where the engine's answer
	// arrives rather than only at the comparison. Every other path in these fixtures
	// is already clean and single-slashed, so an extraction that tidied separators on
	// its way to the comparison — `filepath.Clean`, or a trailing separator trimmed —
	// would leave the whole suite passing while turning a near-miss into a join. A
	// near-miss join is not a harmless loosening: the row it finds is the directory a
	// later eject or delete acts on, and the pack a model list marks loaded. Folding
	// the case is caught from the other side, by the join that has to succeed on the
	// measured mixed-case path; what these two cases pin is that no separator is
	// tidied away.
	//
	// Each case is staged so the tidied spelling really does find the row, because a
	// reported path that missed under a normalising join as well would answer `false`
	// for the wrong reason and pin nothing.
	cases := []struct {
		name string
		// reported is the directory the running server answers with: the cached
		// pack's own path, written with one piece of separator redundancy.
		reported string
		// tidied is what taking that redundancy back off leaves, which is exactly the
		// row the cache holds. It is written out per case so the bite of each one is
		// visible without reasoning about what a path routine does.
		tidied string
	}{
		{
			name:     "a reported path with one more trailing separator than the cached row",
			reported: mtplxServedPath + "/",
			tidied:   mtplxServedPath,
		},
		{
			name:     "a reported path with a doubled separator inside the cached row",
			reported: strings.Replace(mtplxServedPath, "/models/", "/models//", 1),
			tidied:   mtplxServedPath,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NotEqual(t, tc.tidied, tc.reported, "the case stages a spelling the cache holds no row for")

			ex := residencyAsking(t, residencyManifest(t), map[string]string{
				residencyInventoryPath: sagittariusModelsBody,
				residencyHealthPath:    residencyHealthBody(tc.reported),
			})

			answer := ex.residentJoin(t.Context(), "mtplx", inventoryAnswer{})

			assert.False(t, answer.named, "the path this server named is not any row's path, so no pack is known")
			assert.Equal(t, packRow{}, answer.row, "a path that joins only after tidying must not mark the row it tidies to as loaded")
			// The join is exact, and it is not silent about having read: both halves
			// answered, so this is an emptiness an eject refuses on rather than one it
			// has to stop for. "Unknown" is the wrong word for it, and the one bool
			// these cases had before complete could not tell the two apart.
			assert.True(t, answer.complete, "the engine answered, the listing answered, and the miss between them is the finding")

			rows, inventoryOK := ex.packInventoryFrom(t.Context(), "mtplx", inventoryAnswer{})
			require.True(t, inventoryOK, "the listing arrived, so the miss belongs to the reported path")
			require.Len(t, rows, 1, "the cache holds exactly the pack this answer is one separator away from")
			require.Equal(t, tc.tidied, rows[0].path, "separator redundancy is the only difference between the two spellings")

			_, foundByTidying := packRowForKey(rows, tc.tidied)
			require.True(t, foundByTidying,
				"with that redundancy removed the same rows do hold this pack, so the answer above is exactness and not an unrelated miss")
		})
	}
}

func TestResidentJoin_WithoutADeclaredReadAnswersNothingWasAsked(t *testing.T) {
	// A residency block that names no read has not said where the loaded path is
	// answered, so there is nothing to ask and no guessing where to ask it anyway. The
	// fixture still answers the inventory, so an empty answer here cannot be blamed on a
	// listing that failed to arrive: it comes from having no declared question to ask.
	// Complete is the word for that — there is nothing this engine could still tell PAIR
	// about what it has mapped — and it is still not the answer "this engine has nothing
	// loaded", which is the reading a caller has to be kept off either way. It is what
	// lets a guard refuse on this emptiness when it cannot refuse on the emptiness of a
	// read that never came: see residencyAnswer.
	manifest := residencyManifest(t)
	loaded := manifest.Actions["loaded_models"]
	loaded.Result.Residency.Read = nil
	manifest.Actions["loaded_models"] = loaded
	var asked []residencyAsk
	ex := residencyAskedAbout(t, manifest, map[string]string{
		residencyInventoryPath: sagittariusModelsBody,
		residencyHealthPath:    residencyHealthBody(mtplxServedPath),
	}, &asked)

	answer := ex.residentJoin(t.Context(), "mtplx", inventoryAnswer{})

	assert.False(t, answer.named, "a manifest that names no read cannot be asked what it loaded")
	assert.Equal(t, packRow{}, answer.row, "and not being able to ask is not the same as nothing being resident")
	assert.True(t, answer.complete, "there is no read left to make, so this emptiness is all there is to know")
	assert.Empty(t, asked, "so the engine is not asked")
}

func TestResidentJoin_ReadsAnExternallyStartedServerOnPort6666(t *testing.T) {
	// Review Focus 1: a MTPLX PAIR never started, on a port PAIR never chose. The
	// name keeps the measurement — this is the server found on port 6666 on the Mac
	// these fixtures came from, kept in a settings file rather than in PAIR. The
	// staged port is one the OS assigned, because 6666 on the machine running this
	// test is somebody's live session and no test may bind it, read it or disturb
	// it. The health answer is at the measured size all the same: a join proven
	// against a 30-byte stub has not been proven against a real MTPLX.
	// Nothing here reaches this user's MTPLX. The tool the manifest's {cli} resolves
	// to is the fake engine rather than any path the manifest names, the binary is a
	// name in a directory nothing is in, and the settings-file port source is dropped
	// — that file is this machine's real
	// `~/Library/Application Support/MTPLX/settings.json`, and reading it would both
	// make the test depend on it and point the read at a live server. So the only
	// port in the test is the one the OS handed over. relocationRegistry pins the same
	// things for the same reason.
	reg := NewRegistry()
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"))
	manifest, platform := hostPlatformBlock(t, reg, "mtplx")

	port := portRun(t, 1)[0]

	// The shipped inventory read is `mtplx models --json`, and no test may run a real
	// MTPLX, so the fake engine's CLI answers the measured body from a file: {cli} and
	// a listing-answering subcommand, in place of the shipped subcommand and nothing
	// else. This is also the only place the cmd half of the join is driven end to end:
	// Task 1 pinned the cmd shapes at the extractor because no binary could be staged
	// there.
	dir := t.TempDir()
	listing := filepath.Join(dir, "models.json")
	require.NoError(t, os.WriteFile(listing, []byte(sagittariusModelsBody), 0o600))
	listModels := manifest.Actions["list_models"]
	listModels.Cmd = []string{"{cli}", "listfrom", listing}
	manifest.Actions["list_models"] = listModels

	// {cli} follows the tool the platform is observed to have, and nothing is observed
	// here: with no detect path, the manifest's own literal is the only candidate, so
	// the literal is the fake engine. Staging a detect path instead would leave this
	// action running a tool other than the one it wrote.
	platform.Detect = nil
	platform.Runtime.CLI = fakeEngineBin
	platform.Runtime.Bin = filepath.Join(dir, "mtplx-started-by-nobody"+exeExt())
	platform.Runtime.PortSource = nil
	platform.Runtime.Port = port
	manifest.Platforms[hostPlatformKey()] = platform

	ex := newTestExecutor(t, manifest)
	st, err := ex.state("mtplx")
	require.NoError(t, err)
	st.mu.Lock()
	st.running = true
	st.mu.Unlock()
	require.Equal(t, port, st.port,
		"the engine is met on the port it was found on: not the 8101 the manifest configures, and not the 6666 that Mac's settings file named")

	var asked []residencyAsk
	ex.client.Transport = residencyRecordingTransport(map[string]string{
		residencyHealthPath: mtplxHealthBody(t, mtplxServedModel, mtplxServedPath),
		residencyServedPath: residencyServedBody,
	}, &asked)

	answer := ex.residentJoin(t.Context(), "mtplx", inventoryAnswer{})

	require.True(t, answer.named, "the pack a server PAIR did not start has mapped has to be answerable, or the card says nothing true about it")
	assert.Equal(t, "Youssofal/Qwen3.8-Flash-Next-MTPLX-Optimized-Speed", answer.row.repoID,
		"a relocated server's answer joins to the same row a locally started one would")
	require.Len(t, asked, 1, "one question per call, and of the server that is running")
	assert.Equal(t, fmt.Sprintf("127.0.0.1:%d%s", port, residencyHealthPath), asked[0].target,
		"the read went to the port this server was met on, not the 6666 the manifest's settings file prefers")
	assert.Equal(t, "1", asked[0].probe,
		"and it went as a probe, so PAIR's own facade answers for itself instead of forwarding the question to whatever it fronts")

	names, namesOK := ex.servedNames(t.Context(), "mtplx")

	require.True(t, namesOK, "an adopted server answers what it answers to just the same")
	assert.Equal(t, []string{mtplxServedModel}, names)

	// The one question this join puts to the server is bounded the way every probe
	// answer is bounded, which is the rule that keeps a status refresh from waiting on
	// a wedged engine. A stubbed answer arrives instantly, so the request carrying the
	// deadline is the only evidence there is — and a bound widened to the action
	// timeout, which is what the ordinary model-list read uses, would otherwise look
	// like the same passing test.
	assert.LessOrEqual(t, asked[0].bound, 3*time.Second,
		"the residency read is bounded by the probe's bound, not the 30-minute action timeout a model read inherits")
	assert.Greater(t, asked[0].bound, 2*time.Second,
		"and it is that bound being applied, not an already-expired context answering for it")
}

// mtplxPackRepoID is what the cache listing calls the one pack the Mac these
// fixtures came from holds. It is the key every consumer of the model payload
// matches rows by, because that listing's field is what the model manager lists,
// and the loaded list is unioned onto those rows and looked up by exact string.
// The pack has three other spellings — the folder it sits in, which writes the
// separator twice; the name the running server answers requests as; and, for a
// pair bundle, a directory no listing names — and only this one joins.
const mtplxPackRepoID = "Youssofal/Qwen3.8-Flash-Next-MTPLX-Optimized-Speed"

// asksForPath is the questions PAIR really put to one path. A case counts the reads
// of one action this way rather than assuming nothing else in the sweep asked
// anything, which is what makes "this action ran once" a fact about that action.
func asksForPath(asked []residencyAsk, path string) []residencyAsk {
	var got []residencyAsk
	for _, ask := range asked {
		if strings.HasSuffix(ask.target, path) {
			got = append(got, ask)
		}
	}
	return got
}

func TestLoadedModels_ForMTPLXMarksTheCacheRowNotTheServedName(t *testing.T) {
	// A running MTPLX, the cache listing it gives for itself, and the directory it
	// mapped: the model list has to report which pack is resident in the spelling its
	// own rows already carry. It is not a cosmetic choice. The model manager unions
	// this loaded list onto the cached rows by exact string and marks a row loaded by
	// looking a row name up in it, so a resident pack named any other way shows as two
	// rows — the cached one idle, the served name loaded — and the cached row's
	// Eject, Delete and Load buttons are aimed by that same lookup. A row that is
	// merely cached offering Load restarts an engine that is already serving it.
	ex := residencyAsking(t, residencyManifest(t), map[string]string{
		residencyInventoryPath: sagittariusModelsBody,
		residencyHealthPath:    residencyHealthBody(mtplxServedPath),
		residencyServedPath:    residencyServedBody,
	})

	res := ex.ModelsResult(t.Context())

	require.Equal(t, []string{mtplxServedModel}, res.Models, "the node-level advertised list")
	require.Equal(t, map[string][]string{"mtplx": {mtplxServedModel}}, res.ByEngine,
		"what a request has to be routed by is the served name, and this task leaves it there")
	require.Equal(t, map[string][]string{"mtplx": {mtplxPackRepoID}}, res.LoadedByEngine,
		"residency is the cache's own key for the pack, not a fourth spelling nothing matches")
	require.Equal(t, map[string][]string{"mtplx": {mtplxPackRepoID}}, res.DownloadedByEngine,
		"and it is the key the cached rows already carry, so the union of the two is one row")
	assert.NotContains(t, res.LoadedByEngine["mtplx"], mtplxServedModel,
		"the served name stays out of the loaded list, or this payload names the pack twice")
}

func TestLoadedModels_KeepsTheServedNameAsARowWhenNoPackMatches(t *testing.T) {
	// A pair bundle handed straight to the server is really mapped and really is not
	// in the cache listing, and the join refuses to answer rather than name the cached
	// pack whose folder name it shares. The model list then has to report what it does
	// know — the served name is loaded — instead of nothing at all. This is the
	// deliberate half of a behaviour the desktop pins at
	// tests/modular/lmstudio-stale-model.test.ts: a served thing no cached row names
	// keeps a loaded row of its own. "I cannot tell which cached row this is" is not
	// the statement "nothing is loaded", which is the same mistake as naming the wrong
	// row, travelling the other way.
	ex := residencyAsking(t, residencyManifest(t), map[string]string{
		residencyInventoryPath: sagittariusModelsBody,
		residencyHealthPath:    residencyHealthBody(residencyPairBundlePath),
		residencyServedPath:    residencyServedBody,
	})

	res := ex.ModelsResult(t.Context())

	require.Equal(t, map[string][]string{"mtplx": {mtplxServedModel}}, res.LoadedByEngine,
		"the served name is still reported as loaded, because it is a real served thing")
	require.Equal(t, map[string][]string{"mtplx": {mtplxPackRepoID}}, res.DownloadedByEngine,
		"the pack this machine does cache is still listed, and stays not loaded")
	assert.Equal(t, map[string][]string{"mtplx": {mtplxServedModel}}, res.ByEngine, "advertised")
}

func TestLoadedModels_ReadsThePackInventoryOncePerSweep(t *testing.T) {
	// The join reads the same action the model list reads for its rows, so a sweep
	// that asks the engine for that listing twice pays for it twice. For MTPLX the
	// listing is `mtplx models --json`: a subprocess that carries the action timeout
	// with it, not a request bounded by the probe's 3 s. One sweep, one run.
	var asked []residencyAsk
	ex := residencyAskedAbout(t, residencyManifest(t), map[string]string{
		residencyInventoryPath: sagittariusModelsBody,
		residencyHealthPath:    residencyHealthBody(mtplxServedPath),
		residencyServedPath:    residencyServedBody,
	}, &asked)

	res := ex.ModelsResult(t.Context())

	require.Equal(t, map[string][]string{"mtplx": {mtplxPackRepoID}}, res.LoadedByEngine,
		"the answer has to come from the join, or counting the reads proves nothing about it")
	assert.Len(t, asksForPath(asked, residencyInventoryPath), 1,
		"one inventory read per sweep, whichever half needed it: %v", asked)
	assert.Len(t, asksForPath(asked, residencyHealthPath), 1, "one question to the running server: %v", asked)
	assert.Len(t, asksForPath(asked, residencyServedPath), 1, "one served-name read: %v", asked)
}

func TestLoadedModels_ReadsAResidencySourceActionTheSweepDidNotAlreadyRead(t *testing.T) {
	// A join may name any action for its rows. When it names one the sweep has no
	// other reason to read, reusing the listing the sweep did read would be answering
	// the join from rows it was never declared against, so that action is read — once,
	// and without reading the listing a second time either.
	manifest := residencyManifest(t)
	manifest.Actions["pack_rows"] = Action{
		HTTP:   &ActionHTTP{Method: "GET", Path: "/fixture/pack-rows"},
		Result: &ActionResult{Array: "models", Field: "repo_id"},
	}
	loaded := manifest.Actions["loaded_models"]
	loaded.Result.Residency.Source = "pack_rows"
	manifest.Actions["loaded_models"] = loaded

	var asked []residencyAsk
	ex := residencyAskedAbout(t, manifest, map[string]string{
		residencyInventoryPath: sagittariusModelsBody,
		"/fixture/pack-rows":   sagittariusModelsBody,
		residencyHealthPath:    residencyHealthBody(mtplxServedPath),
		residencyServedPath:    residencyServedBody,
	}, &asked)

	res := ex.ModelsResult(t.Context())

	require.Equal(t, map[string][]string{"mtplx": {mtplxPackRepoID}}, res.LoadedByEngine,
		"the join reads the rows from the action that was declared, not the one that was handy")
	assert.Len(t, asksForPath(asked, "/fixture/pack-rows"), 1, "the declared read, once: %v", asked)
	assert.Len(t, asksForPath(asked, residencyInventoryPath), 1, "and the listing is still read once: %v", asked)
}

func TestManifest_TheBundledMTPLXManifestDeclaresThePackJoin(t *testing.T) {
	raw, err := fs.ReadFile(bundledManifests, "manifests/mtplx.json")
	require.NoError(t, err)
	var mf Manifest
	require.NoError(t, json.Unmarshal(raw, &mf), "the shipped manifest has to parse into the same shape a test builds")
	require.NoError(t, mf.Validate())

	loaded := mf.Actions["loaded_models"]
	require.NotNil(t, loaded.Result, "loaded_models names its array and field before anything else")
	residency := loaded.Result.Residency
	require.NotNil(t, residency, "MTPLX answers for exactly one pack, so the join has to be declared")
	assert.Equal(t, "list_models", residency.Source, "the pack rows come from the cache listing")
	require.NotNil(t, residency.Read)
	assert.Equal(t, "GET", residency.Read.Method, "read method")
	assert.Equal(t, "/health", residency.Read.Path, "that is where MTPLX reports the path it loaded")
	assert.Equal(t, "model_path", residency.PathField, "that is the field it reports it in")
	assert.Equal(t, "path", residency.RowPathField, "that is the field each cached pack carries")

	source := mf.Actions[residency.Source]
	require.NotNil(t, source.Result, "the listing has to name a field before residency can key on it")
	assert.Equal(t, "repo_id", source.Result.Field, "a listing keyed on anything else would key the join on nothing")
	assert.Empty(t, residency.RowKeyField, "the row key is the listing's own field, not a second spelling")
}

func TestManifestValidate_RejectsAResidencyThatCouldNeverResolve(t *testing.T) {
	// Each case is a residency block that would leave PAIR looking for a pack
	// with the wrong recipe: no rows to look in, nothing to look the path up
	// with, or the join declared on an action that never joins anything. They are
	// manifest-load failures, which is why they are rejected before an engine is
	// ever started rather than discovered as a wrong answer later.
	cases := []struct {
		name     string
		resident *ActionResidency
		want     string
	}{
		{
			name:     "names no inventory to read",
			resident: &ActionResidency{Read: &ActionHTTP{Method: "GET", Path: "/health"}, PathField: "model_path", RowPathField: "path"},
			want:     "source",
		},
		{
			name:     "names an inventory that this manifest has no such field in",
			resident: &ActionResidency{Source: "list_models", Read: &ActionHTTP{Method: "GET", Path: "/health"}, PathField: "model_path"},
			want:     "row_path_field",
		},
		{
			name:     "names no field to read the loaded path from",
			resident: &ActionResidency{Source: "list_models", Read: &ActionHTTP{Method: "GET", Path: "/health"}, RowPathField: "path"},
			want:     "path_field",
		},
		{
			name:     "reads its own action for the loaded path",
			resident: &ActionResidency{Source: "loaded_models", Read: &ActionHTTP{Method: "GET", Path: "/v1/models"}, PathField: "id", RowPathField: "path"},
			want:     "source",
		},
		{
			name:     "keys the row on a field name its reader cannot follow",
			resident: &ActionResidency{Source: "list_models", Read: &ActionHTTP{Method: "GET", Path: "/health"}, PathField: "model_path", RowPathField: "path", RowKeyField: "validation.missing_files"},
			want:     "row_key_field",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := residencyManifest(t)
			loaded := manifest.Actions["loaded_models"]
			loaded.Result.Residency = tc.resident
			manifest.Actions["loaded_models"] = loaded

			err := manifest.Validate()

			require.Error(t, err, "a join that cannot resolve has to fail at load")
			assert.Contains(t, err.Error(), "residency")
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	t.Run("the inventory itself has to carry a key field", func(t *testing.T) {
		manifest := residencyManifest(t)
		list := manifest.Actions["list_models"]
		list.Result = nil
		manifest.Actions["list_models"] = list

		err := manifest.Validate()

		require.Error(t, err, "residency keyed on a field nobody declares is no join")
		assert.Contains(t, err.Error(), "residency")
	})

	t.Run("only loaded_models may declare a join", func(t *testing.T) {
		manifest := residencyManifest(t)
		listed := manifest.Actions["list_models"]
		listed.Result.Residency = &ActionResidency{Source: "loaded_models", Read: &ActionHTTP{Method: "GET", Path: "/health"}, PathField: "id", RowPathField: "path"}
		manifest.Actions["list_models"] = listed

		err := manifest.Validate()

		require.Error(t, err, "the listing action does not serve a model, so it has nothing to join")
		assert.Contains(t, err.Error(), "loaded_models")
	})

	t.Run("a read block needs a method and a path", func(t *testing.T) {
		manifest := residencyManifest(t)
		loaded := manifest.Actions["loaded_models"]
		loaded.Result.Residency.Read = &ActionHTTP{}
		manifest.Actions["loaded_models"] = loaded

		err := manifest.Validate()

		require.Error(t, err, "a residency read has to say where the loaded path is answered")
		assert.Contains(t, err.Error(), "residency")
	})
}

func TestManifestValidate_RejectsAResidencySourceThatStopsOrRestartsTheEngine(t *testing.T) {
	// A residency lookup runs while an engine is serving, and it runs often, so a
	// source action that also takes the engine down would make answering "what is
	// this engine serving?" stop, restart or relaunch the thing being asked. PAIR
	// never does that to a running engine on its own, which is why this is checked
	// when the manifest loads rather than at the moment of the read. Each case is
	// written the way a manifest would write it, with the source still carrying a
	// result so the only thing wrong with it is what else it does.
	cases := []struct {
		name   string
		source string
		action Action
	}{
		{
			name:   "a source that restarts the engine after it answers",
			source: "list_models",
			action: Action{
				Cmd:          []string{"mtplx", "models", "--json"},
				Result:       &ActionResult{Array: "models", Field: "repo_id"},
				RestartAfter: true,
			},
		},
		{
			name:   "a source that rewrites the launch settings and relaunches",
			source: "list_models",
			action: Action{
				LaunchModel: &ActionLaunchModel{Flags: []string{"--model"}},
				Result:      &ActionResult{Array: "models", Field: "repo_id"},
			},
		},
		{
			name:   "a source that stops the engine",
			source: "unload_model",
			action: Action{
				StopEngine: true,
				Result:     &ActionResult{Array: "data", Field: "id"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := residencyManifestWithRestart(t)
			manifest.Actions[tc.source] = tc.action
			loaded := manifest.Actions["loaded_models"]
			loaded.Result.Residency.Source = tc.source
			manifest.Actions["loaded_models"] = loaded

			err := manifest.Validate()

			require.Error(t, err, "a residency lookup must only read")
			// The wording is the point: a failure naming the platform instead would
			// mean the case tripped some other rule and proved nothing here.
			assert.Contains(t, err.Error(), "a residency lookup must only read", "which rule fired: %v", err)
			assert.Contains(t, err.Error(), tc.source)
		})
	}

	t.Run("a cmd source that only reads still validates", func(t *testing.T) {
		// The rule is about what a source does, not how it is carried. MTPLX reads
		// its cache with a cmd action, so a manifest that stages the same shape has
		// to pass: a rule that rejected cmd sources would reject the one manifest
		// this whole file is about.
		manifest := residencyManifestWithRestart(t)
		manifest.Actions["list_models"] = Action{
			Cmd:    []string{"mtplx", "models", "--json"},
			Result: &ActionResult{Array: "models", Field: "repo_id"},
		}

		require.NoError(t, manifest.Validate())
	})
}
