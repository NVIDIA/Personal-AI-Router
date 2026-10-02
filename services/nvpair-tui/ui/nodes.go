// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// manualRefreshInterval re-lists manual nodes so the probe-driven reachability
// columns stay current (nvpair-manual-nodes re-probes every 10s).
const manualRefreshInterval = 10 * time.Second

// nodesInputMode is which text field, if any, is currently capturing keys.
type nodesInputMode int

const (
	nodesInputNone nodesInputMode = iota
	nodesInputManualAddress
	nodesInputInviteAddress
	nodesInputPin
	nodesInputFilter
)

// nodesView is the Nodes tab: every machine PAIR knows about — discovered on
// the network, added by hand, or paired into this cluster — one row each, with
// whether it is reachable and where it stands in the cluster.
//
// The usual path: find the machine (/ filters by name or address, f adds one
// discovery cannot see), pair with it (p, or n by address) and read the PIN to
// whoever is at the other machine, then press enter for its detail screen —
// engines, models, hardware, and their controls. A pairing request from
// another machine appears above the table, answered with a or d.
//
// One row per machine because a machine is the unit an operator reasons about:
// its address, reachability, and membership read together, and pairing has
// one entry point with one set of guards. The user-facing guide is
// docs/terminal-interface.mdx.
type nodesView struct {
	client *rpc.Client
	table  table.Model

	feeds nodeFeeds
	rows  []nodeRow

	// selectedKey tracks the highlighted node by identity, not row index. The
	// list re-sorts as nodes come and go, and an index would silently move the
	// operator's selection onto a different machine between keypresses.
	selectedKey string
	// detail is the open drill-down for one node, nil when the list is showing.
	detail *nodeDetail

	identity clusterIdentity
	// clusterName is the cluster's display label. It is a separate field because
	// cluster:get-node-id does not return it: it is a node setting, read at
	// startup, carried on the cluster:identity-changed push, and taken from the
	// Service tab's save when it is renamed there, since the settings worker
	// sends no push of its own.
	clusterName string
	// inbound is the pairing request on screen, awaiting our answer, and queued
	// the ones that arrived while it was up, oldest first. Each is a live
	// session another machine is waiting on, so a newer one waits its turn
	// rather than replacing the one being read.
	inbound *clusterInvite
	queued  []clusterInvite
	// membersPushes counts nodes:changed pushes, so a roster read can tell
	// whether one has arrived since it was sent; see clusterMembersMsg.
	membersPushes int
	// answering is the inbound request whose answer is in flight. It stays on
	// screen until the cluster manager settles it: a PIN it refuses as
	// malformed leaves the session open for another try.
	answering string
	// invitedKey is the node an outbound invite is pending against, held so the
	// pinned PIN can be retired once that node turns up trusted. Empty for an
	// invite sent by address, which has no node identity to key on — see
	// invitedAddress.
	invitedKey string
	// invitedAddress is the host an invite-by-address is pending against. That
	// path has no UUID, so the joined peer is recognised by its address instead;
	// without it the PIN it pinned was never retired.
	invitedAddress string
	// outboundInviteID is the invite our pending PIN belongs to. Terminal
	// notifications carry an inviteId and the manager supports concurrent
	// pairings, so an event is only ours if the ids match — otherwise an
	// unrelated invite's decline cleared this one's PIN.
	outboundInviteID string
	// inviteSending is an invite awaiting its reply, and outboundName and
	// outboundPIN describe the one outstanding — see refuseSecondInvite.
	inviteSending bool
	outboundName  string
	outboundPIN   string
	// confirmLeave and confirmRemove gate the two trust teardowns behind a
	// second keystroke. Both keys are lowercase and sit beside the navigation
	// keys, so a single press is too easy to hit by accident — and removing a
	// member acts on someone else's row, which makes a misfire worse rather
	// than better.
	confirmLeave bool
	// confirmRemove holds the node key awaiting confirmation, so the row cannot
	// change underneath the confirmation.
	confirmRemove string
	// all is every node the merge produced; rows is the subset on screen. They
	// differ only when a filter is set, and the distinction matters: the cluster
	// summary and the pairing-completion check are about the cluster, not about
	// what the operator is currently looking at.
	all []nodeRow
	// filter narrows the list by name or address. Empty shows everything.
	filter string
	// feedFailures maps a feed name to why it last failed. An empty table is
	// ambiguous — nothing discovered yet, or nothing could be read — and the
	// difference decides whether the operator waits or goes looking at the
	// service, so it has to be on screen.
	feedFailures map[string]string

	input  textinput.Model
	mode   nodesInputMode
	status toast

	width, height int
}

// The feeds that populate this tab. Each can fail independently, and a failure
// is reported by name because the consequences differ: no identity means this
// machine cannot be told apart from its peers, while no manual list only hides
// hand-added entries.
const (
	feedIdentity    = "cluster identity"
	feedMembers     = "cluster members"
	feedClusterName = "cluster name"
	feedManual      = "manual nodes"
	feedDiscovery   = "discovery"
)

// noteFeed records or clears a feed's failure.
//
// Held as state rather than announced as a toast because these are conditions,
// not events: the manual list re-reads on a tick, so a toast per failure would
// bury every other message while a worker is down, and a toast that expires
// would leave the tab looking merely empty again.
func (v *nodesView) noteFeed(name string, err error) {
	if err == nil {
		delete(v.feedFailures, name)
		return
	}
	if v.feedFailures == nil {
		v.feedFailures = map[string]string{}
	}
	v.feedFailures[name] = err.Error()
}

// feedWarning is the one-line summary of what could not be read, or "" when
// everything is current.
func (v *nodesView) feedWarning() string {
	if len(v.feedFailures) == 0 {
		return ""
	}
	names := make([]string, 0, len(v.feedFailures))
	for name := range v.feedFailures {
		names = append(names, name)
	}
	sort.Strings(names)
	// One representative reason: the failures almost always share a cause (the
	// worker behind them is down), and repeating it per feed would push the
	// table off a short terminal.
	return fmt.Sprintf("unavailable: %s (%s)",
		strings.Join(names, ", "), v.feedFailures[names[0]])
}

type discoverySubscribedMsg struct{ err error }

// engineSubscribedMsg is the ack for the engine push stream. A failure is
// surfaced because everything on a node's detail screen goes stale without it.
type engineSubscribedMsg struct{ err error }

type clusterIdentityMsg struct {
	id  clusterIdentity
	err error
}

// clusterMembersMsg carries a nodes:get-initial read. pushes is how many
// nodes:changed pushes had arrived when it was sent; see membersPushes.
type clusterMembersMsg struct {
	nodes  []clusterNode
	pushes int
	err    error
}

// clusterNameMsg carries the cluster's display label.
type clusterNameMsg struct {
	name string
	err  error
}

type manualNodesMsg struct {
	nodes []manualNode
	err   error
}

type manualTickMsg struct{}

// nodeActionMsg is the outcome of any single-shot node command.
type nodeActionMsg struct {
	what string
	err  error
}

// nodeInviteMsg carries the outcome of a cluster:invite-node: the PIN to read
// to the joining node, an explicit rejection, or the failure to surface.
type nodeInviteMsg struct {
	name string
	// inviteID identifies the session, so a later terminal notification can be
	// matched to the PIN this result pinned.
	inviteID string
	// address is set when the invite went out by address rather than to a
	// discovered node, which is how the joined peer is recognised later.
	address  string
	pin      string
	rejected bool
	reason   string
	// ended is the terminal state the manager answered with instead of a
	// PIN, when no invite went out.
	ended string
	err   error
}

// Key labels distinguish the two things an address can be used for, which are
// easily confused: "pair" establishes mutual trust and needs the other side to
// accept a PIN, while "find" only teaches this node an address so a machine mDNS
// cannot see becomes visible. Neither implies the other.
var (
	nodeDetailKey = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "details"))
	// p pairs and a accepts, matching the words on screen. The verb everywhere
	// in this UI is "pair", so the key that starts one is p; i only ever made
	// sense against "invite", which nothing says any more.
	nodeInviteKey     = key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "pair"))
	nodeInviteAddrKey = key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "pair by address"))
	// f rather than a, which accepting took. The bubbles table binds f to
	// page-down, so this is the third verb on this tab to win a key from the
	// table's paging — d and l already do — and the paging key still works on
	// every row action that does not apply.
	nodeAddKey     = key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "find by address"))
	nodeRemoveKey  = key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "remove"))
	nodePairKey    = key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "accept pairing"))
	nodeDeclineKey = key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "decline"))
	nodeLeaveKey   = key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "leave cluster"))
	nodeCancelKey  = key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "cancel invite"))
	nodeFilterKey  = key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter"))
	nodeClearKey   = key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "clear filter"))
	nodeConfirmKey = key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "confirm"))
)

func newNodesView(client *rpc.Client) *nodesView {
	ti := textinput.New()
	v := &nodesView{client: client, input: ti}
	v.table = newTable(nodesColumns(defaultTableWidth))
	return v
}

// nodesColumns is the node table's layout, shared by construction and resize so
// the two cannot drift. STATUS and CLUSTER are separate on purpose: reachability
// and membership are independent facts, and merging them made a departed member
// read as connected.
//
// There is deliberately no "last seen" column. The only timestamp discovery
// carries is the moment a record was last written, which nvpair-node-scanner
// documents as explicitly not a liveness clock: the browser reports a node only
// when its record changes, so a healthy peer's timestamp freezes at first
// discovery, and the local node's advances only when it republishes. Rendered as
// an age it invited exactly the wrong reading — a steadily climbing number
// beside "this machine", whose reachability is never in question. STATUS is the
// reachability verdict, and it has better evidence behind it.
func nodesColumns(w int) []table.Column {
	return layoutColumns(w, []column{
		flexCol("NAME", 10, 2),
		flexCol("ADDRESS", 10, 2),
		fixedCol("STATUS", 7),
		fixedCol("CLUSTER", 13),
		fixedCol("MODELS", 6),
	})
}

func (v *nodesView) Title() string { return "Nodes" }

func (v *nodesView) Init() tea.Cmd {
	return tea.Batch(
		call(v.client, "discovery:subscribe", nil, func(_ *rpc.Message, err error) tea.Msg {
			return discoverySubscribedMsg{err: err}
		}),
		// The engine push stream is opt-in and off by default: without this the
		// broker discards every engine:state-changed, engine:models-changed,
		// and install/pull/remote progress notification, so a node's detail
		// screen would show a one-shot snapshot that never updates and no
		// progress would ever appear. Subscribed here, once, because this tab
		// owns the detail screens that consume those pushes.
		call(v.client, "engine:subscribe", nil, func(_ *rpc.Message, err error) tea.Msg {
			return engineSubscribedMsg{err: err}
		}),
		v.identityCmd(),
		v.clusterNameCmd(),
		v.membersCmd(),
		v.manualCmd(),
		v.manualTickCmd(),
	)
}

func (v *nodesView) clusterNameCmd() tea.Cmd {
	return call(v.client, getClusterNameMethod, nil,
		func(msg *rpc.Message, err error) tea.Msg {
			if err != nil {
				return clusterNameMsg{err: err}
			}
			var r struct {
				Value string `json:"value"`
			}
			decodeOrLog(getClusterNameMethod, msg.Result, &r)
			return clusterNameMsg{name: r.Value}
		})
}

func (v *nodesView) identityCmd() tea.Cmd {
	return call(v.client, "cluster:get-node-id", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return clusterIdentityMsg{err: err}
		}
		var id clusterIdentity
		decodeOrLog("cluster:get-node-id", msg.Result, &id)
		return clusterIdentityMsg{id: id}
	})
}

func (v *nodesView) membersCmd() tea.Cmd {
	pushes := v.membersPushes
	return call(v.client, "nodes:get-initial", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return clusterMembersMsg{pushes: pushes, err: err}
		}
		var r struct {
			Nodes []clusterNode `json:"nodes"`
		}
		decodeOrLog("nodes:get-initial", msg.Result, &r)
		return clusterMembersMsg{nodes: r.Nodes, pushes: pushes}
	})
}

func (v *nodesView) manualCmd() tea.Cmd {
	return call(v.client, "nodes/list", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return manualNodesMsg{err: err}
		}
		var r struct {
			Nodes []manualNode `json:"nodes"`
		}
		decodeOrLog("nodes/list", msg.Result, &r)
		return manualNodesMsg{nodes: r.Nodes}
	})
}

func (v *nodesView) manualTickCmd() tea.Cmd {
	return tea.Tick(manualRefreshInterval, func(time.Time) tea.Msg { return manualTickMsg{} })
}

// SetSize records the budget and fixes the table's width. Its height is set in
// View, from the chrome actually being rendered — see fitTable.
func (v *nodesView) SetSize(w, h int) {
	v.width, v.height = w, h
	v.table.SetColumns(nodesColumns(w))
	v.table.SetWidth(w)
	if v.detail != nil {
		v.detail.SetSize(w, h)
	}
}

// CapturingInput reports a text field having the keyboard, in the list or in an
// open detail screen, so the shell stops applying its global bindings.
func (v *nodesView) CapturingInput() bool {
	if v.detail != nil {
		return v.detail.CapturingInput()
	}
	// An armed teardown answers the next key too. Without this the shell's own
	// bindings still fired, so tab or a digit switched away and left the action
	// armed behind a prompt no longer on screen — to be confirmed by whatever
	// the operator pressed on returning to the tab.
	return v.mode != nodesInputNone || v.confirmLeave || v.confirmRemove != ""
}

func (v *nodesView) Update(msg tea.Msg) tea.Cmd {
	// An open detail screen owns the keyboard. Everything else still reaches
	// the list underneath so its state is current when the operator returns,
	// and reaches the detail too so engine and model pushes land there.
	if v.detail != nil {
		if _, isKey := msg.(tea.KeyMsg); isKey {
			cmd, stayOpen := v.detail.update(msg)
			if !stayOpen {
				v.detail = nil
				v.SetSize(v.width, v.height)
			}
			return cmd
		}
		detailCmd, _ := v.detail.update(msg)
		if detailCmd != nil {
			return tea.Batch(detailCmd, v.updateList(msg))
		}
	}
	return v.updateList(msg)
}

func (v *nodesView) updateList(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case discoverySubscribedMsg:
		// Recorded as well as announced: without discovery the list simply stops
		// filling, and by the time the operator wonders why, the toast is gone.
		v.noteFeed(feedDiscovery, msg.err)
		if msg.err != nil {
			v.status.error("discovery subscribe failed: %s", msg.err)
		}
		return nil

	case engineSubscribedMsg:
		if msg.err != nil {
			v.status.error("engine updates unavailable: %s", msg.err)
		}
		return nil

	case clusterIdentityMsg:
		v.noteFeed(feedIdentity, msg.err)
		if msg.err == nil {
			v.identity = msg.id
			v.feeds.selfUUID = msg.id.NodeUUID
			v.rebuild()
		}
		return nil

	case clusterMembersMsg:
		v.noteFeed(feedMembers, msg.err)
		// Taken only if no push has arrived since the read was sent. Reads and
		// pushes are both whole rosters, but a reply and a push reach this
		// loop by different paths, so arrival order says nothing about which
		// is newer — except that a push arriving after the read was asked for
		// is at least as new as its reply. Pushes carry every change, so a
		// reply dropped here loses nothing they will not bring.
		if msg.err == nil && msg.pushes == v.membersPushes {
			v.feeds.members = msg.nodes
			v.rebuild()
		}
		return nil

	case clusterNameMsg:
		v.noteFeed(feedClusterName, msg.err)
		if msg.err == nil {
			v.clusterName = msg.name
		}
		return nil

	case settingSavedMsg:
		// A rename on the Service tab. The settings worker sends no push, so
		// without this the name read at startup stayed on screen here.
		if msg.err == nil && msg.method == setClusterNameMethod {
			v.clusterName = msg.value
		}
		return nil

	case manualNodesMsg:
		v.noteFeed(feedManual, msg.err)
		if msg.err == nil {
			v.feeds.manual = msg.nodes
			v.rebuild()
		}
		return nil

	case manualTickMsg:
		return tea.Batch(v.manualCmd(), v.manualTickCmd())

	case TickMsg:
		// Relative ages and presence both derive from the clock, so a node
		// going quiet has to re-grade without waiting for a broker push.
		v.rebuild()
		return nil

	case nodeActionMsg:
		if msg.err != nil {
			v.status.error("%s failed: %s", msg.what, msg.err)
		} else {
			v.status.ok("%s ok", msg.what)
		}
		return tea.Batch(v.membersCmd(), v.manualCmd())

	case nodeInviteMsg:
		return v.handleInviteResult(msg)

	case pairingResultMsg:
		return v.handlePairingResult(msg)

	case waitingDeclinedMsg:
		if msg.err != nil {
			// Logged, not shown. The outcome here is the same either way — the
			// request could not have been accepted — and one that expired or
			// was cancelled in the meantime fails this harmlessly.
			slog.Warn("could not decline a pairing request after joining a cluster",
				"from", msg.from, "err", msg.err)
		}
		return nil

	case inviteCancelledMsg:
		return v.handleInviteCancelled(msg)

	case NotificationMsg:
		return v.handleNotification(msg.Msg)

	case tea.KeyMsg:
		return v.handleKey(msg)
	}
	return nil
}

func (v *nodesView) handleInviteResult(msg nodeInviteMsg) tea.Cmd {
	v.inviteSending = false
	switch {
	case msg.err != nil:
		v.clearOutboundInvite()
		v.status.error("invite failed: %s", msg.err)
	case msg.rejected && msg.reason == reasonAlreadyClustered:
		// The one rejection with a remedy on the other side: that machine has
		// to leave the cluster it is in before it can join this one.
		v.clearOutboundInvite()
		v.status.error("%s is already in a cluster - it has to leave that cluster before it can pair",
			msg.name)
	case msg.rejected:
		v.clearOutboundInvite()
		v.status.error("%s rejected the invite (%s)", msg.name, rejectReason(msg.reason))
	case msg.ended != "":
		v.clearOutboundInvite()
		v.status.error("the invite to %s did not go out (%s) - check the Logs tab", msg.name, msg.ended)
	case msg.pin != "":
		// Pinned, not expiring: the operator reads this PIN to someone at the
		// other machine. It clears when the invite resolves.
		//
		// The id is recorded so a terminal notification can be matched to this
		// session, and the address so an invite sent by address — which has no
		// node identity — can still recognise the peer once it joins.
		v.outboundInviteID = msg.inviteID
		v.invitedAddress = msg.address
		v.outboundName, v.outboundPIN = msg.name, msg.pin
		v.status.pin("invite sent to %s - PIN %s (read it to that node)", msg.name, msg.pin)
	default:
		v.status.ok("invite sent to %s", msg.name)
	}
	return nil
}

// clearOutboundInvite forgets the pending outbound pairing session.
func (v *nodesView) clearOutboundInvite() {
	v.invitedKey = ""
	v.invitedAddress = ""
	v.outboundInviteID = ""
	v.outboundName = ""
	v.outboundPIN = ""
}

// refuseSecondInvite turns away a new invite while one is outstanding, and
// reports whether it did.
//
// One at a time, because the status line carries one PIN. The manager would
// accept a second, but this view tracked a single target and its replies do
// not say which request they answer, so out-of-order replies could show one
// peer's PIN while watching for the other to join — or a failure of the older
// request could cancel the newer one.
func (v *nodesView) refuseSecondInvite() bool {
	switch {
	case v.inviteSending:
		v.status.busy("still inviting %s - wait for the PIN", v.outboundName)
	case v.outboundInviteID != "":
		// Pinned again rather than replaced: the PIN is what the operator is
		// reading out, and a refusal standing in its place would hide it.
		v.status.pin("invite to %s is still open - PIN %s; press %s to cancel it first",
			v.outboundName, v.outboundPIN, nodeCancelKey.Help().Key)
	default:
		return false
	}
	return true
}

func (v *nodesView) handleNotification(msg *rpc.Message) tea.Cmd {
	switch msg.Method {
	case "discovery:nodes-changed":
		var nodes []availableNode
		decodeOrLog(msg.Method, msg.Params, &nodes)
		v.feeds.discovered = nodes
		v.rebuild()

	case "nodes:changed":
		var r struct {
			Nodes []clusterNode `json:"nodes"`
		}
		if decodeOrLog(msg.Method, msg.Params, &r) {
			v.membersPushes++
			v.feeds.members = r.Nodes
			v.rebuild()
		}

	case "cluster:identity-changed":
		var r struct {
			ClusterID           string `json:"clusterId"`
			ClusterFriendlyName string `json:"clusterFriendlyName"`
		}
		decodeOrLog(msg.Method, msg.Params, &r)
		v.identity.ClusterID = r.ClusterID
		v.clusterName = r.ClusterFriendlyName

	case "cluster:invite-received":
		var inv clusterInvite
		if decodeOrLog(msg.Method, msg.Params, &inv) {
			v.receiveInvite(inv)
		}

	default:
		// Terminal invite events retire a pinned PIN or an inbound prompt that
		// can no longer be acted on. Without them a declined or expired invite
		// stayed on screen looking live.
		if outcome, ok := inviteOutcome(msg.Method); ok {
			v.retireInvite(msg.Params, outcome)
		}
	}
	return nil
}

// receiveInvite puts an inbound pairing request on screen, or behind the one
// already there. A request delivered twice is kept once.
//
// The prompt is a row of the frame, not a status line — see inboundPrompt.
func (v *nodesView) receiveInvite(inv clusterInvite) {
	if v.inbound == nil {
		v.inbound = &inv
		return
	}
	if v.inbound.InviteID == inv.InviteID {
		return
	}
	for _, q := range v.queued {
		if q.InviteID == inv.InviteID {
			return
		}
	}
	v.queued = append(v.queued, inv)
}

// dropInbound forgets an inbound request that is settled, bringing the next
// one waiting on screen if it was the one showing.
func (v *nodesView) dropInbound(id string) bool {
	if v.inbound != nil && v.inbound.InviteID == id {
		v.forgetPIN()
		v.inbound = nil
		if v.answering == id {
			v.answering = ""
		}
		if len(v.queued) > 0 {
			next := v.queued[0]
			v.queued = v.queued[1:]
			v.inbound = &next
		}
		return true
	}
	for i, q := range v.queued {
		if q.InviteID == id {
			v.queued = append(v.queued[:i], v.queued[i+1:]...)
			return true
		}
	}
	return false
}

// forgetPIN closes the PIN field and clears what was typed in it, for when the
// request it was opened for is gone. Left open, the digits typed for one
// request answered whichever came up next.
func (v *nodesView) forgetPIN() {
	if v.mode == nodesInputPin {
		v.input.SetValue("")
		v.cancelInput()
	}
}

// inboundPrompt is the standing line for a pairing request someone sent us,
// or empty when there is none.
//
// It follows the request through its states rather than describing only the
// first. Pressing "a" does not finish anything — it opens the PIN field — so a
// prompt that went on offering "a to accept" after the field was already up
// told the operator to do the thing they had just done, while the answer it
// actually wanted was on the line below.
func (v *nodesView) inboundPrompt() string {
	if v.inbound == nil {
		return ""
	}
	var line string
	switch {
	case v.answering == v.inbound.InviteID:
		line = fmt.Sprintf("answering the pairing request from %s...", v.inbound.FromNodeName)
	case v.mode == nodesInputPin:
		line = fmt.Sprintf("accepting %s - enter the PIN shown on that machine",
			v.inbound.FromNodeName)
	default:
		line = fmt.Sprintf("pairing request from %s - %s to accept, %s to decline",
			v.inbound.FromNodeName, nodePairKey.Help().Key, nodeDeclineKey.Help().Key)
	}
	if n := len(v.queued); n > 0 {
		line += fmt.Sprintf(" (%d more waiting)", n)
	}
	return statusOKStyle.Render(line)
}

// pairingLine is the pairing state that stays in view over a node's detail
// screen: a request waiting on this machine, or the PIN of one sent from it.
// Both are sessions someone at another machine is waiting on, and the detail
// screen replaces the list where they are otherwise shown.
func (v *nodesView) pairingLine() string {
	switch {
	case v.inbound != nil && v.answering == v.inbound.InviteID:
		return statusOKStyle.Render(fmt.Sprintf(
			"answering the pairing request from %s...", v.inbound.FromNodeName))
	case v.inbound != nil:
		return statusOKStyle.Render(fmt.Sprintf(
			"pairing request from %s - esc to the node list to answer it", v.inbound.FromNodeName))
	case v.outboundInviteID != "":
		return statusOKStyle.Render(fmt.Sprintf(
			"invite to %s is open - PIN %s", v.outboundName, v.outboundPIN))
	}
	return ""
}

// retireInvite clears whichever pairing session a terminal event belongs to.
//
// Matched on inviteId, because concurrent pairings are supported: an outbound
// decline arriving while an inbound request is on screen must not clear the
// inbound prompt, and vice versa. Every terminal notification the cluster
// manager emits carries the invite it refers to, so an event without one
// belongs to no session this view is tracking and is ignored rather than
// applied to both.
func (v *nodesView) retireInvite(params []byte, outcome inviteResolution) {
	var ref inviteRef
	decodeOrLog("invite outcome", params, &ref)
	if ref.InviteID == "" {
		return
	}

	if ref.InviteID == v.outboundInviteID {
		v.clearOutboundInvite()
		v.status.set(outcome.kind, "invite %s", outcome.label)
	}
	if v.dropInbound(ref.InviteID) {
		v.status.set(outcome.kind, "pairing request %s", outcome.label)
	}
}

func (v *nodesView) handleKey(msg tea.KeyMsg) tea.Cmd {
	if v.mode != nodesInputNone {
		switch msg.String() {
		case "enter":
			return v.submitInput()
		case "esc":
			v.cancelInput()
			return nil
		}
		var cmd tea.Cmd
		v.input, cmd = v.input.Update(msg)
		return cmd
	}

	// Trust teardown is armed, not done: anything other than the confirmation
	// cancels, so a stray key never removes a peer or leaves a cluster.
	if v.confirmLeave {
		v.confirmLeave = false
		if key.Matches(msg, nodeConfirmKey) {
			return v.leaveCluster()
		}
		v.status.info("cancelled")
		return nil
	}
	if v.confirmRemove != "" {
		target := v.confirmRemove
		v.confirmRemove = ""
		if key.Matches(msg, nodeConfirmKey) {
			return v.removeMember(target)
		}
		v.status.info("cancelled")
		return nil
	}

	switch {
	case key.Matches(msg, nodeDetailKey):
		return v.openDetail()
	case key.Matches(msg, nodeInviteKey):
		if v.refuseSecondInvite() {
			return nil
		}
		return v.inviteSelected()
	case key.Matches(msg, nodeInviteAddrKey):
		// Refused before the field opens, not after an address has been typed.
		if v.refuseSecondInvite() {
			return nil
		}
		v.beginInput(nodesInputInviteAddress, "host (or host:port; default 14321)")
		return textinput.Blink
	case key.Matches(msg, nodeAddKey):
		v.beginInput(nodesInputManualAddress, "host")
		return textinput.Blink
	case key.Matches(msg, nodeRemoveKey):
		return v.removeSelected()
	case key.Matches(msg, nodePairKey):
		if v.inbound == nil {
			v.status.info("no pairing request to accept")
			return nil
		}
		if v.answering != "" {
			return nil
		}
		v.beginInput(nodesInputPin, "PIN from the inviting node")
		return textinput.Blink
	case key.Matches(msg, nodeDeclineKey) && v.inbound != nil && v.answering == "":
		// Gated on there being something to decline, so that with no pairing
		// request pending the key falls through to the table, where d is the
		// standard half-page-down. Unconditionally intercepting it meant paging
		// a long node list answered with a message about pairing.
		return v.respondToInvite(false, "")
	case key.Matches(msg, nodeCancelKey):
		return v.cancelInvite()
	case key.Matches(msg, nodeFilterKey):
		v.beginInput(nodesInputFilter, "filter by name or address")
		v.input.SetValue(v.filter)
		return textinput.Blink
	case key.Matches(msg, nodeClearKey) && v.filter != "":
		v.filter = ""
		v.rebuild()
		return nil
	case key.Matches(msg, nodeLeaveKey):
		if v.identity.ClusterID == "" {
			v.status.error("not in a cluster")
			return nil
		}
		v.confirmLeave = true
		v.status.arm("leave the cluster? press y to confirm, any other key to cancel")
		return nil
	}

	var cmd tea.Cmd
	v.table, cmd = v.table.Update(msg)
	// Moving the cursor re-anchors the selection so a later refresh keeps it.
	if row := v.rowAt(v.table.Cursor()); row != nil {
		v.selectedKey = row.key
	}
	return cmd
}

// reset closes an open detail screen so the tab shows the node list again.
//
// Called when the operator leaves this tab, not when they press esc — esc has
// its own path through Update. The list underneath has been kept current the
// whole time the detail was up, so there is nothing to reload.
func (v *nodesView) reset() {
	if v.detail == nil {
		return
	}
	v.detail = nil
	// The list was sized for the space the detail screen was using.
	v.SetSize(v.width, v.height)
}

// openDetail drills into the selected node. The detail screen is built from the
// merged row, so a remote node's models are on screen immediately from the
// discovery snapshot while its engine list is being fetched.
func (v *nodesView) openDetail() tea.Cmd {
	row := v.selectedRow()
	if row == nil {
		v.status.error("no node selected")
		return nil
	}
	v.detail = newNodeDetail(v.client, *row)
	v.detail.SetSize(v.width, v.height)
	return v.detail.Init()
}

func (v *nodesView) beginInput(mode nodesInputMode, placeholder string) {
	v.mode = mode
	v.input.SetValue("")
	v.input.Placeholder = placeholder
	v.input.Focus()
	v.SetSize(v.width, v.height)
}

func (v *nodesView) cancelInput() {
	v.mode = nodesInputNone
	v.input.Blur()
	v.SetSize(v.width, v.height)
}

func (v *nodesView) submitInput() tea.Cmd {
	val := strings.TrimSpace(v.input.Value())
	mode := v.mode
	v.cancelInput()

	switch mode {
	case nodesInputFilter:
		// Applied on submit rather than per keystroke: the list re-sorts as
		// nodes come and go, and narrowing it under the cursor while the
		// operator is still typing moves the selection out from under them.
		v.filter = val
		v.rebuild()
		return nil

	case nodesInputManualAddress:
		if val == "" {
			v.status.error("address required")
			return nil
		}
		v.status.busy("looking for %s...", val)
		return call(v.client, "node/add", map[string]string{"address": val},
			func(_ *rpc.Message, err error) tea.Msg {
				return nodeActionMsg{what: "add " + val, err: err}
			})

	case nodesInputInviteAddress:
		if val == "" {
			v.status.error("address required")
			return nil
		}
		return v.inviteAddress(val)

	case nodesInputPin:
		return v.respondToInvite(true, val)
	}
	return nil
}

// inviteAddress pairs with a host discovery has not found, for networks that
// filter multicast.
//
// nvpair-cluster-manager treats "address" as a bare host and appends the port
// itself (default 14321). If the operator typed host:port, split it so the port
// lands in the manager's separate field instead of being glued onto the host.
func (v *nodesView) inviteAddress(val string) tea.Cmd {
	// The same guard the discovered-node path applies. A node the table already
	// shows as belonging to another cluster cannot accept, and typing its
	// address instead of selecting its row should not get a different answer —
	// the operator would otherwise read out a PIN for an invite that is already
	// doomed.
	for _, n := range v.all {
		if addressMatches(n, val) && !n.membership.invitable() {
			v.status.error("%s is already %s - remove that relationship before pairing",
				n.name, n.membership.relationship())
			return nil
		}
	}

	params := map[string]any{"address": val}
	if host, portStr, err := net.SplitHostPort(val); err == nil {
		// A malformed or out-of-range port is rejected rather than folded back
		// into the host. Passing "host:notaport" through as an address sends a
		// string no dialer can use, and the failure surfaces much later as an
		// unreachable peer; "host:99999" was forwarded to the manager verbatim.
		port, ok := parsePort(portStr)
		if !ok {
			v.status.error("%q is not a port between 1 and 65535", portStr)
			return nil
		}
		params["address"] = host
		params["port"] = port
	}
	v.inviteSending, v.outboundName = true, val
	v.status.busy("inviting %s...", val)
	return inviteNodeCmd(v.client, params, func(res inviteNodeResult, err error) tea.Msg {
		return inviteResultMsg(val, val, res, err)
	})
}

// inviteSelected sends a cluster invite to the highlighted node.
//
// The node's discovery IP is the dial target; the manager appends the fixed
// cluster-manager port, so the row's own port (the node-info port) is
// deliberately not passed. The nodeId travels too so the manager stamps it as
// the invite's target identity.
func (v *nodesView) inviteSelected() tea.Cmd {
	row := v.selectedRow()
	if row == nil {
		v.status.error("no node selected")
		return nil
	}
	if row.self {
		v.status.error("cannot invite this machine to its own cluster")
		return nil
	}
	// One guard for every path into pairing. Previously the discovered-node
	// path checked this and the invite-by-address path did not, so the same
	// doomed invite could still be sent from the other tab.
	if !row.membership.invitable() {
		v.status.error("%s is already %s - remove that relationship before pairing",
			row.name, row.membership.relationship())
		return nil
	}
	if row.address == "" {
		v.status.error("%s has no known address - use %s to pair by address",
			row.name, nodeInviteAddrKey.Help().Key)
		return nil
	}

	params := map[string]any{"address": row.address, "nodeId": row.key}
	name := row.name
	v.invitedKey = row.key
	v.inviteSending, v.outboundName = true, name
	v.status.busy("inviting %s...", name)
	return inviteNodeCmd(v.client, params, func(res inviteNodeResult, err error) tea.Msg {
		return inviteResultMsg(name, "", res, err)
	})
}

// inviteNodeResult is the decoded cluster:invite-node result the UI acts on: a
// PIN to display on success, or an explicit rejection (e.g. the target is
// already clustered) carrying its reason. No PIN accompanies a rejection.
type inviteNodeResult struct {
	// InviteID identifies this pairing session. The manager mints one per
	// invite and stamps it on every terminal notification, so it is what lets a
	// view tell its own invite's outcome from a concurrent one's.
	InviteID string  `json:"inviteId"`
	State    string  `json:"state"`
	Pin      *string `json:"pin"`
	Reason   string  `json:"reason"`
}

// inviteResolution is how the UI reports an invite that is no longer pending.
type inviteResolution struct {
	kind  toastKind
	label string
}

// inviteRef is the invite a terminal notification refers to. The manager
// supports concurrent pairings, so an event has to be matched against the
// session it belongs to.
type inviteRef struct {
	InviteID string `json:"inviteId"`
}

// inviteOutcome maps a cluster-manager terminal invite event to its report.
// These notifications are the only signal that a sent invite has stopped being
// pending — the synchronous cluster:invite-node result only covers the handoff —
// so a view that displays a PIN must consume them or leave a dead invite on
// screen looking live.
func inviteOutcome(method string) (inviteResolution, bool) {
	switch method {
	case "cluster:invite-declined":
		return inviteResolution{kind: toastError, label: "declined by the other node"}, true
	case "cluster:invite-expired":
		return inviteResolution{kind: toastError, label: "expired before it was accepted"}, true
	case "cluster:invite-canceled":
		return inviteResolution{kind: toastInfo, label: "canceled"}, true
	case "cluster:invite-failed":
		return inviteResolution{kind: toastError, label: "failed - check the Logs tab"}, true
	default:
		return inviteResolution{}, false
	}
}

// inviteNodeCmd issues a single cluster:invite-node request and maps the
// decoded result (or error) into the caller's view message.
//
// There is no separate "create cluster" step: the backend auto-founds a
// cluster of one when this node isn't clustered yet, so the invite is the one
// authoritative call and the UI carries no membership orchestration.
func inviteNodeCmd(client *rpc.Client, params map[string]any, finish func(res inviteNodeResult, err error) tea.Msg) tea.Cmd {
	return call(client, "cluster:invite-node", params, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return finish(inviteNodeResult{}, err)
		}
		var r inviteNodeResult
		decodeOrLog("cluster:invite-node", msg.Result, &r)
		return finish(r, nil)
	})
}

// inviteResultMsg maps a cluster:invite-node outcome onto the view message.
// address is empty for an invite aimed at a discovered node.
func inviteResultMsg(name, address string, res inviteNodeResult, err error) tea.Msg {
	if err != nil {
		return nodeInviteMsg{name: name, address: address, err: err}
	}
	switch res.State {
	case inviteStatePending:
		pin := ""
		if res.Pin != nil {
			pin = *res.Pin
		}
		return nodeInviteMsg{name: name, address: address, inviteID: res.InviteID, pin: pin}
	case inviteStateRejected:
		return nodeInviteMsg{
			name: name, address: address, rejected: true, reason: res.Reason,
		}
	default:
		return nodeInviteMsg{name: name, address: address, ended: res.State}
	}
}

func (v *nodesView) respondToInvite(accept bool, pin string) tea.Cmd {
	if v.inbound == nil {
		// Say so rather than doing nothing. A key that silently ignores a press
		// is indistinguishable from one the terminal dropped, and the accept
		// path already answers.
		v.status.info("no pairing request to answer")
		return nil
	}
	if v.answering != "" {
		return nil
	}
	id, from := v.inbound.InviteID, v.inbound.FromNodeName
	params := map[string]any{"inviteId": id, "accept": accept}
	if accept && pin != "" {
		params["pin"] = pin
	}
	// Held on screen, not cleared, until the answer settles. The prompt says
	// the answer is in flight — the handshake crosses to another machine and
	// is the slowest call this tab makes — and the request stays answerable
	// if the cluster manager hands it back.
	v.answering = id
	return call(v.client, "cluster:respond-to-invite", params,
		func(msg *rpc.Message, err error) tea.Msg {
			if err != nil {
				return pairingResultMsg{inviteID: id, from: from, err: err}
			}
			// A wrong PIN is NOT a JSON-RPC error. The cluster manager tears the
			// session down and replies successfully with the invite, whose state
			// is "failed" and whose reason says why. Reading only the transport
			// error would report success for a pairing that had just been
			// rejected.
			var res inviteNodeResult
			decodeOrLog("cluster:respond-to-invite", msg.Result, &res)
			return pairingResultMsg{inviteID: id, from: from, state: res.State, reason: res.Reason}
		})
}

// The pairing session states nvpair-cluster-manager reports, spelled as its
// InviteState values (services/nvpair-cluster-manager/membership.go).
// cluster:invite-node answers pending with a PIN, rejected when the peer
// refused, or failed when the first exchange did not complete — or whatever
// state a cancel or teardown recorded first. Only pending means an invite went
// out. Answering a request settles it as paired, declined, or failed.
const (
	inviteStatePending  = "pending"
	inviteStatePaired   = "paired"
	inviteStateDeclined = "declined"
	inviteStateRejected = "rejected"
)

// The cluster manager's reason codes that get their own wording, because each
// has a specific remedy: a PIN that failed verification is the operator's typo,
// and an already-clustered peer has to leave its cluster first.
const (
	reasonIncorrectPIN     = "incorrect-pin"
	reasonAlreadyClustered = "already-clustered"
)

// rejectReason renders the reason a peer gave for refusing as human text.
func rejectReason(reason string) string {
	switch reason {
	case reasonAlreadyClustered:
		return "already in a cluster"
	case "":
		return "rejected by peer"
	default:
		return reason
	}
}

// pairingResultMsg is the outcome of answering an inbound pairing request.
type pairingResultMsg struct {
	inviteID string
	from     string
	state    string
	reason   string
	err      error
}

// handlePairingResult reports whether this machine actually joined.
func (v *nodesView) handlePairingResult(msg pairingResultMsg) tea.Cmd {
	if v.answering == msg.inviteID {
		v.answering = ""
	}
	if msg.err != nil {
		// Kept on screen. An error reply settles nothing: the cluster manager
		// answers a malformed PIN this way and leaves the session open, so the
		// request can be answered again — and if it is in fact gone, its
		// expiry retires it.
		v.status.error("could not answer the pairing request from %s: %s - answer it again",
			msg.from, msg.err)
		return nil
	}
	v.dropInbound(msg.inviteID)
	// Membership is what actually changed, so re-read it rather than trusting
	// this reply.
	cmds := []tea.Cmd{v.membersCmd(), v.identityCmd()}
	switch {
	case msg.state == inviteStatePaired:
		declines := v.declineWaiting()
		cmds = append(cmds, declines...)
		if len(declines) > 0 {
			v.status.ok("paired with %s - declined %d other pairing request(s), since a machine can only be in one cluster",
				msg.from, len(declines))
		} else {
			v.status.ok("paired with %s", msg.from)
		}
	case msg.reason == reasonIncorrectPIN:
		// The specific case worth naming: it is the operator's typo, and the
		// remedy is a fresh invite because the PIN is single-use.
		v.status.error("wrong PIN - ask %s to send a new invite, then try again", msg.from)
	case msg.state == inviteStateDeclined:
		v.status.info("pairing request declined")
	default:
		v.status.error("pairing with %s failed (%s) - ask for a new invite",
			msg.from, rejectReason(msg.reason))
	}
	return tea.Batch(cmds...)
}

// waitingDeclinedMsg is the outcome of declining a request that could no
// longer be accepted.
type waitingDeclinedMsg struct {
	from string
	err  error
}

// declineWaiting declines every pairing request still waiting, once this
// machine has joined a cluster through one of them.
//
// None of them can succeed now: the cluster manager refuses an accept on a
// machine that is already in a cluster. Declined, each sender is told at once.
// Left alone, they came up one by one looking answerable, and each sender went
// on showing its PIN until the invite expired.
func (v *nodesView) declineWaiting() []tea.Cmd {
	waiting := v.queued
	if v.inbound != nil {
		v.forgetPIN()
		waiting = append([]clusterInvite{*v.inbound}, waiting...)
	}
	v.inbound, v.queued = nil, nil
	cmds := make([]tea.Cmd, 0, len(waiting))
	for _, inv := range waiting {
		from := inv.FromNodeName
		cmds = append(cmds, call(v.client, "cluster:respond-to-invite",
			map[string]any{"inviteId": inv.InviteID, "accept": false},
			func(_ *rpc.Message, err error) tea.Msg {
				return waitingDeclinedMsg{from: from, err: err}
			}))
	}
	return cmds
}

// removeSelected drops the selected node's strongest relationship: cluster
// membership if it is a member, otherwise the manual entry that added it.
func (v *nodesView) removeSelected() tea.Cmd {
	row := v.selectedRow()
	if row == nil {
		v.status.error("no node selected")
		return nil
	}
	switch {
	case row.membership == membershipMember || row.membership == membershipPending:
		if row.self {
			v.status.error("use %s to leave the cluster from this machine",
				nodeLeaveKey.Help().Key)
			return nil
		}
		// Arm, do not act. Un-pairing a peer tears down mutual trust and is not
		// something a single keystroke on a moving list should do.
		v.confirmRemove = row.key
		v.status.arm("remove %s from the cluster? press y to confirm, any other key to cancel",
			row.name)
		return nil

	case row.manualID != "":
		return call(v.client, "node/remove", map[string]string{"id": row.manualID},
			func(_ *rpc.Message, err error) tea.Msg {
				return nodeActionMsg{what: "remove manual entry " + row.name, err: err}
			})

	default:
		v.status.info("%s is only discovered - nothing to remove", row.name)
		return nil
	}
}

// cancelInvite aborts an outbound invite the operator no longer wants to
// complete.
//
// This is the inviter's half of decline, and without it a PIN read out to the
// wrong person could only be retired by waiting for it to expire — the invite
// stayed live and answerable the whole time. The manager evicts the pairing
// session, which invalidates the PIN immediately, and best-effort tells the
// other side so its prompt disappears too.
//
// No confirmation: this is the safe direction. Cancelling an invite in flight
// costs one keystroke to redo, while the thing being prevented is a stranger
// completing a join.
func (v *nodesView) cancelInvite() tea.Cmd {
	if v.outboundInviteID == "" {
		v.status.info("no invite is waiting")
		return nil
	}
	sent := sentInvite{
		id: v.outboundInviteID, key: v.invitedKey, address: v.invitedAddress,
		name: v.outboundName, pin: v.outboundPIN,
	}
	// Cleared at once: the PIN must stop being displayed the moment the
	// operator asks, not when the round trip finishes.
	v.clearOutboundInvite()
	v.status.busy("cancelling invite...")
	return call(v.client, "cluster:cancel-invite", map[string]string{"inviteId": sent.id},
		func(_ *rpc.Message, err error) tea.Msg {
			return inviteCancelledMsg{invite: sent, err: err}
		})
}

// sentInvite is an outbound invite's tracking state, set aside while a cancel
// is in flight so a failed cancel can put it back.
type sentInvite struct{ id, key, address, name, pin string }

// inviteCancelledMsg is the outcome of cluster:cancel-invite.
type inviteCancelledMsg struct {
	invite sentInvite
	err    error
}

// handleInviteCancelled reports a cancel, and puts the invite back if the
// cancel failed.
//
// A failed cancel leaves the session live and its PIN answerable, so it is
// tracked and shown again, and c can be pressed again — unless another invite
// has been sent in the meantime, which has the one slot now.
func (v *nodesView) handleInviteCancelled(msg inviteCancelledMsg) tea.Cmd {
	if msg.err == nil {
		v.status.ok("invite to %s cancelled", msg.invite.name)
		return tea.Batch(v.membersCmd(), v.manualCmd())
	}
	if v.outboundInviteID != "" || v.inviteSending {
		v.status.error("could not cancel the invite to %s: %s", msg.invite.name, msg.err)
		return nil
	}
	s := msg.invite
	v.outboundInviteID, v.invitedKey, v.invitedAddress = s.id, s.key, s.address
	v.outboundName, v.outboundPIN = s.name, s.pin
	v.status.pin("could not cancel the invite to %s (%s) - PIN %s is still live; %s to try again",
		s.name, msg.err, s.pin, nodeCancelKey.Help().Key)
	return nil
}

// removeMember un-pairs a confirmed peer.
//
// Keyed by the stable nodeUuid rather than the display name: a member that
// renamed its PC keeps its UUID, so matching on a possibly stale name would
// silently fail. The row is looked up again by key so a list that re-sorted
// between arming and confirming cannot redirect the removal at another node.
func (v *nodesView) removeMember(key string) tea.Cmd {
	name := key
	for _, n := range v.all {
		if n.key == key {
			name = n.name
			break
		}
	}
	// Replaces the armed prompt, which is sticky: without this the screen went
	// on asking whether to remove the peer while the removal was under way.
	v.status.busy("removing %s from the cluster...", name)
	return call(v.client, "nodes:remove", map[string]string{"nodeUuid": key},
		func(_ *rpc.Message, err error) tea.Msg {
			return nodeActionMsg{what: "remove " + name, err: err}
		})
}

// leaveCluster unjoins this node. The cluster-manager tears down local trust and
// pushes cluster:identity-changed and nodes:changed, which refresh the view.
func (v *nodesView) leaveCluster() tea.Cmd {
	if v.identity.ClusterID == "" {
		v.status.error("not in a cluster")
		return nil
	}
	// Same as removeMember: the armed prompt does not expire, and this is the
	// slowest relay in the client, so it has to be replaced rather than left
	// asking a question that has already been answered.
	v.status.busy("leaving the cluster...")
	return call(v.client, "cluster:leave", nil, func(_ *rpc.Message, err error) tea.Msg {
		return nodeActionMsg{what: "leave cluster", err: err}
	})
}

// rebuild re-merges the feeds and repaints the table, preserving the operator's
// selection by key across the re-sort.
func (v *nodesView) rebuild() {
	v.all = mergeNodes(v.feeds)
	v.rows = filterNodeRows(v.all, v.filter)
	v.retirePendingInvite()
	v.followDetail()

	rows := make([]table.Row, 0, len(v.rows))
	for _, n := range v.rows {
		name := n.name
		if n.self {
			name += " (this machine)"
		}
		models := "-"
		if c := n.modelCount(); c > 0 {
			models = strconv.Itoa(c)
		}
		rows = append(rows, table.Row{
			name,
			n.address,
			n.presence.String(),
			n.membership.String(),
			models,
		})
	}
	v.table.SetRows(rows)
	v.restoreSelection()
}

// followDetail hands an open detail screen its node's freshly merged row.
//
// Looked up in the full set rather than the filtered view, since a filter
// hiding a node does not make it any less present. A hand-added host is also
// looked for by its manual entry: once discovery finds it, its row takes the
// host's own key, and the screen opened on the manual one must not read that
// as the node vanishing.
func (v *nodesView) followDetail() {
	if v.detail == nil {
		return
	}
	for _, n := range v.all {
		if n.key == v.detail.node.key {
			v.detail.followNode(n, true)
			return
		}
	}
	if id := v.detail.node.manualID; id != "" {
		for _, n := range v.all {
			if n.manualID == id {
				v.detail.followNode(n, true)
				return
			}
		}
	}
	v.detail.followNode(nodeRow{}, false)
}

// restoreSelection puts the cursor back on the node it was on before the merge
// re-ordered the rows, falling back to the first row when that node is gone.
func (v *nodesView) restoreSelection() {
	if v.selectedKey == "" && len(v.rows) > 0 {
		v.selectedKey = v.rows[0].key
	}
	for i, n := range v.rows {
		if n.key == v.selectedKey {
			v.table.SetCursor(i)
			return
		}
	}
	if len(v.rows) > 0 {
		v.selectedKey = v.rows[0].key
		v.table.SetCursor(0)
	}
}

// retirePendingInvite replaces the pinned PIN with a success note once the node
// we invited shows up as a member. Pairing completing is the one outcome with no
// terminal notification of its own, so it is detected from the merged state.
func (v *nodesView) retirePendingInvite() {
	if v.invitedKey == "" && v.invitedAddress == "" {
		return
	}
	// The full set, not the filtered view: a peer that joined while hidden by a
	// filter still completes the pairing, and its PIN still has to stop showing.
	for _, n := range v.all {
		if n.membership != membershipMember {
			continue
		}
		// Either identity works: a discovered node was invited by UUID, while an
		// invite by address has none, so that peer is recognised by the address
		// it was invited at. Without the address arm, a PIN pinned by the
		// by-address path was never retired at all.
		if (v.invitedKey != "" && n.key == v.invitedKey) ||
			(v.invitedAddress != "" && addressMatches(n, v.invitedAddress)) {
			v.clearOutboundInvite()
			v.status.ok("%s joined the cluster", n.name)
			return
		}
	}
}

// addressMatches reports whether a node answers to the given address. The
// operator may have typed a host:port form, and a node publishes several
// addresses, so the host part is compared against every candidate.
func addressMatches(n nodeRow, address string) bool {
	host := normalizeHost(address)
	if host == "" {
		return false
	}
	if normalizeHost(n.address) == host {
		return true
	}
	for _, candidate := range n.addresses {
		if normalizeHost(candidate) == host {
			return true
		}
	}
	return strings.EqualFold(strings.TrimSpace(n.name), host)
}

func (v *nodesView) rowAt(idx int) *nodeRow {
	if idx < 0 || idx >= len(v.rows) {
		return nil
	}
	return &v.rows[idx]
}

func (v *nodesView) selectedRow() *nodeRow {
	for i, n := range v.rows {
		if n.key == v.selectedKey {
			return &v.rows[i]
		}
	}
	return v.rowAt(v.table.Cursor())
}

func (v *nodesView) View() string {
	if v.detail != nil {
		// Pairing stays in view over the detail screen, which replaces the
		// list where it is otherwise shown. Sized here, from the line actually
		// rendered, because it comes and goes with events the detail screen
		// never sees.
		line := v.pairingLine()
		h := v.height
		if line != "" {
			h -= countLines(line)
		}
		v.detail.SetSize(v.width, h)
		return joinLines(line, v.detail.View())
	}

	// Everything that is not the table, gathered before the table is sized so
	// its height can be whatever is left. Empty entries cost nothing.
	above := v.clusterLine()

	filterNote := ""
	if v.filter != "" && len(v.rows) > 0 {
		// A filtered table looks like the whole cluster, so it has to say it is
		// not. Without this an operator can conclude a node has vanished when
		// they are simply still filtered.
		filterNote = footerStyle.Render(fmt.Sprintf(
			"showing %d of %d - filter %q, esc to clear",
			len(v.rows), len(v.all), v.filter))
	}
	feedNote := ""
	if warning := v.feedWarning(); warning != "" && len(v.rows) > 0 {
		// A populated table can still be missing a feed, and then it is worse
		// than an empty one: it looks complete.
		feedNote = statusErrStyle.Render("Some node data is " + warning)
	}
	inboundNote := v.inboundPrompt()
	editor := ""
	if v.mode != nodesInputNone {
		editor = v.inputLabel() + v.input.View()
	}

	body := ""
	if len(v.rows) == 0 {
		// An empty table means one of three very different things, and the
		// operator's next move depends on which: clear the filter, wait, or go
		// look at the service. Say which one this is.
		switch {
		case v.filter != "":
			body = footerStyle.Render(fmt.Sprintf(
				"No node matches %q. %d known - press esc to clear the filter.",
				v.filter, len(v.all)))
		case v.feedWarning() != "":
			body = statusErrStyle.Render("Cannot read the node list - " + v.feedWarning())
		default:
			body = footerStyle.Render(fmt.Sprintf(
				"No nodes yet. Discovery is browsing the network; press %s to add one by address.",
				nodeAddKey.Help().Key))
		}
	}

	status := v.status.render()
	if len(v.rows) > 0 {
		if fitTable(&v.table, v.height, above, filterNote, feedNote, inboundNote, editor, status) {
			body = v.table.View()
		} else {
			body = footerStyle.Render(fmt.Sprintf(
				"  (too little room to list %d nodes)", len(v.rows)))
		}
	}
	return joinLines(above, body, filterNote, feedNote, inboundNote, editor, status)
}

// clusterLine is the one-line summary of this machine's cluster standing.
func (v *nodesView) clusterLine() string {
	if v.identity.ClusterID == "" {
		return footerStyle.Render("Not in a cluster - inviting a node forms one automatically")
	}
	members := 0
	// Counted over every known node: a filter narrows what is on screen, not
	// what the cluster contains, and a shrinking member count would be alarming.
	for _, n := range v.all {
		if n.membership == membershipMember {
			members++
		}
	}
	name := v.identity.Name
	if name == "" {
		name = v.identity.NodeID
	}
	// The label if one is set, the id otherwise: the id is what anything
	// operational keys off, so it is the honest fallback rather than "unnamed".
	label := v.clusterName
	if label == "" {
		label = truncate(v.identity.ClusterID, 12)
	}
	return titleStyle.Render(fmt.Sprintf("Cluster %s - %d member(s), this machine is %s",
		label, members, name))
}

func (v *nodesView) inputLabel() string {
	switch v.mode {
	case nodesInputManualAddress:
		return "find node at host: "
	case nodesInputInviteAddress:
		return "pair with host: "
	case nodesInputPin:
		return "PIN: "
	default:
		return ""
	}
}

func (v *nodesView) Help() []key.Binding {
	if v.detail != nil {
		return v.detail.Help()
	}
	if v.mode != nodesInputNone {
		switch v.mode {
		case nodesInputFilter:
			return inputHelp("apply filter")
		case nodesInputPin:
			return inputHelp("submit PIN")
		default:
			return inputHelp("submit address")
		}
	}
	if v.confirmLeave || v.confirmRemove != "" {
		return []key.Binding{nodeConfirmKey}
	}
	bindings := []key.Binding{nodeDetailKey, nodeInviteKey, nodeInviteAddrKey, nodeAddKey, nodeRemoveKey, nodeFilterKey}
	if v.filter != "" {
		bindings = append(bindings, nodeClearKey)
	}
	if v.inbound != nil && v.answering == "" {
		bindings = append(bindings, nodePairKey, nodeDeclineKey)
	}
	// Only while there is something to cancel: a key offered with nothing
	// pending invites a press that can only answer "nothing is waiting".
	if v.outboundInviteID != "" {
		bindings = append(bindings, nodeCancelKey)
	}
	if v.identity.ClusterID != "" {
		bindings = append(bindings, nodeLeaveKey)
	}
	return bindings
}
