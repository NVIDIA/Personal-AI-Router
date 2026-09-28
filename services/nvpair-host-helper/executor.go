// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"time"

	"nvpair-shared/hostbootstrap"
)

type localBootstrapExecutor struct{}

func (localBootstrapExecutor) Inspect(
	parent context.Context,
	request hostbootstrap.Request,
) (hostbootstrap.Status, error) {
	raw, err := runFixedBootstrap(parent, "inspect", request)
	if err != nil {
		return hostbootstrap.Status{}, err
	}
	status, err := hostbootstrap.DecodeStatus(raw)
	if err != nil ||
		status.OperationID != request.OperationID ||
		status.Binding != request.Binding ||
		status.Phase != hostbootstrap.PhaseInspect {
		return hostbootstrap.Status{}, hostbootstrap.ErrHelperProtocol
	}
	return status, nil
}

func (localBootstrapExecutor) Apply(
	parent context.Context,
	request hostbootstrap.Request,
	plan hostbootstrap.Plan,
) (hostbootstrap.Status, error) {
	raw, err := runFixedBootstrap(parent, "apply", plan)
	if err != nil {
		return hostbootstrap.Status{}, err
	}
	status, err := hostbootstrap.DecodeStatus(raw)
	if err != nil ||
		status.OperationID != request.OperationID ||
		status.Binding != request.Binding ||
		status.Decision != plan.Decision ||
		(status.Phase != hostbootstrap.PhaseApply &&
			status.Phase != hostbootstrap.PhaseVerify) {
		return hostbootstrap.Status{}, hostbootstrap.ErrHelperProtocol
	}
	return status, nil
}

func (localBootstrapExecutor) Verify(
	parent context.Context,
	request hostbootstrap.Request,
	plan hostbootstrap.Plan,
) (hostbootstrap.Receipt, error) {
	raw, err := runFixedBootstrap(parent, "verify", plan)
	if err != nil {
		return hostbootstrap.Receipt{}, err
	}
	receipt, err := hostbootstrap.DecodeReceipt(raw)
	if err != nil ||
		receipt.OperationID != request.OperationID ||
		receipt.Binding != request.Binding ||
		receipt.Decision != plan.Decision {
		return hostbootstrap.Receipt{}, hostbootstrap.ErrHelperProtocol
	}
	return receipt, nil
}

func runFixedBootstrap(
	parent context.Context,
	verb string,
	document any,
) ([]byte, error) {
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, hostbootstrap.ErrHelperProtocol
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, fixedBootstrapPath(), verb)
	command.Stdin = bytes.NewReader(raw)
	output := &helperCommandOutput{limit: 64 << 10}
	command.Stdout = output
	command.Stderr = output
	command.WaitDelay = time.Second
	if err := command.Run(); err != nil {
		return nil, errors.New("fixed bootstrap action failed")
	}
	if output.exceeded {
		return nil, errors.New("fixed bootstrap output exceeded its limit")
	}
	return append([]byte(nil), output.buffer.Bytes()...), nil
}

type helperCommandOutput struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (output *helperCommandOutput) Write(data []byte) (int, error) {
	if output.buffer.Len()+len(data) > output.limit {
		output.exceeded = true
		return 0, errors.New("fixed bootstrap output exceeded its limit")
	}
	return output.buffer.Write(data)
}
