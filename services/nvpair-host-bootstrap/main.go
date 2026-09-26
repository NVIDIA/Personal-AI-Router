// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"nvpair-shared/hostbootstrap"
)

var Version = "dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(Version)
		return
	}
	if err := runBootstrapCLI(context.Background(), os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(2)
	}
}

func runBootstrapCLI(
	ctx context.Context,
	args []string,
	input io.Reader,
	output io.Writer,
) error {
	if len(args) != 1 {
		return errors.New("expected one fixed verb: inspect, review, apply, verify, or uninstall")
	}
	raw, err := io.ReadAll(io.LimitReader(input, (64<<10)+1))
	if err != nil || len(raw) == 0 || len(raw) > 64<<10 {
		return errors.New("bootstrap contract document is invalid")
	}
	switch args[0] {
	case "inspect", "review", "uninstall":
		request, err := hostbootstrap.DecodeRequest(raw)
		if err != nil {
			return err
		}
		bootstrap, err := newRuntimeBootstrap(request.Binding.Target)
		if err != nil {
			return err
		}
		switch args[0] {
		case "inspect":
			status, err := bootstrap.Inspect(ctx, request)
			return encodeBootstrapResult(output, status, err)
		case "review":
			plan, err := bootstrap.Review(ctx, request)
			return encodeBootstrapResult(output, plan, err)
		default:
			if err := bootstrap.Uninstall(ctx, request); err != nil {
				return err
			}
			return json.NewEncoder(output).Encode(struct {
				SchemaVersion int    `json:"schemaVersion"`
				OperationID   string `json:"operationId"`
				Uninstalled   bool   `json:"uninstalled"`
			}{
				SchemaVersion: hostbootstrap.SchemaVersion,
				OperationID:   request.OperationID,
				Uninstalled:   true,
			})
		}
	case "apply", "verify":
		plan, err := hostbootstrap.DecodePlan(raw)
		if err != nil {
			return err
		}
		bootstrap, err := newRuntimeBootstrap(plan.Binding.Target)
		if err != nil {
			return err
		}
		if args[0] == "apply" {
			var status hostbootstrap.Status
			useRepair := false
			if plan.Decision == hostbootstrap.DecisionRepairOwned {
				storedRequest, _, loadErr := bootstrap.state.LoadOperation()
				if loadErr == nil {
					useRepair = storedRequest.OperationID == plan.OperationID
				} else if !errors.Is(loadErr, ErrStateMissing) {
					return loadErr
				}
			}
			if useRepair {
				status, err = bootstrap.Repair(ctx, plan)
			} else {
				status, err = bootstrap.Apply(ctx, plan)
			}
			return encodeBootstrapResult(output, status, err)
		}
		receipt, err := bootstrap.Verify(ctx, plan)
		return encodeBootstrapResult(output, receipt, err)
	default:
		return errors.New("unsupported bootstrap verb")
	}
}

func encodeBootstrapResult(output io.Writer, value any, err error) error {
	if err != nil {
		return err
	}
	if err := json.NewEncoder(output).Encode(value); err != nil {
		return errors.New("bootstrap result could not be written")
	}
	return nil
}
