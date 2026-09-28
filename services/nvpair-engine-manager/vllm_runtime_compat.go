// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
)

type vllmDependencyIdentity struct {
	Name, Version, ContentSHA256 string
}

type vllmCompatibilityABI struct {
	Implementation string
	Python         string
	SOABI          string
	Architecture   string
	GILDisabled    bool
	Libc           string
	Glibc          string
	SeedPip        string
}

type vllmRuntimeCompatibility struct {
	Schema        int
	EngineVersion string
	RecipeID      string
	RecipeSHA256  string
	UVSHA256      string
	WheelSHA256   string
	ABI           vllmCompatibilityABI
	Dependencies  []vllmDependencyIdentity
}

type vllmRuntimeLock struct {
	SourceRuntimeDigest string
	CompatibilitySHA256 string
}

type vllmDependencyReportEntry struct {
	Metadata struct {
		Name, Version string
	} `json:"metadata"`
	Download struct {
		Archive struct {
			Hashes map[string]string `json:"hashes"`
		} `json:"archive_info"`
	} `json:"download_info"`
}

type vllmDependencyReport struct {
	Version    string                      `json:"version"`
	PipVersion string                      `json:"pip_version"`
	Install    []vllmDependencyReportEntry `json:"install"`
}

type vllmManagedDependencyFile struct {
	Path, Hash string
	Size       *int64
}

type vllmManagedDependencyPackage struct {
	Name, Version string
	Files         []vllmManagedDependencyFile
}

type vllmManagedDependencyReport struct {
	Schema   int
	Packages []vllmManagedDependencyPackage
}

type vllmPythonFacts struct {
	Python         string `json:"python"`
	Implementation string `json:"implementation"`
	SOABI          string `json:"soabi"`
	GILDisabled    bool   `json:"gil_disabled"`
	Libc           string `json:"libc"`
	Glibc          string `json:"glibc"`
	Pip            string `json:"pip"`
}

var vllmDependencyName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)
var vllmDependencyVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.!+_-]{0,127}$`)
var vllmDependencySeparators = regexp.MustCompile(`[-_.]+`)

func normalizedVLLMDependencyName(name string) string {
	return strings.ToLower(vllmDependencySeparators.ReplaceAllString(name, "-"))
}

func canonicalVLLMCompatibility(value vllmRuntimeCompatibility) (vllmRuntimeCompatibility, string, error) {
	abi := value.ABI
	archABI := map[string]string{"arm64": "aarch64", "amd64": "x86_64"}[abi.Architecture]
	pythonParts := strings.Split(abi.Python, ".")
	if value.Schema != 1 || len(pythonParts) != 3 || abi.Implementation != "cpython" ||
		abi.GILDisabled || archABI == "" ||
		abi.SOABI != "cpython-"+pythonParts[0]+pythonParts[1]+"-"+archABI+"-linux-gnu" ||
		abi.Libc != "glibc" || abi.Glibc == "" || abi.SeedPip == "" {
		return value, "", errors.New("actual supported CPython ABI and architecture are required")
	}
	if len(value.Dependencies) < 1 || len(value.Dependencies) > 4096 {
		return value, "", errors.New("bounded complete dependency identity is required")
	}
	value.Dependencies = slices.Clone(value.Dependencies)
	seen, foundVLLM := map[string]bool{}, false
	for index := range value.Dependencies {
		dependency := &value.Dependencies[index]
		if !vllmDependencyName.MatchString(dependency.Name) ||
			!vllmDependencyVersion.MatchString(dependency.Version) ||
			!validSHA256(dependency.ContentSHA256) {
			return value, "", errors.New("dependency requires name, exact version and content SHA256")
		}
		dependency.Name = normalizedVLLMDependencyName(dependency.Name)
		if seen[dependency.Name] {
			return value, "", errors.New("duplicate normalized dependency identity")
		}
		seen[dependency.Name] = true
		if dependency.Name == "vllm" {
			if dependency.Version != value.EngineVersion ||
				value.WheelSHA256 != "" && dependency.ContentSHA256 != value.WheelSHA256 {
				return value, "", errors.New("dependency set has a different vLLM archive")
			}
			foundVLLM = true
		}
	}
	if !foundVLLM {
		return value, "", errors.New("complete dependency identity is missing vLLM")
	}
	slices.SortFunc(value.Dependencies, func(a, b vllmDependencyIdentity) int {
		return strings.Compare(a.Name, b.Name)
	})
	raw, err := json.Marshal(value)
	if err != nil {
		return value, "", err
	}
	sum := sha256.Sum256(raw)
	return value, hex.EncodeToString(sum[:]), nil
}

func vllmCompatibilityBase(facts vllmPythonFacts, receipt vllmRuntimeReceipt) vllmRuntimeCompatibility {
	return vllmRuntimeCompatibility{
		Schema: 1, EngineVersion: receipt.Version, RecipeID: receipt.RecipeID,
		RecipeSHA256: receipt.RecipeSHA256, UVSHA256: receipt.UVSHA256,
		WheelSHA256: receipt.WheelSHA256,
		ABI: vllmCompatibilityABI{
			Implementation: facts.Implementation, Python: facts.Python, SOABI: facts.SOABI,
			Architecture: receipt.Architecture, GILDisabled: facts.GILDisabled,
			Libc: facts.Libc, Glibc: facts.Glibc, SeedPip: facts.Pip,
		},
	}
}

func vllmCompatibilityFromLegacyReport(report vllmDependencyReport, facts vllmPythonFacts, receipt vllmRuntimeReceipt) (vllmRuntimeCompatibility, string, error) {
	if report.Version != "1" || report.PipVersion == "" {
		return vllmRuntimeCompatibility{}, "", errors.New("complete pip install report is required")
	}
	value := vllmCompatibilityBase(facts, receipt)
	for _, entry := range report.Install {
		value.Dependencies = append(value.Dependencies, vllmDependencyIdentity{
			Name: entry.Metadata.Name, Version: entry.Metadata.Version,
			ContentSHA256: entry.Download.Archive.Hashes["sha256"],
		})
	}
	return canonicalVLLMCompatibility(value)
}

func vllmCompatibilityFromManagedReport(report vllmManagedDependencyReport, facts vllmPythonFacts, receipt vllmRuntimeReceipt) (vllmRuntimeCompatibility, string, error) {
	if report.Schema != 1 {
		return vllmRuntimeCompatibility{}, "", errors.New("complete managed dependency report is required")
	}
	value := vllmCompatibilityBase(facts, receipt)
	for _, dependency := range report.Packages {
		files := make([]vllmManagedDependencyFile, 0, len(dependency.Files))
		for _, file := range dependency.Files {
			clean := path.Clean(strings.ReplaceAll(file.Path, "\\", "/"))
			parts := strings.Split(clean, "/")
			// Wheel installers rewrite console-script shebangs with the local
			// environment path. They are separately bound by the runtime/CLI
			// receipt and cannot represent cross-node dependency drift.
			if strings.HasPrefix(clean, "../") && len(parts) > 1 && parts[len(parts)-2] == "bin" {
				continue
			}
			files = append(files, file)
		}
		if len(files) == 0 {
			return value, "", errors.New("dependency identity contains no wheel-owned files")
		}
		slices.SortFunc(files, func(a, b vllmManagedDependencyFile) int {
			return strings.Compare(a.Path, b.Path)
		})
		raw, err := json.Marshal(files)
		if err != nil {
			return value, "", err
		}
		sum := sha256.Sum256(raw)
		value.Dependencies = append(value.Dependencies, vllmDependencyIdentity{
			Name: dependency.Name, Version: dependency.Version,
			ContentSHA256: hex.EncodeToString(sum[:]),
		})
	}
	return canonicalVLLMCompatibility(value)
}

const vllmPythonProbeCode = `import ensurepip,json,platform,sys,sysconfig; print(json.dumps(dict(python=platform.python_version(),implementation=sys.implementation.name,soabi=sysconfig.get_config_var('SOABI'),gil_disabled=bool(sysconfig.get_config_var('Py_GIL_DISABLED')),libc=platform.libc_ver()[0],glibc=platform.libc_ver()[1],pip=ensurepip.version()),separators=(',',':')))`

func vllmPythonProbeEnv() []string {
	return []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LANG=C.UTF-8", "PYTHONNOUSERSITE=1", "PYTHONDONTWRITEBYTECODE=1"}
}

func (e *Executor) probeVLLMPython(ctx context.Context, python string) (vllmPythonFacts, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var facts vllmPythonFacts
	out, err := e.runVLLMCommand(ctx, python, []string{"-I", "-B", "-c", vllmPythonProbeCode}, vllmPythonProbeEnv())
	if err != nil || len(out) > 4096 || json.Unmarshal(out, &facts) != nil {
		return facts, errors.New("managed Python ABI facts are unavailable")
	}
	if facts.Python == "" || facts.Implementation == "" || facts.SOABI == "" ||
		facts.Libc == "" || facts.Glibc == "" || facts.Pip == "" {
		return facts, errors.New("managed Python ABI facts are incomplete")
	}
	return facts, nil
}

const vllmInstalledDependencyProbe = `import importlib.metadata,json; print(json.dumps([dict(name=d.metadata['Name'],version=d.version) for d in importlib.metadata.distributions()],separators=(',',':')))`

func (e *Executor) verifyVLLMInstalledDependencySet(ctx context.Context, python string, env []string, value vllmRuntimeCompatibility) error {
	raw, err := e.runVLLMCommand(ctx, python, []string{"-I", "-B", "-c", vllmInstalledDependencyProbe}, env)
	if err != nil {
		return err
	}
	var actual []struct{ Name, Version string }
	if len(raw) > 1<<20 || json.Unmarshal(raw, &actual) != nil {
		return errors.New("actual installed dependency inventory is unavailable")
	}
	expected := map[string]string{}
	for _, dependency := range value.Dependencies {
		expected[dependency.Name] = dependency.Version
	}
	if _, ok := expected["pip"]; !ok {
		expected["pip"] = value.ABI.SeedPip
	}
	for _, dependency := range actual {
		name := normalizedVLLMDependencyName(dependency.Name)
		if version, ok := expected[name]; !ok || version != dependency.Version {
			return fmt.Errorf("installed dependency %s differs from the reviewed set", name)
		}
		delete(expected, name)
	}
	if len(expected) != 0 {
		return errors.New("installed environment is missing reviewed dependencies")
	}
	return nil
}

func (e *Executor) captureVLLMRuntimeLock(ctx context.Context, st *engineState) (vllmRuntimeLock, error) {
	if e.vllmRuntimeLock != nil {
		return e.vllmRuntimeLock(ctx, st)
	}
	var empty vllmRuntimeLock
	record, err := readVLLMRuntimeRecord(st)
	if err != nil || record.Active == "" || record.Removing {
		return empty, errors.New("managed runtime is unavailable")
	}
	python, _, receipt, err := validateVLLMRuntimeBinaries(st, record.Active)
	if err != nil {
		return empty, err
	}
	facts, err := e.probeVLLMPython(ctx, python)
	if err != nil {
		return empty, err
	}
	reportPath := filepath.Join(receipt.Environment, "pip-report.json")
	var compatibility vllmRuntimeCompatibility
	var digest string
	if receipt.Schema == vllmEnvironmentReceiptSchema {
		var report vllmManagedDependencyReport
		if err = readVLLMJSON(st.installDir, reportPath, 16<<20, &report); err == nil {
			compatibility, digest, err = vllmCompatibilityFromManagedReport(report, facts, receipt)
		}
	} else {
		var report vllmDependencyReport
		if err = readVLLMJSON(st.installDir, reportPath, 16<<20, &report); err == nil {
			compatibility, digest, err = vllmCompatibilityFromLegacyReport(report, facts, receipt)
		}
	}
	if err != nil {
		return empty, err
	}
	env, cleanup, err := e.vllmChildEnv(st, receipt.Environment)
	if err != nil {
		return empty, err
	}
	defer cleanup()
	if err = e.verifyVLLMInstalledDependencySet(ctx, python, env, compatibility); err != nil {
		return empty, err
	}
	_, _, after, err := validateVLLMRuntimeBinaries(st, record.Active)
	if err != nil || after != receipt {
		return empty, errors.New("runtime changed during compatibility inspection")
	}
	current, err := readVLLMRuntimeRecord(st)
	if err != nil || !reflect.DeepEqual(current, record) {
		return empty, errors.New("active runtime changed during compatibility inspection")
	}
	return vllmRuntimeLock{SourceRuntimeDigest: vllmRankRuntimeDigest(receipt), CompatibilitySHA256: digest}, nil
}
