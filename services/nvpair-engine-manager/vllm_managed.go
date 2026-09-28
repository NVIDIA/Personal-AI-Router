// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

const (
	vllmRuntimeRecordFile         = "active-runtime.json"
	vllmEnvironmentReceiptFile    = "pair-runtime.json"
	vllmLegacyRuntimeRecordSchema = 1
	vllmLegacyReceiptSchema       = 1
	vllmRuntimeRecordSchema       = 2
	vllmEnvironmentReceiptSchema  = 2
	// Covers the multi-gigabyte wheel set over a slow link.
	vllmInstallTimeout = 3 * time.Hour
	// Must stay well inside the desktop's 30-minute idle window for a quiet install.
	vllmInstallHeartbeat         = time.Minute
	vllmDependencyReportMaxBytes = 16 << 20
	managedVLLMRecipeID          = "vllm-0.29.0-py312-cu130-uv0.12.17-v1"
	managedVLLMDependencyReport  = `import importlib.metadata,json
packages=[]
for distribution in importlib.metadata.distributions():
 files=[]
 for entry in distribution.files or ():
  value=getattr(entry,'hash',None)
  if value is not None:
   files.append(dict(path=str(entry),hash=str(value),size=getattr(entry,'size',None)))
 packages.append(dict(name=distribution.metadata['Name'],version=distribution.version,files=sorted(files,key=lambda item:item['path'])))
packages.sort(key=lambda item:(item['name'].lower(),item['version']))
print(json.dumps(dict(schema=1,packages=packages),separators=(',',':')))`
)

// The receipt/rotation design is adapted from independently pin-matched,
// read-only Engine Manager source evidence. That evidence supplies the
// ownership pattern only; aggregate status and commit ancestry are not
// acceptance authority for this implementation.
type vllmRuntimeRecord struct {
	Schema        int                           `json:"schema"`
	Active        string                        `json:"active,omitempty"`
	Previous      string                        `json:"previous,omitempty"`
	Retired       []string                      `json:"retired,omitempty"`
	Staged        string                        `json:"staged,omitempty"`
	Activating    *vllmActivationIntent         `json:"activating,omitempty"`
	Removing      bool                          `json:"removing,omitempty"`
	RemovalProofs map[string]vllmRuntimeReceipt `json:"removalProofs,omitempty"`
}

// vllmActivationIntent is the durable transaction boundary for a runtime
// replacement. Prior* is sufficient to reconstruct the exact pre-update
// ownership record after a crash at any point between stop and commit.
type vllmActivationIntent struct {
	Candidate     string   `json:"candidate"`
	PriorActive   string   `json:"priorActive,omitempty"`
	PriorPrevious string   `json:"priorPrevious,omitempty"`
	PriorRetired  []string `json:"priorRetired,omitempty"`
	WasRunning    bool     `json:"wasRunning,omitempty"`
}

type vllmRuntimeReceipt struct {
	Schema              int    `json:"schema"`
	RecipeID            string `json:"recipeId"`
	Engine              string `json:"engine"`
	OwnerRoot           string `json:"ownerRoot"`
	Environment         string `json:"environment"`
	Version             string `json:"version"`
	Architecture        string `json:"architecture"`
	UVURL               string `json:"uvUrl"`
	UVSHA256            string `json:"uvSha256"`
	PythonSHA256        string `json:"pythonSha256"`
	CLISHA256           string `json:"cliSha256"`
	WheelURL            string `json:"wheelUrl,omitempty"`
	WheelSHA256         string `json:"wheelSha256,omitempty"`
	PythonSource        string `json:"pythonSource,omitempty"`
	PipReportSHA256     string `json:"pipReportSha256,omitempty"`
	RecipeSHA256        string `json:"recipeSha256,omitempty"`
	RecipeReceiptSHA256 string `json:"recipeReceiptSha256,omitempty"`
	BundleReceiptSHA256 string `json:"bundleReceiptSha256,omitempty"`
	NodeID              string `json:"nodeId,omitempty"`
	CreatedAt           string `json:"createdAt"`
}

var (
	vllmEnvironmentID  = regexp.MustCompile(`^v[0-9][A-Za-z0-9._-]{1,100}$`)
	vllmReceiptVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+[A-Za-z0-9.+_-]*$`)
)

func vllmRecordedIDs(record vllmRuntimeRecord) []string {
	ids := append([]string{}, record.Retired...)
	if record.Previous != "" {
		ids = append(ids, record.Previous)
	}
	if record.Active != "" {
		ids = append(ids, record.Active)
	}
	if record.Staged != "" {
		ids = append(ids, record.Staged)
	}
	return ids
}

func nextVLLMRuntimeRecord(old vllmRuntimeRecord, id string) vllmRuntimeRecord {
	retired := append([]string{}, old.Retired...)
	if old.Previous != "" {
		retired = append(retired, old.Previous)
	}
	return vllmRuntimeRecord{
		Schema:     vllmRuntimeRecordSchema,
		Active:     id,
		Previous:   old.Active,
		Retired:    retired,
		Activating: old.Activating,
	}
}

func vllmEnvironmentPath(st *engineState, id string) (string, error) {
	if !vllmEnvironmentID.MatchString(id) {
		return "", fmt.Errorf("invalid owned vLLM environment identity")
	}
	root, err := filepath.Abs(st.installDir)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, "environments", id)
	if err := validateManagedVLLMLexicalPath(root, path); err != nil {
		return "", err
	}
	return path, nil
}

func validateVLLMRuntimeRecord(record vllmRuntimeRecord) error {
	if record.Schema != vllmRuntimeRecordSchema {
		return fmt.Errorf("unsupported vLLM runtime record schema")
	}
	if record.Active == "" && record.Previous != "" {
		return fmt.Errorf("vLLM previous runtime exists without an active runtime")
	}
	ids := vllmRecordedIDs(record)
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !vllmEnvironmentID.MatchString(id) {
			return fmt.Errorf("invalid owned vLLM environment identity")
		}
		if seen[id] {
			return fmt.Errorf("duplicate vLLM runtime ownership entry")
		}
		seen[id] = true
	}
	if record.Removing {
		if record.Staged != "" || record.Activating != nil {
			return fmt.Errorf("vLLM activation must be reconciled before removal")
		}
		if len(ids) == 0 || len(record.RemovalProofs) != len(ids) {
			return fmt.Errorf("incomplete vLLM removal ownership proofs")
		}
		for _, id := range ids {
			if _, ok := record.RemovalProofs[id]; !ok {
				return fmt.Errorf("missing vLLM removal ownership proof")
			}
		}
	} else if len(record.RemovalProofs) != 0 {
		return fmt.Errorf("vLLM removal proofs lack durable removal intent")
	}
	if activation := record.Activating; activation != nil {
		for _, id := range append([]string{activation.Candidate, activation.PriorActive, activation.PriorPrevious}, activation.PriorRetired...) {
			if id != "" && !vllmEnvironmentID.MatchString(id) {
				return fmt.Errorf("invalid vLLM activation ownership identity")
			}
		}
		if activation.Candidate == "" || activation.Candidate == activation.PriorActive {
			return fmt.Errorf("invalid vLLM activation candidate")
		}
		preSwap := record.Active == activation.PriorActive && record.Previous == activation.PriorPrevious && slices.Equal(record.Retired, activation.PriorRetired) && record.Staged == activation.Candidate
		wantRetired := append([]string{}, activation.PriorRetired...)
		if activation.PriorPrevious != "" {
			wantRetired = append(wantRetired, activation.PriorPrevious)
		}
		postSwap := record.Active == activation.Candidate && record.Staged == "" && record.Previous == activation.PriorActive && slices.Equal(record.Retired, wantRetired)
		if !preSwap && !postSwap {
			return fmt.Errorf("vLLM activation record does not match its durable phase")
		}
	}
	return nil
}

func readVLLMRuntimeRecord(st *engineState) (vllmRuntimeRecord, error) {
	record := vllmRuntimeRecord{Schema: vllmRuntimeRecordSchema}
	path := filepath.Join(st.installDir, vllmRuntimeRecordFile)
	if err := readManagedVLLMJSON(st.installDir, path, 1<<20, &record); err != nil {
		if os.IsNotExist(err) {
			return vllmRuntimeRecord{Schema: vllmRuntimeRecordSchema}, nil
		}
		return record, err
	}
	if record.Schema == vllmLegacyRuntimeRecordSchema {
		// Schema 1 had no crash-safe activation phase. Only its stable ownership
		// shape is read-compatible; any recovery/removal state remains fail-closed.
		if record.Staged != "" || record.Activating != nil || record.Removing || len(record.RemovalProofs) != 0 {
			return record, fmt.Errorf("legacy vLLM runtime recovery state requires the originating manager")
		}
		record.Schema = vllmRuntimeRecordSchema
	}
	if err := validateVLLMRuntimeRecord(record); err != nil {
		return record, err
	}
	return record, nil
}

func writeVLLMRuntimeRecord(st *engineState, record vllmRuntimeRecord) error {
	if err := validateVLLMRuntimeRecord(record); err != nil {
		return err
	}
	if err := os.MkdirAll(st.installDir, 0o700); err != nil {
		return err
	}
	return writeManagedVLLMJSON(st.installDir, filepath.Join(st.installDir, vllmRuntimeRecordFile), record)
}

func readManagedVLLMJSON(root, path string, limit int64, out any) error {
	if err := validateManagedVLLMLexicalPath(root, path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return fmt.Errorf("invalid vLLM record file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("oversized vLLM record file")
	}
	return json.Unmarshal(data, out)
}

func writeManagedVLLMJSON(root, path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return fmt.Errorf("vLLM ownership record exceeds its size bound")
	}
	return writeManagedVLLMFile(root, path, append(data, '\n'))
}

func writeManagedVLLMFile(root, path string, data []byte) error {
	if err := validateManagedVLLMLexicalPath(root, path); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pair-vllm-record-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmpPath, path)
}

func writeManagedVLLMDependencyReport(root, path string, data []byte) error {
	if len(data) == 0 || len(data) > vllmDependencyReportMaxBytes {
		return fmt.Errorf("vLLM dependency content report exceeds its size bound")
	}
	return writeManagedVLLMFile(root, path, data)
}

type vllmDependencyReportEnvelope struct {
	Schema   int               `json:"schema"`
	Packages []json.RawMessage `json:"packages"`
}

func validateManagedVLLMDependencyReport(raw []byte) error {
	if len(raw) == 0 || len(raw) > vllmDependencyReportMaxBytes {
		return fmt.Errorf("vLLM dependency content report exceeds its size bound")
	}
	var report vllmDependencyReportEnvelope
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("vLLM dependency content report must contain exactly one JSON value")
	}
	if report.Schema != 1 || len(report.Packages) == 0 || len(report.Packages) > 4096 {
		return fmt.Errorf("invalid vLLM dependency content report envelope")
	}
	return nil
}

func validateManagedVLLMLexicalPath(root, target string) error {
	if root == "" || target == "" {
		return fmt.Errorf("vLLM owned directory is unavailable")
	}
	for _, raw := range []string{root, target} {
		for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == '/' || r == '\\' }) {
			if part == ".." {
				return fmt.Errorf("vLLM owned paths must be canonical, without parent traversal")
			}
		}
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("path escapes the PAIR-owned vLLM directory")
	}
	if filepath.Dir(rootAbs) == rootAbs {
		return fmt.Errorf("vLLM ownership root cannot be a volume root")
	}
	for current := filepath.Clean(targetAbs); ; current = filepath.Dir(current) {
		info, inspectErr := os.Lstat(current)
		if inspectErr != nil && !os.IsNotExist(inspectErr) {
			return fmt.Errorf("inspect vLLM owned path: %w", inspectErr)
		}
		if inspectErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("vLLM owned path contains a redirected ancestor")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}

func managedVLLMFileHash(root, path string) (string, error) {
	resolved, err := pathInside(root, path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 256<<20 {
		return "", fmt.Errorf("vLLM provenance target is not a bounded regular owned file")
	}
	f, err := os.Open(resolved)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func validateManagedVLLMRuntimeBinary(root, target string) error {
	// uv virtual environments may make the final interpreter/console-script
	// entry a symlink. Ancestor redirection is never accepted, and the resolved
	// bounded regular file must remain inside this exact environment.
	if err := validateManagedVLLMLexicalPath(root, filepath.Dir(target)); err != nil {
		return err
	}
	info, err := os.Lstat(target)
	if err != nil || !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("vLLM runtime binary is unavailable or unsupported")
	}
	resolved, err := pathInside(root, target)
	if err != nil {
		return err
	}
	info, err = os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("vLLM runtime binary does not resolve to an owned regular file")
	}
	return nil
}

func validateVLLMReceiptMetadata(st *engineState, id string, receipt vllmRuntimeReceipt) error {
	dir, err := vllmEnvironmentPath(st, id)
	if err != nil {
		return err
	}
	rootAbs, err := filepath.Abs(st.installDir)
	if err != nil {
		return err
	}
	dirAbs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if receipt.Engine != "vllm" || receipt.OwnerRoot != rootAbs || receipt.Environment != dirAbs ||
		!vllmReceiptVersion.MatchString(receipt.Version) || (receipt.Architecture != "amd64" && receipt.Architecture != "arm64") || receipt.Architecture != runtime.GOARCH {
		return fmt.Errorf("vLLM runtime provenance does not match this owned environment")
	}
	if _, err := time.Parse(time.RFC3339Nano, receipt.CreatedAt); err != nil {
		return fmt.Errorf("vLLM runtime receipt has an invalid creation time")
	}
	if receipt.Schema == vllmLegacyReceiptSchema {
		u, urlErr := url.Parse(receipt.WheelURL)
		qwen := ownedRetainedQwen38Receipt(receipt)
		if qwen && receipt.UVURL == "" && receipt.UVSHA256 == "" && receipt.CLISHA256 == "" &&
			slices.Contains(legacyVLLMPythonSources, receipt.PythonSource) {
			return nil
		}
		if receipt.RecipeID != "" || receipt.UVURL != "" || receipt.UVSHA256 != "" || receipt.CLISHA256 != "" ||
			urlErr != nil || u.Scheme != "https" || u.User != nil || !validSHA256(receipt.WheelSHA256) ||
			!validSHA256(receipt.PythonSHA256) || !validSHA256(receipt.PipReportSHA256) ||
			!slices.Contains(legacyVLLMPythonSources, receipt.PythonSource) || !recognizedManagedVLLMReceipt(st, receipt) {
			return fmt.Errorf("legacy vLLM runtime provenance does not match a retained owned recipe")
		}
		return nil
	}
	u, urlErr := url.Parse(receipt.UVURL)
	qwen := ownedRetainedQwen38Receipt(receipt) || legacyQwen38UVMarkerReceipt(receipt)
	validBootstrap := urlErr == nil && u.Scheme == "https" && u.User == nil && validSHA256(receipt.UVSHA256)
	if qwen {
		validBootstrap = receipt.UVURL == "" && receipt.UVSHA256 == "" || legacyQwen38UVMarkerReceipt(receipt)
	}
	if receipt.Schema != vllmEnvironmentReceiptSchema || receipt.RecipeID == "" || receipt.WheelURL != "" || receipt.WheelSHA256 != "" || receipt.PythonSource != "" ||
		!validBootstrap || !validSHA256(receipt.PythonSHA256) || !validSHA256(receipt.CLISHA256) || !validSHA256(receipt.PipReportSHA256) {
		return fmt.Errorf("vLLM runtime provenance does not match this owned environment")
	}
	if !recognizedManagedVLLMReceipt(st, receipt) {
		return fmt.Errorf("vLLM runtime receipt recipe is not in the owned recipe catalog")
	}
	if qwen {
		if err := validatePreparedQwen38ReceiptsForOwnership(st, dir, receipt); err != nil {
			return errors.Join(errors.New("prepared Qwen3.8 runtime provenance is invalid"), err)
		}
	}
	return nil
}

func validSHA256(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == sha256.Size
}

func validateVLLMEnvironment(st *engineState, id string) (string, vllmRuntimeReceipt, error) {
	var receipt vllmRuntimeReceipt
	dir, err := vllmEnvironmentPath(st, id)
	if err != nil {
		return "", receipt, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", receipt, fmt.Errorf("owned vLLM environment is unavailable")
	}
	if err = readManagedVLLMJSON(st.installDir, filepath.Join(dir, vllmEnvironmentReceiptFile), 32<<10, &receipt); err != nil {
		return "", receipt, err
	}
	if err = validateVLLMReceiptMetadata(st, id, receipt); err != nil {
		return "", receipt, err
	}
	python, cli, err := vllmRuntimeBinaries(dir, receipt)
	if err != nil {
		return "", receipt, err
	}
	pythonHash, err := managedVLLMFileHash(dir, python)
	if err != nil || pythonHash != receipt.PythonSHA256 {
		return "", receipt, fmt.Errorf("vLLM interpreter provenance verification failed")
	}
	if receipt.Schema == vllmLegacyReceiptSchema {
		reportHash, reportErr := managedVLLMFileHash(dir, filepath.Join(dir, "pip-report.json"))
		if reportErr != nil || reportHash != receipt.PipReportSHA256 {
			return "", receipt, fmt.Errorf("legacy vLLM dependency report provenance verification failed")
		}
		if receipt.RecipeID != "" {
			if err := validateInstalledQwen38Receipts(st, dir, receipt); err != nil {
				return "", receipt, err
			}
		}
		// Schema 1 did not bind the console-script bytes. Keep ownership and any
		// process identity on the receipt-hashed interpreter. Only exact retained
		// group profiles receive group authority; standalone start remains closed.
		return python, receipt, nil
	}
	cliHash, err := managedVLLMFileHash(dir, cli)
	if err != nil || cliHash != receipt.CLISHA256 {
		return "", receipt, fmt.Errorf("vLLM CLI provenance verification failed")
	}
	reportHash, reportErr := managedVLLMFileHash(dir, filepath.Join(dir, "pip-report.json"))
	if reportErr != nil || reportHash != receipt.PipReportSHA256 {
		return "", receipt, fmt.Errorf("vLLM dependency report provenance verification failed")
	}
	return cli, receipt, nil
}

// validateVLLMRuntimeBinaries keeps the runtime layout explicit for callers
// that need both the interpreter and console script. Schema 1 used the
// interpreter as its ownership entrypoint; schema 2 binds the console script,
// so treating validateVLLMEnvironment's first result as "python" is unsafe.
func validateVLLMRuntimeBinaries(st *engineState, id string) (python, cli string, receipt vllmRuntimeReceipt, err error) {
	entrypoint, receipt, err := validateVLLMEnvironment(st, id)
	if err != nil {
		return "", "", receipt, err
	}
	python, cli, err = vllmRuntimeBinaries(receipt.Environment, receipt)
	if err != nil {
		return "", "", receipt, err
	}
	want := cli
	if receipt.Schema == vllmLegacyReceiptSchema {
		want = python
	}
	if entrypoint != want {
		return "", "", receipt, fmt.Errorf("vLLM runtime entrypoint changed during validation")
	}
	return python, cli, receipt, nil
}

func vllmRuntimeBinaries(dir string, receipt vllmRuntimeReceipt) (python, cli string, err error) {
	binDir := filepath.Join(dir, "venv", "bin")
	if receipt.Schema == vllmLegacyReceiptSchema {
		binDir = filepath.Join(dir, "bin")
	}
	python, cli = filepath.Join(binDir, "python"), filepath.Join(binDir, "vllm")
	if err = validateManagedVLLMRuntimeBinary(dir, python); err != nil {
		return "", "", err
	}
	err = validateManagedVLLMRuntimeBinary(dir, cli)
	return python, cli, err
}

func currentManagedVLLMReceipt(st *engineState, receipt vllmRuntimeReceipt) bool {
	return receipt.RecipeID == managedVLLMRecipeID && admittedManagedVLLMReceipt(receipt)
}

// Ownership recognition is closed over explicitly shipped recipes. A recipe
// may remain updateable after it stops being runnable, but arbitrary receipts
// never gain PAIR ownership merely by sharing an installer URL.
func recognizedManagedVLLMReceipt(st *engineState, receipt vllmRuntimeReceipt) bool {
	if receipt.Schema == vllmLegacyReceiptSchema {
		if ownedRetainedQwen38Receipt(receipt) {
			return true
		}
		for _, recipe := range managedVLLMLegacyOwnershipRecipes {
			if receipt.Version == recipe.Version && receipt.Architecture == recipe.Architecture && receipt.WheelURL == recipe.WheelURL && strings.EqualFold(receipt.WheelSHA256, recipe.WheelSHA256) {
				return true
			}
		}
		return false
	}
	if ownedRetainedQwen38Receipt(receipt) {
		return true
	}
	if legacyQwen38UVMarkerReceipt(receipt) {
		return true
	}
	for _, recipe := range managedVLLMOwnershipRecipes {
		if receiptMatchesVLLMRecipe(receipt, recipe) {
			return true
		}
	}
	return false
}

type vllmAdmittedRecipe struct {
	RecipeID, Version, Architecture, UVURL, UVSHA256 string
}

type vllmLegacyRecipe struct {
	Version, Architecture, WheelURL, WheelSHA256 string
}

var legacyVLLMPythonSources = []string{
	"/usr/bin/python3.12", "/usr/local/bin/python3.12",
	"/usr/bin/python3.13", "/usr/local/bin/python3.13",
	"/usr/bin/python3",
}

var managedVLLMLegacyOwnershipRecipes = []vllmLegacyRecipe{
	{"0.28.0", "amd64", "https://github.com/vllm-project/vllm/releases/download/v0.28.0/vllm-0.28.0-cp38-abi3-manylinux_2_28_x86_64.whl", "addb0ffdaafd8155d75e9b3f5ddb3da28fdee9e8a7097ede91f7db2e9e1a3889"},
	{"0.28.0", "arm64", "https://github.com/vllm-project/vllm/releases/download/v0.28.0/vllm-0.28.0-cp38-abi3-manylinux_2_28_aarch64.whl", "817b8181f7f61b4a62dc1d5d9ab39f2bfb60a6cb86c29879a78a147b85756787"},
}

var managedVLLMOwnershipRecipes = []vllmAdmittedRecipe{
	{managedVLLMRecipeID, managedVLLMVersion, "amd64", "https://github.com/astral-sh/uv/releases/download/0.12.17/uv-x86_64-unknown-linux-gnu.tar.gz", "fa82fd8dde8e8eefdecada6aa0889666556cfceb690d06e0c3bca49eb3070a63"},
	{managedVLLMRecipeID, managedVLLMVersion, "arm64", "https://github.com/astral-sh/uv/releases/download/0.12.17/uv-aarch64-unknown-linux-gnu.tar.gz", "d636d1b678e9e7f367ecb22b46bd1cabbed234d6bc3b4d96365d2b507f72f86c"},
	{vllmQwen38RecipeID, vllmQwen38Runtime, "arm64", "", ""},
}

var managedVLLMRunnableRecipes = append([]vllmAdmittedRecipe{}, managedVLLMOwnershipRecipes...)

func receiptMatchesVLLMRecipe(receipt vllmRuntimeReceipt, recipe vllmAdmittedRecipe) bool {
	return receipt.RecipeID == recipe.RecipeID && receipt.Version == recipe.Version && receipt.Architecture == recipe.Architecture && receipt.UVURL == recipe.UVURL && strings.EqualFold(receipt.UVSHA256, recipe.UVSHA256)
}

func admittedManagedVLLMReceipt(receipt vllmRuntimeReceipt) bool {
	if receipt.Schema != vllmEnvironmentReceiptSchema {
		return false
	}
	for _, recipe := range managedVLLMRunnableRecipes {
		if receiptMatchesVLLMRecipe(receipt, recipe) {
			return true
		}
	}
	return false
}

func validateManagedVLLMStartAdmission(st *engineState) error {
	record, err := readVLLMRuntimeRecord(st)
	if err != nil {
		return err
	}
	if record.Activating != nil {
		return fmt.Errorf("vLLM activation recovery is incomplete")
	}
	if record.Active == "" {
		return fmt.Errorf("vLLM has no active PAIR-owned runtime")
	}
	_, receipt, err := validateVLLMEnvironment(st, record.Active)
	if err != nil {
		return err
	}
	if receipt.RecipeID == vllmQwen38RecipeID {
		return qwen38ServingGroupRequired()
	}
	if !admittedManagedVLLMReceipt(receipt) {
		return fmt.Errorf("vLLM %s is receipt-owned but not admitted to run; update to the pinned %s recipe", receipt.Version, managedVLLMVersion)
	}
	return nil
}

func validateManagedVLLMActivationCandidate(st *engineState) error {
	record, err := readVLLMRuntimeRecord(st)
	if err != nil {
		return err
	}
	if record.Activating == nil || record.Active != record.Activating.Candidate || record.Staged != "" {
		return fmt.Errorf("vLLM activation candidate is not durably selected")
	}
	_, receipt, err := validateVLLMEnvironment(st, record.Active)
	if err != nil {
		return err
	}
	if !admittedManagedVLLMReceipt(receipt) {
		return fmt.Errorf("vLLM activation candidate is not an admitted runtime recipe")
	}
	return nil
}

func verifyManagedVLLMVersion(ctx context.Context, st *engineState, id string, receipt vllmRuntimeReceipt) error {
	dir, err := vllmEnvironmentPath(st, id)
	if err != nil {
		return err
	}
	python, _, err := vllmRuntimeBinaries(dir, receipt)
	if err != nil {
		return err
	}
	env := managedVLLMRuntimeEnv(st, dir)
	if receipt.Schema == vllmLegacyReceiptSchema {
		env[0] = "PATH=" + filepath.Join(dir, "bin") + ":/usr/bin:/bin"
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := runManagedVLLMCommand(probeCtx, dir, python, []string{"-I", "-B", "-c", `import importlib.metadata;print(importlib.metadata.version('vllm'))`}, env)
	if err != nil || strings.TrimSpace(string(out)) != receipt.Version {
		return fmt.Errorf("managed vLLM version verification failed")
	}
	return nil
}

func managedVLLMRuntimeEnv(st *engineState, runtimeDir string) []string {
	venvBin := filepath.Join(runtimeDir, "venv", "bin")
	return []string{
		"PATH=" + venvBin + ":/usr/bin:/bin",
		"HOME=" + filepath.Join(st.installDir, "runtime-home"),
		"HF_HOME=" + filepath.Join(st.modelDir, "huggingface"),
		"HF_HUB_OFFLINE=1",
		"TRANSFORMERS_OFFLINE=1",
		"VLLM_USE_FLASHINFER_SAMPLER=0",
		"VLLM_ALLREDUCE_USE_FLASHINFER=0",
		"PYTHONNOUSERSITE=1",
		"PYTHONDONTWRITEBYTECODE=1",
	}
}

func managedVLLMInstallEnv(st *engineState, runtimeDir string) []string {
	return []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + filepath.Join(runtimeDir, "home"),
		"UV_PYTHON_INSTALL_DIR=" + filepath.Join(runtimeDir, "python"),
		"UV_CACHE_DIR=" + managedVLLMCacheDir(st),
		"PYTHONNOUSERSITE=1",
		"PYTHONDONTWRITEBYTECODE=1",
	}
}

// managedVLLMCacheDir sits outside every staged environment, so a failed or
// timed-out install keeps the wheels it finished downloading for the next
// attempt.
func managedVLLMCacheDir(st *engineState) string {
	return filepath.Join(st.installDir, "uv-cache")
}

func managedVLLMPipInstallArgs(python string) []string {
	return []string{"pip", "install", "--python", python, "--only-binary=:all:", "vllm==" + managedVLLMVersion, "--torch-backend=cu130"}
}

func removeManagedVLLMCache(st *engineState, remove func(string) error) error {
	dir := managedVLLMCacheDir(st)
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to remove a non-directory vLLM package cache path")
	}
	return remove(dir)
}

func runManagedVLLMCommand(ctx context.Context, dir, bin string, args, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append([]string{}, env...)
	configureSysProcAttr(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", filepath.Base(bin), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func extractManagedUV(archivePath, targetDir string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	target := filepath.Join(targetDir, "uv")
	found := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if filepath.Base(filepath.FromSlash(hdr.Name)) != "uv" || (hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA) {
			continue
		}
		if found || hdr.Size <= 0 || hdr.Size > 256<<20 {
			return "", fmt.Errorf("pinned uv archive has an invalid executable payload")
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
		if err != nil {
			return "", err
		}
		_, copyErr := io.CopyN(out, tr, hdr.Size)
		closeErr := out.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		found = true
	}
	if !found {
		return "", fmt.Errorf("pinned uv archive did not contain the uv executable")
	}
	return target, nil
}

func (e *Executor) stageManagedVLLM(ctx context.Context, st *engineState, download string) (id string, resultErr error) {
	record, err := readVLLMRuntimeRecord(st)
	if err != nil {
		return "", err
	}
	if record.Staged != "" {
		return "", fmt.Errorf("a prior managed vLLM stage must be reconciled first")
	}
	parent := filepath.Join(st.installDir, "environments")
	if err = os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(parent, "v"+managedVLLMVersion+"-")
	if err != nil {
		return "", err
	}
	id = filepath.Base(dir)
	stagedID := id
	record.Staged = id
	if err = writeVLLMRuntimeRecord(st, record); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	defer cleanupFailedManagedVLLMStage(st, stagedID, &resultErr)
	if err = os.MkdirAll(filepath.Join(dir, "home"), 0o700); err != nil {
		return "", err
	}
	uv, err := extractManagedUV(download, dir)
	if err != nil {
		return "", err
	}
	env := managedVLLMInstallEnv(st, dir)
	e.emitInstallProgress("vllm", "creating-environment", 60)
	if _, err = runManagedVLLMCommand(ctx, dir, uv, []string{"venv", "--python", "3.12", "--seed", "--managed-python", filepath.Join(dir, "venv")}, env); err != nil {
		return "", err
	}
	e.emitInstallProgress("vllm", "installing", 75)
	reportPath := filepath.Join(dir, "pip-report.json")
	stopHeartbeat := e.repeatInstallProgress("vllm", "installing", 75, vllmInstallHeartbeat)
	_, err = runManagedVLLMCommand(ctx, dir, uv, managedVLLMPipInstallArgs(filepath.Join(dir, "venv", "bin", "python")), env)
	stopHeartbeat()
	if err != nil {
		return "", err
	}
	python := filepath.Join(dir, "venv", "bin", "python")
	cli := filepath.Join(dir, "venv", "bin", "vllm")
	qualification := `import importlib.metadata,torch,vllm; version=importlib.metadata.version('vllm'); assert version=='` + managedVLLMVersion + `'; assert torch.cuda.is_available(); assert any(torch.cuda.get_device_capability(i)>=(7,5) for i in range(torch.cuda.device_count())); print(version)`
	out, err := runManagedVLLMCommand(ctx, dir, python, []string{"-I", "-B", "-c", qualification}, managedVLLMRuntimeEnv(st, dir))
	if err != nil || strings.TrimSpace(string(out)) != managedVLLMVersion {
		return "", fmt.Errorf("installed vLLM version verification failed")
	}
	report, err := runManagedVLLMCommand(ctx, dir, python, []string{"-I", "-B", "-c", managedVLLMDependencyReport}, managedVLLMRuntimeEnv(st, dir))
	if err != nil || validateManagedVLLMDependencyReport(report) != nil {
		return "", fmt.Errorf("installed vLLM dependency content report failed")
	}
	if err = writeManagedVLLMDependencyReport(st.installDir, reportPath, report); err != nil {
		return "", fmt.Errorf("retain installed vLLM dependency content report: %w", err)
	}
	pythonHash, err := managedVLLMFileHash(dir, python)
	if err != nil {
		return "", err
	}
	cliHash, err := managedVLLMFileHash(dir, cli)
	if err != nil {
		return "", fmt.Errorf("vLLM installation did not provide its expected CLI: %w", err)
	}
	reportHash, err := managedVLLMFileHash(dir, reportPath)
	if err != nil {
		return "", fmt.Errorf("vLLM installation did not provide its dependency report: %w", err)
	}
	rootAbs, _ := filepath.Abs(st.installDir)
	dirAbs, _ := filepath.Abs(dir)
	fetch := st.plat.Install.Fetch
	receipt := vllmRuntimeReceipt{
		Schema: vllmEnvironmentReceiptSchema, Engine: "vllm", RecipeID: managedVLLMRecipeID, OwnerRoot: rootAbs, Environment: dirAbs,
		Version: managedVLLMVersion, Architecture: runtime.GOARCH, UVURL: fetch.URL, UVSHA256: fetch.SHA256,
		PythonSHA256: pythonHash, CLISHA256: cliHash, PipReportSHA256: reportHash, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err = writeManagedVLLMJSON(st.installDir, filepath.Join(dir, vllmEnvironmentReceiptFile), receipt); err != nil {
		return "", err
	}
	_ = removeManagedVLLMCache(st, os.RemoveAll)
	_ = os.Remove(uv)
	return id, nil
}

func cleanupFailedManagedVLLMStage(st *engineState, stagedID string, resultErr *error) {
	if *resultErr == nil {
		return
	}
	if cleanupErr := removeManagedVLLMEnvironment(st, stagedID, os.RemoveAll); cleanupErr == nil {
		current, readErr := readVLLMRuntimeRecord(st)
		if readErr == nil && current.Staged == stagedID {
			current.Staged = ""
			*resultErr = errors.Join(*resultErr, writeVLLMRuntimeRecord(st, current))
		}
	} else {
		*resultErr = errors.Join(*resultErr, cleanupErr)
	}
}

func (e *Executor) reconcileManagedVLLMStage(st *engineState, record vllmRuntimeRecord) (vllmRuntimeRecord, error) {
	if record.Staged == "" {
		return record, nil
	}
	dir, err := vllmEnvironmentPath(st, record.Staged)
	if err != nil {
		return record, err
	}
	info, statErr := os.Lstat(dir)
	if statErr == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return record, fmt.Errorf("staged vLLM ownership path is not an owned directory")
	}
	if statErr != nil && !os.IsNotExist(statErr) {
		return record, statErr
	}
	if statErr == nil {
		if err = os.RemoveAll(dir); err != nil {
			return record, err
		}
	}
	record.Staged = ""
	if err = writeVLLMRuntimeRecord(st, record); err != nil {
		return record, err
	}
	return record, nil
}

type vllmRuntimeControl struct {
	stop          func() error
	start         func(context.Context) error
	recoveryStart func(context.Context) error
	write         func(vllmRuntimeRecord) error
	detect        func(bool) error
}

func (e *Executor) managedVLLMRuntimeControl(st *engineState) vllmRuntimeControl {
	return vllmRuntimeControl{
		stop: func() error { return e.doStop(st, "vllm") },
		start: func(ctx context.Context) error {
			return e.doStart(ctx, st, "vllm", startOpts{allowVLLMActivationCandidate: true})
		},
		recoveryStart: func(ctx context.Context) error {
			return e.doStart(ctx, st, "vllm", startOpts{allowOwnedVLLMRecovery: true})
		},
		write: func(record vllmRuntimeRecord) error { return writeVLLMRuntimeRecord(st, record) },
		detect: func(allowActivating bool) error {
			_, err := detectManagedVLLMRecord(st, allowActivating)
			return err
		},
	}
}

func detectVLLMWith(control vllmRuntimeControl, st *engineState, allowActivating bool) error {
	if control.detect != nil {
		return control.detect(allowActivating)
	}
	_, err := detectManagedVLLMRecord(st, allowActivating)
	return err
}

func writeVLLMRuntimeRecordWith(control vllmRuntimeControl, st *engineState, record vllmRuntimeRecord) error {
	if control.write != nil {
		return control.write(record)
	}
	return writeVLLMRuntimeRecord(st, record)
}

func startVLLMRecovery(control vllmRuntimeControl, ctx context.Context) error {
	if control.recoveryStart != nil {
		return control.recoveryStart(ctx)
	}
	return control.start(ctx)
}

func (e *Executor) activateManagedVLLM(ctx context.Context, st *engineState, old vllmRuntimeRecord, id string, wasRunning bool, control vllmRuntimeControl) error {
	if old.Staged != id {
		return fmt.Errorf("vLLM staged ownership changed before activation")
	}
	if _, _, err := validateVLLMEnvironment(st, id); err != nil {
		return err
	}
	old.Activating = &vllmActivationIntent{
		Candidate: id, PriorActive: old.Active, PriorPrevious: old.Previous,
		PriorRetired: append([]string{}, old.Retired...), WasRunning: wasRunning,
	}
	if err := writeVLLMRuntimeRecordWith(control, st, old); err != nil {
		return fmt.Errorf("persist vLLM activation intent: %w", err)
	}
	if wasRunning {
		if err := control.stop(); err != nil {
			return err
		}
	}
	if err := e.ensureManagedVLLMPortFree(ctx, st); err != nil {
		// Keep Staged + Activating durable. A later reconciliation may roll back
		// only after the foreign/indeterminate listener is gone.
		return err
	}
	next := nextVLLMRuntimeRecord(old, id)
	if err := writeVLLMRuntimeRecordWith(control, st, next); err != nil {
		return errors.Join(err, e.restoreManagedVLLM(ctx, st, old, id, wasRunning, control))
	}
	activationErr := detectVLLMWith(control, st, true)
	if activationErr == nil && wasRunning {
		activationErr = control.start(ctx)
	}
	if activationErr == nil {
		committed := next
		committed.Activating = nil
		if err := writeVLLMRuntimeRecordWith(control, st, committed); err == nil {
			return nil
		} else {
			activationErr = fmt.Errorf("commit vLLM activation: %w", err)
		}
	}
	stopErr := error(nil)
	if wasRunning {
		stopErr = control.stop()
	}
	rollbackErr := e.restoreManagedVLLM(ctx, st, old, id, wasRunning, control)
	return errors.Join(fmt.Errorf("vLLM update activation failed: %w", activationErr), stopErr, rollbackErr)
}

func (e *Executor) restoreManagedVLLM(ctx context.Context, st *engineState, old vllmRuntimeRecord, newID string, restart bool, control vllmRuntimeControl) error {
	rollback := old
	if activation := old.Activating; activation != nil {
		rollback = vllmRuntimeRecord{
			Schema: vllmRuntimeRecordSchema, Active: activation.PriorActive,
			Previous: activation.PriorPrevious, Retired: append([]string{}, activation.PriorRetired...),
		}
		if newID == "" {
			newID = activation.Candidate
		}
	}
	rollback.Staged, rollback.Activating = "", nil
	if newID != "" && !containsString(vllmRecordedIDs(rollback), newID) {
		rollback.Retired = append(rollback.Retired, newID)
	}
	rollback.Removing = false
	rollback.RemovalProofs = nil
	if err := writeVLLMRuntimeRecordWith(control, st, rollback); err != nil {
		return err
	}
	if err := detectVLLMWith(control, st, false); err != nil {
		return err
	}
	if !restart || rollback.Active == "" || e.shuttingDown.Load() {
		return nil
	}
	_, receipt, err := validateVLLMEnvironment(st, rollback.Active)
	if err != nil {
		return err
	}
	if !managedVLLMRecoveryStartAllowed(receipt) {
		// Legacy runtimes and both Qwen recipe generations are ownership-only
		// after rollback. Qwen remains serving-group only; none of these pointers
		// may become a standalone recovery Start.
		return nil
	}
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	return startVLLMRecovery(control, recoveryCtx)
}

func managedVLLMRecoveryStartAllowed(receipt vllmRuntimeReceipt) bool {
	return receipt.Schema != vllmLegacyReceiptSchema && !ownedRetainedQwen38Receipt(receipt) && !legacyQwen38UVMarkerReceipt(receipt)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func managedVLLMImages(st *engineState, id string) ([]string, error) {
	dir, err := vllmEnvironmentPath(st, id)
	if err != nil {
		return nil, err
	}
	_, receipt, err := validateVLLMEnvironment(st, id)
	if err != nil {
		return nil, err
	}
	python, cli, err := vllmRuntimeBinaries(dir, receipt)
	if err != nil {
		return nil, err
	}
	if receipt.Schema == vllmLegacyReceiptSchema {
		// The retained receipt binds the interpreter and dependency report, not
		// the console-script bytes. A legacy process is owned only by that exact
		// interpreter image; the old runtime is not admitted to start.
		return []string{python}, nil
	}
	return []string{cli, python}, nil
}

func managedVLLMImageMatches(st *engineState, id, image string) bool {
	images, err := managedVLLMImages(st, id)
	if err != nil {
		return false
	}
	for _, owned := range images {
		if isOurEngineImage(image, owned) {
			return true
		}
	}
	return false
}

func managedVLLMRecordedImageMatches(st *engineState, image string) bool {
	record, err := readVLLMRuntimeRecord(st)
	if err != nil {
		return false
	}
	for _, id := range vllmRecordedIDs(record) {
		if managedVLLMImageMatches(st, id, image) {
			return true
		}
	}
	return false
}

func activeVLLMRuntimeID(st *engineState) string {
	record, err := readVLLMRuntimeRecord(st)
	if err != nil {
		return ""
	}
	return record.Active
}

// observeManagedVLLMListener distinguishes a receipt-owned orphan from an
// external vLLM listener before generic presence reconciliation can label both
// as adopted. Unknown image ownership always fails closed.
func (e *Executor) observeManagedVLLMListener(ctx context.Context, st *engineState, id string) (owned, occupied bool, err error) {
	return e.observeManagedVLLMListeners(ctx, st, []string{id})
}

func (e *Executor) observeManagedVLLMRecordedListener(ctx context.Context, st *engineState, record vllmRuntimeRecord) (owned, occupied bool, err error) {
	return e.observeManagedVLLMListeners(ctx, st, vllmRecordedIDs(record))
}

func (e *Executor) observeManagedVLLMListeners(ctx context.Context, st *engineState, ids []string) (owned, occupied bool, err error) {
	st.mu.Lock()
	if st.proc != nil && st.running && !st.adopted {
		st.mu.Unlock()
		return true, true, nil
	}
	port, ready := st.port, st.plat.Runtime.Ready
	st.mu.Unlock()
	if port <= 0 || ready == nil {
		return false, false, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, presenceRefusalWindow)
	listener := probeListener(probeCtx, ready, port)
	cancel()
	if listener == listenerProbeRefused {
		return false, false, nil
	}
	pid, image, ok := pidOnPort(port)
	matchedID := ""
	if ok {
		for _, id := range ids {
			if managedVLLMImageMatches(st, id, image) {
				matchedID = id
				break
			}
		}
	}
	if matchedID == "" {
		return false, true, nil
	}
	cli, receipt, validateErr := validateVLLMEnvironment(st, matchedID)
	if validateErr != nil {
		return false, true, validateErr
	}
	st.mu.Lock()
	st.installed, st.running, st.adopted = true, true, false
	healthy := e.probe(ctx, ready, port)
	if ctx.Err() == nil {
		st.healthy = healthy
	}
	st.binPath, st.version = cli, receipt.Version
	st.proc = nil
	st.mu.Unlock()
	_ = pid // ownership is represented by the exact port/image pair; doStop resolves it again.
	return true, true, nil
}

func (e *Executor) reconcileManagedVLLMActivation(ctx context.Context, st *engineState) error {
	return e.reconcileManagedVLLMActivationWithControl(ctx, st, e.managedVLLMRuntimeControl(st))
}

func (e *Executor) reconcileManagedVLLMActivationWithControl(ctx context.Context, st *engineState, control vllmRuntimeControl) error {
	record, err := readVLLMRuntimeRecord(st)
	if err != nil || record.Activating == nil {
		return err
	}
	// Status and get-installed may enter crash recovery. Once recovery would
	// become an effect, the retained serving-group owner takes precedence too.
	if err := e.rejectVLLMGroupMutation("vllm", "reconcile activation for"); err != nil {
		return err
	}
	activation := *record.Activating
	currentID := activation.PriorActive
	postSwap := record.Active == activation.Candidate
	if postSwap {
		currentID = activation.Candidate
	}
	restart := activation.WasRunning
	if restart && e.desired != nil {
		enabled, known, desiredErr := e.desired.get("vllm")
		if desiredErr != nil {
			return desiredErr
		}
		if known && !enabled {
			restart = false
		}
	}
	if currentID != "" {
		cli, receipt, validateErr := validateVLLMEnvironment(st, currentID)
		if validateErr != nil {
			return validateErr
		}
		st.mu.Lock()
		st.installed, st.binPath, st.version = true, cli, receipt.Version
		if st.proc == nil || !st.running || st.adopted {
			st.running, st.healthy, st.adopted, st.proc = false, false, false, nil
		}
		st.mu.Unlock()
		owned, occupied, observeErr := e.observeManagedVLLMListener(ctx, st, currentID)
		if observeErr != nil {
			return observeErr
		}
		if occupied && !owned {
			return fmt.Errorf("vLLM activation recovery is blocked by a foreign or unproved listener")
		}
		if owned && (postSwap || !restart) {
			if err := e.doStop(st, "vllm"); err != nil {
				return err
			}
		}
	}
	return e.restoreManagedVLLM(ctx, st, record, activation.Candidate, restart, control)
}

func (e *Executor) ensureManagedVLLMPortFree(ctx context.Context, st *engineState) error {
	record, err := readVLLMRuntimeRecord(st)
	if err != nil {
		return err
	}
	id := record.Active
	if record.Activating != nil && record.Active == record.Activating.Candidate {
		id = record.Activating.Candidate
	}
	if id != "" {
		owned, occupied, observeErr := e.observeManagedVLLMListener(ctx, st, id)
		if observeErr != nil {
			return observeErr
		}
		if owned {
			if err := e.doStop(st, "vllm"); err != nil {
				return err
			}
			owned, occupied, observeErr = e.observeManagedVLLMListener(ctx, st, id)
			if observeErr != nil {
				return observeErr
			}
		}
		if occupied || owned {
			return fmt.Errorf("vLLM listener is occupied; refusing managed runtime activation")
		}
		return nil
	}
	st.mu.Lock()
	port, ready := st.port, st.plat.Runtime.Ready
	st.mu.Unlock()
	if port <= 0 || ready == nil {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, presenceRefusalWindow)
	listener := probeListener(probeCtx, ready, port)
	cancel()
	if listener != listenerProbeRefused {
		return fmt.Errorf("vLLM listener is occupied; refusing managed runtime activation")
	}
	return nil
}

func (e *Executor) ensureManagedVLLMRemovalPortFree(ctx context.Context, st *engineState, record vllmRuntimeRecord) error {
	st.mu.Lock()
	port, ready := st.port, st.plat.Runtime.Ready
	st.mu.Unlock()
	if port <= 0 || ready == nil {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, presenceRefusalWindow)
	listener := probeListener(probeCtx, ready, port)
	cancel()
	if listener == listenerProbeRefused {
		return nil
	}
	pid, image, ok := pidOnPort(port)
	if ok {
		for _, id := range vllmRecordedIDs(record) {
			if managedVLLMImageMatches(st, id, image) {
				terminatePID(pid, stopGrace(st.plat.Runtime))
				if !pidAlive(pid) && e.waitUnavailable(ready, port, presenceRefusalWindow) {
					return nil
				}
				return fmt.Errorf("receipt-owned vLLM process did not exit during removal recovery")
			}
		}
	}
	return fmt.Errorf("vLLM removal recovery is blocked by a foreign or unproved listener")
}

func (e *Executor) installManagedVLLM(parent context.Context, st *engineState, update bool) error {
	if st.plat.Install == nil || st.plat.Install.Driver != "vllm-python" || st.plat.Install.Fetch == nil {
		return fmt.Errorf("vLLM managed install recipe is incomplete")
	}
	ctx, cancel := context.WithTimeout(parent, vllmInstallTimeout)
	defer cancel()
	if err := validateManagedVLLMLexicalPath(st.installDir, st.installDir); err != nil {
		return err
	}
	if err := os.MkdirAll(st.installDir, 0o700); err != nil {
		return err
	}
	for _, dir := range []string{st.modelDir, filepath.Join(st.installDir, "runtime-home")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	record, err := readVLLMRuntimeRecord(st)
	if err != nil {
		return err
	}
	if record.Removing {
		return fmt.Errorf("vLLM removal is incomplete; retry Uninstall before installing")
	}
	if record.Staged != "" {
		record, err = e.reconcileManagedVLLMStage(st, record)
		if err != nil {
			return err
		}
	}
	if record.Active != "" {
		_, receipt, err := validateVLLMEnvironment(st, record.Active)
		if err != nil {
			return err
		}
		if !update {
			e.emitInstallProgress("vllm", "already-installed", 100)
			return nil
		}
		if currentManagedVLLMReceipt(st, receipt) {
			// Reuse the established terminal install-progress contract so the
			// fire-and-forget Update spinner clears even when the pinned recipe is
			// already active.
			e.emitInstallProgress("vllm", "done", 100)
			e.emitState("vllm")
			return nil
		}
	} else if update {
		return fmt.Errorf("vLLM is not installed; use Install before Update")
	}
	e.emitInstallProgress("vllm", "downloading", 0)
	download, err := e.download(ctx, "vllm", st.plat.Install.Fetch)
	if err != nil {
		return err
	}
	defer os.Remove(download)
	id, err := e.stageManagedVLLM(ctx, st, download)
	if err != nil {
		return err
	}
	staged, err := readVLLMRuntimeRecord(st)
	if err != nil || staged.Staged != id {
		return errors.Join(fmt.Errorf("vLLM staged ownership was not durably recorded"), err)
	}
	st.mu.Lock()
	wasRunning := st.running && !st.adopted
	st.mu.Unlock()
	if err = e.activateManagedVLLM(ctx, st, staged, id, wasRunning, e.managedVLLMRuntimeControl(st)); err != nil {
		return err
	}
	e.reporter.clear(installFailedID("vllm"))
	e.emitInstallProgress("vllm", "done", 100)
	e.emitState("vllm")
	return nil
}

func (e *Executor) Update(ctx context.Context, engine string) error {
	if engine != "vllm" {
		return fmt.Errorf("engine %q does not have a managed atomic update driver", engine)
	}
	if err := e.rejectVLLMGroupMutation(engine, "update"); err != nil {
		return err
	}
	st, err := e.state(engine)
	if err != nil {
		return err
	}
	st.opMu.Lock()
	defer st.opMu.Unlock()
	ctx, finish, err := e.beginVLLMMutation(ctx, st)
	if err != nil {
		return err
	}
	defer finish()
	record, err := readVLLMRuntimeRecord(st)
	if err != nil {
		return err
	}
	if record.Activating != nil {
		if err := e.reconcileManagedVLLMActivation(ctx, st); err != nil {
			return err
		}
		record, err = readVLLMRuntimeRecord(st)
		if err != nil {
			return err
		}
	}
	st.mu.Lock()
	port := st.port
	st.mu.Unlock()
	if record.Active != "" && !record.Removing {
		if _, _, observeErr := e.observeManagedVLLMRecordedListener(ctx, st, record); observeErr != nil {
			return observeErr
		}
	}
	e.reconcilePresence(ctx, engine, st, record.Active != "" && !record.Removing, port, false)
	if err := adoptedVLLMMutationError(st, "update"); err != nil {
		return err
	}
	if record.Removing || record.Active == "" {
		return fmt.Errorf("vLLM is not installed; use Install before Update")
	}
	cli, receipt, err := validateVLLMEnvironment(st, record.Active)
	if err != nil {
		return err
	}
	if err = verifyManagedVLLMVersion(ctx, st, record.Active, receipt); err != nil {
		return err
	}
	st.mu.Lock()
	legacyRunning := receipt.Schema == vllmLegacyReceiptSchema && st.running
	st.installed, st.binPath, st.version = true, cli, receipt.Version
	st.mu.Unlock()
	if legacyRunning {
		return fmt.Errorf("stop the retained vLLM %s runtime before updating to %s", receipt.Version, managedVLLMVersion)
	}
	if supported, reason := installSupport(engine, st.plat); !supported {
		return fmt.Errorf("engine %q update is unavailable: %s", engine, reason)
	}
	return e.installManagedVLLM(ctx, st, true)
}

func (e *Executor) uninstallManagedVLLM(ctx context.Context, st *engineState) error {
	return e.uninstallManagedVLLMWithIO(ctx, st, os.RemoveAll, func(record vllmRuntimeRecord) error {
		return writeVLLMRuntimeRecord(st, record)
	})
}

func (e *Executor) uninstallManagedVLLMWithIO(ctx context.Context, st *engineState, remove func(string) error, write func(vllmRuntimeRecord) error) error {
	record, err := readVLLMRuntimeRecord(st)
	if err != nil {
		return err
	}
	if !record.Removing && record.Staged != "" {
		record, err = e.reconcileManagedVLLMStage(st, record)
		if err != nil {
			return err
		}
	}
	ids := vllmRecordedIDs(record)
	if len(ids) == 0 {
		return removeManagedVLLMCache(st, remove)
	}
	if !record.Removing {
		proofs := make(map[string]vllmRuntimeReceipt, len(ids))
		for _, id := range ids {
			_, receipt, err := validateVLLMEnvironment(st, id)
			if err != nil {
				return err
			}
			proofs[id] = receipt
		}
		st.mu.Lock()
		running, adopted := st.running, st.adopted
		st.mu.Unlock()
		if adopted {
			return adoptedVLLMMutationError(st, "uninstall")
		}
		if running {
			if err := e.doStop(st, "vllm"); err != nil {
				return err
			}
		}
		if err := e.ensureManagedVLLMPortFree(ctx, st); err != nil {
			return err
		}
		record.Removing = true
		record.RemovalProofs = proofs
		if err := write(record); err != nil {
			return err
		}
	} else {
		st.mu.Lock()
		active := st.proc != nil || st.running || st.adopted
		st.mu.Unlock()
		if active {
			return fmt.Errorf("cannot resume vLLM removal while process ownership is active or unproved")
		}
		if err := e.ensureManagedVLLMRemovalPortFree(ctx, st, record); err != nil {
			return err
		}
		for _, id := range ids {
			if err := validateVLLMReceiptMetadata(st, id, record.RemovalProofs[id]); err != nil {
				return err
			}
		}
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := removeManagedVLLMEnvironment(st, id, remove); err != nil {
			return fmt.Errorf("owned vLLM runtime removal did not complete: %w", err)
		}
	}
	if err := write(vllmRuntimeRecord{Schema: vllmRuntimeRecordSchema}); err != nil {
		return err
	}
	st.mu.Lock()
	st.installed, st.binPath, st.version = false, "", ""
	st.mu.Unlock()
	return removeManagedVLLMCache(st, remove)
}

func removeManagedVLLMEnvironment(st *engineState, id string, remove func(string) error) error {
	dir, err := vllmEnvironmentPath(st, id)
	if err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to remove a non-directory vLLM ownership path")
	}
	return remove(dir)
}
