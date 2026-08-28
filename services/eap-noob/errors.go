// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import "fmt"

// EAP-NOOB error codes (RFC 9140, Section 5.3, Table 10).
const (
	ErrInvalidNAI              = 1001
	ErrInvalidMessageStructure = 1002
	ErrInvalidData             = 1003
	ErrUnexpectedMessageType   = 1004
	ErrUnexpectedPeerIdentier  = 2001 // unexpected peer identifier
	ErrUnrecognizedOOBMsgID    = 2003 // invalid ECDHE key / unrecognized NoobId
	ErrUnwantedPeer            = 2004
	ErrStateMismatch           = 2002
	ErrUnsupportedVersion      = 3001
	ErrUnsupportedCryptosuite  = 3002
	ErrNoMutuallySupportedOOB  = 3003 // no mutually supported OOB direction
	ErrHMACVerificationFailed  = 4001
	ErrApplicationSpecific     = 5001
	ErrInvalidServerInfo       = 5002
)

// ProtocolError represents an EAP-NOOB error notification (Type=0) that one
// endpoint sent to the other (RFC 9140, Section 3.6).
type ProtocolError struct {
	Code int
	Info string
}

func (e *ProtocolError) Error() string {
	if e.Info != "" {
		return fmt.Sprintf("eapnoob: protocol error %d: %s", e.Code, e.Info)
	}
	return fmt.Sprintf("eapnoob: protocol error %d", e.Code)
}

// errorMessage builds the wire bytes for a Type=0 error notification.
func errorMessage(code int, info string) ([]byte, error) {
	t := typeError
	m := &wireMessage{Type: &t, ErrorCode: &code, ErrorInfo: info}
	b, _, err := encode(m)
	return b, err
}
