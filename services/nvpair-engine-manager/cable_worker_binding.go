// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"

	"nvpair-shared/clustertrust"
)

func cableWorkerCapabilitiesJSON() string {
	return `{"protocol":"` + cableWorkerProtocol + `","maxSeconds":20,"maxPorts":2}`
}

const controlCableWorkerPath = "/v1/cables/worker"

// Private paired-EC attestation of this running worker, never a UI supplied path
// or an installation/privilege grant. It supports existing PAIR installations
// without manufacturing an onboarding history or requiring identical versions.
type cableWorkerBinding struct {
	NodeID            string             `json:"nodeId"`
	Principal         string             `json:"principal"`
	UID               int                `json:"uid"`
	WorkerPath        string             `json:"workerPath"`
	WorkerSHA256      string             `json:"workerSha256"`
	WorkerBytes       int64              `json:"workerBytes"`
	ProfileDir        string             `json:"profileDir"`
	CertificateSHA256 string             `json:"certificateSha256"`
	Protocol          string             `json:"protocol"`
	RequesterAddress  string             `json:"requesterAddress,omitempty"`
	CleanupProtocol   string             `json:"cleanupProtocol,omitempty"`
	CleanupScope      *cableCleanupScope `json:"cleanupScope,omitempty"`
}

func cableExecutableDigest(filename string) (string, int64, error) {
	file, err := os.Open(filename)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > cableWorkerMaxBytes {
		return "", 0, errors.New("worker executable is not a bounded regular file")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, cableWorkerMaxBytes+1))
	if err != nil || n != info.Size() {
		return "", 0, errors.New("worker executable changed while inspected")
	}
	return hex.EncodeToString(hash.Sum(nil)), n, nil
}

func inspectRunningCableWorker(local *cableLocalFacts, mesh *clustertrust.Mesh) (cableWorkerBinding, error) {
	invalid := errors.New("a current Linux worker and its normal account/profile binding are unavailable")
	if runtime.GOOS != "linux" || os.Getuid() <= 0 || local == nil || !cableIdentifier(local.nodeID, 128) || !filepath.IsAbs(local.profileDir) {
		return cableWorkerBinding{}, invalid
	}
	mesh.Refresh()
	principal := mesh.NodeUUID()
	certificate := cableSelfCertificate(mesh, principal)
	if !mesh.Clustered() || len(certificate) == 0 {
		return cableWorkerBinding{}, invalid
	}
	filename, err := os.Executable()
	if err != nil {
		return cableWorkerBinding{}, invalid
	}
	filename, err = filepath.EvalSymlinks(filename)
	if err != nil {
		return cableWorkerBinding{}, invalid
	}
	// /proc/self/exe pins the image actually running, even if its pathname was
	// replaced. The reviewed on-disk file must still contain those exact bytes.
	processHash, processBytes, err := cableExecutableDigest("/proc/self/exe")
	if err != nil {
		return cableWorkerBinding{}, invalid
	}
	diskHash, diskBytes, err := cableExecutableDigest(filename)
	if err != nil || diskHash != processHash || diskBytes != processBytes {
		return cableWorkerBinding{}, invalid
	}
	cleanupScope, cleanupScopeErr := cableCleanupHostScope()
	mesh.Refresh()
	after := cableSelfCertificate(mesh, principal)
	expected := sha256.Sum256(certificate)
	current := sha256.Sum256(after)
	if !mesh.Clustered() || mesh.NodeUUID() != principal || len(after) == 0 || current != expected {
		return cableWorkerBinding{}, invalid
	}
	binding := cableWorkerBinding{NodeID: local.nodeID, Principal: principal, UID: os.Getuid(), WorkerPath: filename, WorkerSHA256: processHash, WorkerBytes: processBytes, ProfileDir: filepath.Clean(local.profileDir), CertificateSHA256: hex.EncodeToString(expected[:]), Protocol: cableWorkerProtocol}
	if cleanupScopeErr == nil {
		binding.CleanupProtocol, binding.CleanupScope = cableCleanupProtocol, cleanupScope
	}
	return binding, nil
}

func (s *controlServer) handleCableWorkerBinding(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	binding, err := inspectRunningCableWorker(s.cableLocal, s.mesh)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	s.mesh.Refresh()
	if _, ok := s.mesh.VerifyClientPin(r); !ok {
		http.Error(w, "paired access changed during worker inspection", http.StatusForbidden)
		return
	}
	if address, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if ip := net.ParseIP(address); ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsMulticast() {
			binding.RequesterAddress = ip.String()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(binding)
}
