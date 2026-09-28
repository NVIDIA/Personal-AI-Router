// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

const onboardingArtifactLimit int64 = 1 << 30

var onboardingBinaries = []string{"nvpair-ui-broker", "nvpair-node-scanner", "nvpair-node-info", "nvpair-cluster-manager", "nvpair-engine-manager", "nvpair-node-settings", "nvpair-errors", "nvpair-manual-nodes", "nvpair-workload-manager", "nvpair-job-scheduler", "nvpair-tui", "nvpair-proxy"}

var onboardingLegacyBinaries = []string{"nvpair-ui-broker", "nvpair-node-scanner", "nvpair-node-info", "nvpair-cluster-manager", "nvpair-engine-manager", "nvpair-node-settings", "nvpair-errors", "nvpair-manual-nodes", "nvpair-workload-manager", "nvpair-job-scheduler", "nvpair-tui", "ollama-proxy", "lmstudio-proxy"}

func onboardingSupportedBinaries(count int) []string {
	switch count {
	case 12:
		return append([]string(nil), onboardingBinaries...)
	case 13:
		return append([]string(nil), onboardingLegacyBinaries...)
	case 15:
		return append(append([]string(nil), onboardingLegacyBinaries...), "llamacpp-proxy", "vllm-proxy")
	default:
		return nil
	}
}

type onboardingArtifactSource struct {
	onboardingArtifact
	File string `json:"file,omitempty"`
	URL  string `json:"url,omitempty"`
}
type onboardingCatalog struct {
	Artifacts []onboardingArtifactSource `json:"artifacts"`
}
type onboardingPackage struct {
	source      onboardingArtifactSource
	file        string
	archiveRoot string
	bytes       int64
}
type onboardingBuildManifest struct {
	Source            string            `json:"source"`
	SourceFingerprint string            `json:"sourceFingerprint"`
	Services          string            `json:"services"`
	Platform          string            `json:"platform"`
	Arch              string            `json:"arch"`
	Components        map[string]string `json:"components"`
	BuiltAt           string            `json:"builtAt"`
	Files             []struct {
		FileName string `json:"fileName"`
		Size     int64  `json:"size"`
		SHA256   string `json:"sha256"`
	} `json:"files"`
}

func onboardingArch(arch string) string {
	if arch == "x64" {
		return "amd64"
	}
	return arch
}

// Import is an explicit in-product selection, not a renderer supplied URL or
// shell recipe. Snapshot once into owned cache before deriving public metadata.
func (s *onboardingService) importArtifact(ctx context.Context, file string) (onboardingArtifact, error) {
	if !onboardingLocalArtifactPath(file) {
		return onboardingArtifact{}, errors.New("select an absolute local package file; network and device paths are not accepted")
	}
	before, err := os.Lstat(file)
	if err != nil || !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > onboardingArtifactLimit {
		return onboardingArtifact{}, errors.New("selected package is not an available bounded regular file")
	}
	source, err := os.Open(file)
	if err != nil {
		return onboardingArtifact{}, errors.New("selected package is unavailable")
	}
	defer source.Close()
	after, err := source.Stat()
	if err != nil || !os.SameFile(before, after) {
		return onboardingArtifact{}, errors.New("selected package changed before snapshot")
	}
	dir := filepath.Join(s.m.exec.baseDir, "onboarding-artifacts")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return onboardingArtifact{}, errors.New("package cache is unavailable")
	}
	tmp, err := os.CreateTemp(dir, ".import-")
	if err != nil {
		return onboardingArtifact{}, errors.New("package snapshot could not be created")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	hash := sha256.New()
	buffer := make([]byte, 1<<20)
	var copied int64
	for {
		if err = ctx.Err(); err != nil {
			break
		}
		var n int
		n, err = source.Read(buffer)
		if n > 0 {
			copied += int64(n)
			if copied > onboardingArtifactLimit {
				err = errors.New("package exceeds limit")
				break
			}
			_, errWrite := io.MultiWriter(tmp, hash).Write(buffer[:n])
			if errWrite != nil {
				err = errWrite
				break
			}
		}
		if err == io.EOF {
			err = nil
			break
		}
		if err != nil {
			break
		}
	}
	closeErr := tmp.Close()
	if err != nil || closeErr != nil || copied != before.Size() {
		return onboardingArtifact{}, errors.New("selected package snapshot did not complete")
	}
	manifest, err := onboardingArchiveManifest(tmpName)
	if err != nil {
		return onboardingArtifact{}, err
	}
	artifact := onboardingArtifact{ArtifactID: "import-" + hex.EncodeToString(hash.Sum(nil))[:24], Version: manifest.Services, Platform: manifest.Platform, Arch: onboardingArch(manifest.Arch), SHA256: hex.EncodeToString(hash.Sum(nil)), Provenance: "engineering", SourceFingerprint: manifest.SourceFingerprint}
	if !onboardingToken.MatchString(artifact.Version) || artifact.Platform != "linux" || (artifact.Arch != "arm64" && artifact.Arch != "amd64") {
		return onboardingArtifact{}, errors.New("selected package has no supported native Linux identity")
	}
	if _, _, err = verifyOnboardingArchive(tmpName, artifact); err != nil {
		return onboardingArtifact{}, err
	}
	final := filepath.Join(dir, artifact.SHA256+".tar.gz")
	if _, err = os.Lstat(final); errors.Is(err, os.ErrNotExist) {
		err = os.Rename(tmpName, final)
	} else if err == nil {
		_, _, err = verifyOnboardingArchive(final, artifact)
	}
	if err != nil {
		return onboardingArtifact{}, errors.New("verified package snapshot could not be retained")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.imports) >= 8 {
		if _, exists := s.imports[artifact.ArtifactID]; !exists {
			return onboardingArtifact{}, errors.New("at most eight reviewed local packages may be retained per session")
		}
	}
	s.imports[artifact.ArtifactID] = onboardingArtifactSource{onboardingArtifact: artifact, File: final}
	return artifact, nil
}
func onboardingArchiveManifest(file string) (onboardingBuildManifest, error) {
	var manifest onboardingBuildManifest
	f, err := os.Open(file)
	if err != nil {
		return manifest, errors.New("package snapshot is unavailable")
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return manifest, errors.New("select the portable PAIR tar.gz bundle")
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	var total int64
	for count := 0; count < 4096; count++ {
		h, e := reader.Next()
		if e != nil {
			break
		}
		if h.Size < 0 || h.Size > 256<<20 {
			return manifest, errors.New("package component exceeds limit")
		}
		total += h.Size
		if total > 2<<30 {
			return manifest, errors.New("expanded package exceeds limit")
		}
		parts := strings.Split(h.Name, "/")
		if len(parts) == 3 && parts[1] == "bin" && parts[2] == "manifest.json" && h.Typeflag == tar.TypeReg && h.Size <= 128<<10 {
			raw, e := io.ReadAll(io.LimitReader(reader, (128<<10)+1))
			if e == nil && json.Unmarshal(raw, &manifest) == nil && manifest.Source == "services-build" {
				return manifest, nil
			}
			break
		}
	}
	return manifest, errors.New("selected package lacks the normal PAIR build manifest")
}
func readOnboardingFile(file string, max int64) ([]byte, error) {
	before, err := os.Lstat(file)
	if err != nil || !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > max {
		return nil, errors.New("required file is not an available bounded regular file")
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, errors.New("required file is unavailable")
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Size() > max {
		return nil, errors.New("required file changed before reading")
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(data)) > max {
		return nil, errors.New("required file exceeds its read bound")
	}
	return data, nil
}

func readOnboardingCatalog(base string) (onboardingCatalog, error) {
	var result onboardingCatalog
	locations := []string{filepath.Join(base, "onboarding-catalog.json")}
	if exe, err := os.Executable(); err == nil {
		locations = append(locations, filepath.Join(filepath.Dir(exe), "onboarding-catalog.json"))
	}
	for _, location := range locations {
		f, err := os.Open(location)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return result, errors.New("PAIR artifact catalog is unavailable")
		}
		data, err := io.ReadAll(io.LimitReader(f, (128<<10)+1))
		_ = f.Close()
		if err != nil || len(data) > 128<<10 {
			return result, errors.New("PAIR artifact catalog exceeds its limit")
		}
		if onboardingDecode(data, &result) != nil || len(result.Artifacts) > 32 {
			return result, errors.New("PAIR artifact catalog is invalid")
		}
		seen := map[string]bool{}
		for _, artifact := range result.Artifacts {
			if !onboardingToken.MatchString(artifact.ArtifactID) || seen[artifact.ArtifactID] || !onboardingToken.MatchString(artifact.Version) || !onboardingSHA.MatchString(artifact.SHA256) || artifact.Platform != "linux" || (artifact.Arch != "arm64" && artifact.Arch != "amd64") || (artifact.Provenance != "official-release" && artifact.Provenance != "engineering") {
				return result, errors.New("PAIR artifact catalog has an unsupported identity")
			}
			seen[artifact.ArtifactID] = true
			if (artifact.File == "") == (artifact.URL == "") {
				return result, errors.New("each PAIR artifact needs one backend-owned source")
			}
			if artifact.File != "" && !filepath.IsAbs(artifact.File) {
				return result, errors.New("artifact file must be absolute")
			}
			if artifact.URL != "" {
				u, e := url.Parse(artifact.URL)
				if e != nil || u.Scheme != "https" || u.User != nil || u.Host == "" {
					return result, errors.New("artifact download requires HTTPS without credentials")
				}
			}
		}
		return result, nil
	}
	return result, nil
}
func verifyOnboardingArchive(file string, artifact onboardingArtifact) (string, int64, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", 0, errors.New("PAIR package file is unavailable")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() <= 0 || st.Size() > onboardingArtifactLimit {
		return "", 0, errors.New("PAIR package is not a bounded regular file")
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", 0, err
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), artifact.SHA256) {
		return "", 0, errors.New("PAIR package checksum does not match the selected artifact")
	}
	if _, err = f.Seek(0, 0); err != nil {
		return "", 0, err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", 0, errors.New("PAIR package must be the portable tar.gz bundle")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	root := ""
	seen := map[string]bool{}
	binaries := map[string]bool{}
	actualHashes := map[string]string{}
	actualSizes := map[string]int64{}
	var manifestData []byte
	var total int64
	for count := 0; ; count++ {
		header, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil || count > 4096 {
			return "", 0, errors.New("PAIR package archive is invalid or oversized")
		}
		name := strings.TrimSuffix(header.Name, "/")
		parts := strings.Split(name, "/")
		if name == "" || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") || path.Clean(name) != name || len(parts) < 1 || parts[0] == ".." || seen[name] {
			return "", 0, errors.New("PAIR package contains an unsafe or duplicate path")
		}
		if root == "" {
			root = parts[0]
		}
		if parts[0] != root {
			return "", 0, errors.New("PAIR package must have one top-level directory")
		}
		seen[name] = true
		if header.Mode&06000 != 0 {
			return "", 0, errors.New("PAIR package must not contain privileged file modes")
		}
		if header.Typeflag != tar.TypeDir && header.Typeflag != tar.TypeReg {
			return "", 0, errors.New("PAIR package links and special files are not accepted")
		}
		if header.Size < 0 || header.Size > 256<<20 {
			return "", 0, errors.New("PAIR component exceeds size limit")
		}
		total += header.Size
		if total > 2<<30 {
			return "", 0, errors.New("PAIR package expanded size exceeds limit")
		}
		if len(parts) == 3 && parts[1] == "bin" && header.Typeflag == tar.TypeReg {
			if parts[2] == "manifest.json" {
				if header.Size > 128<<10 {
					return "", 0, errors.New("PAIR manifest exceeds limit")
				}
				manifestData, e = io.ReadAll(io.LimitReader(tr, (128<<10)+1))
				if e != nil {
					return "", 0, e
				}
				continue
			}
			if header.Mode&0111 == 0 {
				return "", 0, errors.New("PAIR binary is not executable")
			}
			binaries[parts[2]] = true
			var prefix [20]byte
			if _, err := io.ReadFull(tr, prefix[:]); err != nil || !onboardingELF(prefix[:], artifact.Arch) {
				return "", 0, errors.New("PAIR binary ELF architecture does not match the reviewed target")
			}
			componentHash := sha256.New()
			_, _ = componentHash.Write(prefix[:])
			if _, e = io.Copy(componentHash, tr); e != nil {
				return "", 0, e
			}
			actualHashes[parts[2]] = hex.EncodeToString(componentHash.Sum(nil))
			actualSizes[parts[2]] = header.Size
		}
		if len(parts) > 3 && parts[1] == "bin" {
			return "", 0, errors.New("PAIR package has unexpected nested binary content")
		}
	}
	if len(binaries) != len(onboardingBinaries) {
		return "", 0, errors.New("PAIR package must contain the complete application binary set")
	}
	for _, name := range onboardingBinaries {
		if !binaries[name] {
			return "", 0, errors.New("PAIR package is missing a required product binary")
		}
	}
	var manifest onboardingBuildManifest
	if len(manifestData) == 0 || json.Unmarshal(manifestData, &manifest) != nil || manifest.Source != "services-build" || manifest.Platform != artifact.Platform || onboardingArch(manifest.Arch) != artifact.Arch || manifest.Services != artifact.Version || len(manifest.Files) != len(onboardingBinaries) {
		return "", 0, errors.New("PAIR package lacks a matching normal build manifest")
	}
	manifestNames := map[string]bool{}
	for _, entry := range manifest.Files {
		if manifestNames[entry.FileName] || actualHashes[entry.FileName] == "" || !strings.EqualFold(actualHashes[entry.FileName], entry.SHA256) || actualSizes[entry.FileName] != entry.Size {
			return "", 0, errors.New("PAIR manifest component identity does not match the archived binary")
		}
		manifestNames[entry.FileName] = true
	}
	return root, st.Size(), nil
}
func prepareOnboardingPackage(ctx context.Context, base string, artifact onboardingArtifactSource) (onboardingPackage, error) {
	file := artifact.File
	if file == "" {
		dir := filepath.Join(base, "onboarding-artifacts")
		if err := os.MkdirAll(dir, 0700); err != nil {
			return onboardingPackage{}, err
		}
		file = filepath.Join(dir, strings.ToLower(artifact.SHA256)+".tar.gz")
		if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, artifact.URL, nil)
			if err != nil {
				return onboardingPackage{}, errors.New("invalid artifact download request")
			}
			client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) > 5 || req.URL.Scheme != "https" || req.URL.User != nil {
					return errors.New("unsafe artifact redirect")
				}
				return nil
			}}
			response, err := client.Do(request)
			if err != nil {
				return onboardingPackage{}, errors.New("PAIR artifact download failed")
			}
			defer response.Body.Close()
			if response.StatusCode != 200 {
				return onboardingPackage{}, errors.New("PAIR artifact source did not return a package")
			}
			tmp, err := os.CreateTemp(dir, ".download-")
			if err != nil {
				return onboardingPackage{}, err
			}
			tmpName := tmp.Name()
			defer os.Remove(tmpName)
			n, copyErr := io.Copy(tmp, io.LimitReader(response.Body, onboardingArtifactLimit+1))
			closeErr := tmp.Close()
			if copyErr != nil || closeErr != nil || n > onboardingArtifactLimit {
				return onboardingPackage{}, errors.New("PAIR artifact download incomplete or oversized")
			}
			if _, _, err = verifyOnboardingArchive(tmpName, artifact.onboardingArtifact); err != nil {
				return onboardingPackage{}, err
			}
			if err = os.Rename(tmpName, file); err != nil {
				return onboardingPackage{}, err
			}
		}
	}
	root, size, err := verifyOnboardingArchive(file, artifact.onboardingArtifact)
	if err != nil {
		return onboardingPackage{}, err
	}
	return onboardingPackage{source: artifact, file: file, archiveRoot: root, bytes: size}, nil
}

func onboardingMarshal(v any) []byte { data, _ := json.Marshal(v); return data }

func onboardingELF(data []byte, arch string) bool {
	if len(data) < 20 || string(data[:4]) != "\x7fELF" || data[4] != 2 || data[5] != 1 {
		return false
	}
	machine := binary.LittleEndian.Uint16(data[18:20])
	return (arch == "arm64" && machine == 183) || (arch == "amd64" && machine == 62)
}

// Reuse the shipped build manifest and exact sibling set for homogeneous
// Linux fleets. No separate catalog or download is needed for this path.
func onboardingSelfPackage(base, bundle string) (onboardingArtifactSource, error) {
	return onboardingPackageBundle(base, bundle, runtime.GOOS, runtime.GOARCH)
}
func onboardingPackageBundle(base, bundle, platform, arch string) (onboardingArtifactSource, error) {
	var result onboardingArtifactSource
	manifestPath := filepath.Join(bundle, "manifest.json")
	if _, err := os.Stat(manifestPath); errors.Is(err, os.ErrNotExist) {
		manifestPath = filepath.Join(filepath.Dir(bundle), "manifest.json")
	}
	data, err := readOnboardingFile(manifestPath, 128<<10)
	if err != nil {
		return result, errors.New("the installed bundle has no verified build manifest")
	}
	var manifest onboardingBuildManifest
	if len(data) > 128<<10 || json.Unmarshal(data, &manifest) != nil || manifest.Source != "services-build" || manifest.Platform != "linux" || platform != "linux" || onboardingArch(manifest.Arch) != onboardingArch(arch) || !onboardingToken.MatchString(manifest.Services) {
		return result, errors.New("the installed manifest is not a matching complete Linux bundle")
	}
	expected := map[string]bool{}
	for _, name := range onboardingBinaries {
		expected[name] = true
	}
	allowedBundledTools := map[string]bool{
		"nvpair-host-bootstrap": true,
		"nvpair-host-helper":    true,
	}
	seen := map[string]bool{}
	selectedFiles := manifest.Files[:0:0]
	dir := filepath.Join(base, "onboarding-artifacts")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return result, err
	}
	tmp, err := os.CreateTemp(dir, ".self-")
	if err != nil {
		return result, err
	}
	name := tmp.Name()
	defer func() { _ = tmp.Close(); _ = os.Remove(name) }()
	hash := sha256.New()
	gz := gzip.NewWriter(io.MultiWriter(tmp, hash))
	tw := tar.NewWriter(gz)
	archiveRoot := "NVIDIA-Personal-AI-Router-" + manifest.Services
	for _, file := range manifest.Files {
		if seen[file.FileName] || (!expected[file.FileName] && !allowedBundledTools[file.FileName]) || file.Size <= 0 || file.Size > 256<<20 || !onboardingSHA.MatchString(file.SHA256) {
			return result, errors.New("installed binary inventory is invalid")
		}
		seen[file.FileName] = true
		body, e := readOnboardingFile(filepath.Join(bundle, file.FileName), 256<<20)
		if e != nil || int64(len(body)) != file.Size {
			return result, errors.New("installed component changed during snapshot")
		}
		sum := sha256.Sum256(body)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), file.SHA256) || !onboardingELF(body, onboardingArch(manifest.Arch)) {
			return result, errors.New("installed component hash or native architecture is inconsistent")
		}
		if allowedBundledTools[file.FileName] {
			continue
		}
		delete(expected, file.FileName)
		selectedFiles = append(selectedFiles, file)
		if e = tw.WriteHeader(&tar.Header{Name: archiveRoot + "/bin/" + file.FileName, Mode: 0755, Size: file.Size, Typeflag: tar.TypeReg}); e != nil {
			return result, e
		}
		if _, e = tw.Write(body); e != nil {
			return result, e
		}
	}
	if len(expected) != 0 {
		return result, errors.New("installed bundle is incomplete")
	}
	current, e := readOnboardingFile(manifestPath, 128<<10)
	if e != nil || !bytes.Equal(data, current) {
		return result, errors.New("installed manifest changed during snapshot")
	}
	packagedManifest := manifest
	packagedManifest.Files = selectedFiles
	if packagedManifest.Components != nil {
		components := map[string]string{}
		for _, file := range selectedFiles {
			if version, ok := packagedManifest.Components[file.FileName]; ok {
				components[file.FileName] = version
			}
		}
		packagedManifest.Components = components
	}
	packagedManifestData, e := json.Marshal(packagedManifest)
	if e != nil {
		return result, errors.New("installed manifest could not be filtered")
	}
	if e = tw.WriteHeader(&tar.Header{Name: archiveRoot + "/bin/manifest.json", Mode: 0644, Size: int64(len(packagedManifestData)), Typeflag: tar.TypeReg}); e != nil {
		return result, e
	}
	if _, e = tw.Write(packagedManifestData); e != nil {
		return result, e
	}
	if e = tw.Close(); e != nil {
		return result, e
	}
	if e = gz.Close(); e != nil {
		return result, e
	}
	if e = tmp.Close(); e != nil {
		return result, e
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	destination := filepath.Join(dir, sum+".tar.gz")
	if _, e = os.Stat(destination); errors.Is(e, os.ErrNotExist) {
		if e = os.Rename(name, destination); e != nil {
			return result, e
		}
	}
	return onboardingArtifactSource{onboardingArtifact: onboardingArtifact{ArtifactID: "installed-" + sum[:16], Version: manifest.Services, Platform: "linux", Arch: onboardingArch(manifest.Arch), SHA256: sum, Provenance: "engineering", SourceFingerprint: manifest.SourceFingerprint}, File: destination}, nil
}
