// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"nvpair-shared/hostbootstrap"
)

var ErrFirewallUnavailable = errors.New("no supported active firewall manager is available")

const ownedKeyCommentPrefix = "NVIDIA-PAIR-OWNED:"

type commandRunner interface {
	Run(context.Context, commandSpec) (string, error)
}

type boundedCommandRunner struct{}

func (boundedCommandRunner) Run(
	parent context.Context,
	spec commandSpec,
) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, spec.Path, spec.Args...)
	output := &boundedOutput{limit: 64 << 10}
	command.Stdout = output
	command.Stderr = output
	command.WaitDelay = time.Second
	err := command.Run()
	if output.exceeded {
		return "", errors.New("native command output exceeded its fixed limit")
	}
	return strings.TrimSpace(output.buffer.String()), err
}

type boundedOutput struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (output *boundedOutput) Write(data []byte) (int, error) {
	if output.buffer.Len()+len(data) > output.limit {
		output.exceeded = true
		return 0, errors.New("native command output exceeded its fixed limit")
	}
	return output.buffer.Write(data)
}

type runtimePlatform struct {
	target             hostbootstrap.Target
	distro             string
	definition         nativeDefinition
	owner              nativeOwnerManifest
	state              *diskState
	runner             commandRunner
	executable         string
	payloadFS          payloadFileSystem
	payload            payloadBundle
	internalEffectHook func(effectPoint, string) error
	processes          processInspector
	firewall           windowsFirewallAPI
	helperComponents   helperComponentInspector
	windowsServices    windowsServiceAPI
}

type processInspector interface {
	Snapshot() ([]processRecord, error)
}

type processRecord struct {
	PID  uint32
	PPID uint32
	Path string
}

type nativeProbe struct {
	Available      bool
	Present        bool
	Exact          bool
	NotApplicable  bool
	Unavailable    bool
	IdentitySHA256 string
	PriorExact     bool
	IdentityExact  bool
	LifecycleExact bool
}

type nativeEffect struct {
	ID              string
	Satisfied       func() (bool, error)
	PrefixSatisfied func() (bool, error)
	Apply           func() error
	ReplaySafe      bool
}

func (platform *runtimePlatform) runNativeEffect(
	effectID string,
	effect func() error,
) error {
	pending, err := platform.state.LoadPending()
	if errors.Is(err, ErrStateMissing) {
		uninstall, uninstallErr := platform.state.LoadUninstallPending()
		if errors.Is(uninstallErr, ErrStateMissing) {
			return effect()
		}
		if uninstallErr != nil {
			return uninstallErr
		}
		if uninstall.EffectID == effectID &&
			uninstall.EffectComplete {
			return effect()
		}
		if uninstall.EffectID != effectID ||
			uninstall.EffectComplete {
			next := uninstall
			next.EffectID = effectID
			next.EffectComplete = false
			next.Revision++
			if err := platform.state.UpdateUninstallPending(
				uninstall,
				next,
			); err != nil {
				return err
			}
			uninstall = next
		}
		if platform.internalEffectHook != nil {
			if err := platform.internalEffectHook(
				effectBefore,
				effectID,
			); err != nil {
				return err
			}
		}
		if err := effect(); err != nil {
			return err
		}
		complete := uninstall
		complete.EffectComplete = true
		complete.Revision++
		if err := platform.state.UpdateUninstallPending(
			uninstall,
			complete,
		); err != nil {
			return err
		}
		if platform.internalEffectHook != nil {
			return platform.internalEffectHook(effectAfter, effectID)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if pending.EffectID != effectID || pending.EffectComplete {
		next := pending
		next.EffectID = effectID
		next.EffectComplete = false
		next.Revision++
		if err := platform.state.UpdatePending(pending, next); err != nil {
			return err
		}
		pending = next
	}
	if platform.internalEffectHook != nil {
		if err := platform.internalEffectHook(effectBefore, effectID); err != nil {
			return err
		}
	}
	if err := effect(); err != nil {
		return err
	}
	complete := pending
	complete.EffectComplete = true
	complete.Revision++
	if err := platform.state.UpdatePending(pending, complete); err != nil {
		return err
	}
	if platform.internalEffectHook != nil {
		if err := platform.internalEffectHook(effectAfter, effectID); err != nil {
			return err
		}
	}
	return nil
}

func (platform *runtimePlatform) resumeNativeEffectSuffix(
	pending pendingEffect,
	effects []nativeEffect,
) error {
	start := 0
	if pending.EffectID != "" {
		recorded := -1
		for index, effect := range effects {
			if effect.ID == pending.EffectID {
				if recorded != -1 {
					return ErrStateIdentity
				}
				recorded = index
			}
		}
		if recorded == -1 {
			return ErrStateIdentity
		}
		for index := 0; index < recorded; index++ {
			effect := effects[index]
			postcondition := effect.PrefixSatisfied
			if postcondition == nil {
				postcondition = effect.Satisfied
			}
			if postcondition == nil {
				return ErrStateIdentity
			}
			satisfied, err := postcondition()
			if err != nil {
				return err
			}
			if satisfied {
				continue
			}
			if !effect.ReplaySafe {
				return ErrStateIdentity
			}
			if err := executeReplaySafePrefix(effect); err != nil {
				return err
			}
		}
		start = recorded
		if pending.EffectComplete {
			postcondition := effects[recorded].PrefixSatisfied
			if postcondition == nil {
				postcondition = effects[recorded].Satisfied
			}
			if postcondition == nil {
				return ErrStateIdentity
			}
			satisfied, err := postcondition()
			if err != nil {
				return err
			}
			if !satisfied {
				return ErrStateIdentity
			}
			start++
		} else {
			if effects[recorded].Satisfied == nil &&
				!effects[recorded].ReplaySafe {
				return ErrStateIdentity
			}
		}
	}
	for _, effect := range effects[start:] {
		if err := platform.executeNativeEffect(effect); err != nil {
			return err
		}
	}
	return nil
}

func executeReplaySafePrefix(effect nativeEffect) error {
	if !effect.ReplaySafe || effect.Apply == nil {
		return ErrStateIdentity
	}
	if err := effect.Apply(); err != nil {
		return err
	}
	postcondition := effect.PrefixSatisfied
	if postcondition == nil {
		postcondition = effect.Satisfied
	}
	if postcondition == nil {
		return ErrStateIdentity
	}
	satisfied, err := postcondition()
	if err != nil {
		return err
	}
	if !satisfied {
		return ErrVerification
	}
	return nil
}

func (platform *runtimePlatform) executeNativeEffect(effect nativeEffect) error {
	if effect.ID == "" || effect.Apply == nil {
		return ErrStateIdentity
	}
	if effect.Satisfied != nil {
		satisfied, err := effect.Satisfied()
		if err != nil {
			return err
		}
		if satisfied {
			applyPending, applyErr := platform.state.LoadPending()
			uninstallPending, uninstallErr :=
				platform.state.LoadUninstallPending()
			if applyErr != nil &&
				!errors.Is(applyErr, ErrStateMissing) {
				return applyErr
			}
			if uninstallErr != nil &&
				!errors.Is(uninstallErr, ErrStateMissing) {
				return uninstallErr
			}
			applyIncomplete := applyErr == nil &&
				applyPending.EffectID == effect.ID &&
				!applyPending.EffectComplete
			uninstallIncomplete := uninstallErr == nil &&
				uninstallPending.EffectID == effect.ID &&
				!uninstallPending.EffectComplete
			if !applyIncomplete && !uninstallIncomplete {
				return nil
			}
		}
	}
	return platform.runNativeEffect(effect.ID, func() error {
		if effect.Satisfied != nil {
			satisfied, err := effect.Satisfied()
			if err != nil {
				return err
			}
			if satisfied {
				return nil
			}
		}
		if err := effect.Apply(); err != nil {
			return err
		}
		postcondition := effect.Satisfied
		if postcondition == nil {
			postcondition = effect.PrefixSatisfied
		}
		if postcondition == nil {
			return ErrStateIdentity
		}
		satisfied, err := postcondition()
		if err != nil {
			return err
		}
		if !satisfied {
			return ErrVerification
		}
		return nil
	})
}

func (platform *runtimePlatform) runCommands(
	ctx context.Context,
	prefix string,
	commands []commandSpec,
	request hostbootstrap.Request,
) error {
	for _, effect := range platform.commandEffects(
		ctx,
		prefix,
		commands,
		request,
	) {
		if err := platform.executeNativeEffect(effect); err != nil {
			return err
		}
	}
	return nil
}

func (platform *runtimePlatform) commandEffects(
	ctx context.Context,
	prefix string,
	commands []commandSpec,
	request hostbootstrap.Request,
) []nativeEffect {
	effects := make([]nativeEffect, 0, len(commands))
	for index, command := range commands {
		command := resolveOwnerCommand(command, request)
		effectID := fmt.Sprintf("%s:command:%d", prefix, index)
		replaySafe := prefix == "apply:helper" &&
			platform.target.Platform == hostbootstrap.PlatformLinux &&
			index == 0
		if platform.target.Platform == hostbootstrap.PlatformLinux &&
			command.Path == "/usr/bin/systemctl" &&
			reflect.DeepEqual(command.Args, []string{"daemon-reload"}) {
			replaySafe = true
		}
		effect := nativeEffect{
			ID:         effectID,
			ReplaySafe: replaySafe,
			Apply: func() error {
				_, err := platform.runner.Run(ctx, command)
				return err
			},
		}
		if !replaySafe {
			effect.Satisfied = func() (bool, error) {
				return platform.commandEffectSatisfied(
					ctx,
					prefix,
					index,
					request,
				)
			}
		} else {
			effect.PrefixSatisfied = func() (bool, error) {
				if prefix == "uninstall:helper" {
					return platform.commandEffectSatisfied(
						ctx,
						prefix,
						index,
						request,
					)
				}
				return true, nil
			}
		}
		effects = append(effects, effect)
	}
	return effects
}

func (platform *runtimePlatform) windowsSSHRepairEffects(
	ctx context.Context,
) ([]nativeEffect, error) {
	apply := platform.definition.Apply[hostbootstrap.ResourceSSHService]
	remove := platform.definition.Remove[hostbootstrap.ResourceSSHService]
	if len(apply) != 3 || len(remove) != 2 {
		return nil, ErrStateIdentity
	}
	inspect := func() (windowsSSHInspection, error) {
		return inspectWindowsSSHNative(
			ctx,
			platform.runner,
			platform.windowsServices,
		)
	}
	run := func(command commandSpec) func() error {
		return func() error {
			_, err := platform.runner.Run(ctx, command)
			return err
		}
	}
	return []nativeEffect{
		{
			ID: "ssh:capability-remove-for-repair",
			Satisfied: func() (bool, error) {
				inspection, err := inspect()
				if err != nil {
					return false, err
				}
				return !(inspection.Capability.Exact &&
					!inspection.Service.Present), nil
			},
			Apply: run(remove[1]),
		},
		{
			ID: "ssh:capability-add",
			Satisfied: func() (bool, error) {
				inspection, err := inspect()
				if err != nil {
					return false, err
				}
				return inspection.Capability.Exact &&
					inspection.Service.Present, nil
			},
			Apply: run(apply[0]),
		},
		{
			ID: "ssh:service-config",
			Satisfied: func() (bool, error) {
				inspection, err := inspect()
				if err != nil {
					return false, err
				}
				if inspection.Service.Present &&
					!inspection.Service.Exact {
					return false, ErrForeignCollision
				}
				return inspection.Service.Exact &&
					inspection.Enabled, nil
			},
			Apply: run(apply[1]),
		},
		{
			ID: "ssh:service-start",
			Satisfied: func() (bool, error) {
				inspection, err := inspect()
				if err != nil {
					return false, err
				}
				return inspection.Exact(), nil
			},
			Apply: run(apply[2]),
		},
	}, nil
}

func (platform *runtimePlatform) commandEffectSatisfied(
	ctx context.Context,
	prefix string,
	index int,
	request hostbootstrap.Request,
) (bool, error) {
	switch prefix {
	case "apply:ssh-service":
		switch platform.target.Platform {
		case hostbootstrap.PlatformWindows:
			inspection, err := inspectWindowsSSHNative(
				ctx,
				platform.runner,
				platform.windowsServices,
			)
			if err != nil {
				return false, err
			}
			if inspection.Present() && !inspection.IdentityExact() {
				return false, ErrForeignCollision
			}
			switch index {
			case 0:
				return inspection.Capability.Exact, nil
			case 1:
				return inspection.Service.Exact &&
					inspection.Enabled, nil
			case 2:
				return inspection.Exact(), nil
			}
		case hostbootstrap.PlatformDarwin:
			_, exact, _, err := platform.probeEndpointResource(
				ctx,
				hostbootstrap.ResourceSSHService,
			)
			return exact, err
		case hostbootstrap.PlatformLinux:
			inspection, _, err := inspectLinuxSSHNative(
				ctx,
				platform.runner,
			)
			if err != nil {
				return false, err
			}
			if index == 0 {
				return inspection.PackageInstalled &&
					inspection.PackageVerified, nil
			}
			if index == 1 {
				return inspection.Exact(), nil
			}
		}
	case "apply:helper":
		switch platform.target.Platform {
		case hostbootstrap.PlatformWindows:
			if platform.windowsServices == nil {
				return false, ErrVerification
			}
			service, err := platform.windowsServices.Inspect("nvpair-host-helper")
			if errors.Is(err, ErrStateMissing) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			definitionExact := strings.EqualFold(
				service.Executable,
				windowsHelperPath,
			) &&
				reflect.DeepEqual(service.Arguments, []string{"service"}) &&
				service.ServiceType == 0x10 &&
				service.StartType == 2 &&
				service.Account == "LocalSystem"
			if index == 0 {
				if !definitionExact {
					return false, ErrForeignCollision
				}
				return definitionExact, nil
			}
			if !definitionExact {
				return false, ErrForeignCollision
			}
			if index == 1 {
				return definitionExact && service.State == 4, nil
			}
		case hostbootstrap.PlatformDarwin:
			output, err := platform.runner.Run(ctx, commandSpec{
				Path: "/bin/launchctl",
				Args: []string{"print", "system/com.nvidia.nvpair.host-helper"},
			})
			if err != nil {
				if launchdJobNotLoaded(err) {
					return false, nil
				}
				return false, err
			}
			present, identityExact, lifecycleExact, valid := parseLaunchdJob(
				output,
				"com.nvidia.nvpair.host-helper",
				darwinHelperPath,
				[]string{darwinHelperPath, "service"},
				"root",
			)
			if !valid {
				return false, ErrVerification
			}
			if index == 0 {
				if present && !identityExact {
					return false, ErrForeignCollision
				}
				return present && identityExact, nil
			}
			if !present || !identityExact {
				return false, ErrForeignCollision
			}
			return lifecycleExact, nil
		case hostbootstrap.PlatformLinux:
			if index == 0 {
				return false, nil
			}
			if index == 2 {
				output, err := platform.runner.Run(ctx, commandSpec{
					Path: "/usr/bin/systemctl",
					Args: []string{
						"show",
						"nvpair-host-helper.service",
						"--property=LoadState",
						"--property=ActiveState",
					},
				})
				if err != nil {
					return false, err
				}
				properties := parseSystemdProperties(output)
				return properties["LoadState"] == "loaded" &&
					properties["ActiveState"] == "active", nil
			}
			output, err := platform.runner.Run(ctx, commandSpec{
				Path: "/usr/bin/systemctl",
				Args: []string{
					"show",
					"nvpair-host-helper.socket",
					"--property=LoadState",
					"--property=ActiveState",
					"--property=UnitFileState",
				},
			})
			if err != nil {
				return false, err
			}
			return parseSystemdServiceState(output).Exact(), nil
		}
	case "uninstall:helper":
		inspection, err := platform.InspectHelper(ctx, request)
		if err != nil {
			return false, err
		}
		if (inspection.Executable.Present &&
			(!inspection.Executable.Exact ||
				inspection.ExecutableDigest !=
					request.Binding.Helper.SHA256)) ||
			!inspection.DefinitionCompatible ||
			(inspection.Endpoint.Present &&
				!inspection.Endpoint.Exact) {
			return false, ErrForeignCollision
		}
		switch platform.target.Platform {
		case hostbootstrap.PlatformWindows:
			switch index {
			case 0:
				return (!inspection.Definition.Present ||
					inspection.Definition.Exact) &&
					(!inspection.Service.Present ||
						!inspection.Service.Exact) &&
					!inspection.Endpoint.Present, nil
			case 1:
				return !inspection.Definition.Present &&
					!inspection.Service.Present &&
					!inspection.Endpoint.Present, nil
			}
		case hostbootstrap.PlatformDarwin:
			if index == 0 {
				return !inspection.Service.Present &&
					!inspection.Endpoint.Present, nil
			}
		case hostbootstrap.PlatformLinux:
			if index == 0 {
				return !inspection.Service.Exact &&
					!inspection.Endpoint.Present, nil
			}
			if index == 1 {
				return !inspection.Definition.Present &&
					!inspection.Service.Present &&
					!inspection.Endpoint.Present, nil
			}
		}
	case "uninstall:ssh-service":
		probe, err := platform.probe(
			ctx,
			hostbootstrap.ResourceSSHService,
			request,
		)
		if err != nil {
			return false, err
		}
		if probe.Present && !probe.IdentityExact {
			return false, ErrForeignCollision
		}
		switch platform.target.Platform {
		case hostbootstrap.PlatformWindows:
			if index == 0 {
				inspection, err := inspectWindowsSSHNative(
					ctx,
					platform.runner,
					platform.windowsServices,
				)
				if err != nil {
					return false, err
				}
				return !inspection.Service.Present ||
					!inspection.Lifecycle.Exact, nil
			}
			return !probe.Present, nil
		case hostbootstrap.PlatformDarwin:
			return !probe.Present, nil
		case hostbootstrap.PlatformLinux:
			if index == 0 {
				return !probe.LifecycleExact, nil
			}
			return !probe.Present, nil
		}
	case "role:start", "role:repair-stop", "role:repair-remove",
		"role:stop", "role:remove":
		return platform.roleCommandSatisfied(ctx, prefix, index, request)
	}
	return false, nil
}

func (platform *runtimePlatform) roleCommandSatisfied(
	ctx context.Context,
	prefix string,
	index int,
	request hostbootstrap.Request,
) (bool, error) {
	switch platform.target.Platform {
	case hostbootstrap.PlatformWindows:
		if platform.windowsServices == nil {
			return false, ErrVerification
		}
		service, err := platform.windowsServices.Inspect(
			platform.owner.HeadlessIdentity,
		)
		missing := errors.Is(err, ErrStateMissing)
		if err != nil && !missing {
			return false, err
		}
		definitionExact := missing ||
			(strings.EqualFold(
				service.Executable,
				platform.owner.TUIPath,
			) &&
				reflect.DeepEqual(
					service.Arguments,
					platform.owner.OwnerCommand[1:],
				) &&
				service.ServiceType == 0x10 &&
				service.StartType == 2 &&
				service.Account == "LocalSystem")
		if !definitionExact {
			return false, ErrForeignCollision
		}
		switch prefix {
		case "role:start":
			if missing {
				return false, nil
			}
			if index == 0 {
				return true, nil
			}
			return service.State == 4, nil
		case "role:stop", "role:repair-stop":
			return missing || service.State != 4, nil
		case "role:remove", "role:repair-remove":
			return missing, nil
		}
	case hostbootstrap.PlatformDarwin:
		output, err := platform.runner.Run(ctx, commandSpec{
			Path: "/bin/launchctl",
			Args: []string{"print", "system/" + platform.owner.HeadlessIdentity},
		})
		missing := launchdJobNotLoaded(err)
		if err != nil && !missing {
			return false, err
		}
		if missing {
			switch prefix {
			case "role:start":
				return false, nil
			case "role:stop", "role:remove", "role:repair-stop",
				"role:repair-remove":
				return true, nil
			}
		}
		present, identityExact, lifecycleExact, valid := parseLaunchdJob(
			output,
			platform.owner.HeadlessIdentity,
			platform.owner.TUIPath,
			platform.owner.OwnerCommand,
			request.Binding.Account.Name,
		)
		if !valid {
			return false, ErrVerification
		}
		identityExact = present && identityExact
		if !identityExact {
			return false, ErrForeignCollision
		}
		switch prefix {
		case "role:start":
			if missing {
				return false, nil
			}
			if index == 0 {
				return true, nil
			}
			return lifecycleExact, nil
		case "role:stop", "role:remove", "role:repair-stop",
			"role:repair-remove":
			return missing, nil
		}
	case hostbootstrap.PlatformLinux:
		present, identityExact, activeExact, err :=
			platform.inspectLinuxHeadlessOwner(ctx, request)
		if err != nil {
			return false, err
		}
		if !identityExact {
			return false, ErrForeignCollision
		}
		switch prefix {
		case "role:start":
			return present && activeExact, nil
		case "role:stop", "role:repair-stop":
			return !present || !activeExact, nil
		case "role:remove", "role:repair-remove":
			return !present, nil
		}
	}
	return false, nil
}

type helperComponentState struct {
	Present bool
	Exact   bool
}

type windowsServiceInfo struct {
	Executable  string
	Arguments   []string
	ServiceType uint32
	StartType   uint32
	Account     string
	State       uint32
}

type windowsSSHInspection struct {
	Capability helperComponentState
	Service    helperComponentState
	Lifecycle  helperComponentState
	Enabled    bool
}

func (inspection windowsSSHInspection) Present() bool {
	return inspection.Capability.Present || inspection.Service.Present
}

func (inspection windowsSSHInspection) IdentityExact() bool {
	return (!inspection.Capability.Present || inspection.Capability.Exact) &&
		(!inspection.Service.Present || inspection.Service.Exact)
}

func (inspection windowsSSHInspection) Exact() bool {
	return inspection.Capability.Exact &&
		inspection.Service.Exact &&
		inspection.Lifecycle.Exact &&
		inspection.Enabled
}

func inspectWindowsSSHNative(
	ctx context.Context,
	runner commandRunner,
	services windowsServiceAPI,
) (windowsSSHInspection, error) {
	var inspection windowsSSHInspection
	output, err := runner.Run(ctx, commandSpec{
		Path: `C:\Windows\System32\dism.exe`,
		Args: []string{
			"/Online",
			"/Get-CapabilityInfo",
			"/CapabilityName:OpenSSH.Server~~~~0.0.1.0",
			"/English",
		},
	})
	if err != nil {
		return inspection, err
	}
	stateLines := 0
	for _, line := range strings.Split(
		strings.ReplaceAll(output, "\r\n", "\n"),
		"\n",
	) {
		if strings.HasPrefix(strings.TrimSpace(line), "State :") {
			stateLines++
			state := strings.TrimSpace(
				strings.TrimPrefix(strings.TrimSpace(line), "State :"),
			)
			if state == "Installed" {
				inspection.Capability = helperComponentState{
					Present: true,
					Exact:   true,
				}
			} else if state != "Not Present" {
				inspection.Capability = helperComponentState{Present: true}
			}
		}
	}
	if stateLines != 1 {
		return inspection, ErrVerification
	}
	if services == nil {
		return inspection, ErrVerification
	}
	service, err := services.Inspect("sshd")
	if errors.Is(err, ErrStateMissing) {
		return inspection, nil
	}
	if err != nil {
		return inspection, err
	}
	inspection.Service = helperComponentState{
		Present: true,
		Exact: strings.EqualFold(service.Executable, windowsSSHDPath) &&
			len(service.Arguments) == 0 &&
			service.ServiceType == 0x10 &&
			service.Account == "LocalSystem",
	}
	inspection.Lifecycle = helperComponentState{
		Present: true,
		Exact:   service.State == 4,
	}
	inspection.Enabled = service.StartType == 2
	return inspection, nil
}

type nativeServiceState struct {
	Present bool
	Running bool
	Enabled bool
}

type remoteLoginState int

const (
	remoteLoginUnavailable remoteLoginState = iota
	remoteLoginOff
	remoteLoginOn
)

func parseRemoteLoginState(output string) remoteLoginState {
	switch strings.TrimSpace(output) {
	case "Remote Login: On":
		return remoteLoginOn
	case "Remote Login: Off":
		return remoteLoginOff
	default:
		return remoteLoginUnavailable
	}
}

type linuxSSHInspection struct {
	PackageInstalled bool
	PackageVerified  bool
	UnitLoaded       bool
	Running          bool
	Enabled          bool
	FragmentExact    bool
	NoDropIns        bool
	ExecStartExact   bool
}

func (inspection linuxSSHInspection) Exact() bool {
	return inspection.IdentityExact() &&
		inspection.LifecycleExact()
}

func (inspection linuxSSHInspection) IdentityExact() bool {
	return inspection.PackageInstalled &&
		inspection.PackageVerified &&
		inspection.UnitLoaded &&
		inspection.FragmentExact &&
		inspection.NoDropIns &&
		inspection.ExecStartExact
}

func (inspection linuxSSHInspection) LifecycleExact() bool {
	return inspection.Running && inspection.Enabled
}

func parseLinuxSSHInspection(
	packageStatus string,
	packageVerify string,
	service string,
) linuxSSHInspection {
	properties := make(map[string]string)
	for _, line := range strings.Split(
		strings.ReplaceAll(service, "\r\n", "\n"),
		"\n",
	) {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if found {
			properties[key] = value
		}
	}
	return linuxSSHInspection{
		PackageInstalled: strings.TrimSpace(packageStatus) ==
			"Status: install ok installed",
		PackageVerified: strings.TrimSpace(packageVerify) == "",
		UnitLoaded:      properties["LoadState"] == "loaded",
		Running:         properties["ActiveState"] == "active",
		Enabled:         properties["UnitFileState"] == "enabled",
		FragmentExact: properties["FragmentPath"] ==
			"/lib/systemd/system/ssh.service",
		NoDropIns:      properties["DropInPaths"] == "",
		ExecStartExact: linuxSSHExecStartExact(properties["ExecStart"]),
	}
}

func linuxSSHExecStartExact(value string) bool {
	return strings.Count(value, "{ path=") == 1 &&
		strings.HasPrefix(
			value,
			"{ path=/usr/sbin/sshd ; argv[]=/usr/sbin/sshd -D $SSHD_OPTS ;",
		) &&
		strings.HasSuffix(value, "}")
}

func classifyLinuxSSHOwnership(
	present bool,
	identityExact bool,
	markerOwned bool,
) hostbootstrap.Ownership {
	if !present {
		return hostbootstrap.OwnershipAbsent
	}
	if !markerOwned || !identityExact {
		return hostbootstrap.OwnershipForeign
	}
	return hostbootstrap.OwnershipOwned
}

func (state nativeServiceState) Exact() bool {
	return state.Present && state.Running && state.Enabled
}

func parseSystemdServiceState(output string) nativeServiceState {
	properties := make(map[string]string)
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if found {
			properties[key] = value
		}
	}
	return nativeServiceState{
		Present: properties["LoadState"] == "loaded",
		Running: properties["ActiveState"] == "active",
		Enabled: properties["UnitFileState"] == "enabled",
	}
}

func parseLaunchdSSHServiceState(
	remoteLogin string,
	disabledServices string,
	service string,
) (nativeServiceState, bool) {
	loginState := parseRemoteLoginState(remoteLogin)
	present, identityExact, lifecycleExact, jobValid := parseLaunchdJob(
		service,
		"com.openssh.sshd",
		"/usr/sbin/sshd",
		[]string{"/usr/sbin/sshd", "-i"},
		"root",
	)
	enabled, disabledValid := parseLaunchdDisabledState(
		disabledServices,
		"com.openssh.sshd",
	)
	valid := loginState != remoteLoginUnavailable &&
		jobValid &&
		disabledValid &&
		(!present || identityExact)
	return nativeServiceState{
		Present: present,
		Running: lifecycleExact,
		Enabled: loginState == remoteLoginOn && enabled,
	}, valid
}

func parseLaunchdDisabledState(
	output string,
	label string,
) (bool, bool) {
	var lines []string
	for _, raw := range strings.Split(
		strings.ReplaceAll(output, "\r\n", "\n"),
		"\n",
	) {
		if line := strings.TrimSpace(raw); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) < 3 ||
		lines[0] != "disabled services = {" ||
		lines[len(lines)-1] != "}" {
		return false, false
	}
	count := 0
	enabled := false
	for _, line := range lines[1 : len(lines)-1] {
		key, value, found := strings.Cut(line, " => ")
		if !found ||
			len(key) < 3 ||
			key[0] != '"' ||
			key[len(key)-1] != '"' ||
			(value != "true" && value != "false") {
			return false, false
		}
		if key != `"`+label+`"` {
			continue
		}
		switch value {
		case "false":
			count++
			enabled = true
		case "true":
			count++
			enabled = false
		}
	}
	return enabled, count == 1
}

type windowsServiceAPI interface {
	Inspect(string) (windowsServiceInfo, error)
}

type helperInspection struct {
	Executable           helperComponentState
	Definition           helperComponentState
	Service              helperComponentState
	Endpoint             helperComponentState
	IdentityExact        bool
	DefinitionCompatible bool
	ExecutableDigest     string
	ServiceState         uint32
}

type helperComponentInspector interface {
	InspectHelper(context.Context, hostbootstrap.Request) (helperInspection, error)
}

func inspectHelperComponents(
	ctx context.Context,
	request hostbootstrap.Request,
	inspector helperComponentInspector,
) (helperInspection, error) {
	return inspector.InspectHelper(ctx, request)
}

func newRuntimeBootstrap(target hostbootstrap.Target) (*Bootstrap, error) {
	actual, distro, err := currentNativeTarget()
	if err != nil {
		return nil, err
	}
	if actual != target {
		return nil, ErrUnsupportedTarget
	}
	definition, err := nativeDefinitionFor(actual, distro)
	if err != nil {
		return nil, err
	}
	owner, err := nativeOwnerManifestFor(actual)
	if err != nil {
		return nil, err
	}
	root, err := nativeStateDirectory(definition.StateDirectory)
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, err
	}
	state := newDiskState(root, nativeSecureFS{})
	runner := boundedCommandRunner{}
	platform := &runtimePlatform{
		target:          actual,
		distro:          distro,
		definition:      definition,
		owner:           owner,
		state:           state,
		runner:          runner,
		executable:      executable,
		payloadFS:       nativePayloadFS{},
		processes:       newNativeProcessInspector(runner),
		firewall:        newNativeWindowsFirewallAPI(),
		windowsServices: newNativeWindowsServiceAPI(),
	}
	return NewBootstrap(platform, state), nil
}

func (platform *runtimePlatform) Target(context.Context) (
	hostbootstrap.Target,
	string,
	error,
) {
	return platform.target, platform.distro, nil
}

func (platform *runtimePlatform) Privileged() (bool, error) {
	return nativePrivileged()
}

func (platform *runtimePlatform) DesktopRunning(
	ctx context.Context,
	request hostbootstrap.Request,
) (bool, error) {
	if platform.processes == nil {
		return false, ErrVerification
	}
	if len(platform.owner.DesktopIdentities) == 0 {
		return false, ErrVerification
	}
	processes, err := platform.processes.Snapshot()
	if err != nil {
		return false, err
	}
	desktop, headless, foreign := classifyBrokerProcessTrees(
		processes,
		platform.target.Platform,
		platform.owner.DesktopIdentities[0],
		platform.owner.TUIPath,
		platform.owner.BrokerPath,
	)
	if foreign || (desktop && headless) {
		return false, ErrRuntimeOwnerActive
	}
	return desktop, nil
}

func classifyBrokerProcessTrees(
	processes []processRecord,
	targetPlatform hostbootstrap.Platform,
	desktopPath string,
	headlessPath string,
	brokerPath string,
) (bool, bool, bool) {
	byPID := make(map[uint32]processRecord, len(processes))
	for _, process := range processes {
		if process.PID != 0 {
			byPID[process.PID] = process
		}
	}
	desktop := false
	headless := false
	foreign := false
	for _, process := range processes {
		if !nativePathEqual(targetPlatform, process.Path, brokerPath) {
			continue
		}
		owner := process.PPID
		visited := make(map[uint32]bool)
		classified := false
		for owner != 0 && !visited[owner] {
			visited[owner] = true
			parent, found := byPID[owner]
			if !found {
				break
			}
			switch {
			case nativePathEqual(targetPlatform, parent.Path, desktopPath):
				desktop = true
				classified = true
			case nativePathEqual(targetPlatform, parent.Path, headlessPath):
				headless = true
				classified = true
			}
			if classified {
				break
			}
			owner = parent.PPID
		}
		if !classified {
			foreign = true
		}
	}
	return desktop, headless, foreign
}

func nativePathEqual(
	targetPlatform hostbootstrap.Platform,
	left string,
	right string,
) bool {
	if targetPlatform == hostbootstrap.PlatformWindows {
		return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func (platform *runtimePlatform) ValidatePending(
	ctx context.Context,
	pending pendingEffect,
	request hostbootstrap.Request,
	current hostbootstrap.Observations,
) error {
	if pending.EffectID == "" {
		if sameResourceObservation(
			current,
			pending.ExpectedPre,
			pending.Resource,
		) {
			return nil
		}
		return ErrPlanChanged
	}
	effects, err := platform.pendingResourceEffects(ctx, pending, request)
	if err != nil {
		return err
	}
	found := false
	for _, effect := range effects {
		if effect.ID == pending.EffectID {
			if found {
				return ErrPlanChanged
			}
			found = true
		}
	}
	if !found {
		return ErrPlanChanged
	}
	if pending.Resource == hostbootstrap.ResourceHelper &&
		helperHeadlessTransitionEffect(pending.EffectID) {
		roleMarker, markerFound, err := platform.state.LoadMarker(
			hostbootstrap.ResourceActiveRole,
		)
		if err != nil || !markerFound ||
			roleMarker.IdentitySHA256 != resourceIdentitySHA256(
				hostbootstrap.ResourceActiveRole,
				request.Binding,
			) {
			return ErrPlanChanged
		}
		present, identity, lifecycle, err :=
			platform.probeHeadlessOwner(ctx, request)
		if err != nil || !present || !identity {
			return ErrPlanChanged
		}
		if pending.EffectID != "helper:stop-headless-wrapper" &&
			pending.EffectID != "helper:start-headless-wrapper" &&
			lifecycle {
			return ErrPlanChanged
		}
	}
	return nil
}

func (platform *runtimePlatform) markerEffect(
	ctx context.Context,
	kind hostbootstrap.ResourceKind,
	request hostbootstrap.Request,
	expected resourceMarker,
	expectedFound bool,
) (nativeEffect, error) {
	marker := resourceMarker{
		SchemaVersion:  hostbootstrap.SchemaVersion,
		Kind:           kind,
		OperationID:    request.OperationID,
		IdentitySHA256: resourceIdentitySHA256(kind, request.Binding),
	}
	if kind == hostbootstrap.ResourceProduct {
		marker.Files = append(
			[]payloadManifestFile(nil),
			platform.payload.Product.Files...,
		)
	} else if kind == hostbootstrap.ResourceAuthorizedKey {
		marker.AuthorizedKeyLine = desiredAuthorizedKeyLine(request)
	} else if kind == hostbootstrap.ResourceFirewall &&
		platform.target.Platform == hostbootstrap.PlatformLinux {
		families, err := linuxFirewallOwnedFamiliesNative(request)
		if err != nil {
			return nativeEffect{}, err
		}
		marker.FirewallFamilies = families
	}
	return nativeEffect{
		ID: "marker:" + string(kind),
		Satisfied: func() (bool, error) {
			existing, found, err := platform.state.LoadMarker(kind)
			if err != nil {
				return false, err
			}
			if found && reflect.DeepEqual(existing, marker) {
				return true, nil
			}
			if found != expectedFound ||
				(found && !reflect.DeepEqual(existing, expected)) {
				return false, ErrStateIdentity
			}
			return false, nil
		},
		Apply: func() error {
			probe, err := platform.probe(ctx, kind, request)
			if err != nil {
				return err
			}
			if !probe.Available ||
				!probe.Present ||
				!probe.Exact ||
				probe.IdentitySHA256 != marker.IdentitySHA256 {
				return ErrVerification
			}
			return platform.replaceMarkerCAS(
				expected,
				expectedFound,
				marker,
			)
		},
	}, nil
}

func (platform *runtimePlatform) pendingResourceEffects(
	ctx context.Context,
	pending pendingEffect,
	request hostbootstrap.Request,
) ([]nativeEffect, error) {
	if pending.Resource == hostbootstrap.ResourceActiveRole {
		return platform.pendingRoleEffects(ctx, pending, request)
	}
	existingMarker := pending.PriorMarker
	markerFound := pending.PriorMarkerFound
	var effects []nativeEffect
	switch pending.Resource {
	case hostbootstrap.ResourceSSHService:
		if platform.target.Platform == hostbootstrap.PlatformWindows {
			windowsEffects, err := platform.windowsSSHRepairEffects(ctx)
			if err != nil {
				return nil, err
			}
			effects = append(effects, windowsEffects...)
		} else {
			effects = append(
				effects,
				platform.commandEffects(
					ctx,
					"apply:"+string(hostbootstrap.ResourceSSHService),
					platform.definition.Apply[hostbootstrap.ResourceSSHService],
					request,
				)...,
			)
		}
	case hostbootstrap.ResourceFirewall:
		switch platform.target.Platform {
		case hostbootstrap.PlatformWindows:
			effects = append(effects, nativeEffect{
				ID: "firewall:windows-add",
				Satisfied: func() (bool, error) {
					probe := inspectWindowsFirewallAPI(platform.firewall)
					if probe.Unavailable {
						return false, ErrFirewallUnavailable
					}
					if probe.Present && !probe.Exact {
						return false, ErrForeignCollision
					}
					return probe.Exact, nil
				},
				Apply: func() error {
					return addWindowsFirewallRule(platform.firewall)
				},
			})
		case hostbootstrap.PlatformLinux:
			firewallEffects, err := linuxFirewallNativeEffects(
				ctx,
				platform.runner,
				request,
			)
			if err != nil {
				return nil, err
			}
			effects = append(effects, firewallEffects...)
		default:
			return nil, ErrUnsupportedTarget
		}
	case hostbootstrap.ResourceAuthorizedKey:
		effects = append(effects, nativeEffect{
			ID: "authorized-key:publish",
			Satisfied: func() (bool, error) {
				priorLine := ""
				if markerFound {
					priorLine = existingMarker.AuthorizedKeyLine
				}
				probe, err := probeAuthorizedKeyWithPrior(
					request,
					priorLine,
				)
				if err != nil {
					return false, err
				}
				if probe.Present &&
					probe.Exact &&
					probe.IdentitySHA256 ==
						request.Binding.ControllerKey.FingerprintSHA256 {
					return true, nil
				}
				if !probe.Present {
					return false, nil
				}
				if markerFound &&
					probe.PriorExact &&
					probe.IdentitySHA256 ==
						existingMarker.IdentitySHA256 {
					return false, nil
				}
				return false, ErrForeignCollision
			},
			Apply: func() error {
				return applyAuthorizedKey(
					request,
					existingMarker,
					markerFound,
				)
			},
		})
	case hostbootstrap.ResourceHelper:
		headlessWrapperTransition := false
		if markerFound &&
			existingMarker.IdentitySHA256 != request.Binding.Helper.SHA256 {
			if platform.target.Platform ==
				hostbootstrap.PlatformWindows &&
				len(pending.ExpectedPre.RuntimeOwners) == 1 &&
				pending.ExpectedPre.RuntimeOwners[0].Ownership ==
					hostbootstrap.OwnershipOwned &&
				pending.ExpectedPre.RuntimeOwners[0].Owner ==
					hostbootstrap.RoleHeadless {
				roleMarker, found, err := platform.state.LoadMarker(
					hostbootstrap.ResourceActiveRole,
				)
				if err != nil || !found ||
					platform.validateMarkerHistory(
						hostbootstrap.ResourceActiveRole,
						roleMarker,
					) != nil {
					return nil, ErrStateIdentity
				}
				headlessWrapperTransition = true
				effects = append(effects, nativeEffect{
					ID: "helper:stop-headless-wrapper",
					Satisfied: func() (bool, error) {
						service, err := platform.windowsServices.Inspect(
							platform.owner.HeadlessIdentity,
						)
						if errors.Is(err, ErrStateMissing) {
							return true, nil
						}
						return service.State == 1, err
					},
					Apply: func() error {
						_, err := platform.runner.Run(
							ctx,
							platform.owner.Stop[0],
						)
						return err
					},
				})
			}
			removeCommands :=
				platform.definition.Remove[hostbootstrap.ResourceHelper]
			if len(removeCommands) == 0 {
				return nil, ErrStateIdentity
			}
			stopCommand := resolveOwnerCommand(removeCommands[0], request)
			effects = append(effects, nativeEffect{
				ID: "helper:stop-prior",
				Satisfied: func() (bool, error) {
					inspection, owned, err :=
						platform.inspectOwnedHelper(ctx, request)
					if err != nil || !owned {
						return false, err
					}
					if platform.target.Platform ==
						hostbootstrap.PlatformWindows {
						return (!inspection.Service.Present ||
							inspection.ServiceState == 1) &&
							!inspection.Endpoint.Present, nil
					}
					return !inspection.Service.Exact &&
						!inspection.Endpoint.Present, nil
				},
				Apply: func() error {
					_, err := platform.runner.Run(ctx, stopCommand)
					return err
				},
			})
		}
		effects = append(effects, nativeEffect{
			ID: "helper:install-executable",
			Satisfied: func() (bool, error) {
				state, digest, err := inspectHelperExecutableNative(
					request.Binding.Helper.Path,
					request.Binding.Helper.SHA256,
				)
				return state.Present &&
					state.Exact &&
					digest == request.Binding.Helper.SHA256, err
			},
			Apply: func() error {
				allowedExisting := ""
				if markerFound {
					allowedExisting = existingMarker.IdentitySHA256
				}
				return installPayloadSource(
					platform.payloadFS,
					platform.payload.Helper,
					request.Binding.Helper.Path,
					allowedExisting,
				)
			},
		})
		principal, err := resolveReviewedPrincipal(
			request.Binding.Account.Name,
		)
		if err != nil {
			return nil, err
		}
		effects = append(effects, nativeEffect{
			ID: "helper:save-principal",
			Satisfied: func() (bool, error) {
				existing, err := platform.state.LoadPrincipal()
				if errors.Is(err, ErrStateMissing) {
					return false, nil
				}
				return existing == principal, err
			},
			Apply: func() error {
				return platform.state.SavePrincipal(principal)
			},
		})
		effects = append(effects, nativeEffect{
			ID:         "helper:ensure-product-parent",
			ReplaySafe: true,
			PrefixSatisfied: func() (bool, error) {
				return rootDefinitionDirectoryExact(
					filepath.Dir(request.Binding.Product.Path),
				)
			},
			Apply: func() error {
				return ensureRootDefinitionDirectory(
					filepath.Dir(request.Binding.Product.Path),
				)
			},
		})
		definitionEffects, err := platform.helperDefinitionEffects(
			request,
			principal,
		)
		if err != nil {
			return nil, err
		}
		effects = append(effects, definitionEffects...)
		effects = append(
			effects,
			platform.commandEffects(
				ctx,
				"apply:"+string(hostbootstrap.ResourceHelper),
				platform.definition.Apply[hostbootstrap.ResourceHelper],
				request,
			)...,
		)
		if headlessWrapperTransition {
			effects = append(effects, nativeEffect{
				ID: "helper:start-headless-wrapper",
				Satisfied: func() (bool, error) {
					present, identity, lifecycle, err :=
						platform.probeHeadlessOwner(ctx, request)
					return present && identity && lifecycle, err
				},
				Apply: func() error {
					if len(platform.owner.Start) != 2 {
						return ErrStateIdentity
					}
					_, err := platform.runner.Run(
						ctx,
						platform.owner.Start[1],
					)
					return err
				},
			})
		}
	case hostbootstrap.ResourceProduct:
		effects = append(effects, nativeEffect{
			ID: "product:install-tree",
			Satisfied: func() (bool, error) {
				present, exact, err := verifyInstalledProductTree(
					request.Binding.Product.Path,
					platform.payload.Product.Files,
					platform.target.Platform,
				)
				if err != nil || !present || !exact {
					return false, err
				}
				return true, nil
			},
			Apply: func() error {
				allowedExisting := ""
				if markerFound {
					allowedExisting = existingMarker.IdentitySHA256
				}
				return installProductArchive(
					platform.payloadFS,
					platform.payload.Product,
					request.Binding.Product.Path,
					allowedExisting,
					platform.target.Platform,
					request.OperationID,
					existingMarker.Files,
				)
			},
		})
		effects = append(effects, nativeEffect{
			ID:         "product:cleanup-recovery",
			ReplaySafe: true,
			Satisfied: func() (bool, error) {
				return productRecoveryClean(
					request.Binding.Product.Path,
					request.OperationID,
				)
			},
			Apply: func() error {
				return cleanupProductRecovery(
					request.Binding.Product.Path,
					request.OperationID,
				)
			},
		})
	default:
		return nil, ErrStateIdentity
	}
	markerEffect, err := platform.markerEffect(
		ctx,
		pending.Resource,
		request,
		existingMarker,
		markerFound,
	)
	if err != nil {
		return nil, err
	}
	effects = append(effects, markerEffect)
	return effects, nil
}

func (platform *runtimePlatform) pendingRoleEffects(
	ctx context.Context,
	pending pendingEffect,
	request hostbootstrap.Request,
) ([]nativeEffect, error) {
	var effects []nativeEffect
	removalEffects, err := platform.ownerDefinitionRemovalEffects(request)
	if err != nil {
		return nil, err
	}
	installEffects, err := platform.ownerDefinitionInstallEffects(request)
	if err != nil {
		return nil, err
	}
	desired := request.Binding.RuntimeOwner
	if desired == hostbootstrap.RoleHeadless {
		running, err := platform.DesktopRunning(ctx, request)
		if err != nil {
			return nil, err
		}
		if running {
			return nil, ErrRuntimeOwnerActive
		}
	}
	var previous hostbootstrap.Role
	if len(pending.ExpectedPre.RuntimeOwners) == 1 {
		owner := pending.ExpectedPre.RuntimeOwners[0]
		if owner.Ownership == hostbootstrap.OwnershipOwned {
			previous = owner.Owner
		}
	}
	if previous != "" && previous != desired {
		if previous == hostbootstrap.RoleHeadless {
			effects = append(
				effects,
				platform.commandEffects(
					ctx,
					"role:stop",
					platform.owner.Stop,
					request,
				)...,
			)
			effects = append(
				effects,
				removalEffects...,
			)
			effects = append(
				effects,
				platform.commandEffects(
					ctx,
					"role:remove",
					platform.owner.Remove,
					request,
				)...,
			)
			effects = append(effects, platform.verifyHeadlessAbsentEffect(
				ctx,
				request,
				"role:verify-prior-absent",
			))
			markerRemoval, err := platform.roleMarkerRemovalEffect(
				"role:marker-remove",
				pending.PriorMarker,
				pending.PriorMarkerFound,
			)
			if err != nil {
				return nil, err
			}
			effects = append(effects, markerRemoval)
		} else {
			markerRemoval, err := platform.roleMarkerRemovalEffect(
				"role:desktop-marker-remove",
				pending.PriorMarker,
				pending.PriorMarkerFound,
			)
			if err != nil {
				return nil, err
			}
			effects = append(effects, markerRemoval)
		}
	}
	if desired == hostbootstrap.RoleDesktop {
		effects = append(effects, platform.verifyHeadlessAbsentEffect(
			ctx,
			request,
			"role:verify-headless-absent",
		))
		markerEffect, err := platform.roleMarkerAdvanceEffect(ctx, request)
		if err != nil {
			return nil, err
		}
		effects = append(effects, markerEffect)
		return effects, nil
	}
	if previous == hostbootstrap.RoleHeadless {
		effects = append(
			effects,
			platform.commandEffects(
				ctx,
				"role:repair-stop",
				platform.owner.Stop,
				request,
			)...,
		)
		effects = append(
			effects,
			removalEffects...,
		)
		effects = append(
			effects,
			platform.commandEffects(
				ctx,
				"role:repair-remove",
				platform.owner.Remove,
				request,
			)...,
		)
	}
	effects = append(effects, installEffects...)
	effects = append(
		effects,
		platform.commandEffects(
			ctx,
			"role:start",
			platform.owner.Start,
			request,
		)...,
	)
	effects = append(effects, nativeEffect{
		ID: "role:verify-headless-exact",
		Satisfied: func() (bool, error) {
			present, identityExact, lifecycleExact, err :=
				platform.probeHeadlessOwner(ctx, request)
			return present && identityExact && lifecycleExact, err
		},
		Apply: func() error {
			present, identityExact, lifecycleExact, err :=
				platform.probeHeadlessOwner(ctx, request)
			if err != nil {
				return err
			}
			if !present || !identityExact || !lifecycleExact {
				return ErrVerification
			}
			return nil
		},
	})
	markerEffect, err := platform.roleMarkerAdvanceEffect(ctx, request)
	if err != nil {
		return nil, err
	}
	effects = append(effects, markerEffect)
	return effects, nil
}

func (platform *runtimePlatform) roleMarkerRemovalEffect(
	effectID string,
	expected resourceMarker,
	found bool,
) (nativeEffect, error) {
	if found &&
		(expected.Kind != hostbootstrap.ResourceActiveRole ||
			expected.validate() != nil) {
		return nativeEffect{}, ErrStateIdentity
	}
	return nativeEffect{
		ID: effectID,
		Satisfied: func() (bool, error) {
			existing, currentFound, err := platform.state.LoadMarker(
				hostbootstrap.ResourceActiveRole,
			)
			if err != nil {
				return false, err
			}
			if !currentFound {
				return true, nil
			}
			if !found || !reflect.DeepEqual(existing, expected) {
				return false, ErrStateIdentity
			}
			return false, nil
		},
		Apply: func() error {
			if !found {
				return ErrStateIdentity
			}
			return platform.state.RemoveMarkerCAS(expected)
		},
	}, nil
}

func (platform *runtimePlatform) roleMarkerAdvanceEffect(
	ctx context.Context,
	request hostbootstrap.Request,
) (nativeEffect, error) {
	marker := resourceMarker{
		SchemaVersion: hostbootstrap.SchemaVersion,
		Kind:          hostbootstrap.ResourceActiveRole,
		OperationID:   request.OperationID,
		IdentitySHA256: resourceIdentitySHA256(
			hostbootstrap.ResourceActiveRole,
			request.Binding,
		),
	}
	return nativeEffect{
		ID: "role:marker-advance",
		Satisfied: func() (bool, error) {
			existing, found, err := platform.state.LoadMarker(
				hostbootstrap.ResourceActiveRole,
			)
			if err != nil {
				return false, err
			}
			if found && reflect.DeepEqual(existing, marker) {
				exact, err := platform.runtimeOwnerPostcondition(
					ctx,
					request,
				)
				return exact, err
			}
			if found {
				return false, ErrStateIdentity
			}
			return false, nil
		},
		Apply: func() error {
			exact, err := platform.runtimeOwnerPostcondition(ctx, request)
			if err != nil {
				return err
			}
			if !exact {
				return ErrVerification
			}
			return platform.replaceMarkerCAS(
				resourceMarker{},
				false,
				marker,
			)
		},
	}, nil
}

func (platform *runtimePlatform) runtimeOwnerPostcondition(
	ctx context.Context,
	request hostbootstrap.Request,
) (bool, error) {
	present, identityExact, lifecycleExact, err :=
		platform.probeHeadlessOwner(ctx, request)
	if err != nil {
		return false, err
	}
	if request.Binding.RuntimeOwner == hostbootstrap.RoleHeadless {
		return present && identityExact && lifecycleExact, nil
	}
	return !present, nil
}

func (platform *runtimePlatform) verifyHeadlessAbsentEffect(
	ctx context.Context,
	request hostbootstrap.Request,
	effectID string,
) nativeEffect {
	return nativeEffect{
		ID: effectID,
		Satisfied: func() (bool, error) {
			present, _, _, err := platform.probeHeadlessOwner(ctx, request)
			return !present, err
		},
		Apply: func() error {
			present, _, _, err := platform.probeHeadlessOwner(ctx, request)
			if err != nil {
				return err
			}
			if present {
				return ErrVerification
			}
			return nil
		},
	}
}

func (platform *runtimePlatform) ownerDefinitionInstallEffects(
	request hostbootstrap.Request,
) ([]nativeEffect, error) {
	if platform.target.Platform != hostbootstrap.PlatformDarwin &&
		platform.target.Platform != hostbootstrap.PlatformLinux {
		return nil, nil
	}
	principal, err := resolveReviewedPrincipal(request.Binding.Account.Name)
	if err != nil {
		return nil, err
	}
	effects := make([]nativeEffect, 0, len(platform.owner.ManagedFiles))
	for index, managed := range platform.owner.ManagedFiles {
		managed := managed
		content, err := renderManagedFile(managed, request, principal)
		if err != nil {
			return nil, err
		}
		effects = append(effects, nativeEffect{
			ID: fmt.Sprintf("role:definition:%d", index),
			Satisfied: func() (bool, error) {
				raw, err := readBoundedRegularFile(managed.Path, 64<<10)
				if errors.Is(err, fs.ErrNotExist) {
					return false, nil
				}
				if err != nil {
					return false, err
				}
				if err := validateRootDefinition(managed.Path, managed.Mode); err != nil {
					return false, err
				}
				return string(raw) == content, nil
			},
			Apply: func() error {
				return writeRootDefinitionNoFollow(
					managed.Path,
					[]byte(content),
					managed.Mode,
				)
			},
		})
	}
	return effects, nil
}

func (platform *runtimePlatform) ownerDefinitionRemovalEffects(
	request hostbootstrap.Request,
) ([]nativeEffect, error) {
	if platform.target.Platform != hostbootstrap.PlatformDarwin &&
		platform.target.Platform != hostbootstrap.PlatformLinux {
		return nil, nil
	}
	principal, err := resolveReviewedPrincipal(request.Binding.Account.Name)
	if err != nil {
		return nil, err
	}
	effects := make([]nativeEffect, 0, len(platform.owner.ManagedFiles))
	for index, managed := range platform.owner.ManagedFiles {
		managed := managed
		content, err := renderManagedFile(managed, request, principal)
		if err != nil {
			return nil, err
		}
		effects = append(effects, nativeEffect{
			ID: fmt.Sprintf("role:definition-remove:%d", index),
			Satisfied: func() (bool, error) {
				raw, err := readBoundedRegularFile(managed.Path, 64<<10)
				if errors.Is(err, fs.ErrNotExist) {
					return true, nil
				}
				if err != nil {
					return false, err
				}
				if err := validateRootDefinition(managed.Path, managed.Mode); err != nil {
					return false, err
				}
				if string(raw) != content {
					return false, ErrStateIdentity
				}
				return false, nil
			},
			Apply: func() error {
				if err := os.Remove(managed.Path); err != nil {
					return err
				}
				return syncDirectoryForPublication(
					filepath.Dir(managed.Path),
				)
			},
		})
	}
	return effects, nil
}

func sameResourceObservation(
	left hostbootstrap.Observations,
	right hostbootstrap.Observations,
	resource hostbootstrap.ResourceKind,
) bool {
	switch resource {
	case hostbootstrap.ResourceSSHService:
		return reflect.DeepEqual(left.SSHService, right.SSHService)
	case hostbootstrap.ResourceFirewall:
		return reflect.DeepEqual(left.Firewall, right.Firewall)
	case hostbootstrap.ResourceAuthorizedKey:
		return reflect.DeepEqual(left.AuthorizedKey, right.AuthorizedKey)
	case hostbootstrap.ResourceHelper:
		return reflect.DeepEqual(left.Helper, right.Helper)
	case hostbootstrap.ResourceProduct:
		return reflect.DeepEqual(left.Product, right.Product)
	case hostbootstrap.ResourceActiveRole:
		return reflect.DeepEqual(left.RuntimeOwners, right.RuntimeOwners)
	default:
		return false
	}
}

func (platform *runtimePlatform) RecoverPending(
	ctx context.Context,
	pending pendingEffect,
	request hostbootstrap.Request,
) (bool, error) {
	payload, err := loadPayloadBundle(platform.executable, request, platform.payloadFS)
	if err != nil {
		return false, err
	}
	platform.payload = payload
	if pending.Resource == hostbootstrap.ResourceActiveRole {
		exact, err := platform.roleActionFinalExact(ctx, request)
		if err != nil {
			return false, err
		}
		if exact {
			adopted := pending
			adopted.EffectID = "role:marker-advance"
			adopted.EffectComplete = true
			adopted.Revision++
			if err := platform.state.AdoptPending(
				pending,
				adopted,
			); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	if pending.EffectID == "" {
		return false, nil
	}
	effects, err := platform.pendingResourceEffects(ctx, pending, request)
	if err != nil {
		return false, err
	}
	if err := platform.resumeNativeEffectSuffix(pending, effects); err != nil {
		return false, err
	}
	fresh, err := platform.Inspect(ctx, request)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(fresh, pending.ExpectedPost), nil
}

func (platform *runtimePlatform) roleActionFinalExact(
	ctx context.Context,
	request hostbootstrap.Request,
) (bool, error) {
	marker, found, err := platform.state.LoadMarker(
		hostbootstrap.ResourceActiveRole,
	)
	if err != nil || !found {
		return false, err
	}
	desired := resourceMarker{
		SchemaVersion: hostbootstrap.SchemaVersion,
		Kind:          hostbootstrap.ResourceActiveRole,
		OperationID:   request.OperationID,
		IdentitySHA256: resourceIdentitySHA256(
			hostbootstrap.ResourceActiveRole,
			request.Binding,
		),
	}
	if !reflect.DeepEqual(marker, desired) {
		return false, nil
	}
	return platform.runtimeOwnerPostcondition(ctx, request)
}

func (platform *runtimePlatform) ValidateRepair(
	ctx context.Context,
	plan hostbootstrap.Plan,
) error {
	request := hostbootstrap.Request{
		SchemaVersion: plan.SchemaVersion,
		OperationID:   plan.OperationID,
		Binding:       plan.Binding,
	}
	for _, action := range plan.Actions {
		switch action {
		case hostbootstrap.ResourceSSHService:
			probe, err := platform.probe(ctx, action, request)
			if err != nil ||
				(probe.Present && !probe.IdentityExact) {
				return ErrRerunSignedBootstrap
			}
		case hostbootstrap.ResourceHelper:
			_, owned, err := platform.inspectOwnedHelper(ctx, request)
			if err != nil || !owned {
				return ErrRerunSignedBootstrap
			}
		case hostbootstrap.ResourceProduct:
			marker, found, err := platform.state.LoadMarker(action)
			if err != nil || !found {
				return ErrRerunSignedBootstrap
			}
			_, _, partialExact, err := inspectInstalledProductTree(
				request.Binding.Product.Path,
				marker.Files,
				platform.target.Platform,
			)
			if err != nil || !partialExact {
				return ErrRerunSignedBootstrap
			}
		case hostbootstrap.ResourceFirewall:
			marker, found, err := platform.state.LoadMarker(action)
			if err != nil || !found {
				return ErrRerunSignedBootstrap
			}
			probe, err := platform.probeFirewallResource(ctx, request)
			if err != nil ||
				probe.Unavailable ||
				(probe.Present && !probe.IdentityExact) {
				return ErrRerunSignedBootstrap
			}
			if platform.target.Platform == hostbootstrap.PlatformLinux {
				families, err := linuxFirewallOwnedFamiliesNative(request)
				if err != nil ||
					!reflect.DeepEqual(families, marker.FirewallFamilies) {
					return ErrRerunSignedBootstrap
				}
			}
		}
	}
	return nil
}

func (platform *runtimePlatform) ValidateRemoval(
	ctx context.Context,
	kind hostbootstrap.ResourceKind,
	request hostbootstrap.Request,
) (bool, error) {
	marker, found, err := platform.state.LoadMarker(kind)
	if err != nil || !found {
		return false, err
	}
	if err := platform.validateMarkerHistory(kind, marker); err != nil {
		return false, ErrForeignCollision
	}
	exact := false
	switch kind {
	case hostbootstrap.ResourceProduct:
		present, treeExact, err := verifyInstalledProductTree(
			request.Binding.Product.Path,
			marker.Files,
			platform.target.Platform,
		)
		if err != nil {
			return false, err
		}
		exact = present &&
			treeExact &&
			marker.IdentitySHA256 == request.Binding.Product.SHA256
	case hostbootstrap.ResourceHelper:
		inspection, err := platform.InspectHelper(ctx, request)
		if err != nil {
			return false, err
		}
		exact = (!inspection.Executable.Present ||
			(inspection.Executable.Exact &&
				inspection.ExecutableDigest == marker.IdentitySHA256)) &&
			marker.IdentitySHA256 == request.Binding.Helper.SHA256 &&
			inspection.DefinitionCompatible &&
			(!inspection.Endpoint.Present ||
				inspection.Endpoint.Exact)
	case hostbootstrap.ResourceActiveRole:
		owner, ownerFound, err := platform.inspectRole(ctx, request)
		if err != nil {
			return false, err
		}
		exact = ownerFound &&
			owner.Ownership == hostbootstrap.OwnershipOwned &&
			owner.Owner == request.Binding.RuntimeOwner &&
			marker.IdentitySHA256 == resourceIdentitySHA256(
				kind,
				request.Binding,
			)
	default:
		probe, err := platform.probe(ctx, kind, request)
		if err != nil {
			return false, err
		}
		exact = probe.Available &&
			probe.Present &&
			probe.Exact &&
			probe.IdentitySHA256 == marker.IdentitySHA256 &&
			marker.IdentitySHA256 == resourceIdentitySHA256(
				kind,
				request.Binding,
			)
		if exact &&
			kind == hostbootstrap.ResourceFirewall &&
			platform.target.Platform == hostbootstrap.PlatformLinux {
			families, err := linuxFirewallOwnedFamiliesNative(request)
			if err != nil {
				return false, err
			}
			exact = reflect.DeepEqual(
				families,
				marker.FirewallFamilies,
			)
		}
	}
	if !exact {
		return false, ErrForeignCollision
	}
	return true, nil
}

func (platform *runtimePlatform) ValidateUninstallPending(
	ctx context.Context,
	pending uninstallEffect,
	request hostbootstrap.Request,
	current hostbootstrap.Observations,
) error {
	marker, found, err := platform.state.LoadMarker(pending.Resource)
	if err != nil {
		return err
	}
	if found && !reflect.DeepEqual(marker, pending.PriorMarker) {
		return ErrStateIdentity
	}
	for _, kind := range uninstallOrder {
		if kind == pending.Resource {
			continue
		}
		if !sameResourceObservation(
			current,
			pending.ExpectedPre,
			kind,
		) {
			return ErrPlanChanged
		}
	}
	if sameResourceObservation(
		current,
		pending.ExpectedPre,
		pending.Resource,
	) || sameResourceObservation(
		current,
		pending.ExpectedPost,
		pending.Resource,
	) {
		return nil
	}
	switch pending.Resource {
	case hostbootstrap.ResourceHelper:
		inspection, err := platform.InspectHelper(ctx, request)
		if err != nil ||
			!inspection.DefinitionCompatible ||
			(inspection.Executable.Present &&
				(!inspection.Executable.Exact ||
					inspection.ExecutableDigest !=
						pending.PriorMarker.IdentitySHA256)) ||
			(inspection.Endpoint.Present &&
				!inspection.Endpoint.Exact) {
			return ErrStateIdentity
		}
	case hostbootstrap.ResourceSSHService:
		probe, err := platform.probe(ctx, pending.Resource, request)
		if err != nil || (probe.Present && !probe.IdentityExact) {
			return ErrStateIdentity
		}
	case hostbootstrap.ResourceFirewall:
		probe, err := platform.probeFirewallResource(ctx, request)
		if err != nil || (probe.Present && !probe.IdentityExact) {
			return ErrStateIdentity
		}
	case hostbootstrap.ResourceActiveRole:
		_, identityExact, _, err :=
			platform.probeHeadlessOwner(ctx, request)
		if err != nil || !identityExact {
			return ErrStateIdentity
		}
	case hostbootstrap.ResourceProduct:
		present, _, partialExact, err := inspectInstalledProductTree(
			request.Binding.Product.Path,
			pending.PriorMarker.Files,
			platform.target.Platform,
		)
		if err != nil || (present && !partialExact) {
			return ErrStateIdentity
		}
	case hostbootstrap.ResourceAuthorizedKey:
		probe, err := probeAuthorizedKeyWithPrior(
			request,
			pending.PriorMarker.AuthorizedKeyLine,
		)
		if err != nil ||
			(probe.Present &&
				(!probe.PriorExact ||
					probe.IdentitySHA256 !=
						pending.PriorMarker.IdentitySHA256)) {
			return ErrStateIdentity
		}
	default:
		return ErrStateIdentity
	}
	return nil
}

func (platform *runtimePlatform) CapturePendingMarker(
	kind hostbootstrap.ResourceKind,
) (resourceMarker, bool, error) {
	return platform.state.LoadMarker(kind)
}

func (platform *runtimePlatform) InspectHelper(
	ctx context.Context,
	request hostbootstrap.Request,
) (helperInspection, error) {
	if platform.helperComponents != nil {
		return platform.helperComponents.InspectHelper(ctx, request)
	}
	var inspection helperInspection
	executableState, digest, err := inspectHelperExecutableNative(
		request.Binding.Helper.Path,
		request.Binding.Helper.SHA256,
	)
	if err != nil {
		return inspection, err
	}
	inspection.Executable = executableState
	inspection.ExecutableDigest = digest
	switch platform.target.Platform {
	case hostbootstrap.PlatformWindows:
		if platform.windowsServices == nil {
			return inspection, ErrVerification
		}
		service, serviceErr := platform.windowsServices.Inspect("nvpair-host-helper")
		if serviceErr == nil {
			inspection.Definition = helperComponentState{
				Present: true,
				Exact: strings.EqualFold(service.Executable, windowsHelperPath) &&
					reflect.DeepEqual(service.Arguments, []string{"service"}) &&
					service.ServiceType == 0x10 &&
					service.StartType == 2 &&
					service.Account == "LocalSystem",
			}
			inspection.Service = helperComponentState{
				Present: true,
				Exact:   service.State == 4,
			}
			inspection.ServiceState = service.State
		} else if !errors.Is(serviceErr, ErrStateMissing) {
			return inspection, serviceErr
		}
		inspection.DefinitionCompatible =
			!inspection.Definition.Present ||
				inspection.Definition.Exact
	case hostbootstrap.PlatformDarwin:
		definitionPresent, definitionExact, definitionCompatible, err :=
			platform.probeManagedDefinitions(request)
		if err != nil {
			return inspection, err
		}
		inspection.Definition = helperComponentState{
			Present: definitionPresent,
			Exact:   definitionExact,
		}
		spec, _ := namedProbeDefinition(platform.target.Platform, hostbootstrap.ResourceHelper)
		output, serviceErr := platform.runner.Run(ctx, spec)
		if serviceErr == nil {
			present, identityExact, lifecycleExact, valid := parseLaunchdJob(
				output,
				"com.nvidia.nvpair.host-helper",
				darwinHelperPath,
				[]string{darwinHelperPath, "service"},
				"root",
			)
			if !valid {
				return inspection, ErrVerification
			}
			inspection.Definition.Present =
				inspection.Definition.Present || present
			inspection.Definition.Exact =
				inspection.Definition.Exact && identityExact
			definitionCompatible = definitionCompatible &&
				(!present || identityExact)
			inspection.Service = helperComponentState{
				Present: present,
				Exact:   lifecycleExact,
			}
		} else if !launchdJobNotLoaded(serviceErr) {
			return inspection, serviceErr
		}
		inspection.DefinitionCompatible = definitionCompatible
	case hostbootstrap.PlatformLinux:
		definitionPresent, definitionExact, definitionCompatible, err :=
			platform.probeManagedDefinitions(request)
		if err != nil {
			return inspection, err
		}
		principal, err := resolveReviewedPrincipal(request.Binding.Account.Name)
		if err != nil {
			return inspection, err
		}
		serviceOutput, serviceErr := platform.runner.Run(ctx, commandSpec{
			Path: "/usr/bin/systemctl",
			Args: []string{
				"show",
				"nvpair-host-helper.service",
				"--property=LoadState",
				"--property=ActiveState",
				"--property=UnitFileState",
				"--property=FragmentPath",
				"--property=DropInPaths",
				"--property=ExecStart",
				"--property=User",
				"--property=Group",
				"--property=Type",
			},
		})
		socketOutput, socketErr := platform.runner.Run(ctx, commandSpec{
			Path: "/usr/bin/systemctl",
			Args: []string{
				"show",
				"nvpair-host-helper.socket",
				"--property=LoadState",
				"--property=ActiveState",
				"--property=UnitFileState",
				"--property=FragmentPath",
				"--property=DropInPaths",
				"--property=Listen",
				"--property=SocketMode",
				"--property=SocketUser",
				"--property=SocketGroup",
			},
		})
		if serviceErr != nil {
			return inspection, serviceErr
		}
		if socketErr != nil {
			return inspection, socketErr
		}
		effective := parseLinuxHelperEffectiveInspection(
			serviceOutput,
			socketOutput,
			nativeGroupName(principal),
		)
		inspection.Definition = helperComponentState{
			Present: definitionPresent || effective.Present,
			Exact: definitionExact &&
				effective.IdentityExact,
		}
		inspection.DefinitionCompatible = definitionCompatible &&
			effective.IdentityCompatible
		inspection.Service = helperComponentState{
			Present: effective.Present,
			Exact:   effective.LifecycleExact,
		}
	default:
		return inspection, ErrUnsupportedTarget
	}
	endpoint, err := inspectHelperEndpointNative(request, platform.definition.Local)
	if err != nil {
		return inspection, err
	}
	inspection.Endpoint = endpoint
	inspection.IdentityExact =
		(!inspection.Executable.Present || inspection.Executable.Exact) &&
			inspection.DefinitionCompatible &&
			(!inspection.Endpoint.Present || inspection.Endpoint.Exact)
	return inspection, nil
}

func (platform *runtimePlatform) inspectOwnedHelper(
	ctx context.Context,
	request hostbootstrap.Request,
) (helperInspection, bool, error) {
	desired, err := platform.InspectHelper(ctx, request)
	if err != nil || desired.IdentityExact {
		return desired, desired.IdentityExact, err
	}
	marker, found, err := platform.state.LoadMarker(
		hostbootstrap.ResourceHelper,
	)
	if err != nil || !found {
		return desired, false, err
	}
	if err := platform.validateMarkerHistory(
		hostbootstrap.ResourceHelper,
		marker,
	); err != nil {
		return desired, false, nil
	}
	priorRequest, _, operationErr := platform.state.LoadOperation()
	if operationErr != nil || priorRequest.OperationID != marker.OperationID {
		receipt, receiptErr := platform.state.loadReceiptFor(marker.OperationID)
		if receiptErr != nil {
			return desired, false, nil
		}
		priorRequest = request
		priorRequest.Binding = receipt.Binding
	}
	prior, err := platform.InspectHelper(ctx, priorRequest)
	if err != nil {
		return helperInspection{}, false, err
	}
	owned := prior.IdentityExact &&
		prior.Executable.Present &&
		prior.Executable.Exact &&
		prior.ExecutableDigest == marker.IdentitySHA256
	return prior, owned, nil
}

func parseSystemdProperties(output string) map[string]string {
	properties := make(map[string]string)
	for _, line := range strings.Split(
		strings.ReplaceAll(output, "\r\n", "\n"),
		"\n",
	) {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if found {
			properties[key] = value
		}
	}
	return properties
}

type linuxHelperEffectiveInspection struct {
	Present            bool
	IdentityExact      bool
	IdentityCompatible bool
	LifecycleExact     bool
}

type darwinHeadlessInspection struct {
	Present        bool
	IdentityExact  bool
	LifecycleExact bool
	Valid          bool
}

func parseDarwinHeadlessInspection(
	output string,
	owner nativeOwnerManifest,
	account string,
) darwinHeadlessInspection {
	present, identityExact, lifecycleExact, valid := parseLaunchdJob(
		output,
		owner.HeadlessIdentity,
		owner.TUIPath,
		owner.OwnerCommand,
		account,
	)
	return darwinHeadlessInspection{
		Present:        present,
		IdentityExact:  identityExact,
		LifecycleExact: lifecycleExact,
		Valid:          valid,
	}
}

func parseLaunchdJob(
	output string,
	label string,
	program string,
	expectedArguments []string,
	user string,
) (bool, bool, bool, bool) {
	rawLines := strings.Split(
		strings.ReplaceAll(output, "\r\n", "\n"),
		"\n",
	)
	var lines []string
	for _, raw := range rawLines {
		if line := strings.TrimSpace(raw); line != "" {
			lines = append(lines, line)
		}
	}
	present := len(lines) != 0
	if !present {
		return false, false, false, false
	}
	if lines[0] != "system/"+label+" = {" ||
		lines[len(lines)-1] != "}" {
		return true, false, false, false
	}
	programValue := ""
	username := ""
	state := ""
	throttledValue := ""
	var arguments []string
	inArguments := false
	argumentBlockSeen := false
	programSeen := false
	usernameSeen := false
	stateSeen := false
	throttledSeen := false
	valid := true
	unknownDepth := 0
	for _, line := range lines[1 : len(lines)-1] {
		if unknownDepth > 0 {
			if line == "}" {
				unknownDepth--
			} else if strings.HasSuffix(line, " = {") {
				unknownDepth++
			}
			continue
		}
		if inArguments {
			if line == "}" {
				inArguments = false
			} else if line != "" {
				arguments = append(arguments, line)
			}
			continue
		}
		key, value, found := strings.Cut(line, " = ")
		if !found {
			valid = false
			continue
		}
		switch key {
		case "program":
			if programSeen {
				valid = false
			}
			programSeen = true
			programValue = value
		case "username":
			if usernameSeen {
				valid = false
			}
			usernameSeen = true
			username = value
		case "state":
			if stateSeen ||
				(value != "running" && value != "stopped") {
				valid = false
			}
			stateSeen = true
			state = value
		case "arguments":
			if argumentBlockSeen || value != "{" {
				valid = false
			} else {
				argumentBlockSeen = true
				inArguments = true
			}
		case "throttled":
			if throttledSeen ||
				(value != "true" && value != "false") {
				valid = false
			}
			throttledSeen = true
			throttledValue = value
		default:
			if value == "{" {
				unknownDepth = 1
			}
		}
	}
	if inArguments ||
		unknownDepth != 0 ||
		!argumentBlockSeen ||
		!programSeen ||
		!usernameSeen ||
		!stateSeen {
		valid = false
	}
	identityExact := present &&
		valid &&
		programValue == program &&
		reflect.DeepEqual(arguments, expectedArguments) &&
		username == user
	lifecycleExact := valid &&
		state == "running" &&
		throttledValue != "true"
	return present, identityExact, lifecycleExact, valid
}

func launchdJobNotLoaded(err error) bool {
	var exitError *exec.ExitError
	return errors.As(err, &exitError) && exitError.ExitCode() == 113
}

func combineDarwinHeadlessInspection(
	filePresent bool,
	fileExact bool,
	job darwinHeadlessInspection,
) (bool, bool, bool) {
	present := filePresent || job.Present
	identityCompatible := (!filePresent || fileExact) &&
		(!job.Present || job.IdentityExact)
	completeLifecycle := filePresent &&
		job.Present &&
		job.LifecycleExact
	return present, identityCompatible, completeLifecycle
}

func parseLinuxHelperEffectiveInspection(
	serviceOutput string,
	socketOutput string,
	group string,
) linuxHelperEffectiveInspection {
	service := parseSystemdProperties(serviceOutput)
	socket := parseSystemdProperties(socketOutput)
	serviceLoaded := service["LoadState"] == "loaded"
	socketLoaded := socket["LoadState"] == "loaded"
	serviceIdentity := serviceLoaded &&
		service["FragmentPath"] ==
			"/etc/systemd/system/nvpair-host-helper.service" &&
		service["DropInPaths"] == "" &&
		service["Type"] == "simple" &&
		service["UnitFileState"] == "static" &&
		linuxSystemdExecStartExact(
			service["ExecStart"],
			linuxHelperPath,
			linuxHelperPath+" service",
		) &&
		service["User"] == "root" &&
		service["Group"] == "root"
	socketIdentity := socketLoaded &&
		socket["FragmentPath"] ==
			"/etc/systemd/system/nvpair-host-helper.socket" &&
		socket["DropInPaths"] == "" &&
		socket["Listen"] ==
			"/run/nvpair-host-helper.sock (Stream)" &&
		socket["SocketMode"] == "0660" &&
		socket["SocketUser"] == "root" &&
		socket["SocketGroup"] == group
	return linuxHelperEffectiveInspection{
		Present:       serviceLoaded || socketLoaded,
		IdentityExact: serviceIdentity && socketIdentity,
		IdentityCompatible: (!serviceLoaded || serviceIdentity) &&
			(!socketLoaded || socketIdentity),
		LifecycleExact: (service["ActiveState"] == "active" ||
			service["ActiveState"] == "inactive") &&
			serviceLoaded &&
			socketLoaded &&
			socket["ActiveState"] == "active" &&
			socket["UnitFileState"] == "enabled",
	}
}

func (platform *runtimePlatform) Inspect(
	ctx context.Context,
	request hostbootstrap.Request,
) (hostbootstrap.Observations, error) {
	var observed hostbootstrap.Observations
	payload, err := loadPayloadBundle(platform.executable, request, platform.payloadFS)
	if err != nil {
		return observed, err
	}
	platform.payload = payload
	if err := validateReviewedAccount(request.Binding.Account); err != nil {
		return observed, err
	}
	err = nil
	observed.SSHService, err = platform.inspectEndpoint(ctx, hostbootstrap.ResourceSSHService, request)
	if err != nil {
		return observed, err
	}
	observed.Firewall, err = platform.inspectFirewall(ctx, request)
	if err != nil {
		return observed, err
	}
	observed.AuthorizedKey, err = platform.inspectAuthorizedKey(request)
	if err != nil {
		return observed, err
	}
	observed.Helper, err = platform.inspectArtifact(ctx, hostbootstrap.ResourceHelper, request)
	if err != nil {
		return observed, err
	}
	observed.Product, err = platform.inspectArtifact(ctx, hostbootstrap.ResourceProduct, request)
	if err != nil {
		return observed, err
	}
	owner, found, err := platform.inspectRole(ctx, request)
	if err != nil {
		return observed, err
	}
	observed.RuntimeOwners = []hostbootstrap.RuntimeOwnerObservation{}
	if found {
		observed.RuntimeOwners = []hostbootstrap.RuntimeOwnerObservation{owner}
	}
	return observed, observed.Validate(request.Binding.Target)
}

func (platform *runtimePlatform) inspectEndpoint(
	ctx context.Context,
	kind hostbootstrap.ResourceKind,
	request hostbootstrap.Request,
) (hostbootstrap.EndpointObservation, error) {
	marker, owned, err := platform.state.LoadMarker(kind)
	if err != nil {
		return hostbootstrap.EndpointObservation{}, err
	}
	if owned {
		if err := platform.validateMarkerHistory(kind, marker); err != nil {
			return hostbootstrap.EndpointObservation{}, err
		}
	}
	probe, err := platform.probe(ctx, kind, request)
	if err != nil {
		return hostbootstrap.EndpointObservation{}, err
	}
	if owned {
		endpoint := request.Binding.Endpoint
		if kind == hostbootstrap.ResourceSSHService &&
			classifyLinuxSSHOwnership(
				probe.Present,
				probe.IdentityExact,
				true,
			) == hostbootstrap.OwnershipForeign {
			endpoint.Port = 2222
			return hostbootstrap.EndpointObservation{
				Ownership: hostbootstrap.OwnershipForeign,
				Endpoint:  &endpoint,
			}, nil
		}
		if !probe.Available || !probe.Present || !probe.Exact ||
			marker.IdentitySHA256 != resourceIdentitySHA256(kind, request.Binding) {
			endpoint.Port = 2222
		}
		return hostbootstrap.EndpointObservation{
			Ownership: hostbootstrap.OwnershipOwned,
			Endpoint:  &endpoint,
		}, nil
	}
	if !probe.Available || !probe.Present {
		return hostbootstrap.EndpointObservation{Ownership: hostbootstrap.OwnershipAbsent}, nil
	}
	endpoint := request.Binding.Endpoint
	if !probe.Exact {
		endpoint.Port = 2222
	}
	return hostbootstrap.EndpointObservation{
		Ownership: hostbootstrap.OwnershipForeign,
		Endpoint:  &endpoint,
	}, nil
}

func (platform *runtimePlatform) inspectFirewall(
	ctx context.Context,
	request hostbootstrap.Request,
) (hostbootstrap.EndpointObservation, error) {
	kind := hostbootstrap.ResourceFirewall
	probe, err := platform.probeFirewallResource(ctx, request)
	if err != nil {
		return hostbootstrap.EndpointObservation{}, err
	}
	if probe.NotApplicable {
		return hostbootstrap.EndpointObservation{
			Ownership: hostbootstrap.OwnershipNotApplicable,
		}, nil
	}
	if probe.Unavailable {
		return hostbootstrap.EndpointObservation{
			Ownership: hostbootstrap.OwnershipUnavailable,
		}, nil
	}
	marker, owned, err := platform.state.LoadMarker(kind)
	if err != nil {
		return hostbootstrap.EndpointObservation{}, err
	}
	if owned {
		if err := platform.validateMarkerHistory(kind, marker); err != nil {
			return hostbootstrap.EndpointObservation{}, err
		}
		endpoint := request.Binding.Endpoint
		if platform.target.Platform == hostbootstrap.PlatformLinux {
			families, err := linuxFirewallOwnedFamiliesNative(request)
			if err != nil {
				return hostbootstrap.EndpointObservation{}, err
			}
			if !reflect.DeepEqual(families, marker.FirewallFamilies) {
				endpoint.Port = 2222
				return hostbootstrap.EndpointObservation{
					Ownership: hostbootstrap.OwnershipForeign,
					Endpoint:  &endpoint,
				}, nil
			}
			if probe.Present && !probe.IdentityExact {
				endpoint.Port = 2222
				return hostbootstrap.EndpointObservation{
					Ownership: hostbootstrap.OwnershipForeign,
					Endpoint:  &endpoint,
				}, nil
			}
		}
		if platform.target.Platform == hostbootstrap.PlatformWindows &&
			probe.Present &&
			!probe.Exact {
			endpoint.Port = 2222
			return hostbootstrap.EndpointObservation{
				Ownership: hostbootstrap.OwnershipForeign,
				Endpoint:  &endpoint,
			}, nil
		}
		if !probe.Present ||
			!probe.Exact ||
			marker.IdentitySHA256 != resourceIdentitySHA256(kind, request.Binding) {
			endpoint.Port = 2222
		}
		return hostbootstrap.EndpointObservation{
			Ownership: hostbootstrap.OwnershipOwned,
			Endpoint:  &endpoint,
		}, nil
	}
	if !probe.Present {
		return hostbootstrap.EndpointObservation{Ownership: hostbootstrap.OwnershipAbsent}, nil
	}
	endpoint := request.Binding.Endpoint
	if !probe.Exact {
		endpoint.Port = 2222
	}
	return hostbootstrap.EndpointObservation{
		Ownership: hostbootstrap.OwnershipForeign,
		Endpoint:  &endpoint,
	}, nil
}

func (platform *runtimePlatform) inspectAuthorizedKey(
	request hostbootstrap.Request,
) (hostbootstrap.AuthorizedKeyObservation, error) {
	kind := hostbootstrap.ResourceAuthorizedKey
	marker, owned, err := platform.state.LoadMarker(kind)
	if err != nil {
		return hostbootstrap.AuthorizedKeyObservation{}, err
	}
	if owned {
		if err := platform.validateMarkerHistory(kind, marker); err != nil {
			return hostbootstrap.AuthorizedKeyObservation{}, err
		}
	}
	priorLine := ""
	if owned {
		priorLine = marker.AuthorizedKeyLine
	}
	probe, err := probeAuthorizedKeyWithPrior(request, priorLine)
	if err != nil {
		return hostbootstrap.AuthorizedKeyObservation{}, err
	}
	if owned {
		return classifyOwnedAuthorizedKey(probe, marker, request)
	}
	if !probe.Present {
		return hostbootstrap.AuthorizedKeyObservation{Ownership: hostbootstrap.OwnershipAbsent}, nil
	}
	return hostbootstrap.AuthorizedKeyObservation{
		Ownership: hostbootstrap.OwnershipForeign,
		Identity: &hostbootstrap.AuthorizedKeyIdentity{
			AccountName:       request.Binding.Account.Name,
			Path:              request.Binding.Account.AuthorizedKeysPath,
			FingerprintSHA256: probe.IdentitySHA256,
		},
	}, nil
}

func classifyOwnedAuthorizedKey(
	probe nativeProbe,
	marker resourceMarker,
	request hostbootstrap.Request,
) (hostbootstrap.AuthorizedKeyObservation, error) {
	fingerprint := marker.IdentitySHA256
	if probe.Present {
		fingerprint = probe.IdentitySHA256
	}
	ownership := hostbootstrap.OwnershipOwned
	if probe.Present {
		desiredExact := probe.Exact &&
			probe.IdentitySHA256 ==
				request.Binding.ControllerKey.FingerprintSHA256
		priorExact := probe.PriorExact &&
			probe.IdentitySHA256 == marker.IdentitySHA256
		if !desiredExact && !priorExact {
			ownership = hostbootstrap.OwnershipForeign
		}
	} else if !probe.Present {
		fingerprint = strings.Repeat("0", 64)
	}
	return hostbootstrap.AuthorizedKeyObservation{
		Ownership: ownership,
		Identity: &hostbootstrap.AuthorizedKeyIdentity{
			AccountName:       request.Binding.Account.Name,
			Path:              request.Binding.Account.AuthorizedKeysPath,
			FingerprintSHA256: fingerprint,
		},
	}, nil
}

func (platform *runtimePlatform) inspectArtifact(
	ctx context.Context,
	kind hostbootstrap.ResourceKind,
	request hostbootstrap.Request,
) (hostbootstrap.ArtifactObservation, error) {
	marker, owned, err := platform.state.LoadMarker(kind)
	if err != nil {
		return hostbootstrap.ArtifactObservation{}, err
	}
	if owned {
		if err := platform.validateMarkerHistory(kind, marker); err != nil {
			return hostbootstrap.ArtifactObservation{}, err
		}
	}
	if !owned && kind == hostbootstrap.ResourceHelper {
		inspection, err := platform.InspectHelper(ctx, request)
		if err != nil {
			return hostbootstrap.ArtifactObservation{}, err
		}
		if inspection.Executable.Present &&
			inspection.Executable.Exact &&
			inspection.ExecutableDigest ==
				request.Binding.Helper.SHA256 &&
			!inspection.Definition.Present &&
			!inspection.Service.Present &&
			!inspection.Endpoint.Present {
			return hostbootstrap.ArtifactObservation{
				Ownership: hostbootstrap.OwnershipAbsent,
			}, nil
		}
	}
	var probe nativeProbe
	if owned && kind == hostbootstrap.ResourceProduct {
		present, exact, partialExact, err := inspectInstalledProductTree(
			request.Binding.Product.Path,
			marker.Files,
			platform.target.Platform,
		)
		if err != nil {
			return hostbootstrap.ArtifactObservation{}, err
		}
		probe = nativeProbe{
			Available:      true,
			Present:        present,
			Exact:          exact,
			IdentitySHA256: marker.IdentitySHA256,
			IdentityExact:  partialExact,
		}
	} else {
		probe, err = platform.probe(ctx, kind, request)
		if err != nil {
			return hostbootstrap.ArtifactObservation{}, err
		}
	}
	desired := request.Binding.Product
	if kind == hostbootstrap.ResourceHelper {
		desired = request.Binding.Helper
		if owned {
			prior, found := platform.helperIdentityForMarker(marker)
			if !found {
				return hostbootstrap.ArtifactObservation{},
					ErrStateIdentity
			}
			desired = prior
		}
	}
	if owned {
		if kind == hostbootstrap.ResourceProduct &&
			probe.Present &&
			!probe.IdentityExact {
			identity := desired
			identity.SHA256 = marker.IdentitySHA256
			return hostbootstrap.ArtifactObservation{
				Ownership: hostbootstrap.OwnershipForeign,
				Identity:  &identity,
			}, nil
		}
		if kind == hostbootstrap.ResourceHelper &&
			probe.Present &&
			!probe.IdentityExact {
			identity := desired
			if probe.IdentitySHA256 != "" {
				identity.SHA256 = probe.IdentitySHA256
			}
			return hostbootstrap.ArtifactObservation{
				Ownership: hostbootstrap.OwnershipForeign,
				Identity:  &identity,
			}, nil
		}
		if probe.Present &&
			probe.IdentitySHA256 != "" &&
			probe.IdentitySHA256 != marker.IdentitySHA256 {
			return hostbootstrap.ArtifactObservation{}, ErrStateIdentity
		}
		identity := desired
		identity.SHA256 = marker.IdentitySHA256
		if kind == hostbootstrap.ResourceHelper &&
			probe.Present &&
			probe.IdentitySHA256 != "" {
			identity.SHA256 = probe.IdentitySHA256
		} else if !probe.Present || !probe.Exact {
			identity.SHA256 = strings.Repeat("0", 64)
		}
		return hostbootstrap.ArtifactObservation{
			Ownership: hostbootstrap.OwnershipOwned,
			Identity:  &identity,
		}, nil
	}
	if !probe.Present {
		return hostbootstrap.ArtifactObservation{Ownership: hostbootstrap.OwnershipAbsent}, nil
	}
	identity := desired
	if probe.IdentitySHA256 != "" {
		identity.SHA256 = probe.IdentitySHA256
	}
	return hostbootstrap.ArtifactObservation{
		Ownership: hostbootstrap.OwnershipForeign,
		Identity:  &identity,
	}, nil
}

func (platform *runtimePlatform) helperIdentityForMarker(
	marker resourceMarker,
) (hostbootstrap.ArtifactIdentity, bool) {
	request, _, err := platform.state.LoadOperation()
	if err == nil && request.OperationID == marker.OperationID {
		return request.Binding.Helper,
			request.Binding.Helper.SHA256 == marker.IdentitySHA256
	}
	receipt, err := platform.state.loadReceiptFor(marker.OperationID)
	if err != nil {
		return hostbootstrap.ArtifactIdentity{}, false
	}
	return receipt.Binding.Helper,
		receipt.Binding.Helper.SHA256 == marker.IdentitySHA256
}

func (platform *runtimePlatform) inspectRole(
	ctx context.Context,
	request hostbootstrap.Request,
) (hostbootstrap.RuntimeOwnerObservation, bool, error) {
	marker, found, err := platform.state.LoadMarker(hostbootstrap.ResourceActiveRole)
	if err != nil {
		return hostbootstrap.RuntimeOwnerObservation{}, false, err
	}
	headlessPresent, headlessIdentityExact, headlessLifecycleExact, err :=
		platform.probeHeadlessOwner(ctx, request)
	if err != nil {
		return hostbootstrap.RuntimeOwnerObservation{}, false, err
	}
	desktopRunning, err := platform.DesktopRunning(ctx, request)
	if err != nil {
		return hostbootstrap.RuntimeOwnerObservation{}, false, err
	}
	if dualOwnerConflict(headlessPresent, desktopRunning) {
		return hostbootstrap.RuntimeOwnerObservation{
			Ownership: hostbootstrap.OwnershipForeign,
			Owner:     hostbootstrap.RoleHeadless,
		}, true, nil
	}
	if !found {
		switch {
		case headlessPresent:
			return hostbootstrap.RuntimeOwnerObservation{
				Ownership: hostbootstrap.OwnershipForeign,
				Owner:     hostbootstrap.RoleHeadless,
			}, true, nil
		case desktopRunning:
			return hostbootstrap.RuntimeOwnerObservation{
				Ownership: hostbootstrap.OwnershipForeign,
				Owner:     hostbootstrap.RoleDesktop,
			}, true, nil
		default:
			return hostbootstrap.RuntimeOwnerObservation{}, false, nil
		}
	}
	if err := platform.validateMarkerHistory(hostbootstrap.ResourceActiveRole, marker); err != nil {
		return hostbootstrap.RuntimeOwnerObservation{}, false, err
	}
	owner := request.Binding.RuntimeOwner
	desired := resourceIdentitySHA256(hostbootstrap.ResourceActiveRole, request.Binding)
	if marker.IdentitySHA256 != desired {
		if owner == hostbootstrap.RoleDesktop {
			owner = hostbootstrap.RoleHeadless
		} else {
			owner = hostbootstrap.RoleDesktop
		}
	}
	if headlessPresent {
		if !headlessIdentityExact || owner != hostbootstrap.RoleHeadless {
			return hostbootstrap.RuntimeOwnerObservation{
				Ownership: hostbootstrap.OwnershipForeign,
				Owner:     hostbootstrap.RoleHeadless,
			}, true, nil
		}
		if !headlessLifecycleExact {
			return hostbootstrap.RuntimeOwnerObservation{}, false, nil
		}
		owner = hostbootstrap.RoleHeadless
	} else if desktopRunning && owner == hostbootstrap.RoleHeadless {
		return hostbootstrap.RuntimeOwnerObservation{
			Ownership: hostbootstrap.OwnershipForeign,
			Owner:     hostbootstrap.RoleDesktop,
		}, true, nil
	} else if owner == hostbootstrap.RoleHeadless {
		return hostbootstrap.RuntimeOwnerObservation{}, false, nil
	}
	return hostbootstrap.RuntimeOwnerObservation{
		Ownership: hostbootstrap.OwnershipOwned,
		Owner:     owner,
	}, true, nil
}

func dualOwnerConflict(headlessPresent, desktopRunning bool) bool {
	return headlessPresent && desktopRunning
}

func (platform *runtimePlatform) validateMarkerHistory(
	kind hostbootstrap.ResourceKind,
	marker resourceMarker,
) error {
	request, _, err := platform.state.LoadOperation()
	if err == nil && request.OperationID == marker.OperationID {
		if marker.IdentitySHA256 != resourceIdentitySHA256(kind, request.Binding) {
			return ErrStateIdentity
		}
		return nil
	}
	if err != nil && !errors.Is(err, ErrStateMissing) {
		return err
	}
	receipt, receiptErr := platform.state.loadReceiptFor(marker.OperationID)
	if receiptErr != nil ||
		receipt.Validate() != nil ||
		request.Binding.Target != receipt.Binding.Target ||
		request.Binding.Account != receipt.Binding.Account ||
		marker.IdentitySHA256 !=
			resourceIdentitySHA256(kind, receipt.Binding) ||
		!resourceOwned(receipt.Verified, kind) {
		return ErrStateIdentity
	}
	return nil
}

func (platform *runtimePlatform) ApplyResource(
	ctx context.Context,
	kind hostbootstrap.ResourceKind,
	request hostbootstrap.Request,
) error {
	if !validResourceKind(kind) {
		return ErrUnsupportedIdentity
	}
	pending, err := platform.state.LoadPending()
	if err != nil {
		return err
	}
	if pending.Resource != kind ||
		pending.OperationID != request.OperationID {
		return ErrStateIdentity
	}
	effects, err := platform.pendingResourceEffects(ctx, pending, request)
	if err != nil {
		return err
	}
	for _, effect := range effects {
		if err := platform.executeNativeEffect(effect); err != nil {
			return err
		}
	}
	return nil
}

func (platform *runtimePlatform) StopRole(
	ctx context.Context,
	role hostbootstrap.Role,
	request hostbootstrap.Request,
) error {
	if role == hostbootstrap.RoleDesktop {
		running, err := platform.DesktopRunning(ctx, request)
		if err != nil {
			return err
		}
		if running {
			return ErrRuntimeOwnerActive
		}
		return platform.runNativeEffect("role:desktop-marker-remove", func() error {
			return platform.state.RemoveMarker(hostbootstrap.ResourceActiveRole)
		})
	}
	if err := platform.runCommands(ctx, "role:stop", platform.owner.Stop, request); err != nil {
		return err
	}
	if err := platform.removeOwnerDefinitions(ctx, request); err != nil {
		return err
	}
	if err := platform.runCommands(ctx, "role:remove", platform.owner.Remove, request); err != nil {
		return err
	}
	present, _, _, err := platform.probeHeadlessOwner(ctx, request)
	if err != nil {
		return err
	}
	if present {
		return ErrVerification
	}
	return platform.runNativeEffect("role:marker-remove", func() error {
		return platform.state.RemoveMarker(hostbootstrap.ResourceActiveRole)
	})
}

func resolveOwnerCommand(
	command commandSpec,
	request hostbootstrap.Request,
) commandSpec {
	resolved := commandSpec{Path: command.Path, Args: make([]string, len(command.Args))}
	for index, argument := range command.Args {
		argument = strings.ReplaceAll(argument, "TARGET_ACCOUNT", request.Binding.Account.Name)
		argument = strings.ReplaceAll(argument, "TARGET_HOME", request.Binding.Account.HomePath)
		resolved.Args[index] = argument
	}
	return resolved
}

func (platform *runtimePlatform) removeOwnerDefinitions(
	ctx context.Context,
	request hostbootstrap.Request,
) error {
	if platform.target.Platform != hostbootstrap.PlatformDarwin &&
		platform.target.Platform != hostbootstrap.PlatformLinux {
		return nil
	}
	principal, err := resolveReviewedPrincipal(request.Binding.Account.Name)
	if err != nil {
		return err
	}
	for index, managed := range platform.owner.ManagedFiles {
		expected, err := renderManagedFile(managed, request, principal)
		if err != nil {
			return err
		}
		raw, err := readBoundedRegularFile(managed.Path, 64<<10)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := validateRootDefinition(managed.Path, managed.Mode); err != nil {
			return err
		}
		if string(raw) != expected {
			return ErrStateIdentity
		}
		if err := platform.runNativeEffect(
			fmt.Sprintf("role:definition-remove:%d", index),
			func() error {
				if err := os.Remove(managed.Path); err != nil {
					return err
				}
				return syncDirectoryForPublication(
					filepath.Dir(managed.Path),
				)
			},
		); err != nil {
			return err
		}
	}
	return nil
}

func (platform *runtimePlatform) probeHeadlessOwner(
	ctx context.Context,
	request hostbootstrap.Request,
) (bool, bool, bool, error) {
	switch platform.target.Platform {
	case hostbootstrap.PlatformWindows:
		if platform.windowsServices == nil {
			return false, false, false, ErrVerification
		}
		service, err := platform.windowsServices.Inspect(
			platform.owner.HeadlessIdentity,
		)
		if errors.Is(err, ErrStateMissing) {
			return false, false, false, nil
		}
		if err != nil {
			return false, false, false, err
		}
		exact := strings.EqualFold(service.Executable, platform.owner.TUIPath) &&
			reflect.DeepEqual(
				service.Arguments,
				platform.owner.OwnerCommand[1:],
			) &&
			service.ServiceType == 0x10 &&
			service.StartType == 2 &&
			service.Account == "LocalSystem"
		return true, exact, service.State == 4, nil
	case hostbootstrap.PlatformDarwin:
		managed := platform.owner.ManagedFiles[0]
		principal, err := resolveReviewedPrincipal(
			request.Binding.Account.Name,
		)
		if err != nil {
			return false, false, false, err
		}
		expected, err := renderManagedFile(managed, request, principal)
		if err != nil {
			return false, false, false, err
		}
		output, launchErr := platform.runner.Run(
			ctx,
			resolveOwnerCommand(platform.owner.Inspect[0], request),
		)
		job := darwinHeadlessInspection{}
		if launchErr == nil {
			job = parseDarwinHeadlessInspection(
				output,
				platform.owner,
				request.Binding.Account.Name,
			)
			if !job.Valid {
				return false, false, false, ErrVerification
			}
		} else if !launchdJobNotLoaded(launchErr) {
			return false, false, false, launchErr
		}
		raw, fileErr := readBoundedRegularFile(managed.Path, 64<<10)
		filePresent := fileErr == nil
		fileExact := false
		if filePresent {
			if err := validateRootDefinition(managed.Path, managed.Mode); err != nil {
				return true, false, false, err
			}
			fileExact = string(raw) == expected
		} else if !errors.Is(fileErr, fs.ErrNotExist) {
			return job.Present, false, false, fileErr
		}
		present, identityCompatible, completeLifecycle :=
			combineDarwinHeadlessInspection(filePresent, fileExact, job)
		return present, identityCompatible, completeLifecycle, nil
	case hostbootstrap.PlatformLinux:
		present, identityExact, activeExact, err :=
			platform.inspectLinuxHeadlessOwner(ctx, request)
		return present, identityExact, activeExact, err
	default:
		return false, false, false, ErrUnsupportedTarget
	}
}

func (platform *runtimePlatform) inspectLinuxHeadlessOwner(
	ctx context.Context,
	request hostbootstrap.Request,
) (bool, bool, bool, error) {
	if len(platform.owner.ManagedFiles) != 1 {
		return false, false, false, ErrStateIdentity
	}
	managed := platform.owner.ManagedFiles[0]
	principal, err := resolveReviewedPrincipal(request.Binding.Account.Name)
	if err != nil {
		return false, false, false, err
	}
	expected, err := renderManagedFile(managed, request, principal)
	if err != nil {
		return false, false, false, err
	}
	raw, fileErr := readBoundedRegularFile(managed.Path, 64<<10)
	filePresent := fileErr == nil
	fileExact := false
	if filePresent {
		if err := validateRootDefinition(managed.Path, managed.Mode); err != nil {
			return true, false, false, err
		}
		fileExact = string(raw) == expected
	} else if !errors.Is(fileErr, fs.ErrNotExist) {
		return false, false, false, fileErr
	}
	output, err := platform.runner.Run(
		ctx,
		resolveOwnerCommand(platform.owner.Inspect[0], request),
	)
	if err != nil {
		return filePresent, false, false, err
	}
	properties := make(map[string]string)
	for _, line := range strings.Split(
		strings.ReplaceAll(output, "\r\n", "\n"),
		"\n",
	) {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if found {
			properties[key] = value
		}
	}
	loaded := properties["LoadState"] == "loaded"
	present := filePresent || loaded
	execStart := platform.owner.TUIPath +
		" --headless --broker-path " + platform.owner.BrokerPath
	identityExact := fileExact &&
		loaded &&
		properties["FragmentPath"] == managed.Path &&
		properties["DropInPaths"] == "" &&
		linuxSystemdExecStartExact(
			properties["ExecStart"],
			platform.owner.TUIPath,
			execStart,
		) &&
		properties["User"] == request.Binding.Account.Name &&
		properties["Group"] == nativeGroupName(principal) &&
		properties["Environment"] ==
			"XDG_RUNTIME_DIR=/run/nvpair-headless"
	activeExact := properties["ActiveState"] == "active" &&
		properties["UnitFileState"] == "enabled"
	return present, identityExact, activeExact, nil
}

func linuxSystemdExecStartExact(
	value string,
	path string,
	command string,
) bool {
	return strings.Count(value, "{ path=") == 1 &&
		strings.HasPrefix(
			value,
			"{ path="+path+" ; argv[]="+command+" ;",
		) &&
		strings.HasSuffix(value, "}")
}

func (platform *runtimePlatform) RemoveResource(
	ctx context.Context,
	kind hostbootstrap.ResourceKind,
	request hostbootstrap.Request,
) error {
	owned, err := platform.ValidateRemoval(ctx, kind, request)
	if err != nil {
		return err
	}
	uninstall, uninstallErr := platform.state.LoadUninstallPending()
	journalBound := uninstallErr == nil &&
		uninstall.OperationID == request.OperationID &&
		uninstall.Resource == kind
	if uninstallErr != nil &&
		!errors.Is(uninstallErr, ErrStateMissing) {
		return uninstallErr
	}
	if !owned && !journalBound {
		return nil
	}
	marker, found, err := platform.state.LoadMarker(kind)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if marker.IdentitySHA256 == "" {
		return ErrStateIdentity
	}
	if journalBound &&
		!reflect.DeepEqual(marker, uninstall.PriorMarker) {
		return ErrStateIdentity
	}
	effects, err := platform.uninstallResourceEffects(
		ctx,
		kind,
		request,
		marker,
	)
	if err != nil {
		return err
	}
	markerEffect := nativeEffect{
		ID: "uninstall:marker:" + string(kind),
		Satisfied: func() (bool, error) {
			_, found, err := platform.state.LoadMarker(kind)
			return !found, err
		},
		Apply: func() error {
			return platform.state.RemoveMarkerCAS(marker)
		},
	}
	effects = append(effects, markerEffect)
	return platform.resumeUninstallEffectSuffix(uninstall, effects)
}

func (platform *runtimePlatform) resumeUninstallEffectSuffix(
	pending uninstallEffect,
	effects []nativeEffect,
) error {
	start := 0
	if pending.EffectID != "" {
		recorded := -1
		for index, effect := range effects {
			if effect.ID == pending.EffectID {
				if recorded != -1 {
					return ErrStateIdentity
				}
				recorded = index
			}
		}
		if recorded == -1 {
			return ErrStateIdentity
		}
		for index := 0; index < recorded; index++ {
			effect := effects[index]
			if effect.ReplaySafe {
				if err := executeReplaySafePrefix(effect); err != nil {
					return err
				}
				continue
			}
			postcondition := effect.Satisfied
			if postcondition == nil {
				postcondition = effect.PrefixSatisfied
			}
			if postcondition == nil {
				return ErrStateIdentity
			}
			satisfied, err := postcondition()
			if err != nil {
				return err
			}
			if satisfied {
				continue
			}
			return ErrStateIdentity
		}
		start = recorded
		if pending.EffectComplete {
			postcondition := effects[recorded].Satisfied
			if postcondition == nil {
				postcondition = effects[recorded].PrefixSatisfied
			}
			if postcondition == nil {
				return ErrStateIdentity
			}
			satisfied, err := postcondition()
			if err != nil || !satisfied {
				if err != nil {
					return err
				}
				return ErrStateIdentity
			}
			start++
		}
	}
	for _, effect := range effects[start:] {
		if err := platform.executeNativeEffect(effect); err != nil {
			return err
		}
	}
	return nil
}

func (platform *runtimePlatform) uninstallResourceEffects(
	ctx context.Context,
	kind hostbootstrap.ResourceKind,
	request hostbootstrap.Request,
	marker resourceMarker,
) ([]nativeEffect, error) {
	switch kind {
	case hostbootstrap.ResourceSSHService:
		effects := platform.commandEffects(
			ctx,
			"uninstall:ssh-service",
			platform.definition.Remove[kind],
			request,
		)
		return effects, nil
	case hostbootstrap.ResourceAuthorizedKey:
		return []nativeEffect{{
			ID: "uninstall:authorized-key",
			Satisfied: func() (bool, error) {
				probe, err := probeAuthorizedKey(request)
				return !probe.Present, err
			},
			Apply: func() error {
				return removeAuthorizedKey(request, marker)
			},
		}}, nil
	case hostbootstrap.ResourceProduct:
		if marker.IdentitySHA256 != platform.payload.Product.SHA256 {
			return nil, ErrStateIdentity
		}
		trash := productUninstallTrashPath(
			request.Binding.Product.Path,
			request.OperationID,
		)
		effects := []nativeEffect{
			{
				ID: "uninstall:product:rename-trash",
				Satisfied: func() (bool, error) {
					_, err := os.Lstat(request.Binding.Product.Path)
					if errors.Is(err, fs.ErrNotExist) {
						return true, nil
					}
					return false, err
				},
				Apply: func() error {
					return renameInstalledProductToTrash(
						request.Binding.Product.Path,
						trash,
						platform.payload.Product.Files,
						platform.target.Platform,
					)
				},
			},
			{
				ID:         "uninstall:product:remove-trash",
				ReplaySafe: true,
				Satisfied: func() (bool, error) {
					_, err := os.Lstat(trash)
					if errors.Is(err, fs.ErrNotExist) {
						return true, nil
					}
					return false, err
				},
				Apply: func() error {
					return removeProductUninstallTrash(trash)
				},
			},
		}
		return effects, nil
	case hostbootstrap.ResourceHelper:
		if err := platform.validateMarkerHistory(kind, marker); err != nil {
			return nil, ErrForeignCollision
		}
		commandEffects := platform.commandEffects(
			ctx,
			"uninstall:helper",
			platform.definition.Remove[kind],
			request,
		)
		var effects []nativeEffect
		if platform.target.Platform == hostbootstrap.PlatformLinux {
			if len(commandEffects) != 2 {
				return nil, ErrStateIdentity
			}
			effects = append(effects, commandEffects[0])
		} else {
			effects = append(effects, commandEffects...)
		}
		effects = append(effects, nativeEffect{
			ID: "uninstall:helper:definitions",
			Satisfied: func() (bool, error) {
				return platform.managedDefinitionsAbsent(request)
			},
			Apply: func() error {
				return platform.removeManagedDefinitions(request)
			},
		})
		if platform.target.Platform == hostbootstrap.PlatformLinux {
			effects = append(effects, commandEffects[1])
		}
		effects = append(effects, nativeEffect{
			ID: "uninstall:helper:executable",
			Satisfied: func() (bool, error) {
				state, digest, err := inspectHelperExecutableNative(
					request.Binding.Helper.Path,
					marker.IdentitySHA256,
				)
				if err != nil {
					return false, err
				}
				if !state.Present {
					return true, nil
				}
				if !state.Exact || digest != marker.IdentitySHA256 {
					return false, ErrForeignCollision
				}
				return false, nil
			},
			Apply: func() error {
				return removeExactArtifact(
					request.Binding.Helper.Path,
					marker.IdentitySHA256,
				)
			},
		})
		effects = append(effects, nativeEffect{
			ID: "uninstall:helper:verify-absent",
			Satisfied: func() (bool, error) {
				return platform.resourceNativeAbsent(
					ctx,
					hostbootstrap.ResourceHelper,
					request,
				)
			},
			Apply: func() error {
				absent, err := platform.resourceNativeAbsent(
					ctx,
					hostbootstrap.ResourceHelper,
					request,
				)
				if err != nil {
					return err
				}
				if !absent {
					return ErrVerification
				}
				return nil
			},
		})
		return effects, nil
	case hostbootstrap.ResourceActiveRole:
		effects, err := platform.uninstallRoleEffects(ctx, request)
		if err != nil {
			return nil, err
		}
		return effects, nil
	case hostbootstrap.ResourceFirewall:
		if platform.target.Platform == hostbootstrap.PlatformWindows {
			effect := nativeEffect{
				ID: "uninstall:firewall:windows",
				Satisfied: func() (bool, error) {
					probe := inspectWindowsFirewallAPI(platform.firewall)
					return !probe.Present && !probe.Unavailable, nil
				},
				Apply: func() error {
					return removeWindowsFirewallRule(platform.firewall)
				},
			}
			return []nativeEffect{effect}, nil
		} else if platform.target.Platform == hostbootstrap.PlatformLinux {
			effects, err := linuxFirewallRemovalEffects(
				ctx,
				platform.runner,
				request,
			)
			if err != nil {
				return nil, err
			}
			return effects, nil
		}
		return platform.commandEffects(
			ctx,
			"uninstall:firewall",
			platform.definition.Remove[kind],
			request,
		), nil
	default:
		return platform.commandEffects(
			ctx,
			"uninstall:"+string(kind),
			platform.definition.Remove[kind],
			request,
		), nil
	}
}

func (platform *runtimePlatform) uninstallRoleEffects(
	ctx context.Context,
	request hostbootstrap.Request,
) ([]nativeEffect, error) {
	effects := platform.commandEffects(
		ctx,
		"role:stop",
		platform.owner.Stop,
		request,
	)
	definitions, err := platform.ownerDefinitionRemovalEffects(request)
	if err != nil {
		return nil, err
	}
	effects = append(effects, definitions...)
	effects = append(effects, platform.commandEffects(
		ctx,
		"role:remove",
		platform.owner.Remove,
		request,
	)...)
	effects = append(effects, platform.verifyHeadlessAbsentEffect(
		ctx,
		request,
		"uninstall:role:verify-absent",
	))
	return effects, nil
}

func (platform *runtimePlatform) resourceNativeAbsent(
	ctx context.Context,
	kind hostbootstrap.ResourceKind,
	request hostbootstrap.Request,
) (bool, error) {
	switch kind {
	case hostbootstrap.ResourceHelper:
		inspection, err := platform.InspectHelper(ctx, request)
		return !inspection.Executable.Present &&
			!inspection.Definition.Present &&
			!inspection.Service.Present &&
			!inspection.Endpoint.Present, err
	case hostbootstrap.ResourceProduct:
		present, _, err := verifyInstalledProductTree(
			request.Binding.Product.Path,
			platform.payload.Product.Files,
			platform.target.Platform,
		)
		return !present, err
	case hostbootstrap.ResourceAuthorizedKey:
		probe, err := probeAuthorizedKey(request)
		return !probe.Present, err
	case hostbootstrap.ResourceFirewall:
		probe, err := platform.probeFirewallResource(ctx, request)
		return probe.NotApplicable || !probe.Present, err
	case hostbootstrap.ResourceSSHService:
		probe, err := platform.probe(ctx, kind, request)
		return !probe.Present, err
	case hostbootstrap.ResourceActiveRole:
		_, found, err := platform.inspectRole(ctx, request)
		return !found, err
	default:
		return false, ErrUnsupportedIdentity
	}
}

func (platform *runtimePlatform) replaceMarkerCAS(
	expected resourceMarker,
	expectedFound bool,
	marker resourceMarker,
) error {
	if expectedFound {
		return platform.state.ReplaceMarker(expected, marker)
	}
	return platform.state.SaveMarker(marker)
}

func (platform *runtimePlatform) probe(
	ctx context.Context,
	kind hostbootstrap.ResourceKind,
	request hostbootstrap.Request,
) (nativeProbe, error) {
	switch kind {
	case hostbootstrap.ResourceProduct:
		present, exact, err := verifyInstalledProductTree(
			request.Binding.Product.Path,
			platform.payload.Product.Files,
			platform.target.Platform,
		)
		return nativeProbe{
			Available:      true,
			Present:        present,
			Exact:          exact,
			IdentitySHA256: platform.payload.Product.SHA256,
		}, err
	case hostbootstrap.ResourceHelper:
		inspection, owned, inspectErr := platform.inspectOwnedHelper(
			ctx,
			request,
		)
		if inspectErr != nil {
			return nativeProbe{}, inspectErr
		}
		anyPresent := inspection.Executable.Present ||
			inspection.Definition.Present ||
			inspection.Service.Present ||
			inspection.Endpoint.Present
		return nativeProbe{
			Available: true,
			Present:   anyPresent,
			Exact: owned &&
				inspection.ExecutableDigest ==
					request.Binding.Helper.SHA256 &&
				inspection.Executable.Exact &&
				inspection.Definition.Exact &&
				inspection.Service.Exact &&
				inspection.Endpoint.Exact,
			IdentitySHA256: inspection.ExecutableDigest,
			IdentityExact:  owned,
			LifecycleExact: inspection.Service.Exact,
		}, nil
	case hostbootstrap.ResourceAuthorizedKey:
		probe, err := probeAuthorizedKey(request)
		probe.Exact = probe.Exact &&
			probe.IdentitySHA256 == request.Binding.ControllerKey.FingerprintSHA256
		return probe, err
	case hostbootstrap.ResourceFirewall:
		return platform.probeFirewallResource(ctx, request)
	case hostbootstrap.ResourceSSHService:
		if platform.target.Platform == hostbootstrap.PlatformWindows {
			inspection, err := inspectWindowsSSHNative(
				ctx,
				platform.runner,
				platform.windowsServices,
			)
			identity := ""
			if inspection.IdentityExact() && inspection.Present() {
				identity = resourceIdentitySHA256(kind, request.Binding)
			} else if inspection.Present() {
				identity = strings.Repeat("0", 64)
			}
			return nativeProbe{
				Available:      err == nil,
				Present:        inspection.Present(),
				Exact:          inspection.Exact(),
				IdentitySHA256: identity,
				IdentityExact:  inspection.IdentityExact(),
				LifecycleExact: inspection.Lifecycle.Exact,
			}, err
		}
		if platform.target.Platform == hostbootstrap.PlatformLinux {
			inspection, available, err := inspectLinuxSSHNative(
				ctx,
				platform.runner,
			)
			identity := ""
			if inspection.IdentityExact() {
				identity = resourceIdentitySHA256(kind, request.Binding)
			} else if inspection.PackageInstalled || inspection.UnitLoaded {
				identity = strings.Repeat("0", 64)
			}
			return nativeProbe{
				Available:      available,
				Present:        inspection.PackageInstalled || inspection.UnitLoaded,
				Exact:          inspection.Exact(),
				IdentitySHA256: identity,
				IdentityExact:  inspection.IdentityExact(),
				LifecycleExact: inspection.LifecycleExact(),
			}, err
		}
		if platform.target.Platform == hostbootstrap.PlatformDarwin {
			return platform.probeDarwinSSH(ctx, request)
		}
		present, exact, available, err := platform.probeEndpointResource(ctx, kind)
		identity := ""
		if exact {
			identity = resourceIdentitySHA256(kind, request.Binding)
		} else if present {
			identity = strings.Repeat("0", 64)
		}
		return nativeProbe{
			Available:      available,
			Present:        present,
			Exact:          exact,
			IdentitySHA256: identity,
			IdentityExact:  exact,
			LifecycleExact: exact,
		}, err
	default:
		return nativeProbe{Available: true}, nil
	}
}

func (platform *runtimePlatform) probeDarwinSSH(
	ctx context.Context,
	request hostbootstrap.Request,
) (nativeProbe, error) {
	remoteLogin, err := platform.runner.Run(ctx, commandSpec{
		Path: "/usr/sbin/systemsetup",
		Args: []string{"-getremotelogin"},
	})
	if err != nil {
		return nativeProbe{}, err
	}
	loginState := parseRemoteLoginState(remoteLogin)
	if loginState == remoteLoginUnavailable {
		return nativeProbe{}, ErrVerification
	}
	disabled, err := platform.runner.Run(ctx, commandSpec{
		Path: "/bin/launchctl",
		Args: []string{"print-disabled", "system"},
	})
	if err != nil {
		return nativeProbe{}, err
	}
	launchEnabled, disabledValid := parseLaunchdDisabledState(
		disabled,
		"com.openssh.sshd",
	)
	if !disabledValid {
		return nativeProbe{}, ErrVerification
	}
	service, serviceErr := platform.runner.Run(ctx, commandSpec{
		Path: "/bin/launchctl",
		Args: []string{"print", "system/com.openssh.sshd"},
	})
	if serviceErr != nil {
		if !launchdJobNotLoaded(serviceErr) {
			return nativeProbe{}, serviceErr
		}
		present := loginState == remoteLoginOn
		return nativeProbe{
			Available: true,
			Present:   present,
			Exact:     false,
			IdentitySHA256: resourceIdentitySHA256(
				hostbootstrap.ResourceSSHService,
				request.Binding,
			),
			IdentityExact:  true,
			LifecycleExact: false,
		}, nil
	}
	present, identityExact, lifecycleExact, valid := parseLaunchdJob(
		service,
		"com.openssh.sshd",
		"/usr/sbin/sshd",
		[]string{"/usr/sbin/sshd", "-i"},
		"root",
	)
	if !valid {
		return nativeProbe{}, ErrVerification
	}
	lifecycleExact = lifecycleExact &&
		loginState == remoteLoginOn &&
		launchEnabled
	identity := strings.Repeat("0", 64)
	if identityExact {
		identity = resourceIdentitySHA256(
			hostbootstrap.ResourceSSHService,
			request.Binding,
		)
	}
	return nativeProbe{
		Available:      true,
		Present:        present || loginState == remoteLoginOn,
		Exact:          present && identityExact && lifecycleExact,
		IdentitySHA256: identity,
		IdentityExact:  !present || identityExact,
		LifecycleExact: lifecycleExact,
	}, nil
}

func (platform *runtimePlatform) probeEndpointResource(
	ctx context.Context,
	kind hostbootstrap.ResourceKind,
) (bool, bool, bool, error) {
	if kind == hostbootstrap.ResourceSSHService {
		switch platform.target.Platform {
		case hostbootstrap.PlatformWindows:
			if platform.windowsServices == nil {
				return false, false, false, ErrVerification
			}
			service, err := platform.windowsServices.Inspect("sshd")
			if errors.Is(err, ErrStateMissing) {
				return false, false, true, nil
			}
			if err != nil {
				return false, false, false, err
			}
			state := nativeServiceState{
				Present: true,
				Running: service.State == 4,
				Enabled: service.StartType == 2,
			}
			exact := state.Exact() &&
				strings.EqualFold(service.Executable, windowsSSHDPath) &&
				len(service.Arguments) == 0 &&
				service.ServiceType == 0x10 &&
				service.Account == "LocalSystem"
			return true, exact, true, nil
		case hostbootstrap.PlatformDarwin:
			remoteLogin, err := platform.runner.Run(ctx, commandSpec{
				Path: "/usr/sbin/systemsetup",
				Args: []string{"-getremotelogin"},
			})
			if err != nil {
				return false, false, false, err
			}
			loginState := parseRemoteLoginState(remoteLogin)
			if loginState == remoteLoginUnavailable {
				return false, false, false, ErrVerification
			}
			disabled, err := platform.runner.Run(ctx, commandSpec{
				Path: "/bin/launchctl",
				Args: []string{"print-disabled", "system"},
			})
			if err != nil {
				return false, false, false, err
			}
			launchEnabled, disabledValid := parseLaunchdDisabledState(
				disabled,
				"com.openssh.sshd",
			)
			if !disabledValid {
				return false, false, false, ErrVerification
			}
			service, err := platform.runner.Run(ctx, commandSpec{
				Path: "/bin/launchctl",
				Args: []string{"print", "system/com.openssh.sshd"},
			})
			if err != nil {
				if launchdJobNotLoaded(err) {
					if loginState == remoteLoginOff {
						if launchEnabled {
							return false, false, false, ErrVerification
						}
						return false, false, true, nil
					}
					return true, false, true, nil
				}
				return false, false, false, err
			}
			state, valid := parseLaunchdSSHServiceState(
				remoteLogin,
				disabled,
				service,
			)
			if !valid {
				return false, false, false, ErrVerification
			}
			return state.Present, state.Exact(), true, nil
		case hostbootstrap.PlatformLinux:
			inspection, available, err := inspectLinuxSSHNative(
				ctx,
				platform.runner,
			)
			return inspection.PackageInstalled,
				inspection.Exact(),
				available,
				err
		}
	}
	spec, exactFragments, availableFragments := endpointProbeDefinition(platform.target.Platform, kind)
	output, err := platform.runner.Run(ctx, spec)
	if err != nil {
		return false, false, true, nil
	}
	available := true
	for _, fragment := range availableFragments {
		if !strings.Contains(strings.ToLower(output), strings.ToLower(fragment)) {
			available = false
		}
	}
	if !available {
		return false, false, false, nil
	}
	exact := true
	for _, fragment := range exactFragments {
		if !strings.Contains(strings.ToLower(output), strings.ToLower(fragment)) {
			exact = false
		}
	}
	return output != "", exact, true, nil
}

func (platform *runtimePlatform) probeFirewallResource(
	ctx context.Context,
	request hostbootstrap.Request,
) (nativeProbe, error) {
	switch platform.target.Platform {
	case hostbootstrap.PlatformDarwin:
		return nativeProbe{
			Available:     true,
			Exact:         true,
			NotApplicable: true,
		}, nil
	case hostbootstrap.PlatformLinux:
		return probeLinuxFirewallNative(ctx, platform.runner, request)
	case hostbootstrap.PlatformWindows:
		probe := inspectWindowsFirewallAPI(platform.firewall)
		identity := ""
		if probe.Exact {
			identity = resourceIdentitySHA256(hostbootstrap.ResourceFirewall, request.Binding)
		} else if probe.Present {
			identity = strings.Repeat("0", 64)
		}
		probe.IdentitySHA256 = identity
		return probe, nil
	default:
		return nativeProbe{}, ErrUnsupportedTarget
	}
}

type windowsFirewallRule struct {
	Name                        string
	Enabled                     bool
	Direction                   int32
	Profiles                    int32
	Protocol                    int32
	LocalPorts                  string
	RemotePorts                 string
	LocalAddresses              string
	RemoteAddresses             string
	ApplicationName             string
	ServiceName                 string
	InterfaceTypes              string
	Interfaces                  string
	IcmpTypesAndCodes           string
	EdgeTraversal               bool
	EdgeTraversalOptions        int32
	Grouping                    string
	LocalAppPackageID           string
	LocalUserOwner              string
	LocalUserAuthorizedList     string
	RemoteUserAuthorizedList    string
	RemoteMachineAuthorizedList string
	SecureFlags                 int32
	Action                      int32
}

const windowsFirewallRuleName = "NVIDIA PAIR SSH (owned)"

type windowsFirewallAPI interface {
	Rules() ([]windowsFirewallRule, error)
	Add(windowsFirewallRule) error
	Remove(string) error
}

func exactWindowsFirewallRule() windowsFirewallRule {
	return windowsFirewallRule{
		Name:            windowsFirewallRuleName,
		Enabled:         true,
		Direction:       1,
		Profiles:        2,
		Protocol:        6,
		LocalPorts:      "22",
		RemotePorts:     "*",
		LocalAddresses:  "*",
		RemoteAddresses: "*",
		InterfaceTypes:  "All",
		Action:          1,
	}
}

func (rule windowsFirewallRule) exact() bool {
	return reflect.DeepEqual(rule, exactWindowsFirewallRule())
}

func inspectWindowsFirewallAPI(api windowsFirewallAPI) nativeProbe {
	if api == nil {
		return nativeProbe{Unavailable: true}
	}
	rules, err := api.Rules()
	if err != nil {
		return nativeProbe{Unavailable: true}
	}
	var owned []windowsFirewallRule
	for _, rule := range rules {
		if rule.Name == windowsFirewallRuleName {
			owned = append(owned, rule)
		}
	}
	if len(owned) > 1 {
		return nativeProbe{Unavailable: true}
	}
	if len(owned) == 0 {
		return nativeProbe{Available: true}
	}
	return nativeProbe{Available: true, Present: true, Exact: owned[0].exact()}
}

func addWindowsFirewallRule(api windowsFirewallAPI) error {
	probe := inspectWindowsFirewallAPI(api)
	if probe.Unavailable {
		return ErrFirewallUnavailable
	}
	if probe.Present {
		return ErrForeignCollision
	}
	return api.Add(exactWindowsFirewallRule())
}

func removeWindowsFirewallRule(api windowsFirewallAPI) error {
	probe := inspectWindowsFirewallAPI(api)
	if probe.Unavailable || !probe.Present || !probe.Exact {
		return ErrStateIdentity
	}
	return api.Remove(windowsFirewallRuleName)
}

func (platform *runtimePlatform) probeManagedDefinitions(
	request hostbootstrap.Request,
) (bool, bool, bool, error) {
	if len(platform.definition.ManagedFiles) == 0 {
		return true, true, true, nil
	}
	principal, principalErr := platform.state.LoadPrincipal()
	exact := principalErr == nil
	compatible := principalErr == nil
	present := false
	complete := true
	for _, managed := range platform.definition.ManagedFiles {
		raw, err := readBoundedRegularFile(managed.Path, 64<<10)
		if errors.Is(err, fs.ErrNotExist) {
			complete = false
			continue
		}
		if err != nil {
			if errors.Is(err, ErrUnsafeState) {
				present = true
				exact = false
				compatible = false
				continue
			}
			return false, false, false, err
		}
		if err := validateRootDefinition(managed.Path, managed.Mode); err != nil {
			present = true
			exact = false
			compatible = false
			continue
		}
		present = true
		if principalErr == nil {
			expected, renderErr := renderManagedFile(managed, request, principal)
			if renderErr != nil {
				return false, false, false, renderErr
			}
			if string(raw) != expected {
				exact = false
				compatible = false
			}
		}
	}
	return present, complete && exact, compatible, nil
}

func resourceIdentitySHA256(
	kind hostbootstrap.ResourceKind,
	binding hostbootstrap.Binding,
) string {
	var value any
	switch kind {
	case hostbootstrap.ResourceSSHService, hostbootstrap.ResourceFirewall:
		value = binding.Endpoint
	case hostbootstrap.ResourceAuthorizedKey:
		return binding.ControllerKey.FingerprintSHA256
	case hostbootstrap.ResourceHelper:
		return binding.Helper.SHA256
	case hostbootstrap.ResourceProduct:
		return binding.Product.SHA256
	case hostbootstrap.ResourceActiveRole:
		value = binding.RuntimeOwner
	default:
		return strings.Repeat("0", 64)
	}
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func probeAuthorizedKey(request hostbootstrap.Request) (nativeProbe, error) {
	return probeAuthorizedKeyWithPrior(request, "")
}

func probeAuthorizedKeyWithPrior(
	request hostbootstrap.Request,
	priorLine string,
) (nativeProbe, error) {
	path := request.Binding.Account.AuthorizedKeysPath
	if err := validateAuthorizedKeyRead(path, request.Binding.Account.Name); err != nil {
		return nativeProbe{}, err
	}
	raw, err := readBoundedRegularFile(path, 1<<20)
	if errors.Is(err, fs.ErrNotExist) {
		return nativeProbe{Available: true}, nil
	}
	if err != nil {
		return nativeProbe{}, err
	}
	return probeAuthorizedKeyBytes(raw, request, priorLine)
}

func probeAuthorizedKeyBytes(
	raw []byte,
	request hostbootstrap.Request,
	priorLine string,
) (nativeProbe, error) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 32<<10)
	var found string
	exactLine := false
	priorExact := false
	desiredLine := desiredAuthorizedKeyLine(request)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		markerLine, digest, valid := parseManagedAuthorizedKeyLine(line)
		if !markerLine {
			continue
		}
		if !valid {
			digest = strings.Repeat("0", 64)
		}
		if found != "" {
			return nativeProbe{}, ErrStateIdentity
		}
		found = digest
		exactLine = line == desiredLine
		priorExact = priorLine != "" && line == priorLine
	}
	if err := scanner.Err(); err != nil {
		return nativeProbe{}, err
	}
	return nativeProbe{
		Available:      true,
		Present:        found != "",
		Exact:          found != "" && exactLine,
		IdentitySHA256: found,
		PriorExact:     found != "" && priorExact,
	}, nil
}

func parseManagedAuthorizedKeyLine(
	line string,
) (bool, string, bool) {
	key, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || len(bytes.TrimSpace(rest)) != 0 {
		if strings.Contains(line, ownedKeyCommentPrefix) {
			return true, "", false
		}
		return false, "", false
	}
	if !strings.HasPrefix(comment, ownedKeyCommentPrefix) {
		return false, "", false
	}
	sum := sha256.Sum256(key.Marshal())
	return true, hex.EncodeToString(sum[:]), len(options) == 0
}

func desiredAuthorizedKeyLine(request hostbootstrap.Request) string {
	return string(request.Binding.ControllerKey.Algorithm) + " " +
		request.Binding.ControllerKey.Material + " " +
		ownedKeyCommentPrefix + request.Binding.Helper.SHA256
}

type authorizedKeyStore interface {
	Read() ([]byte, error)
	CompareAndSwap([]byte, []byte) error
}

type nativeAuthorizedKeyStore struct {
	path        string
	principal   reviewedPrincipalState
	operationID string
}

func (store nativeAuthorizedKeyStore) Read() ([]byte, error) {
	raw, err := readBoundedRegularFile(store.path, 1<<20)
	if errors.Is(err, fs.ErrNotExist) {
		return []byte{}, nil
	}
	return raw, err
}

func (store nativeAuthorizedKeyStore) CompareAndSwap(expected, replacement []byte) error {
	return writeAuthorizedKeysNoFollow(
		store.path,
		replacement,
		store.principal,
		expected,
		store.operationID,
	)
}

func applyAuthorizedKey(
	request hostbootstrap.Request,
	marker resourceMarker,
	markerFound bool,
) error {
	principal, err := resolveReviewedPrincipal(request.Binding.Account.Name)
	if err != nil {
		return err
	}
	adopted := false
	if err := recoverAuthorizedKeysBackup(
		request.Binding.Account.AuthorizedKeysPath,
		request.OperationID,
		func(displaced, current []byte) (bool, error) {
			replacement, err := authorizedKeyApplyBody(
				displaced,
				request,
				marker,
				markerFound,
			)
			if err != nil {
				return false, err
			}
			exact := bytes.Equal(current, replacement)
			adopted = adopted || exact
			return exact, nil
		},
	); err != nil {
		return err
	}
	if adopted {
		return nil
	}
	return applyAuthorizedKeyCAS(
		nativeAuthorizedKeyStore{
			path:        request.Binding.Account.AuthorizedKeysPath,
			principal:   principal,
			operationID: request.OperationID,
		},
		request,
		marker,
		markerFound,
	)
}

func applyAuthorizedKeyCAS(
	store authorizedKeyStore,
	request hostbootstrap.Request,
	marker resourceMarker,
	markerFound bool,
) error {
	raw, err := store.Read()
	if err != nil {
		return err
	}
	body, err := authorizedKeyApplyBody(raw, request, marker, markerFound)
	if err != nil {
		return err
	}
	return store.CompareAndSwap(raw, body)
}

func authorizedKeyApplyBody(
	raw []byte,
	request hostbootstrap.Request,
	marker resourceMarker,
	markerFound bool,
) ([]byte, error) {
	lines := splitPreservedRawLines(raw)
	var filtered bytes.Buffer
	ownedLines := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(string(bytes.TrimSuffix(line, []byte{'\n'})))
		markerLine, digest, valid := parseManagedAuthorizedKeyLine(trimmed)
		if markerLine {
			ownedLines++
			if !valid ||
				!markerFound ||
				trimmed != marker.AuthorizedKeyLine ||
				digest != marker.IdentitySHA256 {
				return nil, ErrStateIdentity
			}
			continue
		}
		_, _ = filtered.Write(line)
	}
	if ownedLines > 1 {
		return nil, ErrStateIdentity
	}
	if filtered.Len() > 0 &&
		filtered.Bytes()[filtered.Len()-1] != '\n' {
		_ = filtered.WriteByte('\n')
	}
	_, _ = filtered.WriteString(desiredAuthorizedKeyLine(request))
	_ = filtered.WriteByte('\n')
	return filtered.Bytes(), nil
}

func removeAuthorizedKey(
	request hostbootstrap.Request,
	marker resourceMarker,
) error {
	principal, err := resolveReviewedPrincipal(request.Binding.Account.Name)
	if err != nil {
		return err
	}
	store := nativeAuthorizedKeyStore{
		path:        request.Binding.Account.AuthorizedKeysPath,
		principal:   principal,
		operationID: request.OperationID,
	}
	adopted := false
	if err := recoverAuthorizedKeysBackup(
		store.path,
		request.OperationID,
		func(displaced, current []byte) (bool, error) {
			replacement, err := authorizedKeyRemovalBody(displaced, marker)
			if err != nil {
				return false, err
			}
			exact := bytes.Equal(current, replacement)
			adopted = adopted || exact
			return exact, nil
		},
	); err != nil {
		return err
	}
	if adopted {
		return nil
	}
	raw, err := store.Read()
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	body, err := authorizedKeyRemovalBody(raw, marker)
	if err != nil {
		return err
	}
	return store.CompareAndSwap(raw, body)
}

func authorizedKeyRemovalBody(
	raw []byte,
	marker resourceMarker,
) ([]byte, error) {
	lines := splitPreservedRawLines(raw)
	var filtered bytes.Buffer
	for _, line := range lines {
		trimmed := strings.TrimSpace(string(bytes.TrimSuffix(line, []byte{'\n'})))
		markerLine, digest, valid := parseManagedAuthorizedKeyLine(trimmed)
		if markerLine {
			if !valid ||
				trimmed != marker.AuthorizedKeyLine ||
				digest != marker.IdentitySHA256 {
				return nil, ErrStateIdentity
			}
			continue
		}
		_, _ = filtered.Write(line)
	}
	return filtered.Bytes(), nil
}

func splitPreservedRawLines(raw []byte) [][]byte {
	var lines [][]byte
	for len(raw) > 0 {
		index := bytes.IndexByte(raw, '\n')
		if index == -1 {
			lines = append(lines, raw)
			break
		}
		size := index + 1
		lines = append(lines, raw[:size])
		raw = raw[size:]
	}
	return lines
}

func readBoundedRegularFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	links, err := nativeFileLinkCount(path, info)
	if err != nil ||
		!info.Mode().IsRegular() ||
		info.Mode()&os.ModeSymlink != 0 ||
		links != 1 {
		return nil, ErrUnsafeState
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, ErrUnsafeState
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, ErrUnsafeState
	}
	return raw, nil
}

func hashOwnedPath(path string) (string, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", false, ErrUnsafeState
	}
	hash := sha256.New()
	if info.Mode().IsRegular() {
		links, err := nativeFileLinkCount(path, info)
		if err != nil || links != 1 {
			return "", false, ErrUnsafeState
		}
		file, err := os.Open(path)
		if err != nil {
			return "", false, err
		}
		opened, statErr := file.Stat()
		if statErr != nil || !os.SameFile(info, opened) {
			_ = file.Close()
			return "", false, ErrUnsafeState
		}
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return "", false, copyErr
		}
		if closeErr != nil {
			return "", false, closeErr
		}
		return hex.EncodeToString(hash.Sum(nil)), true, nil
	}
	if !info.IsDir() {
		return "", false, ErrUnsafeState
	}
	var entries []string
	err = filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return ErrUnsafeState
		}
		if current != path {
			relative, err := filepath.Rel(path, current)
			if err != nil {
				return err
			}
			entries = append(entries, filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	sort.Strings(entries)
	for _, relative := range entries {
		current := filepath.Join(path, filepath.FromSlash(relative))
		info, err := os.Lstat(current)
		if err != nil {
			return "", false, err
		}
		_, _ = io.WriteString(hash, relative)
		_, _ = io.WriteString(hash, "\x00")
		if info.Mode().IsRegular() {
			links, err := nativeFileLinkCount(current, info)
			if err != nil || links != 1 {
				return "", false, ErrUnsafeState
			}
			file, err := os.Open(current)
			if err != nil {
				return "", false, err
			}
			opened, statErr := file.Stat()
			if statErr != nil || !os.SameFile(info, opened) {
				_ = file.Close()
				return "", false, ErrUnsafeState
			}
			_, copyErr := io.Copy(hash, file)
			closeErr := file.Close()
			if copyErr != nil {
				return "", false, copyErr
			}
			if closeErr != nil {
				return "", false, closeErr
			}
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), true, nil
}

func removeExactArtifact(path, expectedDigest string) error {
	digest, present, err := hashOwnedPath(path)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if digest != expectedDigest {
		return ErrStateIdentity
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return os.Remove(path)
	}
	var entries []string
	if err := filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return ErrUnsafeState
		}
		entries = append(entries, current)
		return nil
	}); err != nil {
		return err
	}
	sort.Slice(entries, func(left, right int) bool {
		return len(entries[left]) > len(entries[right])
	})
	for _, entry := range entries {
		info, err := os.Lstat(entry)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafeState
		}
		if err := os.Remove(entry); err != nil {
			return err
		}
	}
	return nil
}

func renderManagedFile(
	managed managedFile,
	request hostbootstrap.Request,
	principal reviewedPrincipalState,
) (string, error) {
	authorizedKeysDirectory := filepath.Dir(request.Binding.Account.AuthorizedKeysPath)
	if request.Binding.Target.Platform != hostbootstrap.PlatformWindows {
		authorizedKeysDirectory = path.Dir(request.Binding.Account.AuthorizedKeysPath)
	}
	values := []struct {
		placeholder string
		value       string
	}{
		{"TARGET_AUTHORIZED_KEYS_DIR", authorizedKeysDirectory},
		{"TARGET_AUTHORIZED_KEYS", request.Binding.Account.AuthorizedKeysPath},
		{"TARGET_PRODUCT", request.Binding.Product.Path},
		{"TARGET_HELPER", request.Binding.Helper.Path},
		{"TARGET_ACCOUNT", request.Binding.Account.Name},
		{"TARGET_HOME", request.Binding.Account.HomePath},
		{"TARGET_GROUP", nativeGroupName(principal)},
	}
	content := managed.Content
	for _, replacement := range values {
		placeholder, value := replacement.placeholder, replacement.value
		if strings.ContainsAny(value, "\r\n\"") {
			return "", ErrUnsupportedIdentity
		}
		if placeholder != "TARGET_GROUP" && strings.Contains(value, " ") {
			value = `"` + value + `"`
		}
		content = strings.ReplaceAll(content, placeholder, value)
	}
	if strings.Contains(content, "TARGET_") {
		return "", ErrUnsupportedIdentity
	}
	return content, nil
}

func (platform *runtimePlatform) helperDefinitionEffects(
	request hostbootstrap.Request,
	principal reviewedPrincipalState,
) ([]nativeEffect, error) {
	effects := make([]nativeEffect, 0, len(platform.definition.ManagedFiles))
	for index, managed := range platform.definition.ManagedFiles {
		content, err := renderManagedFile(managed, request, principal)
		if err != nil {
			return nil, err
		}
		effects = append(effects, nativeEffect{
			ID: fmt.Sprintf("helper:definition:%d", index),
			Satisfied: func() (bool, error) {
				raw, err := readBoundedRegularFile(managed.Path, 64<<10)
				if errors.Is(err, fs.ErrNotExist) {
					return false, nil
				}
				if err != nil {
					return false, err
				}
				if err := validateRootDefinition(managed.Path, managed.Mode); err != nil {
					return false, err
				}
				return string(raw) == content, nil
			},
			Apply: func() error {
				return writeRootDefinitionNoFollow(
					managed.Path,
					[]byte(content),
					managed.Mode,
				)
			},
		})
	}
	return effects, nil
}

func (platform *runtimePlatform) removeManagedDefinitions(
	request hostbootstrap.Request,
) error {
	for _, managed := range platform.definition.ManagedFiles {
		raw, err := readBoundedRegularFile(managed.Path, 64<<10)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := validateRootDefinition(managed.Path, managed.Mode); err != nil {
			return err
		}
		principal, err := platform.state.LoadPrincipal()
		if err != nil {
			return err
		}
		content, err := renderManagedFile(managed, request, principal)
		if err != nil {
			return err
		}
		expected := []byte(content)
		if !bytes.Equal(raw, expected) {
			return ErrStateIdentity
		}
		if err := os.Remove(managed.Path); err != nil {
			return err
		}
		if err := syncDirectoryForPublication(
			filepath.Dir(managed.Path),
		); err != nil {
			return err
		}
	}
	return nil
}

func (platform *runtimePlatform) managedDefinitionsAbsent(
	request hostbootstrap.Request,
) (bool, error) {
	principal, err := platform.state.LoadPrincipal()
	if err != nil {
		return false, err
	}
	for _, managed := range platform.definition.ManagedFiles {
		raw, err := readBoundedRegularFile(managed.Path, 64<<10)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if err := validateRootDefinition(managed.Path, managed.Mode); err != nil {
			return false, err
		}
		content, err := renderManagedFile(managed, request, principal)
		if err != nil {
			return false, err
		}
		if !bytes.Equal(raw, []byte(content)) {
			return false, ErrStateIdentity
		}
		return false, nil
	}
	return true, nil
}

func writeRootDefinitionNoFollow(path string, content []byte, mode fs.FileMode) error {
	if existing, err := readBoundedRegularFile(path, 64<<10); err == nil {
		if err := validateRootDefinition(path, mode); err != nil {
			return err
		}
		if bytes.Equal(existing, content) {
			return nil
		}
		return ErrForeignCollision
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	directory := filepath.Dir(path)
	if err := ensureRootDefinitionDirectory(directory); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".nvpair-definition-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return ErrForeignCollision
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	return syncDirectoryForPublication(directory)
}

func endpointProbeDefinition(
	platform hostbootstrap.Platform,
	kind hostbootstrap.ResourceKind,
) (commandSpec, []string, []string) {
	switch platform {
	case hostbootstrap.PlatformWindows:
		if kind == hostbootstrap.ResourceSSHService {
			return commandSpec{
				Path: `C:\Windows\System32\sc.exe`,
				Args: []string{"query", "sshd"},
			}, []string{"running"}, []string{}
		}
		return commandSpec{}, nil, nil
	case hostbootstrap.PlatformDarwin:
		return commandSpec{
			Path: "/usr/sbin/systemsetup",
			Args: []string{"-getremotelogin"},
		}, []string{"remote login: on"}, []string{}
	default:
		if kind == hostbootstrap.ResourceSSHService {
			return commandSpec{
					Path: "/usr/bin/systemctl",
					Args: []string{
						"show",
						"ssh",
						"--property=LoadState",
						"--property=ActiveState",
						"--property=UnitFileState",
					},
				}, []string{
					"LoadState=loaded",
					"ActiveState=active",
					"UnitFileState=enabled",
				}, []string{}
		}
		return commandSpec{
			Path: "/usr/sbin/ufw",
			Args: []string{"status", "numbered"},
		}, []string{"22/tcp", "nvidia pair ssh (owned)"}, []string{"status: active"}
	}
}

func namedProbeDefinition(
	platform hostbootstrap.Platform,
	_ hostbootstrap.ResourceKind,
) (commandSpec, []string) {
	switch platform {
	case hostbootstrap.PlatformWindows:
		return commandSpec{}, nil
	case hostbootstrap.PlatformDarwin:
		return commandSpec{
			Path: "/bin/launchctl",
			Args: []string{"print", "system/com.nvidia.nvpair.host-helper"},
		}, []string{"com.nvidia.nvpair.host-helper", darwinHelperPath}
	default:
		return commandSpec{
				Path: "/usr/bin/systemctl",
				Args: []string{
					"show",
					"nvpair-host-helper.socket",
					"--property=LoadState",
					"--property=ActiveState",
					"--property=UnitFileState",
					"--property=FragmentPath",
				},
			}, []string{
				"LoadState=loaded",
				"ActiveState=active",
				"UnitFileState=enabled",
				"FragmentPath=/etc/systemd/system/nvpair-host-helper.socket",
			}
	}
}
