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
	"runtime"
	"slices"
	"strconv"
	"strings"
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
	if front := vllm.Platforms["linux/amd64"].Runtime.Front; front == nil || front.BackendPortOffset != 10000 {
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
func frontExecutor(t *testing.T) (e *Executor, public, backend int) {
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
	p.Runtime.Args = []string{"serve", "--listen", "{host}:{port}"}
	p.Runtime.EditableLaunch = &EditableLaunch{FixedArgs: []string{"serve"}, Controls: []LaunchControl{
		{Flags: []string{"--listen"}, Value: "{server.host}:{server.port}"},
	}}
	p.Runtime.Front = &Front{BackendPortOffset: 1}
	m.Platforms[key] = p
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
	e, public, backend := frontExecutor(t)
	if err := e.Start(context.Background(), "fronted"); err != nil {
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
	if err := e.Start(context.Background(), "fronted"); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if got := statusCode(fmt.Sprintf("http://127.0.0.1:%d/api/tags", public)); got != http.StatusOK {
		t.Fatalf("after restart the public port answered %d", got)
	}
}

func TestFrontedEngineGatewayGoesWithACrashedEngine(t *testing.T) {
	e, public, _ := frontExecutor(t)
	if err := e.Start(context.Background(), "fronted"); err != nil {
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
	e, public, backend := frontExecutor(t)
	squatter, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", backend))
	if err != nil {
		t.Skipf("backend port taken: %v", err)
	}
	defer squatter.Close()

	err = e.Start(context.Background(), "fronted")
	if err == nil || !strings.Contains(err.Error(), "in use by another program") {
		t.Fatalf("err = %v, want the occupied internal port reported", err)
	}
	if got := statusCode(fmt.Sprintf("http://127.0.0.1:%d/", public)); got != 0 {
		t.Fatalf("a failed start left a gateway on the public port (%d)", got)
	}
}
