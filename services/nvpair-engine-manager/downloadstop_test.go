// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// fakeDownloadProcess records the signals stopDownload sends it, and exits on
// the one its case names.
type fakeDownloadProcess struct {
	exited chan struct{}
	// exitOn counts the signals sent, from one, to the one the process exits
	// on. Zero means it never exits.
	exitOn int
	// interruptErr, when set, is why no interrupt can be sent.
	interruptErr error
	sent         []string
}

func newFakeDownloadProcess(exitOn int) *fakeDownloadProcess {
	return &fakeDownloadProcess{exited: make(chan struct{}), exitOn: exitOn}
}

func (f *fakeDownloadProcess) process() downloadProcess {
	signal := func(name string) {
		f.sent = append(f.sent, name)
		if len(f.sent) == f.exitOn {
			close(f.exited)
		}
	}
	return downloadProcess{
		exited: f.exited,
		interrupt: func() error {
			if f.interruptErr != nil {
				return f.interruptErr
			}
			signal("interrupt")
			return nil
		},
		kill: func() { signal("kill") },
	}
}

// A process is pushed only as far as it takes to stop it, and one that even
// the kill cannot remove is given up on rather than waited for.
func TestStopDownload_EscalatesOnlyAsFarAsTheProcessNeeds(t *testing.T) {
	graces := downloadStopGraces{interrupt: 50 * time.Millisecond, retry: 50 * time.Millisecond, reap: 50 * time.Millisecond}
	test := func(name string, process *fakeDownloadProcess, graces downloadStopGraces, want downloadStop, wantSent string) {
		t.Run(name, func(t *testing.T) {
			got := stopDownload(process.process(), graces)

			assert.Equal(t, want, got)
			assert.Equal(t, wantSent, strings.Join(process.sent, " "))
		})
	}
	test("it exits when asked", newFakeDownloadProcess(1), graces,
		downloadStop{exited: true}, "interrupt")
	test("it exits when asked again", newFakeDownloadProcess(2), graces,
		downloadStop{exited: true}, "interrupt interrupt")
	noRetry := graces
	noRetry.retry = 0
	test("a tool that allows no retry is killed instead", newFakeDownloadProcess(2), noRetry,
		downloadStop{killed: true, exited: true}, "interrupt kill")
	test("only the kill removes it", newFakeDownloadProcess(3), graces,
		downloadStop{killed: true, exited: true}, "interrupt interrupt kill")
	test("not even the kill removes it", newFakeDownloadProcess(0), graces,
		downloadStop{killed: true}, "interrupt interrupt kill")
}

// A reaped process's PID is the OS's to reissue, and every signal reaches more
// than the process, so one that has already exited is sent nothing.
func TestStopDownload_SendsNothingToAProcessThatHasExited(t *testing.T) {
	process := newFakeDownloadProcess(0)
	close(process.exited)

	got := stopDownload(process.process(), downloadStopGraces{interrupt: time.Minute, reap: time.Minute})

	assert.Equal(t, downloadStop{interruptErr: os.ErrProcessDone, exited: true}, got)
	assert.Empty(t, process.sent)
}

// A process nothing could ask to stop is not given the time it would have had
// to answer.
func TestStopDownload_KillsAtOnceWhenItCannotAsk(t *testing.T) {
	process := newFakeDownloadProcess(1)
	process.interruptErr = errors.New("no console to send the interrupt to")
	start := time.Now()

	got := stopDownload(process.process(), downloadStopGraces{interrupt: 10 * time.Second, retry: 10 * time.Second, reap: time.Minute})

	assert.Equal(t, downloadStop{interruptErr: process.interruptErr, killed: true, exited: true}, got)
	assert.Equal(t, []string{"kill"}, process.sent)
	assert.Less(t, time.Since(start), 5*time.Second, "the interrupt's grace was waited out")
}
