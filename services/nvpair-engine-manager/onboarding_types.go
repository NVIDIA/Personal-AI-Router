// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"

	"nvpair-shared/hostbootstrap"
)

// Product onboarding is not an inference engine. Only opaque selectors cross
// its ordinary API; access material is held by this backend, never a journal.
type onboardingCandidate struct {
	CandidateID           string `json:"candidateId"`
	Label                 string `json:"label"`
	Address               string `json:"address"`
	Port                  int    `json:"port"`
	AccessID              string `json:"accessId"`
	AccessLabel           string `json:"accessLabel"`
	AccessAvailable       bool   `json:"accessAvailable"`
	HostKeySHA256         string `json:"hostKeySha256,omitempty"`
	HostKeyTrusted        bool   `json:"hostKeyTrusted"`
	BootstrapSource       string `json:"bootstrapSource"`
	BootstrapState        string `json:"bootstrapState"`
	BootstrapPlatform     string `json:"bootstrapPlatform,omitempty"`
	BootstrapArchitecture string `json:"bootstrapArchitecture,omitempty"`
	Reason                string `json:"reason,omitempty"`
}
type onboardingArtifact struct {
	SourceFingerprint string `json:"sourceFingerprint,omitempty"`
	ArtifactID        string `json:"artifactId"`
	Version           string `json:"version"`
	Platform          string `json:"platform"`
	Arch              string `json:"arch"`
	SHA256            string `json:"sha256"`
	Provenance        string `json:"provenance"`
}
type onboardingReviewTarget struct {
	Action               string                         `json:"action,omitempty"`
	ExistingInstallation *onboardingInstallationSummary `json:"existingInstallation,omitempty"`
	StartupLifetime      string                         `json:"startupLifetime"`
	NeedsLinger          bool                           `json:"needsLinger"`
	CandidateID          string                         `json:"candidateId"`
	Label                string                         `json:"label"`
	Address              string                         `json:"address"`
	Port                 int                            `json:"port"`
	AccessID             string                         `json:"accessId"`
	AccessLabel          string                         `json:"accessLabel"`
	Hostname             string                         `json:"hostname,omitempty"`
	Platform             string                         `json:"platform,omitempty"`
	Arch                 string                         `json:"arch,omitempty"`
	HostKeySHA256        string                         `json:"hostKeySha256,omitempty"`
	Artifact             *onboardingArtifact            `json:"artifact,omitempty"`
	Status               string                         `json:"status"`
	Reason               string                         `json:"reason,omitempty"`
}
type onboardingInstallationSummary struct {
	NodeID                    string                     `json:"nodeId"`
	ClusterID                 string                     `json:"clusterId"`
	Version                   string                     `json:"version"`
	SourceFingerprint         string                     `json:"sourceFingerprint"`
	Unit                      string                     `json:"unit"`
	Bundle                    string                     `json:"bundle"`
	Retention                 *onboardingRetentionReview `json:"retention,omitempty"`
	LegacyPackageDisposition  string                     `json:"legacyPackageDisposition,omitempty"`
	LegacyPackageStatus       string                     `json:"legacyPackageStatus,omitempty"`
	LegacyPackage             string                     `json:"legacyPackage,omitempty"`
	LegacyPackageVersion      string                     `json:"legacyPackageVersion,omitempty"`
	LegacyPackageArchitecture string                     `json:"legacyPackageArchitecture,omitempty"`
	LegacyAppSHA256           string                     `json:"legacyAppSha256,omitempty"`
	LegacyLauncherSHA256      string                     `json:"legacyLauncherSha256,omitempty"`
	LegacyRollbackSHA256      string                     `json:"legacyRollbackSha256,omitempty"`
	LegacyRollbackBytes       int64                      `json:"legacyRollbackBytes,omitempty"`
}
type onboardingReview struct {
	ReviewID         string                   `json:"reviewId"`
	ExpiresAt        int64                    `json:"expiresAt"`
	ControllerNodeID string                   `json:"controllerNodeId"`
	TargetClusterID  string                   `json:"targetClusterId"`
	Targets          []onboardingReviewTarget `json:"targets"`
	CanApprove       bool                     `json:"canApprove"`
}
type onboardingTargetState struct {
	CandidateID      string `json:"candidateId"`
	Stage            string `json:"stage"`
	Message          string `json:"message,omitempty"`
	NodeID           string `json:"nodeId,omitempty"`
	CanRetry         bool   `json:"canRetry"`
	CanCancel        bool   `json:"canCancel"`
	CleanupConfirmed bool   `json:"cleanupConfirmed"`
}
type onboardingOperation struct {
	OperationID     string                  `json:"operationId"`
	ReviewID        string                  `json:"reviewId"`
	TargetClusterID string                  `json:"targetClusterId"`
	Revision        uint64                  `json:"revision"`
	State           string                  `json:"state"`
	Targets         []onboardingTargetState `json:"targets"`
	StartedAt       int64                   `json:"startedAt"`
	FinishedAt      int64                   `json:"finishedAt,omitempty"`
}
type onboardingInspectRequest struct {
	CandidateIDs     []string `json:"candidateIds"`
	ArtifactID       string   `json:"artifactId,omitempty"`
	AcceptedHostKeys []struct {
		CandidateID string `json:"candidateId"`
		SHA256      string `json:"sha256"`
	} `json:"acceptedHostKeys,omitempty"`
}
type onboardingOperationRequest struct {
	OperationID string `json:"operationId,omitempty"`
	ReviewID    string `json:"reviewId,omitempty"`
	CandidateID string `json:"candidateId,omitempty"`
}
type onboardingAddTargetRequest struct {
	StartupLifetime   string `json:"startupLifetime,omitempty"`
	ElevationPassword string `json:"elevationPassword,omitempty"`
	Passphrase        string `json:"passphrase,omitempty"`
	Address           string `json:"address"`
	Port              int    `json:"port"`
	Username          string `json:"username"`
	Auth              string `json:"auth"`
	KeyPath           string `json:"keyPath,omitempty"`
	Password          string `json:"password,omitempty"` // Volatile request only. Never serialize this type.
	Label             string `json:"label,omitempty"`
}

var onboardingID = regexp.MustCompile(`^[a-f0-9]{32}$`)
var onboardingToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,95}$`)
var onboardingSHA = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

func onboardingDecode(raw json.RawMessage, out any) error {
	if len(raw) > 64<<10 {
		return errors.New("onboarding request exceeds limit")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(out) != nil || dec.Decode(new(any)) != io.EOF {
		return errors.New("invalid onboarding request")
	}
	return nil // Never echo parsing input: a transient access frame can contain a secret.
}
func onboardingPlatform(osName, arch string) (string, string, error) {
	var platform hostbootstrap.Platform
	switch strings.TrimSpace(osName) {
	case "Windows":
		platform = hostbootstrap.PlatformWindows
	case "Darwin":
		platform = hostbootstrap.PlatformDarwin
	case "Linux":
		platform = hostbootstrap.PlatformLinux
	default:
		return "", "", errors.New("remote operating system is not supported")
	}
	var architecture hostbootstrap.Architecture
	switch strings.TrimSpace(arch) {
	case "x86_64", "amd64":
		architecture = hostbootstrap.ArchitectureAMD64
	case "aarch64", "arm64":
		architecture = hostbootstrap.ArchitectureARM64
	default:
		return "", "", errors.New("remote architecture is not supported")
	}
	target := hostbootstrap.Target{
		Platform:     platform,
		Architecture: architecture,
	}
	supported := false
	for _, candidate := range hostbootstrap.SupportedTargets() {
		if candidate == target {
			supported = true
			break
		}
	}
	if !supported {
		return "", "", errors.New("remote target is not supported")
	}
	return string(platform), string(architecture), nil
}
