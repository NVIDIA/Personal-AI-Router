// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"nvpair-shared/jsonrpc"
	"nvpair-tui/pairing"
	"nvpair-tui/rpc"
)

// callTimeout bounds one relayed request. cluster:invite-node and
// cluster:respond-to-invite drive multi-round inter-node exchanges, and the
// broker allows itself 30 s for them, so this sits above that — the same
// budget the interactive UI gives its own calls.
const callTimeout = 35 * time.Second

// JSON-RPC error codes this endpoint originates. The relayed cluster-manager
// codes come back untouched, so these deliberately sit outside its range
// (-32001 unknown invite, -32002 invalid state, -32004 precondition).
const (
	// CodeNoPendingInvite — a response was asked for with no invite id and
	// nothing is waiting.
	CodeNoPendingInvite = -32010
	// CodeAmbiguousInvite — a response was asked for with no invite id and
	// several invites are waiting. data.invites names them.
	CodeAmbiguousInvite = -32011
	// CodeInvalidParams / CodeMethodNotFound / CodeInternal are the standard
	// JSON-RPC 2.0 codes.
	CodeInvalidParams  = -32602
	CodeMethodNotFound = -32601
	CodeInternal       = -32603
	CodeParseError     = -32700
)

// Server answers control-socket requests by relaying them through the TUI's
// one broker connection.
type Server struct {
	pairing *pairing.Service
	version string
	ready   func() bool
}

// NewServer builds the control endpoint's request handler. version is the
// nvpair-tui build version and ready reports whether the broker has announced
// itself; both are what `ping` answers with.
func NewServer(svc *pairing.Service, version string, ready func() bool) *Server {
	if ready == nil {
		ready = func() bool { return false }
	}
	return &Server{pairing: svc, version: version, ready: ready}
}

// Serve accepts control connections until ctx is cancelled or the listener is
// closed. Clients are served concurrently and each may issue any number of
// sequential requests before disconnecting.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	var conns sync.WaitGroup
	defer conns.Wait()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// A listener that has been closed is a normal stop; anything else
			// is fatal for this endpoint, because Accept will not recover.
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept on the control socket: %w", err)
		}
		conns.Add(1)
		go func() {
			defer conns.Done()
			defer func() { _ = conn.Close() }()
			// A client that is connected but idle must not hold the endpoint
			// open past a shutdown: closing its connection unblocks the read
			// this goroutine is parked on, so quitting the TUI is prompt even
			// with a subcommand still attached.
			finished := make(chan struct{})
			defer close(finished)
			go func() {
				select {
				case <-ctx.Done():
					_ = conn.Close()
				case <-finished:
				}
			}()
			s.serveConn(ctx, conn)
		}()
	}
}

// serveConn reads requests from one client until it disconnects.
func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	codec := jsonrpc.NewCodec(conn)
	for {
		msg, err := codec.Read()
		if err != nil {
			var decodeErr *jsonrpc.DecodeError
			if errors.As(err, &decodeErr) {
				// A malformed line is the client's problem, not the
				// endpoint's: say so and keep the connection.
				_ = codec.RespondError(nil, CodeParseError, decodeErr.Error())
				continue
			}
			if !errors.Is(err, io.EOF) {
				slog.Debug("control connection ended", "err", err)
			}
			return
		}
		if !msg.IsRequest() {
			// The control endpoint is request/response only; a notification
			// has nobody to answer and is dropped.
			continue
		}
		result, rpcErr := s.dispatch(ctx, msg.Method, msg.Params)
		if rpcErr != nil {
			if err := codec.RespondErrorData(msg.ID, rpcErr.Code, rpcErr.Message, rpcErr.data); err != nil {
				return
			}
			continue
		}
		if err := codec.Respond(msg.ID, result); err != nil {
			return
		}
	}
}

// methodError is a JSON-RPC error a method chose to return.
type methodError struct {
	Code    int
	Message string
	data    any
}

func (e *methodError) Error() string { return e.Message }

func errorf(code int, format string, args ...any) *methodError {
	return &methodError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// dispatch routes one request. The result is returned as json.RawMessage
// wherever the answer is the cluster manager's own Invite, so a caller sees
// exactly what the manager reported rather than a re-encoding of it.
func (s *Server) dispatch(ctx context.Context, method string, params json.RawMessage) (any, *methodError) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	switch method {
	case "ping":
		return map[string]any{"version": s.version, "brokerReady": s.ready()}, nil

	case "pair:invite":
		var p struct {
			Address string `json:"address"`
			Port    int    `json:"port"`
			NodeID  string `json:"nodeId"`
		}
		if err := decodeParams(params, &p); err != nil {
			return nil, errorf(CodeInvalidParams, "%s", err)
		}
		res, err := s.pairing.Invite(ctx, pairing.InviteRequest{Address: p.Address, Port: p.Port, NodeID: p.NodeID})
		if err != nil {
			return nil, relayed(err)
		}
		return res.Raw, nil

	case "pair:invite-status":
		var p struct {
			InviteID string `json:"inviteId"`
		}
		if err := decodeParams(params, &p); err != nil {
			return nil, errorf(CodeInvalidParams, "%s", err)
		}
		if p.InviteID == "" {
			return nil, errorf(CodeInvalidParams, "inviteId is required")
		}
		res, err := s.pairing.InviteStatus(ctx, p.InviteID)
		if err != nil {
			return nil, relayed(err)
		}
		return res.Raw, nil

	case "pair:pending":
		invites := s.pairing.PendingRaw()
		if invites == nil {
			invites = []json.RawMessage{}
		}
		return map[string]any{"invites": invites}, nil

	case "pair:respond":
		var p struct {
			InviteID string `json:"inviteId"`
			Accept   *bool  `json:"accept"`
			Pin      string `json:"pin"`
		}
		if err := decodeParams(params, &p); err != nil {
			return nil, errorf(CodeInvalidParams, "%s", err)
		}
		if p.Accept == nil {
			return nil, errorf(CodeInvalidParams, "accept is required")
		}
		res, err := s.pairing.Respond(ctx, p.InviteID, *p.Accept, p.Pin)
		if err != nil {
			return nil, relayed(err)
		}
		return res.Raw, nil

	case "pair:members":
		members, err := s.pairing.Members(ctx)
		if err != nil {
			return nil, relayed(err)
		}
		return members, nil

	default:
		return nil, errorf(CodeMethodNotFound, "unknown method %q", method)
	}
}

// relayed turns a service error into the JSON-RPC error the client sees. A
// cluster-manager error keeps its own code and message, so a caller can key on
// the manager's contract through this endpoint exactly as through the broker.
func relayed(err error) *methodError {
	var rpcErr *rpc.RPCError
	if errors.As(err, &rpcErr) {
		return &methodError{Code: rpcErr.Code, Message: rpcErr.Message}
	}
	if errors.Is(err, pairing.ErrNoPendingInvite) {
		return &methodError{Code: CodeNoPendingInvite, Message: err.Error()}
	}
	var ambiguous *pairing.AmbiguousInviteError
	if errors.As(err, &ambiguous) {
		listed := make([]map[string]string, 0, len(ambiguous.Invites))
		for _, inv := range ambiguous.Invites {
			listed = append(listed, map[string]string{
				"inviteId":     inv.InviteID,
				"fromNodeName": inv.FromNodeName,
			})
		}
		return &methodError{
			Code:    CodeAmbiguousInvite,
			Message: err.Error(),
			data:    map[string]any{"invites": listed},
		}
	}
	return &methodError{Code: CodeInternal, Message: err.Error()}
}

func decodeParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("invalid params: %w", err)
	}
	return nil
}
