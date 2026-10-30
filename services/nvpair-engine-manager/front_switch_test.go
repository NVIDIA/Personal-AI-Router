// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// askFor posts a chat request naming model, and returns the status and body.
func askFor(t *testing.T, public int, path, model string) (int, string) {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"messages":[]}`, model)
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d%s", public, path), "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	answer, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(answer)
}

func startSwitchableEngine(t *testing.T) (e *Executor, public, backend int) {
	t.Helper()
	e, public, backend = frontExecutorAdvertising(t, 0, "")
	require.NoError(t, e.StartWith(context.Background(), "fronted", startOpts{Eager: true}))
	return e, public, backend
}

func servedModelOf(t *testing.T, e *Executor) string {
	t.Helper()
	state, err := e.LaunchSettings("fronted")
	require.NoError(t, err)
	tokens, err := parseLaunchText(state.LaunchText)
	require.NoError(t, err)
	model, _ := launchFlagValue(tokens, "--model")
	return model
}

func TestFrontSwitch_RequestForAnotherCachedModelSwitchesAndIsAnswered(t *testing.T) {
	e, public, _ := startSwitchableEngine(t)

	status, body := askFor(t, public, "/v1/chat/completions", "org/m")
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, `"served_by":"org/m"`)

	status, body = askFor(t, public, "/v1/chat/completions", "org/other")
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, `"served_by":"org/other"`, "the request is answered by the model it asked for")
	require.Equal(t, "org/other", servedModelOf(t, e), "the switch is saved, so the launch shows what is running")
	state, err := e.Status("fronted")
	require.NoError(t, err)
	require.True(t, state.Running && !state.Idle && state.Healthy, "status = %+v", state)
}

func TestFrontSwitch_RunningRequestFinishesBeforeTheModelIsUnloaded(t *testing.T) {
	_, public, _ := startSwitchableEngine(t)

	type answer struct {
		status int
		body   string
	}
	slow := make(chan answer, 1)
	go func() {
		status, body := askFor(t, public, "/v1/slow", "org/m")
		slow <- answer{status, body}
	}()
	time.Sleep(200 * time.Millisecond) // let it reach the engine

	status, body := askFor(t, public, "/v1/chat/completions", "org/other")
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, `"served_by":"org/other"`)

	select {
	case got := <-slow:
		require.Equal(t, http.StatusOK, got.status, "the request already running was cut off")
		require.Contains(t, got.body, `"served_by":"org/m"`, "it was answered by the model it started on")
	case <-time.After(5 * time.Second):
		t.Fatal("the running request never finished")
	}
}

func TestFrontSwitch_SimultaneousRequestsForOneNewModelShareOneSwitch(t *testing.T) {
	log := filepath.Join(t.TempDir(), "starts.log")
	t.Setenv("FAKE_START_LOG", log)
	t.Setenv("FAKE_START_DELAY", "500ms")
	_, public, _ := startSwitchableEngine(t)
	before := startCount(t, log)

	var wg sync.WaitGroup
	results := make([]string, 6)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, body := askFor(t, public, "/v1/chat/completions", "org/other")
			results[i] = fmt.Sprintf("%d %s", status, body)
		}()
	}
	wg.Wait()
	for i, got := range results {
		require.Contains(t, got, `200`, "request %d", i)
		require.Contains(t, got, `"served_by":"org/other"`, "request %d", i)
	}
	require.Equal(t, 1, startCount(t, log)-before, "one start for six simultaneous requests")
}

func TestFrontSwitch_ModelThatIsNotDownloadedIsLeftToTheEngine(t *testing.T) {
	e, public, _ := startSwitchableEngine(t)

	// An alias the engine was told to answer to, or a model nobody has: the
	// gateway cannot switch to it, so the engine decides. The fake engine answers.
	status, body := askFor(t, public, "/v1/chat/completions", "org/missing")
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, `"served_by":"org/m"`, "the engine kept serving the model it had")
	require.Equal(t, "org/m", servedModelOf(t, e), "nothing was switched or saved")
}

func TestFrontSwitch_FailedLoadLeavesTheEngineUnloadedAndNextRequestLoads(t *testing.T) {
	t.Setenv("FAKE_FAIL_MODEL", "org/bad")
	e, public, backend := startSwitchableEngine(t)

	status, body := askFor(t, public, "/v1/chat/completions", "org/bad")
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Contains(t, body, `could not switch to model \"org/bad\"`)
	require.Contains(t, body, "unloaded", "the message says the engine is left unloaded")

	state, err := e.Status("fronted")
	require.NoError(t, err)
	require.True(t, state.Running && state.Idle, "status = %+v, want ON and unloaded", state)
	require.False(t, up(backend), "nothing is loaded after a failed switch")
	require.Equal(t, "org/bad", servedModelOf(t, e), "the previous model is not put back")

	status, body = askFor(t, public, "/v1/chat/completions", "org/m")
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, `"served_by":"org/m"`, "the next request loads the model it names")
}

func TestFrontSwitch_UnloadedEngineLoadsTheRequestedModelDirectly(t *testing.T) {
	log := filepath.Join(t.TempDir(), "starts.log")
	t.Setenv("FAKE_START_LOG", log)
	e, public, backend := startSwitchableEngine(t)
	_, err := e.Action(context.Background(), "fronted", "unload_model", []byte(`{}`))
	require.NoError(t, err)
	require.False(t, up(backend))
	before := startCount(t, log)

	status, body := askFor(t, public, "/v1/chat/completions", "org/other")
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, `"served_by":"org/other"`)
	require.Equal(t, 1, startCount(t, log)-before, "one load, not the old model and then the new one")
}

func TestFrontSwitch_UnloadedEngineAdvertisesEveryCachedModel(t *testing.T) {
	e, _, backend := startSwitchableEngine(t)
	_, err := e.Action(context.Background(), "fronted", "unload_model", []byte(`{}`))
	require.NoError(t, err)
	require.False(t, up(backend))

	res := e.ModelsResult(context.Background())
	require.ElementsMatch(t, []string{"org/m", "org/other", "org/bad"}, res.ByEngine["fronted"])
	loaded, known := res.LoadedByEngine["fronted"]
	require.True(t, known)
	require.Empty(t, loaded, "the cached models are offered, none is resident")
}

func TestRequestedModel_ReadsTheModelAndLeavesTheBodyIntact(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		want        string
	}{
		{"model first", "application/json", `{"model":"org/a","messages":[{"role":"user","content":"hi"}]}`, "org/a"},
		{"model after other fields", "application/json; charset=utf-8", `{"messages":[{"role":"user","content":"hi"}],"stream":true,"model":"org/b"}`, "org/b"},
		{"no model field", "application/json", `{"messages":[]}`, ""},
		{"model is not a string", "application/json", `{"model":7}`, ""},
		{"not JSON content type", "text/plain", `{"model":"org/a"}`, ""},
		{"not an object", "application/json", `["org/a"]`, ""},
		{"truncated body", "application/json", `{"messages":[{"role":`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.contentType)

			require.Equal(t, tc.want, requestedModel(r))

			forwarded, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.Equal(t, tc.body, string(forwarded), "the engine must receive the body unchanged")
		})
	}
}

func TestRequestedModel_IgnoresRequestsThatDoNotNeedTheModel(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Content-Type", "application/json")
	require.Empty(t, requestedModel(r))
}

// listedIDs returns the model ids a gateway's /v1/models names.
func listedIDs(t *testing.T, public int) []string {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", public))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
	ids := make([]string, 0, len(list.Data))
	for _, entry := range list.Data {
		ids = append(ids, entry.ID)
	}
	return ids
}

func TestFrontSwitch_ModelListNamesEveryCachedModelWhileLoadedAndWhileUnloaded(t *testing.T) {
	e, public, backend := startSwitchableEngine(t)

	loaded := listedIDs(t, public)
	require.Subset(t, loaded, []string{"org/m", "org/other", "org/bad"}, "the engine's own list is extended")
	unique := map[string]bool{}
	for _, id := range loaded {
		unique[id] = true
	}
	require.Len(t, unique, len(loaded), "no model is listed twice")

	_, err := e.Action(context.Background(), "fronted", "unload_model", []byte(`{}`))
	require.NoError(t, err)
	require.False(t, up(backend))

	require.ElementsMatch(t, []string{"org/m", "org/other", "org/bad"}, listedIDs(t, public))
}

func TestFrontSwitch_ResidentModelListNamesOnlyTheLoadedModel(t *testing.T) {
	e, public, backend := startSwitchableEngine(t)

	resident := func() []string {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models?resident=1", public))
		require.NoError(t, err)
		defer resp.Body.Close()
		var list struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
		ids := []string{}
		for _, entry := range list.Data {
			ids = append(ids, entry.ID)
		}
		return ids
	}
	require.NotContains(t, resident(), "org/other", "a model that is only downloaded is not resident")
	require.NotContains(t, resident(), "org/bad")

	_, err := e.Action(context.Background(), "fronted", "unload_model", []byte(`{}`))
	require.NoError(t, err)
	require.False(t, up(backend))
	require.Equal(t, []string{"org/m"}, resident(), "an unloaded engine still names the model it will load")
}

func TestWithListedModels_AddsOnlyTheModelsTheListLacks(t *testing.T) {
	body := []byte(`{"object":"list","data":[{"id":"org/a","object":"model","max_model_len":65536}]}`)

	merged := withListedModels(body, []string{"org/a", "org/b", "org/b"})

	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID          string `json:"id"`
			MaxModelLen int    `json:"max_model_len"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(merged, &list))
	require.Equal(t, "list", list.Object)
	require.Len(t, list.Data, 2)
	require.Equal(t, "org/a", list.Data[0].ID)
	require.Equal(t, 65536, list.Data[0].MaxModelLen, "the engine's own entry is kept as it was")
	require.Equal(t, "org/b", list.Data[1].ID)

	require.Equal(t, `not json`, string(withListedModels([]byte(`not json`), []string{"org/b"})), "anything else passes through")
}

func TestFrontGateway_ReadsTheDownloadedModelsAgainOnlyWhenTheyAreStale(t *testing.T) {
	reads := 0
	g := &frontGateway{model: "org/m", modelsFn: func(context.Context) []string {
		reads++
		return []string{"org/m", "org/other"}
	}}

	for range 5 {
		require.True(t, g.canSwitchTo(context.Background(), "org/other"))
	}
	require.Equal(t, 1, reads, "repeated requests reuse one read of the cache")

	require.False(t, g.canSwitchTo(context.Background(), "org/m"), "the served model needs no switch")
	require.Equal(t, 1, reads)

	require.False(t, g.canSwitchTo(context.Background(), "alias"))
	require.False(t, g.canSwitchTo(context.Background(), "alias"))
	require.LessOrEqual(t, reads, 2, "an unknown name does not scan the cache on every request")
}

func TestFrontGateway_FailedReadKeepsTheLastListAndIsNotRetriedAtOnce(t *testing.T) {
	reads, fail := 0, false
	g := &frontGateway{model: "org/m", modelsFn: func(context.Context) []string {
		reads++
		if fail {
			return nil
		}
		return []string{"org/m", "org/other"}
	}}
	require.True(t, g.canSwitchTo(context.Background(), "org/other"))

	fail = true
	g.mu.Lock()
	g.list.at = time.Now().Add(-time.Minute) // the list has gone stale
	g.mu.Unlock()
	for range 4 {
		require.True(t, g.canSwitchTo(context.Background(), "org/other"), "a failed read does not forget the models")
	}
	require.Equal(t, 2, reads, "one failed read, then none until the next interval")
}

func TestFrontSwitch_ModelDeletedAfterTheListWasReadIsRefused(t *testing.T) {
	e, public, _ := startSwitchableEngine(t)
	require.Contains(t, listedIDs(t, public), "org/other") // the list is now cached

	st, err := e.state("fronted")
	require.NoError(t, err)
	cache := st.manifest.Actions["list_models"].HFCache.Dir
	require.NoError(t, os.RemoveAll(filepath.Join(cache, "models--org--other")))

	status, body := askFor(t, public, "/v1/chat/completions", "org/other")
	require.Equal(t, http.StatusNotFound, status, "the switch checks the cache itself, not the cached list")
	require.Contains(t, body, "not downloaded")
	require.Equal(t, "org/m", servedModelOf(t, e))
}

func TestFrontSwitch_DifferentModelsRequestedTogetherFromAnUnloadedEngineEachGetTheirOwn(t *testing.T) {
	e, public, backend := startSwitchableEngine(t)
	_, err := e.Action(context.Background(), "fronted", "unload_model", []byte(`{}`))
	require.NoError(t, err)
	require.False(t, up(backend))

	var wg sync.WaitGroup
	answers := make([]string, 2)
	for i, model := range []string{"org/m", "org/other"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, body := askFor(t, public, "/v1/chat/completions", model)
			answers[i] = fmt.Sprintf("%d %s", status, body)
		}()
	}
	wg.Wait()
	require.Contains(t, answers[0], `"served_by":"org/m"`)
	require.Contains(t, answers[1], `"served_by":"org/other"`)
}
