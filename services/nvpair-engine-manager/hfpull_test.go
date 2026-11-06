// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSelectHFFiles(t *testing.T) {
	file := func(name string, size int64) hfSibling { return hfSibling{RFilename: name, Size: size} }
	t.Run("safetensors repo skips pickle weights and consolidated copies", func(t *testing.T) {
		files, total, err := selectHFFiles([]hfSibling{
			file("config.json", 10), file("tokenizer.json", 20),
			file("model-00001-of-00002.safetensors", 1000), file("model-00002-of-00002.safetensors", 1000),
			file("model.safetensors.index.json", 5),
			file("consolidated.safetensors", 2000), file("pytorch_model.bin", 2000),
			file("original/params.json", 5), file("model.gguf", 3000), file("-rf", 1), file("../escape", 1),
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"config.json", "tokenizer.json", "model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors", "model.safetensors.index.json"}
		if !slices.Equal(files, want) || total != 10+20+1000+1000+5 {
			t.Fatalf("files = %v total = %d", files, total)
		}
	})
	t.Run("a repo with only pickle weights keeps them", func(t *testing.T) {
		files, _, err := selectHFFiles([]hfSibling{file("config.json", 1), file("pytorch_model.bin", 9)})
		if err != nil || !slices.Contains(files, "pytorch_model.bin") {
			t.Fatalf("files = %v err = %v", files, err)
		}
	})
	t.Run("only a consolidated file is kept when nothing else exists", func(t *testing.T) {
		files, _, err := selectHFFiles([]hfSibling{file("params.json", 1), file("consolidated.safetensors", 9)})
		if err != nil || !slices.Contains(files, "consolidated.safetensors") {
			t.Fatalf("files = %v err = %v", files, err)
		}
	})
	t.Run("a GGUF-only repo is refused", func(t *testing.T) {
		if _, _, err := selectHFFiles([]hfSibling{file("config.json", 1), file("m.gguf", 9)}); err == nil {
			t.Fatal("expected an error: nothing vLLM can load")
		}
	})
}

// hfPullFixture builds an executor whose engine binary sits beside a fake `hf`
// tool, plus a fake Hugging Face API. script is the body of the fake tool.
func hfPullFixture(t *testing.T, siblings []hfSibling, apiStatus int, script string) (e *Executor, hub string, events *[]ProgressEvent) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the hf tool")
	}
	dir := t.TempDir()
	hub = filepath.Join(dir, "hub")
	bin := filepath.Join(dir, "vllm")
	for path, body := range map[string]string{bin: "#!/bin/sh\nexit 0\n", filepath.Join(dir, "hf"): "#!/bin/sh\n" + script} {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apiStatus != http.StatusOK {
			http.Error(w, "x", apiStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"siblings": siblings})
	}))
	t.Cleanup(api.Close)
	old := hfAPIBase
	hfAPIBase = api.URL
	t.Cleanup(func() { hfAPIBase = old })

	m := testEngineManifest(bin)
	m.Engine = "vllm"
	m.Actions = map[string]Action{"pull_model": {HFPull: &ActionHFPull{Tool: "hf", Dir: hub}}}
	reg := NewRegistry()
	reg.engines[m.Engine] = m
	var mu sync.Mutex
	collected := []ProgressEvent{}
	e = NewExecutor(reg, NewReporter(nil), func(method string, params any) {
		fields, ok := params.(map[string]any)
		if !ok || method != "engine:pull-progress" {
			return
		}
		ev := ProgressEvent{Stage: fields["stage"].(string)}
		if pct, ok := fields["percent"].(int); ok {
			ev.Percent = pct
		}
		mu.Lock()
		collected = append(collected, ev)
		mu.Unlock()
	}, t.TempDir())
	return e, hub, &collected
}

// The fake tool lays out a Hugging Face cache the way `hf download` does.
const fakeHFDownload = `
repo="$(echo "$@" | sed 's/.*--cache-dir //')/models--Qwen--Test"
mkdir -p "$repo/blobs" "$repo/snapshots/abc"
printf '{}' > "$repo/blobs/c"; printf '{}' > "$repo/blobs/t"; head -c 1000 /dev/zero > "$repo/blobs/w"
ln -s ../../blobs/c "$repo/snapshots/abc/config.json"
ln -s ../../blobs/t "$repo/snapshots/abc/tokenizer.json"
ln -s ../../blobs/w "$repo/snapshots/abc/model.safetensors"
`

func TestPullHFModelDownloadsAndReportsSuccess(t *testing.T) {
	e, hub, events := hfPullFixture(t, []hfSibling{
		{RFilename: "config.json", Size: 2}, {RFilename: "tokenizer.json", Size: 2}, {RFilename: "model.safetensors", Size: 1000},
	}, http.StatusOK, fakeHFDownload)

	res, err := e.PullModelStream(context.Background(), "vllm", "Qwen/Test", nil)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if !strings.Contains(string(res), "Qwen/Test") {
		t.Fatalf("result = %s", res)
	}
	models, err := scanHFCache(hub)
	if err != nil || len(models) != 1 || models[0].ID != "Qwen/Test" {
		t.Fatalf("the downloaded model must show in the cache list: %v %v", models, err)
	}
	last := (*events)[len(*events)-1]
	if last.Stage != "success" || last.Percent != 100 {
		t.Fatalf("last progress = %+v, want success at 100", last)
	}
}

func TestPullHFModelReportsAFailedDownload(t *testing.T) {
	e, _, _ := hfPullFixture(t, []hfSibling{{RFilename: "model.safetensors", Size: 10}}, http.StatusOK,
		`echo "connection reset by peer" >&2; exit 1`)
	_, err := e.PullModelStream(context.Background(), "vllm", "Qwen/Test", nil)
	if err == nil || !strings.Contains(err.Error(), "connection reset by peer") {
		t.Fatalf("err = %v, want the tool's own message", err)
	}
}

func TestPullHFModelRefusesBadInputsBeforeDownloading(t *testing.T) {
	cases := []struct {
		name, model string
		status      int
		want        string
	}{
		{"not a repo id", "../etc/passwd", http.StatusOK, "valid Hugging Face model id"},
		{"option-looking id", "--help", http.StatusOK, "valid Hugging Face model id"},
		{"unknown repo", "Qwen/Nope", http.StatusNotFound, "no such model"},
		{"gated repo", "meta/Gated", http.StatusForbidden, "HF_TOKEN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _ := hfPullFixture(t, nil, tc.status, "touch ran-anyway; exit 0")
			_, err := e.PullModelStream(context.Background(), "vllm", tc.model, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestPullHFModelStopsWhenCancelled(t *testing.T) {
	e, _, _ := hfPullFixture(t, []hfSibling{{RFilename: "model.safetensors", Size: 10}}, http.StatusOK, "sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := e.PullModelStream(ctx, "vllm", "Qwen/Test", nil)
	if err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("a cancelled download must stop promptly: err=%v after %s", err, time.Since(start))
	}
}
