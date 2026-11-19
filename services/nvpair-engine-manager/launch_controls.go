// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Launch bindings are strict even though unrelated manifest fields permit
// additive extensions: a misspelled security control must never be ignored.
func (p *EditableLaunch) UnmarshalJSON(data []byte) error {
	type wire EditableLaunch
	var decoded wire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	*p = EditableLaunch(decoded)
	return nil
}

func (control LaunchControl) flag() string {
	if len(control.Flags) > 0 {
		return control.Flags[0]
	}
	return ""
}

// A value template only binds known policy fields. It is not a shell template
// and never expands user text. Literal separators are regexp-escaped, and every
// captured field passes its policy validator before the value is reconstructed.
type launchBinding struct {
	fields    []string
	literals  []string
	managed   bool
	localOnly bool
}

var bindingToken = regexp.MustCompile(`\{([^{}]+)\}`)
var launchFlagName = regexp.MustCompile(`^(?:-[a-zA-Z]|--[a-zA-Z0-9_-]+)$`)

func (control LaunchControl) binding() (launchBinding, error) {
	binding := launchBinding{}
	if !utf8.ValidString(control.Value) {
		return binding, fmt.Errorf("invalid launch value template")
	}
	matches := bindingToken.FindAllStringSubmatchIndex(control.Value, -1)
	if len(matches) == 0 {
		return binding, fmt.Errorf("launch control value must bind a policy field")
	}
	seen, offset := map[string]bool{}, 0
	for _, match := range matches {
		literal, name := control.Value[offset:match[0]], control.Value[match[2]:match[3]]
		field, known := launchFields[name]
		if !known || seen[name] {
			return binding, fmt.Errorf("unknown or repeated launch policy field %q", name)
		}
		if strings.ContainsAny(literal, "{}") || len(binding.fields) > 0 && literal == "" {
			return binding, fmt.Errorf("launch fields require unambiguous literal separators")
		}
		managed := field.managed != nil
		if len(binding.fields) > 0 && (binding.managed != managed || binding.localOnly != field.localOnly) {
			return binding, fmt.Errorf("a launch control cannot mix fields with different ownership")
		}
		binding.managed, binding.localOnly = managed, field.localOnly
		binding.fields = append(binding.fields, name)
		binding.literals = append(binding.literals, literal)
		seen[name], offset = true, match[1]
	}
	tail := control.Value[offset:]
	if strings.ContainsAny(tail, "{}") {
		return binding, fmt.Errorf("invalid launch value template")
	}
	binding.literals = append(binding.literals, tail)
	if control.Implicit != nil {
		if len(control.Flags) == 0 || binding.managed {
			return binding, fmt.Errorf("implicit values require flags and cannot replace managed server fields")
		}
		if _, err := (&launchValues{}).acceptBinding(binding, *control.Implicit); err != nil {
			return binding, fmt.Errorf("invalid implicit value: %w", err)
		}
	}
	return binding, nil
}

func (control LaunchControl) localOnly() bool {
	binding, err := control.binding()
	return err == nil && binding.localOnly
}

func (p *EditableLaunch) environmentControl(name string) *LaunchControl {
	for i := range p.Controls {
		for _, alias := range p.Controls[i].Env {
			if environmentKey(alias) == environmentKey(name) {
				return &p.Controls[i]
			}
		}
	}
	return nil
}

func (p *EditableLaunch) validateControls() error {
	flags, env, fields := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, control := range p.Controls {
		binding, err := control.binding()
		if err != nil {
			return err
		}
		for _, name := range binding.fields {
			fields[name] = true
		}
		if len(control.Flags)+len(control.Env) == 0 {
			return fmt.Errorf("launch controls require a flag or environment source")
		}
		for _, name := range control.Flags {
			if !launchFlagName.MatchString(name) || flags[name] {
				return fmt.Errorf("invalid or duplicate networking option %q", name)
			}
			flags[name] = true
		}
		for _, name := range control.Env {
			key := environmentKey(name)
			if !validEnvironmentKey(name) || env[key] {
				return fmt.Errorf("invalid or duplicate networking environment name")
			}
			env[key] = true
		}
	}
	for name, field := range launchFields {
		if field.managed != nil && !fields[name] {
			return fmt.Errorf("editable launch controls must bind managed field %s", name)
		}
	}
	return nil
}

// Match exact long/short names, equals values and attached short values. A short
// bundle containing a protected option is ambiguous without knowing all other
// vendor options' arities; reject it instead of guessing or letting it bypass us.
func (p *EditableLaunch) readControl(tokens []string, index int) (*LaunchControl, string, int, error) {
	token := tokens[index]
	for ci := range p.Controls {
		control := &p.Controls[ci]
		for _, name := range control.Flags {
			value, attached := "", false
			switch {
			case token == name:
			case strings.HasPrefix(token, name+"="):
				value, attached = token[len(name)+1:], true
			case len(name) == 2 && strings.HasPrefix(token, name) && len(token) > 2:
				value, attached = token[2:], true
			default:
				continue
			}
			if control.Implicit != nil {
				if attached {
					return control, "", index, fmt.Errorf("this option does not take a value")
				}
				return control, *control.Implicit, index, nil
			}
			if !attached {
				index++
				if index >= len(tokens) {
					return control, "", index, fmt.Errorf("a networking option is missing its value")
				}
				value = tokens[index]
			}
			return control, value, index, nil
		}
	}
	if strings.HasPrefix(token, "-") && !strings.HasPrefix(token, "--") && len(token) > 2 {
		for _, control := range p.Controls {
			for _, name := range control.Flags {
				if len(name) == 2 && strings.Contains(token[2:], name[1:]) {
					return nil, "", index, fmt.Errorf("write managed short networking options separately, not in a short-option bundle")
				}
			}
		}
	}
	return nil, "", index, nil
}

// All inputs use the same field-level normalization and conflict detection,
// whether they arrive as a flag, environment alias, or part of a combined value.
type launchValues struct {
	host   string
	fields map[string]string
}

func (values *launchValues) accept(control *LaunchControl, value string) (string, error) {
	binding, err := control.binding()
	if err != nil {
		return "", err
	}
	return values.acceptBinding(binding, value)
}

func (values *launchValues) acceptBinding(binding launchBinding, value string) (string, error) {
	// Field grammars disambiguate formats such as port:IPv6-host; unconstrained
	// greedy captures could split on a colon inside the host instead.
	pattern := "(?s)^"
	for i, name := range binding.fields {
		pattern += regexp.QuoteMeta(binding.literals[i]) + "(" + launchFields[name].match(values.host) + ")"
	}
	compiled, err := regexp.Compile(pattern + regexp.QuoteMeta(binding.literals[len(binding.fields)]) + "$")
	if err != nil {
		return "", fmt.Errorf("invalid launch value template")
	}
	parts := compiled.FindStringSubmatch(value)
	if parts == nil {
		return "", fmt.Errorf("value does not match its declared launch format")
	}
	if values.fields == nil {
		values.fields = map[string]string{}
	}
	output := ""
	for i, name := range binding.fields {
		field := launchFields[name]
		normalized, err := field.normalize(parts[i+1], values.host)
		if err != nil {
			return "", err
		}
		key := normalized
		if field.canonical != nil {
			key = field.canonical(key)
		}
		if previous, exists := values.fields[name]; exists && previous != key {
			return "", fmt.Errorf("conflicting values for %s", name)
		}
		values.fields[name] = key
		output += binding.literals[i] + normalized
	}
	return output + binding.literals[len(binding.fields)], nil
}

func (control LaunchControl) managedValue(host, port string) (string, bool) {
	binding, err := control.binding()
	if err != nil || !binding.managed {
		return "", false
	}
	output := ""
	for i, name := range binding.fields {
		output += binding.literals[i] + launchFields[name].managed(host, port)
	}
	return output + binding.literals[len(binding.fields)], true
}

func (values *launchValues) serverPort() int {
	port, _ := strconv.Atoi(values.fields["server.port"])
	return port
}

func (values *launchValues) localPolicy() []string {
	out := []string{}
	for name, value := range values.fields {
		// Preserve presence, including false/empty: omission may restore an unsafe
		// inherited or vendor default value.
		if launchFields[name].localOnly {
			out = append(out, name+"="+value)
		}
	}
	slices.Sort(out)
	return out
}

// Recheck the actual launch, including overrides saved by older PAIR versions.
// Display/preview resolution stays read-only and can still show those settings
// for repair; unsafe persisted aliases must never escape via a later Start.
func validateEffectiveLaunch(rt Runtime, command launchCommand, host, port string) error {
	p := rt.EditableLaunch
	if p == nil {
		return nil
	}
	values := launchValues{host: host}
	for name, value := range command.Env {
		if control := p.environmentControl(name); control != nil {
			if _, err := values.accept(control, value); err != nil {
				return err
			}
		}
	}
	if len(command.Args) < len(p.FixedArgs) || !slices.Equal(command.Args[:len(p.FixedArgs)], p.FixedArgs) {
		return fmt.Errorf("launch does not match its fixed startup arguments")
	}
	args := command.Args[len(p.FixedArgs):]
	for i := 0; i < len(args); i++ {
		control, value, last, err := p.readControl(args, i)
		if err != nil {
			return err
		}
		i = last
		if control != nil {
			if _, err := values.accept(control, value); err != nil {
				return err
			}
		}
	}
	if values.serverPort() != 0 && strconv.Itoa(values.serverPort()) != port {
		return fmt.Errorf("saved launch port does not match the managed server port")
	}
	return nil
}
