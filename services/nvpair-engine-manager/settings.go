// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strconv"
	"strings"

	settings "nvpair-shared/enginesettings"
)

func applyLiteralLaunch(rt Runtime, command launchCommand, vars map[string]string) (launchCommand, error) {
	if rt.LaunchEnv != nil {
		env, err := literalEnvironment(*rt.LaunchEnv)
		if err != nil {
			return launchCommand{}, err
		}
		// Explicit assignments override manifest defaults. Omitted defaults
		// continue to resolve from the manifest on every launch.
		for key, template := range rt.Env {
			found := false
			for supplied := range env {
				if environmentKey(supplied) == environmentKey(key) {
					found = true
					break
				}
			}
			if !found {
				value, err := resolvePlaceholders(template, vars)
				if err != nil {
					return launchCommand{}, err
				}
				env[key] = value
			}
		}
		command.Env = env
	}
	p := rt.EditableLaunch
	if p == nil {
		if rt.LaunchArgs != nil {
			return launchCommand{}, fmt.Errorf("this engine has no editable launch definition")
		}
		return command, nil
	}
	if ip := net.ParseIP(vars["host"]); ip == nil || !ip.IsLoopback() {
		return launchCommand{}, fmt.Errorf("configured engines must bind to loopback")
	}
	// Override every declared environment spelling, including inherited aliases.
	// This also makes the default preview agree with subsequent canonical launches.
	for _, control := range p.Controls {
		if value, managed := control.managedValue(vars["host"], vars["port"]); managed {
			for _, name := range control.Env {
				if command.Env == nil {
					command.Env = map[string]string{}
				}
				for supplied := range command.Env {
					if environmentKey(supplied) == environmentKey(name) {
						delete(command.Env, supplied)
					}
				}
				command.Env[name] = value
			}
		}
	}
	if rt.LaunchArgs == nil {
		return command, nil
	}
	command.Args = slices.Clone(p.FixedArgs)
	for _, control := range p.Controls {
		if value, managed := control.managedValue(vars["host"], vars["port"]); managed && control.flag() != "" {
			command.Args = append(command.Args, control.flag(), value)
		}
	}
	command.Args = append(command.Args, (*rt.LaunchArgs)...)

	return command, nil
}

func resolveRuntimeCommand(rt Runtime, index int, vars map[string]string) (launchCommand, error) {
	command, err := resolveCommandLaunch(rt.Start[index], vars)
	if err != nil {
		return launchCommand{}, err
	}
	if rt.EditableLaunch != nil && index == rt.EditableLaunch.StartIndex {
		return applyLiteralLaunch(rt, command, vars)
	}
	return command, nil
}

// launchForState is read-only; callers hold opMu, and neither preview nor a
// settings read probes ports, starts processes or writes configuration.
func launchForState(st *engineState, port int) (launchCommand, error) {
	rt := st.plat.Runtime
	vars := map[string]string{"host": effectiveBind(rt.Bind, ""), "port": strconv.Itoa(port), "install_dir": st.installDir}
	if rt.CLI != "" {
		vars["cli"] = expandPath(rt.CLI)
	}
	st.mu.Lock()
	bin := st.binPath
	st.mu.Unlock()
	if rt.modeOrDefault() == "process" {
		return resolveProcessLaunch(rt, bin, vars)
	}
	index := 0
	if rt.EditableLaunch != nil {
		index = rt.EditableLaunch.StartIndex
	}
	if index < 0 || index >= len(rt.Start) {
		return launchCommand{}, fmt.Errorf("invalid editable start command")
	}
	return resolveRuntimeCommand(rt, index, vars)
}

func (e *Executor) launchStateLocked(engine string, st *engineState) settings.LaunchState {
	st.mu.Lock()
	result := settings.LaunchState{Engine: engine, ServerPort: st.plat.Runtime.Port, EffectivePort: st.port, Running: st.running, Adopted: st.adopted, Format: launchTextFormat}
	st.mu.Unlock()
	command, err := launchForState(st, result.ServerPort)
	if err == nil {
		result.LaunchText, err = command.argumentText(st.plat.Runtime.EditableLaunch)
	}
	switch {
	case err != nil:
		result.Reason = "The engine launch configuration cannot be displayed."
	case st.plat.Runtime.EditableLaunch == nil:
		result.Reason = "This engine does not support launch settings."
	case result.Running && result.Adopted && !canMoveAdoptedEngine(st.plat.Runtime):
		result.Reason = "Stop this engine in its own application before changing settings."
	default:
		result.Editable = true
	}
	return result
}

func (e *Executor) LaunchSettings(engine string) (settings.LaunchState, error) {
	st, err := e.state(engine)
	if err != nil {
		return settings.LaunchState{}, err
	}
	st.opMu.Lock()
	defer st.opMu.Unlock()
	return e.launchStateLocked(engine, st), nil
}

// launchCORSAssignments uses the same mandatory-control reader as preview.
// Compare effective policy, not spelling, quoting, ordering or duplicate flags.
func launchCORSAssignments(text string, policy *EditableLaunch) ([]string, error) {
	if policy == nil {
		return nil, nil
	}
	tokens, err := parseLaunchText(text)
	if err != nil {
		return nil, err
	}
	values := launchValues{}
	accept := func(control *LaunchControl, value string) error {
		if control != nil && control.localOnly() {
			_, err := values.accept(control, value)
			return err
		}
		return nil
	}
	i := 0
	for ; i < len(tokens); i++ {
		key, value, assignment := strings.Cut(tokens[i], "=")
		if !assignment || strings.HasPrefix(key, "-") || strings.ContainsAny(key, "/\\") {
			break
		}
		if err := accept(policy.environmentControl(key), value); err != nil {
			return nil, err
		}
	}
	for ; i < len(tokens); i++ {
		control, value, last, err := policy.readControl(tokens, i)
		if err != nil {
			return nil, err
		}
		i = last
		if err := accept(control, value); err != nil {
			return nil, err
		}
	}
	return values.localPolicy(), nil
}

// RejectRemoteCORSChange keeps changes to browser access policy local to the
// engine's device. Other environment assignments use the normal settings path.
func (e *Executor) RejectRemoteCORSChange(request settings.Request) error {
	st, err := e.state(request.Engine)
	if err != nil {
		// An engine this node does not have has no environment to change, and
		// the relay refuses the unknown engine on its own.
		return nil
	}
	st.opMu.Lock()
	defer st.opMu.Unlock()
	return e.rejectRemoteCORSChangeLocked(st, request)
}

func (e *Executor) rejectRemoteCORSChangeLocked(st *engineState, request settings.Request) error {
	policy := st.plat.Runtime.EditableLaunch
	current, err := launchCORSAssignments(e.launchStateLocked(request.Engine, st).LaunchText, policy)
	if err != nil {
		return err
	}
	requested, err := launchCORSAssignments(request.Settings.LaunchText, policy)
	if err != nil {
		return err
	}
	if !slices.Equal(current, requested) {
		return fmt.Errorf("CORS changes are local-only; edit them on the device running this engine")
	}
	return nil
}

func (e *Executor) PreviewLaunch(p settings.Request) (settings.Preview, error) {
	st, err := e.state(p.Engine)
	if err != nil {
		return settings.Preview{}, err
	}
	st.opMu.Lock()
	defer st.opMu.Unlock()
	if p.PreserveCORS {
		if err := e.rejectRemoteCORSChangeLocked(st, p); err != nil {
			return settings.Preview{}, err
		}
	}
	return e.previewLaunchLocked(st, p), nil
}

func (e *Executor) previewLaunchLocked(st *engineState, request settings.Request) settings.Preview {
	result := settings.Preview{Settings: request.Settings, Args: []string{}, Env: []string{}, Errors: map[string]string{}}
	fail := func(message string) settings.Preview { result.Errors["launchText"] = message; return result }
	if request.Settings.ServerPort < 1 || request.Settings.ServerPort > 65535 {
		result.Errors["serverPort"] = "Enter a server port from 1 to 65535."
		return result
	}
	if err := e.reservedPortError(request.Settings.ServerPort); err != nil {
		result.Errors["serverPort"] = err.Error()
		return result
	}
	rt := st.plat.Runtime
	policy := rt.EditableLaunch
	if policy == nil {
		return fail("This engine does not support launch settings.")
	}
	if request.Resolution != "" && request.Resolution != "server" && request.Resolution != "launch" {
		return fail("Invalid port conflict resolution.")
	}
	base, err := launchForState(st, request.Settings.ServerPort)
	if err != nil {
		return fail("Cannot resolve the engine executable.")
	}
	host := effectiveBind(rt.Bind, "")
	vars := map[string]string{
		"host": host, "port": strconv.Itoa(request.Settings.ServerPort),
		"install_dir": st.installDir, "bin": base.Bin, "cli": expandPath(rt.CLI),
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fail("PAIR-managed launch settings require a loopback bind.")
	}
	tokens, err := parseLaunchText(request.Settings.LaunchText)
	if err != nil {
		return fail(err.Error())
	}
	values := launchValues{host: host}
	seenEnv := map[string]bool{}
	for len(tokens) > 0 {
		key, value, assignment := strings.Cut(tokens[0], "=")
		if !assignment || strings.HasPrefix(key, "-") || strings.ContainsAny(key, `/\`) {
			break
		}
		if !validEnvironmentKey(key) {
			return fail("Environment variable names must start with a letter or underscore and contain only letters, digits, or underscores.")
		}
		if seenEnv[environmentKey(key)] {
			return fail("An environment assignment is repeated.")
		}
		seenEnv[environmentKey(key)] = true
		managed := false
		if control := policy.environmentControl(key); control != nil {
			normalized, err := values.accept(control, value)
			if err != nil {
				return fail(err.Error())
			}
			value = normalized
			_, managed = control.managedValue(host, strconv.Itoa(request.Settings.ServerPort))
		}
		if !managed {
			// Omitted manifest defaults remain live; non-networking values stay literal.
			isDefault := false
			for declared, template := range rt.Env {
				if environmentKey(declared) == environmentKey(key) {
					resolved, err := resolvePlaceholders(template, vars)
					isDefault = err == nil && value == resolved
					break
				}
			}
			if !isDefault {
				result.Env = append(result.Env, key+"="+value)
			}
		}

		tokens = tokens[1:]
	}
	// Compatibility for journals created by the earlier full-command editor.
	// New requests contain arguments only; no executable is editable.
	if request.Format == "pair-launch-v1" {
		if len(tokens) < 1+len(policy.FixedArgs) || tokens[0] != base.Bin || !slices.Equal(tokens[1:1+len(policy.FixedArgs)], policy.FixedArgs) {
			return fail("The saved command does not match this engine's startup command.")
		}
		tokens = tokens[1+len(policy.FixedArgs):]
	} else if request.Format != "" && request.Format != launchTextFormat {
		return fail("Unsupported engine argument format.")
	}
	// Every remaining token is validated, including anything after a "--"
	// separator. PAIR appends these after its own managed --port/--bind, so a
	// last-flag-wins engine would honour "-- --bind 0.0.0.0" if the separator
	// ended validation. "--" itself is passed through as a literal argument.
	for i := 0; i < len(tokens); i++ {
		control, value, last, err := policy.readControl(tokens, i)
		if err != nil {
			return fail(err.Error())
		}
		i = last
		if control == nil {
			result.Args = append(result.Args, tokens[i])
			continue
		}
		normalized, err := values.accept(control, value)
		if err != nil {
			return fail(err.Error())
		}
		if _, managed := control.managedValue(host, strconv.Itoa(request.Settings.ServerPort)); !managed {
			result.Args = append(result.Args, control.flag())
			if control.Implicit == nil {
				result.Args = append(result.Args, normalized)
			}
		}
	}

	if values.serverPort() != 0 && values.serverPort() != request.Settings.ServerPort {
		switch request.Resolution {
		case "server":
		case "launch":
			result.Settings.ServerPort = values.serverPort()
		default:
			result.Conflict = &settings.Conflict{ServerPort: request.Settings.ServerPort, LaunchPort: values.serverPort()}
			return result
		}
	}
	if err := e.reservedPortError(result.Settings.ServerPort); err != nil {
		result.Errors["serverPort"] = err.Error()
		return result
	}
	rt.LaunchArgs = &result.Args
	rt.LaunchEnv = &result.Env
	vars["port"] = strconv.Itoa(result.Settings.ServerPort)
	command, err := applyLiteralLaunch(rt, base, vars)
	if err != nil {
		return fail(err.Error())
	}
	result.Settings.LaunchText, err = command.argumentText(policy)
	if err != nil {
		return fail(err.Error())
	}
	current := e.launchStateLocked(request.Engine, st)
	result.Restart = current.Running && (current.ServerPort != result.Settings.ServerPort || current.LaunchText != result.Settings.LaunchText)
	if !current.Editable {
		return fail(current.Reason)
	}
	return result
}

// ConfigureLaunch owns the entire engine operation lock, including the narrow
// parent rebind callback. The parent already holds the node configuration lock;
// that callback must not acquire it again. Explicit ON/OFF intent is untouched.
func (e *Executor) ConfigureLaunch(ctx context.Context, p settings.Configure, rebind func() error) (settings.LaunchState, error) {
	st, err := e.state(p.Engine)
	if err != nil {
		return settings.LaunchState{}, err
	}
	st.opMu.Lock()
	defer st.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return settings.LaunchState{}, err
	}
	preview := e.previewLaunchLocked(st, settings.Request{Engine: p.Engine, Settings: p.Settings})
	if len(preview.Errors) > 0 || preview.Conflict != nil {
		return settings.LaunchState{}, fmt.Errorf("launch configuration failed authoritative validation")
	}
	// This operation returns its one failure through the settings snapshot.
	// Do not also open the global lifecycle error dialog for the same attempt.
	ctx = context.WithValue(ctx, settingsApplyContextKey{}, true)
	e.reporter.clear(startFailedID(p.Engine))
	e.reporter.clear(exitedID(p.Engine))
	old := e.launchStateLocked(p.Engine, st)
	changed := old.ServerPort != preview.Settings.ServerPort || old.LaunchText != preview.Settings.LaunchText
	// Stop before writing the override file. A stop failure returns with the
	// engine still on its old launch, so persisting first would leave disk on
	// the new one and the next start would silently adopt a configuration the
	// user was told had failed.
	if changed && old.Running {
		if err := e.doStop(st, p.Engine); err != nil {
			return e.launchStateLocked(p.Engine, st), err
		}
	}
	if changed || !reflect.DeepEqual(st.plat.Runtime.LaunchArgs, &preview.Args) || !reflect.DeepEqual(st.plat.Runtime.LaunchEnv, &preview.Env) {
		if err := e.persistRuntimeConfig(p.Engine, preview.Settings.ServerPort, &preview.Args, &preview.Env); err != nil {
			return e.launchStateLocked(p.Engine, st), err
		}
	}
	st.mu.Lock()
	st.plat.Runtime.Port = preview.Settings.ServerPort
	st.plat.Runtime.LaunchArgs = &preview.Args
	st.plat.Runtime.LaunchEnv = &preview.Env
	st.port = preview.Settings.ServerPort
	st.mu.Unlock()
	if err := rebind(); err != nil {
		e.emitState(p.Engine)
		return e.launchStateLocked(p.Engine, st), err
	}
	resume := p.Resume
	if enabled, known, err := e.desired.get(p.Engine); err == nil && known && !enabled && !old.Running {
		resume = false
	}
	if (changed && old.Running) || resume {
		if err := e.doStart(ctx, st, p.Engine, startOpts{RequireOwned: true}); err != nil {
			e.emitState(p.Engine)
			return e.launchStateLocked(p.Engine, st), err
		}
	}
	e.emitState(p.Engine)
	return e.launchStateLocked(p.Engine, st), nil
}

type settingsApplyContextKey struct{}
