// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

/**
 * A short, human-comparable label for a pairing invitation.
 *
 * Shown on BOTH sides — next to the PIN on the inviter and in the approval
 * modal on the joiner — so the person carrying the PIN between two machines can
 * see that both screens are talking about the same invitation.
 *
 * This exists because each invitation carries its OWN PIN, and an inviter that
 * cancels and retries mints a fresh one. Without a visible handle, two
 * invitations are indistinguishable ("From sender.local" either way), and a
 * PIN read from one screen can be typed into a modal bound to the other. That
 * fails as an EAP-NOOB Noob mismatch and is reported as "Incorrect PIN", which
 * sends the user looking for a typo that never happened.
 *
 * NOT a secret and NOT an authenticator: the invite id already crosses the LAN
 * in the clear and appears in logs, while the PIN is the only secret in the
 * exchange. This is a selector that lets a human notice they are looking at the
 * wrong invitation — the cryptographic binding is EAP-NOOB's Hoob/NoobId, which
 * is unaffected by anything shown here.
 *
 * Derived from the invite id rather than generated separately, so it needs no
 * new field on the wire and cannot drift out of sync with the invitation it
 * names. Six characters is ample for the handful of invitations a node holds at
 * once; a word list would read better aloud, but it would need a new field
 * carried by the backend to stay stable across both sides.
 */
export function inviteLabel(inviteId: string | null | undefined): string {
    const compact = (inviteId ?? '').replace(/[^0-9a-zA-Z]/g, '').toUpperCase()
    if (!compact) return '------'
    // Right-hand characters: invite ids share a common prefix, so the tail is
    // where two concurrent invitations actually differ.
    const tail = compact.slice(-6).padStart(6, '0')
    return `${tail.slice(0, 3)}-${tail.slice(3)}`
}
