// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"nvpair-shared/hostbootstrap"
)

const bootstrapSSHConfigLimit = 1 << 20

type bootstrapControllerKeys struct {
	SchemaVersion int                               `json:"schemaVersion"`
	Keys          []hostbootstrap.PublicKeyIdentity `json:"keys"`
}

type bootstrapControllerKeySource struct {
	homeDir   func() (string, error)
	agentKeys func(context.Context) ([]*agent.Key, error)
	readFile  func(string) ([]byte, error)
}

func defaultBootstrapControllerKeySource() bootstrapControllerKeySource {
	return bootstrapControllerKeySource{
		homeDir:   os.UserHomeDir,
		agentKeys: listBootstrapAgentKeys,
		readFile:  os.ReadFile,
	}
}

func (s *onboardingService) bootstrapCatalogMetadata() (bootstrapCatalog, error) {
	if s.loadBootstrapCatalog == nil {
		return bootstrapCatalog{}, errors.New("bootstrap catalog loader is unavailable")
	}
	return s.loadBootstrapCatalog()
}

func (s *onboardingService) bootstrapControllerKeyMetadata(
	ctx context.Context,
) (bootstrapControllerKeys, error) {
	if s.loadBootstrapControllerKeys == nil {
		return bootstrapControllerKeys{},
			errors.New("bootstrap controller-key loader is unavailable")
	}
	keys, err := s.loadBootstrapControllerKeys(ctx)
	if err != nil {
		return bootstrapControllerKeys{}, err
	}
	return bootstrapControllerKeys{
		SchemaVersion: hostbootstrap.SchemaVersion,
		Keys:          keys,
	}, nil
}

func readBootstrapControllerKeys(
	ctx context.Context,
	source bootstrapControllerKeySource,
) ([]hostbootstrap.PublicKeyIdentity, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	identities := map[string]hostbootstrap.PublicKeyIdentity{}
	if source.agentKeys != nil {
		if keys, err := source.agentKeys(ctx); err == nil {
			for _, key := range keys {
				if key == nil {
					continue
				}
				public, err := ssh.ParsePublicKey(key.Blob)
				if err != nil || (key.Format != "" && key.Format != public.Type()) {
					continue
				}
				if identity, ok := bootstrapPublicKeyIdentity(public); ok {
					identities[identity.FingerprintSHA256] = identity
				}
			}
		}
	}

	home := ""
	if source.homeDir != nil {
		home, _ = source.homeDir()
	}
	if home != "" && source.readFile != nil {
		for _, path := range bootstrapPublicKeyPaths(home, source.readFile) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			raw, err := source.readFile(path)
			if err != nil || len(raw) == 0 || len(raw) > 64<<10 {
				continue
			}
			scanner := bufio.NewScanner(bytes.NewReader(raw))
			for scanner.Scan() {
				public, _, _, _, err := ssh.ParseAuthorizedKey(scanner.Bytes())
				if err != nil {
					continue
				}
				if identity, ok := bootstrapPublicKeyIdentity(public); ok {
					identities[identity.FingerprintSHA256] = identity
				}
			}
		}
	}

	keys := make([]hostbootstrap.PublicKeyIdentity, 0, len(identities))
	for _, identity := range identities {
		keys = append(keys, identity)
	}
	sort.Slice(keys, func(left, right int) bool {
		return keys[left].FingerprintSHA256 < keys[right].FingerprintSHA256
	})
	return keys, nil
}

func bootstrapPublicKeyIdentity(
	public ssh.PublicKey,
) (hostbootstrap.PublicKeyIdentity, bool) {
	var algorithm hostbootstrap.PublicKeyAlgorithm
	switch public.Type() {
	case string(hostbootstrap.PublicKeyAlgorithmED25519):
		algorithm = hostbootstrap.PublicKeyAlgorithmED25519
	case string(hostbootstrap.PublicKeyAlgorithmRSA):
		algorithm = hostbootstrap.PublicKeyAlgorithmRSA
	case string(hostbootstrap.PublicKeyAlgorithmECDSA):
		algorithm = hostbootstrap.PublicKeyAlgorithmECDSA
	default:
		return hostbootstrap.PublicKeyIdentity{}, false
	}
	material := public.Marshal()
	sum := sha256.Sum256(material)
	return hostbootstrap.PublicKeyIdentity{
		Algorithm:         algorithm,
		Material:          base64.StdEncoding.EncodeToString(material),
		FingerprintSHA256: hex.EncodeToString(sum[:]),
	}, true
}

func bootstrapPublicKeyPaths(
	home string,
	readFile func(string) ([]byte, error),
) []string {
	sshDir := filepath.Join(home, ".ssh")
	paths := []string{
		filepath.Join(sshDir, "id_ed25519.pub"),
		filepath.Join(sshDir, "id_ecdsa.pub"),
		filepath.Join(sshDir, "id_rsa.pub"),
	}
	config, err := readFile(filepath.Join(sshDir, "config"))
	if err == nil && len(config) <= bootstrapSSHConfigLimit {
		paths = append(paths, configuredBootstrapPublicKeyPaths(config, home)...)
	}
	seen := map[string]bool{}
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		clean := filepath.Clean(path)
		if !seen[clean] {
			seen[clean] = true
			result = append(result, clean)
		}
	}
	return result
}

func configuredBootstrapPublicKeyPaths(raw []byte, home string) []string {
	var result []string
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			key = fields[0]
			value = fields[1]
		}
		if !strings.EqualFold(strings.TrimSpace(key), "IdentityFile") {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch {
		case value == "~":
			value = home
		case strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~\`):
			value = filepath.Join(home, value[2:])
		case strings.Contains(value, "%"):
			continue
		case !filepath.IsAbs(value):
			value = filepath.Join(home, ".ssh", value)
		}
		if !strings.HasSuffix(strings.ToLower(value), ".pub") {
			value += ".pub"
		}
		result = append(result, value)
	}
	return result
}
