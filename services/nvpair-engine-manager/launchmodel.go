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

// launchEdit is one change to an engine's saved launch arguments, made while
// holding the engine's operation lock from the first read to the last write. A
// settings edit, start or stop cannot land between reading the launch, changing
// it and putting it back, so a rollback restores exactly what this change
// replaced. Each caller keeps its own failure policy; launchEdit only supplies
// the steps.
type launchEdit struct {
	e      *Executor
	st     *engineState
	engine string
	// before is the launch state when the lock was taken.
	before settings.LaunchState
}

// editLaunch runs change with the engine's operation lock held.
func (e *Executor) editLaunch(engine string, change func(edit *launchEdit) error) error {
	st, err := e.state(engine)
	if err != nil {
		return err
	}
	st.lockOperation()
	defer st.unlockOperation()
	return change(&launchEdit{e: e, st: st, engine: engine, before: e.launchStateLocked(engine, st)})
}

// save writes launch text. A running engine is restarted on it; a stopped one
// is started only when resume is set.
func (l *launchEdit) save(ctx context.Context, launchText string, resume bool) error {
	_, err := l.e.configureLaunchLocked(ctx, l.st, settings.Configure{
		Engine:   l.engine,
		Settings: settings.Config{ServerPort: l.before.ServerPort, LaunchText: launchText},
		Resume:   resume,
	}, func() error { return nil })
	return err
}

// restore puts back the launch text this change started from, and starts the
// engine again if it was running then, unless a Stop or Eject is waiting: the
// user asked for the engine to be off, so it is left stopped. It does not
// depend on the caller still waiting: a remote caller that gives up cancels ctx
// mid-start, and a restore under that ctx would fail at once and leave the
// engine down.
func (l *launchEdit) restore(ctx context.Context) error {
	return l.save(context.WithoutCancel(ctx), l.before.LaunchText, l.before.Running && !l.e.stopWaiting(l.st))
}

// start starts the stopped engine on its saved launch and records that the
// user wants it on, as Start does.
func (l *launchEdit) start(ctx context.Context) error {
	if err := l.e.doStart(ctx, l.st, l.engine, startOpts{}); err != nil {
		return err
	}
	return l.e.setDesiredEnabled(l.engine, true)
}

// switchLaunchModel is the launch_model action. An engine that serves one model
// per process has no load call: the model it serves is a launch argument, so
// loading a model means writing that argument into the saved launch settings
// and starting the engine on it. This is the same path the settings editor
// takes, so the saved launch text always shows what the engine is running.
//
// A running engine is restarted on the new model. A stopped engine, such as
// after Eject, is started on it. Either way, if the new model does not come up
// (it does not fit in GPU memory, or the id does not exist) the previous launch
// is put back, and a running engine is started on it again, so a failed switch
// does not leave the engine down or the settings pointing at a model that
// failed.
//
// A Stop or Eject during the Load ends it without starting anything: the engine
// stays stopped with the previous model selected. That is what the user asked
// for, not a failure, so the reply succeeds with stopped set, and only a restore
// that fails is an error.
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

	var restarted, stopped bool
	err := e.editLaunch(engine, func(edit *launchEdit) error {
		current := edit.before
		if !current.Editable {
			return fmt.Errorf("the launch settings of engine %q cannot be changed: %s", engine, current.Reason)
		}
		text, changed, err := withLaunchFlag(current.LaunchText, spec.Flag, model)
		if err != nil {
			return err
		}
		if !changed && current.Running {
			return nil
		}
		restarted = true
		if current.Running {
			err = edit.save(ctx, text, false)
		} else {
			if changed {
				if err := edit.save(ctx, text, false); err != nil {
					return err
				}
			}
			err = edit.start(ctx)
		}
		if err == nil {
			return nil
		}
		if e.stopWaiting(edit.st) {
			stopped, restarted = true, false
			if !changed {
				return nil
			}
			if restoreErr := edit.restore(ctx); restoreErr != nil {
				return fmt.Errorf("loading %s was stopped, and restoring the previous model failed: %w", model, restoreErr)
			}
			return nil
		}
		if !changed {
			return fmt.Errorf("starting %s failed: %w", model, err)
		}
		// The engine's own error carries a long log tail, so say what happened first.
		if restoreErr := edit.restore(ctx); restoreErr != nil {
			return fmt.Errorf("switching to %s failed and restoring the previous model also failed (%v): %w", model, restoreErr, err)
		}
		return fmt.Errorf("switching to %s failed; the previous model was restored: %w", model, err)
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Model     string `json:"model"`
		Restarted bool   `json:"restarted"`
		Stopped   bool   `json:"stopped,omitempty"`
	}{model, restarted, stopped})
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
	rest, at := dropLaunchFlag(tokens, flag)
	out := slices.Insert(rest, at, flag, value)
	result, err = formatLaunchText(out)
	return result, !slices.Equal(tokens, out), err
}

// withoutLaunchFlag returns launch text with every "flag value" and
// "flag=value" before any "--" terminator removed, and whether any was.
func withoutLaunchFlag(text, flag string) (result string, changed bool, err error) {
	tokens, err := parseLaunchText(text)
	if err != nil {
		return "", false, err
	}
	rest, _ := dropLaunchFlag(tokens, flag)
	result, err = formatLaunchText(rest)
	return result, len(rest) != len(tokens), err
}

// dropLaunchFlag returns the arguments without any "flag value" or "flag=value"
// before a "--" terminator, and the index in that result where the flag first
// stood. Without the flag, the index is the terminator's, or the end.
// Arguments are a list rather than key/value pairs: flags repeat, may take no
// value, and keep their order, so the list is walked rather than looked up.
func dropLaunchFlag(tokens []string, flag string) (rest []string, at int) {
	rest = make([]string, 0, len(tokens)+2)
	at = -1
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		switch {
		case token == "--":
			if at < 0 {
				at = len(rest)
			}
			return append(rest, tokens[i:]...), at
		case token == flag || strings.HasPrefix(token, flag+"="):
			if at < 0 {
				at = len(rest)
			}
			if token == flag {
				i++ // its value
			}
		default:
			rest = append(rest, token)
		}
	}
	if at < 0 {
		at = len(rest)
	}
	return rest, at
}

// modelLaunchFlag is the flag an engine's launch_model action writes, or "" for
// an engine that does not choose its model by launch argument.
func modelLaunchFlag(m *Manifest) string {
	for _, act := range m.Actions {
		if act.LaunchModel != nil {
			return act.LaunchModel.Flag
		}
	}
	return ""
}

// ejectByStopping ejects the served model by stopping the engine, which is the
// only way an engine with one model per process gives its GPU memory back. A
// stopped engine has nothing loaded, so that is a successful no-op. A model the
// engine reports it is not serving is refused, since stopping would eject the
// wrong one.
// The served model is asked of the engine itself, not read from the saved
// launch arguments, because an adopted engine may have been started on another.
func (e *Executor) ejectByStopping(ctx context.Context, engine string, params json.RawMessage) (json.RawMessage, error) {
	var request struct {
		Model string `json:"model"`
	}
	if len(params) > 0 && string(params) != "null" {
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, errors.New(`unload params must be {"model": "<model id>"}`)
		}
	}
	model := strings.TrimSpace(request.Model)
	stopped := false
	st, err := e.state(engine)
	if err != nil {
		return nil, err
	}
	// Eject is a stop: a start in flight, such as a Load restarting the engine,
	// is cancelled rather than waited out.
	done, cancelledStart := e.requestStop(st)
	defer done()
	err = e.editLaunch(engine, func(edit *launchEdit) error {
		if !edit.before.Running {
			// A start this Eject cancelled leaves the engine stopped; record that
			// the user wants it off, as a Stop would, so it is not restarted.
			if cancelledStart {
				return e.setDesiredEnabled(engine, false)
			}
			return nil
		}
		// When the engine cannot say what it serves, it is stopped anyway: Eject
		// exists to free the GPU, and stopping the engine is all it can do.
		if model != "" {
			if serving, err := e.servingModels(ctx, edit.st, engine); err == nil && !slices.Contains(serving, model) {
				return fmt.Errorf("%s is not the model %s is serving (%s)", model, engine, strings.Join(serving, ", "))
			}
		}
		stopped = true
		// Like Stop, this is explicit OFF intent; see Stop for why it is
		// persisted even when the stop itself fails.
		return errors.Join(e.doStop(edit.st, engine), e.setDesiredEnabled(engine, false))
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"model": request.Model, "stopped": stopped})
}

// servingModels asks a running engine which models it is serving, through its
// loaded_models action, and returns every name it reports for them: the name
// requests use and, for an engine that declares an inventory_field, the cached
// model id too. Callers hold st.opMu; this does not take it.
func (e *Executor) servingModels(ctx context.Context, st *engineState, engine string) ([]string, error) {
	act, ok := st.manifest.Actions["loaded_models"]
	if !ok || act.Result == nil {
		return nil, fmt.Errorf("engine %q does not report the models it serves", engine)
	}
	raw, err := e.dispatchAction(ctx, st, engine, "loaded_models", act, nil)
	if err != nil {
		return nil, err
	}
	names, ok := extractStringsResult(raw, act.Result)
	if !ok {
		return nil, fmt.Errorf("engine %q returned an unreadable model list", engine)
	}
	if act.Result.InventoryField != "" {
		inventory := *act.Result
		inventory.Field = act.Result.InventoryField
		ids, ok := extractStringsResult(raw, &inventory)
		if !ok {
			return nil, fmt.Errorf("engine %q returned an unreadable model list", engine)
		}
		names = append(names, ids...)
	}
	return names, nil
}
