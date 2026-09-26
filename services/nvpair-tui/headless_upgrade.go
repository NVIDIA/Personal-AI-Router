// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path"
	"strings"
)

// The parent retains the reviewed peer identity, binary manifests and journal.
// This fixed-unit helper accepts no shell text, alternate unit or data path.
type headlessUpgradeRequest struct {
	Phase              string `json:"phase"`
	ExpectedUnitSHA256 string `json:"expectedUnitSha256"`
	OldExecutable      string `json:"oldExecutable"`
	OldBroker          string `json:"oldBroker"`
	ConfigHome         string `json:"configHome"`
}

type headlessUpgradePlan struct {
	request     headlessUpgradeRequest
	original    string
	drain       string
	replacement string
	executable  string
}

func planHeadlessUpgrade(request headlessUpgradeRequest, executable, broker, config string) (headlessUpgradePlan, error) {
	plan := headlessUpgradePlan{request: request, executable: executable}
	if request.Phase != "stop" && request.Phase != "install" && request.Phase != "rollback" {
		return plan, errors.New("unsupported reviewed upgrade phase")
	}
	if path.Base(request.OldExecutable) != "nvpair-tui" || path.Base(request.OldBroker) != "nvpair-ui-broker" || path.Base(executable) != "nvpair-tui" || path.Base(broker) != "nvpair-ui-broker" {
		return plan, errors.New("reviewed upgrade requires the installed PAIR parent and broker")
	}
	if request.ConfigHome != config || len(request.ExpectedUnitSHA256) != 64 || request.ExpectedUnitSHA256 != strings.ToLower(request.ExpectedUnitSHA256) {
		return plan, errors.New("reviewed upgrade configuration or unit digest is invalid")
	}
	if _, err := hex.DecodeString(request.ExpectedUnitSHA256); err != nil {
		return plan, errors.New("reviewed unit digest is invalid")
	}
	var err error
	plan.drain, err = headlessUnitText(request.OldExecutable, request.OldBroker, config)
	if err != nil {
		return plan, err
	}
	plan.replacement, err = headlessUnitText(executable, broker, config)
	if err != nil {
		return plan, err
	}
	if len(plan.replacement) > 8192 || len(plan.drain) > 8192 {
		return plan, errors.New("reviewed unit exceeds its supported size")
	}
	for _, seconds := range []int{25, headlessUnitStopSeconds} {
		candidate, err := headlessUnitTextWithStop(request.OldExecutable, request.OldBroker, config, seconds)
		if err != nil {
			return plan, err
		}
		digest := sha256.Sum256([]byte(candidate))
		if hex.EncodeToString(digest[:]) == request.ExpectedUnitSHA256 {
			plan.original = candidate
			return plan, nil
		}
	}
	return plan, errors.New("reviewed old unit is not a supported complete PAIR definition")
}

// The narrow native seam lets the lifecycle be exercised without systemd or a
// listener. Read/replace enforce private file ownership; state verifies the
// fixed loaded fragment, lack of overrides and exact running executable.
type headlessUpgradeHost interface {
	readUnit() (string, error)
	state(context.Context, string) (string, error)
	replaceUnit(string, string) error
	systemctl(context.Context, string) error
}

func executeHeadlessUpgrade(ctx context.Context, plan headlessUpgradePlan, host headlessUpgradeHost) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current, err := host.readUnit()
	if err != nil {
		return nil, err
	}
	if current != plan.original && current != plan.drain && current != plan.replacement {
		return nil, errors.New("reviewed unit changed; no upgrade action was performed")
	}
	command := func(action string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		observed, err := host.readUnit()
		if err != nil || observed != current {
			return errors.New("unit changed before the next reviewed service action")
		}
		return host.systemctl(ctx, action)
	}
	stop := func(executable string) error {
		state, err := host.state(ctx, executable)
		if err != nil {
			return err
		}
		if state != "stopped" {
			if err := command("stop"); err != nil {
				return err
			}
		}
		state, err = host.state(ctx, executable)
		if err != nil || state != "stopped" {
			return errors.New("old service termination is unconfirmed; unit and data were retained")
		}
		return nil
	}
	replace := func(content string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if current != content {
			if err := host.replaceUnit(current, content); err != nil {
				return err
			}
			current = content
		}
		return command("daemon-reload")
	}
	state := ""
	switch plan.request.Phase {
	case "stop":
		if current == plan.replacement && current != plan.drain {
			return nil, errors.New("upgrade already replaced the unit; resume its retained later phase")
		}
		if _, err = host.state(ctx, plan.request.OldExecutable); err != nil {
			return nil, err
		}
		// Raise only the reviewed old unit's stop budget before asking it to
		// drain. A lost reply accepts this exactly derived body on retry.
		if err = replace(plan.drain); err != nil {
			return nil, err
		}
		if err = stop(plan.request.OldExecutable); err != nil {
			return nil, err
		}
		state = "stopped"
	case "install":
		if current == plan.replacement {
			if err = command("daemon-reload"); err != nil {
				return nil, err
			}
			if _, err = host.state(ctx, plan.executable); err != nil {
				return nil, err
			}
		} else {
			observed, err := host.state(ctx, plan.request.OldExecutable)
			if err != nil || observed != "stopped" {
				return nil, errors.New("reviewed old service must be stopped before unit replacement")
			}
			if err = replace(plan.replacement); err != nil {
				return nil, err
			}
			if _, err = host.state(ctx, plan.executable); err != nil {
				return nil, err
			}
		}
		state = "installed"
	case "rollback":
		expected := plan.request.OldExecutable
		if current == plan.replacement && current != plan.original {
			expected = plan.executable
		}
		// An already restored and running old owner is an idempotent result.
		observed, err := host.state(ctx, expected)
		if err != nil {
			return nil, err
		}
		if current == plan.drain && current != plan.original {
			// A failed reload can leave the old process running with the old
			// 25-second manager budget. Restore its definition without stopping
			// that already-correct owner during rollback.
			if err = replace(plan.original); err != nil {
				return nil, err
			}
		} else if current != plan.original {
			if err = command("daemon-reload"); err != nil {
				return nil, err
			}
			if err = stop(expected); err != nil {
				return nil, err
			}
			if err = replace(plan.original); err != nil {
				return nil, err
			}
			observed = "stopped"
		}
		if observed == "stopped" {
			if err = command("daemon-reload"); err != nil {
				return nil, err
			}
			if err = command("start"); err != nil {
				return nil, err
			}
		}
		if observed, err = host.state(ctx, plan.request.OldExecutable); err != nil || observed != "running" {
			return nil, errors.New("restored service startup is unconfirmed; retain the upgrade journal")
		}
		state = "rolled-back"
	}
	if final, err := host.readUnit(); err != nil || final != current {
		return nil, errors.New("unit changed after the reviewed phase; completion is unconfirmed")
	}
	return map[string]any{"unit": headlessUnit, "operation": "upgrade", "phase": plan.request.Phase, "state": state, "cleanupConfirmed": true}, nil
}

func readHeadlessUpgrade(ctx context.Context, input io.Reader) (headlessUpgradeRequest, error) {
	type result struct {
		body []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		body, err := io.ReadAll(io.LimitReader(input, headlessRequestLimit+1))
		done <- result{body, err}
	}()
	var request headlessUpgradeRequest
	select {
	case got := <-done:
		if got.err != nil {
			return request, errors.New("cannot read the reviewed upgrade request")
		}
		err := decodeHeadless(got.body, &request)
		return request, err
	case <-ctx.Done():
		return request, errors.New("reviewed upgrade input deadline expired")
	}
}
