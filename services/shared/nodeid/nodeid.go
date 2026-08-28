// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nodeid resolves a stable, per-host node UUID that every nvpair
// subprocess can advertise so peers can tell two hosts apart even when they
// share a hostname (the mDNS instance name defaults to os.Hostname(), so a
// hostname collision otherwise silently merges two machines into one node).
//
// It reuses the nvpair-cluster-manager's persisted cryptographic identity
// (<config>/cluster/identity.json) when present, so a host presents one UUID
// across all of its services. When the cluster manager hasn't run, it mints
// and persists its own <config>/node-id.json. Resolution never fails the
// caller: on any I/O error it returns a fresh ephemeral UUID (empty only if
// the system CSPRNG is unavailable), so advertising still proceeds.
package nodeid

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"nvpair-shared/appdir"
)

type idFile struct {
	NodeUUID  string `json:"node_uuid"`
	CreatedAt int64  `json:"created_at"`
}

// Resolve returns a stable per-host node UUID. baseDir overrides the base
// config directory (empty selects the shared default, matching the other
// subprocesses). It prefers the cluster-manager's identity.json UUID, then its
// own persisted node-id.json, and otherwise mints and persists a new UUID.
func Resolve(baseDir string) string {
	base := baseDir
	if base == "" {
		base = defaultBaseDir()
	}

	// Prefer the cluster-manager's persisted principal so a host has one
	// identity everywhere.
	if u := readUUID(filepath.Join(base, "cluster", "identity.json")); u != "" {
		return u
	}

	idPath := filepath.Join(base, "node-id.json")
	if u := readUUID(idPath); u != "" {
		return u
	}

	u, err := newUUIDv4()
	if err != nil {
		return ""
	}
	_ = persist(idPath, u) // best effort; an unpersisted UUID is still usable for this run
	return u
}

func readUUID(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var f idFile
	if err := json.Unmarshal(data, &f); err != nil {
		return ""
	}
	return f.NodeUUID
}

func persist(path, uuid string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(idFile{NodeUUID: uuid, CreatedAt: time.Now().UnixMilli()}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func defaultBaseDir() string {
	if dir, err := appdir.Dir(); err == nil {
		return dir
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

// newUUIDv4 generates a random RFC 4122 version-4 UUID without an external
// dependency (mirrors nvpair-cluster-manager's newUUIDv4).
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
