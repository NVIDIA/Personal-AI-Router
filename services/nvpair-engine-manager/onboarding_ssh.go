// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"nvpair-shared/hostbootstrap"
)

const onboardingVolatileKeyLimit = 32 << 10
const onboardingControllerCredentialProvider = "controller-ssh-config"

// Only dial and SSH session-open failures may carry this marker. Run errors
// cannot prove whether the native command executed.
var errBeforeRemoteCommand = errors.New("remote command was not started")

type onboardingCredentialBinding struct {
	provider, purpose, publicKeySHA256, account, candidateID, candidateSet, lifetime, accessGeneration string
	expiresAt                                                                                          time.Time
}
type onboardingAccess struct {
	user, keyPath, password, passphrase, elevationPassword, generation, usePurpose string
	signer                                                                         ssh.Signer
	provider                                                                       onboardingCredentialBinding
}

func (a onboardingAccess) forPurpose(purpose string) onboardingAccess {
	a.usePurpose = purpose
	return a
}

func parseOnboardingVolatileSigner(data []byte, passphrase string) (ssh.Signer, error) {
	if len(data) == 0 || len(data) > onboardingVolatileKeyLimit {
		return nil, errors.New("volatile SSH key is empty or exceeds its limit")
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil && passphrase != "" {
		secret := []byte(passphrase)
		signer, err = ssh.ParsePrivateKeyWithPassphrase(data, secret)
		clear(secret)
	}
	if err != nil {
		return nil, errors.New("volatile SSH key could not be unlocked")
	}
	return signer, nil
}

func (a onboardingAccess) validProviderBinding(c onboardingCandidate, now time.Time) bool {
	if a.signer == nil {
		return a.provider == (onboardingCredentialBinding{})
	}
	b := a.provider
	found := false
	for _, id := range strings.Split(b.candidateSet, ",") {
		found = found || id == c.CandidateID
	}
	return b.provider == onboardingControllerCredentialProvider && b.purpose == "enrolled-peer-upgrade" && a.usePurpose == b.purpose &&
		b.publicKeySHA256 == ssh.FingerprintSHA256(a.signer.PublicKey()) && b.account == a.user &&
		b.candidateID == c.CandidateID && found && b.lifetime == "persistent" &&
		onboardingID.MatchString(b.accessGeneration) && b.accessGeneration == a.generation && now.Before(b.expiresAt)
}

type onboardingSSH struct {
	testRun          func(context.Context, string, io.Reader) ([]byte, error)
	testCableSession func() (cableSSHSession, error)
	client           *ssh.Client
	stop             func() bool
	// Only public bytes accepted by the current trust callback and a completed
	// authenticated handshake may seed operation-local MPI known_hosts.
	hostPublicKey onboardingHostPublicKey
}

type onboardingHostPublicKey struct {
	Algorithm   string `json:"algorithm"`
	Blob        string `json:"blob"`
	Fingerprint string `json:"fingerprint"`
}

func onboardingVerifiedHostKey(expected string, checker ssh.HostKeyCallback, host string, remote net.Addr, key ssh.PublicKey) (onboardingHostPublicKey, error) {
	if err := verifyOnboardingCurrentTrust(expected, checker, host, remote, key); err != nil {
		return onboardingHostPublicKey{}, err
	}
	return onboardingHostPublicKey{Algorithm: key.Type(), Blob: base64.StdEncoding.EncodeToString(key.Marshal()), Fingerprint: ssh.FingerprintSHA256(key)}, nil
}

func (c *onboardingSSH) verifiedHostPublicKey(expected string) (onboardingHostPublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(c.hostPublicKey.Blob)
	if err != nil || len(raw) > 8192 {
		return onboardingHostPublicKey{}, errors.New("authenticated SSH public host identity is unavailable")
	}
	key, err := ssh.ParsePublicKey(raw)
	if err != nil || key.Type() != c.hostPublicKey.Algorithm || expected != c.hostPublicKey.Fingerprint || verifyOnboardingFingerprint(expected, key) != nil {
		return onboardingHostPublicKey{}, errors.New("authenticated SSH public host identity does not match the reviewed fingerprint")
	}
	return c.hostPublicKey, nil
}

func (c *onboardingSSH) close() {
	if c.stop != nil {
		c.stop()
	}
	if c.client != nil {
		_ = c.client.Close()
	}
}

// Secrets stay in Go memory and are never embedded in an SSH command line.
// The host callback runs before SSH authentication; no credential is sent to
// an unapproved fingerprint. No host pin or account is created here.
func dialOnboardingSSH(ctx context.Context, c onboardingCandidate, a onboardingAccess) (*onboardingSSH, error) {
	if !c.HostKeyTrusted || c.HostKeySHA256 == "" {
		return nil, errors.New("explicit host-key trust is required before reading access material")
	}
	if !a.validProviderBinding(c, time.Now()) {
		return nil, errors.New("volatile SSH key provider binding is unavailable, changed or expired")
	}
	checker, trustErr := loadOnboardingKnownHosts()
	if trustErr != nil {
		return nil, trustErr
	}
	var auth ssh.AuthMethod
	if a.signer != nil {
		auth = ssh.PublicKeys(a.signer)
	} else if a.keyPath != "" {
		data, err := readOnboardingFile(a.keyPath, 1<<20)
		if err != nil || len(data) > 1<<20 {
			return nil, errors.New("selected SSH key is unavailable")
		}
		defer clear(data)
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil && a.passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(a.passphrase))
		}
		if err != nil {
			return nil, errors.New("selected SSH key could not be unlocked")
		}
		auth = ssh.PublicKeys(signer)
	} else if a.password != "" {
		auth = ssh.Password(a.password)
	} else {
		return nil, errors.New("SSH access must be supplied through PAIR")
	}
	if !c.HostKeyTrusted || c.HostKeySHA256 == "" {
		return nil, errors.New("review and explicitly approve this host fingerprint before authentication")
	}
	dialer := net.Dialer{Timeout: 8 * time.Second}
	address := net.JoinHostPort(c.Address, strconv.Itoa(c.Port))
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, errors.New("SSH endpoint is not reachable")
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(12 * time.Second))
	var identityError error
	var hostPublicKey onboardingHostPublicKey
	config := &ssh.ClientConfig{User: a.user, Auth: []ssh.AuthMethod{auth}, HostKeyCallback: func(host string, remote net.Addr, key ssh.PublicKey) error {
		hostPublicKey, identityError = onboardingVerifiedHostKey(c.HostKeySHA256, checker, host, remote, key)
		return identityError
	}}
	client, chans, requests, err := ssh.NewClientConn(conn, address, config)
	if err != nil {
		stop()
		_ = conn.Close()
		if identityError != nil {
			return nil, identityError
		}
		return nil, errors.New("device account SSH authentication failed")
	}
	_ = conn.SetDeadline(time.Time{})
	return &onboardingSSH{client: ssh.NewClient(client, chans, requests), stop: stop, hostPublicKey: hostPublicKey}, nil
}

func observeOnboardingHostKey(ctx context.Context, c onboardingCandidate, user string) (fingerprint string, trusted, changed bool, err error) {
	address := net.JoinHostPort(c.Address, strconv.Itoa(c.Port))
	dialer := net.Dialer{Timeout: 8 * time.Second}
	conn, e := dialer.DialContext(ctx, "tcp", address)
	if e != nil {
		return "", false, false, errors.New("SSH endpoint is not reachable")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	checker, e := loadOnboardingKnownHosts()
	if e != nil {
		return "", false, true, e
	}
	_, _, _, _ = ssh.NewClientConn(conn, address, &ssh.ClientConfig{User: user, HostKeyCallback: func(host string, remote net.Addr, key ssh.PublicKey) error {
		fingerprint = ssh.FingerprintSHA256(key)
		if checker != nil {
			checkErr := checker(host, remote, key)
			trusted = checkErr == nil
			_, changed = classifyOnboardingTrust(checkErr)
		}
		return errors.New("fingerprint observation complete; no authentication requested")
	}})
	if fingerprint == "" {
		return "", false, false, errors.New("SSH host fingerprint could not be read")
	}
	return fingerprint, trusted, changed, nil
}
func loadOnboardingKnownHosts() (ssh.HostKeyCallback, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, errors.New("SSH trust store location is unavailable")
	}
	locations := []string{filepath.Join(home, ".ssh", "known_hosts")}
	if runtime.GOOS == "windows" {
		if programData := os.Getenv("ProgramData"); programData != "" {
			locations = append(locations, filepath.Join(programData, "ssh", "ssh_known_hosts"))
		}
	} else {
		locations = append(locations, "/etc/ssh/ssh_known_hosts")
	}
	files := []string{}
	for _, file := range locations {
		stat, err := os.Stat(file)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !stat.Mode().IsRegular() || stat.Size() > 4<<20 {
			return nil, errors.New("existing SSH trust store is unavailable or oversized")
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		return nil, nil
	}
	checker, err := knownhosts.New(files...)
	if err != nil {
		return nil, errors.New("existing SSH trust store is unreadable or invalid")
	}
	return checker, nil
}
func classifyOnboardingTrust(err error) (trusted, blocked bool) {
	if err == nil {
		return true, false
	}
	var unknown *knownhosts.KeyError
	if errors.As(err, &unknown) && len(unknown.Want) == 0 {
		return false, false
	}
	return false, true
}
func verifyOnboardingCurrentTrust(expected string, checker ssh.HostKeyCallback, host string, remote net.Addr, key ssh.PublicKey) error {
	if err := verifyOnboardingFingerprint(expected, key); err != nil {
		return err
	}
	if checker != nil {
		if _, blocked := classifyOnboardingTrust(checker(host, remote, key)); blocked {
			return errors.New("existing SSH trust rejects the reviewed host identity")
		}
	}
	return nil
}

type onboardingBoundedOutput struct {
	bytes.Buffer
	max int
}

func (w *onboardingBoundedOutput) Write(p []byte) (int, error) {
	if w.Len()+len(p) > w.max {
		return 0, errors.New("remote response limit exceeded")
	}
	return w.Buffer.Write(p)
}
func (c *onboardingSSH) run(ctx context.Context, command string, input io.Reader) ([]byte, error) {
	return c.runBounded(
		ctx,
		command,
		input,
		128<<10,
		"required native command is missing on the target; Python3 and normal user-service tools are required",
		"fixed remote product command failed; no untrusted stderr was exposed",
	)
}

func (c *onboardingSSH) runBootstrapHelper(
	ctx context.Context,
	platform hostbootstrap.Platform,
	input io.Reader,
) ([]byte, error) {
	command, err := fixedBootstrapHelperCommand(platform)
	if err != nil {
		return nil, err
	}
	return c.runBounded(
		ctx,
		command,
		input,
		hostbootstrap.MaxHelperFrameBytes,
		"the fixed installed nvpair-host-helper command is unavailable",
		"fixed remote helper command failed; no untrusted stderr was exposed",
	)
}

func fixedBootstrapHelperCommand(
	platform hostbootstrap.Platform,
) (string, error) {
	switch platform {
	case hostbootstrap.PlatformWindows:
		return `"C:\Program Files\NVIDIA Corporation\PAIR\nvpair-host-helper.exe" request`, nil
	case hostbootstrap.PlatformDarwin:
		return "/Library/PrivilegedHelperTools/nvpair-host-helper request", nil
	case hostbootstrap.PlatformLinux:
		return "/usr/libexec/nvpair-host-helper request", nil
	default:
		return "", errors.New("bootstrap helper target platform is unsupported")
	}
}

func (c *onboardingSSH) runBounded(
	ctx context.Context,
	command string,
	input io.Reader,
	stdoutLimit int,
	missingCommand string,
	failedCommand string,
) ([]byte, error) {
	if c.testRun != nil {
		return c.testRun(ctx, command, input)
	}
	// Session-open itself can block. Cancellation closes only this operation's
	// SSH connection, including a channel open that has not returned yet.
	closePending := context.AfterFunc(ctx, func() { _ = c.client.Close() })
	defer closePending()
	session, err := c.client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("%w: SSH product session unavailable", errBeforeRemoteCommand)
	}
	defer session.Close()
	stop := context.AfterFunc(ctx, func() { _ = session.Signal(ssh.SIGTERM); _ = session.Close() })
	defer stop()
	stdout := &onboardingBoundedOutput{max: stdoutLimit}
	stderr := &onboardingBoundedOutput{max: 16 << 10}
	session.Stdout, session.Stderr, session.Stdin = stdout, stderr, input
	if err := session.Run(command); err != nil {
		var exit *ssh.ExitError
		if errors.As(err, &exit) && exit.ExitStatus() == 127 {
			return nil, errors.New(missingCommand)
		}
		return nil, errors.New(failedCommand)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return append([]byte(nil), stdout.Bytes()...), nil
}

func verifyOnboardingFingerprint(expected string, key ssh.PublicKey) error {
	if expected == "" || ssh.FingerprintSHA256(key) != expected {
		return errors.New("SSH host identity changed")
	}
	return nil
}
func onboardingQuote(s string) string       { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func onboardingPython(script string) string { return "python3 -c " + onboardingQuote(script) }

type onboardingPlatformInfo struct {
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	Home         string `json:"home"`
	UID          int    `json:"uid"`
	FreeBytes    int64  `json:"freeBytes"`
	ExistingPAIR bool   `json:"existingPair"`
	UserRuntime  bool   `json:"userRuntime"`
	Linger       bool   `json:"linger"`
}

const onboardingInspectScript = `import json,os,platform,pwd,shutil,socket,subprocess
u=pwd.getpwuid(os.geteuid()); home=u.pw_dir
root=os.path.join(home,'.config','Nvidia Corporation','Personal AI Router')
runtime='/run/user/'+str(os.geteuid())
r=subprocess.run(['loginctl','show-user',str(os.geteuid()),'-p','Linger','--value'],capture_output=True,text=True,timeout=5)
print(json.dumps(dict(hostname=socket.gethostname(),os=platform.system(),arch=platform.machine(),home=home,uid=os.geteuid(),freeBytes=shutil.disk_usage(home).free,existingPair=os.path.exists(os.path.join(root,'node-id.json')) or os.path.exists(os.path.join(root,'cluster','identity.json')) or shutil.which('nvpair-ui-broker') is not None,userRuntime=os.path.isdir(runtime) and os.path.exists(os.path.join(runtime,'bus')),linger=r.returncode==0 and r.stdout.strip()=='yes')))
`

func inspectOnboardingPlatform(ctx context.Context, c *onboardingSSH) (onboardingPlatformInfo, error) {
	info, err := readOnboardingPlatform(ctx, c)
	if err != nil {
		return info, err
	}
	return info, validateFreshOnboardingPlatform(info)
}

func validateFreshOnboardingPlatform(info onboardingPlatformInfo) error {
	if info.ExistingPAIR {
		return errors.New("an existing PAIR identity or installation must not be replaced by onboarding")
	}
	if !info.UserRuntime {
		return errors.New("headless PAIR requires an existing per-account runtime and user bus; sign in on the target first")
	}
	if info.FreeBytes < 512<<20 {
		return fmt.Errorf("insufficient user storage for the PAIR bundle")
	}
	return nil
}
func readOnboardingPlatform(ctx context.Context, c *onboardingSSH) (onboardingPlatformInfo, error) {
	var info onboardingPlatformInfo
	body, err := c.run(ctx, onboardingPython(onboardingInspectScript), nil)
	if err != nil {
		return info, err
	}
	if onboardingDecode(body, &info) != nil || info.UID <= 0 || info.Hostname == "" || !strings.HasPrefix(info.Home, "/") || info.Home == "/" || len(info.Home) > 1024 {
		return info, errors.New("remote native platform response is invalid")
	}
	if platform, _, err := onboardingPlatform(info.OS, info.Arch); err != nil {
		return info, err
	} else if platform != "linux" {
		return info, errors.New("SSH enrollment runs after the signed target bootstrap")
	}
	return info, nil
}
