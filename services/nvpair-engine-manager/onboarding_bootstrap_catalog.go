// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"nvpair-shared/hostbootstrap"
)

const bootstrapCatalogFile = "onboarding-bootstrap-catalog.json"
const bootstrapCatalogResourceDirectory = "onboarding-bootstrap"
const bootstrapCatalogChecksumFile = bootstrapCatalogFile + ".sha256"

var bootstrapCatalogSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
var bootstrapCatalogVersion = regexp.MustCompile(
	`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?(?:\+[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$`,
)
var bootstrapCatalogFileName = regexp.MustCompile(
	`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`,
)

type bootstrapCatalogSignature struct {
	Status        string `json:"status"`
	Kind          string `json:"kind"`
	Identity      string `json:"identity"`
	Notarized     bool   `json:"notarized"`
	SignatureFile string `json:"signatureFile"`
	ChecksumFile  string `json:"checksumFile"`
	ContentSHA256 string `json:"contentSHA256"`
	ContentSize   int64  `json:"contentSize"`
}

type bootstrapCatalogIntegrity struct {
	ChecksumAlgorithm string                    `json:"checksumAlgorithm"`
	ChecksumFile      string                    `json:"checksumFile"`
	Signature         bootstrapCatalogSignature `json:"signature"`
}

type bootstrapCatalogArtifact struct {
	Identity   hostbootstrap.ArtifactIdentity `json:"identity"`
	FileName   string                         `json:"fileName"`
	Size       int64                          `json:"size"`
	Provenance string                         `json:"provenance"`
	Signature  bootstrapCatalogSignature      `json:"signature"`
}

type bootstrapCatalogExpectedArtifact struct {
	ID       string
	Path     string
	FileName string
}

type bootstrapCatalogTarget struct {
	Target      hostbootstrap.Target        `json:"target"`
	Roles       []hostbootstrap.Role        `json:"roles"`
	Bootstrap   bootstrapCatalogArtifact    `json:"bootstrap"`
	Helper      bootstrapCatalogArtifact    `json:"helper"`
	Product     bootstrapCatalogArtifact    `json:"product"`
	Combination bootstrapCatalogCombination `json:"combination"`
}

type bootstrapCatalogCombination struct {
	FileName   string                    `json:"fileName"`
	Size       int64                     `json:"size"`
	SHA256     string                    `json:"sha256"`
	Provenance string                    `json:"provenance"`
	Signature  bootstrapCatalogSignature `json:"signature"`
}

type bootstrapCatalog struct {
	SchemaVersion int                       `json:"schemaVersion"`
	Integrity     bootstrapCatalogIntegrity `json:"integrity"`
	Targets       []bootstrapCatalogTarget  `json:"targets"`
}

func parseBootstrapCatalog(raw []byte) (bootstrapCatalog, error) {
	var catalog bootstrapCatalog
	if err := validateBootstrapCatalogShape(raw); err != nil {
		return bootstrapCatalog{}, err
	}
	if err := onboardingDecode(raw, &catalog); err != nil {
		return bootstrapCatalog{}, err
	}
	supported := hostbootstrap.SupportedTargets()
	if catalog.SchemaVersion != hostbootstrap.SchemaVersion ||
		len(catalog.Targets) != len(supported) {
		return bootstrapCatalog{}, errors.New("bootstrap catalog must list the six supported targets")
	}
	official := false
	for index, entry := range catalog.Targets {
		if entry.Target != supported[index] ||
			!reflect.DeepEqual(
				entry.Roles,
				[]hostbootstrap.Role{
					hostbootstrap.RoleDesktop,
					hostbootstrap.RoleHeadless,
				},
			) {
			return bootstrapCatalog{}, errors.New("bootstrap catalog entry does not match a supported target")
		}
		bootstrapIdentity, helperIdentity, productIdentity, err :=
			bootstrapCatalogFixedIdentities(entry.Target)
		if err != nil ||
			validateBootstrapCatalogArtifact(
				entry.Bootstrap,
				bootstrapIdentity,
			) != nil ||
			validateBootstrapCatalogArtifact(
				entry.Helper,
				helperIdentity,
			) != nil ||
			validateBootstrapCatalogArtifact(
				entry.Product,
				productIdentity,
			) != nil ||
			validateBootstrapCatalogCombination(entry) != nil ||
			entry.Bootstrap.Provenance != entry.Helper.Provenance ||
			entry.Bootstrap.Provenance != entry.Product.Provenance ||
			entry.Bootstrap.Provenance != entry.Combination.Provenance {
			return bootstrapCatalog{}, errors.New("bootstrap catalog artifact identity is invalid")
		}
		entryOfficial := entry.Bootstrap.Provenance == "official-release"
		if index == 0 {
			official = entryOfficial
		} else if official != entryOfficial {
			return bootstrapCatalog{}, errors.New("bootstrap catalog provenance is inconsistent")
		}
		if validateBootstrapCatalogTargetSignatures(entry, official) != nil {
			return bootstrapCatalog{}, errors.New("bootstrap catalog signature metadata is invalid")
		}
	}
	if validateBootstrapCatalogIntegrity(catalog.Integrity, official) != nil {
		return bootstrapCatalog{}, errors.New("bootstrap catalog integrity metadata is invalid")
	}
	return catalog, nil
}

func bootstrapCatalogResourcePath(executable string) (string, error) {
	if !filepath.IsAbs(executable) ||
		filepath.Clean(executable) != executable {
		return "", errors.New("engine manager executable path is invalid")
	}
	return filepath.Clean(filepath.Join(
		filepath.Dir(executable),
		"..",
		bootstrapCatalogResourceDirectory,
		bootstrapCatalogFile,
	)), nil
}

func currentBootstrapCatalogResourcePath() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", err
	}
	return bootstrapCatalogResourcePath(executable)
}

func readBootstrapCatalog(catalogPath string) (bootstrapCatalog, error) {
	raw, err := readOnboardingFile(catalogPath, 128<<10)
	if err != nil {
		return bootstrapCatalog{}, errors.New("bootstrap catalog is unavailable")
	}
	catalog, err := parseBootstrapCatalog(raw)
	if err != nil {
		return bootstrapCatalog{}, err
	}
	digest := sha256.Sum256(raw)
	checksum, err := readOnboardingFile(
		filepath.Join(filepath.Dir(catalogPath), catalog.Integrity.ChecksumFile),
		256,
	)
	if err != nil ||
		string(checksum) != hex.EncodeToString(digest[:])+
			"  "+bootstrapCatalogFile+"\n" {
		return bootstrapCatalog{}, errors.New("bootstrap catalog checksum is invalid")
	}
	if catalog.Integrity.Signature.Status == "signed" {
		signature, err := readOnboardingFile(
			filepath.Join(
				filepath.Dir(catalogPath),
				catalog.Integrity.Signature.SignatureFile,
			),
			1<<20,
		)
		if err != nil || len(signature) == 0 {
			return bootstrapCatalog{}, errors.New("bootstrap catalog signature is unavailable")
		}
	}
	return catalog, nil
}

func (catalog bootstrapCatalog) validateBinding(
	binding hostbootstrap.Binding,
) error {
	for _, entry := range catalog.Targets {
		if entry.Target != binding.Target {
			continue
		}
		if entry.Helper.Identity != binding.Helper ||
			entry.Product.Identity != binding.Product {
			return errors.New("bootstrap request artifacts do not match the signed catalog")
		}
		for _, role := range entry.Roles {
			if role == binding.RuntimeOwner {
				return nil
			}
		}
		return errors.New("bootstrap runtime role is unavailable for this target")
	}
	return errors.New("bootstrap target is absent from the signed catalog")
}

func validateBootstrapCatalogArtifact(
	artifact bootstrapCatalogArtifact,
	expected bootstrapCatalogExpectedArtifact,
) error {
	if artifact.Identity.ID != expected.ID ||
		artifact.Identity.Path != expected.Path ||
		!bootstrapCatalogVersion.MatchString(artifact.Identity.Version) ||
		!bootstrapCatalogSHA256.MatchString(artifact.Identity.SHA256) ||
		artifact.FileName != expected.FileName ||
		artifact.Size < 1 ||
		artifact.Size > 8<<30 ||
		(artifact.Provenance != "official-release" &&
			artifact.Provenance != "engineering") {
		return errors.New("invalid bootstrap catalog artifact")
	}
	return nil
}

func validateBootstrapCatalogCombination(
	entry bootstrapCatalogTarget,
) error {
	expected := "nvpair-bootstrap-" +
		string(entry.Target.Platform) + "-" +
		string(entry.Target.Architecture) + ".zip"
	if entry.Combination.FileName != expected ||
		entry.Combination.Size < 1 ||
		entry.Combination.Size > 8<<30 ||
		!bootstrapCatalogSHA256.MatchString(
			entry.Combination.SHA256,
		) ||
		(entry.Combination.Provenance != "official-release" &&
			entry.Combination.Provenance != "engineering") {
		return errors.New("invalid bootstrap catalog combination")
	}
	return nil
}

func validateBootstrapCatalogTargetSignatures(
	entry bootstrapCatalogTarget,
	official bool,
) error {
	if validateBootstrapCatalogSignature(
		entry.Bootstrap.Signature,
		entry.Bootstrap.Identity.SHA256,
		entry.Bootstrap.Size,
		entry.Target,
		"bootstrap",
		official,
	) != nil ||
		validateBootstrapCatalogSignature(
			entry.Helper.Signature,
			entry.Helper.Identity.SHA256,
			entry.Helper.Size,
			entry.Target,
			"helper",
			official,
		) != nil ||
		validateBootstrapCatalogSignature(
			entry.Product.Signature,
			"",
			0,
			entry.Target,
			"product",
			official,
		) != nil ||
		validateBootstrapCatalogSignature(
			entry.Combination.Signature,
			entry.Combination.SHA256,
			entry.Combination.Size,
			entry.Target,
			"combination",
			official,
		) != nil {
		return errors.New("invalid signature metadata")
	}
	return nil
}

func validateBootstrapCatalogSignature(
	signature bootstrapCatalogSignature,
	expectedSHA256 string,
	expectedSize int64,
	target hostbootstrap.Target,
	artifact string,
	official bool,
) error {
	if !bootstrapCatalogSHA256.MatchString(
		signature.ContentSHA256,
	) ||
		signature.ContentSize < 1 ||
		signature.ContentSize > 8<<30 ||
		(expectedSHA256 != "" &&
			signature.ContentSHA256 != expectedSHA256) ||
		(expectedSize != 0 &&
			signature.ContentSize != expectedSize) {
		return errors.New("signature content identity is invalid")
	}
	if signature.Status == "unsigned" {
		if official ||
			signature.Kind != "none" ||
			signature.Identity != "" ||
			signature.Notarized ||
			signature.SignatureFile != "" ||
			signature.ChecksumFile != "" {
			return errors.New("unsigned signature metadata is invalid")
		}
		return nil
	}
	if signature.Status != "signed" ||
		signature.Identity == "" ||
		!bootstrapCatalogFileName.MatchString(
			signature.ChecksumFile,
		) {
		return errors.New("signed signature metadata is invalid")
	}
	expectedKind := "detached-release"
	if artifact != "combination" &&
		target.Platform == hostbootstrap.PlatformWindows {
		expectedKind = "authenticode"
	}
	if artifact != "combination" &&
		target.Platform == hostbootstrap.PlatformDarwin {
		expectedKind = "apple-code-sign"
	}
	if official && signature.Kind != expectedKind {
		return errors.New("official signature kind is invalid")
	}
	if official &&
		target.Platform == hostbootstrap.PlatformDarwin &&
		artifact != "combination" &&
		!signature.Notarized {
		return errors.New("official macOS artifact is not notarized")
	}
	if signature.Kind == "detached-release" &&
		!bootstrapCatalogFileName.MatchString(
			signature.SignatureFile,
		) {
		return errors.New("detached signature file is invalid")
	}
	return nil
}

func validateBootstrapCatalogIntegrity(
	integrity bootstrapCatalogIntegrity,
	official bool,
) error {
	signature := integrity.Signature
	if integrity.ChecksumAlgorithm != "sha256" ||
		integrity.ChecksumFile != bootstrapCatalogChecksumFile ||
		signature.ChecksumFile != bootstrapCatalogChecksumFile ||
		signature.ContentSHA256 != "" ||
		signature.ContentSize != 0 ||
		signature.Notarized {
		return errors.New("invalid catalog integrity")
	}
	if !official {
		if signature.Status != "unsigned" ||
			signature.Kind != "none" ||
			signature.Identity != "" ||
			signature.SignatureFile != "" {
			return errors.New("invalid engineering catalog identity")
		}
		return nil
	}
	if signature.Status != "signed" ||
		signature.Kind != "detached-release" ||
		signature.Identity == "" ||
		!bootstrapCatalogFileName.MatchString(
			signature.SignatureFile,
		) {
		return errors.New("invalid official catalog identity")
	}
	return nil
}

func bootstrapCatalogFixedIdentities(
	target hostbootstrap.Target,
) (
	bootstrapCatalogExpectedArtifact,
	bootstrapCatalogExpectedArtifact,
	bootstrapCatalogExpectedArtifact,
	error,
) {
	bootstrap := bootstrapCatalogExpectedArtifact{
		ID:       "nvpair-host-bootstrap",
		FileName: "nvpair-host-bootstrap",
	}
	helper := bootstrapCatalogExpectedArtifact{
		ID:       "nvpair-host-helper",
		FileName: "nvpair-host-helper",
	}
	product := bootstrapCatalogExpectedArtifact{
		ID:       "nvpair",
		FileName: "nvpair-product.zip",
	}
	switch target.Platform {
	case hostbootstrap.PlatformWindows:
		bootstrap.Path =
			`C:\Program Files\NVIDIA Corporation\PAIR\nvpair-host-bootstrap.exe`
		bootstrap.FileName = "nvpair-host-bootstrap.exe"
		helper.Path =
			`C:\Program Files\NVIDIA Corporation\PAIR\nvpair-host-helper.exe`
		helper.FileName = "nvpair-host-helper.exe"
		product.Path =
			`C:\Program Files\NVIDIA Corporation\PAIR\product`
	case hostbootstrap.PlatformDarwin:
		bootstrap.Path =
			"/Library/PrivilegedHelperTools/nvpair-host-bootstrap"
		helper.Path =
			"/Library/PrivilegedHelperTools/nvpair-host-helper"
		product.Path = "/Applications/NVPAIR.app"
	case hostbootstrap.PlatformLinux:
		bootstrap.Path = "/usr/libexec/nvpair-host-bootstrap"
		helper.Path = "/usr/libexec/nvpair-host-helper"
		product.Path = "/opt/nvpair/product"
	default:
		return bootstrapCatalogExpectedArtifact{},
			bootstrapCatalogExpectedArtifact{},
			bootstrapCatalogExpectedArtifact{},
			errors.New("unsupported bootstrap target")
	}
	if target.Architecture != hostbootstrap.ArchitectureAMD64 &&
		target.Architecture != hostbootstrap.ArchitectureARM64 {
		return bootstrapCatalogExpectedArtifact{},
			bootstrapCatalogExpectedArtifact{},
			bootstrapCatalogExpectedArtifact{},
			errors.New("unsupported bootstrap architecture")
	}
	return bootstrap, helper, product, nil
}

func validateBootstrapCatalogShape(raw []byte) error {
	if len(raw) == 0 || len(raw) > 128<<10 {
		return errors.New("bootstrap catalog is empty or oversized")
	}
	top, err := bootstrapCatalogObject(
		raw,
		[]string{"schemaVersion", "integrity", "targets"},
	)
	if err != nil {
		return err
	}
	integrity, err := bootstrapCatalogObject(
		top["integrity"],
		[]string{
			"checksumAlgorithm",
			"checksumFile",
			"signature",
		},
	)
	if err != nil {
		return err
	}
	if err := validateBootstrapCatalogSignatureShape(
		integrity["signature"],
	); err != nil {
		return err
	}
	var targets []json.RawMessage
	if json.Unmarshal(top["targets"], &targets) != nil {
		return errors.New("bootstrap catalog targets are invalid")
	}
	for _, targetRaw := range targets {
		target, err := bootstrapCatalogObject(
			targetRaw,
			[]string{
				"target",
				"roles",
				"bootstrap",
				"helper",
				"product",
				"combination",
			},
		)
		if err != nil {
			return err
		}
		if _, err := bootstrapCatalogObject(
			target["target"],
			[]string{"platform", "architecture"},
		); err != nil {
			return err
		}
		for _, field := range []string{
			"bootstrap",
			"helper",
			"product",
		} {
			artifact, err := bootstrapCatalogObject(
				target[field],
				[]string{
					"identity",
					"fileName",
					"size",
					"provenance",
					"signature",
				},
			)
			if err != nil {
				return err
			}
			if _, err := bootstrapCatalogObject(
				artifact["identity"],
				[]string{
					"id",
					"version",
					"sha256",
					"path",
				},
			); err != nil {
				return err
			}
			if err := validateBootstrapCatalogSignatureShape(
				artifact["signature"],
			); err != nil {
				return err
			}
		}
		combination, err := bootstrapCatalogObject(
			target["combination"],
			[]string{
				"fileName",
				"size",
				"sha256",
				"provenance",
				"signature",
			},
		)
		if err != nil {
			return err
		}
		if err := validateBootstrapCatalogSignatureShape(
			combination["signature"],
		); err != nil {
			return err
		}
	}
	return nil
}

func validateBootstrapCatalogSignatureShape(raw []byte) error {
	_, err := bootstrapCatalogObject(
		raw,
		[]string{
			"status",
			"kind",
			"identity",
			"notarized",
			"signatureFile",
			"checksumFile",
			"contentSHA256",
			"contentSize",
		},
	)
	return err
}

func bootstrapCatalogObject(
	raw []byte,
	fields []string,
) (map[string]json.RawMessage, error) {
	allowed := make(map[string]bool, len(fields))
	for _, field := range fields {
		allowed[field] = true
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("bootstrap catalog object is invalid")
	}
	result := make(map[string]json.RawMessage, len(fields))
	folded := make(map[string]bool, len(fields))
	for decoder.More() {
		token, err := decoder.Token()
		field, ok := token.(string)
		if err != nil || !ok || !allowed[field] ||
			folded[strings.ToLower(field)] {
			return nil, errors.New("bootstrap catalog field is unknown or duplicated")
		}
		folded[strings.ToLower(field)] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, errors.New("bootstrap catalog value is invalid")
		}
		result[field] = value
	}
	if token, err := decoder.Token(); err != nil ||
		token != json.Delim('}') ||
		len(result) != len(fields) {
		return nil, errors.New("bootstrap catalog object is incomplete")
	}
	var trailing json.RawMessage
	if decoder.Decode(&trailing) != io.EOF {
		return nil, errors.New("bootstrap catalog has trailing data")
	}
	return result, nil
}
