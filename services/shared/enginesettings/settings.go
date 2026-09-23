// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package enginesettings is the node-owned engine settings wire contract.
package enginesettings

type Config struct {
	ServerPort int `json:"serverPort"`
	ProxyPort  int `json:"proxyPort"`
	// Arguments and leading environment assignments; excludes the executable
	// and manifest-owned startup subcommand. Snapshot.Format identifies syntax.
	LaunchText string `json:"launchText"`
}

type Request struct {
	NodeID           string `json:"nodeId,omitempty"`
	Engine           string `json:"engine"`
	ExpectedRevision uint64 `json:"expectedRevision"`
	RequestID        string `json:"requestId,omitempty"`
	Settings         Config `json:"settings"`
	Resolution       string `json:"resolution,omitempty"`
	// Format is supplied only when migrating a saved full-command journal.
	Format string `json:"format,omitempty"`
	// Internal preview guard, overwritten by the broker from the authenticated
	// caller. Client input cannot disable the target's local-only CORS policy.
	PreserveCORS bool `json:"preserveCORS,omitempty"`
}

type Snapshot struct {
	NodeID              string `json:"nodeId"`
	Engine              string `json:"engine"`
	Revision            uint64 `json:"revision"`
	AppliedRevision     uint64 `json:"appliedRevision"`
	Sequence            uint64 `json:"sequence"`
	Epoch               string `json:"epoch"`
	Settings            Config `json:"settings"`
	EffectiveServerPort int    `json:"effectiveServerPort"`
	EffectiveProxyPort  int    `json:"effectiveProxyPort"`
	Running             bool   `json:"running"`
	Adopted             bool   `json:"adopted"`
	Editable            bool   `json:"editable"`
	Reason              string `json:"reason,omitempty"`
	Format              string `json:"format"`
	Phase               string `json:"phase"`
	Error               string `json:"error,omitempty"`
	RequestID           string `json:"requestId,omitempty"`
}

type Conflict struct {
	ServerPort int `json:"serverPort"`
	LaunchPort int `json:"launchPort"`
}

type Preview struct {
	Settings Config            `json:"settings"`
	Revision uint64            `json:"revision"`
	Conflict *Conflict         `json:"conflict,omitempty"`
	Errors   map[string]string `json:"errors,omitempty"`
	Restart  bool              `json:"restart"`
	Rebind   bool              `json:"rebind"`
	// Named to match Runtime's persisted override keys: these carry the same
	// user-supplied launch content, and a log sink redacts them by key. Plain
	// "args"/"env" would collide with every worker-spawn diagnostic.
	Args []string `json:"launch_args"`
	Env  []string `json:"launch_env"`
}

type LaunchState struct {
	Engine        string `json:"engine"`
	ServerPort    int    `json:"serverPort"`
	EffectivePort int    `json:"effectivePort"`
	LaunchText    string `json:"launchText"`
	Running       bool   `json:"running"`
	Adopted       bool   `json:"adopted"`
	Editable      bool   `json:"editable"`
	Reason        string `json:"reason,omitempty"`
	Format        string `json:"format"`
}

type Configure struct {
	Engine      string `json:"engine"`
	Settings    Config `json:"settings"`
	OperationID string `json:"operationId"`
	Resume      bool   `json:"resume,omitempty"`
}

// Relay is a correlated worker-to-parent request. It never blocks a reader.
type Relay struct {
	ID      string  `json:"id"`
	Method  string  `json:"method"`
	Caller  string  `json:"caller,omitempty"`
	Request Request `json:"request"`
}
