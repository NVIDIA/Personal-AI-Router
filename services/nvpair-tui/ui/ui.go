// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"bufio"
	"io"

	"nvpair-tui/pairing"
	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
)

// Deps is everything the tabbed program is built over. The broker's
// notification stream arrives as a channel rather than straight off the
// client, because the process fans those pushes out to the pairing service
// first — the Cluster tab is one consumer of pairing state, not its owner.
type Deps struct {
	// Client issues broker requests.
	Client *rpc.Client
	// Notifications carries the broker's pushes. Closed when the broker goes
	// away, which is how the UI learns it disconnected.
	Notifications <-chan *rpc.Message
	// Stderr is the broker's captured stderr, shown in the Logs tab.
	Stderr io.Reader
	// Pairing is the process-wide pairing service, shared with the control
	// socket so an invite created from a script shows up here too.
	Pairing *pairing.Service
}

// Run builds the tabbed program over its dependencies and blocks until the
// user quits. The caller is responsible for shutting the broker down
// afterwards.
func Run(deps Deps) error {
	logCh := make(chan string, 2000)
	go scanLines(deps.Stderr, logCh)

	p := tea.NewProgram(
		New(deps, logCh, defaultViews(deps)),
		tea.WithAltScreen(),
	)
	_, err := p.Run()
	return err
}

// scanLines forwards each line of r onto out, closing out at EOF. The
// buffer matches the broker's so a long structured log line is never
// split mid-record.
func scanLines(r io.Reader, out chan<- string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		out <- sc.Text()
	}
	close(out)
}

// defaultViews lists the tabs in display order.
func defaultViews(deps Deps) []View {
	client := deps.Client
	return []View{
		newHealthView(client),
		newErrorsView(client),
		newNodesView(client, deps.Pairing),
		newProxiesView(client),
		newWorkloadsView(client),
		newEnginesView(client),
		newClusterView(client, deps.Pairing),
		newManualView(client),
		newSettingsView(client),
		newLogsView(client),
	}
}
