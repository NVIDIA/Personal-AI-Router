// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
)

// Peer drives the EAP peer side of one EAP-NOOB association.
type Peer struct {
	peerInfo           json.RawMessage
	state              State
	completionVerified bool

	methodState
}

// NewPeer creates a PAIR joiner. peerInfo carries the node's identity and
// certificate; Completion authenticates it to the inviter.
func NewPeer(peerInfo PairingInfo) *Peer {
	p := &Peer{peerInfo: marshalInfo(peerInfo), state: StateUnregistered}
	p.nai = defaultNAI
	return p
}

// State returns the peer's current association state.
func (p *Peer) State() State { return p.state }

// ServerInfo returns the raw ServerInfo object the server supplied during the
// Initial Exchange. It is folded into the Completion MACs, so it is
// authenticated once the association reaches Registered.
func (p *Peer) ServerInfo() json.RawMessage { return p.in.ServerInfo }

// Receive processes one inbound message from the server and returns the next
// action.
func (p *Peer) Receive(in []byte) (Outcome, error) {
	wm, err := decode(in)
	if err != nil {
		return p.fail(ErrInvalidMessageStructure, "malformed JSON")
	}
	if wm.EAP != "" {
		return p.onResult(wm)
	}
	p.completionVerified = false
	if wm.ErrorCode != nil {
		return Outcome{State: p.state, Done: true, Err: &ProtocolError{Code: *wm.ErrorCode, Info: wm.ErrorInfo}}, nil
	}
	if wm.Type == nil {
		return p.fail(ErrInvalidMessageStructure, "missing Type")
	}
	switch *wm.Type {
	case typeDiscovery:
		return p.onDiscoveryRequest(wm)
	case typeNegotiation:
		return p.onNegotiationRequest(wm)
	case typeKeyExchange:
		return p.onKeyExchangeRequest(wm)
	case typeWaiting:
		return p.onWaitingRequest(wm)
	case typeNoobID:
		return p.onNoobIDRequest(wm)
	case typeCompletion:
		return p.onCompletionRequest(wm)
	default:
		return p.fail(ErrUnexpectedMessageType, fmt.Sprintf("unexpected type %d", *wm.Type))
	}
}

func (p *Peer) onResult(wm *wireMessage) (Outcome, error) {
	if wm.EAP == eapSuccess {
		if !p.completionVerified {
			return p.fail(ErrStateMismatch, "success before verified Completion")
		}
		p.completionVerified = false
		p.state = StateRegistered
		return Outcome{State: p.state, Done: true, Success: true}, nil
	}
	// EAP-Failure: the conversation ended without registration.
	p.completionVerified = false
	return Outcome{State: p.state, Done: true}, nil
}

func (p *Peer) onDiscoveryRequest(wm *wireMessage) (Outcome, error) {
	m := &wireMessage{
		Type:      intp(typeDiscovery),
		PeerState: intp(int(p.state)),
		NAI:       rawString(p.nai),
	}
	// Omit PeerId in the Unregistered state (RFC 9140, Section 3.2.1).
	if p.state != StateUnregistered && p.peerId != "" {
		m.PeerId = rawString(p.peerId)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Send: b, State: p.state}, nil
}

func (p *Peer) onNegotiationRequest(wm *wireMessage) (Outcome, error) {
	peerId, err := rawToString(wm.PeerId)
	if err != nil {
		return p.fail(ErrInvalidData, "bad PeerId")
	}
	p.peerId = peerId

	serverVers, err := rawToIntSlice(wm.Vers)
	if err != nil {
		return p.fail(ErrInvalidData, "bad Vers")
	}
	if !containsInt(serverVers, protocolVersion) {
		return p.fail(ErrUnsupportedVersion, "no common protocol version")
	}
	serverCS, err := rawToIntSlice(wm.Cryptosuites)
	if err != nil {
		return p.fail(ErrInvalidData, "bad Cryptosuites")
	}
	csp, ok := firstSupported(serverCS)
	if !ok {
		return p.fail(ErrUnsupportedCryptosuite, "no common cryptosuite")
	}
	serverDirs, err := rawToInt(wm.Dirs)
	if err != nil {
		return p.fail(ErrInvalidData, "bad Dirs")
	}
	if serverDirs != oobDirection && serverDirs != 3 {
		return p.fail(ErrNoMutuallySupportedOOB, "no mutually supported OOB direction")
	}

	suite, err := suiteByID(csp)
	if err != nil {
		return p.fail(ErrUnsupportedCryptosuite, err.Error())
	}
	p.suite = suite

	// Determine the effective NAI: a server-assigned NewNAI overrides the
	// default NAI (RFC 9140, Section 3.3.1).
	if len(wm.NewNAI) > 0 {
		if nai, err := rawToString(wm.NewNAI); err == nil {
			p.nai = nai
		}
	}

	verpRaw := rawInt(protocolVersion)
	cspRaw := rawInt(int(csp))
	dirpRaw := rawInt(oobDirection)

	p.in.Vers = wm.Vers
	p.in.PeerId = wm.PeerId
	p.in.Cryptosuites = wm.Cryptosuites
	p.in.Dirs = wm.Dirs
	p.in.ServerInfo = ensureRaw(wm.ServerInfo)
	p.in.Verp = verpRaw
	p.in.Cryptosuitep = cspRaw
	p.in.Dirp = dirpRaw
	p.in.PeerInfo = p.peerInfo
	p.in.NAI = rawString(p.nai)

	m := &wireMessage{
		Type:         intp(typeNegotiation),
		Verp:         verpRaw,
		PeerId:       rawString(peerId),
		Cryptosuitep: cspRaw,
		Dirp:         dirpRaw,
		PeerInfo:     p.peerInfo,
	}
	b, err := json.Marshal(m)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Send: b, State: p.state}, nil
}

func (p *Peer) onKeyExchangeRequest(wm *wireMessage) (Outcome, error) {
	if p.state != StateUnregistered || p.suite == nil {
		return p.fail(ErrStateMismatch, "key exchange requires negotiation during the initial exchange")
	}
	jwk, err := rawToJWK(wm.PKs)
	if err != nil {
		return p.fail(ErrInvalidData, "bad PKs")
	}
	nsB64, err := rawToString(wm.Ns)
	if err != nil {
		return p.fail(ErrInvalidData, "bad Ns")
	}
	ns, err := b64Decode(nsB64)
	if err != nil {
		return p.fail(ErrInvalidData, "bad Ns encoding")
	}

	priv, pubJWK, err := p.suite.generateKeypair()
	if err != nil {
		return Outcome{}, err
	}
	z, err := p.suite.computeZ(priv, jwk)
	if err != nil {
		return p.fail(ErrInvalidData, "ECDHE failure")
	}
	np := make([]byte, 32)
	if _, err := rand.Read(np); err != nil {
		return Outcome{}, err
	}
	pkp, err := jwkRaw(pubJWK)
	if err != nil {
		return Outcome{}, err
	}
	npRaw := rawString(b64Encode(np))

	p.priv = priv
	p.z = z
	p.ns = ns
	p.np = np
	p.in.PKs = wm.PKs
	p.in.Ns = wm.Ns
	p.in.PKp = pkp
	p.in.Np = npRaw

	m := &wireMessage{
		Type:   intp(typeKeyExchange),
		PeerId: rawString(p.peerId),
		PKp:    pkp,
		Np:     npRaw,
	}
	b, err := json.Marshal(m)
	if err != nil {
		return Outcome{}, err
	}
	// Initial Exchange complete; the peer moves to Waiting for OOB.
	p.state = StateWaiting
	return Outcome{Send: b, State: p.state}, nil
}

func (p *Peer) onWaitingRequest(wm *wireMessage) (Outcome, error) {
	m := &wireMessage{Type: intp(typeWaiting), PeerId: rawString(p.peerId)}
	b, err := json.Marshal(m)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Send: b, State: p.state}, nil
}

func (p *Peer) onNoobIDRequest(wm *wireMessage) (Outcome, error) {
	if len(p.noobID) == 0 {
		return p.fail(ErrStateMismatch, "no OOB message received")
	}
	m := &wireMessage{
		Type:   intp(typeNoobID),
		PeerId: rawString(p.peerId),
		NoobId: rawString(b64Encode(p.noobID)),
	}
	b, err := json.Marshal(m)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Send: b, State: p.state}, nil
}

func (p *Peer) onCompletionRequest(wm *wireMessage) (Outcome, error) {
	if len(p.noob) == 0 {
		return p.fail(ErrStateMismatch, "no OOB message processed")
	}
	noobID, err := decodeNoobID(wm.NoobId)
	if err != nil {
		return p.fail(ErrInvalidData, "bad NoobId")
	}
	if !bytes.Equal(noobID, p.noobID) {
		return p.fail(ErrUnrecognizedOOBMsgID, "unrecognized NoobId")
	}
	macs, err := decodeMAC(wm.MACs)
	if err != nil {
		return p.fail(ErrInvalidData, "bad MACs")
	}

	p.deriveCompletionKeys()
	expected := p.suite.computeMAC(p.keyM.Kms, 2, p.in)
	if !hmacEqual(macs, expected) {
		return p.fail(ErrHMACVerificationFailed, "MACs verification failed")
	}

	macp := p.suite.computeMAC(p.keyM.Kmp, 1, p.in)
	m := &wireMessage{
		Type:   intp(typeCompletion),
		PeerId: rawString(p.peerId),
		MACp:   rawString(b64Encode(macp)),
	}
	b, err := json.Marshal(m)
	if err != nil {
		return Outcome{}, err
	}
	p.completionVerified = true
	return Outcome{Send: b, State: p.state}, nil
}

// OOBInput accepts the out-of-band message for the server-to-peer direction and
// verifies its fingerprint (RFC 9140, Section 3.2.3). On success the peer moves
// to the OOB Received state.
func (p *Peer) OOBInput(msg OOBMessage) error {
	if p.state != StateWaiting {
		return fmt.Errorf("eapnoob: OOBInput requires Waiting state, have %s", p.state)
	}
	if err := oobInput(&p.methodState, msg); err != nil {
		return err
	}
	p.state = StateOOBReceived
	return nil
}

// OOBInputNoob accepts a caller-supplied 16-byte Noob for the server-to-peer
// direction (e.g. derived from a user-carried PIN) without an OOBMessage
// envelope or Hoob computation. On success the peer moves to OOB Received.
// Transcript agreement is proven later by the Completion Exchange MACs.
func (p *Peer) OOBInputNoob(noob []byte) error {
	if p.state != StateWaiting {
		return fmt.Errorf("eapnoob: OOBInputNoob requires Waiting state, have %s", p.state)
	}
	if err := setOOBNoob(&p.methodState, noob); err != nil {
		return err
	}
	p.state = StateOOBReceived
	return nil
}

func (p *Peer) fail(code int, info string) (Outcome, error) {
	p.completionVerified = false
	b, err := errorMessage(code, info)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Send: b, State: p.state, Done: true, Err: &ProtocolError{Code: code, Info: info}}, nil
}

func firstSupported(serverCS []int) (cryptosuiteID, bool) {
	for _, offeredSuite := range serverCS {
		id := cryptosuiteID(offeredSuite)
		if isSupportedCryptosuite(id) {
			return id, true
		}
	}
	return 0, false
}
