// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync/atomic"
	"time"

	"nvpair-tui/rpc"
)

const headlessFrameLimit = 1 << 20
const headlessRequestLimit = 16 << 10
const headlessRequestTimeout = 30 * time.Second

type headlessRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}
type headlessError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	RPCCode int    `json:"rpcCode,omitempty"`
}
type headlessResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *headlessError  `json:"error,omitempty"`
}

type headlessCaller interface {
	Call(context.Context, string, any) (*rpc.Message, error)
}

func decodeHeadless(raw []byte, out any) error {
	if len(raw) > headlessRequestLimit {
		return errors.New("request exceeds the supported size")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return errors.New("invalid control request")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("exactly one control request is required")
	}
	return nil
}

// Only the existing pairing/identity operations are available. This is not a
// general broker relay, an installer, or a permanent network auto-accept mode.
func validateHeadlessRequest(request headlessRequest) error {
	params := request.Params
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	var target any
	switch request.Method {
	case "headless:status", "cluster:get-node-id", "nodes:get-initial":
		target = &struct{}{}
	case "cluster:invite-node":
		target = &struct {
			Address string  `json:"address"`
			Port    *int    `json:"port,omitempty"`
			NodeID  *string `json:"nodeId,omitempty"`
		}{}
	case "cluster:respond-to-invite":
		target = &struct {
			InviteID string  `json:"inviteId"`
			Accept   *bool   `json:"accept"`
			Pin      *string `json:"pin,omitempty"`
		}{}
	case "cluster:invite-status", "cluster:cancel-invite":
		target = &struct {
			InviteID string `json:"inviteId"`
		}{}
	default:
		return errors.New("method is not available on private onboarding control")
	}
	return decodeHeadless(params, target)
}

func headlessFailure(code, message string) headlessResponse {
	return headlessResponse{Error: &headlessError{Code: code, Message: message}}
}

func callHeadless(ctx context.Context, client headlessCaller, ready bool, request headlessRequest) headlessResponse {
	if err := validateHeadlessRequest(request); err != nil {
		return headlessFailure("invalid-request", err.Error())
	}
	if request.Method == "headless:status" {
		// Ready means the broker control channel is available, not that all
		// workers, runtimes or pairing capabilities succeeded. Query identity
		// and roster explicitly through their existing methods before enrolling.
		state := "starting"
		if ready {
			state = "ready"
		}
		result, _ := json.Marshal(map[string]any{"state": state, "version": Version, "mode": "headless"})
		return headlessResponse{Result: result}
	}
	if !ready {
		return headlessFailure("not-ready", "The PAIR broker control channel is still starting")
	}
	response, err := client.Call(ctx, request.Method, request.Params)
	if err != nil {
		// Do not copy arbitrary broker error bodies, request params, or PINs into
		// logs/error strings. Structured results travel only to this private peer.
		var rpcErr *rpc.RPCError
		if errors.As(err, &rpcErr) {
			return headlessResponse{Error: &headlessError{Code: "pairing-failed", Message: "The pairing or identity request was rejected", RPCCode: rpcErr.Code}}
		}
		return headlessFailure("completion-unknown", "The request did not return a confirmed result; inspect pairing state before retrying")
	}
	if response == nil || len(response.Result) > headlessFrameLimit {
		return headlessFailure("invalid-response", "The service returned an unsupported control response")
	}
	return headlessResponse{Result: response.Result}
}

func serveHeadlessConnection(parent context.Context, conn net.Conn, client headlessCaller, ready *atomic.Bool) {
	defer conn.Close()
	if !headlessSameUser(conn) {
		return
	}
	_ = conn.SetDeadline(time.Now().Add(headlessRequestTimeout))
	ctx, cancel := context.WithTimeout(parent, headlessRequestTimeout)
	defer cancel()
	closed := make(chan struct{})
	defer close(closed)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-closed:
		}
	}()
	reader := bufio.NewReaderSize(conn, headlessRequestLimit+1)
	line, err := reader.ReadSlice('\n')
	var request headlessRequest
	if err == nil {
		err = decodeHeadless(line, &request)
	}
	if err != nil {
		_ = json.NewEncoder(conn).Encode(headlessFailure("invalid-request", "Expected one bounded JSON request"))
		return
	}
	// The client keeps its write side open until the response. A disconnect or
	// second frame cancels this waiter, without pretending an accepted pairing
	// mutation was rolled back. No raw frame is logged or persisted.
	go func() { _, _ = reader.ReadByte(); cancel() }()
	result := callHeadless(ctx, client, ready.Load(), request)
	_ = json.NewEncoder(conn).Encode(result)
}

func runHeadless(ctx context.Context, brokerPath string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := headlessPortConflicts(); err != nil {
		return err
	}
	listener, cleanup, err := listenHeadless()
	if err != nil {
		return err
	}
	defer cleanup()
	sup, err := Spawn(ctx, brokerPath)
	if err != nil {
		return err
	}
	defer func() { cancel(); sup.Shutdown() }()
	// Drain both streams continuously. Headless logs contain lifecycle summaries
	// only; raw broker stderr/notification frames may contain pairing material.
	go func() { _, _ = io.Copy(io.Discard, sup.Stderr) }()
	var ready atomic.Bool
	brokerClosed := make(chan struct{})
	go func() {
		defer close(brokerClosed)
		for notification := range sup.Client.Notifications() {
			if notification.Method == "app:ready" {
				ready.Store(true)
			}
		}
	}()
	go func() {
		timer := time.NewTimer(headlessRequestTimeout)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-brokerClosed:
		case <-timer.C:
			if !ready.Load() {
				_ = listener.Close()
			}
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
		case <-brokerClosed:
		}
		_ = listener.Close()
	}()
	capacity := make(chan struct{}, 8)
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			select {
			case <-brokerClosed:
				return errors.New("PAIR broker exited")
			default:
				return errors.New("private control listener stopped")
			}
		}
		select {
		case capacity <- struct{}{}:
			go func() { defer func() { <-capacity }(); serveHeadlessConnection(ctx, connection, sup.Client, &ready) }()
		default:
			_ = connection.Close()
		}
	}
}

func runHeadlessControl(ctx context.Context, input io.Reader, output io.Writer) error {
	type inputResult struct {
		raw []byte
		err error
	}
	inputDone := make(chan inputResult, 1)
	go func() {
		raw, err := io.ReadAll(io.LimitReader(input, headlessRequestLimit+1))
		inputDone <- inputResult{raw, err}
	}()
	var raw []byte
	var err error
	select {
	case result := <-inputDone:
		raw, err = result.raw, result.err
	case <-ctx.Done():
		return errors.New("private control input deadline expired")
	}
	if err != nil {
		return errors.New("cannot read private control request")
	}
	var request headlessRequest
	if err := decodeHeadless(raw, &request); err != nil {
		return err
	}
	if err := validateHeadlessRequest(request); err != nil {
		return err
	}
	conn, err := dialHeadless(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	connectionDone := make(chan struct{})
	defer close(connectionDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-connectionDone:
		}
	}()
	_ = conn.SetDeadline(time.Now().Add(headlessRequestTimeout))
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return errors.New("cannot send private control request")
	}
	reader := bufio.NewReaderSize(conn, headlessFrameLimit+1)
	line, err := reader.ReadSlice('\n')
	if err != nil {
		return errors.New("private control returned no bounded result; completion is unknown")
	}
	var response headlessResponse
	if json.Unmarshal(line, &response) != nil {
		return errors.New("private control returned an invalid result")
	}
	written := make(chan error, 1)
	go func() { _, err := output.Write(line); written <- err }()
	select {
	case err := <-written:
		if err != nil {
			return errors.New("cannot deliver private control result")
		}
	case <-ctx.Done():
		return errors.New("private control output deadline expired")
	}
	return nil
}

func privateControlPipes() error {
	for _, file := range []*os.File{os.Stdin, os.Stdout} {
		st, err := file.Stat()
		if err != nil || st.Mode()&os.ModeCharDevice != 0 {
			return fmt.Errorf("private control requires non-terminal stdin and stdout; never pass pairing material in arguments")
		}
	}
	return nil
}
