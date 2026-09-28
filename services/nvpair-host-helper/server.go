// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"net"
	"time"

	"nvpair-shared/hostbootstrap"
)

const (
	helperReadTimeout    = 30 * time.Second
	helperExecuteTimeout = 5 * time.Minute
	helperWriteTimeout   = 30 * time.Second
)

type reviewedPrincipal struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
	SID string `json:"sid"`
}

type peerIdentity struct {
	UID           uint32
	GID           uint32
	SID           string
	System        bool
	Administrator bool
}

type peerAuthorizer interface {
	Authorize(net.Conn, reviewedPrincipal) error
}

type helperState interface {
	LoadPrincipal() (reviewedPrincipal, error)
	LoadOperation() (hostbootstrap.Request, hostbootstrap.Plan, error)
	LoadRepairAuthorization(hostbootstrap.Request) (hostbootstrap.Receipt, ownedResourceSet, error)
	HasPending() (bool, error)
	RunMutation(func() error) error
}

type helperExecutor interface {
	Inspect(
		context.Context,
		hostbootstrap.Request,
	) (hostbootstrap.Status, error)
	Apply(
		context.Context,
		hostbootstrap.Request,
		hostbootstrap.Plan,
	) (hostbootstrap.Status, error)
	Verify(
		context.Context,
		hostbootstrap.Request,
		hostbootstrap.Plan,
	) (hostbootstrap.Receipt, error)
}

func authorizeIdentity(peer peerIdentity, principal reviewedPrincipal) error {
	if peer.System || peer.Administrator || peer.UID == 0 {
		return nil
	}
	if principal.SID != "" && peer.SID == principal.SID {
		return nil
	}
	if principal.UID != 0 && peer.UID == principal.UID {
		return nil
	}
	if principal.GID != 0 && peer.GID == principal.GID {
		return nil
	}
	return ErrPeerUnauthorized
}

func handleConnection(
	ctx context.Context,
	connection net.Conn,
	authorizer peerAuthorizer,
	state helperState,
	executor helperExecutor,
) error {
	defer connection.Close()
	if err := connection.SetReadDeadline(time.Now().Add(helperReadTimeout)); err != nil {
		return err
	}
	principal, err := state.LoadPrincipal()
	if err != nil {
		return writeFailure(connection, hostbootstrap.HelperResponse{}, err)
	}
	if err := authorizer.Authorize(connection, principal); err != nil {
		return writeFailure(
			connection,
			hostbootstrap.HelperResponse{},
			ErrPeerUnauthorized,
		)
	}
	raw, err := hostbootstrap.ReadHelperFrame(connection)
	if err != nil {
		return writeFailure(connection, hostbootstrap.HelperResponse{}, err)
	}
	request, err := hostbootstrap.DecodeHelperRequest(raw)
	if err != nil {
		return writeFailure(connection, hostbootstrap.HelperResponse{}, err)
	}
	if err := connection.SetReadDeadline(time.Time{}); err != nil {
		return err
	}
	response := hostbootstrap.HelperResponse{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   request.OperationID,
		Action:        request.Action,
	}
	if request.Action == hostbootstrap.HelperActionRankReconcile {
		response.Accepted = true
		if err := connection.SetWriteDeadline(time.Now().Add(helperWriteTimeout)); err != nil {
			return err
		}
		return writeResponse(connection, response)
	}
	executeCtx, cancel := context.WithTimeout(ctx, helperExecuteTimeout)
	defer cancel()
	execute := func() error {
		storedRequest, storedPlan, err := state.LoadOperation()
		if err != nil {
			return err
		}
		if request.OperationID != storedRequest.OperationID ||
			storedPlan.OperationID != storedRequest.OperationID ||
			storedPlan.Binding != storedRequest.Binding {
			return hostbootstrap.ErrHelperProtocol
		}
		switch request.Action {
		case hostbootstrap.HelperActionInspect:
			status, err := executor.Inspect(executeCtx, storedRequest)
			if err != nil {
				return err
			}
			response.Status = &status
			return nil
		case hostbootstrap.HelperActionApply:
			receipt, markers, err := state.LoadRepairAuthorization(
				storedRequest,
			)
			if err != nil {
				return err
			}
			if err := validateHelperRepairAuthorization(
				storedRequest,
				receipt,
				markers,
			); err != nil {
				return err
			}
			pending, err := state.HasPending()
			if err != nil {
				return err
			}
			if pending {
				if storedPlan.Decision != hostbootstrap.DecisionRepairOwned {
					return hostbootstrap.ErrHelperProtocol
				}
			} else {
				status, err := executor.Inspect(executeCtx, storedRequest)
				if err != nil {
					return err
				}
				storedPlan, err = deriveHelperRepairPlan(
					storedRequest,
					receipt,
					markers,
					status.Observed,
				)
				if err != nil {
					return err
				}
			}
			status, err := executor.Apply(
				executeCtx,
				storedRequest,
				storedPlan,
			)
			if err != nil {
				return err
			}
			response.Status = &status
			return nil
		case hostbootstrap.HelperActionVerify:
			receipt, err := executor.Verify(
				executeCtx,
				storedRequest,
				storedPlan,
			)
			if err != nil {
				return err
			}
			response.Receipt = &receipt
			return nil
		default:
			return hostbootstrap.ErrHelperProtocol
		}
	}
	mutating := request.Action == hostbootstrap.HelperActionApply ||
		request.Action == hostbootstrap.HelperActionVerify
	if mutating {
		err = state.RunMutation(execute)
	} else {
		err = execute()
	}
	if err != nil {
		return writeFailure(connection, response, err)
	}
	response.Accepted = true
	if err := connection.SetWriteDeadline(time.Now().Add(helperWriteTimeout)); err != nil {
		return err
	}
	return writeResponse(connection, response)
}

func writeFailure(
	connection net.Conn,
	response hostbootstrap.HelperResponse,
	cause error,
) error {
	if err := connection.SetWriteDeadline(time.Now().Add(helperWriteTimeout)); err != nil {
		return err
	}
	response.SchemaVersion = hostbootstrap.SchemaVersion
	response.Accepted = false
	response.Status = nil
	response.Receipt = nil
	response.Reason = publicReason(cause)
	if err := writeResponse(connection, response); err != nil {
		return err
	}
	return cause
}

func writeResponse(
	connection net.Conn,
	response hostbootstrap.HelperResponse,
) error {
	raw, err := hostbootstrap.EncodeHelperResponse(response)
	if err != nil {
		return err
	}
	return hostbootstrap.WriteHelperFrame(connection, raw)
}

func publicReason(err error) string {
	switch {
	case errors.Is(err, ErrPeerUnauthorized):
		return ErrPeerUnauthorized.Error()
	case errors.Is(err, hostbootstrap.ErrHelperFrameTooLarge):
		return hostbootstrap.ErrHelperFrameTooLarge.Error()
	default:
		return hostbootstrap.ErrHelperProtocol.Error()
	}
}
