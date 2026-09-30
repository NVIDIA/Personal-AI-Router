// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startGroupLeader runs script under /bin/sh as the leader of its own process
// group, the way PAIR starts an engine, and returns its PID, a channel closed
// once it has exited, and the first line it prints.
func startGroupLeader(t *testing.T, script string, args ...string) (int, <-chan struct{}, string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{"-c", script, "sh"}, args...)...)
	configureSysProcAttr(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start group leader: %v", err)
	}
	line, readErr := bufio.NewReader(stdout).ReadString('\n')
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	})
	if readErr != nil {
		t.Fatalf("read the group leader's first line: %v", readErr)
	}
	return cmd.Process.Pid, done, strings.TrimSpace(line)
}

// waitForFile reports whether path exists within timeout.
func waitForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// ignoresTermScript is a process that survives SIGTERM, so terminatePID has to
// decide whether to escalate. It prints once the disposition is in place.
const ignoresTermScript = `trap '' TERM; echo ready; exec sleep 60`

// TestTerminatePIDWithholdsTheForcedKillWhenNoLongerOurs checks that a process
// which outlives the graceful signal is not force-killed once stillOurs says it
// is no longer the process the ownership check confirmed.
func TestTerminatePIDWithholdsTheForcedKillWhenNoLongerOurs(t *testing.T) {
	pid, done, _ := startGroupLeader(t, ignoresTermScript)

	terminatePID(pid, 200*time.Millisecond, func(int) bool { return false })

	select {
	case <-done:
		t.Fatal("process was force-killed after stillOurs reported it is no longer ours")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestTerminatePIDForceKillsAProcessThatIsStillOurs checks the escalation itself:
// a process that ignores SIGTERM and is still ours is killed.
func TestTerminatePIDForceKillsAProcessThatIsStillOurs(t *testing.T) {
	pid, done, _ := startGroupLeader(t, ignoresTermScript)

	terminatePID(pid, 200*time.Millisecond, func(int) bool { return true })

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a process that ignores SIGTERM survived the forced kill")
	}
}

// groupWithMemberScript leads a process group that has one other member. The
// member records a SIGTERM in the marker file named by $1 and prints its own PID
// once its trap is in place; the leader then waits.
const groupWithMemberScript = `sh -c 'trap "touch \"$0\"; exit 0" TERM; echo $$; while :; do sleep 1; done' "$1" &
exec sleep 60`

// TestSignalPIDSparesTheGroupOfANonLeader checks that signalling a process that
// does not lead its group reaches that process only. Its group was started by
// something else, and the ownership check examined only the target.
func TestSignalPIDSparesTheGroupOfANonLeader(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "member-signalled")
	_, leaderDone, memberLine := startGroupLeader(t, groupWithMemberScript, marker)
	member, err := strconv.Atoi(memberLine)
	if err != nil {
		t.Fatalf("parse the group member's PID %q: %v", memberLine, err)
	}

	if err := signalPID(member, false); err != nil {
		t.Fatalf("signal the group member: %v", err)
	}

	if !waitForFile(marker, 5*time.Second) {
		t.Fatal("the group member never received SIGTERM")
	}
	select {
	case <-leaderDone:
		t.Fatal("signalling a non-leader also signalled its group leader")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestSignalPIDSignalsTheGroupOfALeader checks that signalling a group leader
// reaches the rest of its group, which is how a forced stop reaches the helpers
// an engine PAIR started has forked.
func TestSignalPIDSignalsTheGroupOfALeader(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "member-signalled")
	leader, leaderDone, _ := startGroupLeader(t, groupWithMemberScript, marker)

	if err := signalPID(leader, false); err != nil {
		t.Fatalf("signal the group leader: %v", err)
	}

	select {
	case <-leaderDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the group leader survived SIGTERM")
	}
	if !waitForFile(marker, 5*time.Second) {
		t.Fatal("signalling the group leader did not reach the rest of its group")
	}
}
