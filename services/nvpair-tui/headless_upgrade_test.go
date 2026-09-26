// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type upgradeFixture struct {
	unit        string
	executable  string
	plan        headlessUpgradePlan
	effects     []string
	fail        string
	stopPending bool
	foreign     bool
}

func (f *upgradeFixture) readUnit() (string, error) { return f.unit, nil }
func (f *upgradeFixture) state(_ context.Context, executable string) (string, error) {
	if f.foreign {
		return "", errors.New("different loaded fragment, drop-in or executable")
	}
	if f.executable == "" {
		return "stopped", nil
	}
	if f.executable != executable {
		return "", errors.New("different executable")
	}
	return "running", nil
}
func (f *upgradeFixture) replaceUnit(expected, body string) error {
	if f.fail == "changed-unit" {
		f.unit = "foreign edit"
	}
	if f.unit != expected {
		return errors.New("changed before exchange")
	}
	f.effects = append(f.effects, "replace-unit")
	f.unit = body
	return nil
}
func (f *upgradeFixture) systemctl(_ context.Context, action string) error {
	f.effects = append(f.effects, action)
	if f.fail == action {
		return context.DeadlineExceeded
	}
	if action == "stop" && !f.stopPending {
		f.executable = ""
	}
	if action == "start" {
		f.executable = f.plan.request.OldExecutable
	}
	return nil
}

func reviewedUpgrade(t *testing.T, phase string) headlessUpgradePlan {
	t.Helper()
	old, err := headlessUnitTextWithStop("/home/operator/old/resources/cli-bin/nvpair-tui", "/home/operator/old/resources/cli-bin/nvpair-ui-broker", "/home/operator/.config", 25)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(old))
	request := headlessUpgradeRequest{Phase: phase, ExpectedUnitSHA256: hex.EncodeToString(digest[:]), OldExecutable: "/home/operator/old/resources/cli-bin/nvpair-tui", OldBroker: "/home/operator/old/resources/cli-bin/nvpair-ui-broker", ConfigHome: "/home/operator/.config"}
	plan, err := planHeadlessUpgrade(request, "/home/operator/new/bin/nvpair-tui", "/home/operator/new/bin/nvpair-ui-broker", request.ConfigHome)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestHeadlessUpgradeStopsBeforeReplacingAndRestoresOldOwner(t *testing.T) {
	plan := reviewedUpgrade(t, "stop")
	f := &upgradeFixture{unit: plan.original, executable: plan.request.OldExecutable, plan: plan}
	result, err := executeHeadlessUpgrade(context.Background(), plan, f)
	if err != nil || result.(map[string]any)["state"] != "stopped" || f.unit != plan.drain || f.executable != "" {
		t.Fatalf("stop=%v %v", result, err)
	}
	if !reflect.DeepEqual(f.effects, []string{"replace-unit", "daemon-reload", "stop"}) {
		t.Fatalf("drain budget was not active before stop: %v", f.effects)
	}
	plan.request.Phase = "install"
	result, err = executeHeadlessUpgrade(context.Background(), plan, f)
	if err != nil || result.(map[string]any)["state"] != "installed" || f.unit != plan.replacement {
		t.Fatalf("install=%v %v", result, err)
	}
	// Simulate the parent starting the new owner, then requesting rollback.
	f.executable = plan.executable
	plan.request.Phase = "rollback"
	result, err = executeHeadlessUpgrade(context.Background(), plan, f)
	if err != nil || result.(map[string]any)["state"] != "rolled-back" || f.unit != plan.original || f.executable != plan.request.OldExecutable {
		t.Fatalf("rollback=%v %v", result, err)
	}
	for _, effect := range f.effects {
		if effect != "replace-unit" && effect != "daemon-reload" && effect != "stop" && effect != "start" {
			t.Fatalf("upgrade touched a profile, model, membership, enablement or other owner: %s", effect)
		}
	}
}

func TestHeadlessUpgradeRejectsUnreviewedIdentityAndConfiguration(t *testing.T) {
	base := reviewedUpgrade(t, "stop")
	cases := map[string]func(*headlessUpgradeRequest){
		"different-digest":    func(r *headlessUpgradeRequest) { r.ExpectedUnitSHA256 = strings.Repeat("0", 64) },
		"different-config":    func(r *headlessUpgradeRequest) { r.ConfigHome = "/home/other/.config" },
		"different-broker":    func(r *headlessUpgradeRequest) { r.OldBroker = "/home/other/nvpair-ui-broker" },
		"relative-executable": func(r *headlessUpgradeRequest) { r.OldExecutable = "nvpair-tui" },
		"unreviewed-phase":    func(r *headlessUpgradeRequest) { r.Phase = "force" },
		"noncanonical-path": func(r *headlessUpgradeRequest) {
			r.OldExecutable = "/home/operator/old/../old/resources/cli-bin/nvpair-tui"
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			request := base.request
			change(&request)
			if _, err := planHeadlessUpgrade(request, base.executable, "/home/operator/new/bin/nvpair-ui-broker", base.request.ConfigHome); err == nil {
				t.Fatal("unreviewed descriptor admitted")
			}
		})
	}
	for _, mode := range []string{"foreign-bytes", "foreign-owner", "changed-before-exchange"} {
		t.Run(mode, func(t *testing.T) {
			f := &upgradeFixture{unit: base.original, executable: base.request.OldExecutable, plan: base}
			switch mode {
			case "foreign-bytes":
				f.unit = "[Service]\nExecStart=/foreign\n"
			case "foreign-owner":
				f.foreign = true
			case "changed-before-exchange":
				f.fail = "changed-unit"
			}
			if result, err := executeHeadlessUpgrade(context.Background(), base, f); err == nil || result != nil || len(f.effects) != 0 {
				t.Fatalf("unexpected effects %v result=%v error=%v", f.effects, result, err)
			}
		})
	}
}

func TestHeadlessUpgradeTimeoutNeverAdvancesIntoReplacement(t *testing.T) {
	plan := reviewedUpgrade(t, "stop")
	for _, failure := range []string{"stop", "still-running"} {
		t.Run(failure, func(t *testing.T) {
			f := &upgradeFixture{unit: plan.original, executable: plan.request.OldExecutable, plan: plan, fail: failure, stopPending: failure == "still-running"}
			if result, err := executeHeadlessUpgrade(context.Background(), plan, f); err == nil || result != nil || f.unit != plan.drain || f.executable != plan.request.OldExecutable {
				t.Fatalf("unconfirmed stop advanced: %v %v %v", result, err, f.effects)
			}
			install := plan
			install.request.Phase = "install"
			before := append([]string(nil), f.effects...)
			if _, err := executeHeadlessUpgrade(context.Background(), install, f); err == nil || !reflect.DeepEqual(before, f.effects) {
				t.Fatal("install bypassed failed drain")
			}
		})
	}
}

func TestHeadlessUpgradeLostRepliesAreIdempotent(t *testing.T) {
	plan := reviewedUpgrade(t, "stop")
	f := &upgradeFixture{unit: plan.original, executable: plan.request.OldExecutable, plan: plan}
	for _, phase := range []string{"stop", "install", "rollback"} {
		plan.request.Phase = phase
		for attempt := 0; attempt < 2; attempt++ {
			result, err := executeHeadlessUpgrade(context.Background(), plan, f)
			if err != nil || result.(map[string]any)["cleanupConfirmed"] != true {
				t.Fatalf("%s retry %d: %v %v", phase, attempt, result, err)
			}
		}
	}
	stops, starts := 0, 0
	for _, effect := range f.effects {
		if effect == "stop" {
			stops++
		}
		if effect == "start" {
			starts++
		}
	}
	if stops != 1 || starts != 1 || f.unit != plan.original {
		t.Fatalf("retry repeated lifecycle effects: %v", f.effects)
	}
}

func TestHeadlessUpgradeRollbackAfterReloadFailureKeepsOldOwner(t *testing.T) {
	plan := reviewedUpgrade(t, "stop")
	f := &upgradeFixture{unit: plan.original, executable: plan.request.OldExecutable, plan: plan, fail: "daemon-reload"}
	if result, err := executeHeadlessUpgrade(context.Background(), plan, f); err == nil || result != nil || f.unit != plan.drain || f.executable != plan.request.OldExecutable {
		t.Fatalf("expected failed reload after drain publication: %v %v", result, err)
	}
	f.fail = ""
	f.effects = nil
	plan.request.Phase = "rollback"
	result, err := executeHeadlessUpgrade(context.Background(), plan, f)
	if err != nil || result.(map[string]any)["state"] != "rolled-back" || f.unit != plan.original || f.executable != plan.request.OldExecutable {
		t.Fatalf("rollback disturbed original owner: %v %v", result, err)
	}
	if !reflect.DeepEqual(f.effects, []string{"replace-unit", "daemon-reload"}) {
		t.Fatalf("rollback stopped an owner whose drain reload failed: %v", f.effects)
	}
}

func TestHeadlessUpgradeInputAndDrainBudgets(t *testing.T) {
	plan := reviewedUpgrade(t, "stop")
	raw, _ := json.Marshal(plan.request)
	request, err := readHeadlessUpgrade(context.Background(), strings.NewReader(string(raw)))
	if err != nil || request != plan.request {
		t.Fatalf("request changed while decoding: %+v %v", request, err)
	}
	for _, invalid := range []string{string(raw) + "{}", `{"phase":"stop","unit":"other.service"}`, strings.Repeat(" ", headlessRequestLimit+1)} {
		if _, err := readHeadlessUpgrade(context.Background(), strings.NewReader(invalid)); err == nil {
			t.Fatal("unbounded or freeform request admitted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &upgradeFixture{unit: plan.original, executable: plan.request.OldExecutable, plan: plan}
	if _, err := executeHeadlessUpgrade(ctx, plan, f); err == nil || len(f.effects) != 0 {
		t.Fatal("cancelled upgrade caused effects")
	}
	if time.Duration(headlessUnitStopSeconds)*time.Second <= shutdownGrace || headlessServiceStopTimeout <= time.Duration(headlessUnitStopSeconds)*time.Second || headlessServiceTimeout("stop") <= headlessServiceStopTimeout || headlessUpgradeTimeout >= 400*time.Second {
		t.Fatal("product stop and parent/helper deadlines do not nest")
	}
}

func TestHeadlessUpgradeManagerObservationMustBeComplete(t *testing.T) {
	unitPath := "/home/operator/.config/systemd/user/" + headlessUnit
	running := "LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=42\nControlPID=0\nFragmentPath=" + unitPath + "\nDropInPaths=\nJob=0\nControlGroup=/user.slice/pair"
	state, pid, _, err := parseHeadlessUnitState(running, unitPath)
	if err != nil || state != "running" || pid != 42 {
		t.Fatalf("running observation=%s %d %v", state, pid, err)
	}
	stopped := strings.NewReplacer("ActiveState=active", "ActiveState=inactive", "SubState=running", "SubState=dead", "MainPID=42", "MainPID=0").Replace(running)
	if state, pid, _, err = parseHeadlessUnitState(stopped, unitPath); err != nil || state != "stopped" || pid != 0 {
		t.Fatalf("stopped observation=%s %d %v", state, pid, err)
	}
	cases := map[string]string{
		"missing-state":          strings.ReplaceAll(running, "\nSubState=running", ""),
		"duplicate-state":        running + "\nMainPID=42",
		"unknown-field":          running + "\nUnexpected=value",
		"foreign-fragment":       strings.ReplaceAll(running, unitPath, "/other/owner.service"),
		"drop-in":                strings.ReplaceAll(running, "DropInPaths=", "DropInPaths=/other/override.conf"),
		"manager-job":            strings.ReplaceAll(running, "Job=0", "Job=12"),
		"unknown-main":           strings.ReplaceAll(running, "MainPID=42", "MainPID=unknown"),
		"negative-main":          strings.ReplaceAll(running, "MainPID=42", "MainPID=-1"),
		"control-process":        strings.ReplaceAll(running, "ControlPID=0", "ControlPID=12"),
		"loaded-missing":         strings.ReplaceAll(running, "LoadState=loaded", "LoadState=not-found"),
		"active-without-process": strings.ReplaceAll(running, "MainPID=42", "MainPID=0"),
		"inactive-with-process":  strings.ReplaceAll(running, "ActiveState=active", "ActiveState=inactive"),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := parseHeadlessUnitState(raw, unitPath); err == nil {
				t.Fatal("incomplete, contradictory or foreign state admitted")
			}
		})
	}
}
