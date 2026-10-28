// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	settings "nvpair-shared/enginesettings"
)

// hfModelIDRe accepts a Hugging Face repository id, "name" or "org/name". It
// refuses anything that could be read as a flag, a path or a shell fragment,
// since the value becomes an engine argument.
var hfModelIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}(/[A-Za-z0-9][A-Za-z0-9._-]{0,95})?$`)

// switchLaunchModel is the launch_model action. An engine that serves one model
// per process has no load call: the model it serves is a launch argument, so
// "loading" a model means writing that argument into the saved launch settings
// and restarting the engine on it. This is the same path the settings editor
// takes, so the saved launch text always shows what the engine is running.
//
// A running engine is restarted, and if the new model does not come up (it does
// not fit in GPU memory, or the id does not exist) the previous model is
// restored, so a failed switch does not leave the engine down. A stopped engine
// only has its saved launch text changed.
func (e *Executor) switchLaunchModel(ctx context.Context, engine string, spec *ActionLaunchModel, params json.RawMessage) (json.RawMessage, error) {
	var request struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, errors.New(`launch_model params must be {"model": "<model id>"}`)
	}
	model := strings.TrimSpace(request.Model)
	if !hfModelIDRe.MatchString(model) {
		return nil, fmt.Errorf("%q is not a valid model id (want a Hugging Face id such as org/name)", request.Model)
	}

	current, err := e.LaunchSettings(engine)
	if err != nil {
		return nil, err
	}
	if !current.Editable {
		return nil, fmt.Errorf("the launch settings of engine %q cannot be changed: %s", engine, current.Reason)
	}
	text, changed, err := withLaunchFlag(current.LaunchText, spec.Flag, model)
	if err != nil {
		return nil, err
	}
	result := func(restarted bool) (json.RawMessage, error) {
		return json.Marshal(struct {
			Model     string `json:"model"`
			Restarted bool   `json:"restarted"`
		}{model, restarted})
	}
	if !changed {
		return result(false)
	}

	apply := func(launchText string, resume bool) error {
		_, err := e.ConfigureLaunch(ctx, settings.Configure{
			Engine:   engine,
			Settings: settings.Config{ServerPort: current.ServerPort, LaunchText: launchText},
			Resume:   resume,
		}, func() error { return nil })
		return err
	}
	if err := apply(text, false); err != nil {
		if !current.Running {
			return nil, err
		}
		// The engine's own error carries a long log tail, so say what happened first.
		if restoreErr := apply(current.LaunchText, true); restoreErr != nil {
			return nil, fmt.Errorf("switching to %s failed and restoring the previous model also failed (%v): %w", model, restoreErr, err)
		}
		return nil, fmt.Errorf("switching to %s failed; the previous model was restored: %w", model, err)
	}
	return result(current.Running)
}

// withLaunchFlag returns launch text with flag set to value. Every existing
// "flag value" or "flag=value" collapses into one "flag value" at the position
// of the first, and every other argument is kept in place; when the flag is
// absent it is added before any "--" terminator. changed is false when the
// arguments already say the same thing, whatever their quoting: the launch text
// shown to a user is quoted canonically, so comparing text would call an
// unchanged launch changed.
func withLaunchFlag(text, flag, value string) (result string, changed bool, err error) {
	tokens, err := parseLaunchText(text)
	if err != nil {
		return "", false, err
	}
	out := make([]string, 0, len(tokens)+2)
	found := false
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		switch {
		case token == "--":
			if !found {
				out = append(out, flag, value)
				found = true
			}
			out = append(out, tokens[i:]...)
			i = len(tokens)
		case token == flag:
			if !found {
				out = append(out, flag, value)
				found = true
			}
			if i+1 < len(tokens) {
				i++ // the old value
			}
		case strings.HasPrefix(token, flag+"="):
			if !found {
				out = append(out, flag, value)
				found = true
			}
		default:
			out = append(out, token)
		}
	}
	if !found {
		out = append(out, flag, value)
	}
	result, err = formatLaunchText(out)
	return result, !slices.Equal(tokens, out), err
}
