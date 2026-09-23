// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
)

// launchCommand contains exactly what is handed to exec.Command. Keeping
// resolution here gives the settings preview and actual launch one builder.
// Env is the explicitly configured environment, never the inherited one.
type launchCommand struct {
	Bin  string
	Args []string
	Env  map[string]string
}

func environmentKey(key string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}

func validEnvironmentKey(key string) bool {
	if key == "" {
		return false
	}
	for i, c := range key {
		if c != '_' && !(c >= 'A' && c <= 'Z') && !(c >= 'a' && c <= 'z') && !(i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func literalEnvironment(assignments []string) (map[string]string, error) {
	if _, err := formatLaunchText(append([]string{"environment"}, assignments...)); err != nil {
		return nil, err
	}
	env := make(map[string]string, len(assignments))
	seen := make(map[string]bool, len(assignments))
	for _, assignment := range assignments {
		key, value, ok := strings.Cut(assignment, "=")
		if !ok || !validEnvironmentKey(key) {
			return nil, fmt.Errorf("environment variable names must start with a letter or underscore and contain only letters, digits, or underscores")
		}
		if seen[environmentKey(key)] {
			return nil, fmt.Errorf("an environment variable is assigned more than once")
		}
		seen[environmentKey(key)] = true
		env[key] = value
	}
	return env, nil
}

// argumentText omits the manifest-owned executable and startup subcommand.
func (command launchCommand) argumentText(policy *EditableLaunch) (string, error) {
	if policy == nil || len(command.Args) < len(policy.FixedArgs) {
		return "", fmt.Errorf("engine has no editable argument definition")
	}
	return formatLaunchParts(command.Env, command.Args[len(policy.FixedArgs):])
}

func formatLaunchParts(environment map[string]string, args []string) (string, error) {
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	tokens := make([]string, 0, len(keys)+1)
	for _, key := range keys {
		// Quote assignment values consistently, including simple values, so
		// normalization does not appear to remove a user's environment quotes.
		value := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(environment[key])
		tokens = append(tokens, key+`="`+value+`"`)
	}
	argv, err := formatLaunchText(args)
	if err != nil {
		return "", err
	}
	if argv != "" {
		tokens = append(tokens, argv)
	}
	text := strings.Join(tokens, " ")
	if _, err := parseLaunchText(text); err != nil {
		return "", err
	}
	return text, nil
}

func resolveProcessLaunch(rt Runtime, binPath string, vars map[string]string) (launchCommand, error) {
	if binPath == "" {
		bin, err := resolvePlaceholders(rt.Bin, vars)
		if err != nil {
			return launchCommand{}, err
		}
		binPath = expandPath(bin)
	}
	// Resolution must not change the caller's context (in particular {bin}).
	resolvedVars := make(map[string]string, len(vars)+1)
	for key, value := range vars {
		resolvedVars[key] = value
	}
	resolvedVars["bin"] = binPath
	args, err := resolveArgs(rt.Args, resolvedVars)
	if err != nil {
		return launchCommand{}, err
	}
	env := make(map[string]string, len(rt.Env))
	for key, value := range rt.Env {
		resolved, err := resolvePlaceholders(value, resolvedVars)
		if err != nil {
			return launchCommand{}, err
		}
		env[key] = resolved
	}
	return applyLiteralLaunch(rt, launchCommand{Bin: binPath, Args: args, Env: env}, resolvedVars)
}

// resolveCommandLaunch preserves manifest expansion for trusted start steps.
// Future user arguments must be added after this step so their values remain
// literal. They must never be placed into the manifest template array.
func resolveCommandLaunch(template []string, vars map[string]string) (launchCommand, error) {
	argv, err := resolveArgs(template, vars)
	if err != nil {
		return launchCommand{}, err
	}
	if len(argv) == 0 {
		return launchCommand{}, nil
	}
	for i := range argv {
		argv[i] = expandPath(argv[i])
	}
	if argv[0] == "" {
		return launchCommand{}, fmt.Errorf("start command executable is empty")
	}
	return launchCommand{Bin: argv[0], Args: argv[1:]}, nil
}
