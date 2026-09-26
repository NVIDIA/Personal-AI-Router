// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/hex"
	"strings"

	"nvpair-shared/cableprobe"
)

const fabricRecipe = "spark-two-node-temporary-addresses-v1"
const fabricRingRecipe = "spark-three-node-ring-temporary-addresses-v1"
const fabricLeaseSeconds = 0 // Until explicit rollback or reboot; never expires beneath a workload.
const fabricControlPath = "/v1/fabric/control"

type fabricPhysicalPort struct {
	Source   string `json:"source"`
	SwitchID string `json:"switchId"`
	PortName string `json:"portName"`
}
type fabricInterface struct {
	Name             string                  `json:"name"`
	Index            int                     `json:"index"`
	MAC              string                  `json:"mac"`
	PhysicalPort     fabricPhysicalPort      `json:"physicalPort"`
	Addresses        []string                `json:"addresses"`
	Address          string                  `json:"address"`
	Driver           string                  `json:"driver"`
	RDMADevices      []string                `json:"rdmaDevices"`
	MTU              int                     `json:"mtu"`
	GeneratedDefault *fabricGeneratedDefault `json:"generatedDefault,omitempty"`
}

// Native inspection supplies this exact selected-device binding before pause
// consent. Its presence describes the candidate; it does not grant effects.
type fabricGeneratedDefault struct {
	SchemaVersion       int    `json:"schemaVersion"`
	InterfaceName       string `json:"interfaceName"`
	Index               int    `json:"index"`
	Owner               string `json:"owner"`
	BusID               string `json:"busId"`
	DevicePath          string `json:"devicePath"`
	PermanentMAC        string `json:"permanentMAC"`
	OriginalAutoconnect *bool  `json:"originalAutoconnect"`
	UUID                string `json:"uuid"`
	Name                string `json:"name"`
	SettingsPath        string `json:"settingsPath"`
	ProfileDigest       string `json:"profileDigest"`
	ActivePath          string `json:"activePath"`
}

func validFabricGeneratedDefault(d *fabricGeneratedDefault, iface fabricInterface) bool {
	if d == nil {
		return false
	}
	_, busErr := hex.DecodeString(d.BusID)
	_, digestErr := hex.DecodeString(d.ProfileDigest)
	return d.SchemaVersion == 1 && d.InterfaceName == iface.Name && d.Index == iface.Index &&
		strings.HasPrefix(d.Owner, ":") && len(d.Owner) <= 128 && !strings.ContainsAny(d.Owner, " /\\\r\n") &&
		len(d.BusID) == 32 && busErr == nil && fabricNMObject(d.DevicePath, "Devices") &&
		strings.EqualFold(d.PermanentMAC, iface.MAC) && d.OriginalAutoconnect != nil &&
		fabricNMValidUUID(d.UUID) && cableIdentifier(d.Name, 128) && fabricNMObject(d.SettingsPath, "Settings") &&
		len(d.ProfileDigest) == 64 && digestErr == nil && (d.ActivePath == "" || fabricNMObject(d.ActivePath, "ActiveConnection"))
}

type fabricNativeFacts struct {
	Digest            string                   `json:"digest"`
	Routes            []string                 `json:"routes"`
	Blockers          []string                 `json:"blockers"`
	GeneratedDefaults []fabricGeneratedDefault `json:"generatedDefaults,omitempty"`
}
type fabricTarget struct {
	NodeID     string             `json:"nodeId"`
	Principal  string             `json:"principal"`
	SwitchID   string             `json:"switchId,omitempty"`
	PortName   string             `json:"portName,omitempty"`
	Ports      []fabricTargetPort `json:"ports,omitempty"`
	Interfaces []fabricInterface  `json:"interfaces"`
}
type fabricTargetPort struct {
	SwitchID string `json:"switchId"`
	PortName string `json:"portName"`
}
type fabricReview struct {
	SchemaVersion       int                          `json:"schemaVersion"`
	ReviewID            string                       `json:"reviewId"`
	OwnerNodeID         string                       `json:"ownerNodeId"`
	RecipeID            string                       `json:"recipeId"`
	CableRunID          string                       `json:"cableRunId,omitempty"`
	Persistence         string                       `json:"persistence"`
	State               string                       `json:"state"`
	Executable          bool                         `json:"executable"`
	RemainingMs         int64                        `json:"remainingMs"`
	Targets             []fabricTarget               `json:"targets"`
	Blockers            []string                     `json:"blockers"`
	EffectsApplied      bool                         `json:"effectsApplied"`
	Permission          *cableprobe.PermissionReview `json:"permission,omitempty"`
	InspectionRequired  bool                         `json:"inspectionRequired,omitempty"`
	InspectionAvailable bool                         `json:"inspectionAvailable,omitempty"`
}
type fabricOperation struct {
	Failure             *fabricFailure               `json:"failure,omitempty"`
	Permission          *cableprobe.PermissionReview `json:"permission,omitempty"`
	SchemaVersion       int                          `json:"schemaVersion"`
	OperationID         string                       `json:"operationId"`
	ReviewID            string                       `json:"reviewId"`
	OwnerNodeID         string                       `json:"ownerNodeId"`
	RecipeID            string                       `json:"recipeId,omitempty"`
	CableRunID          string                       `json:"cableRunId,omitempty"`
	State               string                       `json:"state"`
	Targets             []fabricTarget               `json:"targets"`
	CleanupConfirmed    bool                         `json:"cleanupConfirmed"`
	EffectsApplied      bool                         `json:"effectsApplied"`
	EffectsUnconfirmed  bool                         `json:"effectsUnconfirmed,omitempty"`
	Message             string                       `json:"message"`
	CreatedAt           int64                        `json:"createdAt"`
	ExpiresAt           int64                        `json:"expiresAt"`
	QualifiedAt         int64                        `json:"qualifiedAt,omitempty"`
	QualificationDigest string                       `json:"qualificationDigest,omitempty"`
	CandidateIPs        []fabricCandidateIP          `json:"candidateIPs,omitempty"`
}

// CandidateIPs are published only after the existing fabric owner proves the
// exact local route and a certificate-pinned identity read in both directions.
// They are peer-specific; a ring has no single fabric address reachable by all
// three members.
type fabricCandidateIP struct {
	NodeID         string `json:"nodeId"`
	PeerNodeID     string `json:"peerNodeId"`
	PeerPrincipal  string `json:"peerPrincipal"`
	Address        string `json:"address"`
	PeerAddress    string `json:"peerAddress"`
	InterfaceName  string `json:"interfaceName"`
	InterfaceIndex int    `json:"interfaceIndex"`
	MAC            string `json:"mac"`
	SwitchID       string `json:"switchId"`
	PortName       string `json:"portName"`
	RDMADevice     string `json:"rdmaDevice"`
	RDMAPort       int    `json:"rdmaPort"`
	GIDIndex       int    `json:"gidIndex"`
	GIDType        string `json:"gidType"`
}

type fabricRDMABinding struct {
	Port     int
	GIDIndex int
	GIDType  string
}

// Optional fixed phase diagnostic; historical operations lack this field. It
// carries no command output, native error text or cleanup claim.
type fabricFailure struct {
	NodeID string `json:"nodeId"`
	Phase  string `json:"phase"`
	Code   string `json:"code"`
}
