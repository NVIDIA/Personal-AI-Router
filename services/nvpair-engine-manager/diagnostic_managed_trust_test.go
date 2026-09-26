// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Synthetic Linux metadata exercises ancestry without claiming Windows UID checks.
type diagnosticManagedTrustInfo struct {
	mode os.FileMode
	uid  int
}

func (i diagnosticManagedTrustInfo) Name() string       { return "fixture" }
func (i diagnosticManagedTrustInfo) Size() int64        { return 4 }
func (i diagnosticManagedTrustInfo) Mode() os.FileMode  { return i.mode }
func (i diagnosticManagedTrustInfo) ModTime() time.Time { return time.Time{} }
func (i diagnosticManagedTrustInfo) IsDir() bool        { return i.mode.IsDir() }
func (i diagnosticManagedTrustInfo) Sys() any           { return i.uid }

func TestDiagnosticManagedTrustRejectsUnsafeArtifactAncestry(t *testing.T) {
	const target = "/home/operator/vendor/bundle/manager"
	for _, mutation := range []string{"none", "group-writable-vendor", "other-writable-bundle", "foreign-home", "redirected-vendor", "redirected-file", "non-directory-parent", "unknown-owner"} {
		t.Run(mutation, func(t *testing.T) {
			entries := map[string]diagnosticManagedTrustInfo{
				"/": {os.ModeDir | 0755, 0}, "/home": {os.ModeDir | 0755, 0},
				"/home/operator":               {os.ModeDir | 0700, 1000},
				"/home/operator/vendor":        {os.ModeDir | 0755, 1000},
				"/home/operator/vendor/bundle": {os.ModeDir | 0755, 1000}, target: {0755, 1000},
			}
			switch mutation {
			case "group-writable-vendor":
				entries["/home/operator/vendor"] = diagnosticManagedTrustInfo{os.ModeDir | 0775, 1000}
			case "other-writable-bundle":
				entries["/home/operator/vendor/bundle"] = diagnosticManagedTrustInfo{os.ModeDir | 0757, 1000}
			case "foreign-home":
				entries["/home/operator"] = diagnosticManagedTrustInfo{os.ModeDir | 0700, 1001}
			case "redirected-vendor":
				entries["/home/operator/vendor"] = diagnosticManagedTrustInfo{os.ModeSymlink | 0777, 0}
			case "redirected-file":
				entries[target] = diagnosticManagedTrustInfo{os.ModeSymlink | 0777, 1000}
			case "non-directory-parent":
				entries["/home/operator/vendor"] = diagnosticManagedTrustInfo{0644, 1000}
			}
			lstat := func(name string) (os.FileInfo, error) {
				info, ok := entries[name]
				if !ok {
					return nil, os.ErrNotExist
				}
				return info, nil
			}
			owner := func(info os.FileInfo) (int, bool) { return info.Sys().(int), mutation != "unknown-owner" }
			resolved, _, err := diagnosticManagedTrustPath(target, 1000, lstat, func(string) (string, error) { t.Fatal("artifact path followed a link"); return "", nil }, owner)
			if mutation == "none" {
				if err != nil || resolved != target {
					t.Fatal("trusted operator artifact rejected", err)
				}
			} else if !errors.Is(err, errDiagnosticManagedTrust) || strings.Contains(err.Error(), target) {
				t.Fatalf("unsafe ancestry did not return fixed safe guidance: %v", err)
			}
		})
	}
}

func TestDiagnosticManagedTrustSystemLinksStayRootOwnedAndBounded(t *testing.T) {
	for _, mutation := range []string{"none", "link-owner", "target-owner", "writable-target-parent", "loop", "not-catalogued"} {
		t.Run(mutation, func(t *testing.T) {
			entries := map[string]diagnosticManagedTrustInfo{
				"/": {os.ModeDir | 0755, 0}, "/usr": {os.ModeDir | 0755, 0}, "/usr/bin": {os.ModeDir | 0755, 0},
				"/etc": {os.ModeDir | 0755, 0}, "/etc/alternatives": {os.ModeDir | 0755, 0},
				"/usr/bin/mpirun": {os.ModeSymlink | 0777, 0}, "/etc/alternatives/mpirun": {os.ModeSymlink | 0777, 0},
				"/usr/bin/mpirun.openmpi": {0755, 0}, "/usr/bin/unreviewed": {os.ModeSymlink | 0777, 0},
			}
			links := map[string]string{"/usr/bin/mpirun": "/etc/alternatives/mpirun", "/etc/alternatives/mpirun": "../../usr/bin/mpirun.openmpi"}
			target := "/usr/bin/mpirun"
			switch mutation {
			case "link-owner":
				entries[target] = diagnosticManagedTrustInfo{os.ModeSymlink | 0777, 1000}
			case "target-owner":
				entries["/usr/bin/mpirun.openmpi"] = diagnosticManagedTrustInfo{0755, 1000}
			case "writable-target-parent":
				entries["/etc/alternatives"] = diagnosticManagedTrustInfo{os.ModeDir | 0775, 0}
			case "loop":
				links[target] = target
			case "not-catalogued":
				target = "/usr/bin/unreviewed"
			}
			linkReads := 0
			lstat := func(name string) (os.FileInfo, error) {
				info, ok := entries[name]
				if !ok {
					return nil, os.ErrNotExist
				}
				return info, nil
			}
			readlink := func(name string) (string, error) { linkReads++; return links[name], nil }
			resolved, _, err := diagnosticManagedTrustPath(target, 1000, lstat, readlink, func(info os.FileInfo) (int, bool) { return info.Sys().(int), true })
			if mutation == "none" {
				if err != nil || resolved != "/usr/bin/mpirun.openmpi" || linkReads != 2 {
					t.Fatal("trusted system alternatives rejected", err)
				}
			} else if !errors.Is(err, errDiagnosticManagedTrust) {
				t.Fatal("unsafe system tool accepted", err)
			}
			if linkReads > 20 {
				t.Fatal("system link resolution exceeded its fixed budget")
			}
		})
	}
}

func TestDiagnosticManagedTrustDoesNotInventNonLinuxPermissionProof(t *testing.T) {
	if runtime.GOOS == "linux" {
		return
	}
	err := verifyDiagnosticManagedTool(diagnosticTool{Path: "/fixture/manager", SHA256: strings.Repeat("a", 64)})
	if !errors.Is(err, errDiagnosticManagedTrustPlatform) {
		t.Fatal("non-Linux managed trust admission was not refused", err)
	}
	if _, err := readDiagnosticMPIArtifact("/fixture/manager"); !errors.Is(err, errDiagnosticManagedTrustPlatform) {
		t.Fatal("managed Review bypassed the shared platform trust policy", err)
	}
}
