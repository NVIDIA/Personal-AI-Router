// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file is about the other half of the same failure: not what a port says when
// PAIR asks it a question, but which ports PAIR asks at all, and what PAIR
// concludes when one of them answers as NVIDIA PAIR itself.
//
// Both come from the same two Macs. A MTPLX that finds its own port taken moves
// upward until it can bind, so on one machine the user's server sat on 8001 after
// PAIR's proxy took 8000, and on the other it sat on 6666 because that is the port
// the user's own app keeps in its settings file. Neither place is the port PAIR
// configures for MTPLX, and PAIR's settings-file read was 3911 bytes of a stale
// answer on one of them, so a single port and a single file are not enough to find
// a running engine.
//
// Widening the search has a cost that has to be paid in the same change: the extra
// ports are not empty. 8000 is PAIR's own vLLM facade, and PAIR's proxy answers a
// marked health check with 409 to say it is a facade and not an engine. Filed as
// "somebody else is here", that answer refuses uninstalls, keeps a quit engine
// marked running, and generally makes PAIR's own plumbing look like a foreign
// service. So the two rules below belong together: look in more places, and read
// PAIR's own facade as an empty port rather than as a squatter.

// portRun reserves n consecutive loopback ports and returns the numbers with
// nothing listening on any of them. Consecutive is the point: the tests below are
// about a search that walks upward from a port, so they need a run they can lay
// servers into, and a run with a hole in it would prove nothing about that walk.
func portRun(t *testing.T, n int) []int {
	t.Helper()

	for {
		// One listener per port, all held at once, so a port PAIR has already
		// proved free cannot be taken by another process between two of them.
		listeners := make([]net.Listener, 0, n)
		try := func() ([]int, bool) {
			first, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return nil, false
			}
			listeners = append(listeners, first)
			base := first.Addr().(*net.TCPAddr).Port
			ports := []int{base}
			for port := base + 1; len(ports) < n; port++ {
				ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
				if err != nil {
					return nil, false
				}
				listeners = append(listeners, ln)
				ports = append(ports, port)
			}
			return ports, true
		}

		ports, ok := try()
		for _, ln := range listeners {
			_ = ln.Close()
		}
		if ok {
			return ports
		}
		// The OS would not hand over a contiguous run that starts here; ask
		// again rather than build a test whose walk has a hole in it.
	}
}

// serveOnPort puts a server up on a port the test already reserved.
//
// It builds the server around the listener rather than calling
// httptest.NewUnstartedServer, because that helper binds a listener of its own
// first — one that replacing it never closes. A fixture that leaks a bound port
// into the range the test is measuring turns an empty port into something that
// answers, which is exactly the distinction these tests are about.
func serveOnPort(t *testing.T, port int, handler http.HandlerFunc) *httptest.Server {
	t.Helper()

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err, "port %d was taken between reserving it and filling it", port)
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: handler}}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// answersJSON replies to every path with a JSON body, which is how a foreign
// router behaves toward a probe that asks it the wrong question.
func answersJSON(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}
}

// pairFacade answers the way NVIDIA PAIR's own compatibility facade answers a
// marked health check: HTTP 409 and the one error code only PAIR writes. It notes
// whether the marker arrived, because that marker is the only reason a facade can
// be told apart from the engine it fronts — an unmarked request would have been
// forwarded, and a fixture that ignored the header would pass even if PAIR stopped
// sending it.
func pairFacade(sawMarker *atomic.Bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(engineIdentityProbeHeader) == "1" {
			sawMarker.Store(true)
		}
		// The body and header are the ones nvpair-proxy actually writes for a
		// marked request (writeIngressError in ingress.go), read off a live
		// facade on this machine rather than written to match the parser.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusConflict)
		_, _ = fmt.Fprint(w, `{"code":"proxy-facade","error":"the compatibility facade is not the MTPLX engine"}`)
	}
}

// sweepManifest builds the manifest these tests need: a readiness probe that asks
// about a key only MTPLX answers, a settings file that may name where the user's
// copy really is, and a declared set of extra ports with how far to walk up from
// each. The binary is a path that does not exist, so a second copy cannot quietly
// come up beside whatever the fixture put on a port.
func sweepManifest(t *testing.T, probe *Probe, configuredPort int, probePorts []int, walk int, source *PortSource) *Manifest {
	t.Helper()

	missing := filepath.Join(t.TempDir(), "mtplx"+exeExt())
	return &Manifest{
		Engine: "mtplx", DisplayName: "MTPLX", ManifestVersion: 1,
		Platforms: map[string]Platform{hostPlatformKey(): {
			Detect: []string{missing},
			Runtime: Runtime{
				Bin:        missing,
				Port:       configuredPort,
				PortSource: source,
				Ready:      probe,
				ProbePorts: probePorts,
				ProbeRun:   walk,
			},
		}},
	}
}

// A user's MTPLX answers on a port their own software chose, and that is the only
// port it will ever answer on. PAIR's configured port is where PAIR would put its
// own copy, not where the engine already running keeps itself, so the search has to
// cover the ports the engine itself looks at.
func TestCandidatePorts_AddsThePortsTheEnginesOwnSoftwareKeeps(t *testing.T) {
	const watched = 8101

	tests := []struct {
		name       string
		settings   string
		probePorts []int
		want       []int
	}{
		{
			name:       "adds the ports MTPLX probes and lands on",
			probePorts: []int{8000, 18083, 18084, 18085},
			want:       []int{watched, 8000, 18083, 18084, 18085},
		},
		{
			// Where the file and the port list disagree, both are asked: the
			// file is what a user's own app wrote, and it can be stale.
			name:       "asks the settings-file port and the declared ports",
			settings:   `{"port":6666}`,
			probePorts: []int{8000, 18083},
			want:       []int{watched, 6666, 8000, 18083},
		},
		{
			name:       "asks a declared port once",
			probePorts: []int{8000, 8000},
			want:       []int{watched, 8000},
		},
		{
			// The watched port stays first: it is the port a second copy would
			// bind, and what PAIR concludes about it decides whether one starts.
			name:       "skips a declared port that is the port already watched",
			probePorts: []int{watched, 8000},
			want:       []int{watched, 8000},
		},
		{
			name:       "says nothing about ports that cannot exist",
			probePorts: []int{0, -1, 70000, 8000},
			want:       []int{watched, 8000},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "missing.json")
			if tc.settings != "" {
				file = writePortSettings(t, tc.settings)
			}
			manifest := sweepManifest(t, healthProbeAskingForMTPLXsKey(), watched, tc.probePorts, 4,
				&PortSource{File: file})
			ex := newTestExecutor(t, manifest)
			st, err := ex.state("mtplx")
			require.NoError(t, err)

			assert.Equal(t, tc.want, ex.candidatePorts(st, watched).anchors)
		})
	}
}

// The four numbers are not PAIR's invention: they are the ports MTPLX's own client
// probes, taken out of the software, plus the port it defaults to. 8000 is also
// PAIR's vLLM facade port, which is why this set is only safe together with the
// rule that a facade answer reads as an empty port.
func TestMTPLXManifest_NamesThePortsMTPLXItselfLooksAt(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"))
	m, ok := reg.Get("mtplx")
	require.True(t, ok, "mtplx manifest not loaded")

	for key, plat := range m.Platforms {
		assert.Equal(t, []int{8000, 18083, 18084, 18085}, plat.Runtime.ProbePorts,
			"%s: MTPLX must be looked for on the ports MTPLX's own software probes and relocates onto", key)
		assert.Greater(t, plat.Runtime.ProbeRun, 1,
			"%s: a MTPLX pushed off its port moves upward, so the search has to walk with it", key)
	}
}

// The relocation case, which is what two Macs actually produced: the user's MTPLX
// wanted 8000, found PAIR's facade there, and moved. Neither the configured port
// nor the settings file said where it landed, and MTPLX's own CLI missed it for the
// same reason.
func TestPortSweep_FindsTheEngineOnThePortsItWasPushedOffTo(t *testing.T) {
	t.Run("walks past the ports it had to leave behind", func(t *testing.T) {
		// One reserved run of ports, laid end to end, so the walk has real
		// ground to cover: PAIR's own empty port, then the listeners a MTPLX
		// that could not bind its preferred port had to leave behind it.
		ports := portRun(t, 4)
		configured, facadePort, llamaPort, mtplxPort := ports[0], ports[1], ports[2], ports[3]

		var sawMarker atomic.Bool
		facade := serveOnPort(t, facadePort, pairFacade(&sawMarker))
		defer facade.Close()
		// llama.cpp on a port of its own: a real router, and not MTPLX. A walk
		// that stopped at the first non-matching answer would end here.
		llama := serveOnPort(t, llamaPort, answersJSON(http.StatusOK, llamaCPPHealthBody))
		defer llama.Close()
		mtplx := serveOnPort(t, mtplxPort, answersJSON(http.StatusOK, mtplxHealthBody(t, mtplxServedModel, mtplxServedPath)))
		defer mtplx.Close()

		probe := healthProbeAskingForMTPLXsKey()
		ex := newTestExecutor(t, sweepManifest(t, probe, configured, []int{facadePort}, 4, nil))
		st, err := ex.state("mtplx")
		require.NoError(t, err)
		sweep := ex.candidatePorts(st, configured)

		obs := ex.probePortObservation(t.Context(), probe, sweep)

		require.True(t, sawMarker.Load(),
			"a probe that does not carry the marker would have been forwarded through the facade to an engine behind it")
		require.Equal(t, mtplxPort, obs.identified,
			"a user's MTPLX on a port it had to move to was not found, so PAIR would start a second one (%+v)", obs)
		// The ground one round covers, in the order it covered it: PAIR's own
		// empty port, then straight up the run of occupied ports the engine left
		// behind it. That the facade and llama.cpp are on the way is why the walk
		// keeps going; neither is a reason to treat the range as somebody else's,
		// and neither is anywhere a second copy would be launched onto.
		assert.Equal(t, []int{configured, facadePort, llamaPort, mtplxPort}, obs.asked,
			"the search did not walk from the declared port up to the one that answered")
	})

	t.Run("stops a walk that goes on longer than the search is allowed to", func(t *testing.T) {
		// Six reserved ports: PAIR's own empty port, and a run of five listeners
		// long enough that the walk has to stop inside it. Every one of them
		// answers, and none of them is MTPLX. The search has to stop somewhere;
		// what matters is that stopping is not reported as having found nobody,
		// because a second copy onto a busy port is a second router for one model.
		ports := portRun(t, 6)
		configured := ports[5]
		for _, port := range ports[:5] {
			srv := serveOnPort(t, port, answersJSON(http.StatusOK, llamaCPPHealthBody))
			defer srv.Close()
		}

		probe := healthProbeAskingForMTPLXsKey()
		ex := newTestExecutor(t, sweepManifest(t, probe, configured, []int{ports[0]}, 4, nil))
		st, err := ex.state("mtplx")
		require.NoError(t, err)

		obs := ex.probePortObservation(t.Context(), probe, ex.candidatePorts(st, configured))

		assert.Zero(t, obs.identified, "a llama.cpp router was taken for MTPLX (%+v)", obs)
		assert.False(t, obs.quiet, "a run full of listeners was reported as an empty port range (%+v)", obs)
		assert.True(t, obs.occupied(), "a port range full of listeners counted as empty (%+v)", obs)
		// One dial for PAIR's own empty port, then the four ports a walk of four
		// is allowed to cover. The fifth port of the run is left alone: a search
		// with no bound is not an answer either, and stopping short costs a lookup,
		// not a second router, because the ports a copy might bind were empty.
		assert.Equal(t, []int{configured, ports[0], ports[1], ports[2], ports[3]}, obs.asked,
			"the walk ran past its bound (%v)", obs.asked)
	})
}

// A facade is not a place an inference engine lives. It answers for whichever
// engine holds the model, which is exactly the ambiguity that made a relocated
// MTPLX read as a running vLLM, and it is NVIDIA PAIR's own process. Treating it
// as "somebody else is here" would make PAIR's plumbing block its own work, so the
// answer is filed as no service at all.
func TestPortSweep_DoesNotCountPAIRsOwnFacadeAsAnotherService(t *testing.T) {
	// Three reserved ports: PAIR's own empty port, the facade, and the port the
	// walk reaches one step past it, so everything the round asks is ground known
	// to be empty before the fixture starts.
	ports := portRun(t, 3)
	configured, facadePort, walkedPort := ports[0], ports[1], ports[2]

	var sawMarker atomic.Bool
	facade := serveOnPort(t, facadePort, pairFacade(&sawMarker))
	defer facade.Close()

	probe := healthProbeAskingForMTPLXsKey()
	ex := newTestExecutor(t, sweepManifest(t, probe, configured, []int{facadePort}, 2, nil))
	st, err := ex.state("mtplx")
	require.NoError(t, err)

	obs := ex.probePortObservation(t.Context(), probe, ex.candidatePorts(st, configured))

	assert.Zero(t, obs.identified, "PAIR's own facade was taken for the engine (%+v)", obs)
	assert.Zero(t, obs.stranger, "PAIR's own facade was filed as a service that is not this engine (%+v)", obs)
	assert.False(t, obs.answered(), "a facade kept the port range from counting as empty (%+v)", obs)
	assert.True(t, obs.quiet, "an empty port range was reported as occupied (%+v)", obs)
	assert.False(t, obs.spawnBlocked(), "a facade away from the bind port stopped a start (%+v)", obs)
	assert.False(t, obs.occupied(), "a facade was treated as somebody to leave alone (%+v)", obs)
	assert.Equal(t, []int{configured, facadePort, walkedPort}, obs.asked,
		"the round did not cover the range the manifest declared (%+v)", obs)
}

// The consequence that matters while a user is trying to get rid of an engine: the
// facade is up whenever PAIR's proxy runs, so a facade on a probe port would refuse
// every uninstall, on every Mac, forever.
func TestUninstall_PAIRsOwnFacadeOnAProbePort_StillDeletesTheEngine(t *testing.T) {
	baseDir := t.TempDir()
	managedBin := filepath.Join(baseDir, "mtplx", "mtplx"+exeExt())
	require.NoError(t, os.MkdirAll(filepath.Dir(managedBin), 0o755))
	copyFile(t, fakeEngineBin, managedBin)

	// The facade sits on the probe port, and the walk of two it triggers reaches
	// the next port, which is empty: none of this is a service, and none of it is
	// anywhere a MTPLX would be launched onto.
	ports := portRun(t, 3)
	configured, facadePort := ports[0], ports[1]
	facade := serveOnPort(t, facadePort, pairFacade(new(atomic.Bool)))
	defer facade.Close()

	manifest := sweepManifest(t, healthProbeAskingForMTPLXsKey(), configured, []int{facadePort}, 2, nil)
	key := hostPlatformKey()
	p := manifest.Platforms[key]
	p.Detect = []string{managedBin}
	p.Runtime.Bin = managedBin
	p.Uninstall = &Uninstall{Run: rmCmd(managedBin)}
	manifest.Platforms[key] = p

	reg := NewRegistry()
	reg.engines[manifest.Engine] = manifest
	ex := NewExecutor(reg, NewReporter(nil), func(string, any) {}, baseDir)

	err := ex.Uninstall(t.Context(), "mtplx")

	require.NoError(t, err, "uninstall was refused because PAIR's own proxy answered a probe port")
	assert.NoFileExists(t, managedBin, "the refusal left the engine's files in place")
}

// The other consequence: while a facade answers a probe port, a MTPLX the user
// started themselves and then quit would never be allowed to read as stopped. The
// card would stay lit, and PAIR would keep waiting on a server that is gone.
func TestStatus_PAIRsOwnFacadeOnAProbePort_KeepsReportingAQuitedEngineAsStopped(t *testing.T) {
	ports := portRun(t, 3)
	configured, facadePort, servedPort := ports[0], ports[1], ports[2]

	var sawMarker atomic.Bool
	facade := serveOnPort(t, facadePort, pairFacade(&sawMarker))
	defer facade.Close()
	engine := serveOnPort(t, servedPort, answersJSON(http.StatusOK, mtplxHealthBody(t, mtplxServedModel, mtplxServedPath)))
	defer engine.Close()

	source := &PortSource{File: writePortSettings(t, fmt.Sprintf(`{"port": %d}`, servedPort))}
	ex := newTestExecutor(t, sweepManifest(t, healthProbeAskingForMTPLXsKey(), configured, []int{facadePort}, 2, source))

	st, err := ex.Status("mtplx")
	require.NoError(t, err)
	require.True(t, st.Running, "the user's own MTPLX was never seen (%v)", st)
	require.Equal(t, servedPort, st.Port, "the engine was tracked at the wrong port (%v)", st)

	// The user quits their engine. The facade stays up, as it does whenever
	// PAIR's proxy runs.
	engine.Close()

	st, err = ex.Status("mtplx")

	require.NoError(t, err)
	assert.False(t, st.Running, "a MTPLX that exited is still shown running while PAIR's facade answers a probe port (%v)", st)
	assert.False(t, st.Healthy, "a MTPLX that exited is still shown healthy (%v)", st)
	assert.Equal(t, configured, st.Port, "PAIR kept holding the port of a service that is gone (%v)", st)
}

// relocationShape is the two-Mac failure laid out on one machine, using three
// consecutive reserved ports. The lowest stays empty, because that is the port a
// second copy would be launched onto. The middle one carries PAIR's own
// compatibility facade, which is a port MTPLX asks at and therefore an anchor a
// search starts from. The top one carries the user's MTPLX, after it failed to
// bind the port it wanted — which, at the default numbers, is also the port PAIR
// reserves for a vLLM. That is how one engine's hiding place became the other
// engine's identity.
type relocationShape struct {
	emptyPort     int
	facadePort    int
	relocatedPort int
	sawMarker     atomic.Bool
}

// stageRelocation puts those two listeners up. Both are asked the marked health
// check, and sawMarker records whether the marker actually arrived: it is the only
// reason a facade can be told apart from whatever it happens to front.
func stageRelocation(t *testing.T) *relocationShape {
	t.Helper()

	ports := portRun(t, 3)
	layout := &relocationShape{emptyPort: ports[0], facadePort: ports[1], relocatedPort: ports[2]}
	serveOnPort(t, layout.facadePort, pairFacade(&layout.sawMarker))
	serveOnPort(t, layout.relocatedPort,
		answersJSON(http.StatusOK, mtplxHealthBody(t, mtplxServedModel, mtplxServedPath)))
	return layout
}

// hostPlatformBlock reads a shipped manifest back for the machine running the
// test, because these tests are about the search a manifest declares: they take it
// as declared and change only the ports. Where a host has no block for an engine
// there is no pair to stage, and MTPLX is Apple Silicon only, so this skips rather
// than fails.
func hostPlatformBlock(t *testing.T, reg *Registry, engine string) (*Manifest, Platform) {
	t.Helper()

	m, ok := reg.Get(engine)
	require.True(t, ok, "%s must be in the registry for its ports to be tested", engine)
	key := hostPlatformKey()
	p, ok := m.Platforms[key]
	if !ok {
		t.Skipf("%s has no block for %s, so this host stages neither engine", engine, key)
	}
	return m, p
}

// relocationRegistry takes the shipped pair of manifests and moves only their
// ports onto the reserved run: MTPLX looks first at an empty port and then upward
// from a facade, and vLLM is pointed at the port the MTPLX had to move to. Neither
// engine keeps a binary that could really be launched here, and vLLM keeps no
// detect path this machine could have, so the fixture says what the two Macs said —
// no vLLM anywhere, and a MTPLX that had to move.
func relocationRegistry(t *testing.T, layout *relocationShape) *Registry {
	t.Helper()

	reg := NewRegistry()
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"))

	mtplxManifest, mtplxPlat := hostPlatformBlock(t, reg, "mtplx")
	require.NotEmpty(t, mtplxPlat.Runtime.ProbePorts, "MTPLX must search the ports MTPLX keeps")
	require.Greater(t, mtplxPlat.Runtime.ProbeRun, 1,
		"a MTPLX pushed off its port moves upward, so the search has to walk with it")

	// A port round only runs for an engine PAIR thinks is present, so the detect
	// path is pinned to a file that exists on every machine rather than to whatever
	// MTPLX.app this one happens to have. The settings-file read goes for the same
	// reason: without it, a test could find a MTPLX that happens to be running on
	// the machine running it and pass for the wrong reason.
	mtplxPlat.Detect = []string{os.DevNull}
	mtplxPlat.Runtime.Bin = filepath.Join(t.TempDir(), "mtplx"+exeExt())
	mtplxPlat.Runtime.Port = layout.emptyPort
	mtplxPlat.Runtime.PortSource = nil
	mtplxPlat.Runtime.ProbePorts = []int{layout.facadePort}
	mtplxPlat.Runtime.ProbeRun = 2
	mtplxManifest.Platforms[hostPlatformKey()] = mtplxPlat

	vllmManifest, vllmPlat := hostPlatformBlock(t, reg, "vllm")
	vllmPlat.Detect = nil
	vllmPlat.Runtime.Bin = filepath.Join(t.TempDir(), "vllm"+exeExt())
	vllmPlat.Runtime.Port = layout.relocatedPort
	vllmManifest.Platforms[hostPlatformKey()] = vllmPlat

	return reg
}

// The pair as one machine, which is what both Macs produced at the default ports:
// MTPLX wanted 8000, PAIR's proxy was holding it, and the MTPLX moved to 8001 —
// the port PAIR reserves for meeting a vLLM. Neither the port PAIR configures for
// MTPLX nor its settings file says where that is, so the search has to start from a
// declared probe port and keep walking over a facade and a foreign router. What each
// answer means is identity_test.go's business; this file is about which ports get
// asked and what PAIR concludes from the round.
func TestStatus_ARelocatedMTPLXBehindTheFacade_IsFoundOnThePortItMovedTo(t *testing.T) {
	layout := stageRelocation(t)
	ex := NewExecutor(relocationRegistry(t, layout), NewReporter(nil), func(string, any) {}, t.TempDir())

	m, err := ex.Status("mtplx")

	require.NoError(t, err)
	require.True(t, m.Running, "the relocated MTPLX was not found, so PAIR would start a second one (%v)", m)
	assert.Equal(t, layout.relocatedPort, m.Port, "the found engine is tracked at the wrong port (%v)", m)
	require.True(t, layout.sawMarker.Load(),
		"a probe that did not carry the marker would have been forwarded through the facade")
}

// Status is only how PAIR knows. The second copy that must not appear arrives
// through Start, and Start is also where an adopted engine's real port has to
// travel: the health loop and every later request follow the port the engine was
// joined at, so finding a MTPLX on the port it moved to and then watching PAIR's
// own port finds nobody.
func TestStart_ARelocatedMTPLX_IsAdoptedOnThePortItMovedTo(t *testing.T) {
	layout := stageRelocation(t)
	ex := NewExecutor(relocationRegistry(t, layout), NewReporter(nil), func(string, any) {}, t.TempDir())

	require.NoError(t, ex.Start(t.Context(), "mtplx"), "Start refused although the engine is answering")

	state, err := ex.state("mtplx")
	require.NoError(t, err)
	state.mu.Lock()
	defer state.mu.Unlock()
	assert.True(t, state.adopted, "Start did not join the MTPLX that was already running")
	assert.Nil(t, state.proc, "Start launched a second MTPLX beside the running one")
	assert.Equal(t, layout.relocatedPort, state.port, "the adopted engine is tracked at the wrong port")
}

// vLLM's half of the same pair. Its probe asks one port and reads no key of its
// own, which is exactly how a relocated MTPLX came to be read as a running vLLM,
// and a vLLM launched onto that port could not have bound it either. The refusal
// has to name the port the listener is really on, because the advice that goes with
// it is "choose a port nothing is listening on".
func TestStart_VLLMsPortHeldByARelocatedMTPLX_RefusesWithoutLaunchingASecondCopy(t *testing.T) {
	layout := stageRelocation(t)
	ex := NewExecutor(relocationRegistry(t, layout), NewReporter(nil), func(string, any) {}, t.TempDir())

	require.NoError(t, ex.Start(t.Context(), "mtplx"),
		"a MTPLX answering on another port is not vLLM's to refuse")

	err := ex.Start(t.Context(), "vllm")

	require.Error(t, err, "vLLM accepted a port a relocated MTPLX is serving on")
	require.ErrorContains(t, err, fmt.Sprintf("port %d", layout.relocatedPort),
		"the refusal named a port that is not where the listener is")
	require.ErrorContains(t, err, "did not identify as vLLM")

	state, err := ex.state("vllm")
	require.NoError(t, err)
	state.mu.Lock()
	defer state.mu.Unlock()
	assert.Nil(t, state.proc, "Start launched a vLLM onto the port a MTPLX is serving on")
	assert.False(t, state.running, "a refused start left vLLM marked running")
}
