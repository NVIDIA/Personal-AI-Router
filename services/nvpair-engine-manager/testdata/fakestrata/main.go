// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command fake-strata stands in for Strata's CLI in the engine-manager
// tests. It honours the contract the bundled strata.json pins: `launch`
// brings up an HTTP server on the requested port and exits immediately,
// like Strata's daemonizer; `launch --stop` stops it; `models list
// --output json` prints the recorded run configs, which needs no running
// server; and `install --family f --model m --yes` records a model
// without serving. The served model id is the placeholder lower(family-
// model) where Strata would report a run-config tag such as
// "qwen3.8-flash-next-q2_0". State lives under $HOME/.fake-strata so a
// test that redirects HOME sees an isolated install.
//
// POSIX only: detachment uses Setsid. It lives under testdata/ so the
// module's own `go build ./...` and `go vet ./...` skip it; the tests
// build it explicitly.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func stateDir() string   { return filepath.Join(os.Getenv("HOME"), ".fake-strata") }
func statePath() string  { return filepath.Join(stateDir(), "running.json") }
func modelsPath() string { return filepath.Join(stateDir(), "models.json") }

type runningState struct {
	Port int    `json:"port"`
	Name string `json:"name"`
}

func readModels() []string {
	data, err := os.ReadFile(modelsPath())
	if err != nil {
		return nil
	}
	var models []string
	if json.Unmarshal(data, &models) != nil {
		return nil
	}
	return models
}

func writeModels(models []string) {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		log.Fatal(err)
	}
	data, _ := json.Marshal(models)
	if err := os.WriteFile(modelsPath(), data, 0o644); err != nil {
		log.Fatal(err)
	}
}

func recordModel(name string) {
	for _, m := range readModels() {
		if m == name {
			return
		}
	}
	writeModels(append(readModels(), name))
}

// flagValue returns the value after the given flag, or "".
func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		log.Fatal("fake-strata: no subcommand")
	}
	switch args[0] {
	case "launch":
		for _, a := range args {
			if a == "--stop" {
				stop()
				return
			}
		}
		launch(args)
	case "serve":
		serve(args)
	case "models":
		listModels()
	case "install":
		install(args)
	default:
		log.Fatalf("fake-strata: unsupported invocation: %s", strings.Join(args, " "))
	}
}

// launch mirrors `strata launch ...`: it records the selected model,
// detaches a server for it, and returns without waiting.
func launch(args []string) {
	port := flagValue(args, "--port")
	family := flagValue(args, "--family")
	model := flagValue(args, "--model")
	if port == "" || family == "" || model == "" {
		log.Fatalf("fake-strata: launch needs --port --family --model, got: %s", strings.Join(args, " "))
	}
	name := strings.ToLower(family + "-" + model)
	recordModel(name)

	self, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	// The server must not inherit this launch command's stdout/stderr:
	// the manager reads those pipes to EOF, and a surviving grandchild
	// holding their write end open would hang it. Logs go to a file in
	// the state dir instead, which also leaves a breadcrumb when the
	// server fails to bind.
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		log.Fatal(err)
	}
	logFile, err := os.OpenFile(filepath.Join(stateDir(), "serve.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	child := exec.Command(self, "serve", "--port", port, "--name", name)
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	child.Stdin = nil
	child.Stdout = logFile
	child.Stderr = logFile
	if err := child.Start(); err != nil {
		log.Fatal(err)
	}
	_ = logFile.Close()
}

// serve is the detached HTTP server: /health for the readiness and health
// probes, /v1/models for loaded_models, /test-stop for `launch --stop`.
func serve(args []string) {
	port := flagValue(args, "--port")
	name := flagValue(args, "--name")
	if port == "" || name == "" {
		log.Fatal("fake-strata: serve needs --port and --name")
	}
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		log.Fatal(err)
	}
	data, _ := json.Marshal(runningState{Port: mustAtoi(port), Name: name})
	if err := os.WriteFile(statePath(), data, 0o644); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"` + name + `","object":"model"}]}`))
	})
	srv := &http.Server{Addr: "127.0.0.1:" + port, Handler: mux}
	mux.HandleFunc("/test-stop", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("stopping"))
		go func() {
			_ = os.Remove(statePath())
			_ = srv.Close()
		}()
	})
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// stop mirrors `strata launch --stop`: no arguments, so it finds the
// running server through the state file written under HOME.
func stop() {
	data, err := os.ReadFile(statePath())
	if err != nil {
		return // nothing running
	}
	var st runningState
	if json.Unmarshal(data, &st) == nil && st.Port != 0 {
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Post("http://127.0.0.1:"+strconv.Itoa(st.Port)+"/test-stop", "", nil)
		if err == nil {
			_ = resp.Body.Close()
		}
	}
	// Wait for the port to close so the manager's health poll sees a
	// stopped engine promptly.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		client := &http.Client{Timeout: 500 * time.Millisecond}
		resp, err := client.Get("http://127.0.0.1:" + strconv.Itoa(st.Port) + "/health")
		if err != nil {
			break
		}
		_ = resp.Body.Close()
		time.Sleep(200 * time.Millisecond)
	}
	_ = os.Remove(statePath())
}

func listModels() {
	models := readModels()
	var rows []map[string]any
	for _, m := range models {
		rows = append(rows, map[string]any{"id": m})
	}
	out, _ := json.Marshal(map[string]any{
		"schema_version": 1,
		"ok":             true,
		"models":         rows,
	})
	fmt.Println(string(out))
}

func install(args []string) {
	family := flagValue(args, "--family")
	model := flagValue(args, "--model")
	if family == "" || model == "" {
		log.Fatalf("fake-strata: install needs --family --model, got: %s", strings.Join(args, " "))
	}
	recordModel(strings.ToLower(family + "-" + model))
}

func mustAtoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
