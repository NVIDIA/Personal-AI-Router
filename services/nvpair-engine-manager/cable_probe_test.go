// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"nvpair-shared/cableprobe"
)

const probeTestMarker = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// Descriptor numbers are synthetic map keys. No test calls nativeCableProbeIO.
type probeTestIO struct {
	mu                                                          sync.Mutex
	owned                                                       map[int]bool
	bound                                                       map[int]int
	closed                                                      []int
	writes                                                      map[int][]cableprobe.Frame
	lockCalls, openCalls, configureCalls, readCalls, writeCalls int
	nextFD                                                      int
	busy                                                        bool
	failOpen, failConfigure, failClose                          int
	onRead                                                      func(time.Duration) (cableProbePacket, error)
	onWrite                                                     func() error
}

func newProbeTestIO() *probeTestIO {
	return &probeTestIO{owned: map[int]bool{}, bound: map[int]int{}, writes: map[int][]cableprobe.Frame{}, nextFD: 10, failClose: -1}
}

func (f *probeTestIO) io() cableProbeIO {
	return cableProbeIO{
		validate: func(cableprobe.Interface, cableprobe.PortRef) error { return nil },
		lock: func() (int, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.lockCalls++
			if f.busy {
				return -1, errors.New("synthetic reservation busy")
			}
			f.busy, f.owned[100] = true, true
			return 100, nil
		},
		open: func() (int, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.openCalls++
			if f.openCalls == f.failOpen {
				return -1, errors.New("synthetic raw permission unavailable")
			}
			fd := f.nextFD
			f.nextFD++
			f.owned[fd] = true
			return fd, nil
		},
		configure: func(fd int, alias cableprobe.Interface, port cableprobe.PortRef) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.configureCalls++
			if !f.owned[fd] || fd == 100 {
				return errors.New("configure used unowned descriptor")
			}
			if f.configureCalls == f.failConfigure {
				return errors.New("synthetic configure failure")
			}
			f.bound[fd] = alias.Index
			return nil
		},
		read: func(fds []int, wait time.Duration) (cableProbePacket, error) {
			f.mu.Lock()
			f.readCalls++
			for _, fd := range fds {
				if !f.owned[fd] || f.bound[fd] == 0 {
					f.mu.Unlock()
					return cableProbePacket{}, errors.New("read used unowned or unbound descriptor")
				}
			}
			f.mu.Unlock()
			if wait < 0 || wait > 50*time.Millisecond {
				return cableProbePacket{}, errors.New("read wait exceeded native bound")
			}
			if f.onRead != nil {
				return f.onRead(wait)
			}
			time.Sleep(wait) // This default is used only inside synctest.Test.
			return cableProbePacket{}, errCableProbeIdle
		},
		write: func(fd int, data []byte) error {
			frame, err := cableprobe.DecodeFrame(data)
			if err != nil {
				return err
			}
			f.mu.Lock()
			f.writeCalls++
			owned := f.owned[fd] && f.bound[fd] != 0
			f.mu.Unlock()
			if !owned {
				return errors.New("write used unowned or unbound descriptor")
			}
			if f.onWrite != nil {
				if err := f.onWrite(); err != nil {
					return err
				}
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if !f.owned[fd] {
				return errors.New("descriptor released during write")
			}
			f.writes[fd] = append(f.writes[fd], frame)
			return nil
		},
		close: func(fd int) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.closed = append(f.closed, fd)
			if !f.owned[fd] {
				return errors.New("descriptor closed twice or never owned")
			}
			if fd == f.failClose {
				return errors.New("synthetic close failure")
			}
			delete(f.owned, fd)
			if fd == 100 {
				f.busy = false
			}
			return nil
		},
	}
}

func (f *probeTestIO) counts() (calls, owned, closed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lockCalls + f.openCalls + f.configureCalls + f.readCalls + f.writeCalls + len(f.closed), len(f.owned), len(f.closed)
}

func probeTestTargets(ports, aliases int) []cableprobe.Target {
	local := cableprobe.Target{NodeID: "local", Principal: "principal-local", Ports: []cableprobe.Port{}}
	for p := 0; p < ports; p++ {
		port := cableprobe.Port{SwitchID: "local-switch", PortName: fmt.Sprintf("p%d", p)}
		for a := 0; a < aliases; a++ {
			index := p*aliases + a + 1
			port.Interfaces = append(port.Interfaces, cableprobe.Interface{Name: fmt.Sprintf("eth%d", index), Index: index, MAC: fmt.Sprintf("02:00:00:00:00:%02x", index)})
		}
		local.Ports = append(local.Ports, port)
	}
	return []cableprobe.Target{local, {NodeID: "peer", Principal: "principal-peer", Ports: []cableprobe.Port{{SwitchID: "peer-switch", PortName: "p0", Interfaces: []cableprobe.Interface{{Name: "peer0", Index: 20, MAC: "02:00:00:00:00:80"}}}}}}
}

func probeTestCurrent(context.Context) error { return nil }

func probeTestPrepare(t *testing.T, f *probeTestIO, targets []cableprobe.Target, current func(context.Context) error) *cableProbeSession {
	t.Helper()
	s, err := prepareCableProbe(context.Background(), true, "local", probeTestMarker, targets, f.io(), current)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func probeTestPacket(t *testing.T, at time.Time, sequence uint32) cableProbePacket {
	t.Helper()
	data, err := cableprobe.EncodeFrame("02:00:00:00:00:80", probeTestMarker, sequence)
	if err != nil {
		t.Fatal(err)
	}
	return cableProbePacket{Data: data, Index: 1, Kind: 2, ReceivedAt: at}
}

func TestCableProbeAdmissionBeforeIO(t *testing.T) {
	f := newProbeTestIO()
	currentCalls := 0
	_, err := prepareCableProbe(context.Background(), false, "local", probeTestMarker, probeTestTargets(1, 1), f.io(), func(context.Context) error { currentCalls++; return nil })
	if !errors.Is(err, errCableProbePermission) {
		t.Fatalf("unapproved launch was not refused: %v", err)
	}
	if calls, _, _ := f.counts(); calls != 0 || currentCalls != 0 {
		t.Fatal("unapproved launch touched I/O or current-state provider")
	}
	for _, name := range []string{"foreign-local", "single-target", "duplicate-node", "duplicate-principal", "invalid-index", "invalid-mac", "five-aliases", "duplicate-alias", "duplicate-port", "invalid-marker"} {
		t.Run(name, func(t *testing.T) {
			f := newProbeTestIO()
			targets, local, marker := probeTestTargets(1, 2), "local", probeTestMarker
			switch name {
			case "foreign-local":
				local = "foreign"
			case "single-target":
				targets = targets[:1]
			case "duplicate-node":
				targets[1].NodeID = targets[0].NodeID
			case "duplicate-principal":
				targets[1].Principal = targets[0].Principal
			case "invalid-index":
				targets[0].Ports[0].Interfaces[0].Index = 0
			case "invalid-mac":
				targets[0].Ports[0].Interfaces[0].MAC = "01:00:00:00:00:01"
			case "five-aliases":
				targets = probeTestTargets(1, 5)
			case "duplicate-alias":
				targets[0].Ports[0].Interfaces[1].Index = targets[0].Ports[0].Interfaces[0].Index
			case "duplicate-port":
				targets[0].Ports = append(targets[0].Ports, targets[0].Ports[0])
			case "invalid-marker":
				marker = "unreviewed"
			}
			if _, err := prepareCableProbe(context.Background(), true, local, marker, targets, f.io(), probeTestCurrent); err == nil {
				t.Fatal("invalid preparation accepted")
			}
			if calls, _, _ := f.counts(); calls != 0 {
				t.Fatal("invalid preparation acquired native resources")
			}
		})
	}
}

func TestCableProbePreparationOwnership(t *testing.T) {
	for _, stage := range []string{"first-open", "second-open", "second-configure"} {
		t.Run(stage, func(t *testing.T) {
			f := newProbeTestIO()
			switch stage {
			case "first-open":
				f.failOpen = 1
			case "second-open":
				f.failOpen = 2
			case "second-configure":
				f.failConfigure = 2
			}
			if _, err := prepareCableProbe(context.Background(), true, "local", probeTestMarker, probeTestTargets(1, 2), f.io(), probeTestCurrent); err == nil {
				t.Fatal("acquisition failure accepted")
			}
			_, owned, closed := f.counts()
			if owned != 0 || closed != f.nextFD-10+1 || f.closed[len(f.closed)-1] != 100 || f.writeCalls != 0 {
				t.Fatalf("partial preparation leaked or sent: owned=%d closed=%v sends=%d", owned, f.closed, f.writeCalls)
			}
		})
	}
	f := newProbeTestIO()
	s := probeTestPrepare(t, f, probeTestTargets(1, 2), probeTestCurrent)
	if f.writeCalls != 0 || f.readCalls != 0 {
		t.Fatal("preparation performed active I/O")
	}
	if _, err := prepareCableProbe(context.Background(), true, "local", probeTestMarker, probeTestTargets(1, 2), f.io(), probeTestCurrent); err == nil {
		t.Fatal("second preparation bypassed node reservation")
	}
	if f.openCalls != 2 {
		t.Fatal("busy reservation opened more raw descriptors")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = probeTestPrepare(t, f, probeTestTargets(1, 1), probeTestCurrent)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, owned, _ := f.counts(); owned != 0 {
		t.Fatal("reservation did not become reusable after cleanup")
	}
}

func TestCableProbeFiniteWindowAndPhysicalPortSendBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newProbeTestIO()
		targets := probeTestTargets(2, 2)
		s := probeTestPrepare(t, f, targets, probeTestCurrent)
		// Caller mutation cannot alter the immutable prepared aliases.
		targets[0].Ports[0].Interfaces[0].MAC = "00:00:00:00:00:00"
		started := time.Now()
		result := s.Run(context.Background())
		if elapsed := time.Since(started); elapsed != cableprobe.Window {
			t.Fatalf("run duration=%v, want finite20s", elapsed)
		}
		if result.State != "completed" || result.Sent != 40 || !result.CleanupConfirmed || result.Directness != "unverified" {
			t.Fatalf("incorrect bounded run result: %+v", result)
		}
		if len(f.bound) != 4 || len(f.writes) != 2 {
			t.Fatal("alias reception or once-per-physical-port transmission changed")
		}
		for _, fd := range []int{10, 12} {
			frames := f.writes[fd]
			if len(frames) != 20 {
				t.Fatalf("sender%d emitted%d frames", fd, len(frames))
			}
			for i, frame := range frames {
				if frame.Sequence != uint32(i+1) || frame.RunMarker != probeTestMarker {
					t.Fatal("unexpected run marker or sequence")
				}
			}
		}
		if _, owned, closed := f.counts(); owned != 0 || closed != 5 {
			t.Fatal("normal completion did not close every raw descriptor and reservation")
		}
		before := f.writeCalls
		if again := s.Run(context.Background()); again.State != "failed" || f.writeCalls != before {
			t.Fatal("used session ran a second time")
		}
	})
}

func TestCableProbeInvalidIngressTimestampAndProfile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newProbeTestIO()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		step := 0
		f.onRead = func(wait time.Duration) (cableProbePacket, error) {
			step++
			if step <= 40 {
				time.Sleep(wait)
				return cableProbePacket{}, errCableProbeIdle
			}
			time.Sleep(time.Millisecond)
			packet := probeTestPacket(t, time.Now(), 1)
			switch step - 40 {
			case 1:
				packet.Truncated = true
			case 2:
				packet.Index = 99
			case 3:
				packet.Kind = 4
			case 4:
				packet.Data, _ = cableprobe.EncodeFrame("02:00:00:00:00:80", strings.Repeat("b", 32), 1)
			case 5:
				packet.Data = []byte("malformed profile")
			case 6:
				packet.Data, _ = cableprobe.EncodeFrame("02:00:00:00:00:01", probeTestMarker, 1)
			case 7:
				packet.Data, _ = cableprobe.EncodeFrame("02:00:00:00:00:99", probeTestMarker, 1)
			case 8:
				packet.ReceivedAt = time.Time{}
			case 9:
				packet.ReceivedAt = time.Now().Add(-3 * time.Second)
			case 10:
				packet.ReceivedAt = time.Now().Add(-2 * time.Second)
			case 11:
				packet.ReceivedAt = time.Now().Add(time.Millisecond)
			default:
				cancel()
				return cableProbePacket{}, errCableProbeIdle
			}
			return packet, nil
		}
		s := probeTestPrepare(t, f, probeTestTargets(1, 1), probeTestCurrent)
		result := s.Run(ctx)
		if result.State != "cancelled" || len(result.Observations) != 0 || !result.CleanupConfirmed {
			t.Fatalf("invalid frames became observations: %+v", result)
		}
	})
}

func TestCableProbeReplayDoesNotRenewObservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newProbeTestIO()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		step := 0
		var acceptedAt time.Time
		f.onRead = func(time.Duration) (cableProbePacket, error) {
			step++
			switch step {
			case 1:
				acceptedAt = time.Now()
				return probeTestPacket(t, acceptedAt, 2), nil
			case 2:
				time.Sleep(20 * time.Millisecond)
				return probeTestPacket(t, time.Now(), 2), nil
			default:
				cancel()
				return cableProbePacket{}, errCableProbeIdle
			}
		}
		s := probeTestPrepare(t, f, probeTestTargets(1, 1), probeTestCurrent)
		result := s.Run(ctx)
		if result.State != "cancelled" || len(result.Observations) != 1 || !result.CleanupConfirmed {
			t.Fatalf("valid observation was lost or replay duplicated it: %+v", result)
		}
		observation := result.Observations[0]
		if observation.Local.NodeID != "local" || observation.Peer.NodeID != "peer" || observation.Sequence != 2 || observation.AgeMs != time.Since(acceptedAt).Milliseconds() || observation.AgeMs < 20 {
			t.Fatalf("ingress/replay binding failed: %+v", observation)
		}
	})
}

func TestCableProbeFloodReceiveBound(t *testing.T) {
	f := newProbeTestIO()
	f.onRead = func(time.Duration) (cableProbePacket, error) { return cableProbePacket{Truncated: true}, nil }
	s := probeTestPrepare(t, f, probeTestTargets(1, 1), probeTestCurrent)
	result := s.Run(context.Background())
	if result.State != "failed" || !strings.Contains(result.Message, "budget") || !result.CleanupConfirmed {
		t.Fatalf("flood did not stop and clean up: %+v", result)
	}
	if result.Received > cableProbeReceiveLimit || f.readCalls > cableProbeReceiveLimit {
		t.Fatalf("receive bound%d consumed%d packets via%d reads", cableProbeReceiveLimit, result.Received, f.readCalls)
	}
}

func TestCableProbeCloseJoinsActiveIO(t *testing.T) {
	for _, operation := range []string{"read", "write"} {
		t.Run(operation, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newProbeTestIO()
				entered, release := make(chan struct{}), make(chan struct{})
				if operation == "read" {
					f.onRead = func(time.Duration) (cableProbePacket, error) {
						close(entered)
						<-release
						return cableProbePacket{}, errCableProbeIdle
					}
				} else {
					f.onWrite = func() error { close(entered); <-release; return nil }
				}
				s := probeTestPrepare(t, f, probeTestTargets(1, 1), probeTestCurrent)
				resultCh, closeCh := make(chan cableProbeResult, 1), make(chan error, 2)
				go func() { resultCh <- s.Run(context.Background()) }()
				<-entered
				go func() { closeCh <- s.Close() }()
				go func() { closeCh <- s.Close() }()
				synctest.Wait()
				if _, _, closed := f.counts(); closed != 0 {
					t.Fatal("descriptor released before active I/O joined")
				}
				select {
				case <-closeCh:
					t.Fatal("Close returned while native I/O was active")
				default:
				}
				close(release)
				result := <-resultCh
				for i := 0; i < 2; i++ {
					if err := <-closeCh; err != nil {
						t.Fatal(err)
					}
				}
				if result.State != "cancelled" || !result.CleanupConfirmed {
					t.Fatalf("concurrent close did not cancel cleanly: %+v", result)
				}
				if _, owned, closed := f.counts(); owned != 0 || closed != 2 {
					t.Fatal("concurrent close leaked or double-released descriptors")
				}
			})
		})
	}
}

func TestCableProbeFinalIdentityAndCloseFailures(t *testing.T) {
	for _, failure := range []string{"final-identity", "close"} {
		t.Run(failure, func(t *testing.T) {
			f := newProbeTestIO()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			read := false
			f.onRead = func(time.Duration) (cableProbePacket, error) {
				if !read {
					read = true
					return probeTestPacket(t, time.Now(), 1), nil
				}
				cancel()
				return cableProbePacket{}, errCableProbeIdle
			}
			current := probeTestCurrent
			if failure == "final-identity" {
				current = func(context.Context) error {
					if _, _, closed := f.counts(); closed > 0 {
						return errors.New("synthetic final identity changed")
					}
					return nil
				}
			} else {
				f.failClose = 10
			}
			s := probeTestPrepare(t, f, probeTestTargets(1, 1), current)
			result := s.Run(ctx)
			if result.State != "failed" {
				t.Fatalf("failure was hidden: %+v", result)
			}
			if failure == "final-identity" && (len(result.Observations) != 0 || !result.CleanupConfirmed || !strings.Contains(result.Message, "Final reviewed identity")) {
				t.Fatalf("changed identity retained observations: %+v", result)
			}
			if failure == "close" && (result.CleanupConfirmed || !strings.Contains(result.Message, "cleanup")) {
				t.Fatalf("failed close was reported clean: %+v", result)
			}
		})
	}
}
