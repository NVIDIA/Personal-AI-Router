// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command stubbroker is a test fixture: the smallest nvpair-ui-broker a real
// nvpair-tui will accept as its parent.
//
// It exists because the real broker's ports are compiled-in constants — one
// broker per machine — while a pairing needs *two* independent nodes. So this
// spawns nothing but a real nvpair-cluster-manager on a port the test chose,
// and relays the one namespace pairing needs (cluster:* / nodes:*) between the
// TUI above and the manager below, in both directions and byte for byte.
//
// Everything the assertions turn on stays real: a real nvpair-tui process, its
// real control socket and subcommands, and a real PIN-authenticated EAP-NOOB
// exchange between two real cluster managers.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"nvpair-shared/jsonrpc"
)

// maxFrame matches the broker's own read buffer, so a large roster snapshot is
// never truncated mid-frame on its way through.
const maxFrame = 1024 * 1024

func main() {
	managerPath := flag.String("cluster-manager-path", "", "path to the nvpair-cluster-manager binary")
	configDir := flag.String("config-dir", "", "cluster identity and trusted-store directory")
	port := flag.Int("port", 0, "inter-node pairing port for the cluster manager")
	flag.Parse()

	if *managerPath == "" || *configDir == "" || *port == 0 {
		log.Fatal("stubbroker needs --cluster-manager-path, --config-dir and --port")
	}

	manager := exec.Command(*managerPath, "--config-dir", *configDir, "--port", strconv.Itoa(*port))
	manager.Stderr = os.Stderr
	managerIn, err := manager.StdinPipe()
	if err != nil {
		log.Fatalf("cluster-manager stdin: %v", err)
	}
	managerOut, err := manager.StdoutPipe()
	if err != nil {
		log.Fatalf("cluster-manager stdout: %v", err)
	}
	if err := manager.Start(); err != nil {
		log.Fatalf("start cluster-manager: %v", err)
	}

	up := newLink(os.Stdin, os.Stdout)
	down := newLink(managerOut, managerIn)

	// The TUI's header and the control socket's ping both wait on this.
	up.write(&jsonrpc.Message{
		JSONRPC: "2.0",
		Method:  "app:ready",
		Params:  json.RawMessage(`{"version":"stub"}`),
	})

	// Manager to TUI. Results keep their ids and notifications pass through
	// untouched — cluster:invite-received above all, which is the only way the
	// invited side learns it has something to answer.
	var pump sync.WaitGroup
	pump.Add(1)
	go func() {
		defer pump.Done()
		for {
			msg, ok := down.read()
			if !ok {
				return
			}
			up.write(msg)
		}
	}()

	shutdown := func() {
		_ = managerIn.Close()
		_ = manager.Wait()
		pump.Wait()
	}

	for {
		msg, ok := up.read()
		if !ok {
			// stdin EOF: the TUI is gone, so take the manager with us.
			shutdown()
			return
		}
		if !msg.IsRequest() {
			continue
		}
		switch {
		case msg.Method == "shutdown":
			up.write(&jsonrpc.Message{JSONRPC: "2.0", ID: msg.ID, Result: json.RawMessage(`{"ok":true}`)})
			shutdown()
			return
		case msg.Method == "ping":
			up.write(&jsonrpc.Message{JSONRPC: "2.0", ID: msg.ID, Result: json.RawMessage(`{"version":"stub","uptimeMs":0}`)})
		case strings.HasPrefix(msg.Method, "cluster:"), strings.HasPrefix(msg.Method, "nodes:"):
			down.write(msg)
		default:
			// Every other broker method is out of this fixture's scope. The
			// TUI renders an unknown-method error as a tab with no data,
			// which is exactly right here.
			up.write(&jsonrpc.Message{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error:   &jsonrpc.RPCError{Code: -32601, Message: "stubbroker relays cluster:* and nodes:* only"},
			})
		}
	}
}

// link is one newline-delimited JSON-RPC direction. Frames are relayed as
// decoded-and-re-encoded Messages rather than through the shared codec's typed
// helpers, because a relay has to be able to pass a *request* along, which
// those helpers deliberately do not expose.
type link struct {
	scanner *bufio.Scanner
	out     io.Writer
	mu      sync.Mutex
}

func newLink(r io.Reader, w io.Writer) *link {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxFrame)
	return &link{scanner: scanner, out: w}
}

// read returns the next frame, or false once the stream ends. A line that will
// not decode is skipped rather than treated as the end.
func (l *link) read() (*jsonrpc.Message, bool) {
	for l.scanner.Scan() {
		var msg jsonrpc.Message
		if err := json.Unmarshal(l.scanner.Bytes(), &msg); err != nil {
			continue
		}
		return &msg, true
	}
	return nil, false
}

func (l *link) write(msg *jsonrpc.Message) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	data = append(data, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.out.Write(data)
}
