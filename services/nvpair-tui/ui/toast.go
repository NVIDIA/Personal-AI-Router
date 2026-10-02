// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"time"
)

// Transient status messages expire on their own. Failures stay longer than
// successes: an operator needs time to read why something did not work, while
// "invite sent" has served its purpose within a few seconds.
const (
	toastTTL      = 6 * time.Second
	toastErrorTTL = 20 * time.Second
)

type toastKind int

const (
	toastInfo toastKind = iota
	toastOK
	toastError
)

// toast is a view's transient status line. Expiry is evaluated at render time
// rather than scheduled, so setting a new message implicitly replaces any
// pending one and no timer can fire against a stale message. The shell's
// one-second tick guarantees the frame refreshes within a tick of expiry.
//
// This exists because per-view status strings were only ever overwritten by the
// next action, so an outcome like "invite sent - PIN 123456" stayed on screen
// indefinitely and read as current long after the invite had been answered.
type toast struct {
	text string
	kind toastKind
	at   time.Time
	// sticky suppresses expiry for a message whose content the user is still
	// acting on — the pairing PIN, which they read aloud to someone standing
	// at another machine. A timer must not take that away mid-sentence, so it
	// persists until the operation resolves and the owner clears it.
	sticky bool
}

func (t *toast) info(format string, args ...any) { t.set(toastInfo, format, args...) }

// busy announces an operation that is under way, and does not expire.
//
// An RPC here is allowed 35 seconds and an engine start or a model download
// routinely uses them, while an ordinary note expires in 6 — so "start
// Ollama..." vanished long before the outcome arrived, leaving a screen
// indistinguishable from a keypress the terminal had dropped. This is bounded
// by the call instead of by a timer: the reply's ok or error replaces it.
func (t *toast) busy(format string, args ...any) {
	t.set(toastInfo, format, args...)
	t.sticky = true
}

// arm sets a destructive confirmation prompt: styled as a warning, and not
// expiring.
//
// Both halves matter. An armed action whose prompt has timed out turns the next
// keystroke into a confirmation the operator has no reason to expect; and a
// prompt asking whether to destroy something should not render in the same
// green as "invite sent", which is what pin would have given it.
func (t *toast) arm(format string, args ...any) {
	t.set(toastError, format, args...)
	t.sticky = true
}

func (t *toast) ok(format string, args ...any)    { t.set(toastOK, format, args...) }
func (t *toast) error(format string, args ...any) { t.set(toastError, format, args...) }

func (t *toast) set(kind toastKind, format string, args ...any) {
	t.text = fmt.Sprintf(format, args...)
	t.kind = kind
	t.at = time.Now()
	t.sticky = false
}

// pin sets a message that stays until explicitly cleared. Reserved for content
// the user must transcribe; everything else expires.
func (t *toast) pin(format string, args ...any) {
	t.set(toastOK, format, args...)
	t.sticky = true
}

func (t toast) ttl() time.Duration {
	if t.kind == toastError {
		return toastErrorTTL
	}
	return toastTTL
}

// expired reports whether the message has outlived its kind's TTL. A sticky
// message never expires on its own.
func (t toast) expired() bool {
	if t.text == "" {
		return true
	}
	if t.sticky {
		return false
	}
	return time.Since(t.at) >= t.ttl()
}

// render returns the styled status line, or "" when there is nothing current to
// show. Callers treat "" as "render no status row".
func (t toast) render() string {
	if t.expired() {
		return ""
	}
	switch t.kind {
	case toastError:
		return statusErrStyle.Render(t.text)
	case toastOK:
		return statusOKStyle.Render(t.text)
	default:
		return footerStyle.Render(t.text)
	}
}
