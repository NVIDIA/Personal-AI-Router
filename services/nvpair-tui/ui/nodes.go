// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
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

// nodesView is the machine-centric surface: every node PAIR knows about,
// whether discovered, added by hand, or paired into our cluster, in one table
// with a detail pane for the selected row.
//
// It replaces the separate Nodes, Manual, and Cluster tabs. Those split one
// machine's story across three places — its address in one, its membership in
// another, its reachability in a third — and gave pairing two entry points with
// different guards. A node is the unit an operator thinks in, so it is the unit
// the tab is built around.
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
	// cluster:get-node-id does not return it: it is a node setting, and arrives
	// either from settings/get-cluster-friendly-name or on the
	// cluster:identity-changed push. Without showing it here the setting was
	// write-only — you could name a cluster and never see the name again.
	clusterName string
	// inbound is the most recent pairing request awaiting our answer.
	inbound *clusterInvite
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

type clusterMembersMsg struct {
	nodes []clusterNode
	err   error
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
	err      error
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
	return call(v.client, "settings/get-cluster-friendly-name", nil,
		func(msg *rpc.Message, err error) tea.Msg {
			if err != nil {
				return clusterNameMsg{err: err}
			}
			var r struct {
				Value string `json:"value"`
			}
			_ = decodeParams(msg.Result, &r)
			return clusterNameMsg{name: r.Value}
		})
}

func (v *nodesView) identityCmd() tea.Cmd {
	return call(v.client, "cluster:get-node-id", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return clusterIdentityMsg{err: err}
		}
		var id clusterIdentity
		_ = decodeParams(msg.Result, &id)
		return clusterIdentityMsg{id: id}
	})
}

func (v *nodesView) membersCmd() tea.Cmd {
	return call(v.client, "nodes:get-initial", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return clusterMembersMsg{err: err}
		}
		var r struct {
			Nodes []clusterNode `json:"nodes"`
		}
		_ = decodeParams(msg.Result, &r)
		return clusterMembersMsg{nodes: r.Nodes}
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
		_ = decodeParams(msg.Result, &r)
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
		if msg.err == nil {
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

	case NotificationMsg:
		return v.handleNotification(msg.Msg)

	case tea.KeyMsg:
		return v.handleKey(msg)
	}
	return nil
}

func (v *nodesView) handleInviteResult(msg nodeInviteMsg) tea.Cmd {
	switch {
	case msg.err != nil:
		v.clearOutboundInvite()
		v.status.error("invite failed: %s", msg.err)
	case msg.rejected:
		v.clearOutboundInvite()
		v.status.error("%s rejected the invite (%s) - remove the existing relationship first",
			msg.name, rejectReason(msg.reason))
	case msg.pin != "":
		// Pinned, not expiring: the operator reads this PIN to someone at the
		// other machine. It clears when the invite resolves.
		//
		// The id is recorded so a terminal notification can be matched to this
		// session, and the address so an invite sent by address — which has no
		// node identity — can still recognise the peer once it joins.
		v.outboundInviteID = msg.inviteID
		v.invitedAddress = msg.address
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
}

func (v *nodesView) handleNotification(msg *rpc.Message) tea.Cmd {
	switch msg.Method {
	case "discovery:nodes-changed":
		var nodes []availableNode
		_ = decodeParams(msg.Params, &nodes)
		v.feeds.discovered = nodes
		v.rebuild()

	case "nodes:changed":
		var r struct {
			Nodes []clusterNode `json:"nodes"`
		}
		_ = decodeParams(msg.Params, &r)
		v.feeds.members = r.Nodes
		v.rebuild()

	case "cluster:identity-changed":
		var r struct {
			ClusterID           string `json:"clusterId"`
			ClusterFriendlyName string `json:"clusterFriendlyName"`
		}
		_ = decodeParams(msg.Params, &r)
		v.identity.ClusterID = r.ClusterID
		v.clusterName = r.ClusterFriendlyName

	case "cluster:invite-received":
		var inv clusterInvite
		_ = decodeParams(msg.Params, &inv)
		v.inbound = &inv
		v.status.pin("pairing request from %s - press %s to accept, %s to decline",
			inv.FromNodeName, nodePairKey.Help().Key, nodeDeclineKey.Help().Key)
		v.SetSize(v.width, v.height)

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
	_ = decodeParams(params, &ref)
	if ref.InviteID == "" {
		return
	}

	if ref.InviteID == v.outboundInviteID {
		v.clearOutboundInvite()
		v.status.set(outcome.kind, "invite %s", outcome.label)
	}
	if v.inbound != nil && ref.InviteID == v.inbound.InviteID {
		v.inbound = nil
		v.status.set(outcome.kind, "pairing request %s", outcome.label)
	}
	v.SetSize(v.width, v.height)
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
		return v.inviteSelected()
	case key.Matches(msg, nodeInviteAddrKey):
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
		v.beginInput(nodesInputPin, "PIN from the inviting node")
		return textinput.Blink
	case key.Matches(msg, nodeDeclineKey) && v.inbound != nil:
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
	v.status.busy("inviting %s...", name)
	return inviteNodeCmd(v.client, params, func(res inviteNodeResult, err error) tea.Msg {
		return inviteResultMsg(name, "", res, err)
	})
}

// inviteResultMsg maps a cluster:invite-node outcome onto the view message.
// address is empty for an invite aimed at a discovered node.
func inviteResultMsg(name, address string, res inviteNodeResult, err error) tea.Msg {
	if err != nil {
		return nodeInviteMsg{name: name, address: address, err: err}
	}
	if res.State == "rejected" {
		return nodeInviteMsg{
			name: name, address: address, rejected: true, reason: res.Reason,
		}
	}
	pin := ""
	if res.Pin != nil {
		pin = *res.Pin
	}
	return nodeInviteMsg{name: name, address: address, inviteID: res.InviteID, pin: pin}
}

func (v *nodesView) respondToInvite(accept bool, pin string) tea.Cmd {
	if v.inbound == nil {
		// Say so rather than doing nothing. A key that silently ignores a press
		// is indistinguishable from one the terminal dropped, and the accept
		// path already answers.
		v.status.info("no pairing request to answer")
		return nil
	}
	params := map[string]any{"inviteId": v.inbound.InviteID, "accept": accept}
	if accept && pin != "" {
		params["pin"] = pin
	}
	from := v.inbound.FromNodeName
	v.inbound = nil
	v.SetSize(v.width, v.height)

	if !accept {
		v.status.busy("declining the pairing request...")
		return call(v.client, "cluster:respond-to-invite", params,
			func(_ *rpc.Message, err error) tea.Msg {
				return nodeActionMsg{what: "decline pairing", err: err}
			})
	}
	// The handshake crosses to another machine and is the slowest call this tab
	// makes; the inbound prompt has already been cleared, so without this the
	// screen is blank until it answers.
	v.status.busy("pairing with %s...", from)
	return call(v.client, "cluster:respond-to-invite", params,
		func(msg *rpc.Message, err error) tea.Msg {
			if err != nil {
				return pairingResultMsg{from: from, err: err}
			}
			// A wrong PIN is NOT a JSON-RPC error. The cluster manager tears the
			// session down and replies successfully with the invite, whose state
			// is "failed" and whose reason says why. Reading only the transport
			// error reported a green "accept pairing ok" for a pairing that had
			// just been rejected — and since the prompt is already gone by then,
			// nothing later corrected it.
			var res inviteNodeResult
			_ = decodeParams(msg.Result, &res)
			return pairingResultMsg{from: from, state: res.State, reason: res.Reason}
		})
}

// pairingResultMsg is the outcome of answering an inbound pairing request.
type pairingResultMsg struct {
	from   string
	state  string
	reason string
	err    error
}

// handlePairingResult reports whether this machine actually joined.
func (v *nodesView) handlePairingResult(msg pairingResultMsg) tea.Cmd {
	switch {
	case msg.err != nil:
		v.status.error("could not answer the pairing request: %s", msg.err)
	case msg.state == "paired":
		v.status.ok("paired with %s", msg.from)
	case msg.reason == reasonIncorrectPIN:
		// The specific case worth naming: it is the operator's typo, and the
		// remedy is a fresh invite because the PIN is single-use.
		v.status.error("wrong PIN - ask %s to send a new invite, then try again", msg.from)
	case msg.state == "declined":
		v.status.info("pairing request declined")
	default:
		v.status.error("pairing with %s failed (%s) - ask for a new invite",
			msg.from, rejectReason(msg.reason))
	}
	// Membership is what actually changed, so re-read it rather than trusting
	// this reply.
	return tea.Batch(v.membersCmd(), v.identityCmd())
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
	id := v.outboundInviteID
	// Cleared optimistically: the PIN must stop being displayed the moment the
	// operator asks, not when the round trip finishes. A failure restores
	// nothing because the invite is either already gone or about to expire.
	v.clearOutboundInvite()
	v.status.busy("cancelling invite...")
	return call(v.client, "cluster:cancel-invite", map[string]string{"inviteId": id},
		func(_ *rpc.Message, err error) tea.Msg {
			return nodeActionMsg{what: "cancel invite", err: err}
		})
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
		return v.detail.View()
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
	inboundNote := ""
	if v.inbound != nil {
		inboundNote = statusOKStyle.Render(fmt.Sprintf(
			"pairing request from %s - %s to accept, %s to decline",
			v.inbound.FromNodeName, nodePairKey.Help().Key, nodeDeclineKey.Help().Key))
	}
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
	if v.inbound != nil {
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
