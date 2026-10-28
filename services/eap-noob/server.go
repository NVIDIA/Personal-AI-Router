// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
)

// Server drives the EAP server side of one EAP-NOOB association.
type Server struct {
	serverInfo json.RawMessage
	state      State

	methodState
}

// NewServer creates a PAIR inviter. serverInfo carries the node's identity and
// certificate; Completion authenticates it to the joiner.
func NewServer(serverInfo PairingInfo) *Server {
	return &Server{serverInfo: marshalInfo(serverInfo), state: StateUnregistered}
}

// State returns the server's current association state.
func (s *Server) State() State { return s.state }

// PeerInfo returns the raw PeerInfo object the peer supplied during the Initial
// Exchange. It is folded into the Completion MACs, so it is authenticated once
// the association reaches Registered.
func (s *Server) PeerInfo() json.RawMessage { return s.in.PeerInfo }

// Start begins a new EAP conversation by producing the first EAP-NOOB request
// (Type=1, PeerId/PeerState discovery).
func (s *Server) Start() ([]byte, error) {
	m := &wireMessage{Type: intp(typeDiscovery)}
	b, err := json.Marshal(m)
	return b, err
}

// Receive processes one inbound message from the peer and returns the next
// action.
func (s *Server) Receive(in []byte) (Outcome, error) {
	wm, err := decode(in)
	if err != nil {
		return s.fail(ErrInvalidMessageStructure, "malformed JSON")
	}
	if wm.ErrorCode != nil {
		return Outcome{State: s.state, Done: true, Err: &ProtocolError{Code: *wm.ErrorCode, Info: wm.ErrorInfo}}, nil
	}
	if wm.Type == nil {
		return s.fail(ErrInvalidMessageStructure, "missing Type")
	}
	switch *wm.Type {
	case typeDiscovery:
		return s.onDiscovery(wm)
	case typeNegotiation:
		return s.onNegotiation(wm)
	case typeKeyExchange:
		return s.onKeyExchange(wm)
	case typeWaiting:
		return s.onWaitingResponse(wm)
	case typeNoobID:
		return s.onNoobID(wm)
	case typeCompletion:
		return s.onCompletion(wm)
	default:
		return s.fail(ErrUnexpectedMessageType, fmt.Sprintf("unexpected type %d", *wm.Type))
	}
}

func (s *Server) onDiscovery(wm *wireMessage) (Outcome, error) {
	if wm.PeerState == nil {
		return s.fail(ErrInvalidData, "missing PeerState")
	}
	if len(wm.NAI) > 0 {
		if nai, err := rawToString(wm.NAI); err == nil {
			s.nai = nai
		}
	}
	if s.nai == "" {
		s.nai = defaultNAI
	}

	ss, ps := s.state, State(*wm.PeerState)
	switch {
	case ss == StateWaiting && ps == StateOOBReceived:
		return s.startCompletion(wm)
	case ss == StateWaiting && ps == StateWaiting:
		return s.buildWaitingRequest()
	case ss == StateUnregistered || ps == StateUnregistered:
		return s.buildNegotiationRequest()
	default:
		return s.fail(ErrStateMismatch, fmt.Sprintf("server state %d / peer state %d", ss, ps))
	}
}

func (s *Server) buildNegotiationRequest() (Outcome, error) {
	peerId, err := newPeerId()
	if err != nil {
		return Outcome{}, err
	}
	s.peerId = peerId

	vers := rawIntSlice([]int{protocolVersion})
	cs := rawIntSlice([]int{int(cryptosuiteP256), int(cryptosuiteX25519)})
	dirs := rawInt(oobDirection)

	m := &wireMessage{
		Type:         intp(typeNegotiation),
		Vers:         vers,
		PeerId:       rawString(peerId),
		Cryptosuites: cs,
		Dirs:         dirs,
		ServerInfo:   s.serverInfo,
	}

	s.in.Vers = vers
	s.in.PeerId = rawString(peerId)
	s.in.Cryptosuites = cs
	s.in.Dirs = dirs
	s.in.ServerInfo = s.serverInfo

	b, err := json.Marshal(m)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Send: b, State: s.state}, nil
}

func (s *Server) onNegotiation(wm *wireMessage) (Outcome, error) {
	if err := s.checkPeerId(wm); err != nil {
		return s.fail(ErrUnexpectedPeerIdentier, err.Error())
	}
	verp, err := rawToInt(wm.Verp)
	if err != nil {
		return s.fail(ErrInvalidData, "bad Verp")
	}
	if verp != protocolVersion {
		return s.fail(ErrUnsupportedVersion, "peer selected unsupported version")
	}
	selectedSuite, err := rawToInt(wm.Cryptosuitep)
	if err != nil {
		return s.fail(ErrInvalidData, "bad Cryptosuitep")
	}
	csp := cryptosuiteID(selectedSuite)
	if !isSupportedCryptosuite(csp) {
		return s.fail(ErrUnsupportedCryptosuite, "peer selected unsupported cryptosuite")
	}
	dirp, err := rawToInt(wm.Dirp)
	if err != nil {
		return s.fail(ErrInvalidData, "bad Dirp")
	}
	if dirp&oobDirection == 0 || dirp < 1 || dirp > 3 {
		return s.fail(ErrNoMutuallySupportedOOB, "no mutually supported OOB direction")
	}

	suite, err := suiteByID(csp)
	if err != nil {
		return s.fail(ErrUnsupportedCryptosuite, err.Error())
	}
	s.suite = suite

	s.in.Verp = wm.Verp
	s.in.Cryptosuitep = wm.Cryptosuitep
	s.in.Dirp = wm.Dirp
	s.in.PeerInfo = ensureRaw(wm.PeerInfo)
	s.in.NAI = rawString(s.nai)

	return s.buildKeyExchangeRequest()
}

func (s *Server) buildKeyExchangeRequest() (Outcome, error) {
	priv, jwk, err := s.suite.generateKeypair()
	if err != nil {
		return Outcome{}, err
	}
	s.priv = priv
	pks, err := jwkRaw(jwk)
	if err != nil {
		return Outcome{}, err
	}
	ns := make([]byte, 32)
	if _, err := rand.Read(ns); err != nil {
		return Outcome{}, err
	}
	s.ns = ns
	nsRaw := rawString(b64Encode(ns))

	m := &wireMessage{
		Type:   intp(typeKeyExchange),
		PeerId: rawString(s.peerId),
		PKs:    pks,
		Ns:     nsRaw,
	}

	s.in.PKs = pks
	s.in.Ns = nsRaw

	b, err := json.Marshal(m)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Send: b, State: s.state}, nil
}

func (s *Server) onKeyExchange(wm *wireMessage) (Outcome, error) {
	if err := s.checkPeerId(wm); err != nil {
		return s.fail(ErrUnexpectedPeerIdentier, err.Error())
	}
	jwk, err := rawToJWK(wm.PKp)
	if err != nil {
		return s.fail(ErrInvalidData, "bad PKp")
	}
	z, err := s.suite.computeZ(s.priv, jwk)
	if err != nil {
		return s.fail(ErrInvalidData, "ECDHE failure")
	}
	npB64, err := rawToString(wm.Np)
	if err != nil {
		return s.fail(ErrInvalidData, "bad Np")
	}
	np, err := b64Decode(npB64)
	if err != nil {
		return s.fail(ErrInvalidData, "bad Np encoding")
	}
	s.z = z
	s.np = np
	s.in.PKp = wm.PKp
	s.in.Np = wm.Np

	// Initial Exchange always ends in EAP-Failure; both sides move to Waiting.
	s.state = StateWaiting
	return Outcome{Send: resultBytes(eapFailure), State: s.state, Done: true}, nil
}

func (s *Server) buildWaitingRequest() (Outcome, error) {
	m := &wireMessage{Type: intp(typeWaiting), PeerId: rawString(s.peerId)}
	b, err := json.Marshal(m)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Send: b, State: s.state}, nil
}

func (s *Server) onWaitingResponse(wm *wireMessage) (Outcome, error) {
	if err := s.checkPeerId(wm); err != nil {
		return s.fail(ErrUnexpectedPeerIdentier, err.Error())
	}
	return Outcome{Send: resultBytes(eapFailure), State: s.state, Done: true}, nil
}

func (s *Server) startCompletion(wm *wireMessage) (Outcome, error) {
	if err := s.checkPeerId(wm); err != nil {
		return s.fail(ErrUnexpectedPeerIdentier, err.Error())
	}
	if len(s.noob) == 0 {
		return s.fail(ErrStateMismatch, "no OOB message processed")
	}
	m := &wireMessage{Type: intp(typeNoobID), PeerId: rawString(s.peerId)}
	b, err := json.Marshal(m)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Send: b, State: s.state}, nil
}

func (s *Server) onNoobID(wm *wireMessage) (Outcome, error) {
	if err := s.checkPeerId(wm); err != nil {
		return s.fail(ErrUnexpectedPeerIdentier, err.Error())
	}
	noobID, err := decodeNoobID(wm.NoobId)
	if err != nil {
		return s.fail(ErrInvalidData, "bad NoobId")
	}
	if !bytes.Equal(noobID, s.noobID) {
		// The OOB message the peer received is unknown to the server.
		return s.fail(ErrUnrecognizedOOBMsgID, "unrecognized NoobId")
	}
	return s.buildCompletionRequest()
}

func (s *Server) buildCompletionRequest() (Outcome, error) {
	s.deriveCompletionKeys()
	macs := s.suite.computeMAC(s.keyM.Kms, 2, s.in)

	m := &wireMessage{
		Type:   intp(typeCompletion),
		PeerId: rawString(s.peerId),
		NoobId: rawString(b64Encode(s.noobID)),
		MACs:   rawString(b64Encode(macs)),
	}
	b, err := json.Marshal(m)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Send: b, State: s.state}, nil
}

func (s *Server) onCompletion(wm *wireMessage) (Outcome, error) {
	if err := s.checkPeerId(wm); err != nil {
		return s.fail(ErrUnexpectedPeerIdentier, err.Error())
	}
	macp, err := decodeMAC(wm.MACp)
	if err != nil {
		return s.fail(ErrInvalidData, "bad MACp")
	}
	expected := s.suite.computeMAC(s.keyM.Kmp, 1, s.in)
	if !hmacEqual(macp, expected) {
		return s.fail(ErrHMACVerificationFailed, "MACp verification failed")
	}

	s.state = StateRegistered
	return Outcome{Send: resultBytes(eapSuccess), State: s.state, Done: true, Success: true}, nil
}

// OOBOutput produces the out-of-band message for the server-to-peer direction
// (RFC 9140, Section 3.2.3). The server must have completed the Initial
// Exchange (Waiting state).
func (s *Server) OOBOutput() (OOBMessage, error) {
	if s.state != StateWaiting {
		return OOBMessage{}, fmt.Errorf("eapnoob: OOBOutput requires Waiting state, have %s", s.state)
	}
	return oobOutput(&s.methodState)
}

// OOBOutputWith records a caller-supplied 16-byte Noob for server-to-peer OOB.
// The server stays Waiting, without building an OOBMessage or computing Hoob.
// Preconditions match OOBOutput; Completion authenticates the transcript.
func (s *Server) OOBOutputWith(noob []byte) error {
	if s.state != StateWaiting {
		return fmt.Errorf("eapnoob: OOBOutputWith requires Waiting state, have %s", s.state)
	}
	return setOOBNoob(&s.methodState, noob)
}

func (s *Server) checkPeerId(wm *wireMessage) error {
	if s.peerId == "" {
		return nil
	}
	got, err := rawToString(wm.PeerId)
	if err != nil {
		return fmt.Errorf("missing PeerId")
	}
	if got != s.peerId {
		return fmt.Errorf("PeerId mismatch")
	}
	return nil
}

func (s *Server) fail(code int, info string) (Outcome, error) {
	b, err := errorMessage(code, info)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Send: b, State: s.state, Done: true, Err: &ProtocolError{Code: code, Info: info}}, nil
}
