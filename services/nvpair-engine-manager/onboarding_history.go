// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	onboardingHistoryDir      = "onboarding-operations"
	onboardingHistoryMaxFiles = 1024
	onboardingHistoryMaxBytes = 256 << 10
)

var onboardingHistoryID = regexp.MustCompile(`^[a-f0-9]{32}$`)

// OnboardingHistorySummary is deliberately read-only. It reports retained
// journals without creating retry, access, approval, cleanup, or runtime
// authority. Effectful onboarding remains unsupported on this generation.
type OnboardingHistorySummary struct {
	Total                      int                          `json:"total"`
	HistoryOnly                int                          `json:"history_only"`
	Current                    int                          `json:"current"`
	Invalid                    int                          `json:"invalid"`
	RecoveryRequired           bool                         `json:"recovery_required"`
	DiagnosticRecoveryRequired bool                         `json:"diagnostic_recovery_required"`
	DiscoveryBlocked           bool                         `json:"discovery_blocked"`
	MutationSupported          bool                         `json:"mutation_supported"`
	Operations                 []OnboardingHistoryOperation `json:"operations"`
}

type OnboardingHistoryOperation struct {
	OperationID     string `json:"operation_id"`
	State           string `json:"state"`
	Classification  string `json:"classification"`
	TargetCount     int    `json:"target_count"`
	FinishedAt      int64  `json:"finished_at,omitempty"`
	MutationAllowed bool   `json:"mutation_allowed"`
}

type onboardingHistoryRun struct {
	Operation        historyOperation       `json:"operation"`
	ControllerNodeID string                 `json:"controllerNodeId"`
	Deadline         int64                  `json:"deadline"`
	Plans            map[string]historyPlan `json:"plans"`
}

func (e *Executor) OnboardingHistory() OnboardingHistorySummary {
	return loadOnboardingHistory(e.baseDir)
}

func loadOnboardingHistory(baseDir string) OnboardingHistorySummary {
	summary := OnboardingHistorySummary{Operations: []OnboardingHistoryOperation{}}
	dirPath := filepath.Join(baseDir, onboardingHistoryDir)
	dir, err := os.Open(dirPath)
	if os.IsNotExist(err) {
		return summary
	}
	if err != nil {
		return failedOnboardingHistory()
	}
	defer dir.Close()
	entries := []os.DirEntry{}
	for {
		batch, readErr := dir.ReadDir(64)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return failedOnboardingHistory()
		}
		for _, entry := range batch {
			if len(entries) >= onboardingHistoryMaxFiles {
				return failedOnboardingHistory()
			}
			entries = append(entries, entry)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		name := entry.Name()
		id := strings.TrimSuffix(name, ".json")
		if entry.IsDir() || !onboardingHistoryID.MatchString(id) {
			continue
		}
		summary.Total++
		op := OnboardingHistoryOperation{OperationID: id, Classification: "invalid", MutationAllowed: false}
		data, readErr := readOnboardingHistoryFile(filepath.Join(dirPath, id+".json"))
		var run onboardingHistoryRun
		if readErr != nil || json.Unmarshal(data, &run) != nil {
			summary.Invalid++
			summary.Operations = append(summary.Operations, op)
			continue
		}
		op.State, op.TargetCount, op.FinishedAt = run.Operation.State, len(run.Operation.Targets), run.Operation.FinishedAt
		classification, valid := classifyOnboardingHistoryRun(id, run)
		if !valid {
			summary.Invalid++
		} else {
			op.Classification = classification
			if classification == "history-only" {
				summary.HistoryOnly++
			} else {
				summary.Current++
			}
		}
		summary.Operations = append(summary.Operations, op)
	}
	summary.RecoveryRequired = summary.Invalid > 0
	summary.DiscoveryBlocked = summary.Invalid > 0
	if summary.validate() != nil {
		return failedOnboardingHistory()
	}
	return summary
}

func failedOnboardingHistory() OnboardingHistorySummary {
	return OnboardingHistorySummary{Total: 1, Invalid: 1, RecoveryRequired: true, DiscoveryBlocked: true, Operations: []OnboardingHistoryOperation{}}
}

func readOnboardingHistoryFile(file string) ([]byte, error) {
	return readOnboardingHistoryFileAfterStat(file, nil)
}

func readOnboardingHistoryFileAfterStat(file string, afterStat func()) ([]byte, error) {
	before, err := os.Lstat(file)
	if err != nil || !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > onboardingHistoryMaxBytes {
		return nil, errors.New("onboarding history file is not an available bounded regular file")
	}
	if afterStat != nil {
		afterStat()
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, errors.New("onboarding history file is unavailable")
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Size() > onboardingHistoryMaxBytes {
		return nil, errors.New("onboarding history file changed before reading")
	}
	data, err := io.ReadAll(io.LimitReader(f, onboardingHistoryMaxBytes+1))
	if err != nil || int64(len(data)) > onboardingHistoryMaxBytes {
		return nil, errors.New("onboarding history file exceeds its read bound")
	}
	return data, nil
}

func classifyOnboardingHistoryRun(id string, run onboardingHistoryRun) (string, bool) {
	op := run.Operation
	if op.OperationID != id || op.Revision < 1 || len(op.Targets) < 1 || len(op.Targets) > 4 || len(run.Plans) != len(op.Targets) {
		return "invalid", false
	}
	anyHistory := false
	for _, target := range op.Targets {
		plan, ok := run.Plans[target.CandidateID]
		if !ok || plan.Candidate.CandidateID != target.CandidateID || plan.Candidate.Address == "" || plan.Info.UID <= 0 || !validHistoryInvitation(plan) {
			return "invalid", false
		}
		currentPlan := validHistoryCurrentPlan(plan)
		history := !currentPlan && validHistoryLegacyPlan(op, target, plan)
		current := currentPlan && currentTerminalHistoryShape(op, target, plan)
		if !current && !history {
			return "invalid", false
		}
		anyHistory = anyHistory || history
	}
	if anyHistory {
		return "history-only", true
	}
	return "current", true
}

func currentTerminalHistoryShape(operation historyOperation, target historyTargetState, plan historyPlan) bool {
	if operation.FinishedAt <= 0 || !target.CleanupConfirmed || !plan.Receipt.CleanupConfirmed {
		return false
	}
	terminal := operation.State == "completed" && target.Stage == "paired" && !target.CanRetry && !target.CanCancel || operation.State == "cancelled" && target.Stage == "cancelled"
	if !terminal {
		return false
	}
	if plan.Review.Action == "" || plan.Review.Action == "install" {
		return true
	}
	if plan.Review.Action != "upgrade" {
		return false
	}
	completed := operation.State == "completed" && plan.UpgradePhase == "retired" && plan.Receipt.Installed && plan.Receipt.ServiceInstalled && plan.Receipt.ServiceStarted
	cancelledBeforeStop := operation.State == "cancelled" && plan.UpgradePhase == "cancelled-before-stop" && !plan.Receipt.Installed && !plan.Receipt.ServiceInstalled && !plan.Receipt.ServiceStarted
	cancelledAfterRollback := operation.State == "cancelled" && plan.UpgradePhase == "rolled-back" &&
		plan.ExistingInstallation != nil && plan.Receipt.Installed && plan.Receipt.Recoverable &&
		!plan.Receipt.ServiceInstalled && !plan.Receipt.ServiceStarted &&
		target.NodeID == plan.ExistingInstallation.NodeID &&
		plan.Receipt.NodeID == plan.ExistingInstallation.NodeID &&
		plan.Receipt.ArtifactSHA256 == plan.Artifact.SHA256
	return completed || cancelledBeforeStop || cancelledAfterRollback
}

func (s OnboardingHistorySummary) validate() error {
	if s.Total != s.HistoryOnly+s.Current+s.Invalid {
		return fmt.Errorf("onboarding history counts do not reconcile")
	}
	return nil
}
