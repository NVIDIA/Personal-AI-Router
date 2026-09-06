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
	"unicode"

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
	// drawn from hex already carries 128 bits, which puts online guessing out
	// of reach without a lockout mechanism.
	MinKeyLength = 32

	// CodeUnauthorized is the ingress error code for a missing or wrong key.
	CodeUnauthorized = "unauthorized"
	// CodeSourceNotAllowed is the ingress error code for a caller outside the
	// configured CIDR allowlist.
	CodeSourceNotAllowed = "source-not-allowed"

	headerAuthorization = "Authorization"
	headerAPIKey        = "X-Api-Key"
	bearerScheme        = "bearer"
	noCredential        = "none"

	unauthorizedMessage = "a valid API key is required for non-loopback requests; " +
		"send it as Authorization: Bearer <key> or X-Api-Key: <key>"
)

// Decision is the gate's verdict on one request. When Allowed is false, Status,
// Code, and Message are what the proxy should answer with, in the same shape as
// its other ingress rejections. KeyFingerprint identifies the presented key for
// the log without revealing it; it is "none" when no credential was sent.
type Decision struct {
	Allowed        bool
	Status         int
	Code           string
	Message        string
	KeyFingerprint string
}

type digest = [sha256.Size]byte

// fileStamp is the part of a key file's metadata that decides whether it must
// be re-read. Mode is included because fixing permissions with chmod changes
// neither size nor modification time, yet must take effect.
type fileStamp struct {
	size    int64
	modTime time.Time
	mode    fs.FileMode
	exists  bool
}

// Gate holds the configured credentials and allowlist. Its zero value is a
// disabled gate; construct one with FromEnv or New.
type Gate struct {
	mu sync.Mutex

	// inline keys come from the environment (or New) and never change.
	inline []digest
	// broken records an unrecoverable configuration error in the environment
	// (a malformed inline key or CIDR). The gate then stays disabled for the
	// life of the process, regardless of the key file.
	broken bool

	// filePath, when non-empty, is re-checked on every Enabled call so keys can
	// be rotated without restarting the proxy. explicitFile records that the
	// operator named the path, so its absence is worth reporting.
	filePath     string
	explicitFile bool
	fileKeys     []digest
	fileStamp    fileStamp
	fileChecked  bool

	cidrs []netip.Prefix

	// announced* remember the last state written to the log, so a change is
	// reported once rather than on every request.
	announcedOnce    bool
	announcedEnabled bool
	announcedKeys    int
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
		g.inline = append(g.inline, sha256.Sum256([]byte(k)))
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
	g := &Gate{}

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
			g.inline = append(g.inline, sha256.Sum256([]byte(k)))
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
// key file first if it changed. The proxy consults this per non-loopback
// request, so adding, rotating, or removing keys needs no restart.
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

// Authorize decides whether a non-loopback plaintext request may proceed. The
// allowlist is checked before the credential, so a caller outside it learns
// nothing about whether its key is valid. Authorize does not write to the
// response; the proxy does, in its own error format.
func (g *Gate) Authorize(r *http.Request) Decision {
	g.mu.Lock()
	g.refreshLocked()
	cidrs := g.cidrs
	digests := make([]digest, 0, len(g.inline)+len(g.fileKeys))
	digests = append(digests, g.inline...)
	digests = append(digests, g.fileKeys...)
	enabled := g.enabledLocked()
	g.mu.Unlock()

	if !enabled {
		// The proxy only asks an enabled gate; answer conservatively anyway.
		return Decision{Status: http.StatusForbidden, Code: CodeSourceNotAllowed,
			Message: "authenticated LAN ingress is not enabled", KeyFingerprint: noCredential}
	}

	if len(cidrs) > 0 {
		ip, ok := remoteAddr(r)
		if !ok || !anyPrefixContains(cidrs, ip) {
			return Decision{Status: http.StatusForbidden, Code: CodeSourceNotAllowed,
				Message: "the caller's address is outside " + EnvAllowedCIDRs, KeyFingerprint: noCredential}
		}
	}

	cred, ok := credentialFrom(r)
	if !ok {
		return Decision{Status: http.StatusUnauthorized, Code: CodeUnauthorized,
			Message: unauthorizedMessage, KeyFingerprint: noCredential}
	}
	if !matchesAny(digests, sha256.Sum256([]byte(cred))) {
		return Decision{Status: http.StatusUnauthorized, Code: CodeUnauthorized,
			Message: unauthorizedMessage, KeyFingerprint: Fingerprint(cred)}
	}
	return Decision{Allowed: true, Status: http.StatusOK, KeyFingerprint: Fingerprint(cred)}
}

// StripCredential removes the presented key from a request the gate admitted,
// so the proxy's credential is never forwarded to an engine or a peer.
func (g *Gate) StripCredential(h http.Header) {
	h.Del(headerAuthorization)
	h.Del(headerAPIKey)
}

// Fingerprint returns the first eight hex characters of a key's SHA-256 digest:
// enough for an operator to tell repeated rejections of one misconfigured
// client apart from a scan, without the log ever holding the key.
func Fingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:4])
}

// matchesAny compares the presented digest against every configured digest in
// constant time and without an early exit, so neither the key length nor the
// position of a match is observable through timing.
func matchesAny(configured []digest, presented digest) bool {
	match := 0
	for i := range configured {
		match |= subtle.ConstantTimeCompare(configured[i][:], presented[:])
	}
	return match == 1
}

// credentialFrom extracts the client's key: a Bearer token first, then the
// X-Api-Key header. A query parameter is deliberately not accepted, because
// URLs end up in access logs and browser histories.
func credentialFrom(r *http.Request) (string, bool) {
	if auth := strings.TrimSpace(r.Header.Get(headerAuthorization)); auth != "" {
		scheme, token, found := strings.Cut(auth, " ")
		if found && strings.EqualFold(scheme, bearerScheme) {
			if token = strings.TrimSpace(token); token != "" {
				return token, true
			}
		}
	}
	if key := strings.TrimSpace(r.Header.Get(headerAPIKey)); key != "" {
		return key, true
	}
	return "", false
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

// refreshLocked re-reads the key file when its metadata changed since the last
// look. Caller holds g.mu.
func (g *Gate) refreshLocked() {
	if g.filePath == "" || g.broken {
		return
	}
	info, err := os.Stat(g.filePath)
	var stamp fileStamp
	switch {
	case err == nil:
		stamp = fileStamp{size: info.Size(), modTime: info.ModTime(), mode: info.Mode(), exists: true}
	case errors.Is(err, fs.ErrNotExist):
		stamp = fileStamp{}
	default:
		// A stat failure other than absence (a parent directory's permissions,
		// an I/O error) counts as absence for this request and is re-examined
		// on the next; report it when it is news.
		if !g.fileChecked || g.fileStamp.exists {
			slog.Error("authenticated LAN ingress: cannot stat key file; no file keys are in effect",
				"path", g.filePath, "err", err)
		}
		stamp = fileStamp{}
	}
	if g.fileChecked && stamp == g.fileStamp {
		return
	}
	g.fileChecked = true
	g.fileStamp = stamp
	g.fileKeys = nil

	if !stamp.exists {
		if g.explicitFile {
			slog.Error("authenticated LAN ingress: key file does not exist; no file keys are in effect",
				"env", EnvKeysFile, "path", g.filePath)
		}
		return
	}
	keys, err := loadKeyFile(g.filePath, info)
	if err != nil {
		slog.Error("authenticated LAN ingress: key file ignored; no file keys are in effect",
			"path", g.filePath, "err", err)
		return
	}
	g.fileKeys = keys
}

// loadKeyFile reads and validates a key file. On Unix-like systems the file must
// not be readable or writable by group or others; on Windows the mode bits
// carry no such meaning and the check is skipped.
func loadKeyFile(path string, info fs.FileInfo) ([]digest, error) {
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return nil, fmt.Errorf("permissions %04o allow other users to read it; chmod 600", perm)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseKeys(f)
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
		keys = append(keys, sha256.Sum256([]byte(entry)))
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
// enough to resist guessing, and printable ASCII with no whitespace so it
// survives an HTTP header unchanged.
func validateKey(key string) error {
	if len(key) < MinKeyLength {
		return fmt.Errorf("key is %d characters; at least %d are required", len(key), MinKeyLength)
	}
	for _, c := range key {
		if c > unicode.MaxASCII || c <= ' ' || c == 0x7f {
			return errors.New("key must be printable ASCII with no whitespace")
		}
	}
	return nil
}

// announceLocked logs a change in the gate's state — enabled with N keys, or
// back to disabled — once per change. Enabling is logged at Warn: it widens the
// proxy's exposure and an operator reading the log should see it plainly.
// Caller holds g.mu.
func (g *Gate) announceLocked() {
	enabled := g.enabledLocked()
	n := len(g.inline) + len(g.fileKeys)
	if g.announcedOnce && enabled == g.announcedEnabled && n == g.announcedKeys {
		return
	}
	g.announcedOnce, g.announcedEnabled, g.announcedKeys = true, enabled, n
	if enabled {
		cidrs := make([]string, 0, len(g.cidrs))
		for _, p := range g.cidrs {
			cidrs = append(cidrs, p.String())
		}
		slog.Warn("authenticated LAN ingress ENABLED: a non-loopback plaintext caller presenting a configured API key is routed",
			"keys", n, "key_file", g.filePath, "allowed_cidrs", cidrs)
		return
	}
	slog.Info("authenticated LAN ingress disabled; plaintext requests are accepted from loopback only",
		"key_file", g.filePath)
}
