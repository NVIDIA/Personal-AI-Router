// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"nvpair-shared/parentwatch"
)

// Version is stamped at build time via -ldflags "-X main.Version=...".
var Version = "dev"

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// envInt reads an integer from the environment, so the count of resident models
// is configurable through the manifest's runtime.env — a map, which the per-user
// manifest override deep-merges cleanly, unlike an argv array which it would
// have to replace wholesale.
func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		slog.Warn("ignoring unusable value", "env", key, "value", v, "using", def)
	}
	return def
}

// defaultModelsDir is where a hand-built model most often lands. Scanned by
// default because a model you quantized yourself is invisible otherwise, and an
// absent directory costs one failed readdir.
func defaultModelsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "models")
}

// envDirs reads a colon-separated list, expanding a leading ~.
func envDirs(key, fallback string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		if fallback == "" {
			return nil
		}
		return []string{fallback}
	}
	var out []string
	for _, d := range strings.Split(raw, ":") {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if strings.HasPrefix(d, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				d = filepath.Join(home, d[2:])
			}
		}
		out = append(out, d)
	}
	return out
}

func defaultHFCache() string {
	if v := strings.TrimSpace(os.Getenv("HF_HOME")); v != "" {
		return filepath.Join(v, "hub")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "huggingface", "hub")
}

func main() {
	host := flag.String("host", "127.0.0.1", "listen address")
	port := flag.Int("port", 8081, "listen port: the single port PAIR treats as the MLX engine")
	maxModels := flag.Int("max-models", envInt("MLX_MAX_MODELS", 2),
		"how many models may be resident at once; the least recently used is evicted for a new one ($MLX_MAX_MODELS)")
	serverBin := flag.String("server-bin", "", "path to the mlx_lm.server entry point (required)")
	childBase := flag.Int("child-port-base", 8200, "first port to place a model server on")
	readyWait := flag.Duration("ready-timeout", 20*time.Minute, "how long a model may take to become ready")
	hfCache := flag.String("hf-cache", defaultHFCache(), "Hugging Face hub cache to list models from")
	var modelDirs multiFlag
	flag.Var(&modelDirs, "models-dir",
		"a directory of models kept outside the Hugging Face cache, advertised by absolute path (repeatable; $MLX_MODELS_DIRS, colon separated)")
	var extras multiFlag
	flag.Var(&extras, "extra-model",
		"a model outside the Hugging Face cache to advertise, e.g. a local directory (repeatable)")
	registeredFile := flag.String("registered-models-file", "",
		"file of model paths added through the UI, one per line, re-read per catalogue request")
	serverFlags := flag.String("server-flags", "", "extra flags passed to every mlx_lm.server child, space separated")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if strings.TrimSpace(*serverBin) == "" {
		slog.Error("--server-bin is required (the mlx_lm.server entry point inside the engine's virtualenv)")
		os.Exit(2)
	}
	if _, err := os.Stat(*serverBin); err != nil {
		slog.Error("mlx_lm.server not found", "path", *serverBin, "err", err)
		os.Exit(2)
	}

	pool := NewPool(*serverBin, strings.Fields(*serverFlags), *maxModels, *childBase, *readyWait)
	if len(modelDirs) == 0 {
		modelDirs = envDirs("MLX_MODELS_DIRS", defaultModelsDir())
	}
	srv := (&Server{pool: pool, hfCache: *hfCache, extra: extras, modelDirs: modelDirs,
		registeredFile: *registeredFile}).Listen(fmt.Sprintf("%s:%d", *host, *port))

	slog.Info("mlx-pool listening", "addr", srv.Addr, "max_models", *maxModels,
		"server_bin", *serverBin, "hf_cache", *hfCache, "extra_models", len(extras), "model_dirs", modelDirs, "version", Version)

	// Children hold gigabytes of weights, so shutdown stops them explicitly
	// rather than relying on the parent's exit to reap them.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// An orphaned pool is the costliest process here to leave behind: it holds a
	// model resident, so the weights stay in memory and :8081 stays bound until
	// someone finds it by hand. Losing the parent shuts it down exactly the way
	// SIGTERM does.
	defer parentwatch.Start("mlx-pool", stop)()
	go func() {
		<-ctx.Done()
		slog.Info("shutting down; stopping every model")
		pool.StopAll()
		shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	if err := srv.ListenAndServe(); err != nil && ctx.Err() == nil {
		slog.Error("server exited", "err", err)
		pool.StopAll()
		os.Exit(1)
	}
	pool.StopAll()
}
