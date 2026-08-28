// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"nvpair-shared/appdir"
	"nvpair-shared/applog"
)

func main() {
	ipcPath := flag.String("ipc", "", "IPC endpoint: Unix domain socket path or Windows named pipe (default: stdin/stdout)")
	scannerPath := flag.String("scanner-path", "", "path to nvpair-node-scanner binary (default: ./nvpair-node-scanner in the current working directory)")
	nodeInfoPath := flag.String("node-info-path", "", "path to nvpair-node-info binary (default: ./nvpair-node-info in the current working directory)")
	proxyPath := flag.String("proxy-path", "", "path to ollama-proxy binary (default: ./ollama-proxy in the current working directory)")
	lmstudioProxyPath := flag.String("lmstudio-proxy-path", "", "path to lmstudio-proxy binary (default: ./lmstudio-proxy in the current working directory)")
	workloadMgrPath := flag.String("workload-manager-path", "", "path to nvpair-workload-manager binary (default: ./nvpair-workload-manager in the current working directory)")
	errorsPath := flag.String("errors-path", "", "path to nvpair-errors binary (default: ./nvpair-errors in the current working directory)")
	engineMgrPath := flag.String("engine-manager-path", "", "path to nvpair-engine-manager binary (default: ./nvpair-engine-manager in the current working directory)")
	manualNodesPath := flag.String("manual-nodes-path", "", "path to nvpair-manual-nodes binary (default: ./nvpair-manual-nodes in the current working directory)")
	settingsPath := flag.String("settings-path", "", "path to nvpair-node-settings binary (default: ./nvpair-node-settings in the current working directory)")
	clusterMgrPath := flag.String("cluster-manager-path", "", "path to nvpair-cluster-manager binary (default: ./nvpair-cluster-manager in the current working directory)")
	schedulerPath := flag.String("scheduler-path", "", "path to nvpair-job-scheduler binary (default: ./nvpair-job-scheduler in the current working directory)")
	clusterDirFlag := flag.String("cluster-dir", "", "cluster config dir (node.crt/node.key + trusted/) the broker passes to its mDNS workers (nvpair-errors, nvpair-workload-manager, nvpair-node-info, nvpair-node-scanner, nvpair-manual-nodes) to enable cluster-scoped inter-node mTLS; defaults to the per-user Nvidia Corporation/Personal AI Router cluster/ dir, where nvpair-cluster-manager mints them")
	showVersion := flag.Bool("version", false, "print version and exit")
	resolveLevel := applog.RegisterFlag(nil, slog.LevelInfo)
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		os.Exit(0)
	}

	// Route this process's logs, and every worker's, through one non-blocking
	// sink so the parent's read rate can never hold a write open. Installed
	// before applog.Init because Init captures the writer.
	sink := newStderrSink(os.Stderr, defaultStderrSpillPath())
	stderrOut = sink
	applog.SetOutput(sink)
	defer sink.Close()

	applog.Init("nvpair-ui-broker", resolveLevel())

	// Fatal paths must not return through the async queue. applog bridges the
	// stdlib log package into slog, which now writes into the sink, and os.Exit
	// skips the deferred Close — so a stdlib fatal would enqueue its message, race
	// the drain goroutine, and normally lose, leaving the process to exit 1 with no
	// explanation at all. Flush before exiting instead. Close is once-guarded, so
	// the defer above stays correct as a no-op.
	fatalf := func(format string, a ...any) {
		slog.Error(fmt.Sprintf(format, a...))
		sink.Close()
		os.Exit(1)
	}

	// The inter-node workers do cluster-scoped mTLS off the cert + pins
	// nvpair-cluster-manager mints under the per-user data dir's cluster/ subtree.
	// Default the broker's view to that same path so mTLS turns on automatically
	// once this node is clustered, with nothing for the spawning UI to pass; an
	// explicit --cluster-dir overrides it.
	//
	// This directory is load-bearing in a way it did not used to be: it is the only
	// channel by which a cluster-scoped worker learns its membership, and the
	// inter-node data plane is mTLS only. A node that resolves no cluster dir
	// therefore exchanges no cluster traffic at all — it does not quietly fall back
	// to plain HTTP. Resolve it with the same chain nvpair-cluster-manager uses for
	// its own base (appdir, then next to the executable) so the writer and the
	// readers cannot pick different directories, and log the result once: this line
	// is the single diagnostic that tells a healthy-looking but isolated node apart
	// from a working one.
	clusterDir := *clusterDirFlag
	if clusterDir == "" {
		clusterDir = defaultClusterDir()
	}
	if clusterDir == "" {
		slog.Warn("no cluster directory could be resolved; this node cannot join or serve a cluster " +
			"(no per-user data dir and no executable path)")
	} else {
		slog.Info("resolved cluster directory", "clusterDir", clusterDir)
	}

	resolvedScanner, err := resolveScannerPath(*scannerPath)
	if err != nil {
		fatalf("scanner binary: %v", err)
	}

	// node-info is auxiliary: unlike the scanner, a missing binary is not
	// fatal. If the user passed an explicit --node-info-path we still
	// surface the failure loudly (it's a clear operator mistake) by
	// exiting; otherwise an absent default sibling just means the broker
	// runs without spawning a local node advertiser.
	resolvedNodeInfo, err := resolveNodeInfoPath(*nodeInfoPath)
	if err != nil {
		if *nodeInfoPath != "" {
			fatalf("node-info binary: %v", err)
		}
		slog.Warn("node-info binary not found; broker will run without local node advertisement", "err", err)
		resolvedNodeInfo = ""
	}

	// ollama-proxy is auxiliary too, resolved with the same rules: an
	// explicit --proxy-path that doesn't exist is a loud operator mistake
	// (fatal), but an absent default sibling just means the broker runs
	// without a local Ollama reverse proxy.
	resolvedProxy, err := resolveProxyPath(*proxyPath)
	if err != nil {
		if *proxyPath != "" {
			fatalf("proxy binary: %v", err)
		}
		slog.Warn("proxy binary not found; broker will run without local Ollama proxy", "err", err)
		resolvedProxy = ""
	}

	// lmstudio-proxy is auxiliary too, resolved with the same rules: an
	// explicit --lmstudio-proxy-path that doesn't exist is a loud operator
	// mistake (fatal), but an absent default sibling just means the broker
	// runs without a local LM Studio reverse proxy.
	resolvedLMStudioProxy, err := resolveLMStudioProxyPath(*lmstudioProxyPath)
	if err != nil {
		if *lmstudioProxyPath != "" {
			fatalf("lmstudio-proxy binary: %v", err)
		}
		slog.Warn("lmstudio-proxy binary not found; broker will run without local LM Studio proxy", "err", err)
		resolvedLMStudioProxy = ""
	}

	// nvpair-workload-manager is auxiliary too, resolved with the same rules:
	// an explicit --workload-manager-path that doesn't exist is a loud
	// operator mistake (fatal), but an absent default sibling just means
	// the broker runs without cluster workload relay.
	resolvedWorkloadMgr, err := resolveWorkloadManagerPath(*workloadMgrPath)
	if err != nil {
		if *workloadMgrPath != "" {
			fatalf("workload-manager binary: %v", err)
		}
		slog.Warn("workload-manager binary not found; broker will run without cluster workload relay", "err", err)
		resolvedWorkloadMgr = ""
	}

	// nvpair-errors is auxiliary too, resolved with the same rules: an
	// explicit --errors-path that doesn't exist is a loud operator mistake
	// (fatal), but an absent default sibling just means the broker runs
	// with the service-error pipeline disabled.
	resolvedErrors, err := resolveErrorsPath(*errorsPath)
	if err != nil {
		if *errorsPath != "" {
			fatalf("errors binary: %v", err)
		}
		slog.Warn("errors binary not found; broker will run with the error pipeline disabled", "err", err)
		resolvedErrors = ""
	}

	// nvpair-engine-manager, nvpair-manual-nodes, and nvpair-node-settings are
	// auxiliary too, resolved with the same rules: an explicit path that
	// doesn't exist is a loud operator mistake (fatal), but an absent
	// default sibling just means the broker runs without that worker.
	resolvedEngineMgr, err := resolveEngineManagerPath(*engineMgrPath)
	if err != nil {
		if *engineMgrPath != "" {
			fatalf("engine-manager binary: %v", err)
		}
		slog.Warn("engine-manager binary not found; broker will run without engine management", "err", err)
		resolvedEngineMgr = ""
	}

	resolvedManualNodes, err := resolveManualNodesPath(*manualNodesPath)
	if err != nil {
		if *manualNodesPath != "" {
			fatalf("manual-nodes binary: %v", err)
		}
		slog.Warn("manual-nodes binary not found; broker will run without manual nodes", "err", err)
		resolvedManualNodes = ""
	}

	resolvedSettings, err := resolveSettingsPath(*settingsPath)
	if err != nil {
		if *settingsPath != "" {
			fatalf("settings binary: %v", err)
		}
		slog.Warn("settings binary not found; broker will run without node settings", "err", err)
		resolvedSettings = ""
	}

	// nvpair-cluster-manager is auxiliary too, resolved with the same rules: an
	// explicit --cluster-manager-path that doesn't exist is a loud operator
	// mistake (fatal), but an absent default sibling just means the broker
	// runs without cluster pairing / membership (cluster:* and nodes:*).
	resolvedClusterMgr, err := resolveClusterManagerPath(*clusterMgrPath)
	if err != nil {
		if *clusterMgrPath != "" {
			fatalf("cluster-manager binary: %v", err)
		}
		slog.Warn("cluster-manager binary not found; broker will run without cluster pairing", "err", err)
		resolvedClusterMgr = ""
	}

	// nvpair-job-scheduler is auxiliary too, resolved with the same rules: an
	// explicit --scheduler-path that doesn't exist is a loud operator mistake
	// (fatal), but an absent default sibling just means the broker runs without
	// scheduler-driven proxy priority (proxies keep their own default ordering).
	resolvedScheduler, err := resolveSchedulerPath(*schedulerPath)
	if err != nil {
		if *schedulerPath != "" {
			fatalf("scheduler binary: %v", err)
		}
		slog.Warn("scheduler binary not found; broker will run without job scheduling", "err", err)
		resolvedScheduler = ""
	}

	var transport io.ReadWriteCloser
	if *ipcPath != "" {
		conn, err := dialIPC(*ipcPath)
		if err != nil {
			fatalf("failed to connect to IPC endpoint %q: %v", *ipcPath, err)
		}
		transport = conn
		log.Printf("using IPC transport: %s", *ipcPath)
	} else {
		transport = newStdioTransport()
		log.Print("using stdio transport")
	}
	defer transport.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-sigCh:
			slog.Info("received signal, shutting down", "signal", sig.String())
			cancel()
		case <-ctx.Done():
		}
	}()

	codec := NewCodec(transport)
	paths := workerPaths{
		scanner:       resolvedScanner,
		nodeInfo:      resolvedNodeInfo,
		proxy:         resolvedProxy,
		lmstudioProxy: resolvedLMStudioProxy,
		workloadMgr:   resolvedWorkloadMgr,
		errors:        resolvedErrors,
		engineMgr:     resolvedEngineMgr,
		manualNodes:   resolvedManualNodes,
		settings:      resolvedSettings,
		clusterMgr:    resolvedClusterMgr,
		scheduler:     resolvedScheduler,
		clusterDir:    clusterDir,
	}
	if err := NewBroker(codec, paths).Serve(ctx); err != nil && ctx.Err() == nil {
		fatalf("broker error: %v", err)
	}
	slog.Info("shutdown complete")
}

// defaultClusterDir resolves the cluster subtree this node's workers read and
// nvpair-cluster-manager writes, mirroring that manager's own base-dir chain
// (defaultBaseDir): the per-user data dir first, then next to the executable for
// dev builds. Mirroring it is the point — the two processes must never resolve
// different directories, because the manager would then pair into a tree no worker
// reads, leaving a node that looks clustered but exchanges nothing.
//
// Returns "" only when neither is available, which the caller reports; unlike the
// manager there is deliberately no "." tier, since a cwd-relative cluster identity
// would move with however the app was launched.
func defaultClusterDir() string {
	if dir, err := appdir.Path("cluster"); err == nil {
		return dir
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "cluster")
	}
	return ""
}

// resolveScannerPath honours the explicit --scanner-path override if
// provided; otherwise it expects ./nvpair-node-scanner (with the platform
// executable extension) sitting in the broker's current working
// directory. See resolveSiblingBinary for the shared resolution rules.
func resolveScannerPath(override string) (string, error) {
	return resolveSiblingBinary(override, "nvpair-node-scanner", "--scanner-path")
}

// resolveNodeInfoPath mirrors resolveScannerPath for the nvpair-node-info
// binary the broker supervises. Its result is optional at the call site:
// a not-found default sibling degrades to "no local node advertisement"
// rather than aborting the broker.
func resolveNodeInfoPath(override string) (string, error) {
	return resolveSiblingBinary(override, "nvpair-node-info", "--node-info-path")
}

// resolveProxyPath mirrors resolveNodeInfoPath for the ollama-proxy binary
// the broker supervises. Like node-info its result is optional at the call
// site: a not-found default sibling degrades to "no local Ollama proxy"
// rather than aborting the broker.
func resolveProxyPath(override string) (string, error) {
	return resolveSiblingBinary(override, "ollama-proxy", "--proxy-path")
}

// resolveLMStudioProxyPath mirrors resolveProxyPath for the lmstudio-proxy
// binary the broker supervises. Like the Ollama proxy its result is optional
// at the call site: a not-found default sibling degrades to "no local LM
// Studio proxy" rather than aborting the broker.
func resolveLMStudioProxyPath(override string) (string, error) {
	return resolveSiblingBinary(override, "lmstudio-proxy", "--lmstudio-proxy-path")
}

// resolveWorkloadManagerPath mirrors resolveProxyPath for the
// nvpair-workload-manager binary the broker supervises. Like node-info and the
// proxy its result is optional at the call site: a not-found default sibling
// degrades to "no cluster workload relay" rather than aborting the broker.
func resolveWorkloadManagerPath(override string) (string, error) {
	return resolveSiblingBinary(override, "nvpair-workload-manager", "--workload-manager-path")
}

// resolveErrorsPath mirrors resolveProxyPath for the nvpair-errors binary the
// broker supervises. Like the other auxiliaries its result is optional at
// the call site: a not-found default sibling degrades to "no service-error
// pipeline" rather than aborting the broker.
func resolveErrorsPath(override string) (string, error) {
	return resolveSiblingBinary(override, "nvpair-errors", "--errors-path")
}

// resolveEngineManagerPath / resolveManualNodesPath / resolveSettingsPath /
// resolveClusterManagerPath mirror resolveErrorsPath for the remaining
// adopted workers. Each result is optional at the call site: a not-found
// default sibling degrades to "without that worker" rather than aborting
// the broker.
func resolveEngineManagerPath(override string) (string, error) {
	return resolveSiblingBinary(override, "nvpair-engine-manager", "--engine-manager-path")
}

func resolveManualNodesPath(override string) (string, error) {
	return resolveSiblingBinary(override, "nvpair-manual-nodes", "--manual-nodes-path")
}

func resolveSettingsPath(override string) (string, error) {
	return resolveSiblingBinary(override, "nvpair-node-settings", "--settings-path")
}

func resolveClusterManagerPath(override string) (string, error) {
	return resolveSiblingBinary(override, "nvpair-cluster-manager", "--cluster-manager-path")
}

func resolveSchedulerPath(override string) (string, error) {
	return resolveSiblingBinary(override, "nvpair-job-scheduler", "--scheduler-path")
}

// resolveSiblingBinary locates a worker binary the broker spawns. It
// honours an explicit override if provided; otherwise it expects the
// binary (with the platform executable extension) sitting in the broker's
// current working directory. We deliberately do NOT fall back to PATH
// lookup or sibling directories — the broker should run alongside the
// other NVPAIR binaries in a known layout, and a confusing PATH match for a
// stale dev build is worse than a clear "not found" error. flagName is
// the override flag surfaced in the not-found error so the message points
// at the right knob.
func resolveSiblingBinary(override, name, flagName string) (string, error) {
	if override != "" {
		if _, err := os.Stat(override); err != nil {
			return "", fmt.Errorf("%s binary %q not accessible: %w", name, override, err)
		}
		return override, nil
	}
	bin := name
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get cwd: %w", err)
	}
	candidate := filepath.Join(cwd, bin)
	if _, err := os.Stat(candidate); err != nil {
		return "", fmt.Errorf("%s not found in %s (use %s to override): %w", bin, cwd, flagName, err)
	}
	return candidate, nil
}
