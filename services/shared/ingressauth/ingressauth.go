// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package ingressauth is the opt-in credential gate the inference proxies apply
// to a plaintext request that did not arrive from loopback. Both proxies share
// it for the same reason they share nvpair-shared/cors: the two must accept and
// refuse an outside caller identically, and one implementation is what keeps
// them from drifting.
//
// With nothing configured the gate is disabled and the proxies keep their
// loopback-only behavior — a LAN caller is refused before this package is
// consulted. An operator enables it by configuring at least one API key, either
// in a key file (NVPAIR_PROXY_API_KEYS_FILE, default <appdir>/proxy-api-keys) or
// inline (NVPAIR_PROXY_API_KEYS). Once enabled, a non-loopback plaintext caller
// must present a configured key as "Authorization: Bearer <key>" or, for clients
// built on the Anthropic SDK convention, "X-Api-Key: <key>"; an optional
// NVPAIR_PROXY_ALLOWED_CIDRS narrows which source addresses may even try.
// Loopback callers are never asked for a key — the desktop application, the
// terminal interface, and local tools are unaffected by enabling the gate.
//
// Every failure fails closed. A key file that cannot be read, is readable by
// other users, or contains an entry that could never match over the wire
// contributes no keys, the reason is logged, and the LAN stays closed. Keys are
// held in memory only as SHA-256 digests and are compared in constant time; a
// rejected credential is logged as a short digest fingerprint, never as itself.
package ingressauth

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"nvpair-shared/appdir"
)

const (
	// EnvKeys holds comma-separated API keys, for headless and container
	// deployments where a file is inconvenient. Combined with the key file.
	EnvKeys = "NVPAIR_PROXY_API_KEYS"
	// EnvKeysFile overrides the key file path. Unset, the file is
	// <appdir>/DefaultKeyFileName and is consulted only if it exists.
	EnvKeysFile = "NVPAIR_PROXY_API_KEYS_FILE"
	// EnvAllowedCIDRs optionally lists comma-separated CIDR prefixes an
	// authenticated non-loopback caller must originate from.
	EnvAllowedCIDRs = "NVPAIR_PROXY_ALLOWED_CIDRS"
	// DefaultKeyFileName is the key file's name inside the application data
	// directory (see nvpair-shared/appdir).
	DefaultKeyFileName = "proxy-api-keys"

	// MinKeyLength is the shortest key the gate accepts. A 32-character key
	// drawn at random from the allowed alphabet carries well over 128 bits,
	// which puts online guessing out of reach without a lockout mechanism. A
	// 32-character passphrase does not; the documentation says to generate
	// keys randomly.
	MinKeyLength = 32

	// MaxKeyLength bounds a key, configured or presented. A credential a
	// client sends is hashed before it is compared, so without a ceiling a
	// caller could make the proxy digest a megabyte of header per request;
	// anything longer than this is not a key and is not examined.
	MaxKeyLength = 512

	// maxKeyFileBytes bounds how much of a key file is read. A key file holds
	// a handful of short lines; anything larger is not a key file.
	maxKeyFileBytes = 64 << 10

	// defaultRecheckEvery is how long a FromEnv gate trusts its last look at
	// the key file before opening it again. Without a floor, a node that never
	// opted in would still pay an open() for every unauthenticated LAN request,
	// which is a remotely triggered cost it did not have before. One second
	// keeps rotation and revocation effectively immediate.
	defaultRecheckEvery = time.Second

	// CodeUnauthorized is the ingress error code for a missing or wrong key.
	CodeUnauthorized = "unauthorized"
	// CodeSourceNotAllowed is the ingress error code for a caller outside the
	// configured CIDR allowlist.
	CodeSourceNotAllowed = "source-not-allowed"

	headerAuthorization = "Authorization"
	headerAPIKey        = "X-Api-Key"
	bearerScheme        = "bearer"
	noCredential        = "none"

	// challengeMissing and challengeInvalid are the WWW-Authenticate values for
	// a 401, per RFC 6750 §3: a request with no credential gets the bare
	// challenge; one whose credential was examined and rejected also carries
	// error="invalid_token".
	challengeMissing = `Bearer realm="nvpair-proxy"`
	challengeInvalid = `Bearer realm="nvpair-proxy", error="invalid_token"`

	unauthorizedMessage = "a valid API key is required for non-loopback requests; " +
		"send it as Authorization: Bearer <key> or X-Api-Key: <key>"
)

// Decision is the gate's verdict on one request. Enabled reports whether any
// key is configured at the moment of the call; when it is false the proxy
// applies its loopback-only refusal and the other fields are unset. When
// Enabled is true and Allowed is false, Status, Code, and Message are what the
// proxy should answer with, in the same shape as its other ingress rejections,
// and Challenge, when non-empty, is the WWW-Authenticate value to send.
// KeyFingerprint identifies the presented key for the log without revealing it;
// it is "none" when no credential was examined.
type Decision struct {
	Enabled        bool
	Allowed        bool
	Status         int
	Code           string
	Message        string
	Challenge      string
	KeyFingerprint string
}

// digest is a SHA-256 of a key. A distinct type, so a raw byte array cannot be
// mistaken for one.
type digest [sha256.Size]byte

func digestOf(key string) digest { return digest(sha256.Sum256([]byte(key))) }

// Gate holds the configured credentials and allowlist. Its zero value is a
// disabled gate; construct one with FromEnv or New.
type Gate struct {
	mu sync.Mutex

	// inline keys come from the environment (or New) and never change.
	inline []digest
	// broken records an unrecoverable configuration error in the environment
	// (a malformed inline key or CIDR). The gate then stays disabled for the
	// life of the process, regardless of the key file. The proxy keeps serving
	// loopback clients; taking it down for an optional setting would punish
	// the desktop application for an operator's typo.
	broken bool

	// filePath, when non-empty, is re-read (at most once per recheckEvery) on
	// Authorize so keys can be rotated without restarting the proxy. The
	// cached keys are reused while the file's content hash is unchanged.
	// explicitFile records that the operator named the path, so its absence
	// is worth reporting. fileErr is the last error logged for the file, so a
	// persisting problem is reported once rather than per request and a new
	// problem is reported when it appears.
	filePath     string
	explicitFile bool
	fileKeys     []digest
	fileHash     digest
	fileLoaded   bool
	fileErr      string
	recheckEvery time.Duration
	lastCheck    time.Time

	cidrs []netip.Prefix

	// announced* remember the last state written to the log, so a change is
	// reported once rather than on every request. The file hash is part of the
	// state so a rotation that keeps the key count is still reported.
	announcedOnce    bool
	announcedEnabled bool
	announcedKeys    int
	announcedHash    digest
}

// New builds a gate from literal keys and prefixes, for tests and callers that
// resolve configuration themselves. Keys are validated like file entries; an
// invalid key panics, since a caller passing literals has a programming error
// rather than an operator mistake.
func New(keys []string, cidrs []netip.Prefix) *Gate {
	g := &Gate{cidrs: cidrs}
	for _, k := range keys {
		if err := validateKey(k); err != nil {
			panic("ingressauth.New: " + err.Error())
		}
		g.inline = append(g.inline, digestOf(k))
	}
	g.mu.Lock()
	g.announceLocked()
	g.mu.Unlock()
	return g
}

// FromEnv builds the gate from the process environment. It never fails: a
// configuration error is logged and yields a gate that stays disabled, which
// leaves the proxy in its loopback-only default.
func FromEnv() *Gate {
	g := &Gate{recheckEvery: defaultRecheckEvery}

	if raw := os.Getenv(EnvKeys); strings.TrimSpace(raw) != "" {
		for i, k := range strings.Split(raw, ",") {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			if err := validateKey(k); err != nil {
				slog.Error("authenticated LAN ingress disabled: invalid inline API key",
					"env", EnvKeys, "entry", i+1, "err", err)
				g.broken = true
				break
			}
			g.inline = append(g.inline, digestOf(k))
		}
	}

	if raw := os.Getenv(EnvAllowedCIDRs); strings.TrimSpace(raw) != "" {
		for _, s := range strings.Split(raw, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			prefix, err := netip.ParsePrefix(s)
			if err != nil {
				slog.Error("authenticated LAN ingress disabled: invalid CIDR allowlist entry",
					"env", EnvAllowedCIDRs, "entry", s, "err", err)
				g.broken = true
				break
			}
			g.cidrs = append(g.cidrs, prefix.Masked())
		}
	}

	if p := os.Getenv(EnvKeysFile); p != "" {
		g.filePath = p
		g.explicitFile = true
	} else if p, err := appdir.Path(DefaultKeyFileName); err == nil {
		g.filePath = p
	} else {
		slog.Warn("authenticated LAN ingress: cannot resolve the default key file location", "err", err)
	}

	g.mu.Lock()
	g.refreshLocked()
	g.announceLocked()
	g.mu.Unlock()
	return g
}

// Enabled reports whether at least one API key is configured, re-reading the
// key file first if it is due. The proxy does not call this per request —
// Authorize reports the same thing in its Decision from a single refresh — but
// it is the natural question for startup logging and tests.
func (g *Gate) Enabled() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refreshLocked()
	g.announceLocked()
	return g.enabledLocked()
}

func (g *Gate) enabledLocked() bool {
	return !g.broken && len(g.inline)+len(g.fileKeys) > 0
}

// Authorize decides whether a non-loopback plaintext request may proceed. It
// refreshes the key file once and answers from that single view, so the
// enabled/disabled state and the key set a request is judged against cannot
// change between two calls. The allowlist is checked before the credential,
// so a caller outside it learns nothing about whether its key is valid — and
// the proxy applies that source decision even to a preflight, which needs no
// credential. Authorize does not write to the response; the proxy does, in
// its own error format.
func (g *Gate) Authorize(r *http.Request) Decision {
	g.mu.Lock()
	g.refreshLocked()
	g.announceLocked()
	enabled := g.enabledLocked()
	cidrs := g.cidrs
	digests := make([]digest, 0, len(g.inline)+len(g.fileKeys))
	digests = append(digests, g.inline...)
	digests = append(digests, g.fileKeys...)
	g.mu.Unlock()

	if !enabled {
		return Decision{}
	}

	if len(cidrs) > 0 {
		ip, ok := remoteAddr(r)
		if !ok || !anyPrefixContains(cidrs, ip) {
			return Decision{Enabled: true, Status: http.StatusForbidden, Code: CodeSourceNotAllowed,
				Message: "the caller's address is outside " + EnvAllowedCIDRs, KeyFingerprint: noCredential}
		}
	}

	creds := credentialsFrom(r)
	if len(creds) == 0 {
		return Decision{Enabled: true, Status: http.StatusUnauthorized, Code: CodeUnauthorized,
			Message: unauthorizedMessage, Challenge: challengeMissing, KeyFingerprint: noCredential}
	}
	// Either presented credential may match. An SDK that always sends a
	// placeholder Bearer token alongside the real X-Api-Key must not be locked
	// out by header precedence; both headers are the caller's to set, so
	// checking both costs nothing in security.
	for _, cred := range creds {
		if matchesAny(digests, digestOf(cred)) {
			return Decision{Enabled: true, Allowed: true, KeyFingerprint: fingerprint(cred)}
		}
	}
	return Decision{Enabled: true, Status: http.StatusUnauthorized, Code: CodeUnauthorized,
		Message: unauthorizedMessage, Challenge: challengeInvalid, KeyFingerprint: fingerprint(creds[0])}
}

// StripCredential removes the presented key from a request the gate admitted,
// so the proxy's credential is never forwarded to an engine or a peer.
func (g *Gate) StripCredential(h http.Header) {
	h.Del(headerAuthorization)
	h.Del(headerAPIKey)
}

// fingerprint returns the first eight hex characters of a key's SHA-256 digest:
// enough for an operator to tell repeated rejections of one misconfigured
// client apart from a scan, without the log ever holding the key. It is
// deliberately unsalted so an operator can compute it from the key they meant
// to configure and confirm which client is misconfigured; the price is that
// the log holds 32 bits of a truncated hash of a client's key, which is one
// more reason keys must be random rather than memorable.
func fingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:4])
}

// matchesAny compares the presented digest against every configured digest
// with a constant-time comparison and no early exit, so neither the key
// length nor the position of a match is observable through timing. (The
// number of configured keys is not a secret.)
func matchesAny(configured []digest, presented digest) bool {
	match := 0
	for i := range configured {
		match |= subtle.ConstantTimeCompare(configured[i][:], presented[:])
	}
	return match == 1
}

// credentialsFrom extracts the client's presented keys: a Bearer token and an
// X-Api-Key header, in that order, whichever are present and no longer than
// MaxKeyLength (an over-long value cannot be a key and is not hashed). A query
// parameter is deliberately not accepted, because URLs end up in access logs
// and browser histories.
func credentialsFrom(r *http.Request) []string {
	var creds []string
	if auth := strings.TrimSpace(r.Header.Get(headerAuthorization)); auth != "" {
		scheme, token, found := strings.Cut(auth, " ")
		if found && strings.EqualFold(scheme, bearerScheme) {
			if token = strings.TrimSpace(token); token != "" && len(token) <= MaxKeyLength {
				creds = append(creds, token)
			}
		}
	}
	if key := strings.TrimSpace(r.Header.Get(headerAPIKey)); key != "" && len(key) <= MaxKeyLength {
		creds = append(creds, key)
	}
	return creds
}

// remoteAddr parses the transport-level peer address. Forwarding headers are
// never consulted: the gate is meant to face callers directly, and a header a
// caller sets itself is not evidence of where it is.
func remoteAddr(r *http.Request) (netip.Addr, bool) {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	return ap.Addr().Unmap(), true
}

func anyPrefixContains(prefixes []netip.Prefix, ip netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// refreshLocked re-reads the key file, at most once per recheckEvery, and
// swaps the cached keys when its content changed. The file is read rather
// than stat-compared: a key file is a few short lines, and a stamp of size
// and modification time cannot see a same-length rewrite within the
// filesystem's timestamp granularity — exactly the rotation a compromised key
// needs. Caller holds g.mu.
func (g *Gate) refreshLocked() {
	if g.filePath == "" || g.broken {
		return
	}
	now := time.Now()
	if !g.lastCheck.IsZero() && now.Sub(g.lastCheck) < g.recheckEvery {
		return
	}
	g.lastCheck = now

	content, err := readKeyFile(g.filePath)
	switch {
	case err == nil:
		g.fileErr = ""
		hash := digest(sha256.Sum256(content))
		if g.fileLoaded && hash == g.fileHash {
			return
		}
		keys, perr := parseKeys(bytes.NewReader(content))
		if perr != nil {
			g.dropFileKeysLocked()
			g.logFileErrorLocked("key file ignored; no file keys are in effect", perr)
			return
		}
		g.fileKeys, g.fileHash, g.fileLoaded = keys, hash, true
	case errors.Is(err, fs.ErrNotExist):
		g.dropFileKeysLocked()
		if g.explicitFile {
			g.logFileErrorLocked("key file does not exist; no file keys are in effect", err)
		}
	default:
		g.dropFileKeysLocked()
		g.logFileErrorLocked("key file ignored; no file keys are in effect", err)
	}
}

func (g *Gate) dropFileKeysLocked() {
	g.fileKeys, g.fileHash, g.fileLoaded = nil, digest{}, false
}

// logFileErrorLocked reports a key-file problem once per distinct error, so a
// persisting misconfiguration does not write a line per request while a new
// one is still reported the moment it appears.
func (g *Gate) logFileErrorLocked(msg string, err error) {
	if err.Error() == g.fileErr {
		return
	}
	g.fileErr = err.Error()
	slog.Error("authenticated LAN ingress: "+msg, "path", g.filePath, "err", err)
}

// readKeyFile opens the key file and checks the opened handle — not a separate
// stat — before reading, so the permissions, type, and owner it validates
// belong to the file it reads. On Unix-like systems the file must belong to
// the proxy's user (or root) and must not be readable or writable by group or
// others; on Windows the mode bits carry no such meaning and the check is
// skipped, leaving protection to the data directory's ACL.
func readKeyFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return nil, fmt.Errorf("permissions %04o allow other users to read it; chmod 600", perm)
		}
	}
	if err := ownedByProcessUser(info); err != nil {
		return nil, err
	}
	content, err := io.ReadAll(io.LimitReader(f, maxKeyFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) > maxKeyFileBytes {
		return nil, fmt.Errorf("larger than %d bytes; not a key file", maxKeyFileBytes)
	}
	return content, nil
}

// parseKeys reads one key per line. Blank lines and lines starting with '#' are
// ignored; surrounding whitespace, including a CR from a Windows editor, is
// trimmed. Any invalid entry fails the whole file: a key that can never match
// over the wire is a misconfiguration to surface, not to skip.
func parseKeys(r io.Reader) ([]digest, error) {
	var keys []digest
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		entry := strings.TrimSpace(sc.Text())
		if entry == "" || strings.HasPrefix(entry, "#") {
			continue
		}
		if err := validateKey(entry); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		keys = append(keys, digestOf(entry))
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, errors.New("contains no keys")
	}
	return keys, nil
}

// validateKey enforces the shape a key must have to be usable at all: long
// enough to resist guessing, and drawn from the RFC 6750 b64token alphabet
// (letters, digits, and - . _ ~ + / =) so it survives an Authorization header
// unchanged through any conformant intermediary. Hex and base64 output both
// qualify.
func validateKey(key string) error {
	if len(key) < MinKeyLength {
		return fmt.Errorf("key is %d characters; at least %d are required", len(key), MinKeyLength)
	}
	if len(key) > MaxKeyLength {
		return fmt.Errorf("key is %d characters; at most %d are allowed", len(key), MaxKeyLength)
	}
	for i := 0; i < len(key); i++ {
		if !isTokenByte(key[i]) {
			return errors.New("key must use only letters, digits, and - . _ ~ + / =")
		}
	}
	return nil
}

func isTokenByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("-._~+/=", c) >= 0
}

// announceLocked logs a change in the gate's state — enabled with N keys, a
// rotation of the key file, or back to disabled — once per change. Enabling
// is logged at Warn: it widens the proxy's exposure and an operator reading
// the log should see it plainly. Caller holds g.mu.
func (g *Gate) announceLocked() {
	enabled := g.enabledLocked()
	n := len(g.inline) + len(g.fileKeys)
	if g.announcedOnce && enabled == g.announcedEnabled && n == g.announcedKeys && g.fileHash == g.announcedHash {
		return
	}
	rotated := g.announcedOnce && enabled && g.announcedEnabled && g.fileHash != g.announcedHash
	g.announcedOnce, g.announcedEnabled, g.announcedKeys, g.announcedHash = true, enabled, n, g.fileHash
	switch {
	case rotated:
		slog.Warn("authenticated LAN ingress: key file changed; the configured key set was replaced",
			"keys", n, "key_file", g.filePath)
	case enabled:
		cidrs := make([]string, 0, len(g.cidrs))
		for _, p := range g.cidrs {
			cidrs = append(cidrs, p.String())
		}
		slog.Warn("authenticated LAN ingress ENABLED: a non-loopback plaintext caller presenting a configured API key is routed",
			"keys", n, "key_file", g.filePath, "allowed_cidrs", cidrs)
	default:
		slog.Info("authenticated LAN ingress disabled; plaintext requests are accepted from loopback only",
			"key_file", g.filePath)
	}
}
