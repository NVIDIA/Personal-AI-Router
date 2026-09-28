// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"testing"

	"nvpair-shared/hostbootstrap"
)

type fakePayloadFS struct {
	files map[string][]byte
}

type fakePayloadReader struct {
	*bytes.Reader
}

func (fakePayloadReader) Close() error {
	return nil
}

func (filesystem fakePayloadFS) OpenPayloadFile(path string) (io.ReadCloser, int64, error) {
	raw, found := filesystem.files[path]
	if !found {
		return nil, 0, fs.ErrNotExist
	}
	return fakePayloadReader{Reader: bytes.NewReader(raw)}, int64(len(raw)), nil
}

func TestPayloadBundleDerivesFixedSiblingManifest(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformWindows,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	product, productFiles := testProductPayload(t, request.Binding.Target)
	helper := []byte("signed helper")
	request.Binding.Product.SHA256 = payloadDigest(product)
	request.Binding.Helper.SHA256 = payloadDigest(helper)
	productName, helperName, err := payloadFileNames(request.Binding.Target)
	if err != nil {
		t.Fatal(err)
	}
	manifest := payloadManifest{
		SchemaVersion: hostbootstrap.SchemaVersion,
		Platform:      request.Binding.Target.Platform,
		Architecture:  request.Binding.Target.Architecture,
		Artifacts: []payloadManifestArtifact{
			{
				ID:        request.Binding.Product.ID,
				Version:   request.Binding.Product.Version,
				FileName: productName,
				ByteCount: int64(len(product)),
				SHA256:    request.Binding.Product.SHA256,
				Files:     productFiles,
			},
			{
				ID:        request.Binding.Helper.ID,
				Version:   request.Binding.Helper.Version,
				FileName: helperName,
				ByteCount: int64(len(helper)),
				SHA256:    request.Binding.Helper.SHA256,
				Files:     []payloadManifestFile{},
			},
		},
	}
	manifestRaw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), "nvpair-host-bootstrap.exe")
	payloadRoot := filepath.Join(filepath.Dir(executable), "payload")
	filesystem := fakePayloadFS{files: map[string][]byte{
		filepath.Join(payloadRoot, "manifest.json"): manifestRaw,
		filepath.Join(payloadRoot, productName):     product,
		filepath.Join(payloadRoot, helperName):      helper,
	}}
	bundle, err := loadPayloadBundle(executable, request, filesystem)
	if err != nil {
		t.Fatalf("loadPayloadBundle() error = %v", err)
	}
	if bundle.Product.Path != filepath.Join(payloadRoot, productName) ||
		bundle.Helper.Path != filepath.Join(payloadRoot, helperName) {
		t.Fatalf("bundle = %#v", bundle)
	}
}

func TestPayloadBundleRejectsMissingChangedAndNoncanonicalInputs(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureARM64,
	})
	product, productFiles := testProductPayload(t, request.Binding.Target)
	helper := []byte("helper")
	request.Binding.Product.SHA256 = payloadDigest(product)
	request.Binding.Helper.SHA256 = payloadDigest(helper)
	productName, helperName, err := payloadFileNames(request.Binding.Target)
	if err != nil {
		t.Fatal(err)
	}
	executable := "/staged/nvpair-host-bootstrap"
	root := filepath.Join(filepath.Dir(executable), "payload")
	valid := payloadManifest{
		SchemaVersion: hostbootstrap.SchemaVersion,
		Platform:      request.Binding.Target.Platform,
		Architecture:  request.Binding.Target.Architecture,
		Artifacts: []payloadManifestArtifact{
			{ID: request.Binding.Product.ID, Version: request.Binding.Product.Version, FileName: productName, ByteCount: int64(len(product)), SHA256: request.Binding.Product.SHA256, Files: productFiles},
			{ID: request.Binding.Helper.ID, Version: request.Binding.Helper.Version, FileName: helperName, ByteCount: int64(len(helper)), SHA256: request.Binding.Helper.SHA256, Files: []payloadManifestFile{}},
		},
	}
	validRaw, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	base := map[string][]byte{
		filepath.Join(root, "manifest.json"): validRaw,
		filepath.Join(root, productName):     product,
		filepath.Join(root, helperName):      helper,
	}
	tests := []struct {
		name  string
		files map[string][]byte
	}{
		{name: "missing helper", files: withoutPayload(base, filepath.Join(root, helperName))},
		{name: "changed product", files: withPayload(base, filepath.Join(root, productName), []byte("changed"))},
		{name: "unknown manifest field", files: withPayload(base, filepath.Join(root, "manifest.json"), []byte(`{"schemaVersion":1,"platform":"linux","architecture":"arm64","artifacts":[],"path":"/tmp"}`))},
		{name: "duplicate manifest field", files: withPayload(base, filepath.Join(root, "manifest.json"), []byte(`{"schemaVersion":1,"schemaVersion":1,"platform":"linux","architecture":"arm64","artifacts":[]}`))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := loadPayloadBundle(executable, request, fakePayloadFS{files: test.files}); !errors.Is(err, ErrPayloadInvalid) {
				t.Fatalf("loadPayloadBundle() error = %v", err)
			}
		})
	}

	wrongName := valid
	wrongName.Artifacts[0].FileName = "../escape"
	wrongRaw, err := json.Marshal(wrongName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadPayloadBundle(
		executable,
		request,
		fakePayloadFS{files: withPayload(base, filepath.Join(root, "manifest.json"), wrongRaw)},
	); !errors.Is(err, ErrPayloadInvalid) {
		t.Fatalf("noncanonical payload filename error = %v", err)
	}
}

func TestProductDirectoryManifestIncludesEveryImplicitDirectoryAt0755(t *testing.T) {
	files := []payloadManifestFile{
		{
			Path:   "bin/tools/nvpair-tui",
			Type:   payloadTypeFile,
			Size:   3,
			Mode:   0755,
			SHA256: payloadDigest([]byte("tui")),
		},
		{
			Path: "assets/icons",
			Type: payloadTypeDirectory,
			Mode: 0755,
		},
	}
	got := productDirectoryModes(files)
	want := map[string]fs.FileMode{
		".":            0755,
		"bin":          0755,
		"bin/tools":    0755,
		"assets":       0755,
		"assets/icons": 0755,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("directory modes = %#v, want %#v", got, want)
	}
}

func TestProductManifestRejectsFileAncestorAndCaseFoldedCollisions(t *testing.T) {
	file := func(path string) payloadManifestFile {
		return payloadManifestFile{
			Path:   path,
			Type:   payloadTypeFile,
			Size:   1,
			Mode:   0644,
			SHA256: payloadDigest([]byte("x")),
		}
	}
	for _, collision := range [][]payloadManifestFile{
		{file("a"), file("a/b")},
		{file("A"), file("a/b")},
		{file("a"), file("a/b/c")},
		{
			{Path: "A", Type: payloadTypeDirectory, Mode: 0755},
			file("a"),
		},
	} {
		files := append([]payloadManifestFile{}, collision...)
		files = append(
			files,
			payloadManifestFile{Path: "nvpair-tui", Type: payloadTypeFile, Size: 3, Mode: 0755, SHA256: payloadDigest([]byte("tui"))},
			payloadManifestFile{Path: "nvpair-ui-broker", Type: payloadTypeFile, Size: 6, Mode: 0755, SHA256: payloadDigest([]byte("broker"))},
		)
		sort.Slice(files, func(left, right int) bool {
			return files[left].Path < files[right].Path
		})
		if err := validateProductFileManifest(
			hostbootstrap.Target{
				Platform:     hostbootstrap.PlatformLinux,
				Architecture: hostbootstrap.ArchitectureAMD64,
			},
			files,
		); !errors.Is(err, ErrPayloadInvalid) {
			t.Fatalf("collision %#v error = %v", collision, err)
		}
	}
}

func TestWindowsProductManifestRejectsReservedAndADSPaths(t *testing.T) {
	validFile := func(path string) payloadManifestFile {
		return payloadManifestFile{
			Path:   path,
			Type:   payloadTypeFile,
			Size:   1,
			Mode:   0644,
			SHA256: payloadDigest([]byte("x")),
		}
	}
	for _, invalid := range []string{
		"CON",
		"AUX.txt",
		"file:stream",
		"trailing.",
		"trailing ",
		`dir\file`,
		`C:/file`,
		`//server/share`,
		`\\?\C:\file`,
	} {
		files := []payloadManifestFile{
			validFile(invalid),
			{Path: "nvpair-tui.exe", Type: payloadTypeFile, Size: 3, Mode: 0755, SHA256: payloadDigest([]byte("tui"))},
			{Path: "nvpair-ui-broker.exe", Type: payloadTypeFile, Size: 6, Mode: 0755, SHA256: payloadDigest([]byte("broker"))},
		}
		sort.Slice(files, func(left, right int) bool {
			return files[left].Path < files[right].Path
		})
		if err := validateProductFileManifest(
			hostbootstrap.Target{
				Platform:     hostbootstrap.PlatformWindows,
				Architecture: hostbootstrap.ArchitectureAMD64,
			},
			files,
		); !errors.Is(err, ErrPayloadInvalid) {
			t.Fatalf("path %q error = %v", invalid, err)
		}
	}
	files := []payloadManifestFile{
		validFile("assets/config.json"),
		{Path: "nvpair-tui.exe", Type: payloadTypeFile, Size: 3, Mode: 0755, SHA256: payloadDigest([]byte("tui"))},
		{Path: "nvpair-ui-broker.exe", Type: payloadTypeFile, Size: 6, Mode: 0755, SHA256: payloadDigest([]byte("broker"))},
	}
	sort.Slice(files, func(left, right int) bool {
		return files[left].Path < files[right].Path
	})
	if err := validateProductFileManifest(
		hostbootstrap.Target{
			Platform:     hostbootstrap.PlatformWindows,
			Architecture: hostbootstrap.ArchitectureAMD64,
		},
		files,
	); err != nil {
		t.Fatalf("safe Windows manifest error = %v", err)
	}
}

func TestProductManifestRejectsInvalidUTF8AndControlCharacters(t *testing.T) {
	invalidPaths := []string{
		"bad\x00name",
		"bad\x1fname",
		"bad\x7fname",
		"bad\u0085name",
		string([]byte{'b', 'a', 'd', 0xff}),
	}
	for _, platform := range []hostbootstrap.Platform{
		hostbootstrap.PlatformWindows,
		hostbootstrap.PlatformLinux,
	} {
		for _, invalid := range invalidPaths {
			tui := "nvpair-tui"
			broker := "nvpair-ui-broker"
			if platform == hostbootstrap.PlatformWindows {
				tui += ".exe"
				broker += ".exe"
			}
			files := []payloadManifestFile{
				{Path: invalid, Type: payloadTypeFile, Size: 1, Mode: 0644, SHA256: payloadDigest([]byte("x"))},
				{Path: tui, Type: payloadTypeFile, Size: 3, Mode: 0755, SHA256: payloadDigest([]byte("tui"))},
				{Path: broker, Type: payloadTypeFile, Size: 6, Mode: 0755, SHA256: payloadDigest([]byte("broker"))},
			}
			sort.Slice(files, func(left, right int) bool {
				return files[left].Path < files[right].Path
			})
			if err := validateProductFileManifest(
				hostbootstrap.Target{
					Platform:     platform,
					Architecture: hostbootstrap.ArchitectureAMD64,
				},
				files,
			); !errors.Is(err, ErrPayloadInvalid) {
				t.Fatalf("platform=%s path=%q error=%v", platform, invalid, err)
			}
		}
	}
}

func TestVerifyProductTreeRequiresEveryManifestDirectory(t *testing.T) {
	root := t.TempDir()
	files := []payloadManifestFile{{
		Path: "empty",
		Type: payloadTypeDirectory,
		Mode: 0755,
	}}
	present, exact, err := verifyInstalledProductTree(
		root,
		files,
		hostbootstrap.PlatformWindows,
	)
	if err != nil || !present || exact {
		t.Fatalf("present=%t exact=%t error=%v", present, exact, err)
	}
}

func TestPublicationFsyncsDirectoriesBottomUpBeforeParentCommit(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("installs a Windows product tree; Unix hosts require root-owned state directories")
	}
	files := []payloadManifestFile{
		{Path: "assets", Type: payloadTypeDirectory, Mode: 0755},
		{Path: "assets/config.json", Type: payloadTypeFile, Size: 2, Mode: 0644, SHA256: payloadDigest([]byte("{}"))},
		{Path: "nvpair-tui.exe", Type: payloadTypeFile, Size: 3, Mode: 0755, SHA256: payloadDigest([]byte("tui"))},
		{Path: "nvpair-ui-broker.exe", Type: payloadTypeFile, Size: 6, Mode: 0755, SHA256: payloadDigest([]byte("broker"))},
	}
	archive := buildProductZip(t, []zipFixture{
		{name: "assets", mode: fs.ModeDir | 0755},
		{name: "assets/config.json", mode: 0644, body: []byte("{}")},
		{name: "nvpair-tui.exe", mode: 0755, body: []byte("tui")},
		{name: "nvpair-ui-broker.exe", mode: 0755, body: []byte("broker")},
	})
	sourcePath := filepath.Join(t.TempDir(), "payload.zip")
	source := payloadSource{
		Path:      sourcePath,
		ByteCount: int64(len(archive)),
		SHA256:    payloadDigest(archive),
		Files:     files,
	}
	destination := filepath.Join(t.TempDir(), "product")
	operationID := "abababababababababababababababab"
	var synced []string
	originalSync := syncDirectoryForPublication
	originalSecure := securePayloadExecutableForInstall
	syncDirectoryForPublication = func(path string) error {
		synced = append(synced, filepath.Clean(path))
		return nil
	}
	securePayloadExecutableForInstall = func(string) error { return nil }
	defer func() {
		syncDirectoryForPublication = originalSync
		securePayloadExecutableForInstall = originalSecure
	}()
	if err := installProductArchive(
		fakePayloadFS{files: map[string][]byte{sourcePath: archive}},
		source,
		destination,
		"",
		hostbootstrap.PlatformWindows,
		operationID,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	stage := destination + ".staging-" + operationID
	want := []string{
		filepath.Join(stage, "assets"),
		stage,
		filepath.Dir(destination),
	}
	if !reflect.DeepEqual(synced, want) {
		t.Fatalf("sync order = %#v, want %#v", synced, want)
	}
	synced = nil
	helperPath := filepath.Join(t.TempDir(), "nvpair-host-helper.exe")
	helperSourcePath := filepath.Join(t.TempDir(), "helper.exe")
	helperBytes := []byte("helper")
	if err := installPayloadSource(
		fakePayloadFS{files: map[string][]byte{
			helperSourcePath: helperBytes,
		}},
		payloadSource{
			Path:      helperSourcePath,
			ByteCount: int64(len(helperBytes)),
			SHA256:    payloadDigest(helperBytes),
		},
		helperPath,
		"",
	); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(
		synced,
		[]string{filepath.Dir(helperPath)},
	) {
		t.Fatalf("helper sync order = %#v", synced)
	}
}

type orderedProductFile struct {
	events []string
}

func (file *orderedProductFile) Write(data []byte) (int, error) {
	file.events = append(file.events, "write")
	return len(data), nil
}

func (file *orderedProductFile) Chmod(fs.FileMode) error {
	file.events = append(file.events, "chmod")
	return nil
}

func (file *orderedProductFile) Sync() error {
	file.events = append(file.events, "sync")
	return nil
}

func (file *orderedProductFile) Close() error {
	file.events = append(file.events, "close")
	return nil
}

func TestProductFileModeIsDurableBeforeClose(t *testing.T) {
	file := &orderedProductFile{}
	if err := writeProductFile(
		file,
		bytes.NewReader([]byte("payload")),
		0755,
	); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(
		file.events,
		[]string{"write", "chmod", "sync", "close"},
	) {
		t.Fatalf("file events = %#v", file.events)
	}
}

func payloadDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func testProductPayload(
	t *testing.T,
	target hostbootstrap.Target,
) ([]byte, []payloadManifestFile) {
	t.Helper()
	tui := "nvpair-tui"
	broker := "nvpair-ui-broker"
	if target.Platform == hostbootstrap.PlatformWindows {
		tui += ".exe"
		broker += ".exe"
	}
	if target.Platform == hostbootstrap.PlatformDarwin {
		tui = "Contents/Resources/cli-bin/" + tui
		broker = "Contents/Resources/cli-bin/" + broker
	}
	files := []payloadManifestFile{
		{Path: tui, Type: payloadTypeFile, Size: 3, Mode: 0755, SHA256: payloadDigest([]byte("tui"))},
		{Path: broker, Type: payloadTypeFile, Size: 6, Mode: 0755, SHA256: payloadDigest([]byte("broker"))},
	}
	archive := buildProductZip(t, []zipFixture{
		{name: tui, mode: 0755, body: []byte("tui")},
		{name: broker, mode: 0755, body: []byte("broker")},
	})
	return archive, files
}

func withoutPayload(source map[string][]byte, path string) map[string][]byte {
	copy := make(map[string][]byte, len(source))
	for key, value := range source {
		if key != path {
			copy[key] = value
		}
	}
	return copy
}

func withPayload(source map[string][]byte, path string, raw []byte) map[string][]byte {
	copy := withoutPayload(source, "")
	copy[path] = raw
	return copy
}
