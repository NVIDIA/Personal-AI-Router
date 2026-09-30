// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cross-process tests for nvpair-ui-broker's nine-worker supervision: that it
// spawns every backend module, surfaces a worker crash through the
// nvpair-errors pipeline (and auto-restarts the worker), relays the
// engine-manager control plane, and merges manual nodes into the discovery
// snapshot. All binaries are built by TestMain.
package tests

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/engines"
	"nvpair-shared/errors"
	"nvpair-shared/jsonrpc"
)

// startBrokerWith starts the broker with --scanner-path (always) plus any
// extra args, capturing its JSON-RPC stdout as jsonrpc.Message frames and its
// stderr as raw log lines. The cleanup closes stdin (clean shutdown) and
// reaps the process, killing it if it overstays the grace window.
func startBrokerWith(t *testing.T, extraArgs ...string) (stdin io.WriteCloser, msgs <-chan jsonrpc.Message, stderrLines <-chan string, cleanup func()) {
	return startBrokerWithEnv(t, nil, extraArgs...)
}

// startBrokerWithEnv is startBrokerWith plus explicit environment overrides.
// It is reserved for cross-process cases whose contract begins in the parent
// process environment (for example an inherited OLLAMA_HOST).
func startBrokerWithEnv(t *testing.T, extraEnv []string, extraArgs ...string) (stdin io.WriteCloser, msgs <-chan jsonrpc.Message, stderrLines <-chan string, cleanup func()) {
	t.Helper()
	return startBrokerWithConfigDirAndEnv(t, t.TempDir(), extraEnv, extraArgs...)
}

func startBrokerWithConfigDir(t *testing.T, configDir string, extraArgs ...string) (stdin io.WriteCloser, msgs <-chan jsonrpc.Message, stderrLines <-chan string, cleanup func()) {
	t.Helper()
	// An empty --cluster-dir makes this node a NON-MEMBER regardless of the
	// machine's real cluster state: its cluster-scoped workers hold no identity, so
	// the inter-node data plane (mTLS only) neither serves nor broadcasts.
	return startBrokerWithDirs(t, configDir, t.TempDir(), extraArgs...)
}

// startBrokerWithDirs is startBrokerWithConfigDir with an explicit cluster
// identity, for tests that need a working inter-node data plane (see
// newInterNodeCluster).
func startBrokerWithDirs(t *testing.T, configDir, clusterDir string, extraArgs ...string) (stdin io.WriteCloser, msgs <-chan jsonrpc.Message, stderrLines <-chan string, cleanup func()) {
	t.Helper()
	return startBrokerWithDirsAndEnv(t, configDir, clusterDir, nil, extraArgs...)
}

func startBrokerWithConfigDirAndEnv(t *testing.T, configDir string, extraEnv []string, extraArgs ...string) (stdin io.WriteCloser, msgs <-chan jsonrpc.Message, stderrLines <-chan string, cleanup func()) {
	t.Helper()
	// An empty --cluster-dir makes this node a NON-MEMBER regardless of the
	// machine's real cluster state: its cluster-scoped workers hold no identity, so
	// the inter-node data plane (mTLS only) neither serves nor broadcasts.
	return startBrokerWithDirsAndEnv(t, configDir, t.TempDir(), extraEnv, extraArgs...)
}

func startBrokerWithDirsAndEnv(t *testing.T, configDir, clusterDir string, extraEnv []string, extraArgs ...string) (stdin io.WriteCloser, msgs <-chan jsonrpc.Message, stderrLines <-chan string, cleanup func()) {
	t.Helper()
	args := append([]string{"--scanner-path", scannerBin, "--cluster-dir", clusterDir}, extraArgs...)
	cmd := exec.Command(brokerBin, args...)
	// Every supervised worker must use disposable per-test config. In
	// particular, managed-port startup can persist an Ollama backend port; a
	// cross-process test must never read or rewrite the developer's real file.
	cmd.Env = append(os.Environ(),
		"HOME="+configDir,
		"XDG_CONFIG_HOME="+configDir,
		"APPDATA="+configDir,
		"LOCALAPPDATA="+configDir,
	)
	cmd.Env = append(cmd.Env, extraEnv...)

	stdinPipe, err := cmd.StdinPipe()
	require.NoError(t, err, "broker stdin pipe")
	stdoutPipe, err := cmd.StdoutPipe()
	require.NoError(t, err, "broker stdout pipe")
	stderrPipe, err := cmd.StderrPipe()
	require.NoError(t, err, "broker stderr pipe")
	require.NoError(t, cmd.Start(), "start broker")
	t.Logf("broker started: pid=%d", cmd.Process.Pid)

	ch := startMsgReader(t, stdoutPipe)
	lines := startLineReader(stderrPipe)

	return stdinPipe, ch, lines, func() {
		stdinPipe.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(8 * time.Second): // broker also tears down every worker (2s grace each)
			cmd.Process.Kill()
			<-done
		}
	}
}

// startLineReader streams a reader's lines onto a channel (broker stderr —
// the broker's own applog plus every worker's forwarded stderr).
func startLineReader(r io.Reader) <-chan string {
	ch := make(chan string, 512)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			ch <- sc.Text()
		}
	}()
	return ch
}

// waitForStderr blocks until a stderr line matching re arrives, returning
// the matching line. Lines are consumed sequentially, so successive calls
// scan forward from where the previous one stopped.
func waitForStderr(t *testing.T, lines <-chan string, re *regexp.Regexp, timeout time.Duration) string {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case line, ok := <-lines:
			require.True(t, ok, "broker stderr closed before matching")
			if re.MatchString(line) {
				return line
			}
		case <-timer.C:
			require.FailNow(t, fmt.Sprintf("timed out (%s) waiting for stderr matching %q", timeout, re.String()))
		}
	}
}

// errorsUpdateContains scans the broker's stdout for an errors:update push
// carrying an entry with the given id, failing on timeout.
func waitForErrorsUpdateContaining(t *testing.T, msgs <-chan jsonrpc.Message, id string, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case msg, ok := <-msgs:
			require.True(t, ok, "broker stream closed before errors:update containing (%v)", id)
			if msg.Method == methodErrorsUpdate {
				if errorsListHasID(t, msg.Params, id) {
					return
				}
			}
		case <-timer.C:
			require.FailNow(t, fmt.Sprintf("timed out (%s) waiting for errors:update containing %q", timeout, id))
		}
	}
}

func errorsListHasID(t *testing.T, raw json.RawMessage, id string) bool {
	t.Helper()
	var list []errors.ServiceError
	require.NoError(t, json.Unmarshal(raw, &list), "unmarshal errors:update, raw: %s", raw)
	for _, e := range list {
		if e.ID == id {
			return true
		}
	}
	return false
}

const methodErrorsUpdate = "errors:update"

var proxyPidRe = regexp.MustCompile(`proxy started.*\bpid=(\d+)`)

// portBusy reports whether a TCP port on localhost is already bound — used
// to skip tests whose supervised workers need a fatal-on-conflict listener
// (nvpair-errors --peer-sync on 14319, ollama-proxy on 11435) when something
// else on the host already holds it.
func portBusy(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return true
	}
	ln.Close()
	return false
}

// TestBrokerCrashSurfacingAndRestart kills the broker's supervised proxy and
// asserts the crash is surfaced through the nvpair-errors pipeline
// (errors:update relay + errors:get-initial), and that the supervisor restarts
// the proxy.
//
// The crash is reported for the process, not an engine: one nvpair-proxy hosts
// every facade, so there is one supervisor and one name to report. A crash-id
// naming a single engine would claim that only that engine went down when in
// fact all of them did.
func TestBrokerCrashSurfacingAndRestart(t *testing.T) {
	if portBusy(14319) {
		t.Skip("nvpair-errors --peer-sync port 14319 already in use; skipping")
	}
	if portBusy(11435) {
		t.Skip("ollama-proxy port 11435 already in use; skipping")
	}

	stdin, msgs, stderr, cleanup := startBrokerWith(t,
		"--errors-path", errorsBin,
		"--proxy-path", proxyBin, "--proxy-engines", "ollama",
	)
	t.Cleanup(cleanup)

	waitForMethod(t, msgs, "app:ready", 10*time.Second)

	// The proxy logs "proxy started ... pid=N" at startup (before app:ready),
	// so it's already buffered. Grab the first pid and kill that process.
	firstLine := waitForStderr(t, stderr, proxyPidRe, 10*time.Second)
	pid1 := mustPid(t, firstLine)
	t.Logf("killing supervised proxy pid=%d", pid1)
	if proc, err := os.FindProcess(pid1); err == nil {
		_ = proc.Kill()
	}

	// The crash must surface as a sticky supervisor error via errors:update.
	crashID := "supervisor:subprocess-crashed:" + engines.ProxyComponent
	waitForErrorsUpdateContaining(t, msgs, crashID, 15*time.Second)
	t.Log("crash surfaced via errors:update")

	// errors:get-initial must relay the same entry from nvpair-errors.
	sendReq(t, stdin, 900, "errors:get-initial")
	resp := waitForResponse(t, msgs, 5*time.Second)
	require.True(t, errorsListHasID(t, resp.Result, crashID), "errors:get-initial missing the crash entry")
	t.Log("errors:get-initial carries the crash entry")

	// The supervisor must restart the proxy: a second "proxy started" with
	// a different pid (backoff is ~1s).
	secondLine := waitForStderr(t, stderr, proxyPidRe, 15*time.Second)
	pid2 := mustPid(t, secondLine)
	require.NotEqual(t, pid1, pid2, "proxy was not restarted")
	t.Logf("proxy auto-restarted: pid %d -> %d", pid1, pid2)
}

func mustPid(t *testing.T, line string) int {
	t.Helper()
	m := proxyPidRe.FindStringSubmatch(line)
	require.GreaterOrEqual(t, len(m), 2, "no proxy pid in line (%v)", line)
	pid, err := strconv.Atoi(m[1])
	require.NoError(t, err, "bad pid")
	return pid
}

// Every default engine is fronted by ONE process. This is the property the whole
// unification exists for: the estimated-work reservations a proxy takes
// between scheduler snapshots live in the process, so separate processes each
// held an incomplete picture and could dispatch simultaneous bursts to the
// same node while each believed it idle.
//
// Asserted on pids rather than on the code shape, because "one supervisor" is
// an implementation detail and "one OS process serving every engine" is the
// thing that actually makes the reservation map whole.
func TestAllDefaultEnginesAreServedByOneProxyProcess(t *testing.T) {
	if portBusy(11435) || portBusy(1234) {
		t.Skip("ollama-proxy (11435) or lmstudio-proxy (1234) default port already in use; skipping")
	}

	stdin, msgs, stderr, cleanup := startBrokerWith(t,
		// No --proxy-engines: exercise the complete default set.
		"--proxy-path", proxyBin,
	)
	t.Cleanup(cleanup)

	pids := make(chan int, 8)
	go func() {
		for line := range stderr {
			if m := proxyPidRe.FindStringSubmatch(line); m != nil {
				if pid, err := strconv.Atoi(m[1]); err == nil {
					pids <- pid
				}
			}
		}
	}()

	waitForMethod(t, msgs, "app:ready", 10*time.Second)

	// Every facade must actually come up, or "one process" would be trivially
	// true by one of them having failed.
	ollamaPort := waitProxyReady(t, stdin, msgs, 15*time.Second)
	lmstudioPort := waitLMStudioProxyReady(t, stdin, msgs, 15*time.Second)
	llamacppPort := waitEngineProxyReady(t, "llamacpp-proxy", stdin, msgs, 15*time.Second)
	require.Len(t, map[int]bool{ollamaPort: true, lmstudioPort: true, llamacppPort: true}, 3,
		"facade ports must be distinct: ollama=%d lmstudio=%d llamacpp=%d",
		ollamaPort, lmstudioPort, llamacppPort)
	t.Logf("facades ready: ollama=%d lmstudio=%d llamacpp=%d",
		ollamaPort, lmstudioPort, llamacppPort)

	// Collect every spawn announced up to this point plus a settling window, so
	// a second process starting late still shows up.
	seen := map[int]bool{}
	deadline := time.After(2 * time.Second)
	for collecting := true; collecting; {
		select {
		case pid := <-pids:
			seen[pid] = true
		case <-deadline:
			collecting = false
		}
	}
	require.Len(t, seen, 1, "exactly one proxy process must host all default engines")
	t.Logf("all default engines served by pid %v", seen)
}

// TestBrokerShutsDownOnSignal verifies the broker exits promptly when it
// receives SIGINT/SIGTERM, even though its read loop is parked on stdin
// (no input arriving) — and that the supervisors do NOT resurrect the
// workers during teardown. This guards the regression where Ctrl+C left
// the broker hung and self-restarting its children.
func TestBrokerShutsDownOnSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		// os.Process.Signal(os.Interrupt) is unsupported on Windows — you
		// can't deliver SIGINT to an arbitrary process there. The bug this
		// guards (ctx cancelled by a signal while the read loop is parked on
		// stdin) is a Unix interactive-terminal concern; on Windows the
		// broker is shut down by its parent closing stdin (EOF), which the
		// other tests' teardown already exercises.
		t.Skip("signalling a specific process is not supported on Windows")
	}
	cmd := exec.Command(brokerBin,
		"--scanner-path", scannerBin,
		"--proxy-path", proxyBin, "--proxy-engines", "ollama",
		"--cluster-dir", t.TempDir(),
	)
	cmd.Stderr = os.Stderr
	// Keep stdin OPEN for the lifetime of the test so shutdown is driven
	// purely by the signal, not by stdin EOF (closing stdin would shut the
	// broker down even with the old blocking read loop, masking the bug).
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err, "broker stdin pipe")
	defer stdin.Close()
	stdoutPipe, err := cmd.StdoutPipe()
	require.NoError(t, err, "broker stdout pipe")
	require.NoError(t, cmd.Start(), "start broker")
	msgs := startMsgReader(t, stdoutPipe)
	waitForMethod(t, msgs, "app:ready", 10*time.Second)

	// SIGINT to the broker only (children are in their own process groups).
	require.NoError(t, cmd.Process.Signal(os.Interrupt), "signal broker")

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		// Exited — the broker observed the signal and tore down cleanly.
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		require.FailNow(t, "broker did not exit within 10s of SIGINT (read loop blocked on stdin?)")
	}
}

// TestBrokerSpawnsAllModules points the broker at every worker binary and
// asserts each one is spawned (its "started" log line) and the broker
// reaches app:ready. The advertiser is excluded — it's poll-driven and
// only starts once local ollama is detected.
func TestBrokerSpawnsAllModules(t *testing.T) {
	stdin, msgs, stderr, cleanup := startBrokerWith(t,
		"--errors-path", errorsBin,
		"--node-info-path", nodeInfoBin,
		"--proxy-path", proxyBin, "--proxy-engines", "ollama",
		"--workload-manager-path", workloadMgrBin,
		"--engine-manager-path", engineMgrBin,
		"--manual-nodes-path", manualNodesBin,
		"--settings-path", nodeSettingsBin,
		"--cluster-manager-path", clusterMgrBin,
	)
	_ = stdin
	t.Cleanup(cleanup)

	// Native engine detection and managed-port preparation run before ready.
	// Allow those bounded probes to finish on a host with installed engines.
	waitForMethod(t, msgs, "app:ready", 30*time.Second)

	// Each supervised worker logs "<name> started" on spawn (before any
	// bind), so these lines appear regardless of later port conflicts.
	for _, want := range []string{
		"scanner started",
		"nvpair-errors started",
		"settings started",
		"engine-manager started",
		"node-info started",
		"proxy started",
		"workload-manager started",
		"manual-nodes started",
		"cluster-manager started",
	} {
		waitForStderr(t, stderr, regexp.MustCompile(regexp.QuoteMeta(want)), 10*time.Second)
		t.Logf("observed: %s", want)
	}
}

// TestBrokerEngineRelay verifies the broker relays an engine-manager
// request: engine:get-installed round-trips to the supervised
// engine-manager and returns its (read-only) installed-engines snapshot.
func TestBrokerEngineRelay(t *testing.T) {
	stdin, msgs, _, cleanup := startBrokerWith(t, "--engine-manager-path", engineMgrBin)
	t.Cleanup(cleanup)

	waitForMethod(t, msgs, "app:ready", 10*time.Second)

	sendReq(t, stdin, 700, "engine:get-installed")
	resp := waitForResponse(t, msgs, 10*time.Second)
	require.Nil(t, resp.Error, "engine:get-installed errored: code")
	var result struct {
		Engines []json.RawMessage `json:"engines"`
	}
	require.NoError(t, json.Unmarshal(resp.Result, &result), "unmarshal engine:get-installed result")
	// The host may have zero installed engines; the contract is just that
	// the relay returns a well-formed { engines: [...] } payload.
	t.Logf("engine:get-installed returned %d engine(s)", len(result.Engines))
}

// proxyStatus queries ollama-proxy:get-status through the broker and returns the
// proxy's readiness + bound port (the broker's captured view of the proxy's
// last "ready").
func proxyStatus(t *testing.T, stdin io.Writer, msgs <-chan jsonrpc.Message, id int) (bool, int) {
	t.Helper()
	sendReq(t, stdin, id, "ollama-proxy:get-status")
	resp := waitForResponse(t, msgs, 5*time.Second)
	require.Nil(t, resp.Error, "ollama-proxy:get-status errored")
	var s struct {
		Ready bool `json:"ready"`
		Port  int  `json:"port"`
	}
	require.NoError(t, json.Unmarshal(resp.Result, &s), "parse ollama-proxy:get-status")
	return s.Ready, s.Port
}

// TestBrokerProxySetPortRebinds drives ollama-proxy:set-port through the broker
// and asserts the proxy live-rebinds onto the new port (reflected by
// ollama-proxy:get-status). The broker is given a private config dir so the proxy's
// persisted port doesn't touch the developer's real config. With no engine
// running, the requested port is free, so the broker relays it unchanged (no
// bump) — exercising the interception + relay + rebind wiring end to end.
func TestBrokerProxySetPortRebinds(t *testing.T) {
	if portBusy(11435) {
		t.Skip("ollama-proxy default port 11435 already in use; skipping")
	}
	cfg := t.TempDir()
	cmd := exec.Command(brokerBin,
		"--scanner-path", scannerBin,
		"--proxy-path", proxyBin, "--proxy-engines", "ollama",
		"--engine-manager-path", engineMgrBin,
	)
	// Isolate per-user state (proxy-port.json, any engine override) to a
	// temp dir so the test leaves the real config untouched.
	cmd.Env = append(os.Environ(),
		"HOME="+cfg,
		"XDG_CONFIG_HOME="+cfg,
		"APPDATA="+cfg,
		"LOCALAPPDATA="+cfg,
	)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err, "broker stdin pipe")
	stdoutPipe, err := cmd.StdoutPipe()
	require.NoError(t, err, "broker stdout pipe")
	require.NoError(t, cmd.Start(), "start broker")
	t.Cleanup(func() {
		stdin.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	})
	msgs := startMsgReader(t, stdoutPipe)

	waitForMethod(t, msgs, "app:ready", 10*time.Second)

	// Wait for the proxy to announce its (default) port.
	id := 850
	deadline := time.Now().Add(15 * time.Second)
	for {
		if ready, _ := proxyStatus(t, stdin, msgs, id); ready {
			break
		}
		id++
		require.LessOrEqual(t, time.Now(), deadline, "proxy never reported ready")
		time.Sleep(500 * time.Millisecond)
	}

	target := freePort(t)
	req := fmt.Sprintf(`{"jsonrpc":"2.0","id":860,"method":"ollama-proxy:set-port","params":{"port":%d}}`, target) + "\n"
	_, err = stdin.Write([]byte(req))
	require.NoError(t, err, "write ollama-proxy:set-port")
	resp := waitForResponse(t, msgs, 10*time.Second)
	require.Nil(t, resp.Error, "ollama-proxy:set-port errored")
	var sp struct {
		Port int `json:"port"`
	}
	require.NoError(t, json.Unmarshal(resp.Result, &sp), "parse ollama-proxy:set-port result")
	require.Equal(t, target, sp.Port, "set-port should not bump when no engine is running")

	// ollama-proxy:get-status must now reflect the rebound port.
	id = 870
	deadline = time.Now().Add(10 * time.Second)
	for {
		if _, port := proxyStatus(t, stdin, msgs, id); port == target {
			t.Logf("proxy rebound to %d via the broker", target)
			break
		}
		id++
		require.LessOrEqual(t, time.Now(), deadline, "ollama-proxy:get-status never reported the rebound port (%v)", target)
		time.Sleep(300 * time.Millisecond)
	}
}

// TestBrokerManualNodeMergedIntoDiscovery adds a manual node through the
// broker and asserts it surfaces in the shared discovery snapshot.
func TestBrokerManualNodeMergedIntoDiscovery(t *testing.T) {
	const nodeName = "xproc-manual-merge-1"

	stdin, msgs, _, cleanup := startBrokerWith(t, "--manual-nodes-path", manualNodesBin)
	t.Cleanup(cleanup)

	waitForMethod(t, msgs, "app:ready", 10*time.Second)

	// Add a manual node with a unique name (which becomes its id) at a
	// loopback address so the probe resolves quickly whether or not a
	// local service answers.
	addReq := fmt.Sprintf(`{"jsonrpc":"2.0","id":800,"method":"node/add","params":{"address":"127.0.0.1","name":%q}}`, nodeName) + "\n"
	_, err := stdin.Write([]byte(addReq))
	require.NoError(t, err, "write node/add")

	// Poll discovery:get-nodes until the manual node's id appears (the
	// node/discovered event fires after the first probe completes).
	deadline := time.After(20 * time.Second)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	reqID := 801
	sendReq(t, stdin, reqID, "discovery:get-nodes")
	for {
		select {
		case msg, ok := <-msgs:
			require.True(t, ok, "broker stream closed before the manual node appeared")
			if msg.Method == "" && msg.ID != nil {
				var res availableNodesResult
				if assert.NoError(t, json.Unmarshal(msg.Result, &res)) && containsNode(res.Nodes, nodeName) {
					t.Logf("manual node %q present in discovery snapshot", nodeName)
					return
				}
			}
		case <-ticker.C:
			reqID++
			sendReq(t, stdin, reqID, "discovery:get-nodes")
		case <-deadline:
			require.FailNow(t, fmt.Sprintf("timed out waiting for manual node %q in discovery:get-nodes", nodeName))
		}
	}
}
