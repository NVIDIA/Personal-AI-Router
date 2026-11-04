// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// The Custom section is a node's custom-endpoint configuration: external
// OpenAI-compatible endpoints its owner configured on or for that node, and the
// one port every endpoint there is reached through.
//
// The endpoints are read-only here. Adding or removing one is a configuration
// change, done on the node that owns the configuration - locally through the
// desktop's per-node editor, remotely by a trusted peer through the pinned
// mTLS channel - never from a screen that opens to look at engines.
//
// The facade's port is the exception: one number, one field, opened with `c`,
// and only while this screen shows this machine. That is the shape the engines'
// proxy-port editor already has. It stops at the machine's own edge - a peer's
// port is shown, never offered for editing - because a channel that reaches
// another node's configuration does not make this screen an editor for it.
//
// The reads happen once when the screen opens, not on a subscription or a tick:
// there is no push channel for endpoint configuration (the broker answers
// custom/list from persisted truth), the list is at most a handful of lines, and
// a stale health flag aging on screen would read as a live fact after the
// endpoint behind it changed. Refresh means reopening the screen, and a facade
// that moved its own port at startup shows up the same way.
//
// Layout sits between the hardware readout and the Engines section. Its
// height is budgeted, not fixed - same discipline as the hardware block,
// which exists because a machine with enough GPUs and an operator with
// enough endpoints, both on one frame, pushed the status line off the screen
// before either block was budgeted.

// customEndpointRow mirrors one custom/list reply entry, on both the local
// and the peer-relayed path. The key never travels: keySet says a credential
// is configured, which is all a browser of this screen can legitimately see.
// Healthy and LastChecked are read when the reply carries them: the broker's
// snapshot merges probe verdicts from the prober's cache, but an endpoint
// with no verdict yet answers healthy=null, which decodes to false - so the
// render must treat "no probe data" as absence of information, never as
// failure.
type customEndpointRow struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	BaseURL     string `json:"baseUrl"`
	KeySet      bool   `json:"keySet"`
	Healthy     bool   `json:"healthy"`
	LastChecked string `json:"lastChecked,omitempty"`
}

// down reports a probe result that says the endpoint is not answering. The
// two absences that are not this: healthy=false with no lastChecked is a
// never-probed entry (an unchecked endpoint answers null and ships no
// timestamp), and an empty reply never reaches a row at all. Marking either
// "down" would state a failure nothing observed.
func (e customEndpointRow) down() bool {
	return e.LastChecked != "" && !e.Healthy
}

// customProxyPort is the Custom facade's own listen port as the addressed node
// reports it: the one number every endpoint above is reached through, which is
// also the number to point an external client at. Running says what kind of
// number Port is - true, something listens there; false, that is where the
// facade will land when it comes up. Port 0 means no answer, which renders no
// line at all: this machine's stock 8000 says nothing about a peer's proxy, and
// an invented number would be read as a fact.
type customProxyPort struct {
	Port    int  `json:"port"`
	Running bool `json:"running"`
}

// line is the port's one-line readout, or "" when there is no port to report.
// A planned port is still shown, because the whole point of reading it is to
// know where the facade is - or will be.
func (p customProxyPort) line() string {
	if p.Port <= 0 {
		return ""
	}
	if p.Running {
		return "proxy port " + strconv.Itoa(p.Port)
	}
	return "proxy port " + strconv.Itoa(p.Port) + " (not listening)"
}

// editable reports whether the port can be moved from here. A facade that is
// not listening cannot be rebound: the owning node refuses that write instead of
// remembering a plan its proxy would ignore on the next start, so a field opened
// against one could only ever come back saying no.
func (p customProxyPort) editable() bool {
	return p.Port > 0 && p.Running
}

// customReadBudget is the time one Custom read may take - the endpoint list or
// the facade port, which are one round trip apart rather than two. A peer's
// answer crosses the cluster twice - the origin's thirty-second dial budget
// plus the owner's relay - and the desktop gives the same round trip
// forty-five seconds for the same reason: a healthy answer arrives inside a
// second, and everything that does not is already the timeout case. The
// shell's generic thirty-five-second call deadline would report failure
// before a slow-but-live peer could answer, which was the exact story the
// budgeted calls exist to kill.
const customReadBudget = 45 * time.Second

// customMaxRows bounds the section the way the hardware block is bounded.
// The endpoints are configuration the operator already knows; the engines
// below deserve the frame.
const customMaxRows = 6

// customSectionChrome counts the rows the section spends on itself: the
// heading, plus the "...and N more" notice once the list is clipped. The port
// line joins that count when the node reported one. Budget and render both read
// it, so the rows left for endpoints cannot be counted twice in two ways.
const customSectionChrome = 2

// customSection is the section's whole state: what the last read answered, or
// the message it answered with, plus the facade port as of the same read. There
// is deliberately no staleness flag; the reads happen once.
type customSection struct {
	endpoints []customEndpointRow
	note      string
	port      customProxyPort
}

// detailCustomMsg is the outcome of the open-time custom/list read.
type detailCustomMsg struct {
	endpoints []customEndpointRow
	note      string
}

// detailCustomPortMsg is the outcome of the open-time custom/get-port read, and
// customPortApplyMsg the outcome of a port the operator applied. They are kept
// apart because only one of them was asked for: a read is recorded quietly, in
// its own place on the frame, while an applied change answers a keystroke and has
// to be seen in the status line.
type detailCustomPortMsg struct {
	port customProxyPort
}

type customPortApplyMsg struct {
	port customProxyPort
	err  error
}

// customReadRequest builds the endpoint read. Both paths use the one method; the
// nodeId is addressing for the broker's fork (empty or this node's own id
// reads locally), not a model query - and it is spelled nodeId because that
// is the custom plane's shape. The engine methods carry `node`; this plane
// does not, and mixing them addresses nothing while looking like it does.
func customReadRequest(nodeKey string) (string, map[string]any) {
	if nodeKey == "" {
		return "custom/list", nil
	}
	return "custom/list", map[string]any{"nodeId": nodeKey}
}

// customPortRequest builds the port read. Same addressing, and the body stays
// empty: a facade port belongs to the node, not to an endpoint, so there is
// nothing to name.
func customPortRequest(nodeKey string) (string, map[string]any) {
	if nodeKey == "" {
		return "custom/get-port", nil
	}
	return "custom/get-port", map[string]any{"nodeId": nodeKey}
}

// customQueryable reports whether a Custom read can be answered at all.
// A peer answers it only over the pinned mTLS channel a cluster member has,
// and only while it is online; asking otherwise does not discover the peer
// is absent, it waits forty-five seconds to be told so. A hand-added entry
// is not a cluster member by definition, and this machine's own
// configuration is always readable.
func (d *nodeDetail) customQueryable() bool {
	if !d.remote() {
		return true
	}
	return d.node.membership == membershipMember && d.node.presence == presenceOnline
}

// customCmd reads the node's endpoint configuration when the screen opens.
func (d *nodeDetail) customCmd() tea.Cmd {
	if !d.customQueryable() {
		return nil
	}
	method, params := customReadRequest(d.nodeArg())
	return d.own(callWithin(d.client, customReadBudget, method, params,
		func(msg *rpc.Message, err error) tea.Msg {
			if err != nil {
				// Kept verbatim. The unified "device is offline or does not
				// support custom endpoint management" answer already names
				// the trouble and the fix; wrapping it in "custom list
				// failed:" would bury the second half.
				return detailCustomMsg{note: err.Error()}
			}
			var endpoints []customEndpointRow
			if !decodeOrLog(method, msg.Result, &endpoints) {
				return nil
			}
			return detailCustomMsg{endpoints: endpoints}
		}))
}

// customPortCmd reads the facade port beside the endpoint list, on the same
// terms: once, on opening, only where an answer exists.
//
// It is a second call rather than a second field of one reply because the two
// questions have different authorities - the endpoint list is persisted
// configuration, the port is a live listener - and a node that answers one can
// refuse the other.
func (d *nodeDetail) customPortCmd() tea.Cmd {
	if !d.customQueryable() {
		return nil
	}
	method, params := customPortRequest(d.nodeArg())
	return d.own(callWithin(d.client, customReadBudget, method, params,
		func(msg *rpc.Message, err error) tea.Msg {
			if err != nil {
				// Swallowed, deliberately. Where the port is missing because
				// the Custom plane is missing, the endpoint read says so beside
				// the section in the same words and has somewhere to be said;
				// a second copy would push endpoints off the frame to repeat a
				// fact with no action in it. What is left is a section without
				// a port line, which is the truth: nothing told us where that
				// node fronts its endpoints.
				return nil
			}
			var port customProxyPort
			if !decodeOrLog(method, msg.Result, &port) {
				return nil
			}
			return detailCustomPortMsg{port: port}
		}))
}

// customLines builds the section as one string per rendered row, and
// customHeight and customBlock both take it from here so the measured
// height and the rendered height cannot drift - the hardware block's
// comment explains exactly how that drift eats the status line.
func (d *nodeDetail) customLines() []string {
	if d.custom.note != "" {
		return []string{footerStyle.Render("  " + d.custom.note)}
	}
	if len(d.custom.endpoints) == 0 {
		return nil
	}
	// A section is only drawn for a node that has endpoints, so the port line
	// rides on the endpoint list rather than standing alone: a port with nothing
	// behind it is a door with no room behind it.
	port := d.custom.port.line()
	chrome := customSectionChrome
	if port != "" {
		chrome++
	}
	budget := d.customBudget()
	if budget < chrome+1 {
		// A heading over one clipped endpoint says less than nothing. The
		// engine list and the status line are the better use of the last
		// rows, same call the models section makes.
		return nil
	}
	lines := make([]string, 0, budget)
	lines = append(lines, footerStyle.Render("  Custom endpoints"))
	if port != "" {
		lines = append(lines, footerStyle.Render("  "+port))
	}
	rows := make([]string, 0, len(d.custom.endpoints))
	for _, e := range d.custom.endpoints {
		text := e.BaseURL
		if e.Name != "" {
			text = e.Name + "  " + text
		}
		if e.down() {
			text += "  down"
		}
		if e.KeySet {
			text += "  [key]"
		}
		rows = append(rows, footerStyle.Render("  "+text))
	}
	lines = append(lines, rows...)
	if len(lines) > budget {
		// `chrome` is the count of rows the section spent on itself, which is
		// exactly the rows that are not endpoints, which is exactly what the
		// notice has to count.
		hidden := len(rows) - (budget - chrome)
		lines = append(lines[:budget-1],
			footerStyle.Render(fmt.Sprintf("  ...and %d more", hidden)))
	}
	return lines
}

// customHeight is the rows the section occupies, for the tables sized
// against the frame budget.
func (d *nodeDetail) customHeight() int {
	return len(d.customLines())
}

// customBlock renders the section, or nothing.
func (d *nodeDetail) customBlock() string {
	return strings.Join(d.customLines(), "\n")
}

// customBudget is the most rows the section may take: half the room the
// hardware block leaves, capped. The two blocks together can then never eat
// more than the frame did before either existed, and one very long list
// halves itself for the other.
func (d *nodeDetail) customBudget() int {
	free := d.height - detailChromeRows - d.hardwareHeight()
	budget := max(free/2, 0)
	if budget > customMaxRows {
		return customMaxRows
	}
	return budget
}

// customPortEditable reports whether the facade-port field may open at all:
// this machine's own proxy, with a port the node named and a listener behind it.
// The help line asks this so a footer never advertises a key whose only answer
// is a refusal.
func (d *nodeDetail) customPortEditable() bool {
	return !d.remote() && d.custom.port.editable()
}

// editCustomPort opens the facade-port field, or says why there is nothing to
// edit.
//
// The refusals are said here rather than sent because each one is already
// settled. This screen does not edit a peer's configuration - a peer's port is
// displayed, and that is all. A node that named no port has nothing for the
// field to hold. And with no live facade there is nothing to rebind, which the
// owning node answers with a refusal rather than a second copy of a number its
// proxy would ignore on the next bring-up.
func (d *nodeDetail) editCustomPort() tea.Cmd {
	if d.remote() {
		d.status.error("changing the Custom proxy port is only available on the machine running it")
		return nil
	}
	port := d.custom.port
	if port.Port <= 0 {
		d.status.error("this node reported no Custom proxy port")
		return nil
	}
	if !port.Running {
		d.status.error("nothing is listening on port %d; start the Custom proxy to change its port", port.Port)
		return nil
	}
	d.mode = detailInputCustomPort
	d.input.Placeholder = "port"
	d.input.CharLimit = 5
	d.input.SetValue(strconv.Itoa(port.Port))
	d.input.Focus()
	d.input.CursorEnd()
	return textinput.Blink
}

// submitCustomPort sends the typed port.
//
// The node's answer replaces the number on screen, which is the difference
// between a port field and a port promise: a move onto a port that is free to
// claim but already held by an unrelated process loses a bind race, and the
// answer says so in the owner's words. Those name the port and who holds it, so
// they reach the status line instead of being wrapped in "custom port failed".
//
// A port that was already the live port is sent anyway. The section reads once,
// so what is on screen may be older than the facade: asking is how a facade that
// moved by itself gets corrected instead of contradicted.
func (d *nodeDetail) submitCustomPort(value string) tea.Cmd {
	port, ok := parsePort(value)
	if !ok {
		d.status.error("invalid port: enter a number between 1 and 65535")
		return nil
	}
	d.status.busy("moving the Custom proxy to port %d...", port)
	return d.own(call(d.client, "custom/set-port", map[string]any{"port": port},
		func(msg *rpc.Message, err error) tea.Msg {
			if err != nil {
				return customPortApplyMsg{err: err}
			}
			var state customProxyPort
			if !decodeOrLog("custom/set-port", msg.Result, &state) {
				return nil
			}
			return customPortApplyMsg{port: state}
		}))
}

// applyCustomPort folds an applied change into the section and says what
// happened. Whatever the node reported is rendered as it reads: a port nothing
// listened on is not a move that worked, and no client should be pointed at one.
func (d *nodeDetail) applyCustomPort(msg customPortApplyMsg) {
	if msg.err != nil {
		d.status.error("Custom proxy port failed: %s", msg.err)
		return
	}
	if !msg.port.Running {
		d.status.error("nothing is listening on port %d", msg.port.Port)
	} else {
		d.status.ok("Custom proxy port is now %d", msg.port.Port)
	}
	d.custom.port = msg.port
}
