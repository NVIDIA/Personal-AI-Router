// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
)

const diagnosticPackageLegacyRecipe = "dgx-spark-ubuntu24.04-arm64-openmpi4-packages-v3"
const diagnosticPackageUpgradeRecipe = "dgx-spark-ubuntu24.04-arm64-openmpi4-packages-v4"

// This is a closed dependency patch policy for the fixed OpenMPI root, not an
// operating-system upgrade selector. Native APT still proves the exact closure.
var diagnosticDependencyUpgradeTransitions = map[string][2]string{
	"cpp-13":                   {"13.3.0-6ubuntu2~24.04", "13.3.0-6ubuntu2~24.04.1"},
	"cpp-13-aarch64-linux-gnu": {"13.3.0-6ubuntu2~24.04", "13.3.0-6ubuntu2~24.04.1"},
	"g++-13":                   {"13.3.0-6ubuntu2~24.04", "13.3.0-6ubuntu2~24.04.1"},
	"g++-13-aarch64-linux-gnu": {"13.3.0-6ubuntu2~24.04", "13.3.0-6ubuntu2~24.04.1"},
	"gcc-13":                   {"13.3.0-6ubuntu2~24.04", "13.3.0-6ubuntu2~24.04.1"},
	"gcc-13-aarch64-linux-gnu": {"13.3.0-6ubuntu2~24.04", "13.3.0-6ubuntu2~24.04.1"},
	"gcc-13-base":              {"13.3.0-6ubuntu2~24.04", "13.3.0-6ubuntu2~24.04.1"},
	"libgcc-13-dev":            {"13.3.0-6ubuntu2~24.04", "13.3.0-6ubuntu2~24.04.1"},
	"libobjc-13-dev":           {"13.3.0-6ubuntu2~24.04", "13.3.0-6ubuntu2~24.04.1"},
	"libstdc++-13-dev":         {"13.3.0-6ubuntu2~24.04", "13.3.0-6ubuntu2~24.04.1"},
	"libevent-2.1-7t64":        {"2.1.12-stable-9ubuntu2", "2.1.12-stable-9ubuntu2.1"},
	"libevent-core-2.1-7t64":   {"2.1.12-stable-9ubuntu2", "2.1.12-stable-9ubuntu2.1"},
	"libnuma1":                 {"2.0.18-1build1", "2.0.18-1ubuntu0.24.04.1"},
	"numactl":                  {"2.0.18-1build1", "2.0.18-1ubuntu0.24.04.1"},
}

// The other packages in the same captured OpenMPI dependency closure. This
// keeps the opt-in from authorizing an unrelated new dependency or candidate.
var diagnosticDependencyNewCandidates = map[string][2]string{
	"gfortran-13":                   {"13.3.0-6ubuntu2~24.04.1", "arm64"},
	"gfortran-13-aarch64-linux-gnu": {"13.3.0-6ubuntu2~24.04.1", "arm64"},
	"libevent-dev":                  {"2.1.12-stable-9ubuntu2.1", "arm64"},
	"libevent-extra-2.1-7t64":       {"2.1.12-stable-9ubuntu2.1", "arm64"},
	"libevent-openssl-2.1-7t64":     {"2.1.12-stable-9ubuntu2.1", "arm64"},
	"libevent-pthreads-2.1-7t64":    {"2.1.12-stable-9ubuntu2.1", "arm64"},
	"libfabric1":                    {"1.17.0-3build2", "arm64"},
	"libgfortran-13-dev":            {"13.3.0-6ubuntu2~24.04.1", "arm64"},
	"libhwloc-dev":                  {"2.10.0-1build1", "arm64"},
	"libhwloc-plugins":              {"2.10.0-1build1", "arm64"},
	"libhwloc15":                    {"2.10.0-1build1", "arm64"},
	"libjs-jquery-ui":               {"1.13.2+dfsg-1", "all"},
	"libmunge2":                     {"0.5.15-4ubuntu0.1", "arm64"},
	"libnuma-dev":                   {"2.0.18-1ubuntu0.24.04.1", "arm64"},
	"libopenmpi-dev":                {"4.1.6-7ubuntu2", "arm64"},
	"libopenmpi3t64":                {"4.1.6-7ubuntu2", "arm64"},
	"libpmix-dev":                   {"5.0.1-4.1build1", "arm64"},
	"libpmix2t64":                   {"5.0.1-4.1build1", "arm64"},
	"openmpi-bin":                   {"4.1.6-7ubuntu2", "arm64"},
	"openmpi-common":                {"4.1.6-7ubuntu2", "all"},
}

type diagnosticDependencyUpgradeApproval struct {
	NodeID     string `json:"nodeId"`
	PlanDigest string `json:"planDigest"`
}

func diagnosticPackagePlanUpgrades(plan diagnosticApprovedPackagePlan) (bool, error) {
	upgrades := 0
	seen := map[string]bool{}
	for _, entry := range plan.Packages {
		old, present := entry["installedVersion"]
		if plan.RecipeID == diagnosticPackageLegacyRecipe || plan.RecipeID == diagnosticPythonPackageRecipe {
			if present {
				return false, errors.New("legacy package plans cannot authorize dependency upgrades")
			}
			continue
		}
		if !present {
			return false, errors.New("dependency package review must name each existing version or new install")
		}
		name, nameOK := entry["name"].(string)
		candidate, candidateOK := entry["version"].(string)
		architecture, architectureOK := entry["architecture"].(string)
		transition, upgradeAllowed := diagnosticDependencyUpgradeTransitions[name]
		expected, installAllowed := diagnosticDependencyNewCandidates[name]
		if upgradeAllowed {
			expected = [2]string{transition[1], "arm64"}
		}
		if !nameOK || !candidateOK || !architectureOK || seen[name] || !upgradeAllowed && !installAllowed || expected != [2]string{candidate, architecture} {
			return false, errors.New("dependency candidate is outside the fixed reviewed OpenMPI closure")
		}
		seen[name] = true
		if old == nil {
			continue
		}
		oldVersion, oldOK := old.(string)
		if !oldOK || !upgradeAllowed || transition != [2]string{oldVersion, candidate} {
			return false, errors.New("dependency upgrade is outside the fixed reviewed Ubuntu patch policy")
		}
		upgrades++
	}
	if upgrades > 14 {
		return false, errors.New("dependency upgrade count exceeds the fixed policy")
	}
	return upgrades != 0, nil
}

func diagnosticRequiredUpgradeApprovals(binding diagnosticPackageBinding) ([]diagnosticDependencyUpgradeApproval, error) {
	required := []diagnosticDependencyUpgradeApproval{}
	for _, target := range binding.Review.Targets {
		if target.State == "already_installed" {
			continue
		}
		result, err := packageResult(target.Review)
		if err != nil {
			return nil, err
		}
		plan, err := approvedPackagePlan(result.Plan)
		if err != nil {
			return nil, err
		}
		upgrades, err := diagnosticPackagePlanUpgrades(plan)
		if err != nil {
			return nil, err
		}
		if upgrades {
			required = append(required, diagnosticDependencyUpgradeApproval{NodeID: target.NodeID, PlanDigest: plan.PlanDigest})
		}
	}
	return required, nil
}

func validateDiagnosticUpgradeApprovals(binding diagnosticPackageBinding, approvals []diagnosticDependencyUpgradeApproval) error {
	required, err := diagnosticRequiredUpgradeApprovals(binding)
	if err != nil {
		return err
	}
	if len(approvals) != len(required) {
		return errors.New("explicit additional approval is required for every reviewed dependency-upgrade plan")
	}
	seen := map[string]bool{}
	for _, approval := range approvals {
		if !diagnosticToken.MatchString(approval.NodeID) || !diagnosticDigest.MatchString(approval.PlanDigest) || seen[approval.NodeID] {
			return errors.New("dependency upgrade approval is invalid or duplicated")
		}
		seen[approval.NodeID] = true
		found := false
		for _, expected := range required {
			found = found || approval == expected
		}
		if !found {
			return errors.New("dependency upgrade approval differs from its reviewed participant and plan")
		}
	}
	return nil
}

func diagnosticPackageRunUpgradeApprovalsValid(run diagnosticPackageRun) bool {
	// Do not reinterpret historical v3 records. New v4 records must retain the
	// exact consent that was checked before their first durable publication.
	v4 := run.Binding.Review.DependencyUpgradeReview
	for _, target := range run.Binding.Review.Targets {
		var review struct {
			Plan struct {
				RecipeID string `json:"recipeId"`
			} `json:"plan"`
		}
		if json.Unmarshal(target.Review, &review) == nil && review.Plan.RecipeID == diagnosticPackageUpgradeRecipe {
			v4 = true
		}
	}
	if !v4 && len(run.DependencyUpgradeApprovals) == 0 {
		return true
	}
	return validateDiagnosticUpgradeApprovals(run.Binding, run.DependencyUpgradeApprovals) == nil
}
