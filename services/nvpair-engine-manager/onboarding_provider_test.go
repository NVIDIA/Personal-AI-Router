// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func onboardingProviderRequest(t *testing.T, candidateIDs []string) onboardingBatchAccess {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return onboardingBatchAccess{
		CandidateIDs:        append([]string(nil), candidateIDs...),
		Username:            "approved-user",
		Auth:                "existing-key",
		PrivateKey:          key,
		CredentialProvider:  "controller-ssh-config",
		CredentialPurpose:   "enrolled-peer-upgrade",
		PublicKeySHA256:     ssh.FingerprintSHA256(signer.PublicKey()),
		CredentialExpiresAt: time.Now().Add(5 * time.Minute).UnixMilli(),
		StartupLifetime:     "persistent",
	}
}

func TestOnboardingProviderKeyBecomesOnlyBoundSigner(t *testing.T) {
	f := newOnboardingControllerFixture(t, 2)
	request := onboardingProviderRequest(t, f.ids)
	raw := request.PrivateKey
	if _, err := f.s.bindAccess(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	for _, value := range raw {
		if value != 0 {
			t.Fatal("mutable private-key holder was not cleared")
		}
	}
	wantCandidates := append([]string(nil), f.ids...)
	sort.Strings(wantCandidates)
	for _, id := range f.ids {
		target := f.s.targets[id]
		if target.access.signer == nil || target.access.keyPath != "" || target.access.password != "" || target.access.provider.account != request.Username || target.access.provider.candidateID != id || target.access.provider.candidateSet != strings.Join(wantCandidates, ",") || target.access.provider.accessGeneration != target.accessGeneration || target.access.provider.lifetime != "persistent" {
			t.Fatal("provider signer lost its public access binding")
		}
		if target.access.validProviderBinding(target.candidate, time.Now()) || !target.access.forPurpose("enrolled-peer-upgrade").validProviderBinding(target.candidate, time.Now()) {
			t.Fatal("provider signer was not isolated to its enrolled-peer-upgrade purpose")
		}
	}
}

func TestOnboardingProviderKeyRejectsInvalidBindingBeforeHostObservation(t *testing.T) {
	for _, kind := range []string{"path-conflict", "password-conflict", "elevation-conflict", "malformed", "oversize", "stale", "fingerprint", "provider", "purpose", "session"} {
		t.Run(kind, func(t *testing.T) {
			f := newOnboardingControllerFixture(t, 1)
			calls := 0
			f.s.observeKey = func(context.Context, onboardingCandidate, string) (string, bool, bool, error) {
				calls++
				return "", false, false, nil
			}
			request := onboardingProviderRequest(t, f.ids)
			switch kind {
			case "path-conflict":
				request.KeyPath = "/conflict"
			case "password-conflict":
				request.Password = "unused-secret"
			case "elevation-conflict":
				request.ElevationPassword = "unused-admin-secret"
			case "malformed":
				request.PrivateKey = []byte("not a key")
			case "oversize":
				request.PrivateKey = make([]byte, onboardingVolatileKeyLimit+1)
			case "stale":
				request.CredentialExpiresAt = time.Now().Add(-time.Second).UnixMilli()
			case "fingerprint":
				request.PublicKeySHA256 = "SHA256:different"
			case "provider":
				request.CredentialProvider = "other-provider"
			case "purpose":
				request.CredentialPurpose = "other"
			case "session":
				request.StartupLifetime = "session"
			}
			raw := request.PrivateKey
			if _, err := f.s.bindAccess(context.Background(), request); err == nil || calls != 0 {
				t.Fatal("invalid provider binding reached host observation")
			}
			for _, value := range raw {
				if value != 0 {
					t.Fatal("rejected private-key holder was not cleared")
				}
			}
		})
	}
}

func TestOnboardingProviderRecoveryRejectsNonUpgradePlan(t *testing.T) {
	f, run, _, calls := onboardingRecoveryAccessFixture(t)
	request := onboardingProviderRequest(t, f.ids)
	request.OperationID = run.Public.OperationID
	request.StartupLifetime = ""
	if _, err := f.s.bindAccess(context.Background(), request); err == nil || *calls != 0 {
		t.Fatal("provider key entered a retained non-upgrade operation")
	}
}
