// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// PAIR fetches the Qwen3.8 recipe's immutable artifacts into its own stable
// cache and publishes only files whose exact byte count and SHA-256 match the
// embedded recipe.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	vllmQwen38PreparedPhase    = "runtime-prepared"
	vllmQwen38CacheReceiptFile = "pair-qwen38-cache.json"
	vllmQwen38PrepareTimeout   = 6 * time.Hour
)

type vllmQwen38ProviderMismatch struct {
	Name            string `json:"name"`
	Reason          string `json:"reason"`
	ExpectedPath    string `json:"expectedPath,omitempty"`
	ObservedPath    string `json:"observedPath,omitempty"`
	ExpectedSHA256  string `json:"expectedSha256,omitempty"`
	ObservedSHA256  string `json:"observedSha256,omitempty"`
	ExpectedBytes   int64  `json:"expectedBytes,omitempty"`
	ObservedBytes   int64  `json:"observedBytes,omitempty"`
	ExpectedPackage string `json:"expectedPackage,omitempty"`
	ObservedPackage string `json:"observedPackage,omitempty"`
}

type vllmQwen38ProviderObservation struct {
	Qualified             bool                         `json:"qualified"`
	ProfileID             string                       `json:"profileId,omitempty"`
	ExpectedClosureSHA256 string                       `json:"expectedClosureSha256"`
	ObservedClosureSHA256 string                       `json:"observedClosureSha256,omitempty"`
	AllowedClosureSHA256  []string                     `json:"allowedClosureSha256"`
	Mismatches            []vllmQwen38ProviderMismatch `json:"mismatches"`
}

type vllmQwen38PreparedArtifact struct {
	Filename string `json:"filename"`
	SHA256   string `json:"sha256"`
	Bytes    int64  `json:"bytes"`
}

type vllmQwen38PreparedBundleReceipt struct {
	Schema                int                           `json:"schema"`
	Phase                 string                        `json:"phase"`
	RecipeID              string                        `json:"recipeId"`
	RecipeSHA256          string                        `json:"recipeSha256"`
	ArtifactClosureSHA256 string                        `json:"artifactClosureSha256"`
	ArtifactCount         int                           `json:"artifactCount"`
	ArtifactBytes         int64                         `json:"artifactBytes"`
	PreparedAt            string                        `json:"preparedAt"`
	LaunchReceiptSHA256   string                        `json:"launchReceiptSha256,omitempty"`
	Provider              vllmQwen38ProviderObservation `json:"provider"`
	Artifacts             []vllmQwen38PreparedArtifact  `json:"artifacts"`
}

type vllmQwen38ProviderIO struct {
	hashFile        func(context.Context, string) (string, int64, error)
	packageIdentity func(context.Context, string) (string, error)
}

func qwen38HashObservedFile(ctx context.Context, path string) (string, int64, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 {
		return "", 0, errors.New("Qwen3.8 provider file is unavailable or redirected")
	}
	digest, err := qwen38HashFile(ctx, path, info.Size())
	return digest, info.Size(), err
}

func qwen38HashFile(ctx context.Context, path string, expected int64) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != expected {
		return "", errors.New("Qwen3.8 artifact is missing, redirected or has the wrong size")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	buffer := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			_, _ = hash.Write(buffer[:n])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func qwen38PackageIdentity(ctx context.Context, name string) (string, error) {
	command := exec.CommandContext(ctx, "/usr/bin/dpkg-query", "-W", "-f=${Status}|${Version}|${Architecture}", name)
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func observeQwen38ProviderProfile(ctx context.Context, profile vllmQwen38ProviderProfile, providerIO vllmQwen38ProviderIO) vllmQwen38ProviderObservation {
	result := vllmQwen38ProviderObservation{ProfileID: profile.ID, ExpectedClosureSHA256: profile.ClosureSHA256,
		AllowedClosureSHA256: []string{}, Mismatches: []vllmQwen38ProviderMismatch{}}
	closure := sha256.New()
	complete := true
	for _, provider := range profile.Providers {
		observedPath := provider.Path
		if _, err := os.Lstat(observedPath); os.IsNotExist(err) {
			stable := filepath.Join(filepath.Dir(provider.Path), provider.Name)
			if resolved, resolveErr := filepath.EvalSymlinks(stable); resolveErr == nil {
				observedPath = resolved
			}
		}
		observedSHA, observedBytes, hashErr := providerIO.hashFile(ctx, observedPath)
		if hashErr != nil {
			complete = false
			result.Mismatches = append(result.Mismatches, vllmQwen38ProviderMismatch{Name: provider.Name, Reason: "provider file is unavailable or differs", ExpectedPath: provider.Path, ObservedPath: observedPath, ExpectedSHA256: provider.SHA256, ExpectedBytes: provider.Bytes, ObservedBytes: observedBytes})
		}
		fmt.Fprintf(closure, "%s\x00%s\x00%d\x00%s\n", provider.Name, observedPath, observedBytes, observedSHA)
		if hashErr == nil && (observedPath != provider.Path || observedBytes != provider.Bytes || observedSHA != provider.SHA256) {
			result.Mismatches = append(result.Mismatches, vllmQwen38ProviderMismatch{Name: provider.Name, Reason: "provider file identity differs", ExpectedPath: provider.Path, ObservedPath: observedPath, ExpectedSHA256: provider.SHA256, ObservedSHA256: observedSHA, ExpectedBytes: provider.Bytes, ObservedBytes: observedBytes})
		}
		for _, pkg := range provider.Packages {
			identity, packageErr := providerIO.packageIdentity(ctx, pkg.Name)
			if packageErr != nil {
				complete = false
			}
			fmt.Fprintf(closure, "%s\x00%s\n", pkg.Name, identity)
			if packageErr != nil || identity != pkg.Identity {
				result.Mismatches = append(result.Mismatches, vllmQwen38ProviderMismatch{Name: provider.Name, Reason: "provider package identity differs", ExpectedPackage: pkg.Name + "=" + pkg.Identity, ObservedPackage: pkg.Name + "=" + identity})
			}
		}
	}
	if complete {
		result.ObservedClosureSHA256 = hex.EncodeToString(closure.Sum(nil))
	}
	result.Qualified = complete && result.ObservedClosureSHA256 == result.ExpectedClosureSHA256 && len(result.Mismatches) == 0
	return result
}

func observeQwen38Providers(ctx context.Context, recipe vllmQwen38Recipe, providerIO vllmQwen38ProviderIO) vllmQwen38ProviderObservation {
	return observeQwen38ProviderProfile(ctx, qwen38ReferenceProviderProfile(recipe), providerIO)
}

func (e *Executor) inspectQwen38Providers(ctx context.Context, recipe vllmQwen38Recipe) vllmQwen38ProviderObservation {
	providerIO := vllmQwen38ProviderIO{hashFile: qwen38HashObservedFile, packageIdentity: qwen38PackageIdentity}
	profiles := qwen38ProviderProfiles(recipe)
	allowed := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		allowed = append(allowed, profile.ClosureSHA256)
	}
	var result vllmQwen38ProviderObservation
	for _, profile := range profiles {
		result = observeQwen38ProviderProfile(ctx, profile, providerIO)
		result.AllowedClosureSHA256 = append([]string{}, allowed...)
		if result.Qualified {
			return result
		}
	}
	result.ProfileID, result.ExpectedClosureSHA256 = "", ""
	return result
}

func qwen38BundleRoot(st *engineState) (string, error) {
	root := filepath.Join(st.installDir, "runtime-artifacts", vllmQwen38RecipeSHA256)
	if err := validateManagedVLLMLexicalPath(st.installDir, root); err != nil {
		return "", err
	}
	return root, nil
}

func qwen38OwnedBundleDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !qwen38BundleOwnedMode(info, true) {
		return errors.New("Qwen3.8 runtime cache is not an exact PAIR-owned directory")
	}
	return nil
}

func qwen38PartialSize(path string) (int64, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !qwen38BundleOwnedMode(info, false) {
		return 0, true, errors.New("Qwen3.8 partial artifact is not an exact PAIR-owned file")
	}
	return info.Size(), true, nil
}

func qwen38RetainedBundleBytes(root string, recipe vllmQwen38Recipe) (int64, error) {
	var retained int64
	for _, artifact := range recipe.Runtime.Artifacts {
		best := int64(0)
		for _, suffix := range []string{"", ".part"} {
			size, exists, err := qwen38PartialSize(filepath.Join(root, artifact.Filename+suffix))
			if err != nil {
				return 0, err
			}
			if exists {
				if size < 0 || size > artifact.Bytes {
					return 0, errors.New("Qwen3.8 retained artifact exceeds its sealed size")
				}
				if size > best {
					best = size
				}
			}
		}
		retained += best
	}
	return retained, nil
}

func qwen38ContentRangeMatches(value string, offset, total int64) bool {
	prefix := "bytes " + strconv.FormatInt(offset, 10) + "-"
	suffix := "/" + strconv.FormatInt(total, 10)
	return strings.HasPrefix(value, prefix) && strings.HasSuffix(value, suffix)
}

func (e *Executor) fetchQwen38Artifact(ctx context.Context, root string, artifact vllmQwen38RecipeArtifact, onBytes func(int64)) (bool, error) {
	final := filepath.Join(root, artifact.Filename)
	partial := final + ".part"
	for _, path := range []string{final, partial} {
		if err := validateManagedVLLMLexicalPath(root, path); err != nil {
			return false, err
		}
	}
	if _, exists, err := qwen38PartialSize(final); err != nil {
		return false, err
	} else if exists {
		digest, hashErr := qwen38HashFile(ctx, final, artifact.Bytes)
		if hashErr == nil && digest == artifact.SHA256 {
			onBytes(artifact.Bytes)
			return true, nil
		}
		if removeErr := os.Remove(final); removeErr != nil {
			return false, errors.New("changed Qwen3.8 cached artifact could not be removed")
		}
	}
	offset, resumed, err := qwen38PartialSize(partial)
	if err != nil {
		return resumed, err
	}
	if offset < 0 || offset > artifact.Bytes {
		if err := os.Remove(partial); err != nil && !os.IsNotExist(err) {
			return resumed, err
		}
		offset = 0
	}
	if offset == artifact.Bytes {
		digest, hashErr := qwen38HashFile(ctx, partial, artifact.Bytes)
		if hashErr == nil && digest == artifact.SHA256 {
			if err := os.Rename(partial, final); err != nil {
				return resumed, err
			}
			onBytes(artifact.Bytes)
			return true, nil
		}
		if err := os.Remove(partial); err != nil {
			return resumed, err
		}
		offset = 0
	}
	if err := validateDownloadURL(artifact.URL); err != nil {
		return resumed, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, artifact.URL, nil)
	if err != nil {
		return resumed, err
	}
	request.Header.Set("Accept-Encoding", "identity")
	if offset > 0 {
		request.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	response, err := e.client.Do(request)
	if err != nil {
		return resumed, err
	}
	defer response.Body.Close()
	if offset > 0 && response.StatusCode == http.StatusOK {
		offset = 0
	} else if offset > 0 && (response.StatusCode != http.StatusPartialContent || !qwen38ContentRangeMatches(response.Header.Get("Content-Range"), offset, artifact.Bytes)) {
		return resumed, fmt.Errorf("Qwen3.8 artifact %s did not honor its exact resume range", artifact.Filename)
	} else if offset == 0 && response.StatusCode != http.StatusOK {
		return resumed, fmt.Errorf("Qwen3.8 artifact %s returned HTTP %d", artifact.Filename, response.StatusCode)
	}
	file, err := os.OpenFile(partial, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return resumed, err
	}
	if err = file.Chmod(0o600); err == nil {
		err = file.Truncate(offset)
	}
	if err == nil {
		_, err = file.Seek(offset, io.SeekStart)
	}
	remaining := artifact.Bytes - offset
	written := int64(0)
	if err == nil {
		buffer := make([]byte, 1<<20)
		for written <= remaining {
			if contextErr := ctx.Err(); contextErr != nil {
				err = contextErr
				break
			}
			limit := int64(len(buffer))
			if left := remaining + 1 - written; left < limit {
				limit = left
			}
			if limit <= 0 {
				break
			}
			n, readErr := response.Body.Read(buffer[:limit])
			if n > 0 {
				if _, writeErr := file.Write(buffer[:n]); writeErr != nil {
					err = writeErr
					break
				}
				written += int64(n)
				onBytes(int64(n))
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				err = readErr
				break
			}
		}
	}
	if syncErr := file.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return resumed || offset > 0 || written > 0, err
	}
	if written != remaining {
		return resumed || offset > 0, fmt.Errorf("Qwen3.8 artifact %s ended after %d of %d bytes", artifact.Filename, offset+written, artifact.Bytes)
	}
	digest, err := qwen38HashFile(ctx, partial, artifact.Bytes)
	if err != nil || digest != artifact.SHA256 {
		_ = os.Remove(partial)
		return resumed, fmt.Errorf("Qwen3.8 artifact %s failed exact SHA-256 verification", artifact.Filename)
	}
	if err := os.Rename(partial, final); err != nil {
		return resumed, err
	}
	return resumed || offset > 0, nil
}

func (e *Executor) ensureQwen38RuntimeBundle(ctx context.Context, st *engineState, recipe vllmQwen38Recipe, provider vllmQwen38ProviderObservation) (string, vllmQwen38PreparedBundleReceipt, bool, error) {
	var receipt vllmQwen38PreparedBundleReceipt
	root, err := qwen38BundleRoot(st)
	if err != nil {
		return "", receipt, false, err
	}
	if err = os.MkdirAll(root, 0o700); err != nil {
		return "", receipt, false, err
	}
	if err = os.Chmod(root, 0o700); err != nil {
		return "", receipt, false, err
	}
	if err = qwen38OwnedBundleDirectory(root); err != nil {
		return "", receipt, false, err
	}
	retained, err := qwen38RetainedBundleBytes(root, recipe)
	if err != nil || retained > recipe.Runtime.ArtifactBytes || retained > recipe.Install.Limits.StageMaxBytes {
		return "", receipt, false, errors.New("Qwen3.8 retained runtime cache exceeds its sealed stage limit")
	}
	available, _, err := e.vllmStorageFilesystem()(root)
	remaining := uint64(recipe.Runtime.ArtifactBytes - retained)
	if err != nil || available < remaining+uint64(recipe.Install.Limits.FreeReserveBytes) {
		return "", receipt, false, errors.New("Qwen3.8 runtime cache cannot preserve the sealed free-space reserve")
	}
	completed := int64(0)
	lastPercent := -1
	emit := func(bytes int64) {
		completed += bytes
		percent := int(completed * 100 / recipe.Runtime.ArtifactBytes)
		if percent > 99 {
			percent = 99
		}
		if percent != lastPercent {
			lastPercent = percent
			e.emitInstallProgress("vllm", "preparing-qwen38-runtime", percent)
		}
	}
	artifacts := make([]vllmQwen38PreparedArtifact, 0, len(recipe.Runtime.Artifacts))
	resumed := false
	for _, artifact := range recipe.Runtime.Artifacts {
		usedExisting, fetchErr := e.fetchQwen38Artifact(ctx, root, artifact, emit)
		if fetchErr != nil {
			return "", receipt, resumed || usedExisting, fetchErr
		}
		resumed = resumed || usedExisting
		artifacts = append(artifacts, vllmQwen38PreparedArtifact{Filename: artifact.Filename, SHA256: artifact.SHA256, Bytes: artifact.Bytes})
	}
	receipt = vllmQwen38PreparedBundleReceipt{Schema: 1, Phase: vllmQwen38PreparedPhase, RecipeID: recipe.ID, RecipeSHA256: vllmQwen38RecipeSHA256, ArtifactClosureSHA256: vllmQwen38ArtifactClosureSHA256, ArtifactCount: len(artifacts), ArtifactBytes: recipe.Runtime.ArtifactBytes, PreparedAt: time.Now().UTC().Format(time.RFC3339Nano), Provider: provider, Artifacts: artifacts}
	if err = writeManagedVLLMJSON(st.installDir, filepath.Join(root, vllmQwen38CacheReceiptFile), receipt); err != nil {
		return "", vllmQwen38PreparedBundleReceipt{}, resumed, err
	}
	e.emitInstallProgress("vllm", "qwen38-runtime-cached", 100)
	return root, receipt, resumed, nil
}
