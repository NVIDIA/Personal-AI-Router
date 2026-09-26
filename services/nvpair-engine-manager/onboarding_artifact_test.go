// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type onboardingArchiveFixtureEntry struct {
	name string
	mode int64
	kind byte
	link string
	body []byte
}

func onboardingFixtureELF(machine uint16) []byte {
	data := make([]byte, 64)
	copy(data, "\x7fELF")
	data[4], data[5], data[6] = 2, 1, 1
	binary.LittleEndian.PutUint16(data[16:18], 2)
	binary.LittleEndian.PutUint16(data[18:20], machine)
	binary.LittleEndian.PutUint32(data[20:24], 1)
	binary.LittleEndian.PutUint16(data[52:54], 64)
	return data
}

func onboardingFixtureEntries(machine uint16) []onboardingArchiveFixtureEntry {
	entries := []onboardingArchiveFixtureEntry{}
	for _, name := range onboardingBinaries {
		entries = append(entries, onboardingArchiveFixtureEntry{
			name: "pair-test/bin/" + name, mode: 0755, kind: tar.TypeReg,
			body: onboardingFixtureELF(machine),
		})
	}
	arch := "arm64"
	if machine == 62 {
		arch = "amd64"
	}
	files := []map[string]any{}
	components := map[string]string{}
	for _, entry := range entries {
		name := filepath.Base(entry.name)
		sum := sha256.Sum256(entry.body)
		files = append(files, map[string]any{"fileName": name, "size": len(entry.body), "sha256": hex.EncodeToString(sum[:])})
		components[name] = "test"
	}
	manifest, err := json.Marshal(map[string]any{"source": "services-build", "sourceFingerprint": strings.Repeat("1", 64), "product": "test", "platform": "linux", "arch": arch, "components": components, "files": files, "builtAt": "2026-01-01T00:00:00Z"})
	if err != nil {
		panic(err)
	}
	entries = append(entries, onboardingArchiveFixtureEntry{name: "pair-test/bin/manifest.json", mode: 0644, kind: tar.TypeReg, body: manifest})
	return entries
}

func writeOnboardingFixture(t *testing.T, entries []onboardingArchiveFixtureEntry) (string, onboardingArtifact) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "package.tar.gz")
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: entry.mode, Typeflag: entry.kind, Linkname: entry.link}
		if entry.kind == tar.TypeReg {
			header.Size = int64(len(entry.body))
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if entry.kind == tar.TypeReg {
			if _, err := tw.Write(entry.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, close := range []func() error{tw.Close, gz.Close, f.Close} {
		if err := close(); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return file, onboardingArtifact{ArtifactID: "fixture", Version: "test", Platform: "linux", Arch: "arm64", SHA256: hex.EncodeToString(sum[:]), Provenance: "engineering"}
}

func TestOnboardingArtifactCompleteNativeInventoryAndLocalPreparation(t *testing.T) {
	for _, tc := range []struct {
		arch    string
		machine uint16
	}{{"arm64", 183}, {"amd64", 62}} {
		t.Run(tc.arch, func(t *testing.T) {
			file, artifact := writeOnboardingFixture(t, onboardingFixtureEntries(tc.machine))
			artifact.Arch = tc.arch
			root, size, err := verifyOnboardingArchive(file, artifact)
			if err != nil || root != "pair-test" || size <= 0 {
				t.Fatalf("complete archive: root=%q size=%d err=%v", root, size, err)
			}
			cache := t.TempDir()
			prepared, err := prepareOnboardingPackage(context.Background(), cache, onboardingArtifactSource{onboardingArtifact: artifact, File: file})
			if err != nil || prepared.file != file || prepared.archiveRoot != root || prepared.bytes != size {
				t.Fatalf("local package preparation: %+v %v", prepared, err)
			}
			entries, err := os.ReadDir(cache)
			if err != nil || len(entries) != 0 {
				t.Fatalf("local validation unexpectedly wrote cache state: %v %v", entries, err)
			}
		})
	}
}

func TestOnboardingArtifactRejectsWrongArchitectureAndUnsafeContent(t *testing.T) {
	mutations := []struct {
		name   string
		change func([]onboardingArchiveFixtureEntry) []onboardingArchiveFixtureEntry
	}{
		{"wrong-architecture", func(e []onboardingArchiveFixtureEntry) []onboardingArchiveFixtureEntry {
			e[0].body = onboardingFixtureELF(62)
			return e
		}},
		{"non-elf", func(e []onboardingArchiveFixtureEntry) []onboardingArchiveFixtureEntry {
			e[0].body = []byte(strings.Repeat("x", 64))
			return e
		}},
		{"truncated-header", func(e []onboardingArchiveFixtureEntry) []onboardingArchiveFixtureEntry {
			e[0].body = []byte("\x7fELF")
			return e
		}},
		{"wrong-class", func(e []onboardingArchiveFixtureEntry) []onboardingArchiveFixtureEntry { e[0].body[4] = 1; return e }},
		{"wrong-byte-order", func(e []onboardingArchiveFixtureEntry) []onboardingArchiveFixtureEntry { e[0].body[5] = 2; return e }},
		{"not-executable", func(e []onboardingArchiveFixtureEntry) []onboardingArchiveFixtureEntry { e[0].mode = 0644; return e }},
		{"setuid", func(e []onboardingArchiveFixtureEntry) []onboardingArchiveFixtureEntry { e[0].mode = 04755; return e }},
		{"setgid", func(e []onboardingArchiveFixtureEntry) []onboardingArchiveFixtureEntry { e[0].mode = 02755; return e }},
		{"missing-component", func(e []onboardingArchiveFixtureEntry) []onboardingArchiveFixtureEntry { return e[1:] }},
		{"missing-manifest", func(e []onboardingArchiveFixtureEntry) []onboardingArchiveFixtureEntry { return e[:len(e)-1] }},
		{"invalid-manifest", func(e []onboardingArchiveFixtureEntry) []onboardingArchiveFixtureEntry {
			e[len(e)-1].body = []byte("not-json")
			return e
		}},
		{"duplicate-manifest", func(e []onboardingArchiveFixtureEntry) []onboardingArchiveFixtureEntry { return append(e, e[len(e)-1]) }},
		{"duplicate-component", func(e []onboardingArchiveFixtureEntry) []onboardingArchiveFixtureEntry { return append(e, e[0]) }},
		{"unexpected-component", func(e []onboardingArchiveFixtureEntry) []onboardingArchiveFixtureEntry {
			e[0].name = "pair-test/bin/unexpected"
			return e
		}},
		{"absolute-path", func(e []onboardingArchiveFixtureEntry) []onboardingArchiveFixtureEntry {
			e[0].name = "/outside/nvpair-ui-broker"
			return e
		}},
		{"traversal", func(e []onboardingArchiveFixtureEntry) []onboardingArchiveFixtureEntry {
			e[0].name = "pair-test/bin/../../outside"
			return e
		}},
		{"backslash", func(e []onboardingArchiveFixtureEntry) []onboardingArchiveFixtureEntry {
			e[0].name = "pair-test\\bin\\nvpair-ui-broker"
			return e
		}},
		{"multiple-roots", func(e []onboardingArchiveFixtureEntry) []onboardingArchiveFixtureEntry {
			e[0].name = "another/bin/nvpair-ui-broker"
			return e
		}},
		{"nested-binary", func(e []onboardingArchiveFixtureEntry) []onboardingArchiveFixtureEntry {
			e[0].name = "pair-test/bin/nested/nvpair-ui-broker"
			return e
		}},
		{"symlink", func(e []onboardingArchiveFixtureEntry) []onboardingArchiveFixtureEntry {
			e[0].kind = tar.TypeSymlink
			e[0].link = "/bin/true"
			return e
		}},
		{"hardlink", func(e []onboardingArchiveFixtureEntry) []onboardingArchiveFixtureEntry {
			e[0].kind = tar.TypeLink
			e[0].link = e[1].name
			return e
		}},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			file, artifact := writeOnboardingFixture(t, tc.change(onboardingFixtureEntries(183)))
			if _, _, err := verifyOnboardingArchive(file, artifact); err == nil {
				t.Fatal("unsafe or incompatible package accepted")
			}
		})
	}
}

func TestOnboardingArtifactRejectsHashMismatchAndPreservesInput(t *testing.T) {
	file, artifact := writeOnboardingFixture(t, onboardingFixtureEntries(183))
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	artifact.SHA256 = strings.Repeat("0", 64)
	if _, _, err := verifyOnboardingArchive(file, artifact); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("bad digest: %v", err)
	}
	after, err := os.ReadFile(file)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("rejected source package was changed")
	}
}

func onboardingFixtureBundle(t *testing.T, machine uint16) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	for _, entry := range onboardingFixtureEntries(machine) {
		if err := os.WriteFile(filepath.Join(bin, filepath.Base(entry.name)), entry.body, os.FileMode(entry.mode)); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

func TestOnboardingArtifactSelfBundlePreservesManifestForNextPeer(t *testing.T) {
	for _, tc := range []struct {
		arch    string
		machine uint16
	}{{"arm64", 183}, {"amd64", 62}, {"x64", 62}} {
		t.Run(tc.arch, func(t *testing.T) {
			bin := onboardingFixtureBundle(t, tc.machine)
			cache := t.TempDir()
			artifact, err := onboardingPackageBundle(cache, bin, "linux", tc.arch)
			if err != nil {
				t.Fatal(err)
			}
			root, _, err := verifyOnboardingArchive(artifact.File, artifact.onboardingArtifact)
			if err != nil {
				t.Fatalf("self package rejected by receiver-side contract: %v", err)
			}
			file, err := os.Open(artifact.File)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			gz, err := gzip.NewReader(file)
			if err != nil {
				t.Fatal(err)
			}
			defer gz.Close()
			tr := tar.NewReader(gz)
			peer := filepath.Join(t.TempDir(), "bin")
			if err := os.Mkdir(peer, 0700); err != nil {
				t.Fatal(err)
			}
			count := 0
			for {
				header, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				prefix := root + "/bin/"
				if !strings.HasPrefix(header.Name, prefix) || header.Typeflag != tar.TypeReg {
					t.Fatalf("unexpected self-package entry: %q", header.Name)
				}
				name := strings.TrimPrefix(header.Name, prefix)
				if name != filepath.Base(name) || strings.ContainsAny(name, "/\\") {
					t.Fatal("fixture extraction escaped the fixed binary directory")
				}
				body, err := io.ReadAll(io.LimitReader(tr, 128<<10))
				if err != nil || int64(len(body)) != header.Size {
					t.Fatal("incomplete fixture entry")
				}
				if err := os.WriteFile(filepath.Join(peer, name), body, os.FileMode(header.Mode)); err != nil {
					t.Fatal(err)
				}
				count++
			}
			if count != len(onboardingBinaries)+1 {
				t.Fatalf("want%d binaries plus normal manifest; got%d", len(onboardingBinaries), count)
			}
			second, err := onboardingPackageBundle(t.TempDir(), peer, "linux", tc.arch)
			if err != nil {
				t.Fatalf("newly installed peer cannot provision its own next peer: %v", err)
			}
			if _, _, err := verifyOnboardingArchive(second.File, second.onboardingArtifact); err != nil {
				t.Fatal(err)
			}
			if artifact.SHA256 != second.SHA256 {
				t.Fatal("identical verified package changed across peer replication")
			}
			entries, err := os.ReadDir(filepath.Join(cache, "onboarding-artifacts"))
			if err != nil || len(entries) != 1 {
				t.Fatalf("unexpected retained temporary artifacts: %v %v", entries, err)
			}
		})
	}
}

func TestOnboardingArtifactSelfBundleFiltersHostBootstrapTools(t *testing.T) {
	bin := onboardingFixtureBundle(t, 183)
	manifestPath := filepath.Join(bin, "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest onboardingBuildManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"nvpair-host-bootstrap", "nvpair-host-helper"} {
		body := onboardingFixtureELF(183)
		if err := os.WriteFile(filepath.Join(bin, name), body, 0755); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		entry := manifest.Files[0]
		entry.FileName = name
		entry.Size = int64(len(body))
		entry.SHA256 = hex.EncodeToString(sum[:])
		manifest.Files = append(manifest.Files, entry)
		manifest.Components[name] = "0.1.0"
	}
	raw, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, raw, 0644); err != nil {
		t.Fatal(err)
	}
	artifact, err := onboardingPackageBundle(t.TempDir(), bin, "linux", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := verifyOnboardingArchive(artifact.File, artifact.onboardingArtifact); err != nil {
		t.Fatal(err)
	}
}

func TestOnboardingArtifactSelfBundleRejectsChangedComponentAndUnsupportedHost(t *testing.T) {
	bin := onboardingFixtureBundle(t, 183)
	if _, err := onboardingPackageBundle(t.TempDir(), bin, "windows", "arm64"); err == nil {
		t.Fatal("Windows binaries cannot be used as a Linux self bundle")
	}
	if _, err := onboardingPackageBundle(t.TempDir(), bin, "linux", "amd64"); err == nil {
		t.Fatal("wrong installed manifest architecture accepted")
	}
	component := filepath.Join(bin, onboardingBinaries[0])
	if err := os.WriteFile(component, onboardingFixtureELF(62), 0755); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	if _, err := onboardingPackageBundle(cache, bin, "linux", "arm64"); err == nil {
		t.Fatal("changed component was copied")
	}
	entries, err := os.ReadDir(filepath.Join(cache, "onboarding-artifacts"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected snapshot left temporary files: %v %v", entries, err)
	}
}
