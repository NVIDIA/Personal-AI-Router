// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"nvpair-shared/hostbootstrap"
)

type fakeProcessInspector struct {
	processes []processRecord
	err       error
}

func (inspector *fakeProcessInspector) Snapshot() ([]processRecord, error) {
	return append([]processRecord(nil), inspector.processes...), inspector.err
}

func TestBrokerProcessGraphDistinguishesDesktopAndHeadless(t *testing.T) {
	for _, target := range hostbootstrap.SupportedTargets() {
		owner, err := nativeOwnerManifestFor(target)
		if err != nil {
			t.Fatal(err)
		}
		if len(owner.DesktopIdentities) < 2 {
			t.Fatalf("%#v desktop identities = %#v", target, owner.DesktopIdentities)
		}
		desktop := processRecord{PID: 10, Path: owner.DesktopIdentities[0]}
		headless := processRecord{PID: 20, Path: owner.TUIPath}
		desktopBroker := processRecord{
			PID:  11,
			PPID: desktop.PID,
			Path: owner.BrokerPath,
		}
		headlessBroker := processRecord{
			PID:  21,
			PPID: headless.PID,
			Path: owner.BrokerPath,
		}
		tests := []struct {
			name      string
			processes []processRecord
			running   bool
			wantErr   bool
		}{
			{
				name:      "desktop",
				processes: []processRecord{desktop, desktopBroker},
				running:   true,
			},
			{
				name:      "headless",
				processes: []processRecord{headless, headlessBroker},
			},
			{
				name: "orphan-broker",
				processes: []processRecord{{
					PID:  30,
					PPID: 999,
					Path: owner.BrokerPath,
				}},
				wantErr: true,
			},
			{
				name: "dual",
				processes: []processRecord{
					desktop,
					desktopBroker,
					headless,
					headlessBroker,
				},
				wantErr: true,
			},
		}
		for _, test := range tests {
			t.Run(string(target.Platform)+"/"+test.name, func(t *testing.T) {
				platform := &runtimePlatform{
					target:    target,
					owner:     owner,
					processes: &fakeProcessInspector{processes: test.processes},
				}
				running, err := platform.DesktopRunning(
					context.Background(),
					hostbootstrap.Request{},
				)
				if (err != nil) != test.wantErr || running != test.running {
					t.Fatalf("running=%t error=%v", running, err)
				}
			})
		}
	}
}

func TestEveryInternalEffectAdvancesPendingJournal(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	plan, err := hostbootstrap.Reconcile(request, absentTestObservations())
	if err != nil {
		t.Fatal(err)
	}
	state := newDiskState("/state", &fakeSecureFS{})
	if err := state.SaveOperation(request, plan); err != nil {
		t.Fatal(err)
	}
	digest, err := digestPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	pending := pendingEffect{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   request.OperationID,
		PlanSHA256:    digest,
		Resource:      plan.Actions[0],
		ActionIndex:   0,
		ExpectedPre:   plan.Observed,
		ExpectedPost:  observationsAfterAction(plan.Observed, request, plan.Actions[0]),
	}
	if err := state.SavePending(pending); err != nil {
		t.Fatal(err)
	}
	var points []string
	platform := &runtimePlatform{
		state: state,
		internalEffectHook: func(point effectPoint, effect string) error {
			points = append(points, string(point)+":"+effect)
			return nil
		},
	}
	applied := 0
	if err := platform.runNativeEffect("ssh:command:0", func() error {
		applied++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if applied != 1 ||
		!reflect.DeepEqual(points, []string{"before:ssh:command:0", "after:ssh:command:0"}) {
		t.Fatalf("effect applied=%d points=%#v", applied, points)
	}
	updated, err := state.LoadPending()
	if err != nil {
		t.Fatal(err)
	}
	if updated.EffectID != "ssh:command:0" || !updated.EffectComplete {
		t.Fatalf("pending = %#v", updated)
	}
}

func TestPendingJournalRejectsStaleTransitionWriter(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformWindows,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	plan, err := hostbootstrap.Reconcile(request, absentTestObservations())
	if err != nil {
		t.Fatal(err)
	}
	state := newDiskState("/state", &fakeSecureFS{})
	if err := state.SaveOperation(request, plan); err != nil {
		t.Fatal(err)
	}
	digest, err := digestPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	initial := pendingEffect{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   request.OperationID,
		PlanSHA256:    digest,
		Resource:      plan.Actions[0],
		ActionIndex:   0,
		ExpectedPre:   plan.Observed,
		ExpectedPost: observationsAfterAction(
			plan.Observed,
			request,
			plan.Actions[0],
		),
	}
	if err := state.SavePending(initial); err != nil {
		t.Fatal(err)
	}
	first := initial
	first.EffectID = "first"
	first.Revision = 1
	if err := state.UpdatePending(initial, first); err != nil {
		t.Fatal(err)
	}
	stale := initial
	stale.EffectID = "stale"
	stale.Revision = 1
	if err := state.UpdatePending(initial, stale); !errors.Is(err, ErrStateIdentity) {
		t.Fatalf("stale transition error = %v", err)
	}
	current, err := state.LoadPending()
	if err != nil {
		t.Fatal(err)
	}
	if current.EffectID != "first" ||
		current.EffectComplete ||
		current.Revision != 1 {
		t.Fatalf("pending = %#v", current)
	}
}

func TestRecoveryRunsOnlyRecordedInternalEffectSuffix(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	plan, err := hostbootstrap.Reconcile(request, absentTestObservations())
	if err != nil {
		t.Fatal(err)
	}
	state := newDiskState("/state", &fakeSecureFS{})
	if err := state.SaveOperation(request, plan); err != nil {
		t.Fatal(err)
	}
	digest, err := digestPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	actionIndex := 1
	expectedPre := observationsAfterAction(
		plan.Observed,
		request,
		plan.Actions[0],
	)
	pending := pendingEffect{
		SchemaVersion:  hostbootstrap.SchemaVersion,
		OperationID:    request.OperationID,
		PlanSHA256:     digest,
		Resource:       plan.Actions[actionIndex],
		ActionIndex:    actionIndex,
		ExpectedPre:    expectedPre,
		ExpectedPost:   observationsAfterAction(expectedPre, request, plan.Actions[actionIndex]),
		EffectID:       "firewall:firewalld-runtime-add",
		EffectComplete: true,
	}
	if err := state.SavePending(pending); err != nil {
		t.Fatal(err)
	}
	var applied []string
	permanentApplied := false
	markerApplied := false
	effects := []nativeEffect{
		{
			ID: "firewall:firewalld-runtime-add",
			Satisfied: func() (bool, error) {
				return true, nil
			},
			Apply: func() error {
				applied = append(applied, "runtime")
				return nil
			},
		},
		{
			ID: "firewall:firewalld-permanent-add",
			Satisfied: func() (bool, error) {
				return permanentApplied, nil
			},
			Apply: func() error {
				permanentApplied = true
				applied = append(applied, "permanent")
				return nil
			},
		},
		{
			ID: "marker:firewall",
			Satisfied: func() (bool, error) {
				return markerApplied, nil
			},
			Apply: func() error {
				markerApplied = true
				applied = append(applied, "marker")
				return nil
			},
		},
	}
	platform := &runtimePlatform{state: state}
	if err := platform.resumeNativeEffectSuffix(pending, effects); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(applied, []string{"permanent", "marker"}) {
		t.Fatalf("recovery replayed the resource instead of its suffix: %#v", applied)
	}
}

func TestRecoveryTrustsCompletedReplaySafeEffectWithoutReplaying(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	plan, err := hostbootstrap.Reconcile(request, absentTestObservations())
	if err != nil {
		t.Fatal(err)
	}
	state := newDiskState("/state", &fakeSecureFS{})
	if err := state.SaveOperation(request, plan); err != nil {
		t.Fatal(err)
	}
	digest, err := digestPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	pending := pendingEffect{
		SchemaVersion:  hostbootstrap.SchemaVersion,
		OperationID:    request.OperationID,
		PlanSHA256:     digest,
		Resource:       plan.Actions[0],
		ActionIndex:    0,
		ExpectedPre:    plan.Observed,
		ExpectedPost:   observationsAfterAction(plan.Observed, request, plan.Actions[0]),
		EffectID:       "apply:helper:command:0",
		EffectComplete: true,
	}
	if err := state.SavePending(pending); err != nil {
		t.Fatal(err)
	}
	var applied []string
	nextApplied := false
	effects := []nativeEffect{
		{
			ID:         pending.EffectID,
			ReplaySafe: true,
			PrefixSatisfied: func() (bool, error) {
				return true, nil
			},
			Apply: func() error {
				applied = append(applied, "daemon-reload")
				return nil
			},
		},
		{
			ID: "next",
			Satisfied: func() (bool, error) {
				return nextApplied, nil
			},
			Apply: func() error {
				nextApplied = true
				applied = append(applied, "next")
				return nil
			},
		},
	}
	platform := &runtimePlatform{state: state}
	if err := platform.resumeNativeEffectSuffix(pending, effects); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(applied, []string{"next"}) {
		t.Fatalf("completed replay-safe effect was replayed: %#v", applied)
	}
}

func TestRecoveryRepairsEarliestMissingSafePrefixBeforeSuffix(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	plan, err := hostbootstrap.Reconcile(request, absentTestObservations())
	if err != nil {
		t.Fatal(err)
	}
	state := newDiskState("/state", &fakeSecureFS{})
	if err := state.SaveOperation(request, plan); err != nil {
		t.Fatal(err)
	}
	digest, err := digestPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	pending := pendingEffect{
		SchemaVersion:  hostbootstrap.SchemaVersion,
		OperationID:    request.OperationID,
		PlanSHA256:     digest,
		Resource:       plan.Actions[0],
		ActionIndex:    0,
		ExpectedPre:    plan.Observed,
		ExpectedPost:   observationsAfterAction(plan.Observed, request, plan.Actions[0]),
		EffectID:       "marker:ssh-service",
		EffectComplete: true,
	}
	if err := state.SavePending(pending); err != nil {
		t.Fatal(err)
	}
	runtimePresent := false
	var applied []string
	effects := []nativeEffect{
		{
			ID:         "firewall:firewalld-runtime-add",
			ReplaySafe: true,
			Satisfied: func() (bool, error) {
				return runtimePresent, nil
			},
			Apply: func() error {
				runtimePresent = true
				applied = append(applied, "runtime")
				return nil
			},
		},
		{
			ID: "firewall:firewalld-permanent-add",
			Satisfied: func() (bool, error) {
				return true, nil
			},
			Apply: func() error {
				applied = append(applied, "permanent")
				return nil
			},
		},
		{
			ID: "marker:ssh-service",
			Satisfied: func() (bool, error) {
				return true, nil
			},
			Apply: func() error {
				applied = append(applied, "marker")
				return nil
			},
		},
	}
	platform := &runtimePlatform{state: state}
	if err := platform.resumeNativeEffectSuffix(pending, effects); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(applied, []string{"runtime"}) {
		t.Fatalf("recovery applied = %#v", applied)
	}
	after, err := state.LoadPending()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, pending) {
		t.Fatalf("replay-safe prefix rewrote pending journal: %#v", after)
	}
}

func TestRecoveryRefusesMissingUnsafeCompletedPrefixBeforeSuffix(t *testing.T) {
	pending := pendingEffect{
		EffectID:       "marker:ssh-service",
		EffectComplete: true,
	}
	mutated := false
	effects := []nativeEffect{
		{
			ID: "unsafe-prefix",
			Satisfied: func() (bool, error) {
				return false, nil
			},
			Apply: func() error {
				mutated = true
				return nil
			},
		},
		{
			ID: "marker:ssh-service",
			Satisfied: func() (bool, error) {
				return true, nil
			},
			Apply: func() error {
				mutated = true
				return nil
			},
		},
	}
	platform := &runtimePlatform{}
	if err := platform.resumeNativeEffectSuffix(
		pending,
		effects,
	); !errors.Is(err, ErrStateIdentity) {
		t.Fatalf("recovery error = %v", err)
	}
	if mutated {
		t.Fatal("unsafe missing prefix allowed a mutation")
	}
}

type firewalldEffectRunner struct {
	runtimeRule   bool
	permanentRule bool
	adds          []string
	rule          string
}

func (runner *firewalldEffectRunner) Run(
	_ context.Context,
	command commandSpec,
) (string, error) {
	joined := strings.Join(command.Args, " ")
	switch joined {
	case "--list-rich-rules":
		if runner.runtimeRule {
			return runner.rule, nil
		}
		return "", nil
	case "--permanent --list-rich-rules":
		if runner.permanentRule {
			return runner.rule, nil
		}
		return "", nil
	case "--add-rich-rule " + runner.rule:
		runner.runtimeRule = true
		runner.adds = append(runner.adds, "runtime")
		return "", nil
	case "--permanent --add-rich-rule " + runner.rule:
		runner.permanentRule = true
		runner.adds = append(runner.adds, "permanent")
		return "", nil
	default:
		return "", errors.New("unexpected firewalld command")
	}
}

func TestFirewalldRecoveryAfterRuntimeAddRunsPermanentSuffixOnly(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	plan, err := hostbootstrap.Reconcile(request, absentTestObservations())
	if err != nil {
		t.Fatal(err)
	}
	actionIndex := 1
	expectedPre := observationsAfterAction(
		plan.Observed,
		request,
		plan.Actions[0],
	)
	digest, err := digestPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	pending := pendingEffect{
		SchemaVersion:  hostbootstrap.SchemaVersion,
		OperationID:    request.OperationID,
		PlanSHA256:     digest,
		Resource:       hostbootstrap.ResourceFirewall,
		ActionIndex:    actionIndex,
		ExpectedPre:    expectedPre,
		ExpectedPost:   observationsAfterAction(expectedPre, request, hostbootstrap.ResourceFirewall),
		EffectID:       "firewall:firewalld-runtime-add",
		EffectComplete: true,
	}
	state := newDiskState("/state", &fakeSecureFS{})
	if err := state.SaveOperation(request, plan); err != nil {
		t.Fatal(err)
	}
	if err := state.SavePending(pending); err != nil {
		t.Fatal(err)
	}
	rule, err := firewalldOwnedRuleForEndpoint(request.Binding.Endpoint.Address)
	if err != nil {
		t.Fatal(err)
	}
	runner := &firewalldEffectRunner{
		runtimeRule: true,
		rule:        rule,
	}
	platform := &runtimePlatform{state: state}
	effects := firewalldNativeEffects(
		context.Background(),
		runner,
		rule,
	)
	if err := platform.resumeNativeEffectSuffix(pending, effects); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runner.adds, []string{"permanent"}) ||
		!runner.runtimeRule ||
		!runner.permanentRule {
		t.Fatalf("firewalld recovery adds=%#v runtime=%t permanent=%t", runner.adds, runner.runtimeRule, runner.permanentRule)
	}
}

func TestFirewalldRecoveryRestoresMissingRuntimePrefixAfterReboot(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	plan, err := hostbootstrap.Reconcile(request, absentTestObservations())
	if err != nil {
		t.Fatal(err)
	}
	actionIndex := 1
	expectedPre := observationsAfterAction(
		plan.Observed,
		request,
		plan.Actions[0],
	)
	digest, err := digestPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	pending := pendingEffect{
		SchemaVersion:  hostbootstrap.SchemaVersion,
		OperationID:    request.OperationID,
		PlanSHA256:     digest,
		Resource:       hostbootstrap.ResourceFirewall,
		ActionIndex:    actionIndex,
		ExpectedPre:    expectedPre,
		ExpectedPost:   observationsAfterAction(expectedPre, request, hostbootstrap.ResourceFirewall),
		EffectID:       "marker:firewall",
		EffectComplete: true,
	}
	state := newDiskState("/state", &fakeSecureFS{})
	if err := state.SaveOperation(request, plan); err != nil {
		t.Fatal(err)
	}
	if err := state.SavePending(pending); err != nil {
		t.Fatal(err)
	}
	rule, err := firewalldOwnedRuleForEndpoint(request.Binding.Endpoint.Address)
	if err != nil {
		t.Fatal(err)
	}
	runner := &firewalldEffectRunner{
		permanentRule: true,
		rule:          rule,
	}
	effects := firewalldNativeEffects(
		context.Background(),
		runner,
		rule,
	)
	effects = append(effects, nativeEffect{
		ID: "marker:firewall",
		Satisfied: func() (bool, error) {
			return true, nil
		},
		Apply: func() error {
			return errors.New("marker must not be replayed")
		},
	})
	platform := &runtimePlatform{state: state}
	if err := platform.resumeNativeEffectSuffix(pending, effects); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runner.adds, []string{"runtime"}) ||
		!runner.runtimeRule ||
		!runner.permanentRule {
		t.Fatalf("firewalld recovery adds=%#v runtime=%t permanent=%t", runner.adds, runner.runtimeRule, runner.permanentRule)
	}
}

func TestRoleRecoveryUsesRecordedDesktopMarkerEffect(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformWindows,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	owner, err := nativeOwnerManifestFor(request.Binding.Target)
	if err != nil {
		t.Fatal(err)
	}
	pending := pendingEffect{
		Resource: hostbootstrap.ResourceActiveRole,
		ExpectedPre: hostbootstrap.Observations{
			RuntimeOwners: []hostbootstrap.RuntimeOwnerObservation{{
				Ownership: hostbootstrap.OwnershipOwned,
				Owner:     hostbootstrap.RoleDesktop,
			}},
		},
	}
	platform := &runtimePlatform{
		target:    request.Binding.Target,
		owner:     owner,
		state:     newDiskState("/state", &fakeSecureFS{}),
		processes: &fakeProcessInspector{},
	}
	effects, err := platform.pendingRoleEffects(
		context.Background(),
		pending,
		request,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(effects) == 0 || effects[0].ID != "role:desktop-marker-remove" {
		t.Fatalf("role recovery effects = %#v", effects)
	}
}

func TestRoleMarkerRequiresLiveRuntimeOwnerPostcondition(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformWindows,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	owner, err := nativeOwnerManifestFor(request.Binding.Target)
	if err != nil {
		t.Fatal(err)
	}
	platform := &runtimePlatform{
		target:          request.Binding.Target,
		owner:           owner,
		state:           newDiskState("/state", &fakeSecureFS{}),
		processes:       &fakeProcessInspector{},
		windowsServices: fakeWindowsServiceAPI{services: map[string]windowsServiceInfo{}},
	}
	effect, err := platform.roleMarkerAdvanceEffect(
		context.Background(),
		request,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := effect.Apply(); !errors.Is(err, ErrVerification) {
		t.Fatalf("marker effect error = %v", err)
	}
	if _, found, err := platform.state.LoadMarker(
		hostbootstrap.ResourceActiveRole,
	); err != nil || found {
		t.Fatalf("role marker found=%t error=%v", found, err)
	}
}

func TestLinuxHelperSandboxRendersRequiredExactPaths(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	definition := linuxNativeDefinition()
	principal := reviewedPrincipalState{UID: 1000, GID: 1000}
	content, err := renderManagedFile(definition.ManagedFiles[0], request, principal)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"ProtectSystem=strict",
		"ProtectHome=false",
		"ReadOnlyPaths=/home /root",
		"/var/lib/nvpair/host-bootstrap",
		"/etc/systemd/system",
		"-/etc/systemd/system/nvidia-pair-headless.service",
		"/etc/ufw",
		"-" + request.Binding.Account.HomePath + "/.ssh",
		"-" + request.Binding.Account.AuthorizedKeysPath,
		"-" + request.Binding.Product.Path,
		"-" + request.Binding.Helper.Path,
	} {
		if !strings.Contains(content, required) {
			t.Fatalf("unit omits required path %q:\n%s", required, content)
		}
	}
	if strings.Contains(content, "ProtectHome=read-only") ||
		strings.Contains(content, "ProtectHome=true") {
		t.Fatal("unit uses a self-conflicting home mount policy")
	}
	if strings.Contains(
		content,
		"ReadWritePaths="+request.Binding.Account.HomePath+" ",
	) {
		t.Fatal("unit exposes the complete reviewed home")
	}
	if strings.Contains(content, ".config/systemd") {
		t.Fatal("helper unit exposes the retired user-systemd subtree")
	}
	for _, broad := range []string{
		" /usr/bin ",
		" /usr/lib ",
		" /usr/share ",
		" /var/lib/dpkg ",
		" /var/cache/apt ",
	} {
		if strings.Contains(content, broad) {
			t.Fatalf("unit exposes unrelated writable tree %q", broad)
		}
	}
}

type fakeFirewallAPI struct {
	rules   []windowsFirewallRule
	err     error
	added   []windowsFirewallRule
	removed []string
}

func (firewall *fakeFirewallAPI) Rules() ([]windowsFirewallRule, error) {
	return append([]windowsFirewallRule(nil), firewall.rules...), firewall.err
}

func (firewall *fakeFirewallAPI) Add(rule windowsFirewallRule) error {
	firewall.added = append(firewall.added, rule)
	return firewall.err
}

func (firewall *fakeFirewallAPI) Remove(name string) error {
	firewall.removed = append(firewall.removed, name)
	return firewall.err
}

func TestWindowsFirewallAPIRequiresOneExactRule(t *testing.T) {
	exact := exactWindowsFirewallRule()
	for _, test := range []struct {
		name        string
		api         *fakeFirewallAPI
		unavailable bool
		present     bool
		exact       bool
	}{
		{name: "absent", api: &fakeFirewallAPI{}},
		{name: "exact", api: &fakeFirewallAPI{rules: []windowsFirewallRule{exact}}, present: true, exact: true},
		{name: "partial", api: &fakeFirewallAPI{rules: []windowsFirewallRule{{Name: windowsFirewallRuleName}}}, present: true},
		{name: "duplicate", api: &fakeFirewallAPI{rules: []windowsFirewallRule{exact, exact}}, unavailable: true},
		{name: "API failure", api: &fakeFirewallAPI{err: errors.New("COM unavailable")}, unavailable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			probe := inspectWindowsFirewallAPI(test.api)
			if probe.Unavailable != test.unavailable ||
				probe.Present != test.present ||
				probe.Exact != test.exact {
				t.Fatalf("probe = %#v", probe)
			}
		})
	}
	partial := &fakeFirewallAPI{rules: []windowsFirewallRule{{Name: windowsFirewallRuleName}}}
	if err := removeWindowsFirewallRule(partial); !errors.Is(err, ErrStateIdentity) {
		t.Fatalf("partial removal error = %v", err)
	}
	if len(partial.removed) != 0 {
		t.Fatal("partial rule was removed")
	}
	empty := &fakeFirewallAPI{}
	if err := addWindowsFirewallRule(empty); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(empty.added, []windowsFirewallRule{exact}) {
		t.Fatalf("added = %#v", empty.added)
	}
}

func TestWindowsFirewallRejectsEveryRule3AdmissionRestriction(t *testing.T) {
	restricted := []windowsFirewallRule{
		exactWindowsFirewallRule(),
		exactWindowsFirewallRule(),
		exactWindowsFirewallRule(),
		exactWindowsFirewallRule(),
		exactWindowsFirewallRule(),
		exactWindowsFirewallRule(),
	}
	restricted[0].LocalAppPackageID = "S-1-15-2-1"
	restricted[1].LocalUserOwner = "S-1-5-21-owner"
	restricted[2].LocalUserAuthorizedList = "O:LSD:(A;;CC;;;S-1-5-21-user)"
	restricted[3].RemoteUserAuthorizedList = "O:LSD:(A;;CC;;;S-1-5-21-user)"
	restricted[4].RemoteMachineAuthorizedList = "O:LSD:(A;;CC;;;S-1-5-21-machine)"
	restricted[5].SecureFlags = 1
	for index, rule := range restricted {
		probe := inspectWindowsFirewallAPI(&fakeFirewallAPI{
			rules: []windowsFirewallRule{rule},
		})
		if !probe.Present || probe.Exact || probe.Unavailable {
			t.Fatalf("restriction %d probe = %#v", index, probe)
		}
	}
}

func TestLinuxFirewallManagerAndRuleIdentityAreExact(t *testing.T) {
	for _, test := range []struct {
		name   string
		config []byte
		want   linuxFirewallManager
	}{
		{name: "active UFW", config: []byte("ENABLED=yes\n"), want: linuxFirewallUFW},
		{name: "inactive UFW falls through", config: []byte("ENABLED=no\n"), want: linuxFirewallFirewalld},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := classifyLinuxFirewallManager(test.config, nil)
			if err != nil || got != test.want {
				t.Fatalf("manager = %q error=%v, want %q", got, err, test.want)
			}
		})
	}
	ipv4, err := firewalldOwnedRuleForEndpoint("192.0.2.10")
	if err != nil || !strings.Contains(ipv4, `family="ipv4"`) {
		t.Fatalf("IPv4 rule = %q error=%v", ipv4, err)
	}
	ipv6, err := firewalldOwnedRuleForEndpoint("2001:db8::10")
	if err != nil || !strings.Contains(ipv6, `family="ipv6"`) {
		t.Fatalf("IPv6 rule = %q error=%v", ipv6, err)
	}
}

func TestUFWOwnedTupleMustImmediatelyOwnItsRawRule(t *testing.T) {
	tuple := "### tuple ### allow tcp 22 0.0.0.0/0 any 0.0.0.0/0 in comment=" +
		windowsFirewallRuleName
	rawRule := "-A ufw-user-input -p tcp --dport 22 -j ACCEPT"
	for _, test := range []struct {
		name  string
		lines []string
		exact bool
	}{
		{name: "coupled", lines: []string{tuple, rawRule}, exact: true},
		{name: "reversed", lines: []string{rawRule, tuple}},
		{name: "separated", lines: []string{tuple, "# unrelated", rawRule}},
		{name: "different raw rule", lines: []string{tuple, "-A ufw-user-input -p tcp --dport 22 -j DROP"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			present, exact, err := parseUFWOwnedRule(
				[]byte(strings.Join(test.lines, "\n") + "\n"),
			)
			if err != nil || !present || exact != test.exact {
				t.Fatalf("present=%t exact=%t error=%v", present, exact, err)
			}
		})
	}
}

func TestUFWConfigAndRuntimeRulesAreCoupledPerAddressFamily(t *testing.T) {
	if enabled, known := parseUFWIPv6([]byte("IPV6=yes\n")); !known || !enabled {
		t.Fatalf("IPV6=yes enabled=%t known=%t", enabled, known)
	}
	if enabled, known := parseUFWIPv6([]byte("IPV6=no\n")); !known || enabled {
		t.Fatalf("IPV6=no enabled=%t known=%t", enabled, known)
	}
	tests := []struct {
		name       string
		family     string
		configCIDR string
	}{
		{name: "IPv4", family: "ipv4", configCIDR: "0.0.0.0/0"},
		{name: "IPv6", family: "ipv6", configCIDR: "::/0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			chain := "ufw-user-input"
			if test.family == "ipv6" {
				chain = "ufw6-user-input"
			}
			config := []byte(
				"### tuple ### allow tcp 22 " + test.configCIDR +
					" any " + test.configCIDR +
					" in comment=" + windowsFirewallRuleName + "\n" +
					"-A " + chain + " -p tcp --dport 22 -j ACCEPT\n",
			)
			present, exact, err := parseUFWOwnedConfigRule(
				config,
				test.family,
			)
			if err != nil || !present || !exact {
				t.Fatalf("config present=%t exact=%t error=%v", present, exact, err)
			}
			runtime := "-A " + chain + " -p tcp -m tcp --dport 22 -j ACCEPT\n"
			present, exact, err = parseUFWRuntimeRule(runtime, test.family)
			if err != nil || !present || !exact {
				t.Fatalf("runtime present=%t exact=%t error=%v", present, exact, err)
			}
		})
	}
	duplicateRuntime := strings.Repeat(
		"-A ufw-user-input -p tcp -m tcp --dport 22 -j ACCEPT\n",
		2,
	)
	if _, _, err := parseUFWRuntimeRule(
		duplicateRuntime,
		"ipv4",
	); !errors.Is(err, ErrFirewallUnavailable) {
		t.Fatalf("duplicate runtime error = %v", err)
	}
}

type fakeAuthorizedKeyStore struct {
	current      []byte
	changeOnSwap []byte
	swaps        int
}

func (store *fakeAuthorizedKeyStore) Read() ([]byte, error) {
	return append([]byte(nil), store.current...), nil
}

func (store *fakeAuthorizedKeyStore) CompareAndSwap(expected, replacement []byte) error {
	store.swaps++
	if store.changeOnSwap != nil {
		store.current = append([]byte(nil), store.changeOnSwap...)
		store.changeOnSwap = nil
	}
	if !bytes.Equal(store.current, expected) {
		return ErrStateIdentity
	}
	store.current = append([]byte(nil), replacement...)
	return nil
}

func TestAuthorizedKeyCASPreservesConcurrentForeignChange(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	original := []byte("ssh-ed25519 FOREIGN first\n")
	concurrent := []byte("ssh-ed25519 FOREIGN first\nssh-ed25519 FOREIGN concurrent\n")
	store := &fakeAuthorizedKeyStore{current: original, changeOnSwap: concurrent}
	if err := applyAuthorizedKeyCAS(store, request, resourceMarker{}, false); !errors.Is(err, ErrStateIdentity) {
		t.Fatalf("CAS error = %v", err)
	}
	if !bytes.Equal(store.current, concurrent) || store.swaps != 1 {
		t.Fatalf("concurrent content was lost: %q", store.current)
	}
}

func TestOwnedAuthorizedKeyRequiresExactManagedLine(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	probe := nativeProbe{
		Available:      true,
		Present:        true,
		Exact:          false,
		IdentitySHA256: request.Binding.ControllerKey.FingerprintSHA256,
	}
	observation, err := classifyOwnedAuthorizedKey(
		probe,
		resourceMarker{
			SchemaVersion:  hostbootstrap.SchemaVersion,
			Kind:           hostbootstrap.ResourceAuthorizedKey,
			OperationID:    request.OperationID,
			IdentitySHA256: request.Binding.ControllerKey.FingerprintSHA256,
		},
		request,
	)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Ownership != hostbootstrap.OwnershipForeign {
		t.Fatalf("authorized-key observation = %#v", observation)
	}
}

func TestAuthorizedKeyRotationRecognizesExactPriorMarkerLineWithSpaces(t *testing.T) {
	oldRequest := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	oldRequest.Binding.ControllerKey = testPublicKey(9)
	request := oldRequest
	request.Binding.ControllerKey = testPublicKey(10)
	priorLine := string(oldRequest.Binding.ControllerKey.Algorithm) + " " +
		oldRequest.Binding.ControllerKey.Material + " " +
		ownedKeyCommentPrefix + oldRequest.Binding.Helper.SHA256 +
		" managed by NVIDIA PAIR"
	priorProbe, err := probeAuthorizedKeyBytes(
		[]byte(priorLine+"\n"),
		request,
		priorLine,
	)
	if err != nil {
		t.Fatal(err)
	}
	if priorProbe.Exact ||
		!priorProbe.PriorExact ||
		priorProbe.IdentitySHA256 !=
			oldRequest.Binding.ControllerKey.FingerprintSHA256 {
		t.Fatalf("prior probe = %#v", priorProbe)
	}
	desiredLine := desiredAuthorizedKeyLine(request)
	desiredProbe, err := probeAuthorizedKeyBytes(
		[]byte(desiredLine+"\n"),
		request,
		priorLine,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !desiredProbe.Exact || desiredProbe.PriorExact {
		t.Fatalf("desired probe = %#v", desiredProbe)
	}
	tabbed := strings.Replace(priorLine, " ", "\t", 2)
	tabbedProbe, err := probeAuthorizedKeyBytes(
		[]byte(tabbed+"\n"),
		request,
		priorLine,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !tabbedProbe.Present ||
		tabbedProbe.Exact ||
		tabbedProbe.PriorExact {
		t.Fatalf("tabbed marker probe = %#v", tabbedProbe)
	}
	optionsLine := `from="192.0.2.1" ` + priorLine
	optionsProbe, err := probeAuthorizedKeyBytes(
		[]byte(optionsLine+"\n"),
		request,
		priorLine,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !optionsProbe.Present ||
		optionsProbe.Exact ||
		optionsProbe.PriorExact {
		t.Fatalf("options marker probe = %#v", optionsProbe)
	}
	unrelatedInvalid := "this is not a valid key"
	store := &fakeAuthorizedKeyStore{
		current: []byte(unrelatedInvalid + "\n" + priorLine + "\n"),
	}
	if err := applyAuthorizedKeyCAS(
		store,
		request,
		resourceMarker{
			SchemaVersion:     hostbootstrap.SchemaVersion,
			Kind:              hostbootstrap.ResourceAuthorizedKey,
			OperationID:       oldRequest.OperationID,
			IdentitySHA256:    oldRequest.Binding.ControllerKey.FingerprintSHA256,
			AuthorizedKeyLine: priorLine,
		},
		true,
	); err != nil {
		t.Fatalf("rotation error = %v", err)
	}
	if string(store.current) != unrelatedInvalid+"\n"+desiredLine+"\n" {
		t.Fatalf("rotated key = %q", store.current)
	}
	filesystem := &fakeSecureFS{}
	state := newDiskState("/state", filesystem)
	priorMarker := resourceMarker{
		SchemaVersion:     hostbootstrap.SchemaVersion,
		Kind:              hostbootstrap.ResourceAuthorizedKey,
		OperationID:       oldRequest.OperationID,
		IdentitySHA256:    oldRequest.Binding.ControllerKey.FingerprintSHA256,
		AuthorizedKeyLine: priorLine,
	}
	if err := state.SaveMarker(priorMarker); err != nil {
		t.Fatal(err)
	}
	nextMarker := resourceMarker{
		SchemaVersion:     hostbootstrap.SchemaVersion,
		Kind:              hostbootstrap.ResourceAuthorizedKey,
		OperationID:       request.OperationID,
		IdentitySHA256:    request.Binding.ControllerKey.FingerprintSHA256,
		AuthorizedKeyLine: desiredLine,
	}
	platform := &runtimePlatform{state: state}
	if err := platform.replaceMarkerCAS(
		priorMarker,
		true,
		nextMarker,
	); err != nil {
		t.Fatalf("marker CAS error = %v", err)
	}
	if err := platform.replaceMarkerCAS(
		priorMarker,
		true,
		nextMarker,
	); !errors.Is(err, ErrStateIdentity) {
		t.Fatalf("stale marker CAS error = %v", err)
	}
}

type fakeHelperComponents struct {
	executable helperComponentState
	definition helperComponentState
	service    helperComponentState
	endpoint   helperComponentState
}

type versionedHelperComponents struct {
	installedDigest string
}

func (components versionedHelperComponents) InspectHelper(
	_ context.Context,
	request hostbootstrap.Request,
) (helperInspection, error) {
	exact := request.Binding.Helper.SHA256 == components.installedDigest
	return helperInspection{
		Executable: helperComponentState{
			Present: true,
			Exact:   exact,
		},
		Definition:           helperComponentState{Present: true, Exact: true},
		Service:              helperComponentState{Present: true, Exact: true},
		Endpoint:             helperComponentState{Present: true, Exact: true},
		IdentityExact:        exact,
		DefinitionCompatible: true,
		ExecutableDigest:     components.installedDigest,
	}, nil
}

type removalHelperComponents struct {
	request hostbootstrap.Request
	phase   int
}

func (components *removalHelperComponents) InspectHelper(
	_ context.Context,
	_ hostbootstrap.Request,
) (helperInspection, error) {
	inspection := helperInspection{
		Executable:           helperComponentState{Present: true, Exact: true},
		Definition:           helperComponentState{Present: true, Exact: true},
		Service:              helperComponentState{Present: true, Exact: true},
		Endpoint:             helperComponentState{Present: true, Exact: true},
		IdentityExact:        true,
		DefinitionCompatible: true,
		ExecutableDigest:     components.request.Binding.Helper.SHA256,
	}
	if components.phase >= 1 {
		inspection.Service.Exact = false
		inspection.Endpoint = helperComponentState{}
	}
	if components.phase >= 2 {
		inspection.Definition = helperComponentState{}
		inspection.Service = helperComponentState{}
	}
	return inspection, nil
}

type helperRemovalRunner struct {
	components *removalHelperComponents
	calls      []string
}

func (runner *helperRemovalRunner) Run(
	_ context.Context,
	command commandSpec,
) (string, error) {
	runner.calls = append(runner.calls, strings.Join(command.Args, " "))
	runner.components.phase++
	return "", nil
}

func (components fakeHelperComponents) InspectHelper(
	_ context.Context,
	request hostbootstrap.Request,
) (helperInspection, error) {
	digest := ""
	if components.executable.Exact {
		digest = request.Binding.Helper.SHA256
	}
	return helperInspection{
		Executable: components.executable,
		Definition: components.definition,
		Service:    components.service,
		Endpoint:   components.endpoint,
		DefinitionCompatible: !components.definition.Present ||
			components.definition.Exact,
		ExecutableDigest: digest,
		IdentityExact: (!components.executable.Present ||
			components.executable.Exact) &&
			(!components.definition.Present ||
				components.definition.Exact) &&
			(!components.endpoint.Present ||
				components.endpoint.Exact),
	}, nil
}

func TestExactPackagedHelperWithoutServiceIsAdoptable(
	t *testing.T,
) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformWindows,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	platform := &runtimePlatform{
		target: request.Binding.Target,
		state:  newDiskState("/state", &fakeSecureFS{}),
		helperComponents: fakeHelperComponents{
			executable: helperComponentState{
				Present: true,
				Exact:   true,
			},
		},
	}
	observation, err := platform.inspectArtifact(
		context.Background(),
		hostbootstrap.ResourceHelper,
		request,
	)
	if err != nil ||
		observation.Ownership != hostbootstrap.OwnershipAbsent ||
		observation.Identity != nil {
		t.Fatalf(
			"packaged helper observation=%#v error=%v",
			observation,
			err,
		)
	}
}

type fakeWindowsServiceAPI struct {
	services map[string]windowsServiceInfo
	err      error
}

func (api fakeWindowsServiceAPI) Inspect(name string) (windowsServiceInfo, error) {
	if api.err != nil {
		return windowsServiceInfo{}, api.err
	}
	service, found := api.services[name]
	if !found {
		return windowsServiceInfo{}, ErrStateMissing
	}
	return service, nil
}

func TestWindowsHeadlessOwnershipUsesExactStructuredSCMState(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformWindows,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	owner, err := nativeOwnerManifestFor(request.Binding.Target)
	if err != nil {
		t.Fatal(err)
	}
	exact := windowsServiceInfo{
		Executable:  owner.TUIPath,
		Arguments:   append([]string(nil), owner.OwnerCommand[1:]...),
		ServiceType: 0x10,
		StartType:   2,
		Account:     "LocalSystem",
		State:       4,
	}
	for _, test := range []struct {
		name string
		edit func(*windowsServiceInfo)
		want bool
	}{
		{name: "exact", want: true},
		{name: "wrong arguments", edit: func(info *windowsServiceInfo) {
			info.Arguments = []string{"--headless"}
		}},
		{name: "manual start", edit: func(info *windowsServiceInfo) {
			info.StartType = 3
		}},
		{name: "wrong account", edit: func(info *windowsServiceInfo) {
			info.Account = "pairuser"
		}},
		{name: "stopped", edit: func(info *windowsServiceInfo) {
			info.State = 1
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			info := exact
			info.Arguments = append([]string(nil), exact.Arguments...)
			if test.edit != nil {
				test.edit(&info)
			}
			runner := &fixedCommandRunner{err: errors.New("sc.exe must not be used for ownership")}
			platform := &runtimePlatform{
				target: request.Binding.Target,
				owner:  owner,
				runner: runner,
				windowsServices: fakeWindowsServiceAPI{services: map[string]windowsServiceInfo{
					owner.HeadlessIdentity: info,
				}},
			}
			present, identityExact, lifecycleExact, err :=
				platform.probeHeadlessOwner(context.Background(), request)
			got := identityExact && lifecycleExact
			if err != nil || !present || got != test.want {
				t.Fatalf("present=%t identity=%t lifecycle=%t error=%v", present, identityExact, lifecycleExact, err)
			}
			if runner.calls != 0 {
				t.Fatalf("SCM ownership fell back to command parsing")
			}
		})
	}
}

func TestWindowsSSHInspectsCapabilityAndServiceIndependently(t *testing.T) {
	runner := &fixedCommandRunner{output: "State : Installed"}
	inspection, err := inspectWindowsSSHNative(
		context.Background(),
		runner,
		fakeWindowsServiceAPI{services: map[string]windowsServiceInfo{}},
	)
	if err != nil ||
		!inspection.Capability.Exact ||
		inspection.Service.Present ||
		inspection.Exact() {
		t.Fatalf("capability-only inspection=%#v error=%v", inspection, err)
	}
	exactService := windowsServiceInfo{
		Executable:  windowsSSHDPath,
		Arguments:   []string{},
		ServiceType: 0x10,
		StartType:   2,
		Account:     "LocalSystem",
		State:       4,
	}
	inspection, err = inspectWindowsSSHNative(
		context.Background(),
		runner,
		fakeWindowsServiceAPI{services: map[string]windowsServiceInfo{
			"sshd": exactService,
		}},
	)
	if err != nil || !inspection.Exact() {
		t.Fatalf("exact SSH inspection=%#v error=%v", inspection, err)
	}
	exactService.Executable = `C:\foreign\sshd.exe`
	inspection, err = inspectWindowsSSHNative(
		context.Background(),
		runner,
		fakeWindowsServiceAPI{services: map[string]windowsServiceInfo{
			"sshd": exactService,
		}},
	)
	if err != nil || inspection.IdentityExact() {
		t.Fatalf("foreign SSH inspection=%#v error=%v", inspection, err)
	}
}

func TestSSHServiceRequiresRunningAndBootEnabled(t *testing.T) {
	if parseRemoteLoginState("Remote Login: On") != remoteLoginOn ||
		parseRemoteLoginState("Remote Login: Off") != remoteLoginOff ||
		parseRemoteLoginState("Remote login: On") != remoteLoginUnavailable {
		t.Fatal("Remote Login parser did not preserve exact three-state semantics")
	}
	runningDisabled := nativeServiceState{
		Present: true,
		Running: true,
		Enabled: false,
	}
	if runningDisabled.Exact() {
		t.Fatal("running but disabled service was accepted")
	}
	linux := parseSystemdServiceState(
		"LoadState=loaded\nActiveState=active\nUnitFileState=disabled\n",
	)
	if !linux.Present || !linux.Running || linux.Enabled || linux.Exact() {
		t.Fatalf("Linux running-disabled state = %#v", linux)
	}
	darwin, valid := parseLaunchdSSHServiceState(
		"Remote Login: On",
		"disabled services = {\n\"com.openssh.sshd\" => true\n}\n",
		"system/com.openssh.sshd = {\nprogram = /usr/sbin/sshd\narguments = {\n/usr/sbin/sshd\n-i\n}\nusername = root\nstate = running\n}\n",
	)
	if !valid || !darwin.Present || !darwin.Running || darwin.Enabled || darwin.Exact() {
		t.Fatalf("macOS running-disabled state = %#v valid=%t", darwin, valid)
	}
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformWindows,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	platform := &runtimePlatform{
		target: request.Binding.Target,
		windowsServices: fakeWindowsServiceAPI{services: map[string]windowsServiceInfo{
			"sshd": {
				Executable:  windowsSSHDPath,
				Arguments:   []string{},
				ServiceType: 0x10,
				StartType:   3,
				Account:     "LocalSystem",
				State:       4,
			},
		}},
	}
	present, exact, available, err := platform.probeEndpointResource(
		context.Background(),
		hostbootstrap.ResourceSSHService,
	)
	if err != nil || !available || !present || exact {
		t.Fatalf("Windows running-disabled present=%t exact=%t available=%t error=%v", present, exact, available, err)
	}
}

func TestLinuxSSHRequiresExactPackageUnitAndNoDropIns(t *testing.T) {
	service := "LoadState=loaded\n" +
		"ActiveState=active\n" +
		"UnitFileState=enabled\n" +
		"FragmentPath=/lib/systemd/system/ssh.service\n" +
		"DropInPaths=\n" +
		"ExecStart={ path=/usr/sbin/sshd ; argv[]=/usr/sbin/sshd -D $SSHD_OPTS ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }\n"
	exact := parseLinuxSSHInspection(
		"Status: install ok installed\n",
		"",
		service,
	)
	if !exact.Exact() {
		t.Fatalf("exact inspection = %#v", exact)
	}
	withDropIn := parseLinuxSSHInspection(
		"Status: install ok installed\n",
		"",
		strings.Replace(
			service,
			"DropInPaths=",
			"DropInPaths=/etc/systemd/system/ssh.service.d/foreign.conf",
			1,
		),
	)
	if withDropIn.Exact() {
		t.Fatalf("drop-in inspection = %#v", withDropIn)
	}
	changedPackage := parseLinuxSSHInspection(
		"Status: install ok installed\n",
		"??5?????? c /etc/ssh/sshd_config\n",
		service,
	)
	if changedPackage.Exact() {
		t.Fatalf("changed package inspection = %#v", changedPackage)
	}
	packageAbsentForeignUnit := parseLinuxSSHInspection("", "", service)
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	for _, inspection := range []linuxSSHInspection{
		withDropIn,
		changedPackage,
		packageAbsentForeignUnit,
		func() linuxSSHInspection {
			changed := exact
			changed.ExecStartExact = false
			return changed
		}(),
	} {
		observed := exactTestObservations(request)
		observed.SSHService.Ownership =
			classifyLinuxSSHOwnership(
				inspection.PackageInstalled || inspection.UnitLoaded,
				inspection.IdentityExact(),
				true,
			)
		plan, err := hostbootstrap.Reconcile(request, observed)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Decision != hostbootstrap.DecisionRefuseForeign ||
			len(plan.Actions) != 0 {
			t.Fatalf("identity drift plan = %#v", plan)
		}
	}
	lifecycle := exact
	lifecycle.Running = false
	observed := exactTestObservations(request)
	observed.SSHService.Ownership =
		classifyLinuxSSHOwnership(
			true,
			lifecycle.IdentityExact(),
			true,
		)
	changedEndpoint := request.Binding.Endpoint
	changedEndpoint.Port = 2222
	observed.SSHService.Endpoint = &changedEndpoint
	plan, err := hostbootstrap.Reconcile(request, observed)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Decision != hostbootstrap.DecisionRepairOwned ||
		!reflect.DeepEqual(
			plan.Actions,
			[]hostbootstrap.ResourceKind{hostbootstrap.ResourceSSHService},
		) {
		t.Fatalf("lifecycle repair plan = %#v", plan)
	}
}

func TestHelperInspectionDoesNotShortCircuitMissingExecutable(t *testing.T) {
	components := fakeHelperComponents{
		executable: helperComponentState{Present: false},
		definition: helperComponentState{Present: true, Exact: false},
		service:    helperComponentState{Present: true, Exact: false},
		endpoint:   helperComponentState{Present: true, Exact: false},
	}
	inspection, err := inspectHelperComponents(
		context.Background(),
		testRequest(hostbootstrap.Target{
			Platform:     hostbootstrap.PlatformWindows,
			Architecture: hostbootstrap.ArchitectureAMD64,
		}),
		components,
	)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Executable.Present ||
		!inspection.Definition.Present ||
		!inspection.Service.Present ||
		!inspection.Endpoint.Present {
		t.Fatalf("inspection short-circuited: %#v", inspection)
	}
}

func TestHelperMissingComponentsAreOwnedPartialButDriftIsForeign(t *testing.T) {
	exact := fakeHelperComponents{
		executable: helperComponentState{Present: true, Exact: true},
		definition: helperComponentState{Present: true, Exact: true},
		service:    helperComponentState{Present: true, Exact: true},
		endpoint:   helperComponentState{Present: true, Exact: true},
	}
	missing := []fakeHelperComponents{exact, exact, exact, exact}
	missing[0].executable = helperComponentState{}
	missing[1].definition = helperComponentState{}
	missing[2].service = helperComponentState{}
	missing[3].endpoint = helperComponentState{}
	for index, components := range missing {
		inspection, err := inspectHelperComponents(
			context.Background(),
			testRequest(hostbootstrap.Target{
				Platform:     hostbootstrap.PlatformWindows,
				Architecture: hostbootstrap.ArchitectureAMD64,
			}),
			components,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !inspection.IdentityExact ||
			(inspection.Executable.Exact &&
				inspection.Definition.Exact &&
				inspection.Service.Exact &&
				inspection.Endpoint.Exact) {
			t.Fatalf("missing component %d inspection = %#v", index, inspection)
		}
	}
	drifted := []fakeHelperComponents{exact, exact, exact, exact}
	drifted[0].executable.Exact = false
	drifted[1].definition.Exact = false
	drifted[2].service.Exact = false
	drifted[3].endpoint.Exact = false
	for index, components := range drifted {
		inspection, err := inspectHelperComponents(
			context.Background(),
			testRequest(hostbootstrap.Target{
				Platform:     hostbootstrap.PlatformWindows,
				Architecture: hostbootstrap.ArchitectureAMD64,
			}),
			components,
		)
		if err != nil {
			t.Fatal(err)
		}
		if (index == 2 && !inspection.IdentityExact) ||
			(index != 2 && inspection.IdentityExact) {
			t.Fatalf("drifted component %d inspection = %#v", index, inspection)
		}
	}
}

func TestHistoricalHelperUpgradeIsOwnedPartial(t *testing.T) {
	original := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformWindows,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	exact := exactTestObservations(original)
	plan, err := hostbootstrap.Reconcile(original, exact)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := hostbootstrap.NewReceipt(plan, exact)
	if err != nil {
		t.Fatal(err)
	}
	state := newDiskState("/state", &fakeSecureFS{})
	if err := state.SaveOperation(original, plan); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	marker := resourceMarker{
		SchemaVersion:  hostbootstrap.SchemaVersion,
		Kind:           hostbootstrap.ResourceHelper,
		OperationID:    original.OperationID,
		IdentitySHA256: original.Binding.Helper.SHA256,
	}
	if err := state.SaveMarker(marker); err != nil {
		t.Fatal(err)
	}
	next := original
	next.OperationID = strings.Repeat("cd", 16)
	next.Binding.Helper.SHA256 = strings.Repeat("56", 32)
	platform := &runtimePlatform{
		target: original.Binding.Target,
		state:  state,
		helperComponents: versionedHelperComponents{
			installedDigest: original.Binding.Helper.SHA256,
		},
	}
	probe, err := platform.probe(
		context.Background(),
		hostbootstrap.ResourceHelper,
		next,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !probe.Present || probe.Exact || !probe.IdentityExact ||
		probe.IdentitySHA256 != original.Binding.Helper.SHA256 {
		t.Fatalf("historical helper probe = %#v", probe)
	}
	observation, err := platform.inspectArtifact(
		context.Background(),
		hostbootstrap.ResourceHelper,
		next,
	)
	if err != nil ||
		observation.Identity == nil ||
		*observation.Identity != original.Binding.Helper {
		t.Fatalf("historical helper observation=%#v error=%v", observation, err)
	}
	observed := exactTestObservations(next)
	observed.Helper = hostbootstrap.ArtifactObservation{
		Ownership: hostbootstrap.OwnershipOwned,
		Identity:  &original.Binding.Helper,
	}
	upgrade, err := hostbootstrap.Reconcile(next, observed)
	if err != nil ||
		upgrade.Decision != hostbootstrap.DecisionRepairOwned {
		t.Fatalf("upgrade plan = %#v error = %v", upgrade, err)
	}
}

func TestProductMarkerNeverResolvesThroughHelperIdentity(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("binds a Windows target to a host directory")
	}
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformWindows,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	if request.Binding.Product.SHA256 == request.Binding.Helper.SHA256 {
		t.Fatal("test requires different product and helper digests")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nvpair"), []byte("x"), 0755); err != nil {
		t.Fatal(err)
	}
	request.Binding.Product.Path = root
	files := []payloadManifestFile{{
		Path:   "nvpair",
		Type:   payloadTypeFile,
		Size:   1,
		Mode:   0755,
		SHA256: payloadDigest([]byte("x")),
	}}
	state := newDiskState("/state", &fakeSecureFS{})
	exact := exactTestObservations(request)
	plan, err := hostbootstrap.Reconcile(request, exact)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SaveOperation(request, plan); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveMarker(resourceMarker{
		SchemaVersion:  hostbootstrap.SchemaVersion,
		Kind:           hostbootstrap.ResourceProduct,
		OperationID:    request.OperationID,
		IdentitySHA256: request.Binding.Product.SHA256,
		Files:          files,
	}); err != nil {
		t.Fatal(err)
	}
	platform := &runtimePlatform{
		target: request.Binding.Target,
		state:  state,
		payload: payloadBundle{
			Product: payloadSource{
				SHA256: request.Binding.Product.SHA256,
				Files:  files,
			},
		},
	}
	observation, err := platform.inspectArtifact(
		context.Background(),
		hostbootstrap.ResourceProduct,
		request,
	)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Identity == nil ||
		observation.Identity.SHA256 != request.Binding.Product.SHA256 {
		t.Fatalf("product observation = %#v", observation)
	}
}

func TestHelperRemovalCommandsResumeFromReadOnlyPostconditions(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformWindows,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	components := &removalHelperComponents{request: request}
	runner := &helperRemovalRunner{components: components}
	platform := &runtimePlatform{
		target:           request.Binding.Target,
		definition:       windowsNativeDefinition(),
		runner:           runner,
		state:            newDiskState("/state", &fakeSecureFS{}),
		helperComponents: components,
	}
	effects := platform.commandEffects(
		context.Background(),
		"uninstall:helper",
		platform.definition.Remove[hostbootstrap.ResourceHelper],
		request,
	)
	if len(effects) != 2 {
		t.Fatalf("helper removal effects = %d", len(effects))
	}
	if err := platform.executeNativeEffect(effects[0]); err != nil {
		t.Fatal(err)
	}
	if err := platform.executeNativeEffect(effects[0]); err != nil {
		t.Fatalf("stopped helper was not retry-safe: %v", err)
	}
	if err := platform.executeNativeEffect(effects[1]); err != nil {
		t.Fatal(err)
	}
	if err := platform.executeNativeEffect(effects[0]); err != nil {
		t.Fatalf("absent service did not satisfy stop retry: %v", err)
	}
	if err := platform.executeNativeEffect(effects[1]); err != nil {
		t.Fatalf("deleted helper was not retry-safe: %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("removal commands replayed = %#v", runner.calls)
	}
}

func TestLinuxHelperEffectiveServiceAndSocketIdentitiesAreExact(t *testing.T) {
	absent := parseLinuxHelperEffectiveInspection(
		"LoadState=not-found\n",
		"LoadState=not-found\n",
		"1000",
	)
	if absent.Present || absent.IdentityExact || absent.LifecycleExact {
		t.Fatalf("absent helper inspection = %#v", absent)
	}
	service := "LoadState=loaded\nActiveState=active\nUnitFileState=static\n" +
		"FragmentPath=/etc/systemd/system/nvpair-host-helper.service\n" +
		"DropInPaths=\nType=simple\nUser=root\nGroup=root\n" +
		"ExecStart={ path=/usr/libexec/nvpair-host-helper ; argv[]=/usr/libexec/nvpair-host-helper service ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }\n"
	socket := "LoadState=loaded\nActiveState=active\nUnitFileState=enabled\n" +
		"FragmentPath=/etc/systemd/system/nvpair-host-helper.socket\n" +
		"DropInPaths=\nListen=/run/nvpair-host-helper.sock (Stream)\n" +
		"SocketMode=0660\nSocketUser=root\nSocketGroup=1000\n"
	exact := parseLinuxHelperEffectiveInspection(service, socket, "1000")
	if !exact.Present || !exact.IdentityExact || !exact.LifecycleExact {
		t.Fatalf("exact helper inspection = %#v", exact)
	}
	inactive := parseLinuxHelperEffectiveInspection(
		strings.Replace(service, "ActiveState=active", "ActiveState=inactive", 1),
		socket,
		"1000",
	)
	if !inactive.IdentityExact || !inactive.LifecycleExact {
		t.Fatalf("socket-activated inactive service = %#v", inactive)
	}
	override := parseLinuxHelperEffectiveInspection(
		strings.Replace(
			service,
			"DropInPaths=",
			"DropInPaths=/etc/systemd/system/nvpair-host-helper.service.d/foreign.conf",
			1,
		),
		socket,
		"1000",
	)
	if override.IdentityExact {
		t.Fatalf("override helper inspection = %#v", override)
	}
	enabledService := parseLinuxHelperEffectiveInspection(
		strings.Replace(
			service,
			"UnitFileState=static",
			"UnitFileState=enabled",
			1,
		),
		socket,
		"1000",
	)
	if enabledService.IdentityExact {
		t.Fatalf("enabled helper service inspection = %#v", enabledService)
	}
}

func TestHelperUninstallRefusesChangedComponentBeforeEffects(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformWindows,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	observed := exactTestObservations(request)
	plan, err := hostbootstrap.Reconcile(request, observed)
	if err != nil {
		t.Fatal(err)
	}
	state := newDiskState("/state", &fakeSecureFS{})
	if err := state.SaveOperation(request, plan); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveMarker(resourceMarker{
		SchemaVersion:  hostbootstrap.SchemaVersion,
		Kind:           hostbootstrap.ResourceHelper,
		OperationID:    request.OperationID,
		IdentitySHA256: request.Binding.Helper.SHA256,
	}); err != nil {
		t.Fatal(err)
	}
	runner := &fixedCommandRunner{}
	platform := &runtimePlatform{
		state:      state,
		target:     request.Binding.Target,
		definition: windowsNativeDefinition(),
		runner:     runner,
		helperComponents: fakeHelperComponents{
			executable: helperComponentState{Present: true, Exact: true},
			definition: helperComponentState{Present: true, Exact: false},
			service:    helperComponentState{Present: true, Exact: true},
			endpoint:   helperComponentState{Present: true, Exact: true},
		},
	}
	if err := platform.RemoveResource(
		context.Background(),
		hostbootstrap.ResourceHelper,
		request,
	); !errors.Is(err, ErrForeignCollision) {
		t.Fatalf("RemoveResource() error = %v", err)
	}
	if runner.calls != 0 {
		t.Fatal("helper uninstall performed an effect after ownership drift")
	}
}

func TestProductArchiveRejectsUnsafeOrIncompleteTrees(t *testing.T) {
	files := []payloadManifestFile{
		{Path: "assets/config.json", Type: payloadTypeFile, Size: 2, Mode: 0644, SHA256: payloadDigest([]byte("{}"))},
		{Path: "nvpair-tui", Type: payloadTypeFile, Size: 3, Mode: 0755, SHA256: payloadDigest([]byte("tui"))},
		{Path: "nvpair-ui-broker", Type: payloadTypeFile, Size: 6, Mode: 0755, SHA256: payloadDigest([]byte("broker"))},
	}
	valid := buildProductZip(t, []zipFixture{
		{name: "assets/config.json", mode: 0644, body: []byte("{}")},
		{name: "nvpair-tui", mode: 0755, body: []byte("tui")},
		{name: "nvpair-ui-broker", mode: 0755, body: []byte("broker")},
	})
	if err := validateProductArchiveBytes(valid, files); err != nil {
		t.Fatalf("valid archive error = %v", err)
	}
	tests := []struct {
		name    string
		archive []byte
		files   []payloadManifestFile
	}{
		{name: "traversal", archive: buildProductZip(t, []zipFixture{{name: "../escape", mode: 0644, body: []byte("x")}}), files: files},
		{name: "case collision", archive: buildProductZip(t, []zipFixture{{name: "A", mode: 0644, body: []byte("x")}, {name: "a", mode: 0644, body: []byte("x")}}), files: files},
		{name: "symlink", archive: buildProductZip(t, []zipFixture{{name: "nvpair-tui", mode: fs.ModeSymlink | 0777, body: []byte("target")}}), files: files},
		{name: "extra", archive: appendProductZipFile(t, valid, "extra", []byte("x")), files: files},
		{name: "missing", archive: buildProductZip(t, []zipFixture{{name: "nvpair-tui", mode: 0755, body: []byte("tui")}}), files: files},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateProductArchiveBytes(test.archive, test.files); !errors.Is(err, ErrPayloadInvalid) {
				t.Fatalf("archive error = %v", err)
			}
		})
	}
}

func TestProductArchiveInstallsAndVerifiesCompleteTree(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("installs a Windows product tree; Unix hosts require root-owned state directories")
	}
	files := []payloadManifestFile{
		{Path: "nvpair-tui", Type: payloadTypeFile, Size: 3, Mode: 0755, SHA256: payloadDigest([]byte("tui"))},
		{Path: "nvpair-ui-broker", Type: payloadTypeFile, Size: 6, Mode: 0755, SHA256: payloadDigest([]byte("broker"))},
	}
	archive := buildProductZip(t, []zipFixture{
		{name: "nvpair-tui", mode: 0755, body: []byte("tui")},
		{name: "nvpair-ui-broker", mode: 0755, body: []byte("broker")},
	})
	sourcePath := filepath.Join(t.TempDir(), "payload.zip")
	filesystem := fakePayloadFS{files: map[string][]byte{sourcePath: archive}}
	source := payloadSource{
		Path:      sourcePath,
		ByteCount: int64(len(archive)),
		SHA256:    payloadDigest(archive),
		Files:     files,
	}
	destination := filepath.Join(t.TempDir(), "product")
	if err := installProductArchive(
		filesystem,
		source,
		destination,
		"",
		hostbootstrap.PlatformWindows,
		"abababababababababababababababab",
		nil,
	); err != nil {
		t.Fatalf("installProductArchive() error = %v", err)
	}
	present, exact, err := verifyInstalledProductTree(
		destination,
		files,
		hostbootstrap.PlatformWindows,
	)
	if err != nil || !present || !exact {
		t.Fatalf("installed tree present=%t exact=%t error=%v", present, exact, err)
	}
	if err := os.Remove(filepath.Join(destination, "nvpair-ui-broker")); err != nil {
		t.Fatal(err)
	}
	present, exact, partialExact, err := inspectInstalledProductTree(
		destination,
		files,
		hostbootstrap.PlatformWindows,
	)
	if err != nil || !present || exact || !partialExact {
		t.Fatalf("partial tree present=%t exact=%t partial=%t error=%v", present, exact, partialExact, err)
	}
	if err := installProductArchive(
		filesystem,
		source,
		destination,
		source.SHA256,
		hostbootstrap.PlatformWindows,
		"cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd",
		files,
	); err != nil {
		t.Fatalf("partial reinstall error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(destination, "extra"), []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	_, exact, err = verifyInstalledProductTree(destination, files, hostbootstrap.PlatformWindows)
	if err != nil || exact {
		t.Fatalf("extra file exact=%t error=%v", exact, err)
	}
}

type zipFixture struct {
	name string
	mode fs.FileMode
	body []byte
}

func buildProductZip(t *testing.T, fixtures []zipFixture) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, fixture := range fixtures {
		header := &zip.FileHeader{Name: fixture.name, Method: zip.Store}
		header.SetMode(fixture.mode)
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(fixture.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func appendProductZipFile(t *testing.T, original []byte, name string, body []byte) []byte {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(original), int64(len(original)))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []zipFixture
	for _, file := range reader.File {
		stream, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(stream)
		if err != nil {
			t.Fatal(err)
		}
		_ = stream.Close()
		fixtures = append(fixtures, zipFixture{name: file.Name, mode: file.Mode(), body: content})
	}
	fixtures = append(fixtures, zipFixture{name: name, mode: 0644, body: body})
	return buildProductZip(t, fixtures)
}
