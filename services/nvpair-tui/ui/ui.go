// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"bufio"
	"io"

	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
)

// Run builds the tabbed program over a connected broker client and the
// broker's stderr stream, and blocks until the user quits. The caller is
// responsible for shutting the broker down afterwards.
func Run(client *rpc.Client, stderr io.Reader) error {
	logCh := make(chan string, 2000)
	go scanLines(stderr, logCh)

	p := tea.NewProgram(
		New(client, logCh, defaultViews(client)),
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
func defaultViews(client *rpc.Client) []View {
	return []View{
		newHealthView(client),
		newErrorsView(client),
		newNodesView(client),
		newProxiesView(client),
		newWorkloadsView(client),
		newEnginesView(client),
		newClusterView(client),
		newManualView(client),
		newSettingsView(client),
		newLogsView(client),
	}
}
