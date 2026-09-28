// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ed25519"
	"errors"
	"net"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestOnboardingPublicHostKeyRequiresCurrentTrust(t *testing.T) {
	key, err := ssh.NewPublicKey(ed25519.PublicKey(make([]byte, ed25519.PublicKeySize)))
	if err != nil {
		t.Fatal(err)
	}
	expected := ssh.FingerprintSHA256(key)
	got, err := onboardingVerifiedHostKey(expected, nil, "192.0.2.1:22", nil, key)
	if err != nil {
		t.Fatal(err)
	}
	client := &onboardingSSH{hostPublicKey: got}
	if checked, err := client.verifiedHostPublicKey(expected); err != nil || checked != got {
		t.Fatalf("verified public key lost: %v", err)
	}
	deny := func(string, net.Addr, ssh.PublicKey) error { return errors.New("changed trust") }
	for _, check := range []ssh.HostKeyCallback{nil, deny} {
		fingerprint := "SHA256:wrong"
		if check != nil {
			fingerprint = expected
		}
		if rejected, err := onboardingVerifiedHostKey(fingerprint, check, "192.0.2.1:22", nil, key); err == nil || rejected != (onboardingHostPublicKey{}) {
			t.Fatal("unaccepted public bytes escaped trust callback")
		}
	}
	if _, err := client.verifiedHostPublicKey("SHA256:changed"); err == nil {
		t.Fatal("stale public key was reused")
	}
	client.hostPublicKey.Blob += "invalid"
	if _, err := client.verifiedHostPublicKey(expected); err == nil {
		t.Fatal("changed public bytes were accepted")
	}
	if _, err := (&onboardingSSH{}).verifiedHostPublicKey(expected); err == nil {
		t.Fatal("missing authenticated host key was accepted")
	}
}
