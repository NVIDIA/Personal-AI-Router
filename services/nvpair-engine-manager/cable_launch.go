// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
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
	"time"

	"golang.org/x/crypto/ssh"
	"nvpair-shared/clustertrust"
)

const cableWorkerMaxBytes = 256 << 20
const cableLaunchHeader = "PAIR-CABLE-LAUNCH/1 "

// Constructed only from fixed provider failures; arbitrary stderr is discarded.
type cableWorkerOutputError string

func (e cableWorkerOutputError) Error() string { return string(e) }

type cableWorkerMessage struct {
	RunID       string              `json:"runId"`
	ReviewID    string              `json:"reviewId"`
	RemainingMs int64               `json:"remainingMs,omitempty"`
	Prearm      *cablePrearmMessage `json:"-"`
	cableProbeResult
}

type cableWorker struct {
	send       func(cableProbeCommand) error
	read       func(context.Context) (cableWorkerMessage, error)
	closeInput func() error
	wait       func(context.Context) error
	close      func()
}

// *ssh.Session implements this seam; no command or executable comes from the UI.
type cableSSHSession interface {
	StdinPipe() (io.WriteCloser, error)
	StdoutPipe() (io.Reader, error)
	StderrPipe() (io.Reader, error)
	Start(string) error
	Wait() error
	Close() error
	Signal(ssh.Signal) error
}

type cableWorkerInspection struct {
	NodeID         string `json:"nodeId"`
	UID            int    `json:"uid"`
	Home           string `json:"home"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	WorkerSHA256   string `json:"workerSha256"`
	WorkerBytes    int64  `json:"workerBytes"`
	CertificatePEM string `json:"certificatePem"`
	Protocol       string `json:"protocol"`
	MaxSeconds     int    `json:"maxSeconds"`
	MaxPorts       int    `json:"maxPorts"`
	WorkerPath     string `json:"workerPath,omitempty"`
	ProfileDir     string `json:"profileDir,omitempty"`
}

func cableAccountPlan(plan cableLaunchPlan) bool {
	return cableIdentifier(plan.NodeID, 128) && cableIdentifier(plan.Principal, 256) &&
		plan.Info.UID > 0 && plan.Info.OS == "Linux" && (plan.Info.Arch == "aarch64" || plan.Info.Arch == "arm64" || plan.Info.Arch == "x86_64" || plan.Info.Arch == "amd64") &&
		strings.HasPrefix(plan.Info.Home, "/") && plan.Info.Home != "/" && path.Clean(plan.Info.Home) == plan.Info.Home && len(plan.Info.Home) <= 1024
}

func cableOwnedPlan(plan cableLaunchPlan) bool {
	root := path.Join(plan.Info.Home, ".local", "share", "Nvidia Corporation", "Personal AI Router")
	r := plan.Receipt
	return cableAccountPlan(plan) &&
		r.Installed && r.NodeID == plan.NodeID && onboardingSHA.MatchString(r.ArtifactSHA256) && onboardingSHA.MatchString(r.ManifestSHA256) &&
		onboardingID.MatchString(path.Base(r.StagePath)) && r.StagePath == path.Join(root, ".onboarding", path.Base(r.StagePath)) &&
		r.BundlePath == path.Join(root, "bundles", r.ArtifactSHA256) && r.TUIPath == path.Join(r.BundlePath, "bin", "nvpair-tui") &&
		(r.StartupLifetime == "session" || r.StartupLifetime == "persistent")
}

// Retained operations and the independent cleanup protocol can reference all
// known worker generations. New probe admission separately requires current v4.
func cableRuntimePlan(plan cableLaunchPlan) bool {
	b := plan.Runtime
	absolute := func(value string) bool {
		return cableIdentifier(value, 4096) && strings.HasPrefix(value, "/") && value != "/" && path.Clean(value) == value
	}
	return b != nil && cableAccountPlan(plan) && b.NodeID == plan.NodeID && b.Principal == plan.Principal && b.UID == plan.Info.UID &&
		(b.Protocol == "pair-cable-worker/1" || b.Protocol == "pair-cable-worker/2" || b.Protocol == "pair-cable-worker/3" || b.Protocol == cableWorkerProtocol) && absolute(b.WorkerPath) && absolute(b.ProfileDir) &&
		onboardingSHA.MatchString(b.WorkerSHA256) && b.WorkerBytes > 0 && b.WorkerBytes <= cableWorkerMaxBytes && onboardingSHA.MatchString(b.CertificateSHA256)
}

// Route-observation metadata belongs to the coordinator, not the privileged
// execution header. Only the authenticated executable/profile binding crosses.
func cableRuntimeLaunchFields(b *cableWorkerBinding) map[string]any {
	return map[string]any{"nodeId": b.NodeID, "principal": b.Principal, "uid": b.UID, "workerPath": b.WorkerPath,
		"workerSha256": b.WorkerSHA256, "workerBytes": b.WorkerBytes, "profileDir": b.ProfileDir,
		"certificateSha256": b.CertificateSHA256, "protocol": b.Protocol}
}

func inspectCableWorker(ctx context.Context, client *onboardingSSH, plan cableLaunchPlan, mesh *clustertrust.Mesh) (cableLaunchPlan, error) {
	invalid := errors.New("owned cable worker, account, capability or paired identity could not be verified")
	if client == nil || mesh == nil || (plan.Runtime == nil && !cableOwnedPlan(plan)) || (plan.Runtime != nil && !cableRuntimePlan(plan)) {
		return plan, invalid
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	mesh.Refresh()
	config, pinned := mesh.ClientTLSConfig(plan.Principal)
	if !pinned || config.VerifyPeerCertificate == nil {
		return plan, invalid
	}
	script := onboardingOwnedScript + cableInspectScript
	input := map[string]any{"receipt": plan.Receipt, "nodeId": plan.NodeID, "uid": plan.Info.UID, "home": plan.Info.Home, "arch": plan.Info.Arch}
	if plan.Runtime != nil {
		script = cableRuntimeInspectScript
		input = map[string]any{"runtime": cableRuntimeLaunchFields(plan.Runtime), "uid": plan.Info.UID, "home": plan.Info.Home, "arch": plan.Info.Arch}
	}
	body, err := client.run(ctx, onboardingPython(script), bytes.NewReader(onboardingMarshal(input)))
	if err != nil {
		return plan, invalid
	}
	var found cableWorkerInspection
	maxSeconds := 20
	if plan.Runtime != nil && plan.Runtime.Protocol != cableWorkerProtocol {
		maxSeconds = 10
	}
	if onboardingDecode(body, &found) != nil || found.NodeID != plan.NodeID || found.UID != plan.Info.UID || found.Home != plan.Info.Home || found.OS != plan.Info.OS || found.Arch != plan.Info.Arch ||
		!onboardingSHA.MatchString(found.WorkerSHA256) || found.WorkerBytes <= 0 || found.WorkerBytes > cableWorkerMaxBytes || len(found.CertificatePEM) > 65536 ||
		((plan.Runtime == nil && found.Protocol != cableWorkerProtocol) || (plan.Runtime != nil && found.Protocol != plan.Runtime.Protocol)) || found.MaxSeconds != maxSeconds || found.MaxPorts != 2 {
		return plan, invalid
	}
	certificate, rest := pem.Decode([]byte(found.CertificatePEM))
	if certificate == nil || certificate.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 || config.VerifyPeerCertificate([][]byte{certificate.Bytes}, nil) != nil {
		return plan, invalid
	}
	// Revalidate against the current shared pin after the remote inspection.
	mesh.Refresh()
	current, pinned := mesh.ClientTLSConfig(plan.Principal)
	if !pinned || current.VerifyPeerCertificate == nil || current.VerifyPeerCertificate([][]byte{certificate.Bytes}, nil) != nil || ctx.Err() != nil {
		return plan, invalid
	}
	digest := sha256.Sum256(certificate.Bytes)
	if b := plan.Runtime; b != nil {
		if found.WorkerPath != b.WorkerPath || found.ProfileDir != b.ProfileDir || !strings.EqualFold(found.WorkerSHA256, b.WorkerSHA256) || found.WorkerBytes != b.WorkerBytes || !strings.EqualFold(hex.EncodeToString(digest[:]), b.CertificateSHA256) {
			return plan, invalid
		}
		// Keep the approved binding independent from the caller's attestation object.
		binding := *b
		binding.WorkerSHA256, binding.CertificateSHA256 = strings.ToLower(binding.WorkerSHA256), strings.ToLower(binding.CertificateSHA256)
		plan.Runtime = &binding
	}
	plan.WorkerSHA256, plan.WorkerBytes, plan.CertificateSHA256 = strings.ToLower(found.WorkerSHA256), found.WorkerBytes, hex.EncodeToString(digest[:])
	return plan, nil
}

func openCableWorker(ctx context.Context, client *onboardingSSH, plan cableLaunchPlan, request cableProbeOnceRequest, elevationPassword string) (*cableWorker, error) {
	validPlan := (plan.Runtime == nil && cableOwnedPlan(plan)) || (plan.Runtime != nil && cableRuntimePlan(plan) && plan.Runtime.Protocol == cableWorkerProtocol && strings.EqualFold(plan.Runtime.WorkerSHA256, plan.WorkerSHA256) && plan.Runtime.WorkerBytes == plan.WorkerBytes && strings.EqualFold(plan.Runtime.CertificateSHA256, plan.CertificateSHA256))
	if client == nil || !validPlan || !onboardingSHA.MatchString(plan.WorkerSHA256) || !onboardingSHA.MatchString(plan.CertificateSHA256) || plan.WorkerBytes <= 0 || plan.WorkerBytes > cableWorkerMaxBytes ||
		request.Protocol != cableWorkerProtocol || request.Review.OwnerNodeID != plan.NodeID || !cableIdentifier(request.RunID, 128) || !cableIdentifier(request.Review.ReviewID, 128) ||
		elevationPassword == "" || len(elevationPassword) > 4096 || strings.ContainsAny(elevationPassword, "\r\n\x00") {
		return nil, errors.New("reviewed worker and separate bounded administrator access are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	launchHeader := map[string]any{"nodeId": plan.NodeID, "principal": plan.Principal, "uid": plan.Info.UID, "home": plan.Info.Home,
		"workerSha256": plan.WorkerSHA256, "workerBytes": plan.WorkerBytes, "certificateSha256": plan.CertificateSHA256}
	if plan.Runtime != nil {
		launchHeader["runtime"] = cableRuntimeLaunchFields(plan.Runtime)
	} else {
		launchHeader["receipt"] = plan.Receipt
	}
	header, err := json.Marshal(launchHeader)
	if err != nil || len(header) > 16384 {
		return nil, errors.New("reviewed launch header exceeds its bound")
	}
	workerRequest, err := json.Marshal(request)
	if err != nil || len(workerRequest) > 32<<10 {
		return nil, errors.New("reviewed worker request exceeds its bound")
	}
	workerBase, cancelCause := context.WithCancelCause(ctx)
	workerCtx, cancel := context.WithTimeout(workerBase, 45*time.Second)
	var session cableSSHSession
	// Closing the supplied operation-owned SSH connection also bounds NewSession.
	stopPending := context.AfterFunc(workerCtx, client.close)
	if client.testCableSession != nil {
		session, err = client.testCableSession()
	} else if client.client != nil {
		session, err = client.client.NewSession()
	} else {
		err = errors.New("SSH session unavailable")
	}
	if err != nil {
		stopPending()
		cancel()
		cancelCause(context.Canceled)
		return nil, errors.New("owned SSH cable session could not be opened")
	}
	var closeOnce sync.Once
	closeWorker := func() {
		closeOnce.Do(func() { cancelCause(context.Canceled); cancel(); client.close(); _ = session.Close() })
	}
	failWorker := func(err error) {
		cancelCause(err)
		closeWorker()
	}
	stopPending()
	stop := context.AfterFunc(workerCtx, closeWorker)
	fail := func() (*cableWorker, error) {
		stop()
		failWorker(errors.New("fixed cable worker could not be launched; cleanup is not confirmed"))
		return nil, context.Cause(workerCtx)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		return fail()
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return fail()
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		return fail()
	}
	// Drain without retaining or exposing any sudo/remote stderr, with a hard cap.
	stderrDone := make(chan struct{})
	var stderrErr error
	go func() {
		defer close(stderrDone)
		n, err := io.Copy(io.Discard, io.LimitReader(stderr, (16<<10)+1))
		if n > 16<<10 {
			stderrErr = cableWorkerOutputError("cable worker diagnostic output exceeds its bound")
		} else if err != nil {
			stderrErr = cableWorkerOutputError("cable worker diagnostic stream did not close cleanly")
		}
		if stderrErr != nil {
			failWorker(stderrErr)
		}
	}()
	started := make(chan error, 1)
	go func() {
		started <- session.Start("/usr/bin/sudo -S -p '' -- /usr/bin/python3 -I -c " + onboardingQuote(cableRootLaunchScript))
	}()
	select {
	case err = <-started:
		if err != nil {
			return fail()
		}
	case <-workerCtx.Done():
		return fail()
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = session.Wait(); close(done) }()
	type incoming struct {
		message cableWorkerMessage
		err     error
	}
	messages := make(chan incoming, 2)
	readDone := make(chan struct{})
	var streamErr error
	go func() {
		defer close(readDone)
		defer close(messages)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), (16<<10)+1)
		count := 0
		prearm := false
		for scanner.Scan() {
			count++
			var message cableWorkerMessage
			var kind struct {
				State string `json:"state"`
			}
			var decodeErr error
			if json.Unmarshal(scanner.Bytes(), &kind) == nil && kind.State == "prearm-failed" && count == 1 {
				message, decodeErr = decodeCablePrearmLine(scanner.Bytes(), request.RunID, request.Review.ReviewID, request.Review.Targets)
				prearm = decodeErr == nil
			} else if count > 2 || prearm || decodeCableProbeLine(scanner.Bytes(), &message) != nil || message.RunID != request.RunID || message.ReviewID != request.Review.ReviewID ||
				(message.State != "armed" && message.State != "completed" && message.State != "cancelled" && message.State != "failed") ||
				(count == 1 && message.State != "armed") || (count == 2 && message.State == "armed") ||
				!validCableFinalValidation(message.FinalValidationCode, message.State) || (message.FinalValidationCode != "" && len(message.Observations) != 0) ||
				(message.State != "armed" && (message.Directness != "unverified" || len(message.Observations) > 6 || message.Sent < 0 || message.Sent > 40 || message.Received < 0 || message.Received > cableProbeReceiveLimit)) {
				decodeErr = cableWorkerOutputError("cable worker returned an invalid bounded protocol message")
			}
			if decodeErr != nil {
				streamErr = cableWorkerOutputError("cable worker returned an invalid bounded protocol message")
				select {
				case messages <- incoming{err: streamErr}:
				default:
				}
				failWorker(streamErr)
				return
			}
			if message.Prearm != nil {
				// This is the first message and the channel has two slots. Once
				// validated, enqueue before a concurrent transport cancellation.
				messages <- incoming{message: message}
				continue
			}
			select {
			case messages <- incoming{message: message}:
			case <-workerCtx.Done():
				streamErr = context.Cause(workerCtx)
				return
			}
		}
		if scanner.Err() != nil || (!prearm && count != 2) || (prearm && count != 1) {
			streamErr = cableWorkerOutputError("cable worker output is incomplete or exceeds its bound")
			failWorker(streamErr)
		}
	}()
	var writeMu sync.Mutex
	var inputOnce sync.Once
	var inputErr error
	closeInput := func() error {
		inputOnce.Do(func() {
			// SSH returns EOF when the remote already closed this channel. That
			// proves input is closed; worker cleanup still needs its own message.
			if err := stdin.Close(); err != nil && !errors.Is(err, io.EOF) {
				inputErr = errors.New("cable worker input closure is not confirmed")
			}
		})
		return inputErr
	}
	// Password is a separate stdin line. It is absent from command arguments,
	// public headers, worker messages, logs and retained operation records.
	preamble := make([]byte, 0, len(elevationPassword)+len(header)+len(workerRequest)+len(cableLaunchHeader)+3)
	preamble = append(preamble, elevationPassword...)
	preamble = append(preamble, '\n')
	preamble = append(preamble, cableLaunchHeader...)
	preamble = append(preamble, header...)
	preamble = append(preamble, '\n')
	preamble = append(preamble, workerRequest...)
	preamble = append(preamble, '\n')
	written := make(chan error, 1)
	go func() {
		defer clear(preamble)
		n, err := stdin.Write(preamble)
		if err == nil && n != len(preamble) {
			err = io.ErrShortWrite
		}
		written <- err
	}()
	writeTimer := time.NewTimer(2 * time.Second)
	defer writeTimer.Stop()
	var prefetchedPrearm *cableWorkerMessage
	initialWriteFailed := false
	select {
	case err = <-written:
		initialWriteFailed = err != nil
	case <-writeTimer.C:
		initialWriteFailed = true
	case <-workerCtx.Done():
		initialWriteFailed = true
	}
	if initialWriteFailed || workerCtx.Err() != nil {
		_, launchErr := fail()
		select {
		case item, ok := <-messages:
			if !ok || item.err != nil || item.message.Prearm == nil {
				return nil, launchErr
			}
			// The remote may consume the request before local Write returns.
			// Receiving the queued primary is its atomic retention point; a
			// separate signal would race publication. Keep the closed worker
			// readable while wait still reports failure and cleanup stays unknown.
			prefetchedPrearm = &item.message
		default:
			return nil, launchErr
		}
	}
	armed, startedCommand, cancelledCommand, prearmTerminal := false, false, false, false
	// Once parsed, the fixed preparation cause survives a later EOF, process or
	// cancellation failure. Those failures still surface through wait and cannot
	// confirm cleanup. Never similarly promote an ordinary incomplete stream.
	retainedPrearm := func() (cableWorkerMessage, bool) {
		writeMu.Lock()
		defer writeMu.Unlock()
		if prefetchedPrearm != nil {
			message := *prefetchedPrearm
			prefetchedPrearm = nil
			prearmTerminal = true
			return message, true
		}
		select {
		case item, ok := <-messages:
			if ok && item.err == nil && item.message.Prearm != nil {
				prearmTerminal = true
				return item.message, true
			}
		default:
		}
		return cableWorkerMessage{}, false
	}
	return &cableWorker{
		send: func(command cableProbeCommand) error {
			writeMu.Lock()
			defer writeMu.Unlock()
			if command.RunID != request.RunID || (command.Command != "start" && command.Command != "cancel") || prearmTerminal || cancelledCommand || (command.Command == "start" && (!armed || startedCommand)) {
				return errors.New("invalid or repeated fixed cable command")
			}
			if err := writeCableProbeMessage(workerCtx, stdin, command); err != nil {
				return errors.New("cable worker command delivery is not confirmed")
			}
			if command.Command == "start" {
				startedCommand = true
			} else {
				cancelledCommand = true
			}
			return nil
		},
		read: func(readCtx context.Context) (cableWorkerMessage, error) {
			if err := readCtx.Err(); err != nil {
				if message, ok := retainedPrearm(); ok {
					return message, nil
				}
				return cableWorkerMessage{}, err
			}
			if err := context.Cause(workerCtx); err != nil {
				if message, ok := retainedPrearm(); ok {
					return message, nil
				}
				return cableWorkerMessage{}, err
			}
			select {
			case item, ok := <-messages:
				if !ok {
					if err := context.Cause(workerCtx); err != nil {
						return cableWorkerMessage{}, err
					}
					return cableWorkerMessage{}, errors.New("cable worker output ended; no additional cleanup acknowledgement exists")
				}
				if item.err == nil && (item.message.State == "armed" || item.message.Prearm != nil) {
					writeMu.Lock()
					armed = item.message.State == "armed"
					prearmTerminal = item.message.Prearm != nil
					writeMu.Unlock()
				}
				return item.message, item.err
			case <-readCtx.Done():
				if message, ok := retainedPrearm(); ok {
					return message, nil
				}
				return cableWorkerMessage{}, readCtx.Err()
			case <-workerCtx.Done():
				if message, ok := retainedPrearm(); ok {
					return message, nil
				}
				return cableWorkerMessage{}, context.Cause(workerCtx)
			}
		},
		closeInput: closeInput,
		wait: func(waitCtx context.Context) error {
			if err := waitCtx.Err(); err != nil {
				return err
			}
			if err := context.Cause(workerCtx); err != nil {
				return err
			}
			select {
			case <-done:
				if err := context.Cause(workerCtx); err != nil {
					return err
				}
				if waitErr != nil {
					return errors.New("cable worker exited without a confirmed successful process result")
				}
			case <-waitCtx.Done():
				return waitCtx.Err()
			case <-workerCtx.Done():
				return context.Cause(workerCtx)
			}
			select {
			case <-readDone:
				if err := context.Cause(workerCtx); err != nil {
					return err
				}
				if streamErr != nil {
					return streamErr
				}
			case <-waitCtx.Done():
				return waitCtx.Err()
			case <-workerCtx.Done():
				return context.Cause(workerCtx)
			}
			select {
			case <-stderrDone:
				if err := waitCtx.Err(); err != nil {
					return err
				}
				if err := context.Cause(workerCtx); err != nil {
					return err
				}
				return stderrErr
			case <-waitCtx.Done():
				return waitCtx.Err()
			case <-workerCtx.Done():
				return context.Cause(workerCtx)
			}
		},
		close: func() { stop(); closeWorker() },
	}, nil
}

const cableInspectScript = `
import platform
if set(h)!={'receipt','nodeId','uid','home','arch'}: raise RuntimeError('invalid fixed cable inspection')
if os.geteuid()<=0 or os.geteuid()!=h['uid'] or home!=h['home'] or platform.system()!='Linux' or platform.machine()!=h['arch']: raise RuntimeError('reviewed native account changed')
verify_tui()
profile=os.path.join(home,'.config','Nvidia Corporation','Personal AI Router','cluster')
if os.path.realpath(profile)!=profile: raise RuntimeError('public profile path changed')
def bounded_public(filename,limit):
 fd=os.open(filename,os.O_RDONLY|os.O_NOFOLLOW|os.O_CLOEXEC)
 try:
  info=os.fstat(fd)
  if not stat.S_ISREG(info.st_mode) or info.st_uid!=os.geteuid() or info.st_size>limit: raise RuntimeError('public profile shape changed')
  with os.fdopen(os.dup(fd),'rb') as stream: data=stream.read(limit+1)
  if len(data)>limit: raise RuntimeError('public profile is oversized')
  return data
 finally: os.close(fd)
identity=json.loads(bounded_public(os.path.join(profile,'identity.json'),8192))
certificate=bounded_public(os.path.join(profile,'node.crt'),65536).decode('ascii')
if identity.get('node_uuid')!=h['nodeId']: raise RuntimeError('PAIR host identity changed')
manifest_raw=bounded_public(os.path.join(bundle,'bin','manifest.json'),131072)
if hashlib.sha256(manifest_raw).hexdigest()!=r['manifestSha256'].lower(): raise RuntimeError('installed manifest changed')
entries=[entry for entry in json.loads(manifest_raw)['files'] if entry.get('fileName')=='nvpair-engine-manager']
if len(entries)!=1: raise RuntimeError('owned worker is unavailable')
worker=entries[0]
if type(worker.get('size')) is not int or not 0<worker['size']<=268435456: raise RuntimeError('worker size is unsupported')
capability=product([os.path.join(bundle,'bin','nvpair-engine-manager'),'--cable-worker-capabilities-json'],limit=1024)
if capability!={'protocol':'pair-cable-worker/4','maxSeconds':20,'maxPorts':2}: raise RuntimeError('fixed worker capability unavailable')
print(json.dumps(dict(nodeId=identity['node_uuid'],uid=os.geteuid(),home=home,os=platform.system(),arch=platform.machine(),workerSha256=worker['sha256'],workerBytes=worker['size'],certificatePem=certificate,**capability)))
`

// The paired running service supplies these paths and byte identities. This
// branch reads current files only; it never fabricates an onboarding checkpoint.
const cableRuntimeInspectScript = `import hashlib,json,os,platform,pwd,ssl,stat,sys
raw=sys.stdin.buffer.read(16385)
if len(raw)>16384: raise RuntimeError('runtime inspection input is oversized')
h=json.loads(raw)
if set(h)!={'runtime','uid','home','arch'}: raise RuntimeError('invalid fixed runtime inspection')
b=h['runtime']; fields={'nodeId','principal','uid','workerPath','workerSha256','workerBytes','profileDir','certificateSha256','protocol'}
if set(b)!=fields or b['protocol'] not in ('pair-cable-worker/1','pair-cable-worker/2','pair-cable-worker/3','pair-cable-worker/4') or type(b['uid']) is not int or b['uid']<=0 or b['uid']!=h['uid'] or os.geteuid()!=b['uid']: raise RuntimeError('attested normal account changed')
home=os.path.normpath(pwd.getpwuid(os.geteuid()).pw_dir)
if home!=h['home'] or platform.system()!='Linux' or platform.machine()!=h['arch']: raise RuntimeError('native account/platform changed')
def digest(value): return isinstance(value,str) and len(value)==64 and all(c in '0123456789abcdefABCDEF' for c in value)
def absolute(value): return isinstance(value,str) and value.startswith('/') and value!='/' and len(value)<=4096 and os.path.normpath(value)==value and '\x00' not in value
if not absolute(b['workerPath']) or not absolute(b['profileDir']) or not digest(b['workerSha256']) or not digest(b['certificateSha256']) or type(b['workerBytes']) is not int or not 0<b['workerBytes']<=268435456: raise RuntimeError('invalid runtime path or byte binding')
opened=[]; owners={0,b['uid']}
def directory(value):
 fd=os.open('/',os.O_RDONLY|os.O_DIRECTORY|os.O_CLOEXEC); opened.append(fd)
 parts=[] if value=='/' else value.split('/')[1:]
 for index,part in enumerate(parts):
  if not part or part in ('.','..'): raise RuntimeError('invalid runtime path component')
  fd=os.open(part,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW|os.O_CLOEXEC,dir_fd=fd); opened.append(fd); info=os.fstat(fd)
  sticky_parent=index<len(parts)-1 and info.st_uid==0 and bool(info.st_mode&stat.S_ISVTX)
  if info.st_uid not in owners or (info.st_mode&0o022 and not sticky_parent): raise RuntimeError('runtime path ancestry is writable or unowned')
 return fd
def file(parent,name,limit):
 fd=os.open(name,os.O_RDONLY|os.O_NOFOLLOW|os.O_CLOEXEC,dir_fd=parent); opened.append(fd); info=os.fstat(fd)
 if not stat.S_ISREG(info.st_mode) or info.st_uid not in owners or info.st_mode&0o022 or info.st_size>limit: raise RuntimeError('runtime file is writable, unowned or oversized')
 return fd,info
def read(parent,name,limit):
 fd,info=file(parent,name,limit)
 with os.fdopen(os.dup(fd),'rb') as source: data=source.read(limit+1)
 if len(data)>limit: raise RuntimeError('runtime public metadata is oversized')
 return data
try:
 source,info=file(directory(os.path.dirname(b['workerPath'])),os.path.basename(b['workerPath']),268435456)
 if info.st_size!=b['workerBytes'] or not info.st_mode&0o111: raise RuntimeError('attested worker size or executable mode changed')
 hash=hashlib.sha256(); count=0
 while count<=b['workerBytes']:
  chunk=os.read(source,min(1048576,b['workerBytes']+1-count))
  if not chunk: break
  hash.update(chunk); count+=len(chunk)
 if count!=b['workerBytes'] or hash.hexdigest()!=b['workerSha256'].lower(): raise RuntimeError('attested running worker bytes changed')
 profile=directory(b['profileDir'])
 identity=json.loads(read(profile,'identity.json',8192)); certificate=read(profile,'node.crt',65536).decode('ascii')
 if identity.get('node_uuid')!=b['principal'] or hashlib.sha256(ssl.PEM_cert_to_DER_cert(certificate)).hexdigest()!=b['certificateSha256'].lower(): raise RuntimeError('attested public profile identity changed')
 print(json.dumps(dict(nodeId=b['nodeId'],uid=os.geteuid(),home=home,os=platform.system(),arch=platform.machine(),workerSha256=hash.hexdigest(),workerBytes=count,certificatePem=certificate,protocol=b['protocol'],maxSeconds=20 if b['protocol']=='pair-cable-worker/4' else 10,maxPorts=2,workerPath=b['workerPath'],profileDir=b['profileDir'])))
finally:
 for fd in opened:
  try: os.close(fd)
  except OSError: pass
`

// Executed only by the explicitly approved launcher. The script is fixed product
// code; stdin contains no executable, argv, environment, or arbitrary path field.
const cableRootLaunchScript = `import fcntl,hashlib,json,os,pwd,ssl,stat,sys
def line(limit):
 data=bytearray()
 while len(data)<=limit:
  item=os.read(0,1)
  if not item: raise RuntimeError('launch input ended')
  if item==b'\n': return bytes(data)
  data.extend(item)
 raise RuntimeError('launch line is oversized')
prefix=b'PAIR-CABLE-LAUNCH/1 '
first=line(16404)
if not first.startswith(prefix):
 if len(first)>4096: raise RuntimeError('invalid elevation framing')
 first=b''
 first=line(16404)
if not first.startswith(prefix): raise RuntimeError('fixed launch header missing')
h=json.loads(first[len(prefix):]); first=b''
required={'nodeId','principal','uid','home','workerSha256','workerBytes','certificateSha256'}
mode='runtime' if 'runtime' in h else 'receipt'
if set(h)!=(required|{mode}) or os.geteuid()!=0 or type(h['uid']) is not int or h['uid']<=0 or int(os.environ.get('SUDO_UID','-1'))!=h['uid']: raise RuntimeError('approved administrator/account boundary changed')
home=os.path.normpath(pwd.getpwuid(h['uid']).pw_dir)
if home!=h['home'] or home=='/' or os.path.realpath(home)!=home: raise RuntimeError('approved native home changed')
def digest(value): return isinstance(value,str) and len(value)==64 and all(c in '0123456789abcdef' for c in value)
if not digest(h['workerSha256']) or not digest(h['certificateSha256']): raise RuntimeError('invalid fixed worker digest')
if type(h['workerBytes']) is not int or not 0<h['workerBytes']<=268435456: raise RuntimeError('invalid worker byte bound')
opened=[]
owners={h['uid']}
def directory(parts):
 fd=os.open(home,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW|os.O_CLOEXEC); opened.append(fd)
 for part in parts:
  fd=os.open(part,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW|os.O_CLOEXEC,dir_fd=fd); opened.append(fd)
  info=os.fstat(fd)
  if info.st_uid!=h['uid'] or info.st_mode&0o022: raise RuntimeError('owned directory changed')
 return fd
def absolute_directory(value):
 if not isinstance(value,str) or not value.startswith('/') or len(value)>4096 or os.path.normpath(value)!=value or '\x00' in value: raise RuntimeError('invalid attested absolute path')
 fd=os.open('/',os.O_RDONLY|os.O_DIRECTORY|os.O_CLOEXEC); opened.append(fd)
 parts=[] if value=='/' else value.split('/')[1:]
 for index,part in enumerate(parts):
  if not part or part in ('.','..'): raise RuntimeError('invalid attested path component')
  fd=os.open(part,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW|os.O_CLOEXEC,dir_fd=fd); opened.append(fd); info=os.fstat(fd)
  sticky_parent=index<len(parts)-1 and info.st_uid==0 and bool(info.st_mode&stat.S_ISVTX)
  if info.st_uid not in owners or (info.st_mode&0o022 and not sticky_parent): raise RuntimeError('attested path ancestry changed')
 return fd
def file(parent,name,limit):
 fd=os.open(name,os.O_RDONLY|os.O_NOFOLLOW|os.O_CLOEXEC,dir_fd=parent); opened.append(fd); info=os.fstat(fd)
 if not stat.S_ISREG(info.st_mode) or info.st_uid not in owners or info.st_mode&0o022 or info.st_size>limit: raise RuntimeError('owned file changed')
 return fd,info
def read(parent,name,limit):
 fd,info=file(parent,name,limit)
 with os.fdopen(os.dup(fd),'rb') as source: data=source.read(limit+1)
 if len(data)>limit: raise RuntimeError('owned metadata is oversized')
 return data
try:
 if mode=='runtime':
  b=h['runtime']; fields={'nodeId','principal','uid','workerPath','workerSha256','workerBytes','profileDir','certificateSha256','protocol'}
  if set(b)!=fields or b['protocol']!='pair-cable-worker/4' or b['nodeId']!=h['nodeId'] or b['principal']!=h['principal'] or b['uid']!=h['uid'] or b['workerSha256']!=h['workerSha256'] or b['workerBytes']!=h['workerBytes'] or b['certificateSha256']!=h['certificateSha256']: raise RuntimeError('approved runtime binding changed')
  if not isinstance(b['workerPath'],str) or not b['workerPath'].startswith('/') or os.path.normpath(b['workerPath'])!=b['workerPath'] or len(b['workerPath'])>4096 or '\x00' in b['workerPath']: raise RuntimeError('invalid attested worker path')
  owners={0,h['uid']}
  binfd=absolute_directory(os.path.dirname(b['workerPath'])); workername=os.path.basename(b['workerPath'])
  profile=b['profileDir']; profilefd=absolute_directory(profile); profile_identity=h['principal']
 else:
  r=h['receipt']; op=os.path.basename(r['stagePath'])
  if not digest(r['artifactSha256']) or not digest(r['manifestSha256']) or len(op)!=32 or any(c not in '0123456789abcdef' for c in op): raise RuntimeError('invalid fixed ownership digest')
  root=os.path.join(home,'.local','share','Nvidia Corporation','Personal AI Router'); bundle=os.path.join(root,'bundles',r['artifactSha256'])
  if r['bundlePath']!=bundle or r['stagePath']!=os.path.join(root,'.onboarding',op) or r['tuiPath']!=os.path.join(bundle,'bin','nvpair-tui') or r.get('nodeId')!=h['nodeId'] or not r.get('installed') or r['startupLifetime'] not in ('session','persistent'): raise RuntimeError('owned receipt changed')
  bundlefd=directory(['.local','share','Nvidia Corporation','Personal AI Router','bundles',r['artifactSha256']])
  marker={'owner':'nvidia-pair-onboarding-v1','operationId':op,'sha256':r['artifactSha256'],'manifestSha256':r['manifestSha256'],'startupLifetime':r['startupLifetime']}
  if json.loads(read(bundlefd,'.pair-onboarding.json',8192))!=marker: raise RuntimeError('owned bundle marker changed')
  binfd=os.open('bin',os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW|os.O_CLOEXEC,dir_fd=bundlefd); opened.append(binfd)
  manifest=read(binfd,'manifest.json',131072)
  if hashlib.sha256(manifest).hexdigest()!=r['manifestSha256']: raise RuntimeError('reviewed manifest changed')
  entries=[entry for entry in json.loads(manifest)['files'] if entry.get('fileName')=='nvpair-engine-manager']
  if len(entries)!=1 or entries[0].get('sha256','').lower()!=h['workerSha256'] or entries[0].get('size')!=h['workerBytes']: raise RuntimeError('reviewed worker identity changed')
  profile=os.path.join(home,'.config','Nvidia Corporation','Personal AI Router','cluster')
  profilefd=directory(['.config','Nvidia Corporation','Personal AI Router','cluster']); workername='nvpair-engine-manager'; profile_identity=h['nodeId']
 identity=json.loads(read(profilefd,'identity.json',8192))
 certificate=read(profilefd,'node.crt',65536).decode('ascii')
 if identity.get('node_uuid')!=profile_identity or hashlib.sha256(ssl.PEM_cert_to_DER_cert(certificate)).hexdigest()!=h['certificateSha256']: raise RuntimeError('reviewed public PAIR identity changed')
 source,info=file(binfd,workername,268435456)
 if info.st_size!=h['workerBytes'] or not info.st_mode&0o111: raise RuntimeError('reviewed worker size or executable mode changed')
 executable=os.memfd_create('nvpair-cable-worker',os.MFD_CLOEXEC|os.MFD_ALLOW_SEALING); opened.append(executable)
 hash=hashlib.sha256(); count=0
 while count<h['workerBytes']:
  chunk=os.read(source,min(1048576,h['workerBytes']-count))
  if not chunk: raise RuntimeError('reviewed worker was truncated')
  hash.update(chunk); count+=len(chunk); view=memoryview(chunk)
  while view:
   written=os.write(executable,view)
   if written<=0: raise RuntimeError('sealed worker copy incomplete')
   view=view[written:]
 if os.read(source,1) or hash.hexdigest()!=h['workerSha256']: raise RuntimeError('reviewed worker bytes changed')
 os.fchmod(executable,0o500)
 fcntl.fcntl(executable,fcntl.F_ADD_SEALS,fcntl.F_SEAL_WRITE|fcntl.F_SEAL_GROW|fcntl.F_SEAL_SHRINK|fcntl.F_SEAL_SEAL)
 for fd in opened:
  if fd!=executable: os.close(fd)
 opened=[executable]
 argv=['nvpair-engine-manager','--cable-probe-once','--node-id',h['nodeId'],'--node-info-port','14318','--cluster-dir',profile]
 os.execve(executable,argv,{'PATH':'/usr/bin:/bin','LANG':'C','LC_ALL':'C','HOME':home})
finally:
 for fd in opened:
  try: os.close(fd)
  except OSError: pass
`
