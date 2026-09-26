// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
)

// The fixed verifier uses the same byte/profile/account checks, but its only
// supported mode inspects process/lock metadata and cannot open probe sockets.
func cableCleanupLoaderScript() string {
	script := strings.Replace(cableRootLaunchScript, "'--cable-probe-once'", "'--cable-cleanup-once'", 1)
	// Cleanup has its own protocol and may inspect retained v1-v3 deployments.
	// Preserve the approved binding token; only this fixed helper accepts them.
	return strings.Replace(script, "b['protocol']!='pair-cable-worker/4'", "b['protocol'] not in ('pair-cable-worker/1','pair-cable-worker/2','pair-cable-worker/3','pair-cable-worker/4')", 1)
}

func cableCleanupShellCommand() string {
	return "/usr/bin/sudo -S -p '' -- /usr/bin/python3 -I -c " + onboardingQuote(cableCleanupLoaderScript())
}

type cableCleanupWorker struct {
	read   func(context.Context) (cableCleanupMessage, error)
	finish func(context.Context, cableCleanupCommand) (cableCleanupMessage, error)
	close  func()
}

func openCableCleanupWorker(ctx context.Context, client *onboardingSSH, plan cableLaunchPlan, request cableCleanupRequest, admin string) (*cableCleanupWorker, error) {
	if client == nil || !cableRuntimePlan(plan) || plan.Runtime.CleanupProtocol != cableCleanupProtocol || plan.Runtime.CleanupScope == nil || admin == "" || len(admin) > 4096 || strings.ContainsAny(admin, "\r\n\x00") {
		return nil, errors.New("verified cleanup inspector and separate administrator approval are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	workerCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	var session cableSSHSession
	var err error
	stopPending := context.AfterFunc(workerCtx, client.close)
	if client.testCableSession != nil {
		session, err = client.testCableSession()
	} else if client.client != nil {
		session, err = client.client.NewSession()
	} else {
		err = errors.New("cleanup SSH session unavailable")
	}
	stopPending()
	if err != nil {
		cancel()
		return nil, errors.New("cleanup SSH session unavailable")
	}
	var once sync.Once
	closeWorker := func() { once.Do(func() { cancel(); client.close(); _ = session.Close() }) }
	stop := context.AfterFunc(workerCtx, closeWorker)
	fail := func() (*cableCleanupWorker, error) {
		stop()
		closeWorker()
		return nil, errors.New("fixed cleanup inspector transport failed")
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
	stderrDone := make(chan struct{})
	var stderrErr error
	go func() {
		defer close(stderrDone)
		n, err := io.Copy(io.Discard, io.LimitReader(stderr, (16<<10)+1))
		if n > 16<<10 || err != nil {
			stderrErr = errors.New("cleanup diagnostic stream did not finish within its bound")
			closeWorker()
		}
	}()
	if err = session.Start(cableCleanupShellCommand()); err != nil {
		return fail()
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = session.Wait(); close(done) }()
	type reply struct {
		message cableCleanupMessage
		err     error
	}
	messages := make(chan reply, 2)
	readDone := make(chan struct{})
	var streamErr error
	go func() {
		defer close(readDone)
		defer close(messages)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), (16<<10)+1)
		count := 0
		firstState := ""
		for scanner.Scan() {
			count++
			var message cableCleanupMessage
			if count > 2 || decodeCableProbeLine(scanner.Bytes(), &message) != nil || message.Protocol != cableCleanupProtocol || message.RunID != request.RunID || message.ReviewID != request.ReviewID || message.AttemptID != request.AttemptID || message.Challenge != request.Challenge || message.NodeID != request.NodeID || message.Principal != request.Principal || (count == 1 && message.State != "ready" && message.State != "blocked") || (count == 2 && (firstState != "ready" || message.State != "closed")) {
				streamErr = errors.New("cleanup inspector returned an invalid bounded reply")
				closeWorker()
				return
			}
			if count == 1 {
				firstState = message.State
			}
			select {
			case messages <- reply{message: message}:
			case <-workerCtx.Done():
				return
			}
		}
		if scanner.Err() != nil || (firstState == "ready" && count != 2) || count == 0 {
			streamErr = errors.New("cleanup inspector reply is incomplete")
		}
	}()
	header := map[string]any{"nodeId": plan.NodeID, "principal": plan.Principal, "uid": plan.Info.UID, "home": plan.Info.Home, "workerSha256": plan.WorkerSHA256, "workerBytes": plan.WorkerBytes, "certificateSha256": plan.CertificateSHA256, "runtime": cableRuntimeLaunchFields(plan.Runtime)}
	headerBytes, _ := json.Marshal(header)
	requestBytes, _ := json.Marshal(request)
	if len(headerBytes) > 16384 || len(requestBytes) > 16384 {
		return fail()
	}
	preamble := append([]byte(admin+"\n"+cableLaunchHeader), headerBytes...)
	preamble = append(preamble, '\n')
	preamble = append(preamble, requestBytes...)
	preamble = append(preamble, '\n')
	written := make(chan error, 1)
	go func() {
		defer clear(preamble)
		n, e := stdin.Write(preamble)
		if e == nil && n != len(preamble) {
			e = io.ErrShortWrite
		}
		written <- e
	}()
	select {
	case err = <-written:
		if err != nil {
			return fail()
		}
	case <-workerCtx.Done():
		return fail()
	}
	read := func(readCtx context.Context) (cableCleanupMessage, error) {
		if err := readCtx.Err(); err != nil {
			return cableCleanupMessage{}, err
		}
		select {
		case item, ok := <-messages:
			if !ok {
				return cableCleanupMessage{}, errors.New("cleanup inspector ended without the required acknowledgement")
			}
			return item.message, item.err
		case <-readCtx.Done():
			return cableCleanupMessage{}, readCtx.Err()
		case <-workerCtx.Done():
			return cableCleanupMessage{}, workerCtx.Err()
		}
	}
	return &cableCleanupWorker{read: read, close: func() { stop(); closeWorker() }, finish: func(finishCtx context.Context, command cableCleanupCommand) (cableCleanupMessage, error) {
		if command.AttemptID != request.AttemptID || command.Challenge != request.Challenge || (command.Command != "release" && command.Command != "cancel") {
			return cableCleanupMessage{}, errors.New("cleanup command does not match the owned attempt")
		}
		if err := writeCableProbeMessage(finishCtx, stdin, command); err != nil {
			return cableCleanupMessage{}, errors.New("cleanup close request delivery is unconfirmed")
		}
		message, err := read(finishCtx)
		if err != nil {
			return message, err
		}
		if err = stdin.Close(); err != nil && !errors.Is(err, io.EOF) {
			return message, errors.New("cleanup input close is unconfirmed")
		}
		select {
		case <-done:
			if waitErr != nil {
				return message, errors.New("cleanup inspector exit is unconfirmed")
			}
		case <-finishCtx.Done():
			return message, finishCtx.Err()
		}
		select {
		case <-readDone:
			if streamErr != nil {
				return message, streamErr
			}
		case <-finishCtx.Done():
			return message, finishCtx.Err()
		}
		select {
		case <-stderrDone:
			if stderrErr != nil {
				return message, stderrErr
			}
		case <-finishCtx.Done():
			return message, finishCtx.Err()
		case <-workerCtx.Done():
			return message, workerCtx.Err()
		}
		if err := finishCtx.Err(); err != nil {
			return message, err
		}
		if err := workerCtx.Err(); err != nil {
			return message, err
		}
		return message, nil
	}}, nil
}
