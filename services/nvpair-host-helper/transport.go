// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync"
	"time"

	"nvpair-shared/hostbootstrap"
)

func runHelperServer(ctx context.Context, console bool) error {
	root := fixedStateDirectory()
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return hostbootstrap.ErrHelperProtocol
	}
	state := newDiskHelperState(root)
	principal, err := state.LoadPrincipal()
	if err != nil {
		return err
	}
	_, steadyState, err := state.AuthorizeStartup(principal)
	if err != nil {
		return err
	}
	listener, err := listenLocal(principal, console, steadyState)
	if err != nil {
		return err
	}
	defer listener.Close()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	var connections sync.WaitGroup
	defer connections.Wait()
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		connections.Add(1)
		go func() {
			defer connections.Done()
			_ = handleConnection(
				ctx,
				connection,
				nativePeerAuthorizer{},
				state,
				localBootstrapExecutor{},
			)
		}()
	}
}

func callLocalHelper(
	ctx context.Context,
	request hostbootstrap.HelperRequest,
) (hostbootstrap.HelperResponse, error) {
	raw, err := hostbootstrap.EncodeHelperRequest(request)
	if err != nil {
		return hostbootstrap.HelperResponse{}, err
	}
	connection, err := dialLocal(ctx)
	if err != nil {
		return hostbootstrap.HelperResponse{}, err
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	writeDeadline := time.Now().Add(helperWriteTimeout)
	if parent, ok := ctx.Deadline(); ok && parent.Before(writeDeadline) {
		writeDeadline = parent
	}
	if err := connection.SetWriteDeadline(writeDeadline); err != nil {
		return hostbootstrap.HelperResponse{}, err
	}
	if err := hostbootstrap.WriteHelperFrame(connection, raw); err != nil {
		return hostbootstrap.HelperResponse{}, err
	}
	if err := connection.SetWriteDeadline(time.Time{}); err != nil {
		return hostbootstrap.HelperResponse{}, err
	}
	readDeadline := time.Now().Add(helperExecuteTimeout + helperReadTimeout)
	if parent, ok := ctx.Deadline(); ok && parent.Before(readDeadline) {
		readDeadline = parent
	}
	if err := connection.SetReadDeadline(readDeadline); err != nil {
		return hostbootstrap.HelperResponse{}, err
	}
	responseRaw, err := hostbootstrap.ReadHelperFrame(connection)
	if err != nil {
		return hostbootstrap.HelperResponse{}, err
	}
	response, err := hostbootstrap.DecodeHelperResponse(responseRaw)
	if err != nil {
		return hostbootstrap.HelperResponse{}, err
	}
	if err := validateHelperResponseCorrelation(request, response); err != nil {
		return hostbootstrap.HelperResponse{}, err
	}
	if !response.Accepted {
		return response, errors.New(response.Reason)
	}
	return response, nil
}

func validateHelperResponseCorrelation(
	request hostbootstrap.HelperRequest,
	response hostbootstrap.HelperResponse,
) error {
	if response.SchemaVersion != hostbootstrap.SchemaVersion ||
		response.OperationID != request.OperationID ||
		response.Action != request.Action {
		return hostbootstrap.ErrHelperProtocol
	}
	return nil
}

func readHelperRequest(reader io.Reader) (hostbootstrap.HelperRequest, error) {
	raw, err := io.ReadAll(
		io.LimitReader(reader, hostbootstrap.MaxHelperFrameBytes+1),
	)
	if err != nil {
		return hostbootstrap.HelperRequest{}, err
	}
	return hostbootstrap.DecodeHelperRequest(raw)
}

type nativePeerAuthorizer struct{}

func (nativePeerAuthorizer) Authorize(
	connection net.Conn,
	principal reviewedPrincipal,
) error {
	identity, err := nativePeerIdentity(connection)
	if err != nil {
		return ErrPeerUnauthorized
	}
	return authorizeIdentity(identity, principal)
}
