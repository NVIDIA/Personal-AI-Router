// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFrontBackendPort(t *testing.T) {
	f := &Front{BackendPortOffset: 10000}
	for public, want := range map[int]int{8001: 18001, 11435: 21435, 60000: 50000} {
		if got, err := frontBackendPort(f, public); err != nil || got != want {
			t.Errorf("frontBackendPort(%d) = %d, %v; want %d", public, got, err, want)
		}
	}
	if _, err := frontBackendPort(&Front{BackendPortOffset: 60000}, 20000); err == nil {
		t.Error("a backend port outside the usable range was accepted")
	}
}

func vllmLikePolicy() *EditableLaunch {
	return &EditableLaunch{FixedArgs: []string{"serve"}, Controls: []LaunchControl{
		{Flags: []string{"--host"}, Value: "{server.host}"},
		{Flags: []string{"--port"}, Value: "{server.port}"},
	}}
}

func TestApplyFrontBackendRepointsOnlyTheListener(t *testing.T) {
	rt := Runtime{Front: &Front{BackendPortOffset: 10000}, EditableLaunch: vllmLikePolicy()}
	in := launchCommand{Bin: "vllm", Args: []string{"serve", "--model", "Qwen/Qwen3-8B", "--host", "0.0.0.0", "--port", "8001", "--max-model-len", "4096"}}
	original := slices.Clone(in.Args)

	out, err := applyFrontBackend(rt, in, 8001)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"serve", "--model", "Qwen/Qwen3-8B", "--host", "127.0.0.1", "--port", "18001", "--max-model-len", "4096"}
	if !slices.Equal(out.Args, want) {
		t.Fatalf("args = %v, want %v", out.Args, want)
	}
	if !slices.Equal(in.Args, original) {
		t.Fatalf("the caller's arguments were modified: %v", in.Args)
	}

	eq, err := applyFrontBackend(rt, launchCommand{Args: []string{"serve", "--host=0.0.0.0", "--port=8001"}}, 8001)
	if err != nil || !slices.Equal(eq.Args, []string{"serve", "--host=127.0.0.1", "--port=18001"}) {
		t.Fatalf("equals form: %v %v", eq.Args, err)
	}

	missing, err := applyFrontBackend(rt, launchCommand{Args: []string{"serve", "--model", "m"}}, 8001)
	if err != nil || !slices.Equal(missing.Args, []string{"serve", "--model", "m", "--host", "127.0.0.1", "--port", "18001"}) {
		t.Fatalf("absent flags must be added: %v %v", missing.Args, err)
	}
}

func TestApplyFrontBackendIsANoOpWithoutAFront(t *testing.T) {
	in := launchCommand{Bin: "x", Args: []string{"serve", "--port", "8001"}}
	out, err := applyFrontBackend(Runtime{EditableLaunch: vllmLikePolicy()}, in, 8001)
	if err != nil || !slices.Equal(out.Args, in.Args) {
		t.Fatalf("an engine with no front must be untouched: %v %v", out.Args, err)
	}
}

// Only vLLM declares a front. Every other engine must keep launching exactly as
// it did, on its own port, with no gateway.
func TestOnlyVLLMHasAFront(t *testing.T) {
	reg := loadWithOverrides(t, t.TempDir())
	for _, name := range reg.Names() {
		manifest, _ := reg.Get(name)
		for key, platform := range manifest.Platforms {
			if (platform.Runtime.Front != nil) != (name == "vllm") {
				t.Errorf("%s %s: front = %+v", name, key, platform.Runtime.Front)
			}
			command := launchCommand{Bin: "bin", Args: []string{"serve", "--port", "1234", "--host", "h"}}
			out, err := applyFrontBackend(platform.Runtime, command, 1234)
			if name != "vllm" && (err != nil || !slices.Equal(out.Args, command.Args)) {
				t.Errorf("%s %s: launch changed to %v (%v)", name, key, out.Args, err)
			}
		}
	}
	vllm, _ := reg.Get("vllm")
	if front := vllm.Platforms["linux/amd64"].Runtime.Front; front == nil || front.BackendPortOffset != 10000 || front.IdleTimeoutS != 300 {
		t.Errorf("vllm front = %+v", front)
	}
}

func TestFrontValidation(t *testing.T) {
	good := Platform{Runtime: Runtime{Bin: "vllm", Port: 8001, Front: &Front{BackendPortOffset: 10000}, EditableLaunch: vllmLikePolicy()}}
	if err := good.validate("linux/amd64"); err != nil {
		t.Fatalf("a valid front was rejected: %v", err)
	}
	for name, mutate := range map[string]func(p *Platform){
		"no offset":          func(p *Platform) { p.Runtime.Front = &Front{} },
		"offset too large":   func(p *Platform) { p.Runtime.Front = &Front{BackendPortOffset: 70000} },
		"no editable launch": func(p *Platform) { p.Runtime.EditableLaunch = nil },
		"command mode":       func(p *Platform) { p.Runtime.Mode = "command" },
		"no port flag control": func(p *Platform) {
			p.Runtime.EditableLaunch = &EditableLaunch{Controls: []LaunchControl{{Flags: []string{"--host"}, Value: "{server.host}"}}}
		},
	} {
		p := good
		mutate(&p)
		if err := p.validate("linux/amd64"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func gatewayFor(t *testing.T, backend http.Handler) (publicURL string) {
	t.Helper()
	srv := httptest.NewServer(backend)
	t.Cleanup(srv.Close)
	_, backendPort, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	bp, _ := strconv.Atoi(backendPort)
	public, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	g, err := startFrontGateway("127.0.0.1", public, bp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.close)
	return fmt.Sprintf("http://127.0.0.1:%d", public)
}

func TestFrontGatewayForwardsRequestsUnchanged(t *testing.T) {
	var gotBody []byte
	var gotHeader http.Header
	url := gatewayFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHeader = r.Header.Clone()
		w.Header().Set("X-Engine", "yes")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTeapot)
		fmt.Fprintf(w, `{"method":%q,"path":%q,"query":%q}`, r.Method, r.URL.Path, r.URL.RawQuery)
	}))

	body := bytes.Repeat([]byte("x"), 5<<20) // a large request, as a vision prompt can be
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions?a=1&b=two", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reply, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusTeapot || resp.Header.Get("X-Engine") != "yes" {
		t.Fatalf("status/headers not preserved: %d %v", resp.StatusCode, resp.Header)
	}
	if string(reply) != `{"method":"POST","path":"/v1/chat/completions","query":"a=1&b=two"}` {
		t.Fatalf("reply = %s", reply)
	}
	if !bytes.Equal(gotBody, body) {
		t.Fatalf("the body was altered (%d bytes in, %d out)", len(body), len(gotBody))
	}
	if gotHeader.Get("Authorization") != "Bearer secret" || gotHeader.Get("Accept-Encoding") != "gzip" {
		t.Fatalf("headers not preserved: %v", gotHeader)
	}
}

func TestFrontGatewayStreamsAsItArrives(t *testing.T) {
	release := make(chan struct{})
	url := gatewayFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "data: two\n\n")
	}))
	resp, err := http.Get(url + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	first := make([]byte, len("data: one\n\n"))
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(resp.Body, first); done <- err }()
	select {
	case err := <-done:
		if err != nil || string(first) != "data: one\n\n" {
			t.Fatalf("first event = %q, %v", first, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first event was held back until the response ended")
	}
	close(release)
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "data: two\n\n" {
		t.Fatalf("rest = %q", rest)
	}
}

func TestFrontGatewayCancelsTheEngineRequestWhenTheClientLeaves(t *testing.T) {
	var cancelled atomic.Bool
	started := make(chan struct{})
	url := gatewayFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server notices a client leaving only once the body has been read.
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
			cancelled.Store(true)
		case <-time.After(10 * time.Second):
		}
	}))
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/completions", strings.NewReader("{}"))
	go func() { _, _ = http.DefaultClient.Do(req) }()
	<-started
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for !cancelled.Load() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !cancelled.Load() {
		t.Fatal("the engine kept generating after the client left")
	}
}

func TestFrontGatewayAnswers503WhileTheEngineIsDown(t *testing.T) {
	public, _ := freePort()
	dead, _ := freePort() // nothing listens here
	g, err := startFrontGateway("127.0.0.1", public, dead)
	if err != nil {
		t.Fatal(err)
	}
	defer g.close()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", public))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "not ready") {
		t.Fatalf("got %d %s, want 503 while the engine is not up", resp.StatusCode, body)
	}
}

func TestFrontGatewayCloseFreesThePortAndIsIdempotent(t *testing.T) {
	public, _ := freePort()
	g, err := startFrontGateway("127.0.0.1", public, 1)
	if err != nil {
		t.Fatal(err)
	}
	g.close()
	g.close()
	(*frontGateway)(nil).close()
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", public))
	if err != nil {
		t.Fatalf("the public port was not released: %v", err)
	}
	ln.Close()
}

// frontExecutor builds an engine whose process listens where the launch flag
// says, so the gateway in front of it can be exercised end to end.
func frontExecutor(t *testing.T, idleSeconds int) (e *Executor, public, backend int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses the fake engine's --listen flag")
	}
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		public, err = freePort()
		if err != nil {
			t.Fatal(err)
		}
		if ln, lerr := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", public+1)); lerr == nil {
			ln.Close()
			break
		}
	}
	backend = public + 1
	m := testEngineManifest(fakeEngineBin)
	m.Engine = "fronted"
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Runtime.Port = public
	p.Runtime.Env = nil
	p.Runtime.Args = []string{"serve", "--model", "org/m", "--listen", "{host}:{port}"}
	p.Runtime.EditableLaunch = &EditableLaunch{FixedArgs: []string{"serve"}, Controls: []LaunchControl{
		{Flags: []string{"--listen"}, Value: "{server.host}:{server.port}"},
	}}
	p.Runtime.Front = &Front{BackendPortOffset: 1, IdleTimeoutS: idleSeconds}
	m.Platforms[key] = p
	// The engine advertises only what it has loaded, as vLLM does; Eject and Load
	// are the same actions vLLM declares.
	m.AdvertisedModels = advertisedModelsLoaded
	m.Actions = map[string]Action{
		"loaded_models": {HTTP: &ActionHTTP{Method: "GET", Path: "/api/ps"}, Result: &ActionResult{Array: "models", Field: "name"}},
		"unload_model":  {StopEngine: true},
		"load_model":    {LaunchModel: &ActionLaunchModel{Flag: "--model"}},
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	e = newTestExecutor(t, m)
	e.overrideDir = t.TempDir()
	if _, err := e.Detect("fronted"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop("fronted") })
	return e, public, backend
}

func statusCode(url string) int {
	resp, err := http.Get(url)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestFrontedEngineIsReachedThroughItsPublicPort(t *testing.T) {
	e, public, backend := frontExecutor(t, 0)
	if err := e.StartWith(context.Background(), "fronted", startOpts{Eager: true}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := statusCode(fmt.Sprintf("http://127.0.0.1:%d/api/tags", public)); got != http.StatusOK {
		t.Fatalf("public port answered %d", got)
	}
	if got := statusCode(fmt.Sprintf("http://127.0.0.1:%d/api/tags", backend)); got != http.StatusOK {
		t.Fatalf("the engine is not on its backend port: %d", got)
	}
	st, err := e.Status("fronted")
	if err != nil || !st.Running || !st.Healthy || st.Port != public {
		t.Fatalf("status = %+v, %v; want running and healthy on the public port %d", st, err, public)
	}

	if err := e.Stop("fronted"); err != nil {
		t.Fatal(err)
	}
	for _, port := range []int{public, backend} {
		if got := statusCode(fmt.Sprintf("http://127.0.0.1:%d/", port)); got != 0 {
			t.Fatalf("port %d still answers after stop (%d)", port, got)
		}
	}

	// A restart takes the public port straight back.
	if err := e.StartWith(context.Background(), "fronted", startOpts{Eager: true}); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if got := statusCode(fmt.Sprintf("http://127.0.0.1:%d/api/tags", public)); got != http.StatusOK {
		t.Fatalf("after restart the public port answered %d", got)
	}
}

func TestFrontedEngineGatewayGoesWithACrashedEngine(t *testing.T) {
	e, public, _ := frontExecutor(t, 0)
	if err := e.StartWith(context.Background(), "fronted", startOpts{Eager: true}); err != nil {
		t.Fatal(err)
	}
	_ = statusCode(fmt.Sprintf("http://127.0.0.1:%d/exit", public)) // the engine dies
	deadline := time.Now().Add(10 * time.Second)
	for statusCode(fmt.Sprintf("http://127.0.0.1:%d/", public)) != 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := statusCode(fmt.Sprintf("http://127.0.0.1:%d/", public)); got != 0 {
		t.Fatalf("the gateway kept answering for a dead engine (%d)", got)
	}
	if st, _ := e.Status("fronted"); st.Running {
		t.Fatalf("a crashed engine is reported running: %+v", st)
	}
}

func TestFrontedEngineRefusesABackendPortSomethingElseHolds(t *testing.T) {
	e, public, backend := frontExecutor(t, 0)
	squatter, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", backend))
	if err != nil {
		t.Skipf("backend port taken: %v", err)
	}
	defer squatter.Close()

	err = e.StartWith(context.Background(), "fronted", startOpts{Eager: true})
	if err == nil || !strings.Contains(err.Error(), "in use by another program") {
		t.Fatalf("err = %v, want the occupied internal port reported", err)
	}
	if got := statusCode(fmt.Sprintf("http://127.0.0.1:%d/", public)); got != 0 {
		t.Fatalf("a failed start left a gateway on the public port (%d)", got)
	}
}

func post(url string) int {
	resp, err := http.Post(url, "application/json", strings.NewReader("{}"))
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func eventually(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func up(port int) bool {
	return statusCode(fmt.Sprintf("http://127.0.0.1:%d/api/tags", port)) == http.StatusOK
}

func TestIsActivity(t *testing.T) {
	for method, want := range map[string]bool{"GET": false, "HEAD": false, "OPTIONS": false, "POST": true, "PUT": true, "DELETE": true} {
		if got := isActivity(&http.Request{Method: method}); got != want {
			t.Errorf("%s: isActivity = %v, want %v", method, got, want)
		}
	}
}

func TestGatewayIdleAccounting(t *testing.T) {
	g := &frontGateway{closed: make(chan struct{})}
	if g.expired(0) || g.enterIdle(0, false) {
		t.Fatal("a gateway whose engine is not up cannot go idle")
	}
	g.markUp()
	if g.expired(time.Hour) {
		t.Fatal("a fresh engine is not idle")
	}
	g.mu.Lock()
	g.lastActive = time.Now().Add(-time.Minute)
	g.inflight = 1
	g.mu.Unlock()
	if g.expired(time.Second) || g.enterIdle(time.Second, false) {
		t.Fatal("a request in flight keeps the engine loaded")
	}
	if !g.enterIdle(time.Second, true) {
		t.Fatal("an explicit eject ignores in-flight work")
	}
	if g.enterIdle(time.Second, true) {
		t.Fatal("an engine already idle cannot be unloaded twice")
	}
}

func TestIdleEngineIsUnloadedAfterTheTimeout(t *testing.T) {
	e, public, backend := frontExecutor(t, 1)
	if err := e.StartWith(context.Background(), "fronted", startOpts{Eager: true}); err != nil {
		t.Fatal(err)
	}
	if !up(public) {
		t.Fatal("not serving")
	}
	eventually(t, "the idle unload", 10*time.Second, func() bool { return !up(backend) })

	st, _ := e.Status("fronted")
	if !st.Running || !st.Idle || st.Port != public {
		t.Fatalf("status = %+v, want ON, idle, on the public port", st)
	}
	if got := statusCode(fmt.Sprintf("http://127.0.0.1:%d/health", public)); got != http.StatusOK {
		t.Fatalf("an idle engine must still pass health checks: %d", got)
	}
	// Only a request that needs the model loads it. A read, or a post to something
	// that is not inference, is refused and leaves it unloaded.
	if got := statusCode(fmt.Sprintf("http://127.0.0.1:%d/api/tags", public)); got != http.StatusServiceUnavailable {
		t.Fatalf("a read while unloaded answered %d, want 503", got)
	}
	if got := post(fmt.Sprintf("http://127.0.0.1:%d/admin/reset", public)); got != http.StatusServiceUnavailable {
		t.Fatalf("a non-inference post answered %d, want 503", got)
	}
	if up(backend) {
		t.Fatal("something that is not inference loaded the model")
	}
}

// The request that arrives while the model is unloaded loads it and is answered,
// as Ollama and llama.cpp do.
func TestARequestLoadsAnUnloadedEngineAndIsAnswered(t *testing.T) {
	e, public, backend := frontExecutor(t, 1)
	if err := e.StartWith(context.Background(), "fronted", startOpts{Eager: true}); err != nil {
		t.Fatal(err)
	}
	for cycle := 0; cycle < 2; cycle++ {
		eventually(t, "the idle unload", 10*time.Second, func() bool { return !up(backend) })
		if got := post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", public)); got != http.StatusOK {
			t.Fatalf("cycle %d: the request that woke the engine answered %d", cycle, got)
		}
		if !up(backend) {
			t.Fatalf("cycle %d: the engine is not loaded", cycle)
		}
		if st, _ := e.Status("fronted"); !st.Running || st.Idle || !st.Healthy {
			t.Fatalf("cycle %d: status = %+v", cycle, st)
		}
	}
}

func startCount(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "\n")
}

func TestRequestsThatArriveTogetherShareOneLoad(t *testing.T) {
	log := filepath.Join(t.TempDir(), "starts.log")
	t.Setenv("FAKE_START_LOG", log)
	t.Setenv("FAKE_START_DELAY", "700ms")
	e, public, backend := frontExecutor(t, 1)
	if err := e.StartWith(context.Background(), "fronted", startOpts{Eager: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the idle unload", 10*time.Second, func() bool { return !up(backend) })
	before := startCount(t, log)

	var wg sync.WaitGroup
	results := make([]int, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = post(fmt.Sprintf("http://127.0.0.1:%d/v1/completions", public))
		}()
	}
	wg.Wait()
	for i, code := range results {
		if code != http.StatusOK {
			t.Fatalf("request %d answered %d", i, code)
		}
	}
	if got := startCount(t, log) - before; got != 1 {
		t.Fatalf("the engine was started %d times for 8 simultaneous requests, want 1", got)
	}
}

func TestAClientLeavingDoesNotAbandonTheLoad(t *testing.T) {
	t.Setenv("FAKE_START_DELAY", "1500ms")
	e, public, backend := frontExecutor(t, 1)
	if err := e.StartWith(context.Background(), "fronted", startOpts{Eager: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the idle unload", 15*time.Second, func() bool { return !up(backend) })

	client := &http.Client{Timeout: 300 * time.Millisecond}
	if resp, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", public), "application/json", strings.NewReader("{}")); err == nil {
		resp.Body.Close()
		t.Fatal("expected the impatient client to give up")
	}
	eventually(t, "the load to finish without the client", 15*time.Second, func() bool { return up(backend) })
	if got := post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", public)); got != http.StatusOK {
		t.Fatalf("the next request answered %d", got)
	}
}

func TestARequestDuringALoadFromEjectWaitsForIt(t *testing.T) {
	t.Setenv("FAKE_START_DELAY", "1s")
	e, public, backend := frontExecutor(t, 0)
	if err := e.StartWith(context.Background(), "fronted", startOpts{Eager: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Action(context.Background(), "fronted", "unload_model", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	loaded := make(chan error, 1)
	go func() {
		_, err := e.Action(context.Background(), "fronted", "load_model", []byte(`{"model":"org/m"}`))
		loaded <- err
	}()
	time.Sleep(300 * time.Millisecond) // the load is under way
	if got := post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", public)); got != http.StatusOK {
		t.Fatalf("a request during the load answered %d", got)
	}
	if err := <-loaded; err != nil || !up(backend) {
		t.Fatalf("load: %v", err)
	}
}

func TestAFailedLoadIsReportedToTheRequestAndRetriedByTheNext(t *testing.T) {
	e, public, backend := frontExecutor(t, 1)
	if err := e.StartWith(context.Background(), "fronted", startOpts{Eager: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the idle unload", 10*time.Second, func() bool { return !up(backend) })
	squatter, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", backend))
	if err != nil {
		t.Skipf("backend port taken: %v", err)
	}
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", public), "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "could not be loaded") {
		t.Fatalf("got %d %s, want 503 saying the model could not be loaded", resp.StatusCode, body)
	}
	if st, _ := e.Status("fronted"); !st.Running || !st.Idle {
		t.Fatalf("status = %+v, want ON and still idle", st)
	}
	squatter.Close()
	if got := post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", public)); got != http.StatusOK {
		t.Fatalf("the next request answered %d, want it to load", got)
	}
}

func TestInferenceKeepsAnEngineLoadedButMonitoringDoesNot(t *testing.T) {
	e, public, backend := frontExecutor(t, 1)
	if err := e.StartWith(context.Background(), "fronted", startOpts{Eager: true}); err != nil {
		t.Fatal(err)
	}
	// Requests every 300 ms against a 1 s limit: never idle.
	end := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(end) {
		post(fmt.Sprintf("http://127.0.0.1:%d/api/generate", public))
		time.Sleep(300 * time.Millisecond)
	}
	if !up(backend) {
		t.Fatal("the engine was unloaded while it was being used")
	}
	// Polling health and the model list, as the proxy and monitors do, does not count.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				statusCode(fmt.Sprintf("http://127.0.0.1:%d/health", public))
				statusCode(fmt.Sprintf("http://127.0.0.1:%d/api/tags", public))
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()
	defer close(stop)
	eventually(t, "unload despite monitoring traffic", 10*time.Second, func() bool { return !up(backend) })
}

func TestARequestInFlightKeepsTheEngineLoaded(t *testing.T) {
	e, public, backend := frontExecutor(t, 1)
	if err := e.StartWith(context.Background(), "fronted", startOpts{Eager: true}); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- post(fmt.Sprintf("http://127.0.0.1:%d/slow?ms=2500", public)) }()
	time.Sleep(1800 * time.Millisecond) // well past the 1 s limit
	if !up(backend) {
		t.Fatal("the engine was unloaded under a request that was still running")
	}
	if got := <-done; got != http.StatusOK {
		t.Fatalf("the long request answered %d", got)
	}
}

func TestZeroTimeoutNeverUnloads(t *testing.T) {
	e, _, backend := frontExecutor(t, 0)
	if err := e.StartWith(context.Background(), "fronted", startOpts{Eager: true}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if !up(backend) {
		t.Fatal("an engine with no idle timeout was unloaded")
	}
}

func TestStoppingAnIdleEngineStopsEverything(t *testing.T) {
	e, public, backend := frontExecutor(t, 1)
	if err := e.StartWith(context.Background(), "fronted", startOpts{Eager: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the idle unload", 10*time.Second, func() bool { return !up(backend) })
	if err := e.Stop("fronted"); err != nil {
		t.Fatalf("stop while idle: %v", err)
	}
	if st, _ := e.Status("fronted"); st.Running || st.Idle {
		t.Fatalf("status = %+v, want stopped", st)
	}
	if got := statusCode(fmt.Sprintf("http://127.0.0.1:%d/health", public)); got != 0 {
		t.Fatalf("the gateway outlived the stop (%d)", got)
	}
	if err := e.StartWith(context.Background(), "fronted", startOpts{Eager: true}); err != nil || !up(public) {
		t.Fatalf("starting after a stop from idle: %v", err)
	}
}

func TestWakeLoadsTheEngineBackBehindTheSameGateway(t *testing.T) {
	e, public, backend := frontExecutor(t, 1)
	if err := e.StartWith(context.Background(), "fronted", startOpts{Eager: true}); err != nil {
		t.Fatal(err)
	}
	for cycle := 0; cycle < 2; cycle++ {
		eventually(t, "the idle unload", 10*time.Second, func() bool { return !up(backend) })
		if err := e.wakeIdle(context.Background(), "fronted"); err != nil {
			t.Fatalf("wake %d: %v", cycle, err)
		}
		if !up(public) || !up(backend) {
			t.Fatalf("wake %d: the engine is not serving", cycle)
		}
		if st, _ := e.Status("fronted"); !st.Running || st.Idle || !st.Healthy {
			t.Fatalf("wake %d: status = %+v", cycle, st)
		}
	}
	// A crash after a wake still takes the gateway with it.
	_ = statusCode(fmt.Sprintf("http://127.0.0.1:%d/exit", backend))
	eventually(t, "the gateway to close", 10*time.Second, func() bool {
		return statusCode(fmt.Sprintf("http://127.0.0.1:%d/health", public)) == 0
	})
}

func TestFailedWakeLeavesTheEngineIdle(t *testing.T) {
	e, public, backend := frontExecutor(t, 1)
	if err := e.StartWith(context.Background(), "fronted", startOpts{Eager: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the idle unload", 10*time.Second, func() bool { return !up(backend) })
	squatter, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", backend))
	if err != nil {
		t.Skipf("backend port taken: %v", err)
	}
	if err := e.wakeIdle(context.Background(), "fronted"); err == nil {
		t.Fatal("a wake onto an occupied port succeeded")
	}
	st, _ := e.Status("fronted")
	if !st.Running || !st.Idle {
		t.Fatalf("status = %+v, want still ON and idle after a failed wake", st)
	}
	if got := statusCode(fmt.Sprintf("http://127.0.0.1:%d/health", public)); got != http.StatusOK {
		t.Fatalf("the idle gateway stopped answering health checks (%d)", got)
	}
	squatter.Close()
	if err := e.wakeIdle(context.Background(), "fronted"); err != nil || !up(public) {
		t.Fatalf("a second wake after the port was freed: %v", err)
	}
}

func TestEjectUnloadsButKeepsTheEngineOn(t *testing.T) {
	e, public, backend := frontExecutor(t, 0)
	if err := e.StartWith(context.Background(), "fronted", startOpts{Eager: true}); err != nil {
		t.Fatal(err)
	}
	raw, err := e.Action(context.Background(), "fronted", "unload_model", []byte(`{"model":"org/m"}`))
	if err != nil || !strings.Contains(string(raw), `"stopped":true`) {
		t.Fatalf("eject: %s %v", raw, err)
	}
	if up(backend) {
		t.Fatal("eject must free the engine's process")
	}
	if st, _ := e.Status("fronted"); !st.Running || !st.Idle {
		t.Fatalf("status = %+v, want the engine still ON, idle", st)
	}
	if got := statusCode(fmt.Sprintf("http://127.0.0.1:%d/health", public)); got != http.StatusOK {
		t.Fatalf("health after eject: %d", got)
	}
	raw, err = e.Action(context.Background(), "fronted", "unload_model", []byte(`{"model":"org/m"}`))
	if err != nil || !strings.Contains(string(raw), `"stopped":false`) {
		t.Fatalf("ejecting an unloaded engine is a no-op: %s %v", raw, err)
	}
	// Load of the model it is set to serve loads it again.
	if _, err := e.Action(context.Background(), "fronted", "load_model", []byte(`{"model":"org/m"}`)); err != nil {
		t.Fatalf("load: %v", err)
	}
	if !up(public) || !up(backend) {
		t.Fatal("load did not bring the model back")
	}
}

func TestIdleEngineStillAdvertisesItsModel(t *testing.T) {
	e, _, backend := frontExecutor(t, 0)
	if err := e.StartWith(context.Background(), "fronted", startOpts{Eager: true}); err != nil {
		t.Fatal(err)
	}
	res := e.ModelsResult(context.Background())
	if len(res.LoadedByEngine["fronted"]) == 0 {
		t.Fatalf("a loaded engine reports what is loaded: %+v", res)
	}
	if _, err := e.Action(context.Background(), "fronted", "unload_model", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if up(backend) {
		t.Fatal("not unloaded")
	}
	res = e.ModelsResult(context.Background())
	if got := res.ByEngine["fronted"]; len(got) != 1 || got[0] != "org/m" {
		t.Fatalf("advertised = %v, want the model it is set to serve, so requests reach it", got)
	}
	if loaded, ok := res.LoadedByEngine["fronted"]; !ok || len(loaded) != 0 {
		t.Fatalf("loaded = %v (%v), want queried and empty", loaded, ok)
	}
}

// A request through the proxy is routed by the node's advertised models, so the
// engine must never drop its model from the inventory while it unloads or loads:
// in that gap the proxy has no node for the model and refuses the request.
func TestTheModelStaysAdvertisedThroughAnUnloadAndALoad(t *testing.T) {
	t.Setenv("FAKE_START_DELAY", "800ms")
	e, public, backend := frontExecutor(t, 0)
	if err := e.StartWith(context.Background(), "fronted", startOpts{Eager: true}); err != nil {
		t.Fatal(err)
	}

	var missing atomic.Int32
	var samples atomic.Int32
	stop := make(chan struct{})
	var watchers sync.WaitGroup
	watchers.Add(1)
	go func() {
		defer watchers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			res := e.ModelsResult(context.Background())
			samples.Add(1)
			if got := res.ByEngine["fronted"]; len(got) == 0 {
				missing.Add(1)
			}
		}
	}()

	if _, err := e.Action(context.Background(), "fronted", "unload_model", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if got := post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", public)); got != http.StatusOK {
		t.Fatalf("the request that loads the engine answered %d", got)
	}
	close(stop)
	watchers.Wait()
	if !up(backend) {
		t.Fatal("not loaded")
	}
	if missing.Load() != 0 {
		t.Fatalf("the model was missing from the inventory in %d of %d samples during the unload and load", missing.Load(), samples.Load())
	}
}

func TestTheModelListIsAnsweredWhileUnloadedAndWhileLoading(t *testing.T) {
	t.Setenv("FAKE_START_DELAY", "1s")
	e, public, backend := frontExecutor(t, 0)
	if err := e.StartWith(context.Background(), "fronted", startOpts{Eager: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Action(context.Background(), "fronted", "unload_model", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	list := func() (int, string) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", public))
		if err != nil {
			return 0, ""
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if code, body := list(); code != http.StatusOK || !strings.Contains(body, `"id":"org/m"`) {
		t.Fatalf("while unloaded: %d %s, want the model it serves", code, body)
	}

	go post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", public)) // loads it
	time.Sleep(400 * time.Millisecond)
	if st, _ := e.Status("fronted"); !st.Running {
		t.Fatalf("status = %+v: an engine that is loading must not look stopped", st)
	}
	if code, body := list(); code != http.StatusOK || !strings.Contains(body, `"id":"org/m"`) {
		t.Fatalf("while loading: %d %s, want the model it serves", code, body)
	}
	eventually(t, "the load", 10*time.Second, func() bool { return up(backend) })
}

// The engine is not serving while it is unloaded or loading, and that is not a
// failure: the health check must not raise a warning for it. A warning is raised
// and then cleared when the load finishes, so it has to be caught while it stands.
func TestNoHealthWarningWhileUnloadedOrLoading(t *testing.T) {
	t.Setenv("FAKE_START_DELAY", "2500ms") // several health ticks long
	e, public, backend := frontExecutor(t, 0)
	if err := e.StartWith(context.Background(), "fronted", startOpts{Eager: true}); err != nil {
		t.Fatal(err)
	}

	var raised atomic.Value
	stop := make(chan struct{})
	var watcher sync.WaitGroup
	watcher.Add(1)
	go func() {
		defer watcher.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, problem := range e.Errors() {
				if problem.ID == unhealthyID("fronted") || problem.ID == exitedID("fronted") {
					raised.Store(problem.Message)
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	if _, err := e.Action(context.Background(), "fronted", "unload_model", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2200 * time.Millisecond) // unloaded across health ticks
	if got := post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", public)); got != http.StatusOK {
		t.Fatalf("the request that loads the engine answered %d", got)
	}
	close(stop)
	watcher.Wait()
	if !up(backend) {
		t.Fatal("not loaded")
	}
	if msg := raised.Load(); msg != nil {
		t.Fatalf("a warning was raised for an engine that was only unloaded or loading: %v", msg)
	}
	if st, _ := e.Status("fronted"); !st.Healthy {
		t.Fatalf("status = %+v, want healthy once loaded", st)
	}
}

// Start on an engine with a front brings it up with nothing loaded, as Ollama and
// llama.cpp start a server: the engine is ON and answers health checks, and the
// model loads when a request or a Load asks for it.
func TestStartIsLazy(t *testing.T) {
	log := filepath.Join(t.TempDir(), "starts.log")
	t.Setenv("FAKE_START_LOG", log)
	e, public, backend := frontExecutor(t, 0)
	start := time.Now()
	if err := e.Start(context.Background(), "fronted"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("a lazy start took %s", time.Since(start))
	}
	if startCount(t, log) != 0 || up(backend) {
		t.Fatal("a lazy start spawned the engine")
	}
	st, _ := e.Status("fronted")
	if !st.Running || !st.Healthy || !st.Idle || st.Port != public {
		t.Fatalf("status = %+v, want ON, healthy and idle on the public port", st)
	}
	if got := statusCode(fmt.Sprintf("http://127.0.0.1:%d/health", public)); got != http.StatusOK {
		t.Fatalf("health = %d", got)
	}
	if res := e.ModelsResult(context.Background()); len(res.ByEngine["fronted"]) != 1 || res.ByEngine["fronted"][0] != "org/m" {
		t.Fatalf("an idle engine advertises the model it serves: %+v", res)
	}
	if got := post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", public)); got != http.StatusOK || !up(backend) {
		t.Fatalf("the first request must load the engine: %d", got)
	}
	if got := startCount(t, log); got != 1 {
		t.Fatalf("starts = %d, want 1", got)
	}
}

func TestStopAfterALazyStartStopsAndStaysOff(t *testing.T) {
	e, public, backend := frontExecutor(t, 0)
	if err := e.Start(context.Background(), "fronted"); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop("fronted"); err != nil {
		t.Fatal(err)
	}
	if got := statusCode(fmt.Sprintf("http://127.0.0.1:%d/health", public)); got != 0 {
		t.Fatalf("the gateway outlived Stop (%d)", got)
	}
	if st, _ := e.Status("fronted"); st.Running || st.Idle {
		t.Fatalf("status = %+v", st)
	}
	if enabled, known, _ := e.desired.get("fronted"); !known || enabled {
		t.Fatalf("Stop must save OFF (enabled=%v known=%v)", enabled, known)
	}
	if up(backend) {
		t.Fatal("the engine is running")
	}
}

// After a restart of PAIR the engine comes back ON but idle, like the others,
// which start a server with nothing loaded.
func TestRestoreBringsAnEngineBackIdle(t *testing.T) {
	log := filepath.Join(t.TempDir(), "starts.log")
	t.Setenv("FAKE_START_LOG", log)
	e, public, backend := frontExecutor(t, 0)
	if err := e.setDesiredEnabled("fronted", true); err != nil {
		t.Fatal(err)
	}
	if err := e.RestoreEnabled(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, _ := e.Status("fronted")
	if !st.Running || !st.Idle || up(backend) || startCount(t, log) != 0 {
		t.Fatalf("status = %+v: a restore must not load the model", st)
	}
	if got := post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", public)); got != http.StatusOK {
		t.Fatalf("the first request after a restore answered %d", got)
	}
}

func TestLoadOnALazilyStartedEngineLoadsIt(t *testing.T) {
	e, _, backend := frontExecutor(t, 0)
	if err := e.Start(context.Background(), "fronted"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Action(context.Background(), "fronted", "load_model", []byte(`{"model":"org/m"}`)); err != nil {
		t.Fatal(err)
	}
	if !up(backend) {
		t.Fatal("Load did not load the model")
	}
	if st, _ := e.Status("fronted"); st.Idle {
		t.Fatalf("status = %+v, want loaded", st)
	}
}

// A launch that cannot work is reported at Start, even though nothing is loaded.
func TestLazyStartStillChecksTheLaunch(t *testing.T) {
	e, public, _ := frontExecutor(t, 0)
	squatter, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", public))
	if err != nil {
		t.Skipf("port taken: %v", err)
	}
	defer squatter.Close()
	if err := e.Start(context.Background(), "fronted"); err == nil {
		t.Fatal("a start onto an occupied port succeeded")
	}
}

func TestIdleTimeoutCanBeReadAndChangedAtRunTime(t *testing.T) {
	e, _, backend := frontExecutor(t, 0)
	m, _ := e.reg.Get("fronted")
	m.Actions["get_idle_timeout"] = Action{FrontIdle: "get"}
	m.Actions["set_idle_timeout"] = Action{FrontIdle: "set"}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	call := func(action, params string) (string, error) {
		raw, err := e.Action(context.Background(), "fronted", action, []byte(params))
		return string(raw), err
	}
	if raw, err := call("get_idle_timeout", `{}`); err != nil || raw != `{"seconds":0}` {
		t.Fatalf("get = %s %v, want the manifest's never", raw, err)
	}
	if err := e.StartWith(context.Background(), "fronted", startOpts{Eager: true}); err != nil {
		t.Fatal(err)
	}
	// Setting it unloads an engine that has already been idle that long, and
	// applies without a restart.
	if raw, err := call("set_idle_timeout", `{"seconds":1}`); err != nil || raw != `{"seconds":1}` {
		t.Fatalf("set = %s %v", raw, err)
	}
	eventually(t, "the unload under the new limit", 10*time.Second, func() bool { return !up(backend) })

	for _, bad := range []string{`{}`, `{"seconds":-1}`, `{"seconds":86401}`, `{"seconds":"soon"}`, `null`} {
		if _, err := call("set_idle_timeout", bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	// It was saved where the override file keeps the rest of the user's choices.
	data, err := os.ReadFile(filepath.Join(e.overrideDir, "fronted.json"))
	if err != nil || !strings.Contains(string(data), `"idle_timeout_s": 1`) {
		t.Fatalf("override = %s %v", data, err)
	}
	if raw, _ := call("set_idle_timeout", `{"seconds":0}`); raw != `{"seconds":0}` {
		t.Fatalf("never = %s", raw)
	}
}
