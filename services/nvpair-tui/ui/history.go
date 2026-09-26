// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"

	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

type onboardingHistorySummary struct {
	Total             int                          `json:"total"`
	HistoryOnly       int                          `json:"history_only"`
	Current           int                          `json:"current"`
	Invalid           int                          `json:"invalid"`
	RecoveryRequired  bool                         `json:"recovery_required"`
	DiscoveryBlocked  bool                         `json:"discovery_blocked"`
	MutationSupported bool                         `json:"mutation_supported"`
	Operations        []onboardingHistoryOperation `json:"operations"`
}

type onboardingHistoryOperation struct {
	OperationID     string `json:"operation_id"`
	State           string `json:"state"`
	Classification  string `json:"classification"`
	TargetCount     int    `json:"target_count"`
	MutationAllowed bool   `json:"mutation_allowed"`
}

type onboardingHistoryMsg struct {
	summary onboardingHistorySummary
	err     error
}

type onboardingHistoryView struct {
	client        *rpc.Client
	table         table.Model
	summary       onboardingHistorySummary
	status        string
	failed        bool
	loaded        bool
	width, height int
}

func newOnboardingHistoryView(client *rpc.Client) *onboardingHistoryView {
	return &onboardingHistoryView{client: client, table: newTable(nil)}
}

func (v *onboardingHistoryView) Title() string { return "Setup history" }

func (v *onboardingHistoryView) Init() tea.Cmd {
	return call(v.client, "engine:onboarding-history", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return onboardingHistoryMsg{err: err}
		}
		var summary onboardingHistorySummary
		if err := decodeParams(msg.Result, &summary); err != nil {
			return onboardingHistoryMsg{err: err}
		}
		return onboardingHistoryMsg{summary: summary}
	})
}

func (v *onboardingHistoryView) SetSize(w, h int) {
	v.width, v.height = w, h
	v.table.SetWidth(w)
	v.table.SetHeight(clampWidth(h-3, 1))
	v.table.SetColumns([]table.Column{
		{Title: "OPERATION", Width: 14},
		{Title: "STATE", Width: 12},
		{Title: "CLASS", Width: 14},
		{Title: "NODES", Width: 7},
		{Title: "AUTHORITY", Width: 12},
	})
}

func (v *onboardingHistoryView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case onboardingHistoryMsg:
		v.loaded = true
		if msg.err != nil {
			v.failed = true
			v.status = "history unavailable: " + msg.err.Error()
			return nil
		}
		v.failed = false
		v.summary = msg.summary
		rows := make([]table.Row, 0, len(msg.summary.Operations))
		for _, operation := range msg.summary.Operations {
			authority := "read-only"
			if operation.MutationAllowed {
				authority = "invalid"
			}
			rows = append(rows, table.Row{truncate(operation.OperationID, 14), operation.State, operation.Classification, fmt.Sprint(operation.TargetCount), authority})
		}
		v.table.SetRows(rows)
		v.status = fmt.Sprintf("%d records · %d history-only · %d current · %d invalid", msg.summary.Total, msg.summary.HistoryOnly, msg.summary.Current, msg.summary.Invalid)
		if msg.summary.RecoveryRequired {
			v.status += " · recovery required"
		}
		return nil
	case tea.KeyMsg:
		var cmd tea.Cmd
		v.table, cmd = v.table.Update(msg)
		return cmd
	}
	return nil
}

func (v *onboardingHistoryView) View() string {
	if !v.loaded {
		return footerStyle.Render("Loading setup history…")
	}
	if v.failed {
		return statusErrStyle.Render("Setup history unavailable.") + "\n" + footerStyle.Render(v.status)
	}
	if len(v.summary.Operations) == 0 && v.summary.RecoveryRequired {
		return statusErrStyle.Render("Setup history inventory failed; recovery required.") + "\n" + footerStyle.Render(v.status)
	}
	if len(v.summary.Operations) == 0 {
		return statusOKStyle.Render("No retained setup history.") + "\n" + footerStyle.Render(v.status)
	}
	return v.table.View() + "\n" + footerStyle.Render(v.status+" · history never grants setup authority")
}

func (v *onboardingHistoryView) Help() []key.Binding { return nil }
