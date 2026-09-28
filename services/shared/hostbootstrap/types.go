// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hostbootstrap

import "errors"

// SchemaVersion is the only accepted host bootstrap contract schema.
const SchemaVersion = 1

const (
	maxPayloadBytes     = 64 << 10
	maxAccountNameBytes = 64
)

var (
	// ErrInvalid reports a malformed or internally inconsistent contract.
	ErrInvalid     = errors.New("invalid host bootstrap contract")
	// ErrProhibited reports a secret-bearing or command-shaped JSON field.
	ErrProhibited  = errors.New("host bootstrap contract contains prohibited content")
	// ErrTooLarge reports a payload or identity field above its fixed limit.
	ErrTooLarge    = errors.New("host bootstrap contract exceeds a size limit")
	// ErrTransition reports a phase change outside the closed state machine.
	ErrTransition  = errors.New("host bootstrap phase transition is not allowed")
)

// Platform is a supported target operating system.
type Platform string

const (
	PlatformWindows Platform = "windows"
	PlatformDarwin  Platform = "darwin"
	PlatformLinux   Platform = "linux"
)

// Architecture is a supported target processor architecture.
type Architecture string

const (
	ArchitectureAMD64 Architecture = "amd64"
	ArchitectureARM64 Architecture = "arm64"
)

// Role is the requested PAIR runtime role or its resolved owner.
type Role string

const (
	RoleAuto     Role = "auto"
	RoleDesktop  Role = "desktop"
	RoleHeadless Role = "headless"
)

// Lane selects the user-reviewed or unattended bootstrap flow.
type Lane string

const (
	LaneQuickConnect Lane = "quick-connect"
	LaneZeroTouch    Lane = "zero-touch"
)

// Phase is one state in a bootstrap operation.
type Phase string

const (
	PhaseInspect  Phase = "inspect"
	PhaseReview   Phase = "review"
	PhaseApply    Phase = "apply"
	PhaseVerify   Phase = "verify"
	PhaseComplete Phase = "complete"
	PhaseBlocked  Phase = "blocked"
)

// Decision is the reconciliation result for a reviewed plan.
type Decision string

const (
	DecisionApply         Decision = "apply"
	DecisionRepairOwned   Decision = "repair-owned"
	DecisionNoOp          Decision = "no-op"
	DecisionRefuseForeign Decision = "refuse-foreign"
)

// Ownership classifies a target resource without transferring ownership.
type Ownership string

const (
	OwnershipAbsent        Ownership = "absent"
	OwnershipOwned         Ownership = "owned"
	OwnershipForeign       Ownership = "foreign"
	OwnershipNotApplicable Ownership = "not-applicable"
	OwnershipUnavailable   Ownership = "unavailable"
)

// PublicKeyAlgorithm is an accepted SSH public-key wire algorithm.
type PublicKeyAlgorithm string

const (
	PublicKeyAlgorithmED25519 PublicKeyAlgorithm = "ssh-ed25519"
	PublicKeyAlgorithmRSA     PublicKeyAlgorithm = "ssh-rsa"
	PublicKeyAlgorithmECDSA   PublicKeyAlgorithm = "ecdsa-sha2-nistp256"
)

// ResourceKind identifies a fixed bootstrap resource, never an executable step.
type ResourceKind string

const (
	ResourceSSHService    ResourceKind = "ssh-service"
	ResourceFirewall      ResourceKind = "firewall"
	ResourceAuthorizedKey ResourceKind = "authorized-key"
	ResourceHelper        ResourceKind = "helper"
	ResourceProduct       ResourceKind = "pair-artifact"
	ResourceActiveRole    ResourceKind = "active-role"
)

// Target selects one member of the closed platform and architecture matrix.
type Target struct {
	Platform     Platform     `json:"platform"`
	Architecture Architecture `json:"architecture"`
}

// AccountIdentity binds the target account and its canonical key paths.
type AccountIdentity struct {
	Name               string `json:"name"`
	HomePath           string `json:"homePath"`
	AuthorizedKeysPath string `json:"authorizedKeysPath"`
}

// PublicKeyIdentity carries public SSH key material and its verified digest.
type PublicKeyIdentity struct {
	Algorithm         PublicKeyAlgorithm `json:"algorithm"`
	Material          string             `json:"material"`
	FingerprintSHA256 string             `json:"fingerprintSha256"`
}

// SSHEndpoint is the desired canonical SSH address and port.
type SSHEndpoint struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
}

// ArtifactIdentity binds an artifact to its version, digest, and target path.
type ArtifactIdentity struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	Path    string `json:"path"`
}

// Binding is the immutable identity set shared by every operation document.
type Binding struct {
	Target        Target            `json:"target"`
	Lane          Lane              `json:"lane"`
	Role          Role              `json:"role"`
	RuntimeOwner  Role              `json:"runtimeOwner"`
	Account       AccountIdentity   `json:"account"`
	ControllerKey PublicKeyIdentity `json:"controllerKey"`
	Endpoint      SSHEndpoint       `json:"endpoint"`
	Product       ArtifactIdentity  `json:"product"`
	Helper        ArtifactIdentity  `json:"helper"`
}

// Request declares one bootstrap operation and its desired identities.
type Request struct {
	SchemaVersion int     `json:"schemaVersion"`
	OperationID   string  `json:"operationId"`
	Binding       Binding `json:"binding"`
}

// EndpointObservation records ownership of an SSH-facing endpoint resource.
type EndpointObservation struct {
	Ownership Ownership   `json:"ownership"`
	Endpoint  *SSHEndpoint `json:"endpoint"`
}

// AuthorizedKeyIdentity identifies one target authorized-key entry.
type AuthorizedKeyIdentity struct {
	AccountName       string `json:"accountName"`
	Path              string `json:"path"`
	FingerprintSHA256 string `json:"fingerprintSha256"`
}

// AuthorizedKeyObservation records ownership of an authorized-key entry.
type AuthorizedKeyObservation struct {
	Ownership Ownership             `json:"ownership"`
	Identity  *AuthorizedKeyIdentity `json:"identity"`
}

// ArtifactObservation records ownership of an installed artifact.
type ArtifactObservation struct {
	Ownership Ownership        `json:"ownership"`
	Identity  *ArtifactIdentity `json:"identity"`
}

// RuntimeOwnerObservation records the one active PAIR runtime owner.
type RuntimeOwnerObservation struct {
	Ownership Ownership `json:"ownership"`
	Owner     Role      `json:"owner"`
}

// Observations is the complete target-local ownership snapshot.
type Observations struct {
	SSHService    EndpointObservation       `json:"sshService"`
	Firewall      EndpointObservation       `json:"firewall"`
	AuthorizedKey AuthorizedKeyObservation  `json:"authorizedKey"`
	Helper        ArtifactObservation       `json:"helper"`
	Product       ArtifactObservation       `json:"product"`
	RuntimeOwners []RuntimeOwnerObservation `json:"runtimeOwners"`
}

// Plan is the deterministic review result for a request and observation set.
type Plan struct {
	SchemaVersion int            `json:"schemaVersion"`
	OperationID   string         `json:"operationId"`
	Phase         Phase          `json:"phase"`
	Decision      Decision       `json:"decision"`
	Binding       Binding        `json:"binding"`
	Observed      Observations   `json:"observed"`
	Actions       []ResourceKind `json:"actions"`
}

// Receipt seals an exact verified state at the complete phase.
type Receipt struct {
	SchemaVersion int          `json:"schemaVersion"`
	OperationID   string       `json:"operationId"`
	Phase         Phase        `json:"phase"`
	Decision      Decision     `json:"decision"`
	Binding       Binding      `json:"binding"`
	Verified      Observations `json:"verified"`
}

// Status is a validated snapshot used by the phase transition validator.
type Status struct {
	SchemaVersion int          `json:"schemaVersion"`
	OperationID   string       `json:"operationId"`
	Phase         Phase        `json:"phase"`
	Decision      Decision     `json:"decision,omitempty"`
	Binding       Binding      `json:"binding"`
	Observed      Observations `json:"observed"`
}
