// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const cableCleanupProtocol = "pair-cable-cleanup/1"
const cableCleanupInspectorFlag = "--cable-cleanup-once"
const cableCleanupProcessLimit = 4096
const cableCleanupCandidateLimit = 32
const cableCleanupMetadataLimit = 16 << 20
const cableCleanupArgLimit = 64 << 10

type cableCleanupScope struct {
	PID        int    `json:"pid"`
	StartTicks string `json:"startTicks"`
	BootID     string `json:"bootId"`
	PIDNS      string `json:"pidNs"`
	MountNS    string `json:"mountNs"`
	NetNS      string `json:"netNs"`
	UserNS     string `json:"userNs"`
}

type cableCleanupRequest struct {
	Protocol             string            `json:"protocol"`
	RunID                string            `json:"runId"`
	ReviewID             string            `json:"reviewId"`
	AttemptID            string            `json:"attemptId"`
	Challenge            string            `json:"challenge"`
	NodeID               string            `json:"nodeId"`
	Principal            string            `json:"principal"`
	UID                  int               `json:"uid"`
	Scope                cableCleanupScope `json:"scope"`
	WorkerSHA256         string            `json:"workerSha256"`
	OriginalWorkerSHA256 string            `json:"originalWorkerSha256"`
}

type cableCleanupCommand struct {
	AttemptID string `json:"attemptId"`
	Challenge string `json:"challenge"`
	Command   string `json:"command"`
}

type cableCleanupMessage struct {
	Protocol         string            `json:"protocol"`
	RunID            string            `json:"runId"`
	ReviewID         string            `json:"reviewId"`
	AttemptID        string            `json:"attemptId"`
	Challenge        string            `json:"challenge"`
	NodeID           string            `json:"nodeId"`
	Principal        string            `json:"principal"`
	State            string            `json:"state"`
	Code             string            `json:"code"`
	Scope            cableCleanupScope `json:"scope"`
	LockDevice       uint64            `json:"lockDevice"`
	LockInode        uint64            `json:"lockInode"`
	Passes           int               `json:"passes"`
	Processes        int               `json:"processes"`
	Candidates       int               `json:"candidates"`
	RemainingMs      int64             `json:"remainingMs"`
	CleanupConfirmed bool              `json:"cleanupConfirmed"`
}

type cableCleanupFault string

func (e cableCleanupFault) Error() string { return string(e) }

func cableCleanupCodeValid(code string) bool {
	switch code {
	case "clear", "released", "cancelled", "invalid-request", "unsupported", "scope-mismatch", "normal-process-changed", "proc-unavailable", "inspection-limit", "candidate-active", "candidate-ambiguous", "candidate-unreadable", "lock-unavailable", "lock-busy", "lock-changed", "deadline-exceeded", "owner-disconnected", "invalid-command", "close-unconfirmed":
		return true
	}
	return false
}

func cableCleanupCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline-exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var fault cableCleanupFault
	if errors.As(err, &fault) && cableCleanupCodeValid(string(fault)) {
		return string(fault)
	}
	return "proc-unavailable"
}

func cableCleanupScopeValid(s cableCleanupScope) bool {
	ticks, err := strconv.ParseUint(s.StartTicks, 10, 64)
	if s.PID <= 1 || s.PID > 2147483647 || err != nil || ticks == 0 || strconv.FormatUint(ticks, 10) != s.StartTicks || len(s.BootID) != 36 {
		return false
	}
	for i, c := range s.BootID {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	for _, ns := range []struct{ value, prefix string }{{s.PIDNS, "pid"}, {s.MountNS, "mnt"}, {s.NetNS, "net"}, {s.UserNS, "user"}} {
		prefix := ns.prefix + ":["
		if !strings.HasPrefix(ns.value, prefix) || !strings.HasSuffix(ns.value, "]") {
			return false
		}
		n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(ns.value, prefix), "]"), 10, 64)
		if err != nil || n == 0 || ns.value != prefix+strconv.FormatUint(n, 10)+"]" {
			return false
		}
	}
	return true
}

func cableCleanupSameNamespace(a, b cableCleanupScope) bool {
	return a.BootID == b.BootID && a.PIDNS == b.PIDNS && a.MountNS == b.MountNS && a.NetNS == b.NetNS && a.UserNS == b.UserNS
}

func cableCleanupRequestValid(r cableCleanupRequest) bool {
	return r.Protocol == cableCleanupProtocol && cableIdentifier(r.RunID, 128) && cableIdentifier(r.ReviewID, 128) && cableIdentifier(r.AttemptID, 128) &&
		len(r.Challenge) >= 32 && cableIdentifier(r.Challenge, 128) && cableIdentifier(r.NodeID, 128) && cableIdentifier(r.Principal, 256) && r.UID > 0 &&
		cableCleanupScopeValid(r.Scope) && onboardingSHA.MatchString(r.WorkerSHA256) && onboardingSHA.MatchString(r.OriginalWorkerSHA256)
}

type cableCleanupProcess struct {
	PID, Parent, UID int
	StartTicks       string
	Executable       string
	Args             []string
	Gone, Kernel     bool
	Bytes            int
}

type cableCleanupLock struct {
	FD            int
	Device, Inode uint64
}

// The native adapter performs fixed proc/lock reads. Tests inject only these
// bounded operations and never invoke native scope qualification or flock.
type cableCleanupIO struct {
	verify    func(context.Context, cableCleanupRequest) (cableCleanupScope, cableCleanupProcess, error)
	processes func(context.Context) ([]int, error)
	process   func(context.Context, int) (cableCleanupProcess, error)
	lock      func(context.Context) (cableCleanupLock, error)
	checkLock func(context.Context, cableCleanupLock) error
	close     func(cableCleanupLock) error
}

func cableCleanupExactAncestor(args []string) bool {
	if len(args) == 4 && strings.HasPrefix(filepath.Base(args[0]), "python3") && args[1] == "-I" && args[2] == "-c" && args[3] == cableCleanupLoaderScript() {
		return true
	}
	if len(args) == 9 && filepath.Base(args[0]) == "sudo" && args[1] == "-S" && args[2] == "-p" && args[3] == "" && args[4] == "--" && args[5] == "/usr/bin/python3" && args[6] == "-I" && args[7] == "-c" && args[8] == cableCleanupLoaderScript() {
		return true
	}
	if len(args) == 3 && (filepath.Base(args[0]) == "sh" || filepath.Base(args[0]) == "bash" || filepath.Base(args[0]) == "dash") && args[1] == "-c" && args[2] == cableCleanupShellCommand() {
		return true
	}
	return false
}

func cableCleanupCandidate(p cableCleanupProcess) string {
	for _, arg := range p.Args {
		if arg == "--cable-probe-once" || arg == "-cable-probe-once" || arg == "--cable-probe-once=true" || arg == "-cable-probe-once=true" {
			return "candidate-active"
		}
		if strings.Contains(arg, "cable-probe-once") || strings.Contains(arg, "nvpair-cable-worker") || strings.Contains(arg, "cable-cleanup-once") || arg == cableRootLaunchScript || arg == cableCleanupLoaderScript() {
			return "candidate-ambiguous"
		}
	}
	if strings.Contains(p.Executable, "memfd:nvpair-cable-worker") {
		return "candidate-ambiguous"
	}
	return ""
}

func inspectCableCleanup(ctx context.Context, request cableCleanupRequest, ops cableCleanupIO, message *cableCleanupMessage) (lock cableCleanupLock, held bool, err error) {
	scope, self, err := ops.verify(ctx, request)
	if err != nil {
		return lock, false, err
	}
	message.Scope = scope
	lock, err = ops.lock(ctx)
	if err != nil {
		return lock, false, err
	}
	held = true
	message.LockDevice, message.LockInode = lock.Device, lock.Inode
	if err = ops.checkLock(ctx, lock); err != nil {
		return
	}
	exempt := map[int]string{self.PID: self.StartTicks}
	budget := 0
	for parent, depth := self.Parent, 0; parent > 1; depth++ {
		if depth >= 8 {
			return lock, true, cableCleanupFault("inspection-limit")
		}
		p, e := ops.process(ctx, parent)
		if e != nil {
			return lock, true, e
		}
		budget += p.Bytes
		if p.Gone || p.Kernel || !cableCleanupExactAncestor(p.Args) {
			break
		}
		if p.UID != 0 && p.UID != request.UID {
			return lock, true, cableCleanupFault("candidate-ambiguous")
		}
		if _, duplicate := exempt[p.PID]; duplicate {
			return lock, true, cableCleanupFault("candidate-ambiguous")
		}
		exempt[p.PID] = p.StartTicks
		parent = p.Parent
	}
	for pass := 0; pass < 2; pass++ {
		if e := ctx.Err(); e != nil {
			return lock, true, e
		}
		pids, e := ops.processes(ctx)
		if e != nil {
			return lock, true, e
		}
		if len(pids) > cableCleanupProcessLimit {
			return lock, true, cableCleanupFault("inspection-limit")
		}
		seen := map[int]bool{}
		for _, pid := range pids {
			if e := ctx.Err(); e != nil {
				return lock, true, e
			}
			if pid < 1 || seen[pid] {
				return lock, true, cableCleanupFault("proc-unavailable")
			}
			seen[pid] = true
			p, e := ops.process(ctx, pid)
			message.Processes++
			if e != nil {
				return lock, true, e
			}
			budget += p.Bytes
			if budget > cableCleanupMetadataLimit {
				return lock, true, cableCleanupFault("inspection-limit")
			}
			if p.Gone || p.Kernel {
				continue
			}
			if generation, ok := exempt[pid]; ok && generation == p.StartTicks &&
				(pid == self.PID || ((p.UID == 0 || p.UID == request.UID) && cableCleanupExactAncestor(p.Args))) {
				continue
			}
			if code := cableCleanupCandidate(p); code != "" {
				message.Candidates++
				if message.Candidates > cableCleanupCandidateLimit {
					code = "inspection-limit"
				}
				return lock, true, cableCleanupFault(code)
			}
		}
		if !seen[self.PID] || !seen[request.Scope.PID] {
			return lock, true, cableCleanupFault("scope-mismatch")
		}
		message.Passes++
		if e = ops.checkLock(ctx, lock); e != nil {
			return lock, true, e
		}
	}
	if _, _, e := ops.verify(ctx, request); e != nil {
		return lock, true, e
	}
	return lock, true, ctx.Err()
}

func runCableCleanupInspector(ctx context.Context, input io.Reader, output io.Writer) error {
	return runCableCleanupInspectorWithIO(ctx, input, output, nativeCableCleanupIO())
}

func runCableCleanupInspectorWithIO(ctx context.Context, input io.Reader, output io.Writer, ops cableCleanupIO) error {
	whole, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var stopOnce sync.Once
	stopInput := func() {
		stopOnce.Do(func() {
			if closer, ok := input.(io.Closer); ok {
				_ = closer.Close()
			}
		})
	}
	stop := context.AfterFunc(whole, stopInput)
	defer stop()
	defer stopInput()
	lines := make(chan []byte, 2)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer close(lines)
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 4096), (32<<10)+1)
		for count := 0; count < 2 && scanner.Scan(); count++ {
			line := bytes.Clone(scanner.Bytes())
			select {
			case lines <- line:
			case <-whole.Done():
				return
			}
		}
	}()
	read := func(readCtx context.Context) ([]byte, error) {
		select {
		case line, ok := <-lines:
			if !ok {
				return nil, cableCleanupFault("owner-disconnected")
			}
			return line, nil
		case <-readCtx.Done():
			return nil, readCtx.Err()
		}
	}
	line, err := read(whole)
	if err != nil {
		return cableCleanupFault(cableCleanupCode(err))
	}
	var request cableCleanupRequest
	if decodeCableProbeLine(line, &request) != nil || !cableCleanupRequestValid(request) {
		return cableCleanupFault("invalid-request")
	}
	message := cableCleanupMessage{Protocol: cableCleanupProtocol, RunID: request.RunID, ReviewID: request.ReviewID, AttemptID: request.AttemptID, Challenge: request.Challenge, NodeID: request.NodeID, Principal: request.Principal, Scope: request.Scope}
	inspection, stopInspection := context.WithTimeout(whole, 5*time.Second)
	lock, held, inspectErr := inspectCableCleanup(inspection, request, ops, &message)
	stopInspection()
	closed := false
	closeOwned := func() bool {
		if closed {
			return !held
		}
		closed = true
		if held {
			if ops.close(lock) != nil {
				return false
			}
			held = false
		}
		return true
	}
	defer closeOwned()
	finish := func(state, code string) error {
		clean := closeOwned()
		if code == "close-unconfirmed" {
			clean = false
		}
		stopInput()
		select {
		case <-readDone:
		case <-whole.Done():
			clean = false
		}
		message.State, message.Code, message.RemainingMs, message.CleanupConfirmed = state, code, 0, clean
		if !clean && state == "closed" {
			message.Code = "close-unconfirmed"
		}
		// The caller still requires a successful process join. A timeout/failed
		// output cannot become a cleanup acknowledgement at the coordinator.
		return writeCableProbeMessage(whole, output, message)
	}
	if inspectErr != nil {
		return finish("blocked", cableCleanupCode(inspectErr))
	}
	lease, stopLease := context.WithTimeout(whole, 5*time.Second)
	defer stopLease()
	deadline, _ := lease.Deadline()
	message.State, message.Code, message.RemainingMs = "ready", "clear", max(0, time.Until(deadline).Milliseconds())
	if message.RemainingMs <= 0 {
		return finish("blocked", "deadline-exceeded")
	}
	if err := writeCableProbeMessage(lease, output, message); err != nil {
		return err
	}
	line, err = read(lease)
	if err != nil {
		return finish("closed", cableCleanupCode(err))
	}
	var command cableCleanupCommand
	if decodeCableProbeLine(line, &command) != nil || command.AttemptID != request.AttemptID || command.Challenge != request.Challenge || (command.Command != "release" && command.Command != "cancel") {
		return finish("closed", "invalid-command")
	}
	if err := lease.Err(); err != nil {
		return finish("closed", cableCleanupCode(err))
	}
	if command.Command == "cancel" {
		return finish("closed", "cancelled")
	}
	if _, _, err := ops.verify(lease, request); err != nil {
		return finish("closed", cableCleanupCode(err))
	}
	if err := ops.checkLock(lease, lock); err != nil {
		return finish("closed", cableCleanupCode(err))
	}
	return finish("closed", "released")
}
