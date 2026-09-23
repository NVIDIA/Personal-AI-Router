// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"sort"
	"strings"

	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// catalogModel is one downloadable model from engine:catalog.
type catalogModel struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Size          uint64   `json:"size"`
	Downloads     int      `json:"downloads"`
	UpdatedAt     string   `json:"updatedAt"`
	Tags          []string `json:"tags"`
	Family        string   `json:"family"`
	ParameterSize string   `json:"parameterSize"`
}

// catalogSort is the order the browser lists models in.
type catalogSort int

const (
	// catalogSortDefault is the order the backend returned: most-downloaded
	// first for a fetched catalogue, library order for a committed one. It leads
	// because it is the most useful answer to "what should I get".
	catalogSortDefault catalogSort = iota
	catalogSortName
	catalogSortSize
)

func (s catalogSort) String() string {
	switch s {
	case catalogSortName:
		return "name"
	case catalogSortSize:
		return "size"
	default:
		return "popularity"
	}
}

type catalogLoadedMsg struct {
	engine    string
	models    []catalogModel
	fetchedAt string
	// platform is the OS the served list was filtered for.
	platform string
	err      error
}

var (
	catalogSearchKey = key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search"))
	catalogSortKey   = key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "sort"))
	catalogGetKey    = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "download"))
	catalogCloseKey  = key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back"))
	// Matches the Logs tab, the other view with a filter that survives being
	// typed. Not esc, which already closes the browser here — a filter matching
	// nothing otherwise left throwing the whole catalogue away as the only exit.
	catalogClearKey = key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "clear filter"))
)

// catalogBrowser lists the models an engine can download, with a filter.
//
// It exists because the alternative was typing a model name from memory: the
// download worked, but only if you already knew what to ask for. The catalogue
// comes from the backend's engine:catalog rather than being assembled here, so
// the terminal interface and the desktop app offer the same models.
//
// Filtering is local over the whole fetched list. The catalogue is thousands of
// entries for Ollama and there is no server-side search, so a keystroke filters
// what is already in memory instead of issuing a request.
type catalogBrowser struct {
	client *rpc.Client
	engine string
	// engineLabel is the engine's display name, for the heading.
	engineLabel string
	// nodeLabel is the machine the download lands on. The browser is reached
	// from a peer's detail screen as well as this machine's, and a heading that
	// does not say which one invites downloading gigabytes to the wrong host.
	nodeLabel string

	all       []catalogModel
	shown     []catalogModel
	table     table.Model
	sortBy    catalogSort
	filter    string
	input     textinput.Model
	searching bool

	loading   bool
	fetchedAt string
	// platform is the OS the catalogue was filtered for, and remote marks a
	// browse aimed at a peer. Together they say whether the list applies to the
	// target: the catalogue is served by THIS machine's engine-manager and
	// filtered for its platform, so a peer running another OS can be shown
	// models it cannot install, or have usable ones hidden.
	platform string
	remote   bool
	status   toast

	width, height int
}

// remote marks a browse aimed at a peer. The catalogue is served by this
// machine's engine-manager and filtered for this machine's platform, so a
// peer-targeted list carries a caveat rather than pretending to be authoritative
// for that peer.
func newCatalogBrowser(client *rpc.Client, engine, label, node string, remote bool) *catalogBrowser {
	ti := textinput.New()
	ti.Placeholder = "filter by name"
	b := &catalogBrowser{
		client:      client,
		engine:      engine,
		engineLabel: label,
		nodeLabel:   node,
		remote:      remote,
		input:       ti,
		loading:     true,
	}
	b.table = newTable(catalogColumns(defaultTableWidth))
	return b
}

func catalogColumns(w int) []table.Column {
	return layoutColumns(w, []column{
		flexCol("MODEL", 20, 3),
		fixedCol("SIZE", 9),
		fixedCol("PARAMS", 7),
		flexCol("FAMILY", 8, 1),
	})
}

func (b *catalogBrowser) Init() tea.Cmd {
	engine := b.engine
	return call(b.client, "engine:catalog", map[string]string{"engine": engine},
		func(msg *rpc.Message, err error) tea.Msg {
			if err != nil {
				return catalogLoadedMsg{engine: engine, err: err}
			}
			var r struct {
				Models    []catalogModel `json:"models"`
				FetchedAt string         `json:"fetchedAt"`
				Platform  string         `json:"platform"`
			}
			_ = decodeParams(msg.Result, &r)
			return catalogLoadedMsg{
				engine:    engine,
				models:    r.Models,
				fetchedAt: r.FetchedAt,
				platform:  r.Platform,
			}
		})
}

// SetSize records the budget and fixes the table's width. Its height is set in
// View, from the chrome actually being rendered — see fitTable.
func (b *catalogBrowser) SetSize(w, h int) {
	b.width, b.height = w, h
	b.table.SetColumns(catalogColumns(w))
	b.table.SetWidth(w)
}

// update handles a message, returning a command, the model to download when the
// operator picked one, and whether the browser should stay open.
func (b *catalogBrowser) update(msg tea.Msg) (tea.Cmd, string, bool) {
	switch msg := msg.(type) {
	case catalogLoadedMsg:
		if msg.engine != b.engine {
			return nil, "", true
		}
		b.loading = false
		if msg.err != nil {
			b.status.error("could not load the %s catalog: %s", b.engineLabel, msg.err)
			return nil, "", true
		}
		b.all = msg.models
		b.fetchedAt = msg.fetchedAt
		b.platform = msg.platform
		b.refresh()
		return nil, "", true

	case tea.KeyMsg:
		return b.handleKey(msg)
	}
	return nil, "", true
}

func (b *catalogBrowser) handleKey(msg tea.KeyMsg) (tea.Cmd, string, bool) {
	if b.searching {
		switch msg.String() {
		case "enter":
			b.filter = strings.TrimSpace(b.input.Value())
			b.searching = false
			b.input.Blur()
			b.refresh()
			return nil, "", true
		case "esc":
			b.searching = false
			b.input.Blur()
			return nil, "", true
		}
		var cmd tea.Cmd
		b.input, cmd = b.input.Update(msg)
		return cmd, "", true
	}

	switch {
	case key.Matches(msg, catalogCloseKey):
		return nil, "", false
	case key.Matches(msg, catalogSearchKey):
		b.searching = true
		b.input.SetValue(b.filter)
		b.input.Focus()
		return textinput.Blink, "", true
	case key.Matches(msg, catalogClearKey) && b.filter != "":
		b.filter = ""
		b.refresh()
		return nil, "", true
	case key.Matches(msg, catalogSortKey):
		b.sortBy = (b.sortBy + 1) % 3
		b.refresh()
		return nil, "", true
	case key.Matches(msg, catalogGetKey):
		if m := b.selected(); m != nil {
			// Name is the pull-ready string the backend guarantees; it is what
			// the engine's download action accepts verbatim.
			return nil, m.Name, false
		}
		return nil, "", true
	}

	var cmd tea.Cmd
	b.table, cmd = b.table.Update(msg)
	return cmd, "", true
}

func (b *catalogBrowser) selected() *catalogModel {
	idx := b.table.Cursor()
	if idx < 0 || idx >= len(b.shown) {
		return nil
	}
	return &b.shown[idx]
}

// refresh reapplies the filter and sort, then repaints.
func (b *catalogBrowser) refresh() {
	needle := strings.ToLower(b.filter)
	b.shown = b.shown[:0]
	for _, m := range b.all {
		if needle != "" && !catalogMatches(m, needle) {
			continue
		}
		b.shown = append(b.shown, m)
	}
	b.sortShown()

	rows := make([]table.Row, 0, len(b.shown))
	for _, m := range b.shown {
		size := "-"
		if m.Size > 0 {
			size = humanBytes(m.Size)
		}
		params := m.ParameterSize
		if params == "" {
			params = "-"
		}
		family := m.Family
		if family == "" {
			family = "-"
		}
		rows = append(rows, table.Row{m.Name, size, params, family})
	}
	b.table.SetRows(rows)
	b.table.SetCursor(0)
}

// catalogMatches tests a model against a lowercase needle. Family and parameter
// size are searched too, so "llama" and "8b" both narrow usefully.
func catalogMatches(m catalogModel, needle string) bool {
	if strings.Contains(strings.ToLower(m.Name), needle) {
		return true
	}
	if strings.Contains(strings.ToLower(m.Family), needle) {
		return true
	}
	if strings.Contains(strings.ToLower(m.ParameterSize), needle) {
		return true
	}
	for _, t := range m.Tags {
		if strings.Contains(strings.ToLower(t), needle) {
			return true
		}
	}
	return false
}

func (b *catalogBrowser) sortShown() {
	switch b.sortBy {
	case catalogSortName:
		sort.SliceStable(b.shown, func(i, j int) bool {
			return strings.ToLower(b.shown[i].Name) < strings.ToLower(b.shown[j].Name)
		})
	case catalogSortSize:
		// Largest first, and models with no reported size sink rather than
		// leading the list as if they were empty.
		sort.SliceStable(b.shown, func(i, j int) bool {
			return b.shown[i].Size > b.shown[j].Size
		})
	default:
		// The backend's order is already the intended default.
	}
}

func (b *catalogBrowser) View() string {
	// Naming the machine, because this screen is reached from a peer's detail
	// as well as this one's and the download lands wherever it was opened from.
	heading := titleStyle.Render(fmt.Sprintf("Download a model for %s on %s",
		b.engineLabel, b.nodeLabel))

	// One of these three occupies the last row, in this order of precedence.
	footer := footerStyle.Render(b.summary())
	if s := b.status.render(); s != "" {
		footer = s
	}
	if b.searching {
		footer = "filter: " + b.input.View()
	}

	body := ""
	switch {
	case b.loading:
		body = footerStyle.Render("  Loading the catalog...")
	case len(b.all) == 0:
		body = footerStyle.Render("  No catalog available for this engine.")
	case len(b.shown) == 0:
		body = footerStyle.Render(fmt.Sprintf(
			"  Nothing matches %q. Press / to change it, c to clear it, or esc to close.",
			b.filter))
	case fitTable(&b.table, b.height, heading, footer):
		body = b.table.View()
	default:
		body = footerStyle.Render("  (too little room to list models)")
	}

	return joinLines(heading, body, footer)
}

func (b *catalogBrowser) summary() string {
	if b.loading {
		return ""
	}
	parts := []string{fmt.Sprintf("%d of %d models", len(b.shown), len(b.all))}
	if b.filter != "" {
		parts[0] = fmt.Sprintf("%d of %d matching %q", len(b.shown), len(b.all), b.filter)
	}
	parts = append(parts, "sorted by "+b.sortBy.String())
	if b.fetchedAt != "" {
		parts = append(parts, "catalog "+shortDate(b.fetchedAt))
	}
	// Said out loud when it might be wrong. The list comes from this machine's
	// engine-manager and is filtered for its platform, so a peer on a different
	// OS may be offered a model it cannot install. Better to state the basis
	// than to let a filtered list look authoritative for another machine.
	if b.remote && b.platform != "" {
		parts = append(parts, "filtered for "+b.platform)
	}
	return strings.Join(parts, "   ")
}

// shortDate trims an RFC3339 timestamp to its date, which is the only part that
// matters for judging how current a catalogue is.
func shortDate(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

func (b *catalogBrowser) Help() []key.Binding {
	if b.searching {
		return inputHelp("apply filter")
	}
	bindings := []key.Binding{catalogCloseKey, catalogSearchKey, catalogSortKey, catalogGetKey}
	if b.filter != "" {
		bindings = append(bindings, catalogClearKey)
	}
	return bindings
}
