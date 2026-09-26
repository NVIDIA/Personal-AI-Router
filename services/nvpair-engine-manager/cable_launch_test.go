// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"nvpair-shared/cableprobe"
	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
)

func launchTestPlan(t *testing.T) (cableLaunchPlan, *clustertrust.Mesh, cableWorkerInspection) {
	t.Helper()
	dir := t.TempDir()
	clustertrusttest.Join(t, dir, "synthetic-cluster", "synthetic-owner")
	mesh := clustertrust.Open(dir)
	config, ok := mesh.ClientTLSConfig("synthetic-owner")
	if !ok {
		t.Fatal("synthetic shared trust unavailable")
	}
	cert := config.Certificates[0].Certificate[0]
	digest := sha256.Sum256(cert)
	root := "/home/fixture/.local/share/Nvidia Corporation/Personal AI Router"
	artifact := strings.Repeat("a", 64)
	bundle := path.Join(root, "bundles", artifact)
	plan := cableLaunchPlan{NodeID: "synthetic-owner", Principal: "synthetic-owner", Info: onboardingPlatformInfo{UID: 1000, Home: "/home/fixture", OS: "Linux", Arch: "aarch64"},
		Receipt: onboardingInstallReceipt{NodeID: "synthetic-owner", Installed: true, ArtifactSHA256: artifact, ManifestSHA256: strings.Repeat("b", 64),
			StagePath: path.Join(root, ".onboarding", strings.Repeat("c", 32)), BundlePath: bundle, TUIPath: path.Join(bundle, "bin", "nvpair-tui"), StartupLifetime: "session"},
		WorkerSHA256: strings.Repeat("d", 64), WorkerBytes: 128, CertificateSHA256: hex.EncodeToString(digest[:])}
	found := cableWorkerInspection{NodeID: plan.NodeID, UID: plan.Info.UID, Home: plan.Info.Home, OS: plan.Info.OS, Arch: plan.Info.Arch,
		WorkerSHA256: plan.WorkerSHA256, WorkerBytes: plan.WorkerBytes, CertificatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert})),
		Protocol: cableWorkerProtocol, MaxSeconds: 20, MaxPorts: 2}
	return plan, mesh, found
}

func launchTestRequest(plan cableLaunchPlan) cableProbeOnceRequest {
	request := cableProbeOnceRequest{Protocol: cableWorkerProtocol, RunID: "synthetic-run", Marker: strings.Repeat("a", 32), Review: cableprobe.Review{
		ReviewID: "synthetic-review", OwnerNodeID: plan.NodeID, RemainingMs: 30000, Targets: []cableprobe.Target{
			{NodeID: plan.NodeID, Principal: plan.Principal, RawPrivilege: "unknown", Ports: []cableprobe.Port{{SwitchID: "aabbccdd", PortName: "p0", Interfaces: []cableprobe.Interface{{Name: "eth0", Index: 7, MAC: "02:00:00:00:00:01"}}}}},
			{NodeID: "synthetic-peer", Principal: "synthetic-peer", RawPrivilege: "unknown", Ports: []cableprobe.Port{{SwitchID: "aabbccee", PortName: "p0", Interfaces: []cableprobe.Interface{{Name: "eth1", Index: 8, MAC: "02:00:00:00:00:02"}}}}},
		}}}
	request.Peers = append(request.Peers, struct {
		NodeID      string `json:"nodeId"`
		Principal   string `json:"principal"`
		Address     string `json:"address"`
		ControlPort int    `json:"controlPort"`
	}{"synthetic-peer", "synthetic-peer", "192.0.2.7", 14323})
	return request
}

func launchTestRuntimePlan(t *testing.T) (cableLaunchPlan, *clustertrust.Mesh, cableWorkerInspection) {
	t.Helper()
	plan, mesh, found := launchTestPlan(t)
	plan.Receipt = onboardingInstallReceipt{}
	plan.Runtime = &cableWorkerBinding{NodeID: plan.NodeID, Principal: plan.Principal, UID: plan.Info.UID,
		WorkerPath: "/opt/PAIR Releases/current/worker", WorkerSHA256: plan.WorkerSHA256, WorkerBytes: plan.WorkerBytes,
		ProfileDir: "/srv/pair-state/fixture/profile", CertificateSHA256: plan.CertificateSHA256, Protocol: cableWorkerProtocol, RequesterAddress: "192.0.2.44"}
	found.WorkerPath, found.ProfileDir = plan.Runtime.WorkerPath, plan.Runtime.ProfileDir
	return plan, mesh, found
}

func TestCableWorkerRuntimeInspectionWithoutOnboardingReceipt(t *testing.T) {
	for _, mode := range []string{"valid", "reported-uid", "reported-hash", "reported-size", "reported-profile", "reported-worker-path", "reported-certificate", "attested-uid", "attested-certificate", "relative-path", "protocol"} {
		t.Run(mode, func(t *testing.T) {
			plan, mesh, found := launchTestRuntimePlan(t)
			switch mode {
			case "reported-uid":
				found.UID++
			case "reported-hash":
				found.WorkerSHA256 = strings.Repeat("f", 64)
			case "reported-size":
				found.WorkerBytes++
			case "reported-profile":
				found.ProfileDir = "/srv/other-profile"
			case "reported-worker-path":
				found.WorkerPath = "/opt/other-worker"
			case "reported-certificate":
				_, _, other := launchTestPlan(t)
				found.CertificatePEM = other.CertificatePEM
			case "attested-uid":
				plan.Runtime.UID++
			case "attested-certificate":
				plan.Runtime.CertificateSHA256 = strings.Repeat("f", 64)
			case "relative-path":
				plan.Runtime.WorkerPath = "relative/worker"
			case "protocol":
				plan.Runtime.Protocol = "unknown"
			}
			calls := 0
			client := &onboardingSSH{testRun: func(_ context.Context, command string, input io.Reader) ([]byte, error) {
				calls++
				if strings.Contains(command, "verify_tui") || strings.Contains(command, "/usr/bin/sudo") || strings.Contains(command, "os.execve") || strings.Contains(command, "subprocess") {
					t.Fatal("runtime inspection invoked legacy ownership or execution")
				}
				var header map[string]json.RawMessage
				if json.NewDecoder(input).Decode(&header) != nil || len(header) != 4 || header["receipt"] != nil {
					t.Fatal("runtime inspection manufactured a receipt")
				}
				var binding map[string]json.RawMessage
				if json.Unmarshal(header["runtime"], &binding) != nil || len(binding) != 9 || binding["requesterAddress"] != nil {
					t.Fatal("routing metadata entered runtime execution binding")
				}
				return json.Marshal(found)
			}}
			approved, err := inspectCableWorker(context.Background(), client, plan, mesh)
			if mode == "valid" {
				if err != nil || approved.Receipt != (onboardingInstallReceipt{}) || approved.WorkerSHA256 != plan.Runtime.WorkerSHA256 || approved.CertificateSHA256 != plan.Runtime.CertificateSHA256 || approved.Runtime.RequesterAddress != plan.Runtime.RequesterAddress {
					t.Fatalf("attested runtime without receipt was rejected or changed: %v", err)
				}
				originalProfile := approved.Runtime.ProfileDir
				plan.Runtime.ProfileDir = "/srv/later-mutation"
				if approved.Runtime.ProfileDir != originalProfile {
					t.Fatal("caller mutation changed approved runtime scope")
				}
			} else if err == nil {
				t.Fatal("changed runtime account, bytes, profile or certificate was accepted")
			}
			if (mode == "attested-uid" || mode == "relative-path" || mode == "protocol") && calls != 0 {
				t.Fatal("invalid runtime binding reached SSH inspection")
			}
		})
	}
}

func TestCableWorkerRuntimeLaunchUsesOnlyAttestedScope(t *testing.T) {
	for _, mode := range []string{"valid", "uid-drift", "hash-drift", "certificate-drift", "invalid-profile"} {
		t.Run(mode, func(t *testing.T) {
			plan, _, _ := launchTestRuntimePlan(t)
			request := launchTestRequest(plan)
			switch mode {
			case "uid-drift":
				plan.Runtime.UID++
			case "hash-drift":
				plan.WorkerSHA256 = strings.Repeat("f", 64)
			case "certificate-drift":
				plan.CertificateSHA256 = strings.Repeat("f", 64)
			case "invalid-profile":
				plan.Runtime.ProfileDir = "../profile"
			}
			session := &launchTestSession{output: strings.NewReader(launchTestMessages(t, request, false)), done: make(chan struct{})}
			calls := 0
			client := &onboardingSSH{testCableSession: func() (cableSSHSession, error) { calls++; return session, nil }}
			worker, err := openCableWorker(context.Background(), client, plan, request, "synthetic-elevation-secret")
			if mode != "valid" {
				if err == nil {
					worker.close()
					t.Fatal("changed runtime launch binding was accepted")
				}
				if calls != 0 {
					t.Fatal("changed binding reached privileged session creation")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer worker.close()
			data, _ := session.input.snapshot()
			lines := bytes.Split(data, []byte{'\n'})
			var header map[string]json.RawMessage
			if len(lines) < 3 || !bytes.HasPrefix(lines[1], []byte(cableLaunchHeader)) || json.Unmarshal(bytes.TrimPrefix(lines[1], []byte(cableLaunchHeader)), &header) != nil || header["receipt"] != nil {
				t.Fatal("runtime launcher manufactured an onboarding receipt")
			}
			var binding map[string]json.RawMessage
			if json.Unmarshal(header["runtime"], &binding) != nil || len(binding) != 9 || binding["requesterAddress"] != nil {
				t.Fatal("unexpected runtime execution fields")
			}
			var workerPath, profileDir string
			if json.Unmarshal(binding["workerPath"], &workerPath) != nil || json.Unmarshal(binding["profileDir"], &profileDir) != nil || workerPath != plan.Runtime.WorkerPath || profileDir != plan.Runtime.ProfileDir {
				t.Fatal("attested executable/profile scope changed")
			}
			if strings.Contains(session.command, plan.Runtime.WorkerPath) || strings.Contains(session.command, plan.Runtime.ProfileDir) {
				t.Fatal("runtime paths were interpolated into SSH command arguments")
			}
		})
	}
}

func launchTestMessages(t *testing.T, request cableProbeOnceRequest, cleanup bool) string {
	t.Helper()
	armed, err := json.Marshal(cableWorkerMessage{RunID: request.RunID, ReviewID: request.Review.ReviewID, RemainingMs: 5000, cableProbeResult: cableProbeResult{State: "armed"}})
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := json.Marshal(cableWorkerMessage{RunID: request.RunID, ReviewID: request.Review.ReviewID, cableProbeResult: cableProbeResult{
		State: "completed", Directness: "unverified", CleanupConfirmed: cleanup, Observations: []cableProbeObservation{}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(armed) + "\n" + string(terminal) + "\n"
}

func TestCableWorkerFinalValidationProtocol(t *testing.T) {
	for _, mode := range []string{"legacy-completed", "failed-final", "failed-stale", "unknown-code", "completed-with-failure", "retained-observations"} {
		t.Run(mode, func(t *testing.T) {
			plan, _, _ := launchTestPlan(t)
			request := launchTestRequest(plan)
			result := cableWorkerMessage{RunID: request.RunID, ReviewID: request.Review.ReviewID, cableProbeResult: cableProbeResult{State: "failed", Directness: "unverified", CleanupConfirmed: true, Message: "Cable receive failed.", FinalValidationCode: "final-revalidation-failed", Observations: []cableProbeObservation{}}}
			valid := mode == "legacy-completed" || mode == "failed-final" || mode == "failed-stale"
			switch mode {
			case "legacy-completed":
				result.State, result.FinalValidationCode = "completed", ""
			case "failed-stale":
				result.FinalValidationCode = "facts-stale"
			case "unknown-code":
				result.FinalValidationCode = "untrusted-code"
			case "completed-with-failure":
				result.State = "completed"
			case "retained-observations":
				result.Observations = []cableProbeObservation{{}}
			}
			terminal, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			armed := strings.Split(launchTestMessages(t, request, true), "\n")[0]
			session := &launchTestSession{output: strings.NewReader(armed + "\n" + string(terminal) + "\n"), done: make(chan struct{})}
			client := &onboardingSSH{testCableSession: func() (cableSSHSession, error) { return session, nil }}
			worker, err := openCableWorker(context.Background(), client, plan, request, "synthetic-admin-input")
			if err != nil {
				if valid {
					t.Fatal(err)
				}
				return
			}
			defer worker.close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, armErr := worker.read(ctx)
			message, readErr := worker.read(ctx)
			session.finish()
			waitErr := worker.wait(ctx)
			if valid {
				if armErr != nil || readErr != nil || waitErr != nil || message.FinalValidationCode != result.FinalValidationCode {
					t.Fatalf("valid bounded final outcome was lost: %v %v %v", armErr, readErr, waitErr)
				}
			} else if armErr == nil && readErr == nil && waitErr == nil {
				t.Fatal("contradictory final failure entered the worker result")
			}
		})
	}
}

func TestCableWorkerInspectionBindsSharedPublicIdentity(t *testing.T) {
	for _, mode := range []string{"valid", "unmanaged", "account", "worker-size", "capability", "legacy-window", "host-id", "untrusted-certificate", "trailing-certificate"} {
		t.Run(mode, func(t *testing.T) {
			plan, mesh, found := launchTestPlan(t)
			calls := 0
			if mode == "unmanaged" {
				plan.Receipt.Installed = false
			}
			switch mode {
			case "account":
				found.UID++
			case "worker-size":
				found.WorkerBytes = cableWorkerMaxBytes + 1
			case "capability":
				found.MaxSeconds = 11
			case "legacy-window":
				found.MaxSeconds = 10
			case "host-id":
				found.NodeID = "foreign"
			case "untrusted-certificate":
				_, _, other := launchTestPlan(t)
				found.CertificatePEM = other.CertificatePEM
			case "trailing-certificate":
				found.CertificatePEM += "unexpected"
			}
			client := &onboardingSSH{testRun: func(_ context.Context, command string, input io.Reader) ([]byte, error) {
				calls++
				if strings.Contains(command, "/usr/bin/sudo") || !strings.Contains(command, "verify_tui()") || !strings.Contains(command, "--cable-worker-capabilities-json") {
					t.Fatal("inspection escaped fixed read-only product checks")
				}
				var header map[string]json.RawMessage
				if json.NewDecoder(input).Decode(&header) != nil || len(header) != 5 {
					t.Fatal("inspection header is not the bounded public contract")
				}
				return json.Marshal(found)
			}}
			result, err := inspectCableWorker(context.Background(), client, plan, mesh)
			if mode == "valid" {
				if err != nil || result.WorkerSHA256 != plan.WorkerSHA256 || result.WorkerBytes != plan.WorkerBytes || result.CertificateSHA256 != plan.CertificateSHA256 {
					t.Fatalf("verified public worker plan rejected: %v", err)
				}
			} else if err == nil {
				t.Fatal("invalid worker plan was admitted")
			}
			if mode == "unmanaged" && calls != 0 {
				t.Fatal("unmanaged worker reached SSH inspection")
			}
		})
	}
}

type launchTestInput struct {
	mu       sync.Mutex
	data     bytes.Buffer
	closed   bool
	closeErr error
}

func (w *launchTestInput) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	return w.data.Write(data)
}
func (w *launchTestInput) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return w.closeErr
}
func (w *launchTestInput) snapshot() ([]byte, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.data.Bytes()), w.closed
}

type launchTestSession struct {
	input             launchTestInput
	output            io.Reader
	stderr            io.Reader
	command           string
	startErr, waitErr error
	done              chan struct{}
	once              sync.Once
}

func (s *launchTestSession) StdinPipe() (io.WriteCloser, error) { return &s.input, nil }
func (s *launchTestSession) StdoutPipe() (io.Reader, error)     { return s.output, nil }
func (s *launchTestSession) StderrPipe() (io.Reader, error) {
	if s.stderr != nil {
		return s.stderr, nil
	}
	return strings.NewReader(""), nil
}
func (s *launchTestSession) Start(command string) error { s.command = command; return s.startErr }
func (s *launchTestSession) Wait() error                { <-s.done; return s.waitErr }
func (s *launchTestSession) Signal(ssh.Signal) error    { return nil }
func (s *launchTestSession) finish()                    { s.once.Do(func() { close(s.done) }) }
func (s *launchTestSession) Close() error {
	s.input.Close()
	if closer, ok := s.output.(io.Closer); ok {
		closer.Close()
	}
	s.finish()
	return nil
}

func TestCableWorkerFixedLaunchAndStreamingLifecycle(t *testing.T) {
	plan, _, _ := launchTestPlan(t)
	request := launchTestRequest(plan)
	session := &launchTestSession{output: strings.NewReader(launchTestMessages(t, request, false)), done: make(chan struct{})}
	client := &onboardingSSH{testCableSession: func() (cableSSHSession, error) { return session, nil }}
	secret := "synthetic-elevation-secret"
	worker, err := openCableWorker(context.Background(), client, plan, request, secret)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.close()
	if strings.Contains(session.command, secret) || !strings.HasPrefix(session.command, "/usr/bin/sudo -S -p '' -- /usr/bin/python3 -I -c ") {
		t.Fatal("credential escaped stdin or launcher command changed")
	}
	input, closed := session.input.snapshot()
	lines := bytes.Split(bytes.TrimSuffix(input, []byte{'\n'}), []byte{'\n'})
	if closed || len(lines) != 3 || string(lines[0]) != secret || !bytes.HasPrefix(lines[1], []byte(cableLaunchHeader)) || bytes.Contains(lines[1], []byte(secret)) || bytes.Contains(lines[2], []byte(secret)) {
		t.Fatal("elevation input was not separate from public worker framing")
	}
	if err := worker.send(cableProbeCommand{RunID: request.RunID, Command: "start"}); err == nil {
		t.Fatal("start was allowed before observed arm")
	}
	armed, err := worker.read(context.Background())
	if err != nil || armed.State != "armed" || armed.RemainingMs != 5000 {
		t.Fatalf("matching arm lost: %v", err)
	}
	if err := worker.send(cableProbeCommand{RunID: "foreign", Command: "start"}); err == nil {
		t.Fatal("foreign run command accepted")
	}
	if err := worker.send(cableProbeCommand{RunID: request.RunID, Command: "start"}); err != nil {
		t.Fatal(err)
	}
	if err := worker.send(cableProbeCommand{RunID: request.RunID, Command: "start"}); err == nil {
		t.Fatal("start was repeated")
	}
	terminal, err := worker.read(context.Background())
	if err != nil || terminal.State != "completed" || terminal.CleanupConfirmed {
		t.Fatalf("terminal cleanup uncertainty was changed: %v", err)
	}
	if err := worker.closeInput(); err != nil {
		t.Fatal(err)
	}
	if _, closed := session.input.snapshot(); !closed {
		t.Fatal("input close was not observable")
	}
	session.finish()
	if err := worker.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.read(context.Background()); err == nil {
		t.Fatal("EOF became an implicit cleanup acknowledgement")
	}
}

func TestCableWorkerRejectsUnboundedOrInvalidLaunchBeforeSession(t *testing.T) {
	for _, mode := range []string{"unmanaged", "missing-elevation", "multiline-elevation", "oversized-request", "wrong-owner", "missing-pin"} {
		t.Run(mode, func(t *testing.T) {
			plan, _, _ := launchTestPlan(t)
			request := launchTestRequest(plan)
			secret := "synthetic-elevation-secret"
			switch mode {
			case "unmanaged":
				plan.Receipt.Installed = false
			case "missing-elevation":
				secret = ""
			case "multiline-elevation":
				secret += "\nextra"
			case "oversized-request":
				request.Review.Reason = strings.Repeat("x", 33<<10)
			case "wrong-owner":
				request.Review.OwnerNodeID = "foreign"
			case "missing-pin":
				plan.WorkerSHA256 = ""
			}
			calls := 0
			client := &onboardingSSH{testCableSession: func() (cableSSHSession, error) { calls++; return nil, errors.New("unexpected session") }}
			if _, err := openCableWorker(context.Background(), client, plan, request, secret); err == nil || calls != 0 {
				t.Fatal("invalid launch reached SSH session creation")
			}
		})
	}
}

func TestCableWorkerIncompleteOrMalformedOutputCannotConfirmCompletion(t *testing.T) {
	for _, mode := range []string{"eof", "armed-only", "foreign-id", "oversized", "extra-message", "repeated-arm"} {
		t.Run(mode, func(t *testing.T) {
			plan, _, _ := launchTestPlan(t)
			request := launchTestRequest(plan)
			output := launchTestMessages(t, request, true)
			switch mode {
			case "eof":
				output = ""
			case "armed-only":
				output = strings.Split(output, "\n")[0] + "\n"
			case "foreign-id":
				output = strings.ReplaceAll(output, request.RunID, "foreign")
			case "oversized":
				output = strings.Repeat("x", (16<<10)+1) + "\n"
			case "extra-message":
				output += "{}\n"
			case "repeated-arm":
				output = strings.Split(output, "\n")[0] + "\n" + strings.Split(output, "\n")[0] + "\n"
			}
			session := &launchTestSession{output: strings.NewReader(output), done: make(chan struct{})}
			client := &onboardingSSH{testCableSession: func() (cableSSHSession, error) { return session, nil }}
			worker, err := openCableWorker(context.Background(), client, plan, request, "synthetic-elevation-secret")
			if err != nil {
				return
			}
			defer worker.close()
			session.finish()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := worker.wait(ctx); err == nil {
				t.Fatal("invalid protocol stream acquired confirmed completion")
			}
		})
	}
}

func TestCableWorkerReadAndWaitRespectCallerDeadline(t *testing.T) {
	plan, _, _ := launchTestPlan(t)
	request := launchTestRequest(plan)
	reader, writer := io.Pipe()
	defer writer.Close()
	session := &launchTestSession{output: reader, done: make(chan struct{})}
	client := &onboardingSSH{testCableSession: func() (cableSSHSession, error) { return session, nil }}
	worker, err := openCableWorker(context.Background(), client, plan, request, "synthetic-elevation-secret")
	if err != nil {
		t.Fatal(err)
	}
	defer worker.close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	message, err := worker.read(ctx)
	if !errors.Is(err, context.Canceled) || message.CleanupConfirmed {
		t.Fatal("cancelled read inferred cleanup")
	}
	if err := worker.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("wait ignored caller cancellation")
	}
}

func TestCableWorkerRemoteEOFClosesInputWithoutInferringCleanup(t *testing.T) {
	plan, _, _ := launchTestPlan(t)
	request := launchTestRequest(plan)
	session := &launchTestSession{output: strings.NewReader(launchTestMessages(t, request, false)), done: make(chan struct{})}
	session.input.closeErr = io.EOF // x/crypto returns EOF after remote channel close.
	client := &onboardingSSH{testCableSession: func() (cableSSHSession, error) { return session, nil }}
	worker, err := openCableWorker(context.Background(), client, plan, request, "synthetic-elevation-secret")
	if err != nil {
		t.Fatal(err)
	}
	defer worker.close()
	if _, err := worker.read(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := worker.send(cableProbeCommand{RunID: request.RunID, Command: "start"}); err != nil {
		t.Fatal(err)
	}
	terminal, err := worker.read(context.Background())
	if err != nil || terminal.CleanupConfirmed {
		t.Fatal("explicit cleanup uncertainty was not preserved")
	}
	session.finish()
	if err := worker.closeInput(); err != nil {
		t.Fatal("remote EOF was treated as failed input closure")
	}
	if err := worker.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if terminal.CleanupConfirmed {
		t.Fatal("input closure or process exit invented a cleanup acknowledgement")
	}
}

// This is a static contract check, not proof that sudo/memfd execution worked.
func TestCableWorkerRootScriptHasOnlyPinnedFixedExecution(t *testing.T) {
	for _, required := range []string{"os.O_NOFOLLOW", "os.memfd_create", "fcntl.F_SEAL_WRITE|fcntl.F_SEAL_GROW|fcntl.F_SEAL_SHRINK|fcntl.F_SEAL_SEAL", "hash.hexdigest()!=h['workerSha256']", "os.execve(executable,argv", "'--cable-probe-once'", "'--node-info-port','14318'"} {
		if !strings.Contains(cableRootLaunchScript, required) {
			t.Fatal("fixed byte-pinned execution contract missing")
		}
	}
	for _, forbidden := range []string{"h['argv']", "h['executable']", "h['environment']", "shell=True", "sudoers", "setcap"} {
		if strings.Contains(cableRootLaunchScript, forbidden) {
			t.Fatal("general execution or persistent privilege entered fixed launcher")
		}
	}
}
