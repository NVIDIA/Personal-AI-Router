// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"encoding/json"
	"fmt"
)

// ServerConfig configures the server's identity and offered OOB directions.
type ServerConfig struct {
	ServerInfo PairingInfo
	// Dirs selects supported OOB directions. The default is OOBBoth.
	Dirs OOBDirection
}

// Server drives the EAP server side of one EAP-NOOB association.
type Server struct {
	serverInfo json.RawMessage
	dirs       OOBDirection
	dirp       OOBDirection
	csp        int
	store      Store
	state      State

	methodState
}

// NewServer creates a server with a typed identity and offered OOB directions.
// Completion authenticates the identity, including its public certificate.
// If store is nil, an in-memory association store is used.
func NewServer(cfg ServerConfig, store Store) *Server {
	if cfg.Dirs == 0 {
		cfg.Dirs = OOBBoth
	}
	if store == nil {
		store = NewMemoryStore()
	}
	return &Server{serverInfo: marshalInfo(cfg.ServerInfo), dirs: cfg.Dirs, store: store, state: StateUnregistered}
}

// State returns the server's current association state.
func (s *Server) State() State { return s.state }

// PeerInfo returns the raw PeerInfo object the peer supplied during the Initial
// Exchange. It is folded into the Completion MACs, so it is authenticated once
// the association reaches Registered.
func (s *Server) PeerInfo() json.RawMessage { return s.in.PeerInfo }

// Association returns the registered association once the Completion Exchange
// has succeeded, or nil.
func (s *Server) Association() *Association { return s.registered }

// Export derives a shared application secret from the registered association
// (see Association.Export).
func (s *Server) Export(label string, context []byte, length int) ([]byte, error) {
	return s.registered.Export(label, context, length)
}

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
	case (ss == StateOOBReceived && (ps == StateWaiting || ps == StateOOBReceived)) ||
		(ps == StateOOBReceived && (ss == StateWaiting || ss == StateOOBReceived)):
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
	dirs := rawInt(int(s.dirs))

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
	suite, err := suiteByID(cryptosuiteID(selectedSuite))
	if err != nil {
		return s.fail(ErrUnsupportedCryptosuite, "peer selected unsupported cryptosuite")
	}
	selectedDirection, err := rawToInt(wm.Dirp)
	if err != nil {
		return s.fail(ErrInvalidData, "bad Dirp")
	}
	dirp := OOBDirection(selectedDirection)
	if dirp&s.dirs == 0 || dirp < OOBPeerToServer || dirp > OOBBoth {
		return s.fail(ErrNoMutuallySupportedOOB, "no mutually supported OOB direction")
	}

	s.suite = suite
	s.csp = selectedSuite
	s.dirp = dirp

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
	if State(*wm.PeerState) != StateOOBReceived {
		// In the peer-to-server direction, the peer is still Waiting.
		return s.buildCompletionRequest()
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
	if !hmac.Equal(macp, expected) {
		return s.fail(ErrHMACVerificationFailed, "MACp verification failed")
	}

	assoc := &Association{
		PeerId:       s.peerId,
		NAI:          s.nai,
		Cryptosuitep: s.csp,
		Kz:           append([]byte(nil), s.keyM.Kz...),
	}
	if err := s.store.Save(assoc); err != nil {
		return Outcome{}, err
	}
	s.registered = assoc
	s.state = StateRegistered
	return Outcome{Send: resultBytes(eapSuccess), State: s.state, Done: true, Success: true}, nil
}

// OOBOutput produces the out-of-band message for the server-to-peer direction
// (RFC 9140, Section 3.2.3). The server must have completed the Initial
// Exchange (Waiting state) and negotiated server-to-peer OOB.
func (s *Server) OOBOutput() (OOBMessage, error) {
	if s.state != StateWaiting {
		return OOBMessage{}, fmt.Errorf("eapnoob: OOBOutput requires Waiting state, have %s", s.state)
	}
	if s.dirp != OOBServerToPeer && s.dirp != OOBBoth {
		return OOBMessage{}, fmt.Errorf("eapnoob: server-to-peer OOB not negotiated")
	}
	return oobOutput(&s.methodState, OOBServerToPeer)
}

// OOBOutputWith records a caller-supplied 16-byte Noob for server-to-peer OOB.
// The server stays Waiting, without building an OOBMessage or computing Hoob.
// Preconditions match OOBOutput; Completion authenticates the transcript.
func (s *Server) OOBOutputWith(noob []byte) error {
	if s.state != StateWaiting {
		return fmt.Errorf("eapnoob: OOBOutputWith requires Waiting state, have %s", s.state)
	}
	if s.dirp != OOBServerToPeer && s.dirp != OOBBoth {
		return fmt.Errorf("eapnoob: server-to-peer OOB not negotiated")
	}
	return setOOBNoob(&s.methodState, noob)
}

// OOBInput accepts the out-of-band message for the peer-to-server direction and
// verifies its fingerprint (RFC 9140, Section 3.2.3). On success the server
// moves to the OOB Received state.
func (s *Server) OOBInput(msg OOBMessage) error {
	if s.state != StateWaiting {
		return fmt.Errorf("eapnoob: OOBInput requires Waiting state, have %s", s.state)
	}
	if s.dirp != OOBPeerToServer && s.dirp != OOBBoth {
		return fmt.Errorf("eapnoob: peer-to-server OOB not negotiated")
	}
	if err := oobInput(&s.methodState, OOBPeerToServer, msg); err != nil {
		return err
	}
	s.state = StateOOBReceived
	return nil
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
