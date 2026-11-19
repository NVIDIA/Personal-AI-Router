// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package modelrequest carries model preparation between the inference proxy
// and engine manager through their supervising broker. It carries no inference
// request or response content.
package modelrequest

import "time"

const (
	Request         = "model:prepare"
	Cancel          = "model:cancel"
	ReplyMethod     = "model:prepared"
	EngineRequest   = "engine:prepare-model"
	EngineCancel    = "engine:cancel-model-preparation"
	EngineReply     = "engine:model-prepared"
	Reset           = "model:worker-reset"
	EngineCancelAll = "engine:cancel-all-model-preparations"
	Budget          = 10 * time.Minute
)

type Preparation struct {
	ID     string `json:"id"`
	Engine string `json:"engine"`
	Model  string `json:"model"`
}

type Reply struct {
	ID     string `json:"id"`
	Engine string `json:"engine"`
	Port   int    `json:"port,omitempty"`
	Error  string `json:"error,omitempty"`
	Status int    `json:"status,omitempty"`
}

// LocalInventory projects cached loader capability and ON/OFF intent under the
// same stable identity used in discovery. Nil Models is a lifecycle-only update;
// an explicit empty slice clears the cached capability set.
type LocalInventory struct {
	Enabled    bool     `json:"enabled"`
	EnginePort int      `json:"enginePort"`
	Engine     string   `json:"engine"`
	ID         string   `json:"id"`
	Models     []string `json:"models"`
}
