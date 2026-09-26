// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"

	"nvpair-shared/hostbootstrap"
	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
)

type setupCandidatePartition struct {
	SSHReady          []setupCandidate
	BootstrapRequired []setupCandidate
}

type setupBootstrapCatalogArtifact struct {
	Identity   hostbootstrap.ArtifactIdentity `json:"identity"`
	FileName   string                         `json:"fileName"`
	Size       int64                          `json:"size"`
	Provenance string                         `json:"provenance"`
	Signature  setupBootstrapCatalogSignature `json:"signature"`
}

type setupBootstrapCatalogSignature struct {
	Status        string `json:"status"`
	Kind          string `json:"kind"`
	Identity      string `json:"identity"`
	Notarized     bool   `json:"notarized"`
	SignatureFile string `json:"signatureFile"`
	ChecksumFile  string `json:"checksumFile"`
	ContentSHA256 string `json:"contentSHA256"`
	ContentSize   int64  `json:"contentSize"`
}

type setupBootstrapCatalogCombination struct {
	FileName   string                         `json:"fileName"`
	Size       int64                          `json:"size"`
	SHA256     string                         `json:"sha256"`
	Provenance string                         `json:"provenance"`
	Signature  setupBootstrapCatalogSignature `json:"signature"`
}

type setupBootstrapCatalogIntegrity struct {
	ChecksumAlgorithm string                         `json:"checksumAlgorithm"`
	ChecksumFile      string                         `json:"checksumFile"`
	Signature         setupBootstrapCatalogSignature `json:"signature"`
}

type setupBootstrapCatalogTarget struct {
	Target      hostbootstrap.Target             `json:"target"`
	Roles       []hostbootstrap.Role             `json:"roles"`
	Bootstrap   setupBootstrapCatalogArtifact    `json:"bootstrap"`
	Helper      setupBootstrapCatalogArtifact    `json:"helper"`
	Product     setupBootstrapCatalogArtifact    `json:"product"`
	Combination setupBootstrapCatalogCombination `json:"combination"`
}

type setupBootstrapCatalog struct {
	SchemaVersion int                            `json:"schemaVersion"`
	Integrity     setupBootstrapCatalogIntegrity `json:"integrity"`
	Targets       []setupBootstrapCatalogTarget  `json:"targets"`
}

type setupBootstrapControllerKeys struct {
	SchemaVersion int                               `json:"schemaVersion"`
	Keys          []hostbootstrap.PublicKeyIdentity `json:"keys"`
}

type setupDiscoveryScope struct {
	ScopeID      string `json:"scopeId"`
	Interface    string `json:"interface"`
	LocalAddress string `json:"localAddress"`
	CIDR         string `json:"cidr"`
	Eligible     bool   `json:"eligible"`
	Reason       string `json:"reason,omitempty"`
}

type setupBootstrapTargetReference struct {
	CandidateID   string `json:"candidateId"`
	AccessID      string `json:"accessId"`
	HostKeySHA256 string `json:"hostKeySha256"`
}

type setupBootstrapCatalogMsg struct {
	catalog setupBootstrapCatalog
	err     error
}

type setupBootstrapControllerKeysMsg struct {
	keys setupBootstrapControllerKeys
	err  error
}

type setupScopesMsg struct {
	scopes []setupDiscoveryScope
	err    error
}

type setupArtifactMsg struct {
	artifact setupArtifact
	err      error
}

type setupBootstrapMsg struct {
	action  string
	status  *hostbootstrap.Status
	plan    *hostbootstrap.Plan
	receipt *hostbootstrap.Receipt
	err     error
}

func setupCandidateLane(candidate setupCandidate) string {
	if candidate.BootstrapState == "ssh-ready" {
		return "ssh-ready"
	}
	return "bootstrap-required"
}

func partitionSetupCandidates(
	candidates []setupCandidate,
) setupCandidatePartition {
	partition := setupCandidatePartition{
		SSHReady:          []setupCandidate{},
		BootstrapRequired: []setupCandidate{},
	}
	for _, candidate := range candidates {
		if setupCandidateLane(candidate) == "ssh-ready" {
			partition.SSHReady = append(partition.SSHReady, candidate)
		} else {
			partition.BootstrapRequired = append(
				partition.BootstrapRequired,
				candidate,
			)
		}
	}
	return partition
}

func setupBootstrapCatalogCmd(client *rpc.Client) tea.Cmd {
	return call(
		client,
		"engine:onboarding-bootstrap-catalog",
		struct{}{},
		func(msg *rpc.Message, err error) tea.Msg {
			if err != nil {
				return setupBootstrapCatalogMsg{err: err}
			}
			var catalog setupBootstrapCatalog
			if err := decodeSetupBootstrap(msg.Result, &catalog); err != nil {
				return setupBootstrapCatalogMsg{err: err}
			}
			if err := validateSetupBootstrapCatalog(catalog); err != nil {
				return setupBootstrapCatalogMsg{err: err}
			}
			return setupBootstrapCatalogMsg{catalog: catalog}
		},
	)
}

func setupBootstrapControllerKeysCmd(client *rpc.Client) tea.Cmd {
	return call(
		client,
		"engine:onboarding-bootstrap-controller-keys",
		struct{}{},
		func(msg *rpc.Message, err error) tea.Msg {
			if err != nil {
				return setupBootstrapControllerKeysMsg{err: err}
			}
			var keys setupBootstrapControllerKeys
			if err := decodeSetupBootstrap(msg.Result, &keys); err != nil {
				return setupBootstrapControllerKeysMsg{err: err}
			}
			if err := validateSetupBootstrapControllerKeys(keys); err != nil {
				return setupBootstrapControllerKeysMsg{err: err}
			}
			return setupBootstrapControllerKeysMsg{keys: keys}
		},
	)
}

func setupScopesCmd(client *rpc.Client) tea.Cmd {
	return call(
		client,
		"engine:onboarding-scopes",
		struct{}{},
		func(msg *rpc.Message, err error) tea.Msg {
			if err != nil {
				return setupScopesMsg{err: err}
			}
			var result struct {
				Scopes []setupDiscoveryScope `json:"scopes"`
			}
			if err := decodeParams(msg.Result, &result); err != nil {
				return setupScopesMsg{err: err}
			}
			return setupScopesMsg{scopes: result.Scopes}
		},
	)
}

func setupDiscoverCmd(client *rpc.Client, scopeID string) tea.Cmd {
	return callWithTimeout(
		client,
		setupOperationTimeout,
		"engine:onboarding-discover",
		map[string]string{"scopeId": scopeID},
		func(msg *rpc.Message, err error) tea.Msg {
			if err != nil {
				return setupCandidatesMsg{err: err}
			}
			var result struct {
				Candidates []setupCandidate `json:"candidates"`
			}
			if err := decodeParams(msg.Result, &result); err != nil {
				return setupCandidatesMsg{err: err}
			}
			return setupCandidatesMsg{candidates: result.Candidates}
		},
	)
}

func setupImportArtifactCmd(client *rpc.Client, file string) tea.Cmd {
	return call(
		client,
		"engine:onboarding-import-artifact",
		map[string]string{"file": file},
		func(msg *rpc.Message, err error) tea.Msg {
			if err != nil {
				return setupArtifactMsg{err: err}
			}
			var artifact setupArtifact
			if err := decodeParams(msg.Result, &artifact); err != nil {
				return setupArtifactMsg{err: err}
			}
			return setupArtifactMsg{artifact: artifact}
		},
	)
}

func setupBootstrapInspectCmd(
	client *rpc.Client,
	reference setupBootstrapTargetReference,
	request hostbootstrap.Request,
) tea.Cmd {
	return setupBootstrapRequestCmd(
		client,
		"engine:onboarding-bootstrap-inspect",
		"bootstrap inspect",
		reference,
		request,
		false,
	)
}

func setupBootstrapReviewCmd(
	client *rpc.Client,
	reference setupBootstrapTargetReference,
	request hostbootstrap.Request,
) tea.Cmd {
	return setupBootstrapRequestCmd(
		client,
		"engine:onboarding-bootstrap-review",
		"bootstrap review",
		reference,
		request,
		true,
	)
}

func setupBootstrapRequestCmd(
	client *rpc.Client,
	method string,
	action string,
	reference setupBootstrapTargetReference,
	request hostbootstrap.Request,
	review bool,
) tea.Cmd {
	if err := request.Validate(); err != nil {
		return func() tea.Msg {
			return setupBootstrapMsg{action: action, err: err}
		}
	}
	params := struct {
		setupBootstrapTargetReference
		Request hostbootstrap.Request `json:"request"`
	}{reference, request}
	return callWithTimeout(
		client,
		setupOperationTimeout,
		method,
		params,
		func(msg *rpc.Message, err error) tea.Msg {
			if err != nil {
				return setupBootstrapMsg{action: action, err: err}
			}
			if review {
				plan, err := hostbootstrap.DecodePlan(msg.Result)
				if err != nil ||
					plan.OperationID != request.OperationID ||
					plan.Binding != request.Binding {
					if err == nil {
						err = errors.New(
							"bootstrap review returned a different request",
						)
					}
					return setupBootstrapMsg{action: action, err: err}
				}
				return setupBootstrapMsg{action: action, plan: &plan}
			}
			status, err := hostbootstrap.DecodeStatus(msg.Result)
			if err != nil ||
				status.OperationID != request.OperationID ||
				status.Binding != request.Binding {
				if err == nil {
					err = errors.New(
						"bootstrap inspection returned a different request",
					)
				}
				return setupBootstrapMsg{action: action, err: err}
			}
			return setupBootstrapMsg{action: action, status: &status}
		},
	)
}

func setupBootstrapApplyCmd(
	client *rpc.Client,
	reference setupBootstrapTargetReference,
	plan hostbootstrap.Plan,
) tea.Cmd {
	return setupBootstrapPlanCmd(
		client,
		"engine:onboarding-bootstrap-apply",
		"bootstrap apply",
		reference,
		plan,
		false,
	)
}

func setupBootstrapVerifyCmd(
	client *rpc.Client,
	reference setupBootstrapTargetReference,
	plan hostbootstrap.Plan,
) tea.Cmd {
	return setupBootstrapPlanCmd(
		client,
		"engine:onboarding-bootstrap-verify",
		"bootstrap verify",
		reference,
		plan,
		true,
	)
}

func setupBootstrapPlanCmd(
	client *rpc.Client,
	method string,
	action string,
	reference setupBootstrapTargetReference,
	plan hostbootstrap.Plan,
	verify bool,
) tea.Cmd {
	if err := plan.Validate(); err != nil {
		return func() tea.Msg {
			return setupBootstrapMsg{action: action, err: err}
		}
	}
	params := struct {
		setupBootstrapTargetReference
		Plan hostbootstrap.Plan `json:"plan"`
	}{reference, plan}
	return callWithTimeout(
		client,
		setupOperationTimeout,
		method,
		params,
		func(msg *rpc.Message, err error) tea.Msg {
			if err != nil {
				return setupBootstrapMsg{action: action, err: err}
			}
			if verify {
				receipt, err := hostbootstrap.DecodeReceipt(msg.Result)
				if err != nil ||
					receipt.OperationID != plan.OperationID ||
					receipt.Binding != plan.Binding ||
					receipt.Decision != plan.Decision {
					if err == nil {
						err = errors.New(
							"bootstrap verification returned a different plan",
						)
					}
					return setupBootstrapMsg{action: action, err: err}
				}
				return setupBootstrapMsg{
					action:  action,
					receipt: &receipt,
				}
			}
			status, err := hostbootstrap.DecodeStatus(msg.Result)
			if err != nil ||
				status.OperationID != plan.OperationID ||
				status.Binding != plan.Binding ||
				status.Decision != plan.Decision {
				if err == nil {
					err = errors.New(
						"bootstrap apply returned a different plan",
					)
				}
				return setupBootstrapMsg{action: action, err: err}
			}
			return setupBootstrapMsg{action: action, status: &status}
		},
	)
}

func setupBootstrapStatusCmd(
	client *rpc.Client,
	reference setupBootstrapTargetReference,
	operationID string,
) tea.Cmd {
	return setupBootstrapOperationCmd(
		client,
		"engine:onboarding-bootstrap-status",
		"bootstrap status",
		reference,
		operationID,
	)
}

func setupBootstrapRecoverCmd(
	client *rpc.Client,
	reference setupBootstrapTargetReference,
	operationID string,
) tea.Cmd {
	return setupBootstrapOperationCmd(
		client,
		"engine:onboarding-bootstrap-recover",
		"bootstrap recover",
		reference,
		operationID,
	)
}

func setupBootstrapOperationCmd(
	client *rpc.Client,
	method string,
	action string,
	reference setupBootstrapTargetReference,
	operationID string,
) tea.Cmd {
	params := struct {
		setupBootstrapTargetReference
		OperationID string `json:"operationId"`
	}{reference, operationID}
	return callWithTimeout(
		client,
		setupOperationTimeout,
		method,
		params,
		func(msg *rpc.Message, err error) tea.Msg {
			if err != nil {
				return setupBootstrapMsg{action: action, err: err}
			}
			status, err := hostbootstrap.DecodeStatus(msg.Result)
			if err != nil || status.OperationID != operationID {
				if err == nil {
					err = errors.New(
						"bootstrap status returned a different operation",
					)
				}
				return setupBootstrapMsg{action: action, err: err}
			}
			return setupBootstrapMsg{action: action, status: &status}
		},
	)
}

func decodeSetupBootstrap(raw json.RawMessage, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("bootstrap response has trailing data")
	}
	return nil
}

func validateSetupBootstrapCatalog(catalog setupBootstrapCatalog) error {
	targets := hostbootstrap.SupportedTargets()
	if catalog.SchemaVersion != hostbootstrap.SchemaVersion ||
		len(catalog.Targets) != len(targets) ||
		catalog.Integrity.ChecksumAlgorithm != "sha256" ||
		catalog.Integrity.ChecksumFile !=
			"onboarding-bootstrap-catalog.json.sha256" {
		return errors.New("bootstrap catalog must contain six targets")
	}
	for index, entry := range catalog.Targets {
		if entry.Target != targets[index] ||
			!reflect.DeepEqual(
				entry.Roles,
				[]hostbootstrap.Role{
					hostbootstrap.RoleDesktop,
					hostbootstrap.RoleHeadless,
				},
			) {
			return errors.New("bootstrap catalog target is invalid")
		}
		for _, artifact := range []setupBootstrapCatalogArtifact{
			entry.Bootstrap,
			entry.Helper,
			entry.Product,
		} {
			if artifact.Identity.ID == "" ||
				artifact.Identity.Version == "" ||
				artifact.Identity.SHA256 == "" ||
				artifact.Identity.Path == "" ||
				artifact.FileName == "" ||
				artifact.Size < 1 ||
				(artifact.Provenance != "official-release" &&
					artifact.Provenance != "engineering") {
				return errors.New("bootstrap catalog artifact is invalid")
			}
			if err := validateSetupBootstrapCatalogSignature(
				artifact.Signature,
				artifact.Provenance,
			); err != nil {
				return err
			}
		}
		if entry.Combination.FileName !=
			"nvpair-bootstrap-"+
				string(entry.Target.Platform)+"-"+
				string(entry.Target.Architecture)+".zip" ||
			entry.Combination.Size < 1 ||
			len(entry.Combination.SHA256) != 64 ||
			entry.Combination.Provenance !=
				entry.Bootstrap.Provenance {
			return errors.New("bootstrap catalog combination is invalid")
		}
		if err := validateSetupBootstrapCatalogSignature(
			entry.Combination.Signature,
			entry.Combination.Provenance,
		); err != nil {
			return err
		}
	}
	return nil
}

func validateSetupBootstrapCatalogSignature(
	signature setupBootstrapCatalogSignature,
	provenance string,
) error {
	if len(signature.ContentSHA256) != 64 ||
		signature.ContentSize < 1 {
		return errors.New("bootstrap catalog signature is invalid")
	}
	if provenance == "engineering" {
		if signature.Status != "unsigned" ||
			signature.Kind != "none" ||
			signature.Identity != "" ||
			signature.Notarized ||
			signature.SignatureFile != "" ||
			signature.ChecksumFile != "" {
			return errors.New("engineering bootstrap signature is invalid")
		}
		return nil
	}
	if signature.Status != "signed" ||
		signature.Kind == "none" ||
		signature.Identity == "" ||
		signature.ChecksumFile == "" {
		return errors.New("official bootstrap signature is invalid")
	}
	return nil
}

func validateSetupBootstrapControllerKeys(
	result setupBootstrapControllerKeys,
) error {
	if result.SchemaVersion != hostbootstrap.SchemaVersion {
		return errors.New("bootstrap controller-key schema is invalid")
	}
	seen := map[string]bool{}
	for _, identity := range result.Keys {
		material, err := base64.StdEncoding.Strict().DecodeString(
			identity.Material,
		)
		if err != nil {
			return errors.New("bootstrap controller public key is invalid")
		}
		sum := sha256.Sum256(material)
		if bootstrapPublicKeyAlgorithm(material) != string(identity.Algorithm) ||
			hex.EncodeToString(sum[:]) != identity.FingerprintSHA256 ||
			seen[identity.FingerprintSHA256] {
			return errors.New("bootstrap controller public key is invalid")
		}
		seen[identity.FingerprintSHA256] = true
	}
	return nil
}

func bootstrapPublicKeyAlgorithm(material []byte) string {
	if len(material) < 4 {
		return ""
	}
	size := int(binary.BigEndian.Uint32(material[:4]))
	if size < 1 || size > 64 || len(material) < 4+size {
		return ""
	}
	return string(material[4 : 4+size])
}
