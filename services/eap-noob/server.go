// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
)

// ServerConfig configures the EAP server role.
type ServerConfig struct {
	// Versions are the supported protocol versions, highest priority first.
	// Defaults to []int{1}.
	Versions []int
	// Cryptosuites are the supported cryptosuites in decreasing priority.
	// Defaults to the recommended order []int{2, 1}.
	Cryptosuites []int
	// Dirs is the supported OOB channel direction (1=peer-to-server,
	// 2=server-to-peer, 3=both). Defaults to 3.
	Dirs int
	// ServerInfo is an optional application-defined JSON object sent to the
	// peer in the Initial Exchange.
	ServerInfo map[string]any
	// NewNAI optionally assigns a new NAI to the peer.
	NewNAI string
	// SleepTime is an optional minimum probe interval advertised to the peer.
	SleepTime int
}

// Server drives the EAP server side of one EAP-NOOB association.
type Server struct {
	cfg   ServerConfig
	store Store
	state State

	methodState

	peerState int
	verp      int
	csp       int
	dirp      int
}

// NewServer creates a server using the given configuration and association
// store. If store is nil an in-memory store is used.
func NewServer(cfg ServerConfig, store Store) *Server {
	if len(cfg.Versions) == 0 {
		cfg.Versions = []int{1}
	}
	if len(cfg.Cryptosuites) == 0 {
		cfg.Cryptosuites = supportedCryptosuites()
	}
	if cfg.Dirs == 0 {
		cfg.Dirs = 3
	}
	if store == nil {
		store = NewMemoryStore()
	}
	return &Server{cfg: cfg, store: store, state: StateUnregistered}
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

// Export derives an arbitrary-length shared secret from the registered
// association (see Association.Export).
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
	s.peerState = *wm.PeerState
	if len(wm.NAI) > 0 {
		if nai, err := rawToString(wm.NAI); err == nil {
			s.nai = nai
		}
	}
	if s.nai == "" {
		s.nai = defaultNAI
	}

	ss, ps := int(s.state), s.peerState
	switch {
	case (ss == 2 && (ps == 1 || ps == 2)) || (ps == 2 && (ss == 1 || ss == 2)):
		return s.startCompletion(wm)
	case ss == 1 && ps == 1:
		return s.buildWaitingRequest()
	case ss == 0 || ps == 0:
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

	vers := rawIntSlice(s.cfg.Versions)
	cs := rawIntSlice(s.cfg.Cryptosuites)
	dirs := rawInt(s.cfg.Dirs)
	srvInfo := marshalInfo(s.cfg.ServerInfo)

	m := &wireMessage{
		Type:         intp(typeNegotiation),
		Vers:         vers,
		PeerId:       rawString(peerId),
		Cryptosuites: cs,
		Dirs:         dirs,
		ServerInfo:   srvInfo,
	}
	if s.cfg.NewNAI != "" {
		m.NewNAI = rawString(s.cfg.NewNAI)
		s.nai = s.cfg.NewNAI
	}

	s.in.Vers = vers
	s.in.PeerId = rawString(peerId)
	s.in.Cryptosuites = cs
	s.in.Dirs = dirs
	s.in.ServerInfo = srvInfo

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
	if !containsInt(s.cfg.Versions, verp) {
		return s.fail(ErrUnsupportedVersion, "peer selected unsupported version")
	}
	csp, err := rawToInt(wm.Cryptosuitep)
	if err != nil {
		return s.fail(ErrInvalidData, "bad Cryptosuitep")
	}
	if !containsInt(s.cfg.Cryptosuites, csp) || !isSupportedCryptosuite(csp) {
		return s.fail(ErrUnsupportedCryptosuite, "peer selected unsupported cryptosuite")
	}
	dirp, err := rawToInt(wm.Dirp)
	if err != nil {
		return s.fail(ErrInvalidData, "bad Dirp")
	}
	if dirp&s.cfg.Dirs == 0 || dirp < 1 || dirp > 3 {
		return s.fail(ErrNoMutuallySupportedOOB, "no mutually supported OOB direction")
	}

	suite, err := suiteByID(csp)
	if err != nil {
		return s.fail(ErrUnsupportedCryptosuite, err.Error())
	}
	s.suite = suite
	s.verp, s.csp, s.dirp = verp, csp, dirp

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
	if s.cfg.SleepTime > 0 {
		m.SleepTime = intp(s.cfg.SleepTime)
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
	if s.cfg.SleepTime > 0 {
		m.SleepTime = intp(s.cfg.SleepTime)
	}
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
	if s.peerState == 2 {
		// Server-to-peer direction: discover the NoobId the peer accepted.
		m := &wireMessage{Type: intp(typeNoobID), PeerId: rawString(s.peerId)}
		b, err := json.Marshal(m)
		if err != nil {
			return Outcome{}, err
		}
		return Outcome{Send: b, State: s.state}, nil
	}
	// Peer-to-server direction (peer still Waiting): send MACs directly.
	return s.buildCompletionRequest()
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
// Exchange (Waiting state) and have negotiated a direction that includes
// server-to-peer.
func (s *Server) OOBOutput() (OOBMessage, error) {
	if s.state != StateWaiting {
		return OOBMessage{}, fmt.Errorf("eapnoob: OOBOutput requires Waiting state, have %s", s.state)
	}
	if s.dirp != 2 && s.dirp != 3 {
		return OOBMessage{}, fmt.Errorf("eapnoob: server-to-peer OOB not negotiated")
	}
	return oobOutput(&s.methodState, 2)
}

// OOBOutputWith produces the server-to-peer OOB material using a caller-supplied
// 16-byte Noob (e.g. derived from a user-carried PIN) instead of a randomly
// generated one. Preconditions match OOBOutput. The returned OOBMessage is
// informational: the Noob is caller-controlled and the peer recomputes Hoob
// locally, so nothing here needs to be transmitted in band.
func (s *Server) OOBOutputWith(noob []byte) (OOBMessage, error) {
	if s.state != StateWaiting {
		return OOBMessage{}, fmt.Errorf("eapnoob: OOBOutputWith requires Waiting state, have %s", s.state)
	}
	if s.dirp != 2 && s.dirp != 3 {
		return OOBMessage{}, fmt.Errorf("eapnoob: server-to-peer OOB not negotiated")
	}
	return oobOutputWith(&s.methodState, 2, noob)
}

// OOBInput accepts the out-of-band message for the peer-to-server direction and
// verifies its fingerprint (RFC 9140, Section 3.2.3). On success the server
// moves to the OOB Received state.
func (s *Server) OOBInput(msg OOBMessage) error {
	if s.state != StateWaiting {
		return fmt.Errorf("eapnoob: OOBInput requires Waiting state, have %s", s.state)
	}
	if s.dirp != 1 && s.dirp != 3 {
		return fmt.Errorf("eapnoob: peer-to-server OOB not negotiated")
	}
	if err := oobInput(&s.methodState, 1, msg); err != nil {
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
