// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"nvpair-shared/cableprobe"
	"nvpair-shared/hostbootstrap"
	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

const setupRefreshInterval = 3 * time.Second

type setupInputMode int

const (
	setupInputNone setupInputMode = iota
	setupInputAddress
	setupInputPort
	setupInputUsername
	setupInputAuth
	setupInputCredential
	setupInputPassphrase
	setupInputElevation
	setupInputLifetime
	setupInputArtifact
	setupInputImportArtifact
	setupInputCablePorts
)

type setupApproval int

const (
	setupApproveNone setupApproval = iota
	setupApproveOnboarding
	setupApproveCable
	setupApproveCleanup
	setupApproveFabric
)

type setupTickMsg struct{}

type setupView struct {
	client *rpc.Client
	table  table.Model
	input  textinput.Model
	mode   setupInputMode

	candidates     []setupCandidate
	artifacts      []setupArtifact
	selected       map[string]bool
	accepted       map[string]string
	accessAccounts map[string]string

	bootstrapCatalog         setupBootstrapCatalog
	bootstrapKeys            setupBootstrapControllerKeys
	bootstrapScopes          []setupDiscoveryScope
	bootstrapRequest         *hostbootstrap.Request
	bootstrapCandidateID     string
	bootstrapStatus          *hostbootstrap.Status
	bootstrapPlan            *hostbootstrap.Plan
	bootstrapReceipt         *hostbootstrap.Receipt
	bootstrapKeyIndex        int
	bootstrapTargetOverrides map[string]hostbootstrap.Target
	bootstrapLane            hostbootstrap.Lane
	bootstrapRole            hostbootstrap.Role

	draftAddress string
	accessDraft  setupAccessRequest

	pendingSelection cableprobe.ReviewRequest
	cableSelection   cableprobe.ReviewRequest

	review             *setupReview
	operation          *setupOperation
	cableReview        *cableprobe.Review
	cleanupReview      *cableprobe.CleanupReview
	cableRun           *cableprobe.Run
	retained           cableprobe.RetainedRuns
	fabricReview       *setupFabricReview
	fabricOperation    *setupFabricOperation
	approval           setupApproval
	uncertainSetupID   string
	uncertainCableID   string
	uncertainOperation string

	loadedCandidates bool
	loadedRetained   bool
	pending          string
	status           string
	lastFabricPoll   time.Time
	width, height    int
}

var (
	setupRefreshKey           = key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh"))
	setupSelectKey            = key.NewBinding(key.WithKeys(" "), key.WithHelp("space", "select"))
	setupAddKey               = key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add target"))
	setupAccessKey            = key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "setup access"))
	setupBootstrapKey         = key.NewBinding(key.WithKeys("b"), key.WithHelp("b", "set up device"))
	setupBootstrapPublicKey   = key.NewBinding(key.WithKeys("P"), key.WithHelp("P", "next public key"))
	setupBootstrapTargetKey   = key.NewBinding(key.WithKeys("T"), key.WithHelp("T", "target override"))
	setupBootstrapLaneKey     = key.NewBinding(key.WithKeys("Z"), key.WithHelp("Z", "setup lane"))
	setupBootstrapRoleKey     = key.NewBinding(key.WithKeys("G"), key.WithHelp("G", "PAIR role"))
	setupBootstrapDiscoverKey = key.NewBinding(key.WithKeys("D"), key.WithHelp("D", "discover scope"))
	setupBootstrapImportKey   = key.NewBinding(key.WithKeys("J"), key.WithHelp("J", "import package"))
	setupCableKey             = key.NewBinding(key.WithKeys("U"), key.WithHelp("U", "cable access"))
	setupTrustKey             = key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "accept SSH key"))
	setupInspectKey           = key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "inspect setup"))
	setupApproveKey           = key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "approve shown review"))
	setupRetryKey             = key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "retry setup"))
	setupCancelKey            = key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "cancel/rollback"))
	setupCableReviewKey       = key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "review cable"))
	setupCleanupKey           = key.NewBinding(key.WithKeys("K"), key.WithHelp("K", "review cleanup"))
	setupFabricKey            = key.NewBinding(key.WithKeys("F"), key.WithHelp("F", "review fabric"))
	setupRecoverKey           = key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "recover setup/fabric"))
)

func newSetupView(client *rpc.Client) *setupView {
	input := textinput.New()
	v := &setupView{
		client: client, input: input, table: newTable(nil),
		selected:                 map[string]bool{},
		accepted:                 map[string]string{},
		accessAccounts:           map[string]string{},
		bootstrapTargetOverrides: map[string]hostbootstrap.Target{},
		bootstrapLane:            hostbootstrap.LaneQuickConnect,
		bootstrapRole:            hostbootstrap.RoleAuto,
	}
	return v
}

func (v *setupView) Title() string { return "Setup" }

func (v *setupView) Init() tea.Cmd {
	return tea.Batch(
		setupCandidatesCmd(v.client),
		setupBootstrapCatalogCmd(v.client),
		setupBootstrapControllerKeysCmd(v.client),
		setupScopesCmd(v.client),
		setupOperationCmd(v.client, "engine:onboarding-status", "setup status", ""),
		setupCableRetainedCmd(v.client),
		v.tickCmd(),
	)
}

func (v *setupView) tickCmd() tea.Cmd {
	return tea.Tick(setupRefreshInterval, func(time.Time) tea.Msg { return setupTickMsg{} })
}

func (v *setupView) SetSize(w, h int) {
	v.width, v.height = w, h
	address := clampWidth(w/4, 12)
	label := clampWidth(w/5, 10)
	access := clampWidth(w/5, 10)
	bootstrap := clampWidth(w/6, 12)
	ssh := clampWidth(w-address-label-access-bootstrap-8, 8)
	v.table.SetColumns([]table.Column{
		{Title: "", Width: 3},
		{Title: "TARGET", Width: label},
		{Title: "ADDRESS", Width: address},
		{Title: "BOOTSTRAP", Width: bootstrap},
		{Title: "ACCESS", Width: access},
		{Title: "SSH", Width: ssh},
	})
	v.table.SetWidth(w)
	v.table.SetHeight(clampWidth(h/3, 3))
}

func (v *setupView) CapturingInput() bool { return v.mode != setupInputNone }

func (v *setupView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case setupCandidatesMsg:
		v.loadedCandidates = true
		if msg.err != nil {
			v.status = "candidate refresh failed: " + msg.err.Error()
			return nil
		}
		if msg.replace {
			v.candidates, v.artifacts = msg.candidates, msg.artifacts
		} else {
			v.mergeCandidates(msg.candidates)
		}
		v.reconcileCandidateChoices()
		v.renderCandidates()
		if v.pending == "add target" {
			v.pending, v.status = "", "target added; select it with space"
		} else if v.pending == "bootstrap discovery" {
			v.pending, v.status = "", "backend discovery completed"
		}
	case setupBootstrapCatalogMsg:
		if msg.err != nil {
			v.status = "bootstrap catalog unavailable: " + msg.err.Error()
			return nil
		}
		v.bootstrapCatalog = msg.catalog
	case setupBootstrapControllerKeysMsg:
		if msg.err != nil {
			v.status = "controller public keys unavailable: " + msg.err.Error()
			return nil
		}
		v.bootstrapKeys = msg.keys
		if v.bootstrapKeyIndex >= len(v.bootstrapKeys.Keys) {
			v.bootstrapKeyIndex = 0
		}
	case setupScopesMsg:
		if msg.err != nil {
			v.status = "setup scopes unavailable: " + msg.err.Error()
			return nil
		}
		v.bootstrapScopes = msg.scopes
	case setupArtifactMsg:
		v.pending = ""
		if msg.err != nil {
			v.status = "artifact import failed: " + msg.err.Error()
			return nil
		}
		v.mergeArtifact(msg.artifact)
		v.status = "artifact imported into the existing setup catalog"
	case setupAccessMsg:
		v.pending = ""
		if msg.err != nil {
			v.status = "access failed: " + msg.err.Error()
			return setupCandidatesCmd(v.client)
		}
		v.mergeCandidates(msg.candidates)
		for _, candidate := range msg.candidates {
			if candidate.AccessAvailable && msg.username != "" {
				v.accessAccounts[candidate.CandidateID] = msg.username
			}
		}
		v.reconcileCandidateChoices()
		v.renderCandidates()
		v.status = "transient access bound; review every displayed SSH fingerprint"
		return setupCandidatesCmd(v.client)
	case setupBootstrapMsg:
		if msg.action != "bootstrap status" {
			v.pending = ""
		}
		if msg.err != nil {
			v.status = msg.action + " unavailable: " + msg.err.Error()
			return nil
		}
		if msg.status != nil {
			v.bootstrapStatus = msg.status
		}
		if msg.plan != nil {
			v.bootstrapPlan = msg.plan
		}
		if msg.receipt != nil {
			v.bootstrapReceipt = msg.receipt
		}
		if msg.action != "bootstrap status" {
			v.status = msg.action + " accepted target-produced state"
		}
		if msg.receipt != nil {
			return setupCandidatesCmd(v.client)
		}
	case setupReviewMsg:
		v.pending = ""
		if msg.err != nil {
			v.status = "setup inspection failed: " + msg.err.Error()
			return setupCandidatesCmd(v.client)
		}
		ids := append([]string(nil), v.accessibleSelection()...)
		if err := validateSetupReview(msg.review, ids); err != nil {
			v.review, v.approval = nil, setupApproveNone
			v.status = "setup inspection rejected: " + err.Error()
			return nil
		}
		v.review = &msg.review
		v.approval = setupApproveOnboarding
		v.status = "setup review ready; p records approval of this exact review"
	case setupOperationMsg:
		if msg.action != "setup status" && msg.action != "setup reconciliation" {
			v.pending = ""
		}
		if msg.err != nil {
			v.status = msg.action + " unavailable: " + msg.err.Error()
			if msg.action == "setup approval" && v.uncertainSetupID != "" {
				return setupOperationByReviewCmd(v.client, v.uncertainSetupID)
			}
			return nil
		}
		if msg.operation != nil {
			if v.uncertainSetupID == "" || msg.operation.ReviewID == v.uncertainSetupID {
				if v.operation == nil || v.operation.OperationID == msg.operation.OperationID || msg.operation.StartedAt >= v.operation.StartedAt {
					v.operation = msg.operation
				}
				if msg.operation.ReviewID == v.uncertainSetupID {
					v.uncertainSetupID = ""
				}
			}
			if msg.action != "setup status" {
				v.status = msg.action + " reconciled to authoritative operation status"
			}
		}
		return setupCandidatesCmd(v.client)
	case setupCableRetainedMsg:
		v.loadedRetained = true
		if msg.err != nil {
			v.status = "retained cable status unavailable: " + msg.err.Error()
			return nil
		}
		v.retained = msg.retained
		if len(msg.retained.Runs) > 0 {
			row := msg.retained.Runs[0]
			if len(v.cableSelection.NodeIDs) == 0 {
				v.cableSelection = cableprobe.ReviewRequest{NodeIDs: append([]string(nil), row.NodeIDs...), Ports: append([]cableprobe.PortRef(nil), row.Ports...)}
			}
			if v.cableRun == nil || v.cableRun.RunID != row.RunID {
				return setupCableStatusCmd(v.client, row.RunID)
			}
		}
	case setupCableReviewMsg:
		v.pending = ""
		if msg.err != nil {
			v.status = "cable review failed: " + msg.err.Error()
			return tea.Batch(setupCandidatesCmd(v.client), setupCableRetainedCmd(v.client))
		}
		if err := validateCableReview(msg.review, v.pendingSelection); err != nil {
			v.cableReview, v.approval = nil, setupApproveNone
			v.status = "cable review rejected: " + err.Error()
			return nil
		}
		v.cableSelection = cloneCableSelection(v.pendingSelection)
		v.cableReview = &msg.review
		v.approval = setupApproveCable
		v.status = "cable review ready; p approves the finite administrator check"
		return setupCandidatesCmd(v.client)
	case setupCleanupReviewMsg:
		v.pending = ""
		if msg.err != nil {
			v.status = "cleanup review failed: " + msg.err.Error()
			return tea.Batch(setupCandidatesCmd(v.client), setupCableRetainedCmd(v.client))
		}
		v.cleanupReview = &msg.review
		v.approval = setupApproveCleanup
		v.status = "cleanup review ready; p approves only its bounded verification"
		return setupCandidatesCmd(v.client)
	case setupCableRunMsg:
		if msg.action != "cable status" && msg.action != "cable reconciliation" {
			v.pending = ""
		}
		if msg.run != nil {
			newer := v.cableRun == nil || v.cableRun.RunID == msg.run.RunID || msg.action != "cable status" || msg.run.StartedAt >= v.cableRun.StartedAt
			if newer {
				v.cableRun = msg.run
				v.cableSelection = selectionFromCableRun(*msg.run)
			}
		}
		if msg.err != nil {
			v.status = msg.action + " unavailable: " + msg.err.Error()
			if msg.action == "cable start" && v.uncertainCableID != "" {
				return setupCableStatusByReviewCmd(v.client, v.uncertainCableID)
			}
			return setupCableRetainedCmd(v.client)
		}
		if msg.run != nil && msg.run.ReviewID == v.uncertainCableID {
			v.uncertainCableID = ""
		}
		if msg.action != "cable status" {
			v.status = msg.action + " reconciled to authoritative run status"
		}
		return setupCableRetainedCmd(v.client)
	case setupFabricReviewMsg:
		v.pending = ""
		if msg.err != nil {
			v.status = "fabric review failed: " + msg.err.Error()
			return setupCandidatesCmd(v.client)
		}
		if err := validateFabricReview(msg.review, v.cableSelection); err != nil {
			v.fabricReview, v.approval = nil, setupApproveNone
			v.status = "fabric review rejected: " + err.Error()
			return nil
		}
		v.fabricReview = &msg.review
		v.approval = setupApproveFabric
		v.status = "fabric review ready; p approves only the exact displayed bindings"
	case setupFabricOperationMsg:
		if msg.action != "fabric status" && msg.action != "fabric reconciliation" {
			v.pending = ""
		}
		if msg.operation != nil {
			v.fabricOperation = msg.operation
		}
		if msg.err != nil {
			v.status = msg.action + " unavailable: " + msg.err.Error()
			if msg.action == "fabric approval" && v.uncertainOperation != "" {
				return setupFabricOperationCmd(v.client, "engine:fabric-status", "fabric reconciliation", v.uncertainOperation)
			}
			return nil
		}
		v.uncertainOperation = ""
		if msg.action != "fabric status" {
			v.status = msg.action + " reconciled to authoritative fabric status"
		}
	case setupTickMsg:
		return tea.Batch(v.pollCmd(), v.tickCmd())
	case tea.KeyMsg:
		return v.handleKey(msg)
	}
	return nil
}

func (v *setupView) pollCmd() tea.Cmd {
	cmds := []tea.Cmd{setupCableRetainedCmd(v.client)}
	operationID := ""
	if v.operation != nil {
		operationID = v.operation.OperationID
	}
	cmds = append(cmds, setupOperationCmd(v.client, "engine:onboarding-status", "setup status", operationID))
	if operationID == "" && v.uncertainSetupID != "" {
		cmds = append(cmds, setupOperationByReviewCmd(v.client, v.uncertainSetupID))
	}
	if v.bootstrapRequest != nil &&
		v.bootstrapStatus != nil &&
		(v.bootstrapStatus.Phase == hostbootstrap.PhaseApply ||
			v.bootstrapStatus.Phase == hostbootstrap.PhaseVerify) {
		if reference, ok := v.bootstrapReference(); ok {
			cmds = append(
				cmds,
				setupBootstrapStatusCmd(
					v.client,
					reference,
					v.bootstrapRequest.OperationID,
				),
			)
		}
	}
	if v.cableRun != nil && v.cableRun.RunID != "" {
		cmds = append(cmds, setupCableStatusCmd(v.client, v.cableRun.RunID))
	}
	if v.uncertainCableID != "" {
		cmds = append(cmds, setupCableStatusByReviewCmd(v.client, v.uncertainCableID))
	}
	fabricID := v.uncertainOperation
	if v.fabricOperation != nil && v.fabricOperation.OperationID != "" {
		fabricID = v.fabricOperation.OperationID
	}
	if fabricID != "" && (v.lastFabricPoll.IsZero() || time.Since(v.lastFabricPoll) >= 30*time.Second || v.fabricOperation != nil && v.fabricOperation.State != "active" && time.Since(v.lastFabricPoll) >= setupRefreshInterval) {
		v.lastFabricPoll = time.Now()
		cmds = append(cmds, setupFabricOperationCmd(v.client, "engine:fabric-status", "fabric status", fabricID))
	}
	return tea.Batch(cmds...)
}

func (v *setupView) handleKey(msg tea.KeyMsg) tea.Cmd {
	if v.mode != setupInputNone {
		switch msg.String() {
		case "enter":
			return v.submitInput()
		case "esc":
			v.cancelInput()
			v.status = "setup input cancelled; no request sent"
			return nil
		}
		var cmd tea.Cmd
		v.input, cmd = v.input.Update(msg)
		return cmd
	}
	if v.pending != "" {
		return nil
	}
	switch {
	case key.Matches(msg, setupRefreshKey):
		v.status = "refreshing authoritative setup state..."
		return tea.Batch(setupCandidatesCmd(v.client), v.pollCmd())
	case key.Matches(msg, setupSelectKey):
		v.toggleSelected()
		return nil
	case key.Matches(msg, setupAddKey):
		v.beginInput(setupInputAddress, "exact device IP address", false)
		return textinput.Blink
	case key.Matches(msg, setupAccessKey):
		return v.beginAccess("")
	case key.Matches(msg, setupBootstrapKey):
		return v.bootstrapAction()
	case key.Matches(msg, setupBootstrapPublicKey):
		v.nextBootstrapPublicKey()
		return nil
	case key.Matches(msg, setupBootstrapTargetKey):
		v.nextBootstrapTarget()
		return nil
	case key.Matches(msg, setupBootstrapLaneKey):
		v.toggleBootstrapLane()
		return nil
	case key.Matches(msg, setupBootstrapRoleKey):
		v.nextBootstrapRole()
		return nil
	case key.Matches(msg, setupBootstrapDiscoverKey):
		for _, scope := range v.bootstrapScopes {
			if scope.Eligible {
				v.pending = "bootstrap discovery"
				return setupDiscoverCmd(v.client, scope.ScopeID)
			}
		}
		v.status = "no eligible backend-reported discovery scope is available"
		return nil
	case key.Matches(msg, setupBootstrapImportKey):
		v.beginInput(
			setupInputImportArtifact,
			"absolute local PAIR package path",
			false,
		)
		return textinput.Blink
	case key.Matches(msg, setupCableKey):
		return v.beginAccess("cable")
	case key.Matches(msg, setupTrustKey):
		v.toggleHostKeyConsent()
		return nil
	case key.Matches(msg, setupInspectKey):
		if len(v.accessibleSelection()) == 0 {
			v.status = "select one to four SSH-ready setup targets first"
			return nil
		}
		v.beginInput(setupInputArtifact, "artifact ID (blank = compatible default)", false)
		return textinput.Blink
	case key.Matches(msg, setupApproveKey):
		return v.approveCurrent()
	case key.Matches(msg, setupRetryKey):
		return v.retrySetup()
	case key.Matches(msg, setupCancelKey):
		return v.cancelCurrent()
	case key.Matches(msg, setupCableReviewKey):
		v.beginInput(setupInputCablePorts, "node/switch/port,node/switch/port[, ...]", false)
		return textinput.Blink
	case key.Matches(msg, setupCleanupKey):
		return v.reviewCleanup()
	case key.Matches(msg, setupFabricKey):
		return v.reviewFabric()
	case key.Matches(msg, setupRecoverKey):
		return v.recoverCurrent()
	}
	var cmd tea.Cmd
	v.table, cmd = v.table.Update(msg)
	return cmd
}

func (v *setupView) beginInput(mode setupInputMode, placeholder string, secret bool) {
	v.mode = mode
	v.input.SetValue("")
	v.input.Placeholder = placeholder
	if secret {
		v.input.EchoMode = textinput.EchoPassword
		v.input.EchoCharacter = '•'
	} else {
		v.input.EchoMode = textinput.EchoNormal
	}
	v.input.Focus()
}

func (v *setupView) cancelInput() {
	v.input.SetValue("")
	v.input.Blur()
	v.mode = setupInputNone
	v.draftAddress = ""
	v.accessDraft.clear()
	v.accessDraft = setupAccessRequest{}
}

func (v *setupView) submitInput() tea.Cmd {
	value := strings.TrimSpace(v.input.Value())
	switch v.mode {
	case setupInputAddress:
		if net.ParseIP(value) == nil {
			v.status = "enter a concrete IPv4 or IPv6 address"
			return nil
		}
		v.draftAddress = value
		v.beginInput(setupInputPort, "SSH port (blank = 22)", false)
		return textinput.Blink
	case setupInputPort:
		port := 22
		var err error
		if value != "" {
			port, err = strconv.Atoi(value)
		}
		if err != nil || port < 1 || port > 65535 {
			v.status = "SSH port must be 1-65535"
			return nil
		}
		address := v.draftAddress
		v.cancelInput()
		v.pending = "add target"
		return setupAddTargetCmd(v.client, address, port)
	case setupInputUsername:
		if !validSetupToken(value) {
			v.status = "enter a valid device account name"
			return nil
		}
		v.accessDraft.Username = value
		v.beginInput(setupInputAuth, "password or key", false)
		return textinput.Blink
	case setupInputAuth:
		switch strings.ToLower(value) {
		case "password", "p":
			v.accessDraft.Auth = "password"
			v.beginInput(setupInputCredential, "device account password", true)
		case "key", "existing-key", "k":
			v.accessDraft.Auth = "existing-key"
			v.beginInput(setupInputCredential, "absolute SSH key path (blank = ~/.ssh/id_ed25519)", false)
		default:
			v.status = "authentication must be password or key"
			return nil
		}
		return textinput.Blink
	case setupInputCredential:
		if v.accessDraft.Auth == "password" {
			if value == "" {
				v.status = "device account password is required"
				return nil
			}
			v.accessDraft.Password = transientSecret([]byte(v.input.Value()))
			v.input.SetValue("")
			v.beginInput(setupInputElevation, "administrator password (blank if not needed)", true)
		} else {
			v.accessDraft.KeyPath = value
			v.beginInput(setupInputPassphrase, "SSH key passphrase (blank if none)", true)
		}
		return textinput.Blink
	case setupInputPassphrase:
		v.accessDraft.Passphrase = transientSecret([]byte(v.input.Value()))
		v.input.SetValue("")
		v.beginInput(setupInputElevation, "administrator password (blank if not needed)", true)
		return textinput.Blink
	case setupInputElevation:
		v.accessDraft.ElevationPassword = transientSecret([]byte(v.input.Value()))
		v.input.SetValue("")
		if v.accessDraft.Purpose == "cable" {
			v.accessDraft.StartupLifetime = "session"
			return v.finishAccess()
		}
		v.beginInput(setupInputLifetime, "persistent or session (blank = persistent)", false)
		return textinput.Blink
	case setupInputLifetime:
		if value == "" {
			value = "persistent"
		}
		if value != "persistent" && value != "session" {
			v.status = "startup lifetime must be persistent or session"
			return nil
		}
		v.accessDraft.StartupLifetime = value
		return v.finishAccess()
	case setupInputArtifact:
		ids := v.accessibleSelection()
		keys := v.takeAcceptedKeys(ids)
		v.cancelInput()
		v.pending = "setup inspection"
		return setupInspectCmd(v.client, ids, value, keys)
	case setupInputImportArtifact:
		if value == "" {
			v.status = "an absolute local package path is required"
			return nil
		}
		v.cancelInput()
		v.pending = "artifact import"
		return setupImportArtifactCmd(v.client, value)
	case setupInputCablePorts:
		selection, err := parseCableSelection(value)
		if err != nil {
			v.status = err.Error()
			return nil
		}
		v.cancelInput()
		v.pendingSelection = selection
		v.pending = "cable review"
		return setupCableReviewCmd(v.client, selection, v.takeAcceptedKeys(v.selectedCandidateIDs()))
	}
	return nil
}

func (v *setupView) beginAccess(purpose string) tea.Cmd {
	ids := v.selectedCandidateIDs()
	if len(ids) == 0 {
		v.status = "select one to four targets before authorizing access"
		return nil
	}
	for _, candidate := range v.candidates {
		if v.selected[candidate.CandidateID] &&
			candidate.BootstrapState != "ssh-ready" {
			v.status = "bootstrap-required targets cannot use SSH enrollment controls"
			return nil
		}
	}
	v.accessDraft.clear()
	v.accessDraft = setupAccessRequest{Purpose: purpose, CandidateIDs: ids}
	if purpose == "" && v.operation != nil && v.operation.State != "running" && operationContainsAny(*v.operation, ids) {
		v.accessDraft.OperationID = v.operation.OperationID
	}
	v.beginInput(setupInputUsername, "device account username", false)
	return textinput.Blink
}

func (v *setupView) finishAccess() tea.Cmd {
	request := v.accessDraft
	v.accessDraft = setupAccessRequest{}
	v.mode = setupInputNone
	v.input.SetValue("")
	v.input.Blur()
	v.pending = "access authorization"
	v.status = "authorizing transient access; secret fields are masked and cleared after the RPC"
	return setupAccessCmd(v.client, request)
}

func (v *setupView) approveCurrent() tea.Cmd {
	switch v.approval {
	case setupApproveOnboarding:
		if v.review == nil || !v.review.CanApprove || v.review.ExpiresAt <= time.Now().UnixMilli() {
			v.status = "setup review is blocked or expired; inspect again"
			return nil
		}
		review := *v.review
		v.review, v.approval = nil, setupApproveNone
		v.pending, v.uncertainSetupID = "setup approval", review.ReviewID
		return setupApproveCmd(v.client, review)
	case setupApproveCable:
		if v.cableReview == nil || !v.cableReview.Available || v.cableReview.RemainingMs <= 0 {
			v.status = "cable review is blocked or expired; review again"
			return nil
		}
		review := *v.cableReview
		v.cableReview, v.approval = nil, setupApproveNone
		v.pending, v.uncertainCableID = "cable start", review.ReviewID
		return setupCableStartCmd(v.client, review)
	case setupApproveCleanup:
		if v.cleanupReview == nil || !v.cleanupReview.Available || v.cleanupReview.RemainingMs <= 0 {
			v.status = "cleanup review is blocked or expired; review again"
			return nil
		}
		review := *v.cleanupReview
		v.cleanupReview, v.approval = nil, setupApproveNone
		v.pending = "cleanup verification"
		return setupCleanupVerifyCmd(v.client, review)
	case setupApproveFabric:
		if v.fabricReview == nil || !v.fabricReview.Executable || v.fabricReview.RemainingMs <= 0 {
			v.status = "fabric review is blocked or expired; review again"
			return nil
		}
		review := *v.fabricReview
		v.fabricReview, v.approval = nil, setupApproveNone
		v.pending, v.uncertainOperation = "fabric approval", review.ReviewID
		return setupFabricApproveCmd(v.client, review)
	default:
		v.status = "no current approvable review"
		return nil
	}
}

func (v *setupView) retrySetup() tea.Cmd {
	if v.operation == nil || v.operation.OperationID == "" {
		v.status = "no retained setup operation to retry"
		return nil
	}
	canRetry := false
	for _, target := range v.operation.Targets {
		canRetry = canRetry || target.CanRetry
	}
	if !canRetry {
		v.status = "the retained setup operation has no retryable target"
		return nil
	}
	v.pending = "setup retry"
	return setupOperationCmd(v.client, "engine:onboarding-retry", "setup retry", v.operation.OperationID)
}

func (v *setupView) cancelCurrent() tea.Cmd {
	if v.fabricOperation != nil && !v.fabricOperation.CleanupConfirmed {
		if v.fabricOperation.State == "recovery-required" {
			v.status = "fabric requires explicit recovery; press R after refreshing access"
			return nil
		}
		v.pending = "fabric rollback"
		return setupFabricOperationCmd(v.client, "engine:fabric-cancel", "fabric rollback", v.fabricOperation.OperationID)
	}
	if v.cableRun != nil && !v.cableRun.CleanupConfirmed {
		cleanup := v.cableRun.CleanupRecovery != nil && (v.cableRun.CleanupRecovery.State == "verifying" || v.cableRun.CleanupRecovery.State == "release-pending")
		v.pending = "cable cancellation"
		return setupCableCancelCmd(v.client, v.cableRun.RunID, cleanup)
	}
	if v.operation != nil {
		for _, target := range v.operation.Targets {
			if target.CanCancel {
				v.pending = "setup cancellation"
				return setupOperationCmd(v.client, "engine:onboarding-cancel", "setup cancellation", v.operation.OperationID)
			}
		}
	}
	v.status = "no current setup, cable, or fabric effect can be cancelled"
	return nil
}

func (v *setupView) reviewCleanup() tea.Cmd {
	if v.cableRun == nil || v.cableRun.RunID == "" || v.cableRun.State != "failed" || v.cableRun.CleanupConfirmed {
		v.status = "cleanup review requires a failed cable run with unconfirmed cleanup"
		return nil
	}
	v.pending = "cleanup review"
	return setupCleanupReviewCmd(v.client, v.cableRun.RunID, v.takeAcceptedKeys(v.selectedCandidateIDs()))
}

func (v *setupView) reviewFabric() tea.Cmd {
	selection := v.cableSelection
	if len(selection.NodeIDs) == 0 && v.cableRun != nil {
		selection = selectionFromCableRun(*v.cableRun)
	}
	if len(selection.NodeIDs) < 2 {
		v.status = "review an exact cable selection before fabric setup"
		return nil
	}
	v.pending = "fabric review"
	return setupFabricReviewCmd(v.client, selection, v.takeAcceptedKeys(v.selectedCandidateIDs()))
}

func (v *setupView) recoverFabric() tea.Cmd {
	if v.fabricOperation == nil || v.fabricOperation.State != "recovery-required" {
		v.status = "the current fabric operation does not require recovery"
		return nil
	}
	v.pending = "fabric recovery"
	return setupFabricOperationCmd(v.client, "engine:fabric-recover", "fabric recovery", v.fabricOperation.OperationID)
}

func (v *setupView) recoverCurrent() tea.Cmd {
	if v.bootstrapPlan != nil && v.bootstrapReceipt == nil {
		reference, ok := v.bootstrapReference()
		if !ok {
			v.status = "bootstrap recovery requires current volatile access"
			return nil
		}
		v.pending = "bootstrap recovery"
		return setupBootstrapRecoverCmd(
			v.client,
			reference,
			v.bootstrapPlan.OperationID,
		)
	}
	return v.recoverFabric()
}

func (v *setupView) bootstrapAction() tea.Cmd {
	candidate, ok := v.selectedBootstrapCandidate()
	if !ok {
		v.status = "select exactly one device for the four-step setup journey"
		return nil
	}
	target, ok := v.bootstrapTarget(candidate)
	if !ok {
		v.status = "target platform and architecture are unavailable; press T to choose an override"
		return nil
	}
	entry, ok := v.bootstrapCatalogEntry(target)
	if !ok {
		v.status = "the signed bootstrap catalog has no exact target entry"
		return nil
	}
	if candidate.BootstrapState != "ssh-ready" {
		v.status = fmt.Sprintf(
			"prepare device with %s version=%s size=%d sha256=%s provenance=%s; run it, then refresh",
			entry.Bootstrap.FileName,
			entry.Bootstrap.Identity.Version,
			entry.Bootstrap.Size,
			entry.Bootstrap.Identity.SHA256,
			entry.Bootstrap.Provenance,
		)
		return nil
	}
	if !candidate.AccessAvailable {
		return v.beginAccess("")
	}
	reference, ok := v.bootstrapReference()
	if !ok {
		v.status = "current candidate access or SSH host fingerprint is unavailable"
		return nil
	}
	if v.bootstrapRequest == nil &&
		!candidate.HostKeyTrusted &&
		v.accepted[candidate.CandidateID] != candidate.HostKeySHA256 {
		v.status = "accept the exact displayed SSH host fingerprint with t"
		return nil
	}
	if len(v.bootstrapKeys.Keys) == 0 {
		v.status = "no controller public identity is available from the OS agent or SSH config"
		return nil
	}
	if v.bootstrapKeyIndex < 0 ||
		v.bootstrapKeyIndex >= len(v.bootstrapKeys.Keys) {
		v.bootstrapKeyIndex = 0
	}
	if v.bootstrapReceipt != nil {
		v.status = "bootstrap receipt is complete; refresh to continue ordinary setup state"
		return setupCandidatesCmd(v.client)
	}
	if v.bootstrapPlan != nil {
		if v.bootstrapPlan.Decision == hostbootstrap.DecisionRefuseForeign {
			v.status = "target owns foreign bootstrap resources; no apply was sent"
			return nil
		}
		if v.bootstrapStatus != nil &&
			(v.bootstrapStatus.Phase == hostbootstrap.PhaseApply ||
				v.bootstrapStatus.Phase == hostbootstrap.PhaseVerify) {
			v.pending = "bootstrap verification"
			return setupBootstrapVerifyCmd(
				v.client,
				reference,
				*v.bootstrapPlan,
			)
		}
		v.pending = "bootstrap apply"
		return setupBootstrapApplyCmd(
			v.client,
			reference,
			*v.bootstrapPlan,
		)
	}
	if v.bootstrapStatus != nil &&
		v.bootstrapStatus.Phase == hostbootstrap.PhaseInspect &&
		v.bootstrapRequest != nil {
		v.pending = "bootstrap review"
		return setupBootstrapReviewCmd(
			v.client,
			reference,
			*v.bootstrapRequest,
		)
	}
	account := v.accessAccounts[candidate.CandidateID]
	if account == "" {
		account = setupAuthorizedUsername(candidate.AccessLabel)
	}
	if account == "" {
		v.status = "authorize current device account access again before bootstrap inspection"
		return nil
	}
	operationID, err := newSetupBootstrapOperationID()
	if err != nil {
		v.status = "bootstrap operation identity is unavailable"
		return nil
	}
	runtimeOwner := hostbootstrap.RoleHeadless
	if v.bootstrapRole == hostbootstrap.RoleDesktop ||
		v.bootstrapRole == hostbootstrap.RoleHeadless {
		runtimeOwner = v.bootstrapRole
	} else if target.Platform != hostbootstrap.PlatformLinux {
		runtimeOwner = hostbootstrap.RoleDesktop
	}
	request := hostbootstrap.Request{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   operationID,
		Binding: hostbootstrap.Binding{
			Target:        target,
			Lane:          v.bootstrapLane,
			Role:          v.bootstrapRole,
			RuntimeOwner:  runtimeOwner,
			Account:       setupBootstrapAccount(target, account),
			ControllerKey: v.bootstrapKeys.Keys[v.bootstrapKeyIndex],
			Endpoint: hostbootstrap.SSHEndpoint{
				Address: candidate.Address,
				Port:    candidate.Port,
			},
			Product: entry.Product.Identity,
			Helper:  entry.Helper.Identity,
		},
	}
	if err := request.Validate(); err != nil {
		v.status = "canonical bootstrap request is invalid: " + err.Error()
		return nil
	}
	v.bootstrapCandidateID = candidate.CandidateID
	v.bootstrapRequest = &request
	v.bootstrapStatus = nil
	v.bootstrapPlan = nil
	v.bootstrapReceipt = nil
	delete(v.accepted, candidate.CandidateID)
	v.pending = "bootstrap inspection"
	return setupBootstrapInspectCmd(v.client, reference, request)
}

func (v *setupView) selectedBootstrapCandidate() (
	setupCandidate,
	bool,
) {
	var selected setupCandidate
	count := 0
	for _, candidate := range v.candidates {
		if v.selected[candidate.CandidateID] {
			selected = candidate
			count++
		}
	}
	return selected, count == 1
}

func (v *setupView) bootstrapReference() (
	setupBootstrapTargetReference,
	bool,
) {
	for _, candidate := range v.candidates {
		if candidate.CandidateID != v.bootstrapCandidateID &&
			(v.bootstrapCandidateID != "" ||
				!v.selected[candidate.CandidateID]) {
			continue
		}
		if candidate.AccessID == "" ||
			candidate.HostKeySHA256 == "" ||
			!candidate.AccessAvailable {
			return setupBootstrapTargetReference{}, false
		}
		return setupBootstrapTargetReference{
			CandidateID:   candidate.CandidateID,
			AccessID:      candidate.AccessID,
			HostKeySHA256: candidate.HostKeySHA256,
		}, true
	}
	return setupBootstrapTargetReference{}, false
}

func (v *setupView) bootstrapTarget(
	candidate setupCandidate,
) (hostbootstrap.Target, bool) {
	if target, ok := v.bootstrapTargetOverrides[candidate.CandidateID]; ok {
		return target, true
	}
	target := hostbootstrap.Target{
		Platform: hostbootstrap.Platform(candidate.BootstrapPlatform),
		Architecture: hostbootstrap.Architecture(
			candidate.BootstrapArchitecture,
		),
	}
	for _, supported := range hostbootstrap.SupportedTargets() {
		if target == supported {
			return target, true
		}
	}
	return hostbootstrap.Target{}, false
}

func (v *setupView) bootstrapCatalogEntry(
	target hostbootstrap.Target,
) (setupBootstrapCatalogTarget, bool) {
	for _, entry := range v.bootstrapCatalog.Targets {
		if entry.Target == target {
			return entry, true
		}
	}
	return setupBootstrapCatalogTarget{}, false
}

func (v *setupView) nextBootstrapPublicKey() {
	if len(v.bootstrapKeys.Keys) == 0 {
		v.status = "no controller public key is available"
		return
	}
	v.bootstrapKeyIndex = (v.bootstrapKeyIndex + 1) %
		len(v.bootstrapKeys.Keys)
	v.resetBootstrapState()
	v.status = "controller public fingerprint selected: " +
		v.bootstrapKeys.Keys[v.bootstrapKeyIndex].FingerprintSHA256
}

func (v *setupView) nextBootstrapTarget() {
	candidate, ok := v.selectedBootstrapCandidate()
	if !ok {
		v.status = "select exactly one target before choosing an override"
		return
	}
	targets := hostbootstrap.SupportedTargets()
	current, currentOK := v.bootstrapTarget(candidate)
	index := -1
	if currentOK {
		for position, target := range targets {
			if target == current {
				index = position
				break
			}
		}
	}
	next := targets[(index+1)%len(targets)]
	v.bootstrapTargetOverrides[candidate.CandidateID] = next
	v.resetBootstrapState()
	v.status = "target override selected: " +
		string(next.Platform) + "/" + string(next.Architecture)
}

func (v *setupView) toggleBootstrapLane() {
	if v.bootstrapLane == hostbootstrap.LaneQuickConnect {
		v.bootstrapLane = hostbootstrap.LaneZeroTouch
	} else {
		v.bootstrapLane = hostbootstrap.LaneQuickConnect
	}
	v.resetBootstrapState()
	v.status = "bootstrap lane: " + string(v.bootstrapLane)
}

func (v *setupView) nextBootstrapRole() {
	switch v.bootstrapRole {
	case hostbootstrap.RoleAuto:
		v.bootstrapRole = hostbootstrap.RoleDesktop
	case hostbootstrap.RoleDesktop:
		v.bootstrapRole = hostbootstrap.RoleHeadless
	default:
		v.bootstrapRole = hostbootstrap.RoleAuto
	}
	v.resetBootstrapState()
	v.status = "PAIR role: " + string(v.bootstrapRole)
}

func (v *setupView) resetBootstrapState() {
	v.bootstrapCandidateID = ""
	v.bootstrapRequest = nil
	v.bootstrapStatus = nil
	v.bootstrapPlan = nil
	v.bootstrapReceipt = nil
}

func setupBootstrapAccount(
	target hostbootstrap.Target,
	account string,
) hostbootstrap.AccountIdentity {
	switch target.Platform {
	case hostbootstrap.PlatformWindows:
		return hostbootstrap.AccountIdentity{
			Name:               account,
			HomePath:           `C:\Users\` + account,
			AuthorizedKeysPath: `C:\Users\` + account + `\.ssh\authorized_keys`,
		}
	case hostbootstrap.PlatformDarwin:
		return hostbootstrap.AccountIdentity{
			Name:               account,
			HomePath:           "/Users/" + account,
			AuthorizedKeysPath: "/Users/" + account + "/.ssh/authorized_keys",
		}
	default:
		return hostbootstrap.AccountIdentity{
			Name:               account,
			HomePath:           "/home/" + account,
			AuthorizedKeysPath: "/home/" + account + "/.ssh/authorized_keys",
		}
	}
}

func setupAuthorizedUsername(accessLabel string) string {
	username, method, found := strings.Cut(accessLabel, " (")
	if !found ||
		(method != "password)" && method != "existing-key)") ||
		!validSetupToken(username) {
		return ""
	}
	return username
}

func newSetupBootstrapOperationID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func (v *setupView) toggleSelected() {
	idx := v.table.Cursor()
	if idx < 0 || idx >= len(v.candidates) {
		return
	}
	id := v.candidates[idx].CandidateID
	if v.selected[id] {
		delete(v.selected, id)
	} else if len(v.selected) >= 4 {
		v.status = "at most four setup targets may be selected"
		return
	} else {
		v.selected[id] = true
	}
	v.resetBootstrapState()
	v.renderCandidates()
}

func (v *setupView) toggleHostKeyConsent() {
	idx := v.table.Cursor()
	if idx < 0 || idx >= len(v.candidates) {
		return
	}
	candidate := v.candidates[idx]
	if candidate.HostKeySHA256 == "" {
		v.status = "authorize access first so PAIR can observe the SSH fingerprint"
		return
	}
	if candidate.HostKeyTrusted {
		v.status = "this fingerprint is already trusted by the host SSH policy"
		return
	}
	if v.accepted[candidate.CandidateID] == candidate.HostKeySHA256 {
		delete(v.accepted, candidate.CandidateID)
		v.status = "transient fingerprint consent removed"
	} else {
		v.accepted[candidate.CandidateID] = candidate.HostKeySHA256
		v.status = "exact displayed fingerprint accepted for the next review only"
	}
	v.renderCandidates()
}

func (v *setupView) selectedCandidateIDs() []string {
	ids := make([]string, 0, len(v.selected))
	for _, candidate := range v.candidates {
		if v.selected[candidate.CandidateID] {
			ids = append(ids, candidate.CandidateID)
		}
	}
	return ids
}

func (v *setupView) accessibleSelection() []string {
	ids := []string{}
	for _, candidate := range v.candidates {
		if v.selected[candidate.CandidateID] &&
			candidate.BootstrapState == "ssh-ready" {
			ids = append(ids, candidate.CandidateID)
		}
	}
	return ids
}

func (v *setupView) acceptedKeys(ids []string) []setupAcceptedHostKey {
	keys := make([]setupAcceptedHostKey, 0, len(ids))
	for _, id := range ids {
		if sha := v.accepted[id]; sha != "" {
			keys = append(keys, setupAcceptedHostKey{CandidateID: id, SHA256: sha})
		}
	}
	return keys
}

func (v *setupView) takeAcceptedKeys(ids []string) []setupAcceptedHostKey {
	keys := v.acceptedKeys(ids)
	for _, key := range keys {
		delete(v.accepted, key.CandidateID)
	}
	v.renderCandidates()
	return keys
}

func (v *setupView) mergeCandidates(next []setupCandidate) {
	byID := make(map[string]setupCandidate, len(v.candidates)+len(next))
	for _, candidate := range v.candidates {
		byID[candidate.CandidateID] = candidate
	}
	for _, candidate := range next {
		byID[candidate.CandidateID] = candidate
	}
	v.candidates = v.candidates[:0]
	for _, candidate := range byID {
		v.candidates = append(v.candidates, candidate)
	}
	sort.Slice(v.candidates, func(i, j int) bool { return v.candidates[i].CandidateID < v.candidates[j].CandidateID })
}

func (v *setupView) mergeArtifact(next setupArtifact) {
	for index, artifact := range v.artifacts {
		if artifact.ArtifactID == next.ArtifactID {
			v.artifacts[index] = next
			return
		}
	}
	v.artifacts = append(v.artifacts, next)
}

func (v *setupView) reconcileCandidateChoices() {
	known := make(map[string]setupCandidate, len(v.candidates))
	for _, candidate := range v.candidates {
		known[candidate.CandidateID] = candidate
	}
	for id := range v.selected {
		if _, ok := known[id]; !ok {
			delete(v.selected, id)
		}
	}
	for id, sha := range v.accepted {
		candidate, ok := known[id]
		if !ok || candidate.HostKeySHA256 != sha || candidate.HostKeyTrusted {
			delete(v.accepted, id)
		}
	}
}

func (v *setupView) renderCandidates() {
	sort.SliceStable(v.candidates, func(left, right int) bool {
		leftLane := setupCandidateLane(v.candidates[left])
		rightLane := setupCandidateLane(v.candidates[right])
		if leftLane != rightLane {
			return leftLane == "ssh-ready"
		}
		return v.candidates[left].CandidateID <
			v.candidates[right].CandidateID
	})
	rows := make([]table.Row, 0, len(v.candidates))
	for _, candidate := range v.candidates {
		selected := "[ ]"
		if v.selected[candidate.CandidateID] {
			selected = "[x]"
		}
		access := "none"
		if candidate.AccessAvailable {
			access = candidate.AccessLabel
		}
		ssh := "unobserved"
		switch {
		case candidate.HostKeyTrusted:
			ssh = "trusted"
		case v.accepted[candidate.CandidateID] == candidate.HostKeySHA256 && candidate.HostKeySHA256 != "":
			ssh = "accepted once"
		case candidate.HostKeySHA256 != "":
			ssh = "review"
		}
		rows = append(rows, table.Row{
			selected,
			candidate.Label,
			net.JoinHostPort(
				candidate.Address,
				strconv.Itoa(candidate.Port),
			),
			setupCandidateLane(candidate),
			access,
			ssh,
		})
	}
	v.table.SetRows(rows)
}

func (v *setupView) View() string {
	var b strings.Builder
	if !v.loadedCandidates {
		b.WriteString(footerStyle.Render("Loading setup candidates…"))
	} else if len(v.candidates) == 0 {
		b.WriteString(footerStyle.Render("No setup candidates. Press a to add an exact device IP."))
	} else {
		partition := partitionSetupCandidates(v.candidates)
		fmt.Fprintf(
			&b,
			"Ready for SSH enrollment: %d · Observed/bootstrap required: %d\n",
			len(partition.SSHReady),
			len(partition.BootstrapRequired),
		)
		b.WriteString(v.table.View())
		idx := v.table.Cursor()
		if idx >= 0 && idx < len(v.candidates) {
			candidate := v.candidates[idx]
			b.WriteString("\n  candidate=" + candidate.CandidateID)
			if candidate.HostKeySHA256 != "" {
				b.WriteString("\n  ssh=" + candidate.HostKeySHA256)
			}
			if candidate.Reason != "" {
				b.WriteString("\n  " + footerStyle.Render(candidate.Reason))
			}
			b.WriteString("\n  lane=" + setupCandidateLane(candidate))
			if candidate.BootstrapPlatform != "" ||
				candidate.BootstrapArchitecture != "" {
				b.WriteString(
					" target=" + candidate.BootstrapPlatform +
						"/" + candidate.BootstrapArchitecture,
				)
			}
		}
	}
	if len(v.artifacts) > 0 {
		b.WriteString("\nArtifacts")
		for _, artifact := range v.artifacts {
			fmt.Fprintf(&b, "\n  %s · %s · %s/%s · sha256=%s", artifact.ArtifactID, artifact.Version, artifact.Platform, artifact.Arch, artifact.SHA256)
		}
	}
	v.renderBootstrap(&b)
	v.renderOnboarding(&b)
	v.renderCable(&b)
	v.renderFabric(&b)
	if label := v.approvalLabel(); label != "" {
		b.WriteString("\n" + statusOKStyle.Render("p approves: "+label))
	}
	if v.mode != setupInputNone {
		b.WriteString("\n" + v.inputPrompt() + ": " + v.input.View())
	}
	if v.pending != "" {
		b.WriteString("\n" + footerStyle.Render(v.pending+" in progress…"))
	}
	if v.status != "" {
		b.WriteString("\n" + footerStyle.Render(v.status))
	}
	return b.String()
}

func (v *setupView) approvalLabel() string {
	switch v.approval {
	case setupApproveOnboarding:
		return "the displayed onboarding review"
	case setupApproveCable:
		return "the displayed finite cable review"
	case setupApproveCleanup:
		return "the displayed cleanup-verification review"
	case setupApproveFabric:
		return "the displayed fabric-address review"
	}
	return ""
}

func (v *setupView) renderBootstrap(b *strings.Builder) {
	fmt.Fprintf(
		b,
		"\nDevice setup · 1 Prepare device · 2 Verify bootstrap · 3 Authorize key · 4 Install or update PAIR\n",
	)
	fmt.Fprintf(
		b,
		"  lane=%s role=%s catalog-targets=%d scopes=%d\n",
		v.bootstrapLane,
		v.bootstrapRole,
		len(v.bootstrapCatalog.Targets),
		len(v.bootstrapScopes),
	)
	candidate, selected := v.selectedBootstrapCandidate()
	if selected {
		target, targetOK := v.bootstrapTarget(candidate)
		if targetOK {
			fmt.Fprintf(
				b,
				"  target=%s/%s source=%s state=%s\n",
				target.Platform,
				target.Architecture,
				candidate.BootstrapSource,
				candidate.BootstrapState,
			)
			if entry, ok := v.bootstrapCatalogEntry(target); ok {
				fmt.Fprintf(
					b,
					"  copy/download=%s version=%s size=%d sha256=%s enterprise=%s\n",
					entry.Bootstrap.FileName,
					entry.Bootstrap.Identity.Version,
					entry.Bootstrap.Size,
					entry.Bootstrap.Identity.SHA256,
					entry.Bootstrap.Provenance,
				)
				fmt.Fprintf(
					b,
					"  fixed helper=%s@%s product=%s@%s\n",
					entry.Helper.Identity.ID,
					entry.Helper.Identity.Path,
					entry.Product.Identity.ID,
					entry.Product.Identity.Path,
				)
			}
		} else {
			b.WriteString("  target metadata unavailable; press T for an explicit override\n")
		}
		if candidate.HostKeySHA256 != "" {
			fmt.Fprintf(
				b,
				"  ssh-host-fingerprint=%s\n",
				candidate.HostKeySHA256,
			)
		}
	}
	if len(v.bootstrapKeys.Keys) > 0 {
		index := v.bootstrapKeyIndex
		if index < 0 || index >= len(v.bootstrapKeys.Keys) {
			index = 0
		}
		key := v.bootstrapKeys.Keys[index]
		fmt.Fprintf(
			b,
			"  controller-public-key=%s fingerprint=%s (%d available; P selects next)\n",
			key.Algorithm,
			key.FingerprintSHA256,
			len(v.bootstrapKeys.Keys),
		)
	} else {
		b.WriteString("  controller-public-key=unavailable\n")
	}
	if v.bootstrapStatus != nil {
		status := v.bootstrapStatus
		fmt.Fprintf(
			b,
			"  bootstrap status operation=%s phase=%s decision=%s\n",
			status.OperationID,
			status.Phase,
			status.Decision,
		)
		fmt.Fprintf(
			b,
			"    ssh=%s firewall=%s authorized-key=%s helper=%s product=%s\n",
			status.Observed.SSHService.Ownership,
			status.Observed.Firewall.Ownership,
			status.Observed.AuthorizedKey.Ownership,
			status.Observed.Helper.Ownership,
			status.Observed.Product.Ownership,
		)
		if status.Observed.Helper.Identity != nil {
			fmt.Fprintf(
				b,
				"    helper-result=%s@%s sha256=%s\n",
				status.Observed.Helper.Identity.ID,
				status.Observed.Helper.Identity.Path,
				status.Observed.Helper.Identity.SHA256,
			)
		}
	}
	if v.bootstrapPlan != nil {
		fmt.Fprintf(
			b,
			"  bootstrap plan operation=%s decision=%s actions=%s\n",
			v.bootstrapPlan.OperationID,
			v.bootstrapPlan.Decision,
			bootstrapResourceList(v.bootstrapPlan.Actions),
		)
	}
	if v.bootstrapReceipt != nil {
		receipt := v.bootstrapReceipt
		fmt.Fprintf(
			b,
			"  bootstrap receipt operation=%s phase=%s decision=%s owner=%s\n",
			receipt.OperationID,
			receipt.Phase,
			receipt.Decision,
			receipt.Binding.RuntimeOwner,
		)
		if receipt.Verified.Helper.Identity != nil {
			fmt.Fprintf(
				b,
				"    verified-helper=%s@%s sha256=%s\n",
				receipt.Verified.Helper.Identity.ID,
				receipt.Verified.Helper.Identity.Path,
				receipt.Verified.Helper.Identity.SHA256,
			)
		}
	}
}

func bootstrapResourceList(actions []hostbootstrap.ResourceKind) string {
	if len(actions) == 0 {
		return "none"
	}
	values := make([]string, 0, len(actions))
	for _, action := range actions {
		values = append(values, string(action))
	}
	return strings.Join(values, ",")
}

func (v *setupView) renderOnboarding(b *strings.Builder) {
	if v.review != nil {
		fmt.Fprintf(b, "\nSetup review %s · controller=%s · cluster=%s · approve=%s\n", v.review.ReviewID, v.review.ControllerNodeID, v.review.TargetClusterID, yesNo(v.review.CanApprove))
		for _, target := range v.review.Targets {
			fmt.Fprintf(b, "  %s -> %s %s/%s · %s · lifetime=%s\n", target.CandidateID, target.Hostname, target.Platform, target.Arch, target.Status, target.StartupLifetime)
			if target.Reason != "" {
				fmt.Fprintf(b, "    %s\n", target.Reason)
			}
			if target.Artifact != nil {
				fmt.Fprintf(b, "    artifact=%s sha256=%s\n", target.Artifact.ArtifactID, target.Artifact.SHA256)
			}
		}
	}
	if v.operation != nil {
		fmt.Fprintf(b, "\nSetup operation %s · review=%s · revision=%d · %s\n", v.operation.OperationID, v.operation.ReviewID, v.operation.Revision, v.operation.State)
		for _, target := range v.operation.Targets {
			fmt.Fprintf(b, "  %s -> %s · %s · cleanup=%s", target.CandidateID, target.NodeID, target.Stage, yesNo(target.CleanupConfirmed))
			if target.Message != "" {
				fmt.Fprintf(b, " · %s", target.Message)
			}
			b.WriteByte('\n')
		}
	}
}

func (v *setupView) renderCable(b *strings.Builder) {
	if v.loadedRetained {
		fmt.Fprintf(b, "\nCable hold=%s · limited=%s · owner=%s", yesNo(v.retained.Held), yesNo(v.retained.Limited), v.retained.OwnerNodeID)
		if v.retained.Reason != "" {
			fmt.Fprintf(b, " · %s", v.retained.Reason)
		}
		b.WriteByte('\n')
		for _, run := range v.retained.Runs {
			fmt.Fprintf(b, "  retained=%s · review=%s · %s · cleanup=%s\n", run.RunID, run.ReviewID, run.State, yesNo(run.CleanupConfirmed))
		}
	}
	if v.cableReview != nil {
		fmt.Fprintf(b, "Cable review %s · owner=%s · available=%s · remaining=%dms\n", v.cableReview.ReviewID, v.cableReview.OwnerNodeID, yesNo(v.cableReview.Available), v.cableReview.RemainingMs)
		if v.cableReview.Reason != "" {
			fmt.Fprintf(b, "  %s\n", v.cableReview.Reason)
		}
		renderCableTargets(b, v.cableReview.Targets)
		renderSetupPermission(b, v.cableReview.Permission)
	}
	if v.cleanupReview != nil {
		fmt.Fprintf(b, "Cleanup review %s · run=%s · available=%s · remaining=%dms\n", v.cleanupReview.ReviewID, v.cleanupReview.RunID, yesNo(v.cleanupReview.Available), v.cleanupReview.RemainingMs)
		for _, target := range v.cleanupReview.Targets {
			fmt.Fprintf(b, "  %s candidate=%s access=%s elevation=%s inspector=%s\n", target.NodeID, target.CandidateID, yesNo(target.AccessAvailable), yesNo(target.ElevationAvailable), yesNo(target.InspectorAvailable))
		}
		for _, effect := range v.cleanupReview.Effects {
			fmt.Fprintf(b, "  effect: %s\n", effect)
		}
	}
	if v.cableRun != nil {
		run := v.cableRun
		fmt.Fprintf(b, "Cable run %s · review=%s · revision=%d · %s/%s · directness=%s · cleanup=%s\n", run.RunID, run.ReviewID, run.Revision, run.State, run.Result, run.Directness, yesNo(run.CleanupConfirmed))
		renderCableTargets(b, run.Targets)
		for _, edge := range run.Edges {
			fmt.Fprintf(b, "  edge %s/%s/%s <-> %s/%s/%s · age=%dms fresh=%s lease=%dms\n", edge.Left.NodeID, edge.Left.SwitchID, edge.Left.PortName, edge.Right.NodeID, edge.Right.SwitchID, edge.Right.PortName, edge.AgeMs, yesNo(edge.Fresh), run.FreshnessRemainingMs)
		}
		if run.CleanupRecovery != nil {
			fmt.Fprintf(b, "  cleanup attempt=%s review=%s · %s/%s · holdReleased=%s\n", run.CleanupRecovery.AttemptID, run.CleanupRecovery.ReviewID, run.CleanupRecovery.State, run.CleanupRecovery.Code, yesNo(run.CleanupRecovery.HoldReleased))
		}
		if run.Message != "" {
			fmt.Fprintf(b, "  %s\n", run.Message)
		}
	}
}

func renderCableTargets(b *strings.Builder, targets []cableprobe.Target) {
	for _, target := range targets {
		fmt.Fprintf(b, "  %s principal=%s raw=%s", target.NodeID, target.Principal, target.RawPrivilege)
		if target.Reason != "" {
			fmt.Fprintf(b, " · %s", target.Reason)
		}
		b.WriteByte('\n')
		for _, port := range target.Ports {
			fmt.Fprintf(b, "    %s/%s", port.SwitchID, port.PortName)
			for _, iface := range port.Interfaces {
				fmt.Fprintf(b, " %s#%d@%s", iface.Name, iface.Index, iface.MAC)
			}
			b.WriteByte('\n')
		}
	}
}

func (v *setupView) renderFabric(b *strings.Builder) {
	if v.fabricReview != nil {
		fmt.Fprintf(b, "\nFabric review %s · owner=%s · recipe=%s · cable=%s · executable=%s · remaining=%dms\n", v.fabricReview.ReviewID, v.fabricReview.OwnerNodeID, v.fabricReview.RecipeID, v.fabricReview.CableRunID, yesNo(v.fabricReview.Executable), v.fabricReview.RemainingMs)
		renderFabricTargets(b, v.fabricReview.Targets)
		renderSetupPermission(b, v.fabricReview.Permission)
		for _, blocker := range v.fabricReview.Blockers {
			fmt.Fprintf(b, "  blocker: %s\n", blocker)
		}
	}
	if v.fabricOperation != nil {
		op := v.fabricOperation
		fmt.Fprintf(b, "\nFabric operation %s · review=%s · recipe=%s · cable=%s · %s · effects=%s/unconfirmed=%s · cleanup=%s\n", op.OperationID, op.ReviewID, op.RecipeID, op.CableRunID, op.State, yesNo(op.EffectsApplied), yesNo(op.EffectsUnconfirmed), yesNo(op.CleanupConfirmed))
		renderFabricTargets(b, op.Targets)
		renderSetupPermission(b, op.Permission)
		for _, binding := range op.CandidateIPs {
			if binding.Gateway != "" {
				fmt.Fprintf(b, "  routed %s(%s) -> %s(%s) via gateway %s on %s#%d/%s/%s principal=%s\n", binding.NodeID, binding.Address, binding.PeerNodeID, binding.PeerAddress, binding.Gateway, binding.InterfaceName, binding.InterfaceIndex, binding.SwitchID, binding.PortName, binding.PeerPrincipal)
				continue
			}
			fmt.Fprintf(b, "  route %s(%s) -> %s(%s) via %s#%d/%s/%s rdma=%s:%d gid=%d/%s principal=%s\n", binding.NodeID, binding.Address, binding.PeerNodeID, binding.PeerAddress, binding.InterfaceName, binding.InterfaceIndex, binding.SwitchID, binding.PortName, binding.RDMADevice, binding.RDMAPort, binding.GIDIndex, binding.GIDType, binding.PeerPrincipal)
		}
		if op.Message != "" {
			fmt.Fprintf(b, "  %s\n", op.Message)
		}
	}
}

func renderSetupPermission(b *strings.Builder, permission *cableprobe.PermissionReview) {
	if permission == nil {
		return
	}
	fmt.Fprintf(b, "  permission mode=%s\n", permission.Mode)
	for _, effect := range permission.Effects {
		fmt.Fprintf(b, "    effect: %s\n", effect)
	}
}

func renderFabricTargets(b *strings.Builder, targets []setupFabricTarget) {
	for _, target := range targets {
		fmt.Fprintf(b, "  %s principal=%s", target.NodeID, target.Principal)
		if target.SwitchID != "" {
			fmt.Fprintf(b, " port=%s/%s", target.SwitchID, target.PortName)
		}
		if target.AdvertisedAddress != "" {
			fmt.Fprintf(b, " advertised=%s", target.AdvertisedAddress)
		}
		b.WriteByte('\n')
		for _, iface := range target.Interfaces {
			fmt.Fprintf(b, "    %s#%d@%s physical=%s/%s address=%s mtu=%d rdma=%s", iface.Name, iface.Index, iface.MAC, iface.PhysicalPort.SwitchID, iface.PhysicalPort.PortName, iface.Address, iface.MTU, strings.Join(iface.RDMADevices, ","))
			for _, route := range iface.Routes {
				fmt.Fprintf(b, " route=%s via %s", route.Destination, route.Gateway)
			}
			b.WriteByte('\n')
		}
	}
}

func (v *setupView) inputPrompt() string {
	switch v.mode {
	case setupInputAddress:
		return "device address"
	case setupInputPort:
		return "SSH port"
	case setupInputUsername:
		return "account"
	case setupInputAuth:
		return "authentication"
	case setupInputCredential:
		if v.accessDraft.Auth == "password" {
			return "password"
		}
		return "key path"
	case setupInputPassphrase:
		return "key passphrase"
	case setupInputElevation:
		return "administrator password"
	case setupInputLifetime:
		return "startup lifetime"
	case setupInputArtifact:
		return "artifact"
	case setupInputImportArtifact:
		return "local package"
	case setupInputCablePorts:
		return "physical ports"
	}
	return "input"
}

func (v *setupView) Help() []key.Binding {
	return []key.Binding{
		setupRefreshKey, setupSelectKey, setupAddKey, setupAccessKey,
		setupBootstrapKey, setupBootstrapPublicKey, setupBootstrapTargetKey,
		setupBootstrapLaneKey, setupBootstrapRoleKey,
		setupBootstrapDiscoverKey, setupBootstrapImportKey, setupCableKey,
		setupTrustKey, setupInspectKey, setupApproveKey, setupRetryKey, setupCancelKey,
		setupCableReviewKey, setupCleanupKey, setupFabricKey, setupRecoverKey,
	}
}

func validSetupToken(value string) bool {
	if value == "" || len(value) > 96 {
		return false
	}
	for i, r := range value {
		if i == 0 && !unicode.IsLetter(r) && !unicode.IsDigit(r) || i > 0 && !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("_.-", r) {
			return false
		}
	}
	return true
}

func parseCableSelection(value string) (cableprobe.ReviewRequest, error) {
	selection := cableprobe.ReviewRequest{NodeIDs: []string{}, Ports: []cableprobe.PortRef{}}
	nodes, ports := map[string]int{}, map[cableprobe.PortRef]bool{}
	for _, raw := range strings.Split(value, ",") {
		parts := strings.Split(strings.TrimSpace(raw), "/")
		if len(parts) != 3 {
			return selection, fmt.Errorf("use node/switch/port for each physical port")
		}
		ref := cableprobe.PortRef{NodeID: strings.TrimSpace(parts[0]), SwitchID: strings.TrimSpace(parts[1]), PortName: strings.TrimSpace(parts[2])}
		if !validSetupIdentifier(ref.NodeID, 128) || !validSetupIdentifier(ref.SwitchID, 128) || !validSetupIdentifier(ref.PortName, 128) || ports[ref] {
			return selection, fmt.Errorf("physical port bindings must be distinct, non-empty, and at most 128 characters")
		}
		if _, ok := nodes[ref.NodeID]; !ok {
			selection.NodeIDs = append(selection.NodeIDs, ref.NodeID)
		}
		nodes[ref.NodeID]++
		ports[ref] = true
		selection.Ports = append(selection.Ports, ref)
	}
	if len(nodes) < 2 || len(nodes) > 3 {
		return selection, fmt.Errorf("select two or three distinct nodes")
	}
	for _, count := range nodes {
		if count < 1 || count > 2 {
			return selection, fmt.Errorf("select one or two physical ports per node")
		}
	}
	return selection, nil
}

func validSetupIdentifier(value string, limit int) bool {
	return value != "" && len(value) <= limit && strings.TrimSpace(value) == value && !strings.ContainsFunc(value, func(r rune) bool { return r < 32 || r == 127 })
}

func validateSetupReview(review setupReview, selected []string) error {
	if review.ReviewID == "" || review.ControllerNodeID == "" || review.ExpiresAt <= 0 || len(review.Targets) != len(selected) || len(selected) < 1 || len(selected) > 4 {
		return fmt.Errorf("review identity or target count is incomplete")
	}
	want := map[string]bool{}
	for _, id := range selected {
		want[id] = true
	}
	for _, target := range review.Targets {
		if !want[target.CandidateID] {
			return fmt.Errorf("review is not bound to the selected targets")
		}
		delete(want, target.CandidateID)
	}
	if len(want) != 0 {
		return fmt.Errorf("review omitted a selected target")
	}
	return nil
}

func validateCableReview(review cableprobe.Review, selection cableprobe.ReviewRequest) error {
	if review.ReviewID == "" || review.OwnerNodeID == "" || len(review.Targets) != len(selection.NodeIDs) || review.RemainingMs < 0 {
		return fmt.Errorf("review identity or target count is incomplete")
	}
	wantNodes := map[string]bool{}
	wantPorts := map[cableprobe.PortRef]bool{}
	for _, node := range selection.NodeIDs {
		wantNodes[node] = true
	}
	for _, port := range selection.Ports {
		wantPorts[port] = true
	}
	seenPorts := 0
	for _, target := range review.Targets {
		if !wantNodes[target.NodeID] {
			return fmt.Errorf("review is not bound to the selected nodes")
		}
		delete(wantNodes, target.NodeID)
		for _, port := range target.Ports {
			ref := cableprobe.PortRef{NodeID: target.NodeID, SwitchID: port.SwitchID, PortName: port.PortName}
			if !wantPorts[ref] {
				return fmt.Errorf("review returned an unselected physical port")
			}
			seenPorts++
		}
	}
	if len(wantNodes) != 0 || review.Available && seenPorts != len(wantPorts) {
		return fmt.Errorf("review omitted selected bindings")
	}
	return nil
}

func validateFabricReview(review setupFabricReview, selection cableprobe.ReviewRequest) error {
	if review.SchemaVersion != 1 || review.ReviewID == "" || review.OwnerNodeID == "" || review.RecipeID == "" || review.RemainingMs < 0 {
		return fmt.Errorf("review identity is incomplete")
	}
	want := map[string]bool{}
	for _, node := range selection.NodeIDs {
		want[node] = true
	}
	seen := map[string]bool{}
	for _, target := range review.Targets {
		if !want[target.NodeID] || seen[target.NodeID] || target.Principal == "" {
			return fmt.Errorf("review returned an unselected or duplicate participant")
		}
		seen[target.NodeID] = true
	}
	if review.Executable && len(seen) != len(want) {
		return fmt.Errorf("executable review omitted selected participants")
	}
	return nil
}

func cloneCableSelection(selection cableprobe.ReviewRequest) cableprobe.ReviewRequest {
	return cableprobe.ReviewRequest{NodeIDs: append([]string(nil), selection.NodeIDs...), Ports: append([]cableprobe.PortRef(nil), selection.Ports...)}
}

func selectionFromCableRun(run cableprobe.Run) cableprobe.ReviewRequest {
	selection := cableprobe.ReviewRequest{NodeIDs: []string{}, Ports: []cableprobe.PortRef{}}
	for _, target := range run.Targets {
		selection.NodeIDs = append(selection.NodeIDs, target.NodeID)
		for _, port := range target.Ports {
			selection.Ports = append(selection.Ports, cableprobe.PortRef{NodeID: target.NodeID, SwitchID: port.SwitchID, PortName: port.PortName})
		}
	}
	return selection
}

func operationContainsAny(operation setupOperation, ids []string) bool {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	for _, target := range operation.Targets {
		if want[target.CandidateID] && target.Stage != "paired" {
			return true
		}
	}
	return false
}
