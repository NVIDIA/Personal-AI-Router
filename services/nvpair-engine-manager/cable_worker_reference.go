// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"runtime"
	"strings"
)

// A remote report cannot nominate new root-executable bytes. The digest must
// match this controller's actual running image, or a package already admitted
// by the existing PAIR artifact owner. This permits different approved builds
// across hosts; matching hashes never identify duplicate physical hosts.
func (s *cableProductService) matchKnownWorker(ctx context.Context, plan cableLaunchPlan) (string, error) {
	platform, arch, err := onboardingPlatform(plan.Info.OS, plan.Info.Arch)
	if err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if runtime.GOOS == platform && runtime.GOARCH == arch {
		digest, size, e := cableExecutableDigest("/proc/self/exe")
		if e == nil && strings.EqualFold(digest, plan.WorkerSHA256) && size == plan.WorkerBytes {
			return "controller-running-image:" + digest, nil
		}
	}
	catalog, err := readOnboardingCatalog(s.m.exec.baseDir)
	if err != nil {
		return "", err
	}
	s.m.onboarding.mu.Lock()
	for _, source := range s.m.onboarding.imports {
		catalog.Artifacts = append(catalog.Artifacts, source)
	}
	s.m.onboarding.mu.Unlock()
	for _, source := range catalog.Artifacts {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		if source.File == "" || source.Platform != platform || source.Arch != arch {
			continue
		}
		if _, _, e := verifyOnboardingArchive(source.File, source.onboardingArtifact); e != nil {
			continue
		}
		manifest, e := onboardingArchiveManifest(source.File)
		if e != nil {
			continue
		}
		for _, file := range manifest.Files {
			if file.FileName == "nvpair-engine-manager" && strings.EqualFold(file.SHA256, plan.WorkerSHA256) && file.Size == plan.WorkerBytes {
				return "verified-package:" + strings.ToLower(source.SHA256), nil
			}
		}
	}
	return "", errors.New("installed worker bytes lack an existing trusted PAIR reference on this controller")
}
