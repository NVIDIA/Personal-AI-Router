// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"nvpair-shared/hostbootstrap"
)

const (
	maxPayloadManifestBytes = 16 << 20
	maxPayloadArtifactBytes = int64(8 << 30)
)

var ErrPayloadInvalid = errors.New("signed sibling payload is invalid")

var payloadFileNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var syncDirectoryForPublication = syncDirectoryNative
var securePayloadExecutableForInstall = securePayloadExecutable

type payloadManifest struct {
	SchemaVersion int                        `json:"schemaVersion"`
	Platform      hostbootstrap.Platform     `json:"platform"`
	Architecture  hostbootstrap.Architecture `json:"architecture"`
	Artifacts     []payloadManifestArtifact  `json:"artifacts"`
}

type payloadManifestArtifact struct {
	ID        string                `json:"id"`
	Version   string                `json:"version"`
	FileName  string                `json:"fileName"`
	ByteCount int64                 `json:"byteCount"`
	SHA256    string                `json:"sha256"`
	Files     []payloadManifestFile `json:"files"`
}

type payloadFileType string

const (
	payloadTypeFile      payloadFileType = "file"
	payloadTypeDirectory payloadFileType = "directory"
)

type payloadManifestFile struct {
	Path   string          `json:"path"`
	Type   payloadFileType `json:"type"`
	Size   int64           `json:"size"`
	Mode   fs.FileMode     `json:"mode"`
	SHA256 string          `json:"sha256"`
}

type payloadFileSystem interface {
	OpenPayloadFile(string) (io.ReadCloser, int64, error)
}

type nativePayloadFS struct{}

type payloadSource struct {
	Path      string
	ByteCount int64
	SHA256    string
	Files     []payloadManifestFile
}

type payloadBundle struct {
	Product payloadSource
	Helper  payloadSource
}

func payloadFileNames(target hostbootstrap.Target) (string, string, error) {
	switch target.Platform {
	case hostbootstrap.PlatformWindows:
		return "nvpair-product.zip", "nvpair-host-helper.exe", nil
	case hostbootstrap.PlatformDarwin:
		return "nvpair-product.zip", "nvpair-host-helper", nil
	case hostbootstrap.PlatformLinux:
		return "nvpair-product.zip", "nvpair-host-helper", nil
	default:
		return "", "", ErrPayloadInvalid
	}
}

func loadPayloadBundle(
	executable string,
	request hostbootstrap.Request,
	filesystem payloadFileSystem,
) (payloadBundle, error) {
	if err := request.Validate(); err != nil {
		return payloadBundle{}, ErrPayloadInvalid
	}
	if !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
		return payloadBundle{}, ErrPayloadInvalid
	}
	root := filepath.Join(filepath.Dir(executable), "payload")
	manifestRaw, err := readPayloadSmall(filesystem, filepath.Join(root, "manifest.json"), maxPayloadManifestBytes)
	if err != nil {
		return payloadBundle{}, ErrPayloadInvalid
	}
	manifest, err := decodePayloadManifest(manifestRaw)
	if err != nil ||
		manifest.SchemaVersion != hostbootstrap.SchemaVersion ||
		manifest.Platform != request.Binding.Target.Platform ||
		manifest.Architecture != request.Binding.Target.Architecture ||
		len(manifest.Artifacts) != 2 {
		return payloadBundle{}, ErrPayloadInvalid
	}
	productName, helperName, err := payloadFileNames(request.Binding.Target)
	if err != nil {
		return payloadBundle{}, err
	}
	var bundle payloadBundle
	seen := make(map[string]bool)
	for _, artifact := range manifest.Artifacts {
		if seen[artifact.ID] ||
			!payloadFileNamePattern.MatchString(artifact.FileName) ||
			artifact.ByteCount < 1 ||
			artifact.ByteCount > maxPayloadArtifactBytes {
			return payloadBundle{}, ErrPayloadInvalid
		}
		seen[artifact.ID] = true
		var desired hostbootstrap.ArtifactIdentity
		var expectedName string
		switch artifact.ID {
		case request.Binding.Product.ID:
			desired, expectedName = request.Binding.Product, productName
		case request.Binding.Helper.ID:
			desired, expectedName = request.Binding.Helper, helperName
		default:
			return payloadBundle{}, ErrPayloadInvalid
		}
		if artifact.Version != desired.Version ||
			artifact.FileName != expectedName ||
			artifact.SHA256 != desired.SHA256 {
			return payloadBundle{}, ErrPayloadInvalid
		}
		if artifact.ID == request.Binding.Product.ID {
			if err := validateProductFileManifest(
				request.Binding.Target,
				artifact.Files,
			); err != nil {
				return payloadBundle{}, err
			}
		} else if len(artifact.Files) != 0 {
			return payloadBundle{}, ErrPayloadInvalid
		}
		source := payloadSource{
			Path:      filepath.Join(root, artifact.FileName),
			ByteCount: artifact.ByteCount,
			SHA256:    artifact.SHA256,
			Files:     append([]payloadManifestFile(nil), artifact.Files...),
		}
		if err := validatePayloadSource(filesystem, source); err != nil {
			return payloadBundle{}, err
		}
		if artifact.ID == request.Binding.Product.ID {
			if err := validateProductArchive(filesystem, source); err != nil {
				return payloadBundle{}, err
			}
		}
		if artifact.ID == request.Binding.Product.ID {
			bundle.Product = source
		} else {
			bundle.Helper = source
		}
	}
	if bundle.Product.Path == "" || bundle.Helper.Path == "" {
		return payloadBundle{}, ErrPayloadInvalid
	}
	return bundle, nil
}

func decodePayloadManifest(raw []byte) (payloadManifest, error) {
	var manifest payloadManifest
	if !utf8.Valid(raw) {
		return manifest, ErrPayloadInvalid
	}
	fields, err := decodePayloadObject(raw, map[string]bool{
		"schemaVersion": true,
		"platform":      true,
		"architecture":  true,
		"artifacts":     true,
	})
	if err != nil || len(fields) != 4 {
		return manifest, ErrPayloadInvalid
	}
	var artifacts []json.RawMessage
	if err := json.Unmarshal(fields["artifacts"], &artifacts); err != nil {
		return manifest, ErrPayloadInvalid
	}
	for _, artifact := range artifacts {
		nested, err := decodePayloadObject(artifact, map[string]bool{
			"id":        true,
			"version":   true,
			"fileName":  true,
			"byteCount": true,
			"sha256":    true,
			"files":     true,
		})
		if err != nil || len(nested) != 6 {
			return manifest, ErrPayloadInvalid
		}
		var files []json.RawMessage
		if err := json.Unmarshal(nested["files"], &files); err != nil {
			return manifest, ErrPayloadInvalid
		}
		for _, file := range files {
			fields, err := decodePayloadObject(file, map[string]bool{
				"path":   true,
				"type":   true,
				"size":   true,
				"mode":   true,
				"sha256": true,
			})
			if err != nil || len(fields) != 5 {
				return manifest, ErrPayloadInvalid
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, ErrPayloadInvalid
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return manifest, ErrPayloadInvalid
	}
	return manifest, nil
}

func decodePayloadObject(
	raw []byte,
	allowed map[string]bool,
) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrPayloadInvalid
	}
	values := make(map[string]json.RawMessage)
	for decoder.More() {
		fieldToken, err := decoder.Token()
		field, ok := fieldToken.(string)
		if err != nil || !ok || !allowed[field] {
			return nil, ErrPayloadInvalid
		}
		if _, duplicate := values[field]; duplicate {
			return nil, ErrPayloadInvalid
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, ErrPayloadInvalid
		}
		values[field] = value
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return nil, ErrPayloadInvalid
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, ErrPayloadInvalid
	}
	return values, nil
}

func readPayloadSmall(
	filesystem payloadFileSystem,
	path string,
	limit int64,
) ([]byte, error) {
	file, size, err := filesystem.OpenPayloadFile(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if size < 1 || size > limit {
		return nil, ErrPayloadInvalid
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(raw)) != size {
		return nil, ErrPayloadInvalid
	}
	return raw, nil
}

func validatePayloadSource(
	filesystem payloadFileSystem,
	source payloadSource,
) error {
	file, size, err := filesystem.OpenPayloadFile(source.Path)
	if err != nil {
		return ErrPayloadInvalid
	}
	defer file.Close()
	if size != source.ByteCount {
		return ErrPayloadInvalid
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, source.ByteCount+1))
	if err != nil ||
		written != source.ByteCount ||
		hex.EncodeToString(hash.Sum(nil)) != source.SHA256 {
		return ErrPayloadInvalid
	}
	return nil
}

func validateProductFileManifest(
	target hostbootstrap.Target,
	files []payloadManifestFile,
) error {
	if len(files) == 0 {
		return ErrPayloadInvalid
	}
	required := map[string]bool{}
	switch target.Platform {
	case hostbootstrap.PlatformWindows:
		required["nvpair-tui.exe"] = true
		required["nvpair-ui-broker.exe"] = true
	case hostbootstrap.PlatformDarwin:
		required["Contents/Resources/cli-bin/nvpair-tui"] = true
		required["Contents/Resources/cli-bin/nvpair-ui-broker"] = true
	case hostbootstrap.PlatformLinux:
		required["nvpair-tui"] = true
		required["nvpair-ui-broker"] = true
	default:
		return ErrPayloadInvalid
	}
	seen := make(map[string]bool)
	folded := make(map[string]payloadManifestFile)
	previous := ""
	for _, file := range files {
		foldedKey := strings.ToLower(file.Path)
		_, foldedSeen := folded[foldedKey]
		if !canonicalArchivePathForPlatform(
			target.Platform,
			file.Path,
		) ||
			seen[file.Path] ||
			foldedSeen ||
			(previous != "" && file.Path <= previous) {
			return ErrPayloadInvalid
		}
		seen[file.Path] = true
		folded[foldedKey] = file
		previous = file.Path
		switch file.Type {
		case payloadTypeFile:
			if file.Size < 0 ||
				file.Size > maxPayloadArtifactBytes ||
				(file.Mode.Perm() != 0644 && file.Mode.Perm() != 0755) ||
				!stateHex64.MatchString(file.SHA256) {
				return ErrPayloadInvalid
			}
		case payloadTypeDirectory:
			if file.Size != 0 || file.Mode.Perm() != 0755 || file.SHA256 != "" {
				return ErrPayloadInvalid
			}
		default:
			return ErrPayloadInvalid
		}
		if required[file.Path] &&
			(file.Type != payloadTypeFile || file.Mode.Perm() != 0755) {
			return ErrPayloadInvalid
		}
		delete(required, file.Path)
	}
	if len(required) != 0 {
		return ErrPayloadInvalid
	}
	for _, file := range files {
		for ancestor := path.Dir(file.Path); ancestor != "."; ancestor = path.Dir(ancestor) {
			entry, found := folded[strings.ToLower(ancestor)]
			if found && entry.Type == payloadTypeFile {
				return ErrPayloadInvalid
			}
		}
	}
	return nil
}

func canonicalArchivePathForPlatform(
	platform hostbootstrap.Platform,
	value string,
) bool {
	if !canonicalArchivePath(value) {
		return false
	}
	if platform != hostbootstrap.PlatformWindows {
		return platform == hostbootstrap.PlatformDarwin ||
			platform == hostbootstrap.PlatformLinux
	}
	for _, segment := range strings.Split(value, "/") {
		if strings.HasSuffix(segment, ".") ||
			strings.HasSuffix(segment, " ") ||
			strings.ContainsAny(segment, `<>:"|?*`) ||
			windowsManifestReservedName(segment) {
			return false
		}
	}
	return true
}

func windowsManifestReservedName(segment string) bool {
	name := strings.ToUpper(segment)
	if before, _, found := strings.Cut(name, "."); found {
		name = before
	}
	switch name {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5",
		"COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5",
		"LPT6", "LPT7", "LPT8", "LPT9":
		return true
	default:
		return false
	}
}

func canonicalArchivePath(value string) bool {
	return value != "" &&
		utf8.ValidString(value) &&
		strings.IndexFunc(value, func(character rune) bool {
			return character < 0x20 ||
				(character >= 0x7f && character <= 0x9f)
		}) == -1 &&
		!strings.Contains(value, `\`) &&
		!strings.HasPrefix(value, "/") &&
		path.Clean(value) == value &&
		value != "." &&
		!strings.HasPrefix(value, "../")
}

func productDirectoryModes(files []payloadManifestFile) map[string]fs.FileMode {
	directories := map[string]fs.FileMode{".": 0755}
	for _, file := range files {
		directory := path.Dir(file.Path)
		for directory != "." {
			directories[directory] = 0755
			directory = path.Dir(directory)
		}
		if file.Type == payloadTypeDirectory {
			directories[file.Path] = 0755
		}
	}
	return directories
}

func syncProductDirectories(
	root string,
	files []payloadManifestFile,
) error {
	directories := productDirectoryModes(files)
	ordered := make([]string, 0, len(directories))
	for relative := range directories {
		ordered = append(ordered, relative)
	}
	sort.Slice(ordered, func(left, right int) bool {
		leftDepth := strings.Count(ordered[left], "/")
		rightDepth := strings.Count(ordered[right], "/")
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		return ordered[left] > ordered[right]
	})
	for _, relative := range ordered {
		directory := root
		if relative != "." {
			directory = filepath.Join(
				root,
				filepath.FromSlash(relative),
			)
		}
		if err := syncDirectoryForPublication(directory); err != nil {
			return err
		}
	}
	return nil
}

type payloadReaderAtCloser interface {
	io.ReaderAt
	io.Closer
}

type durableProductFile interface {
	io.Writer
	Chmod(fs.FileMode) error
	Sync() error
	Close() error
}

func writeProductFile(
	output durableProductFile,
	input io.Reader,
	mode fs.FileMode,
) error {
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Chmod(mode.Perm()); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func validateProductArchive(
	filesystem payloadFileSystem,
	source payloadSource,
) error {
	file, size, err := filesystem.OpenPayloadFile(source.Path)
	if err != nil {
		return ErrPayloadInvalid
	}
	defer file.Close()
	readerAt, ok := file.(io.ReaderAt)
	if !ok || size != source.ByteCount {
		return ErrPayloadInvalid
	}
	return validateProductArchiveReader(readerAt, size, source.Files)
}

func validateProductArchiveBytes(
	raw []byte,
	files []payloadManifestFile,
) error {
	return validateProductArchiveReader(bytes.NewReader(raw), int64(len(raw)), files)
}

func validateProductArchiveReader(
	reader io.ReaderAt,
	size int64,
	files []payloadManifestFile,
) error {
	archive, err := zip.NewReader(reader, size)
	if err != nil {
		return ErrPayloadInvalid
	}
	expected := make(map[string]payloadManifestFile, len(files))
	for _, file := range files {
		expected[file.Path] = file
	}
	seen := make(map[string]bool)
	folded := make(map[string]bool)
	previous := ""
	for _, entry := range archive.File {
		name := strings.TrimSuffix(entry.Name, "/")
		if !canonicalArchivePath(name) ||
			seen[name] ||
			folded[strings.ToLower(name)] ||
			(previous != "" && name <= previous) {
			return ErrPayloadInvalid
		}
		previous = name
		seen[name] = true
		folded[strings.ToLower(name)] = true
		manifest, found := expected[name]
		if !found {
			return ErrPayloadInvalid
		}
		mode := entry.Mode()
		switch manifest.Type {
		case payloadTypeDirectory:
			if !mode.IsDir() || entry.UncompressedSize64 != 0 {
				return ErrPayloadInvalid
			}
		case payloadTypeFile:
			if !mode.IsRegular() ||
				entry.UncompressedSize64 != uint64(manifest.Size) ||
				mode.Perm() != manifest.Mode.Perm() {
				return ErrPayloadInvalid
			}
			stream, err := entry.Open()
			if err != nil {
				return ErrPayloadInvalid
			}
			hash := sha256.New()
			written, copyErr := io.Copy(
				hash,
				io.LimitReader(stream, manifest.Size+1),
			)
			closeErr := stream.Close()
			if copyErr != nil ||
				closeErr != nil ||
				written != manifest.Size ||
				hex.EncodeToString(hash.Sum(nil)) != manifest.SHA256 {
				return ErrPayloadInvalid
			}
		default:
			return ErrPayloadInvalid
		}
		delete(expected, name)
	}
	if len(expected) != 0 {
		return ErrPayloadInvalid
	}
	return nil
}

func installPayloadSource(
	filesystem payloadFileSystem,
	source payloadSource,
	destination string,
	allowedExistingDigest string,
) error {
	if err := validatePayloadSource(filesystem, source); err != nil {
		return err
	}
	existingDigest, present, err := hashOwnedPath(destination)
	if err != nil {
		return err
	}
	if present {
		if existingDigest == source.SHA256 {
			return nil
		}
		if allowedExistingDigest == "" || existingDigest != allowedExistingDigest {
			return ErrForeignCollision
		}
	}
	if err := ensureRootDefinitionDirectory(filepath.Dir(destination)); err != nil {
		return err
	}
	input, size, err := filesystem.OpenPayloadFile(source.Path)
	if err != nil {
		return ErrPayloadInvalid
	}
	defer input.Close()
	if size != source.ByteCount {
		return ErrPayloadInvalid
	}
	output, err := os.CreateTemp(filepath.Dir(destination), ".nvpair-payload-*")
	if err != nil {
		return err
	}
	temporary := output.Name()
	defer os.Remove(temporary)
	if err := output.Chmod(0755); err != nil {
		_ = output.Close()
		return err
	}
	if err := securePayloadExecutableForInstall(temporary); err != nil {
		_ = output.Close()
		return err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(output, hash), io.LimitReader(input, source.ByteCount+1))
	if copyErr != nil ||
		written != source.ByteCount ||
		hex.EncodeToString(hash.Sum(nil)) != source.SHA256 {
		_ = output.Close()
		return ErrPayloadInvalid
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	currentDigest, currentPresent, err := hashOwnedPath(destination)
	if err != nil {
		return err
	}
	if currentPresent &&
		currentDigest != source.SHA256 &&
		currentDigest != allowedExistingDigest {
		return ErrStateIdentity
	}
	if err := os.Rename(temporary, destination); err != nil {
		return err
	}
	return syncDirectoryForPublication(filepath.Dir(destination))
}

func installProductArchive(
	filesystem payloadFileSystem,
	source payloadSource,
	destination string,
	allowedExistingDigest string,
	platform hostbootstrap.Platform,
	operationID string,
	allowedExistingFiles []payloadManifestFile,
) error {
	if !stateHex32.MatchString(operationID) {
		return ErrPayloadInvalid
	}
	backup := destination + ".backup-" + operationID
	present, exact, err := verifyInstalledProductTree(destination, source.Files, platform)
	if err != nil {
		return err
	}
	replaceExisting := false
	backupAlready := false
	if present {
		if exact && allowedExistingDigest == source.SHA256 {
			return nil
		}
		oldPresent, _, oldPartialExact, oldErr := inspectInstalledProductTree(
			destination,
			allowedExistingFiles,
			platform,
		)
		if oldErr != nil ||
			!oldPresent ||
			!oldPartialExact ||
			allowedExistingDigest == "" {
			return ErrStateIdentity
		}
		replaceExisting = true
	} else if backupInfo, backupErr := os.Lstat(backup); backupErr == nil {
		if !backupInfo.IsDir() || backupInfo.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafeState
		}
		oldPresent, _, oldPartialExact, oldErr := inspectInstalledProductTree(
			backup,
			allowedExistingFiles,
			platform,
		)
		if oldErr != nil ||
			!oldPresent ||
			!oldPartialExact ||
			allowedExistingDigest == "" {
			return ErrStateIdentity
		}
		replaceExisting = true
		backupAlready = true
	} else if !errors.Is(backupErr, os.ErrNotExist) {
		return backupErr
	}
	if err := ensureRootDefinitionDirectory(filepath.Dir(destination)); err != nil {
		return err
	}
	input, size, err := filesystem.OpenPayloadFile(source.Path)
	if err != nil {
		return ErrPayloadInvalid
	}
	defer input.Close()
	readerAt, ok := input.(io.ReaderAt)
	if !ok || size != source.ByteCount {
		return ErrPayloadInvalid
	}
	archive, err := zip.NewReader(readerAt, size)
	if err != nil {
		return ErrPayloadInvalid
	}
	stage := destination + ".staging-" + operationID
	if info, err := os.Lstat(stage); err == nil {
		if !info.IsDir() ||
			info.Mode()&os.ModeSymlink != 0 ||
			info.Mode().Perm()&0077 != 0 {
			return ErrUnsafeState
		}
		if err := os.RemoveAll(stage); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Mkdir(stage, 0700); err != nil {
		return err
	}
	removeStage := true
	defer func() {
		if removeStage {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := os.Chmod(stage, 0700); err != nil {
		return err
	}
	directories := productDirectoryModes(source.Files)
	orderedDirectories := make([]string, 0, len(directories)-1)
	for relative := range directories {
		if relative != "." {
			orderedDirectories = append(orderedDirectories, relative)
		}
	}
	sort.Slice(orderedDirectories, func(left, right int) bool {
		leftDepth := strings.Count(orderedDirectories[left], "/")
		rightDepth := strings.Count(orderedDirectories[right], "/")
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		return orderedDirectories[left] < orderedDirectories[right]
	})
	for _, relative := range orderedDirectories {
		directory := filepath.Join(stage, filepath.FromSlash(relative))
		if err := os.Mkdir(directory, 0755); err != nil &&
			!errors.Is(err, fs.ErrExist) {
			return err
		}
		info, err := os.Lstat(directory)
		if err != nil ||
			!info.IsDir() ||
			info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafeState
		}
		if err := os.Chmod(directory, 0755); err != nil {
			return err
		}
	}
	for _, entry := range archive.File {
		relative := strings.TrimSuffix(entry.Name, "/")
		target := filepath.Join(stage, filepath.FromSlash(relative))
		if !strings.HasPrefix(target, stage+string(filepath.Separator)) {
			return ErrPayloadInvalid
		}
		if entry.Mode().IsDir() {
			continue
		}
		inputFile, err := entry.Open()
		if err != nil {
			return ErrPayloadInvalid
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, entry.Mode().Perm())
		if err != nil {
			_ = inputFile.Close()
			return err
		}
		writeErr := writeProductFile(
			output,
			inputFile,
			entry.Mode().Perm(),
		)
		closeInputErr := inputFile.Close()
		if writeErr != nil || closeInputErr != nil {
			return ErrPayloadInvalid
		}
	}
	if err := os.Chmod(stage, 0755); err != nil {
		return err
	}
	stagePresent, stageExact, err := verifyInstalledProductTree(stage, source.Files, platform)
	if err != nil || !stagePresent || !stageExact {
		return ErrPayloadInvalid
	}
	if err := syncProductDirectories(stage, source.Files); err != nil {
		return err
	}
	parent := filepath.Dir(destination)
	if replaceExisting && !backupAlready {
		if _, err := os.Lstat(backup); err == nil {
			return ErrStateIdentity
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(destination, backup); err != nil {
			return err
		}
		if err := syncDirectoryForPublication(parent); err != nil {
			return err
		}
	} else if _, err := os.Lstat(destination); err == nil {
		return ErrStateIdentity
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(stage, destination); err != nil {
		if replaceExisting {
			_ = os.Rename(backup, destination)
			_ = syncDirectoryForPublication(parent)
		}
		return err
	}
	removeStage = false
	if err := syncDirectoryForPublication(parent); err != nil {
		return err
	}
	if replaceExisting {
		if err := os.RemoveAll(backup); err != nil {
			return err
		}
		if err := syncDirectoryForPublication(parent); err != nil {
			return err
		}
	}
	return nil
}

func cleanupProductRecovery(destination, operationID string) error {
	if !stateHex32.MatchString(operationID) {
		return ErrStateIdentity
	}
	for _, candidate := range []string{
		destination + ".staging-" + operationID,
		destination + ".backup-" + operationID,
	} {
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafeState
		}
		if err := os.RemoveAll(candidate); err != nil {
			return err
		}
		if err := syncDirectoryForPublication(
			filepath.Dir(destination),
		); err != nil {
			return err
		}
	}
	return nil
}

func productRecoveryClean(
	destination string,
	operationID string,
) (bool, error) {
	if !stateHex32.MatchString(operationID) {
		return false, ErrStateIdentity
	}
	for _, candidate := range []string{
		destination + ".staging-" + operationID,
		destination + ".backup-" + operationID,
	} {
		_, err := os.Lstat(candidate)
		if err == nil {
			return false, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return true, nil
}

func inspectInstalledProductTree(
	root string,
	files []payloadManifestFile,
	platform hostbootstrap.Platform,
) (bool, bool, bool, error) {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, true, nil
	}
	if err != nil {
		return false, false, false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return true, false, false, ErrUnsafeState
	}
	expectedFiles := make(map[string]payloadManifestFile)
	expectedDirectories := productDirectoryModes(files)
	for _, file := range files {
		if file.Type == payloadTypeFile {
			expectedFiles[file.Path] = file
		}
	}
	partialExact := true
	err = filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.Type()&os.ModeSymlink != 0 {
			return ErrUnsafeState
		}
		if entry.IsDir() {
			expectedMode, found := expectedDirectories[relative]
			if !found {
				partialExact = false
			} else if platform != hostbootstrap.PlatformWindows {
				info, err := entry.Info()
				if err != nil {
					return err
				}
				if info.Mode().Perm() != expectedMode.Perm() {
					partialExact = false
				}
			}
			delete(expectedDirectories, relative)
			return nil
		}
		manifest, found := expectedFiles[relative]
		if !found {
			partialExact = false
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		links, err := nativeFileLinkCount(current, info)
		if err != nil || links != 1 || !info.Mode().IsRegular() || info.Size() != manifest.Size {
			return ErrUnsafeState
		}
		if platform != hostbootstrap.PlatformWindows && info.Mode().Perm() != manifest.Mode.Perm() {
			partialExact = false
		}
		file, err := os.Open(current)
		if err != nil {
			return err
		}
		hash := sha256.New()
		written, copyErr := io.Copy(hash, io.LimitReader(file, manifest.Size+1))
		closeErr := file.Close()
		if copyErr != nil ||
			closeErr != nil ||
			written != manifest.Size ||
			hex.EncodeToString(hash.Sum(nil)) != manifest.SHA256 {
			partialExact = false
		}
		delete(expectedFiles, relative)
		return nil
	})
	if err != nil {
		return true, false, false, err
	}
	complete := len(expectedFiles) == 0 &&
		len(expectedDirectories) == 0
	return true, complete && partialExact, partialExact, nil
}

func verifyInstalledProductTree(
	root string,
	files []payloadManifestFile,
	platform hostbootstrap.Platform,
) (bool, bool, error) {
	present, exact, _, err := inspectInstalledProductTree(
		root,
		files,
		platform,
	)
	return present, exact, err
}

func productUninstallTrashPath(root string, operationID string) string {
	return root + ".uninstall-" + operationID
}

func renameInstalledProductToTrash(
	root string,
	trash string,
	files []payloadManifestFile,
	platform hostbootstrap.Platform,
) error {
	if _, err := os.Lstat(trash); err == nil {
		if _, rootErr := os.Lstat(root); errors.Is(rootErr, fs.ErrNotExist) {
			return nil
		}
		return ErrStateIdentity
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	present, exact, err := verifyInstalledProductTree(root, files, platform)
	if err != nil {
		return err
	}
	if !present || !exact {
		return ErrStateIdentity
	}
	if err := os.Rename(root, trash); err != nil {
		return err
	}
	return syncDirectoryForPublication(filepath.Dir(root))
}

func removeProductUninstallTrash(trash string) error {
	info, err := os.Lstat(trash)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrUnsafeState
	}
	if err := os.RemoveAll(trash); err != nil {
		return err
	}
	return syncDirectoryForPublication(filepath.Dir(trash))
}

func (nativePayloadFS) OpenPayloadFile(path string) (io.ReadCloser, int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, 0, err
	}
	links, err := nativeFileLinkCount(path, info)
	if err != nil ||
		!info.Mode().IsRegular() ||
		info.Mode()&os.ModeSymlink != 0 ||
		links != 1 {
		return nil, 0, ErrPayloadInvalid
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, 0, ErrPayloadInvalid
	}
	return file, opened.Size(), nil
}
