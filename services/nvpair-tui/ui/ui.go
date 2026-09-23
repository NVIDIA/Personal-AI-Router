// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"bufio"
	"io"

	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
)

// Run builds the tabbed program over a connected broker client and the broker's
// stderr stream, and blocks until the user quits. The caller is responsible for
// shutting the broker down afterwards, and for honouring the returned Outcome
// once it has.
func Run(client *rpc.Client, stderr io.Reader) (Outcome, error) {
	logCh := make(chan string, 2000)
	go scanLines(stderr, logCh)

	p := tea.NewProgram(
		New(client, logCh, defaultViews(client)),
		tea.WithAltScreen(),
	)
	final, err := p.Run()
	if m, ok := final.(Model); ok {
		// Before returning, so a demo still inside its window does not leave
		// dispatcher processes behind for the shell to inherit.
		m.close()
		return Outcome{WipeData: m.wipeOnExit}, err
	}
	return Outcome{}, err
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
//
// The tab set is machine-first: Nodes is the primary surface because a node is
// the unit an operator reasons about, and everything specific to one machine
// hangs off its row rather than living in a tab of its own. Diagnostics come
// last, errors before logs, which is the order you consult them in.
//
// Errors is a plain tab rather than an overlay on a dedicated key. As an overlay
// it needed a global binding, and every candidate was either a letter that
// shadowed a view's own verb or a digit that looked like a tab number without
// being one. Its count rides on the tab label instead, so the tab bar is the
// indicator and there is nothing extra to learn.
func defaultViews(client *rpc.Client) []View {
	return []View{
		newNodesView(client),
		newJobsView(client),
		newServiceView(client),
		newErrorsView(client),
		newLogsView(client),
	}
}
