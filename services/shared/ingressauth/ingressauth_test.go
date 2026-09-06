// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ingressauth

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	keyA = "0123456789abcdef0123456789abcdef"                                 // exactly MinKeyLength
	keyB = "b-key-b-key-b-key-b-key-b-key-b-key-b-key-0000"                   // longer, with dashes
	keyC = "sk-nvpair-cccccccccccccccccccccccccccccccccccccccccccccccccccccc" // prefixed, like SDK keys
)

func request(remote string, hdr ...string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = remote
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	return r
}

func TestValidateKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
		ok   bool
	}{
		{"exactly minimum length", keyA, true},
		{"longer with punctuation", keyC, true},
		{"one short", keyA[:MinKeyLength-1], false},
		{"empty", "", false},
		{"embedded space", "0123456789abcdef 123456789abcdef0", false},
		{"embedded tab", "0123456789abcdef\t123456789abcdef0", false},
		{"non-ascii", "0123456789abcdef0123456789abcdé", false},
		{"control char", "0123456789abcdef0123456789abcde\x01", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateKey(tc.key)
			if (err == nil) != tc.ok {
				t.Fatalf("validateKey(%q) err = %v, want ok=%v", tc.key, err, tc.ok)
			}
		})
	}
}

func TestParseKeysSkipsCommentsBlanksAndCRLF(t *testing.T) {
	in := "# leading comment\r\n\r\n  " + keyA + "  \r\n" + keyB + "\n\n   # trailing comment\n"
	keys, err := parseKeys(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parseKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("parsed %d keys, want 2", len(keys))
	}
	g := &Gate{inline: keys}
	for _, k := range []string{keyA, keyB} {
		if d := g.Authorize(request("192.0.2.9:1", "Authorization", "Bearer "+k)); !d.Allowed {
			t.Errorf("key %q from file not accepted: %+v", k, d)
		}
	}
}

func TestParseKeysRejectsWholeFileOnOneBadEntry(t *testing.T) {
	cases := map[string]string{
		"short entry":    keyA + "\nshort\n",
		"whitespace":     "0123456789abcdef 123456789abcdef0\n",
		"only comments":  "# nothing here\n\n",
		"empty":          "",
		"non-ascii line": keyA + "\n0123456789abcdef0123456789abcdé\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if keys, err := parseKeys(strings.NewReader(in)); err == nil {
				t.Fatalf("parseKeys accepted %d keys, want an error", len(keys))
			}
		})
	}
}

func TestAuthorizeCredentials(t *testing.T) {
	g := New([]string{keyA, keyB}, nil)
	cases := []struct {
		name   string
		hdr    []string
		allow  bool
		status int
		fp     string
	}{
		{"bearer first key", []string{"Authorization", "Bearer " + keyA}, true, http.StatusOK, Fingerprint(keyA)},
		{"bearer second key", []string{"Authorization", "Bearer " + keyB}, true, http.StatusOK, Fingerprint(keyB)},
		{"lowercase scheme", []string{"Authorization", "bearer " + keyA}, true, http.StatusOK, Fingerprint(keyA)},
		{"x-api-key", []string{"X-Api-Key", keyA}, true, http.StatusOK, Fingerprint(keyA)},
		{"x-api-key lowercase header name", []string{"x-api-key", keyB}, true, http.StatusOK, Fingerprint(keyB)},
		{"no credential", nil, false, http.StatusUnauthorized, noCredential},
		{"wrong key", []string{"Authorization", "Bearer " + keyC}, false, http.StatusUnauthorized, Fingerprint(keyC)},
		{"wrong scheme", []string{"Authorization", "Basic " + keyA}, false, http.StatusUnauthorized, noCredential},
		{"bearer with no token", []string{"Authorization", "Bearer "}, false, http.StatusUnauthorized, noCredential},
		{"key as prefix only", []string{"Authorization", "Bearer " + keyA + "x"}, false, http.StatusUnauthorized, Fingerprint(keyA + "x")},
		{"wrong bearer but right x-api-key", []string{"Authorization", "Bearer " + keyC, "X-Api-Key", keyA}, false, http.StatusUnauthorized, Fingerprint(keyC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := g.Authorize(request("192.0.2.9:40000", tc.hdr...))
			if d.Allowed != tc.allow || d.Status != tc.status {
				t.Fatalf("decision = %+v, want allowed=%v status=%d", d, tc.allow, tc.status)
			}
			if d.KeyFingerprint != tc.fp {
				t.Errorf("fingerprint = %q, want %q", d.KeyFingerprint, tc.fp)
			}
			if !tc.allow && d.Code != CodeUnauthorized {
				t.Errorf("code = %q, want %q", d.Code, CodeUnauthorized)
			}
			if strings.Contains(d.Message, keyA) || strings.Contains(d.Message, keyC) {
				t.Errorf("message echoes a key: %q", d.Message)
			}
		})
	}
}

func TestAuthorizeCIDRAllowlist(t *testing.T) {
	g := New([]string{keyA}, []netip.Prefix{
		netip.MustParsePrefix("192.168.1.0/24"),
		netip.MustParsePrefix("fd00::/8"),
	})
	auth := []string{"Authorization", "Bearer " + keyA}
	cases := []struct {
		name   string
		remote string
		hdr    []string
		allow  bool
		code   string
	}{
		{"inside v4 with key", "192.168.1.77:5000", auth, true, ""},
		{"inside v6 with key", "[fd00::1]:5000", auth, true, ""},
		{"ipv4-mapped v6 inside", "[::ffff:192.168.1.77]:5000", auth, true, ""},
		{"outside with valid key", "192.168.2.77:5000", auth, false, CodeSourceNotAllowed},
		{"outside without key", "10.0.0.5:5000", nil, false, CodeSourceNotAllowed},
		{"inside without key", "192.168.1.77:5000", nil, false, CodeUnauthorized},
		{"unparseable remote", "garbage", auth, false, CodeSourceNotAllowed},
		{"empty remote", "", auth, false, CodeSourceNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := g.Authorize(request(tc.remote, tc.hdr...))
			if d.Allowed != tc.allow {
				t.Fatalf("decision = %+v, want allowed=%v", d, tc.allow)
			}
			if !tc.allow && d.Code != tc.code {
				t.Errorf("code = %q, want %q", d.Code, tc.code)
			}
			if d.Code == CodeSourceNotAllowed {
				if d.Status != http.StatusForbidden {
					t.Errorf("status = %d, want 403", d.Status)
				}
				// An out-of-policy source learns nothing about its key.
				if d.KeyFingerprint != noCredential {
					t.Errorf("fingerprint = %q, want %q for a source rejection", d.KeyFingerprint, noCredential)
				}
			}
		})
	}
}

func TestAuthorizeIgnoresForwardingHeaders(t *testing.T) {
	g := New([]string{keyA}, []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")})
	d := g.Authorize(request("10.9.9.9:1",
		"Authorization", "Bearer "+keyA,
		"X-Forwarded-For", "192.168.1.5",
		"X-Real-IP", "192.168.1.5",
		"Forwarded", "for=192.168.1.5"))
	if d.Allowed {
		t.Fatal("a forwarding header moved the caller inside the allowlist")
	}
}

func TestDisabledGateNeverAllows(t *testing.T) {
	var zero Gate
	if zero.Enabled() {
		t.Fatal("zero Gate reports enabled")
	}
	if d := zero.Authorize(request("192.0.2.9:1", "Authorization", "Bearer "+keyA)); d.Allowed {
		t.Fatalf("zero Gate allowed a request: %+v", d)
	}
	empty := New(nil, nil)
	if empty.Enabled() {
		t.Fatal("New(nil, nil) reports enabled")
	}
}

func TestFingerprintIsShortStableHexAndNotTheKey(t *testing.T) {
	fp := Fingerprint(keyA)
	if len(fp) != 8 {
		t.Fatalf("fingerprint %q has length %d, want 8", fp, len(fp))
	}
	if strings.ToLower(fp) != fp || strings.Trim(fp, "0123456789abcdef") != "" {
		t.Fatalf("fingerprint %q is not lowercase hex", fp)
	}
	if fp != Fingerprint(keyA) {
		t.Fatal("fingerprint is not stable")
	}
	if fp == Fingerprint(keyB) {
		t.Fatal("distinct keys share a fingerprint")
	}
	if strings.Contains(keyA, fp) {
		t.Fatal("fingerprint is a substring of the key")
	}
}

func TestStripCredentialRemovesBothHeaders(t *testing.T) {
	g := New([]string{keyA}, nil)
	r := request("192.0.2.9:1", "Authorization", "Bearer "+keyA, "X-Api-Key", keyA, "Content-Type", "application/json")
	g.StripCredential(r.Header)
	if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
		t.Fatalf("credential headers survived: %v", r.Header)
	}
	if r.Header.Get("Content-Type") != "application/json" {
		t.Fatal("an unrelated header was removed")
	}
}

func TestNewPanicsOnInvalidLiteralKey(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted an invalid literal key")
		}
	}()
	New([]string{"short"}, nil)
}

// writeKeyFile writes content with mode and pins the modification time so a
// rewrite is distinguishable from the previous version even on filesystems
// with coarse timestamps.
func writeKeyFile(t *testing.T, path, content string, mode os.FileMode, when time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func TestKeyFileLoadsRotatesAndFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys")
	g := &Gate{filePath: path, explicitFile: true}
	base := time.Now().Add(-time.Hour).Truncate(time.Second)

	if g.Enabled() {
		t.Fatal("enabled before the key file exists")
	}

	writeKeyFile(t, path, "# first key\n"+keyA+"\n", 0o600, base)
	if !g.Enabled() {
		t.Fatal("not enabled after the key file appeared")
	}
	if d := g.Authorize(request("192.0.2.9:1", "Authorization", "Bearer "+keyA)); !d.Allowed {
		t.Fatalf("file key rejected: %+v", d)
	}

	// Rotation: replace A with B without a restart.
	writeKeyFile(t, path, keyB+"\n", 0o600, base.Add(10*time.Second))
	if d := g.Authorize(request("192.0.2.9:1", "Authorization", "Bearer "+keyA)); d.Allowed {
		t.Fatal("rotated-out key still accepted")
	}
	if d := g.Authorize(request("192.0.2.9:1", "Authorization", "Bearer "+keyB)); !d.Allowed {
		t.Fatalf("rotated-in key rejected: %+v", d)
	}

	// A malformed rewrite contributes nothing: the LAN closes rather than
	// staying open on the previous keys.
	writeKeyFile(t, path, keyB+"\nshort\n", 0o600, base.Add(20*time.Second))
	if g.Enabled() {
		t.Fatal("enabled on a malformed key file")
	}
	if d := g.Authorize(request("192.0.2.9:1", "Authorization", "Bearer "+keyB)); d.Allowed {
		t.Fatal("previous keys survived a failed reload")
	}

	// Repairing the file re-enables; removing it disables.
	writeKeyFile(t, path, keyB+"\n", 0o600, base.Add(30*time.Second))
	if !g.Enabled() {
		t.Fatal("not re-enabled after the file was repaired")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if g.Enabled() {
		t.Fatal("enabled after the key file was removed")
	}
}

func TestKeyFilePermissionsMustBePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "keys")
	g := &Gate{filePath: path, explicitFile: true}
	when := time.Now().Add(-time.Hour).Truncate(time.Second)

	writeKeyFile(t, path, keyA+"\n", 0o644, when)
	if g.Enabled() {
		t.Fatal("enabled on a world-readable key file")
	}
	// chmod alone changes neither size nor mtime; the gate must still notice.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if !g.Enabled() {
		t.Fatal("not enabled after permissions were tightened")
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if g.Enabled() {
		t.Fatal("enabled on a group-readable key file")
	}
}

func TestKeyFileMustBeRegular(t *testing.T) {
	dir := t.TempDir()
	g := &Gate{filePath: dir, explicitFile: true}
	if g.Enabled() {
		t.Fatal("a directory was accepted as a key file")
	}
}

// clearEnv points every variable the gate reads, and every base directory
// appdir consults, at the test's own scratch space.
func clearEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv(EnvKeys, "")
	t.Setenv(EnvKeysFile, "")
	t.Setenv(EnvAllowedCIDRs, "")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("LOCALAPPDATA", home)
	t.Setenv("APPDATA", home)
	return home
}

func TestFromEnvUnsetIsDisabled(t *testing.T) {
	clearEnv(t)
	g := FromEnv()
	if g.Enabled() {
		t.Fatal("FromEnv with nothing configured is enabled")
	}
	if g.filePath == "" || filepath.Base(g.filePath) != DefaultKeyFileName {
		t.Fatalf("default key file path = %q, want .../%s", g.filePath, DefaultKeyFileName)
	}
}

func TestFromEnvInlineKeysAndFileCombine(t *testing.T) {
	home := clearEnv(t)
	path := filepath.Join(home, "keys")
	writeKeyFile(t, path, keyB+"\n", 0o600, time.Now().Add(-time.Hour))
	t.Setenv(EnvKeys, " "+keyA+" , ,"+keyC)
	t.Setenv(EnvKeysFile, path)

	g := FromEnv()
	if !g.Enabled() {
		t.Fatal("not enabled")
	}
	for _, k := range []string{keyA, keyB, keyC} {
		if d := g.Authorize(request("192.0.2.9:1", "X-Api-Key", k)); !d.Allowed {
			t.Errorf("key %q rejected: %+v", k, d)
		}
	}
}

func TestFromEnvDefaultFileInAppDir(t *testing.T) {
	home := clearEnv(t)
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Skip("no user config dir:", err)
	}
	if !strings.HasPrefix(dir, home) {
		t.Skipf("os.UserConfigDir()=%q is not under the test HOME %q on this platform", dir, home)
	}
	appDir := filepath.Join(dir, "Nvidia Corporation", "Personal AI Router")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeKeyFile(t, filepath.Join(appDir, DefaultKeyFileName), keyA+"\n", 0o600, time.Now().Add(-time.Hour))

	g := FromEnv()
	if !g.Enabled() {
		t.Fatalf("default key file at %q not picked up", g.filePath)
	}
}

func TestFromEnvInvalidInlineKeyDisablesEverything(t *testing.T) {
	home := clearEnv(t)
	path := filepath.Join(home, "keys")
	writeKeyFile(t, path, keyB+"\n", 0o600, time.Now().Add(-time.Hour))
	t.Setenv(EnvKeysFile, path)
	t.Setenv(EnvKeys, keyA+",too-short")

	g := FromEnv()
	if g.Enabled() {
		t.Fatal("enabled despite an invalid inline key")
	}
	if d := g.Authorize(request("192.0.2.9:1", "X-Api-Key", keyB)); d.Allowed {
		t.Fatal("file key accepted while the environment is misconfigured")
	}
}

func TestFromEnvInvalidCIDRDisablesEverything(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvKeys, keyA)
	t.Setenv(EnvAllowedCIDRs, "192.168.1.0/24, not-a-cidr")

	g := FromEnv()
	if g.Enabled() {
		t.Fatal("enabled despite a malformed CIDR allowlist")
	}
}

func TestFromEnvCIDRsAreMaskedAndApplied(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvKeys, keyA)
	t.Setenv(EnvAllowedCIDRs, "192.168.1.77/24")

	g := FromEnv()
	if d := g.Authorize(request("192.168.1.1:1", "X-Api-Key", keyA)); !d.Allowed {
		t.Fatalf("host bits in the prefix were not masked: %+v", d)
	}
	if d := g.Authorize(request("192.168.2.1:1", "X-Api-Key", keyA)); d.Allowed {
		t.Fatal("caller outside the allowlist admitted")
	}
}

func TestFromEnvExplicitMissingFileIsDisabled(t *testing.T) {
	home := clearEnv(t)
	t.Setenv(EnvKeysFile, filepath.Join(home, "does-not-exist"))
	if g := FromEnv(); g.Enabled() {
		t.Fatal("enabled with a missing explicit key file")
	}
}
