// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// This file is the read-only projection of the accepted onboarding journal
// validator. It reuses the accepted package-generation inventory so the
// history RPC cannot drift from the onboarding-capable source.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	historyToken       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,95}$`)
	historySHA         = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
	historyDebianToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+:~_-]{0,127}$`)
)

const historyUpgradeUnit = "nvidia-pair-headless.service"

type historyCandidate struct {
	CandidateID     string `json:"candidateId"`
	Label           string `json:"label"`
	Address         string `json:"address"`
	Port            int    `json:"port"`
	AccessID        string `json:"accessId"`
	AccessLabel     string `json:"accessLabel"`
	AccessAvailable bool   `json:"accessAvailable"`
	HostKeySHA256   string `json:"hostKeySha256,omitempty"`
	HostKeyTrusted  bool   `json:"hostKeyTrusted"`
	Reason          string `json:"reason,omitempty"`
}

type historyArtifact struct {
	SourceFingerprint string `json:"sourceFingerprint,omitempty"`
	ArtifactID        string `json:"artifactId"`
	Version           string `json:"version"`
	Platform          string `json:"platform"`
	Arch              string `json:"arch"`
	SHA256            string `json:"sha256"`
	Provenance        string `json:"provenance"`
}

type historyArtifactSource struct {
	historyArtifact
	File string `json:"file,omitempty"`
	URL  string `json:"url,omitempty"`
}

type historyRetentionReview struct {
	Owner                string   `json:"owner"`
	ActiveDigest         string   `json:"activeDigest"`
	RollbackDigest       string   `json:"rollbackDigest"`
	HeldPruneDigests     []string `json:"heldPruneDigests"`
	Operation            string   `json:"operation"`
	Generation           uint64   `json:"generation"`
	RegistrySHA256       string   `json:"registrySha256"`
	NextHeldPruneDigests []string `json:"nextHeldPruneDigests"`
	NextGeneration       uint64   `json:"nextGeneration"`
}

type historyInstallationSummary struct {
	NodeID                    string                  `json:"nodeId"`
	ClusterID                 string                  `json:"clusterId"`
	Version                   string                  `json:"version"`
	SourceFingerprint         string                  `json:"sourceFingerprint"`
	Unit                      string                  `json:"unit"`
	Bundle                    string                  `json:"bundle"`
	Retention                 *historyRetentionReview `json:"retention,omitempty"`
	LegacyPackageDisposition  string                  `json:"legacyPackageDisposition,omitempty"`
	LegacyPackageStatus       string                  `json:"legacyPackageStatus,omitempty"`
	LegacyPackage             string                  `json:"legacyPackage,omitempty"`
	LegacyPackageVersion      string                  `json:"legacyPackageVersion,omitempty"`
	LegacyPackageArchitecture string                  `json:"legacyPackageArchitecture,omitempty"`
	LegacyAppSHA256           string                  `json:"legacyAppSha256,omitempty"`
	LegacyLauncherSHA256      string                  `json:"legacyLauncherSha256,omitempty"`
	LegacyRollbackSHA256      string                  `json:"legacyRollbackSha256,omitempty"`
	LegacyRollbackBytes       int64                   `json:"legacyRollbackBytes,omitempty"`
}

type historyReviewTarget struct {
	Action               string                      `json:"action,omitempty"`
	ExistingInstallation *historyInstallationSummary `json:"existingInstallation,omitempty"`
	StartupLifetime      string                      `json:"startupLifetime"`
	NeedsLinger          bool                        `json:"needsLinger"`
	CandidateID          string                      `json:"candidateId"`
	Label                string                      `json:"label"`
	Address              string                      `json:"address"`
	Port                 int                         `json:"port"`
	AccessID             string                      `json:"accessId"`
	AccessLabel          string                      `json:"accessLabel"`
	Hostname             string                      `json:"hostname,omitempty"`
	Platform             string                      `json:"platform,omitempty"`
	Arch                 string                      `json:"arch,omitempty"`
	HostKeySHA256        string                      `json:"hostKeySha256,omitempty"`
	Artifact             *historyArtifact            `json:"artifact,omitempty"`
	Status               string                      `json:"status"`
	Reason               string                      `json:"reason,omitempty"`
}

type historyTargetState struct {
	CandidateID      string `json:"candidateId"`
	Stage            string `json:"stage"`
	Message          string `json:"message,omitempty"`
	NodeID           string `json:"nodeId,omitempty"`
	CanRetry         bool   `json:"canRetry"`
	CanCancel        bool   `json:"canCancel"`
	CleanupConfirmed bool   `json:"cleanupConfirmed"`
}

type historyOperation struct {
	OperationID     string               `json:"operationId"`
	ReviewID        string               `json:"reviewId"`
	TargetClusterID string               `json:"targetClusterId"`
	Revision        uint64               `json:"revision"`
	State           string               `json:"state"`
	Targets         []historyTargetState `json:"targets"`
	StartedAt       int64                `json:"startedAt"`
	FinishedAt      int64                `json:"finishedAt,omitempty"`
}

type historyPlatformInfo struct {
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

type historyInstalledComponent struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type historyLegacyFileIdentity struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	UID    int    `json:"uid"`
	GID    int    `json:"gid"`
	Mode   uint32 `json:"mode"`
	Links  uint64 `json:"links"`
}

type historyLegacyPackageProcess struct {
	PID        int    `json:"pid"`
	UID        int    `json:"uid"`
	StartTicks string `json:"startTicks"`
	Executable string `json:"executable"`
	Device     uint64 `json:"device"`
	Inode      uint64 `json:"inode"`
}

type historyLegacyPackageReceipt struct {
	Package             string                        `json:"package"`
	Status              string                        `json:"status"`
	Version             string                        `json:"version"`
	Architecture        string                        `json:"architecture"`
	PackageRecordSHA256 string                        `json:"packageRecordSha256"`
	PackageFilesSHA256  string                        `json:"packageFilesSha256"`
	PackageInfoSHA256   string                        `json:"packageInfoSha256"`
	App                 historyLegacyFileIdentity     `json:"app"`
	Launcher            historyLegacyFileIdentity     `json:"launcher"`
	DesktopEntry        historyLegacyFileIdentity     `json:"desktopEntry"`
	Processes           []historyLegacyPackageProcess `json:"processes"`
	Rollback            historyLegacyFileIdentity     `json:"rollback"`
}

type historyExistingInstallation struct {
	NodeID              string                       `json:"nodeId"`
	ClusterID           string                       `json:"clusterId"`
	CertFingerprint     string                       `json:"certFingerprint"`
	Version             string                       `json:"version"`
	SourceFingerprint   string                       `json:"sourceFingerprint"`
	UID                 int                          `json:"uid"`
	Home                string                       `json:"home"`
	ConfigHome          string                       `json:"configHome"`
	StartupLifetime     string                       `json:"startupLifetime"`
	Unit                string                       `json:"unit"`
	UnitPath            string                       `json:"unitPath"`
	UnitSHA256          string                       `json:"unitSha256"`
	UnitBytes           string                       `json:"unitBytes"`
	Bundle              string                       `json:"bundle"`
	Executable          string                       `json:"executable"`
	Broker              string                       `json:"broker"`
	ManifestSHA256      string                       `json:"manifestSha256"`
	ProcessID           int                          `json:"processId"`
	ProcessStartTicks   string                       `json:"processStartTicks"`
	ExecutableSHA256    string                       `json:"executableSha256"`
	IdentitySHA256      string                       `json:"identitySha256"`
	NodeIdentitySHA256  string                       `json:"nodeIdentitySha256"`
	LauncherPath        string                       `json:"launcherPath"`
	LauncherPresent     bool                         `json:"launcherPresent"`
	LauncherSHA256      string                       `json:"launcherSha256"`
	LauncherBody        string                       `json:"launcherBody"`
	Components          []historyInstalledComponent  `json:"components"`
	Retention           *historyRetentionReview      `json:"retention,omitempty"`
	LegacyPackageStatus string                       `json:"legacyPackageStatus,omitempty"`
	LegacyPackage       *historyLegacyPackageReceipt `json:"legacyPackage,omitempty"`
}

type historyInstallReceipt struct {
	StartupLifetime  string `json:"startupLifetime"`
	ManifestSHA256   string `json:"manifestSha256"`
	NodeID           string `json:"nodeId,omitempty"`
	Recoverable      bool   `json:"recoverable"`
	StagePath        string `json:"stagePath"`
	BundlePath       string `json:"bundlePath"`
	TUIPath          string `json:"tuiPath"`
	ArtifactSHA256   string `json:"artifactSha256"`
	Installed        bool   `json:"installed"`
	ServiceInstalled bool   `json:"serviceInstalled"`
	ServiceStarted   bool   `json:"serviceStarted"`
	CleanupConfirmed bool   `json:"cleanupConfirmed"`
}

type historySettledInvite struct {
	RequestKey string `json:"requestKey"`
	InviteID   string `json:"inviteId"`
	State      string `json:"state"`
}

type historyPlan struct {
	Candidate               historyCandidate             `json:"candidate"`
	Review                  historyReviewTarget          `json:"review"`
	Info                    historyPlatformInfo          `json:"info"`
	Artifact                historyArtifactSource        `json:"artifact"`
	PackageFile             string                       `json:"packageFile"`
	ArchiveRoot             string                       `json:"archiveRoot"`
	ArchiveBytes            int64                        `json:"archiveBytes"`
	Username                string                       `json:"username"`
	AuthKind                string                       `json:"authKind"`
	Receipt                 historyInstallReceipt        `json:"receipt"`
	InviteID                string                       `json:"inviteId,omitempty"`
	InviteRequestKey        string                       `json:"inviteRequestKey,omitempty"`
	InviteExpectedClusterID string                       `json:"inviteExpectedClusterId"`
	InviteNodeID            string                       `json:"inviteNodeId,omitempty"`
	InviteHistory           []historySettledInvite       `json:"inviteHistory,omitempty"`
	LingerChanged           bool                         `json:"lingerChanged"`
	LingerRequested         bool                         `json:"lingerRequested"`
	ExistingInstallation    *historyExistingInstallation `json:"existingInstallation,omitempty"`
	UpgradePhase            string                       `json:"upgradePhase,omitempty"`
}

func historyIdentifier(value string, max int) bool {
	if value == "" || len(value) > max || !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return false
	}
	for _, c := range value {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}

func historyPlatform(osName, arch string) (string, string, error) {
	if strings.TrimSpace(osName) != "Linux" {
		return "", "", errors.New("remote onboarding currently supports Linux only")
	}
	switch strings.TrimSpace(arch) {
	case "aarch64", "arm64":
		return "linux", "arm64", nil
	case "x86_64", "amd64":
		return "linux", "amd64", nil
	}
	return "linux", "", errors.New("remote architecture is not supported")
}

func validHistoryInvitation(plan historyPlan) bool {
	if plan.InviteRequestKey != "" && (!onboardingHistoryID.MatchString(plan.InviteRequestKey) || plan.InviteNodeID == "") {
		return false
	}
	if len(plan.InviteHistory) > 8 {
		return false
	}
	seen := map[string]bool{}
	for _, old := range plan.InviteHistory {
		terminal := old.State == "canceled" || old.State == "declined" || old.State == "expired" || old.State == "failed" || old.State == "rejected"
		if !onboardingHistoryID.MatchString(old.RequestKey) || seen[old.RequestKey] || old.RequestKey == plan.InviteRequestKey || old.InviteID == "" || !terminal {
			return false
		}
		seen[old.RequestKey] = true
	}
	return true
}

func historyLegacySummaryFields(d *historyLegacyPackageReceipt) (string, string, string, string, string, string, string, int64) {
	if d == nil {
		return "", "", "", "", "", "", "", 0
	}
	return d.Package, d.Status, d.Version, d.Architecture, d.App.SHA256, d.Launcher.SHA256, d.Rollback.SHA256, d.Rollback.Bytes
}

func (d historyExistingInstallation) summary() historyInstallationSummary {
	name, status, version, architecture, app, launcher, rollback, rollbackBytes := historyLegacySummaryFields(d.LegacyPackage)
	var retention *historyRetentionReview
	if d.Retention != nil {
		copy := *d.Retention
		copy.HeldPruneDigests = append([]string(nil), d.Retention.HeldPruneDigests...)
		copy.NextHeldPruneDigests = append([]string(nil), d.Retention.NextHeldPruneDigests...)
		retention = &copy
	}
	return historyInstallationSummary{NodeID: d.NodeID, ClusterID: d.ClusterID, Version: d.Version, SourceFingerprint: d.SourceFingerprint, Unit: d.Unit, Bundle: d.Bundle, Retention: retention, LegacyPackageDisposition: d.LegacyPackageStatus, LegacyPackageStatus: status, LegacyPackage: name, LegacyPackageVersion: version, LegacyPackageArchitecture: architecture, LegacyAppSHA256: app, LegacyLauncherSHA256: launcher, LegacyRollbackSHA256: rollback, LegacyRollbackBytes: rollbackBytes}
}

func historyManagedBundleDigest(d historyExistingInstallation) (string, bool) {
	bundle := path.Dir(d.Bundle)
	digest := path.Base(bundle)
	expected := path.Join(d.Home, ".local", "share", "Nvidia Corporation", "Personal AI Router", "bundles")
	return digest, path.Base(d.Bundle) == "bin" && path.Dir(bundle) == expected && historySHA.MatchString(digest)
}

func validHistoryHeldDigests(active, rollback string, held []string) bool {
	if !historySHA.MatchString(active) || rollback != "" && (!historySHA.MatchString(rollback) || rollback == active) || len(held) > 4 {
		return false
	}
	seen := map[string]bool{active: true}
	if rollback != "" {
		seen[rollback] = true
	}
	for _, digest := range held {
		if !historySHA.MatchString(digest) || seen[digest] {
			return false
		}
		seen[digest] = true
	}
	return true
}

func equalHistoryDigests(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func validateHistoryRetention(existing historyExistingInstallation) error {
	digest, managed := historyManagedBundleDigest(existing)
	if !managed {
		if existing.Retention != nil {
			return errors.New("unmanaged predecessor has a bundle retention claim")
		}
		return nil
	}
	r := existing.Retention
	if r == nil || r.ActiveDigest != digest || r.HeldPruneDigests == nil || r.NextHeldPruneDigests == nil || !validHistoryHeldDigests(r.ActiveDigest, r.RollbackDigest, r.HeldPruneDigests) || r.NextGeneration != r.Generation+1 || r.NextGeneration == 0 {
		return errors.New("managed predecessor bundle retention is incomplete")
	}
	if r.Owner == "absent" {
		if r.Generation != 0 || r.Operation != "" || r.RegistrySHA256 != "" || r.RollbackDigest != "" || len(r.HeldPruneDigests) != 0 {
			return errors.New("absent bundle retention registry has conflicting history")
		}
	} else if r.Owner != "nvidia-pair-bundle-retention-v1" && r.Owner != "nvidia-pair-bundle-retention-v2" || r.Generation == 0 || !onboardingHistoryID.MatchString(r.Operation) || !historySHA.MatchString(r.RegistrySHA256) {
		return errors.New("bundle retention registry identity is invalid")
	}
	// History must project the same bounded transition the effectful reviewer
	// admitted. Once the list is full, the oldest reference rotates out while
	// its bytes remain untouched for separate product-owned garbage collection.
	want := nextOnboardingHeldDigests(r.ActiveDigest, r.RollbackDigest, r.HeldPruneDigests)
	if len(want) > 4 || !equalHistoryDigests(want, r.NextHeldPruneDigests) || !validHistoryHeldDigests(r.ActiveDigest, "", r.NextHeldPruneDigests) {
		return errors.New("next bundle retention history exceeds or differs from the reviewed bounded transition")
	}
	return nil
}

func historyRetentionAllowsArtifact(retention *historyRetentionReview, digest string) bool {
	if retention == nil || !historySHA.MatchString(digest) {
		return retention == nil && historySHA.MatchString(digest)
	}
	if digest == retention.ActiveDigest || digest == retention.RollbackDigest {
		return false
	}
	for _, held := range retention.HeldPruneDigests {
		if digest == held {
			return false
		}
	}
	return true
}

func historyQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func historyUpgradeUnitText(executable, broker, config string, seconds int) string {
	quote := func(value string) string {
		return "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%").Replace(value) + "\""
	}
	for _, value := range []string{executable, broker, config} {
		for _, r := range value {
			if unicode.IsControl(r) {
				return ""
			}
		}
	}
	directory := strings.ReplaceAll(path.Dir(broker), "%", "%%")
	if strings.HasSuffix(directory, "\\") || strings.TrimRightFunc(directory, unicode.IsSpace) != directory {
		directory += "/"
	}
	return "# Owned by NVIDIA Personal AI Router headless service; do not replace a foreign unit.\n[Unit]\nDescription=NVIDIA Personal AI Router headless backend\n\n[Service]\nType=simple\nExecStart=" + quote(strings.ReplaceAll(executable, "$", "$$")) + " --headless --broker-path " + quote(strings.ReplaceAll(broker, "$", "$$")) + "\nWorkingDirectory=" + directory + "\nEnvironment=" + quote("XDG_CONFIG_HOME="+config) + "\nRestart=on-failure\nRestartSec=2\nTimeoutStopSec=" + fmt.Sprint(seconds) + "\nKillMode=mixed\nUMask=0077\n\n[Install]\nWantedBy=default.target\n"
}

func historyUpgradeLauncher(executable string, systemStyle bool) string {
	if systemStyle {
		return "#!/bin/sh\n# Personal AI Router terminal UI launcher.\nexec " + historyQuote(executable) + " \"$@\"\n"
	}
	quoted := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "$", "\\$", "`", "\\`").Replace(executable)
	return "#!/bin/sh\n# Auto-generated by Personal AI Router. Runs the bundled terminal UI.\nexec \"" + quoted + "\" \"$@\"\n"
}

func validateHistoryLegacyFile(f historyLegacyFileIdentity, expected string, owners map[int]bool, limit int64, executable bool) error {
	if f.Path != expected || !path.IsAbs(f.Path) || path.Clean(f.Path) != f.Path || !historySHA.MatchString(f.SHA256) || f.Bytes <= 0 || f.Bytes > limit || f.Device == 0 || f.Inode == 0 || !owners[f.UID] || f.GID < 0 || f.Links != 1 || f.Mode > 07777 || f.Mode&0022 != 0 {
		return errors.New("legacy package file identity is incomplete, shared or writable")
	}
	if f.UID == 0 && f.GID != 0 {
		return errors.New("legacy package root-owned file has a foreign group")
	}
	if executable && f.Mode&0111 == 0 {
		return errors.New("legacy package executable identity is not executable")
	}
	return nil
}

func validateHistoryLegacyPackage(status string, d *historyLegacyPackageReceipt, info historyPlatformInfo) error {
	if status == "" {
		return nil
	}
	if status != "absent" && status != "not-applicable" && status != "admitted" && status != "retained-no-exact-rollback" && status != "retained-no-administrator-access" {
		return errors.New("legacy package disposition is invalid")
	}
	if status != "admitted" {
		if d != nil {
			return errors.New("unadmitted legacy package retained an effect receipt")
		}
		return nil
	}
	_, nativeArchitecture, platformErr := historyPlatform(info.OS, info.Arch)
	if d == nil || d.Package != "nvpair" || d.Status != "install ok installed" || !historyDebianToken.MatchString(d.Version) || platformErr != nil || d.Architecture != nativeArchitecture || !historySHA.MatchString(d.PackageRecordSHA256) || !historySHA.MatchString(d.PackageFilesSHA256) || !historySHA.MatchString(d.PackageInfoSHA256) || len(d.Processes) != 0 {
		return errors.New("legacy package receipt is incomplete or has a running owner")
	}
	root := map[int]bool{0: true}
	rollbackOwners := map[int]bool{0: true, info.UID: true}
	if err := validateHistoryLegacyFile(d.App, "/opt/PAIR/nvpair", root, 1<<30, true); err != nil {
		return err
	}
	if err := validateHistoryLegacyFile(d.Launcher, "/usr/bin/nvpair", root, 8192, true); err != nil {
		return err
	}
	if err := validateHistoryLegacyFile(d.DesktopEntry, "/usr/share/applications/nvpair.desktop", root, 1<<20, false); err != nil {
		return err
	}
	if err := validateHistoryLegacyFile(d.Rollback, d.Rollback.Path, rollbackOwners, 1<<30, false); err != nil {
		return err
	}
	cache := "/var/cache/apt/archives/"
	downloads := path.Join(info.Home, "Downloads") + "/"
	if !strings.HasPrefix(d.Rollback.Path, cache) && !strings.HasPrefix(d.Rollback.Path, downloads) || !strings.HasSuffix(d.Rollback.Path, ".deb") {
		return errors.New("legacy rollback archive is outside its bounded review locations")
	}
	return nil
}

func validateHistoryExisting(d historyExistingInstallation, info historyPlatformInfo) error {
	encoded, _ := json.Marshal(d)
	if len(encoded) > 10<<10 {
		return errors.New("existing installation descriptor exceeds the bounded helper input")
	}
	_, arch, platformErr := historyPlatform(info.OS, info.Arch)
	if platformErr != nil || arch == "" || !info.ExistingPAIR || !info.UserRuntime || d.UID != info.UID || d.Home != info.Home || d.UID <= 0 || d.Home == "/" || !path.IsAbs(d.Home) || path.Clean(d.Home) != d.Home {
		return errors.New("existing installation account or native platform is not bound")
	}
	if !historyIdentifier(d.NodeID, 128) || !historyIdentifier(d.ClusterID, 128) || !historyToken.MatchString(d.Version) || !historySHA.MatchString(d.SourceFingerprint) || !historySHA.MatchString(d.ManifestSHA256) || !historySHA.MatchString(d.ExecutableSHA256) || !historySHA.MatchString(d.IdentitySHA256) || !historySHA.MatchString(d.NodeIdentitySHA256) || !strings.HasPrefix(d.CertFingerprint, "sha256:") || !historySHA.MatchString(strings.TrimPrefix(d.CertFingerprint, "sha256:")) {
		return errors.New("existing installation identity or manifest is incomplete")
	}
	if d.ConfigHome != path.Join(d.Home, ".config") || d.Unit != historyUpgradeUnit || d.UnitPath != path.Join(d.ConfigHome, "systemd", "user", historyUpgradeUnit) || len(d.UnitBytes) == 0 || len(d.UnitBytes) > 16384 || d.ProcessID <= 0 || d.ProcessStartTicks == "" {
		return errors.New("existing headless unit or process is unbound")
	}
	unitHash := sha256.Sum256([]byte(d.UnitBytes))
	if hex.EncodeToString(unitHash[:]) != d.UnitSHA256 || path.Clean(d.Bundle) != d.Bundle || !strings.HasPrefix(d.Bundle, d.Home+"/") || d.Executable != path.Join(d.Bundle, "nvpair-tui") || d.Broker != path.Join(d.Bundle, "nvpair-ui-broker") {
		return errors.New("existing unit or bundle paths differ from their binding")
	}
	if d.UnitBytes != historyUpgradeUnitText(d.Executable, d.Broker, d.ConfigHome, 25) && d.UnitBytes != historyUpgradeUnitText(d.Executable, d.Broker, d.ConfigHome, 145) {
		return errors.New("existing unit is not a supported product-owned definition")
	}
	if ticks, err := strconv.ParseUint(d.ProcessStartTicks, 10, 64); err != nil || ticks == 0 {
		return errors.New("existing process generation is invalid")
	}
	if d.LauncherPath != path.Join(d.Home, ".local", "bin", "nvpair") {
		return errors.New("existing user launcher path is unbound")
	}
	if d.LauncherPresent {
		sum := sha256.Sum256([]byte(d.LauncherBody))
		if hex.EncodeToString(sum[:]) != d.LauncherSHA256 || d.LauncherBody != historyUpgradeLauncher(d.Executable, false) && d.LauncherBody != historyUpgradeLauncher(d.Executable, true) {
			return errors.New("existing user launcher is not a recognized wrapper for this peer")
		}
	} else if d.LauncherSHA256 != "" || d.LauncherBody != "" {
		return errors.New("absent launcher has conflicting ownership data")
	}
	if d.StartupLifetime != "session" && d.StartupLifetime != "persistent" || (d.StartupLifetime == "persistent") != info.Linger {
		return errors.New("existing startup lifetime is not confirmed")
	}
	want := map[string]bool{}
	for _, name := range onboardingSupportedBinaries(len(d.Components)) {
		want[name] = true
	}
	if len(want) == 0 || len(d.Components) != len(want) {
		return errors.New("old product inventory is not a supported complete unified or legacy bundle")
	}
	for _, component := range d.Components {
		if !want[component.Name] || component.Bytes <= 0 || component.Bytes > 256<<20 || !historySHA.MatchString(component.SHA256) {
			return errors.New("old product inventory is incomplete or inconsistent")
		}
		delete(want, component.Name)
		if component.Name == "nvpair-tui" && component.SHA256 != d.ExecutableSHA256 {
			return errors.New("running executable differs from its installed manifest")
		}
	}
	if err := validateHistoryRetention(d); err != nil {
		return err
	}
	return validateHistoryLegacyPackage(d.LegacyPackageStatus, d.LegacyPackage, info)
}

func validHistoryCurrentPlan(plan historyPlan) bool {
	if plan.Review.Action == "" || plan.Review.Action == "install" {
		return plan.ExistingInstallation == nil && plan.UpgradePhase == ""
	}
	if plan.Review.Action != "upgrade" || plan.ExistingInstallation == nil || plan.Review.ExistingInstallation == nil || validateHistoryExisting(*plan.ExistingInstallation, plan.Info) != nil || !historyRetentionAllowsArtifact(plan.ExistingInstallation.Retention, plan.Artifact.SHA256) || !reflect.DeepEqual(*plan.Review.ExistingInstallation, plan.ExistingInstallation.summary()) {
		return false
	}
	switch plan.UpgradePhase {
	case "", "staging", "staged", "stopping", "stopped", "installing-unit", "installed", "starting", "started", "identity-verified", "verified", "retiring", "retired", "rolling-back", "rolled-back", "cancelled-before-stop":
		return plan.InviteID == "" && plan.InviteRequestKey == "" && len(plan.InviteHistory) == 0
	}
	return false
}

func validHistoryLegacyPlan(operation historyOperation, target historyTargetState, plan historyPlan) bool {
	completed := operation.State == "completed" && target.Stage == "paired" && !target.CanRetry && !target.CanCancel && target.CleanupConfirmed && plan.UpgradePhase == "retired" && plan.Receipt.Installed && plan.Receipt.ServiceInstalled && plan.Receipt.ServiceStarted && plan.Receipt.CleanupConfirmed
	cancelled := operation.State == "cancelled" && target.Stage == "cancelled" && target.CleanupConfirmed && plan.UpgradePhase == "cancelled-before-stop" && !plan.Receipt.Installed && !plan.Receipt.ServiceInstalled && !plan.Receipt.ServiceStarted && plan.Receipt.CleanupConfirmed
	if operation.FinishedAt <= 0 || !completed && !cancelled || plan.Review.Action != "upgrade" || plan.ExistingInstallation == nil || plan.ExistingInstallation.Retention != nil || plan.Review.ExistingInstallation == nil || !reflect.DeepEqual(*plan.Review.ExistingInstallation, plan.ExistingInstallation.summary()) {
		return false
	}
	if completed && (target.NodeID != plan.ExistingInstallation.NodeID || plan.Receipt.NodeID != plan.ExistingInstallation.NodeID || plan.Receipt.ArtifactSHA256 != plan.Artifact.SHA256) {
		return false
	}
	legacy := *plan.ExistingInstallation
	digest, managed := historyManagedBundleDigest(legacy)
	if !managed {
		return false
	}
	legacy.Retention = &historyRetentionReview{Owner: "absent", ActiveDigest: digest, HeldPruneDigests: []string{}, NextHeldPruneDigests: []string{}, NextGeneration: 1}
	plan.ExistingInstallation = &legacy
	summary := legacy.summary()
	plan.Review.ExistingInstallation = &summary
	return validHistoryCurrentPlan(plan)
}
