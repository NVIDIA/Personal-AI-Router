// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkModel builds a directory model under root and returns its path.
func mkModel(t *testing.T, root, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	for rel, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The model path arrives from a peer over the wire, so this is the boundary that
// decides which directories a paired node can read off this machine. Anything
// outside the configured model roots has to be refused, however it is spelled.
func TestResolveModelDirRefusesAnythingOutsideTheModelRoots(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MLX_MODELS_DIRS", root)
	good := mkModel(t, root, "Qwen3.8-27B-3bit", map[string]string{"config.json": "{}"})

	if got, err := resolveModelDir(good); err != nil || got == "" {
		t.Fatalf("resolveModelDir(%q) = %q, %v; want the directory", good, got, err)
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A sibling that merely shares the root's prefix must not pass: the
	// containment test is on a separator boundary, not a string prefix.
	sibling := root + "-evil"
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sibling) })

	// A symlink INSIDE the root pointing out of it is the interesting case: a
	// prefix check on the un-resolved path would let it through.
	escape := filepath.Join(root, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	for _, bad := range []string{
		outside,
		sibling,
		escape,
		filepath.Join(good, "..", "..", "etc"),
		"relative/path",
		"",
	} {
		if _, err := resolveModelDir(bad); err == nil {
			t.Errorf("resolveModelDir(%q) was allowed; it is outside the model roots", bad)
		}
	}
}

// Every byte must be verifiable before it lands, the same property the cache
// transfer has. A directory carries no digest of its own, so the manifest is
// where it comes from.
func TestDirModelManifestRoundTripVerifiesContent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MLX_MODELS_DIRS", root)
	src := mkModel(t, root, "Qwen3.8-27B-3bit", map[string]string{
		"config.json":           `{"model_type":"qwen3"}`,
		"tokenizer_config.json": `{"bos_token":"<s>"}`,
		"model.safetensors":     "weights-weights-weights",
		"nested/extra.json":     `{"a":1}`,
	})

	m, err := readDirManifest(src)
	if err != nil {
		t.Fatalf("readDirManifest: %v", err)
	}
	if len(m.Files) != 4 {
		t.Fatalf("manifest lists %d files, want 4", len(m.Files))
	}
	if m.Revision == "" {
		t.Error("manifest revision is empty; fetchCacheManifest rejects that")
	}
	// Sorted, so an interrupted transfer resumes over the same sequence.
	for i := 1; i < len(m.Files); i++ {
		if m.Files[i-1].Path >= m.Files[i].Path {
			t.Errorf("manifest is not sorted: %q before %q", m.Files[i-1].Path, m.Files[i].Path)
		}
	}
	// Paths are slash-form on the wire, never absolute.
	for _, f := range m.Files {
		if strings.HasPrefix(f.Path, "/") || strings.Contains(f.Path, "..") {
			t.Errorf("manifest path %q is not a safe relative path", f.Path)
		}
	}

	// Replay it into a second root, exactly as PullModelFromPeer does.
	dstRoot := t.TempDir()
	dst := filepath.Join(dstRoot, filepath.Base(src))
	for _, f := range m.Files {
		body, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(dst, filepath.FromSlash(f.Path))
		if err := writeDirModelFile(out, m.Sizes[f.OID], f.OID, bytes.NewReader(body)); err != nil {
			t.Fatalf("writeDirModelFile(%s): %v", f.Path, err)
		}
		got, err := os.ReadFile(out)
		if err != nil || !bytes.Equal(got, body) {
			t.Errorf("%s did not round-trip", f.Path)
		}
	}
}

func TestWriteDirModelFileRejectsCorruptionAndLeavesNoPartial(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "model.safetensors")
	good := []byte("the real weights")
	m, err := readDirManifest(mkModel(t, dir, "m", map[string]string{"model.safetensors": string(good)}))
	if err != nil {
		t.Fatal(err)
	}
	oid, size := m.Files[0].OID, m.Sizes[m.Files[0].OID]

	if err := writeDirModelFile(dest, size, oid, bytes.NewReader([]byte("tampered bytes!!"))); err == nil {
		t.Error("a file whose content does not match its digest was accepted")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("a rejected file was left on disk where a catalogue scan would find it")
	}

	// A truncated transfer must fail too, not silently write a short file.
	if err := writeDirModelFile(dest, size, oid, bytes.NewReader(good[:4])); err == nil {
		t.Error("a truncated file was accepted")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("a truncated file was left on disk")
	}

	if err := writeDirModelFile(dest, size, oid, bytes.NewReader(good)); err != nil {
		t.Fatalf("the correct bytes were rejected: %v", err)
	}
}

func TestDirModelFilePathRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MLX_MODELS_DIRS", root)
	src := mkModel(t, root, "m", map[string]string{"config.json": "{}"})

	if _, err := dirModelFilePath(src, "config.json"); err != nil {
		t.Fatalf("a legitimate file was refused: %v", err)
	}
	for _, bad := range []string{"../../../etc/passwd", "/etc/passwd", "..", "a/../../b", ""} {
		if _, err := dirModelFilePath(src, bad); err == nil {
			t.Errorf("dirModelFilePath(%q) was allowed", bad)
		}
	}
}

// The catalogue ids these models by path, so a repo id and a path have to be
// told apart before either is used to build a filesystem location.
func TestIsDirModelRef(t *testing.T) {
	for _, p := range []string{"/Users/me/models/Qwen3.8-27B-3bit", "/tmp/m"} {
		if !isDirModelRef(p) {
			t.Errorf("%q should be treated as a directory model", p)
		}
	}
	for _, r := range []string{"mlx-community/Llama-3.2-1B-Instruct-4bit", "bert-base-uncased"} {
		if isDirModelRef(r) {
			t.Errorf("%q is a Hugging Face repo id, not a path", r)
		}
	}
}

// The delete that did not delete: a model under the models directory is found by
// scanning, never appears in the registry file, and so a delete that only edited
// that file removed nothing -- leaving 11 GB the UI could not get rid of.
func TestDeleteRoutesAPathModelToTheActionThatRemovesFiles(t *testing.T) {
	action, params, err := modelActionWire("mlx", "delete", "/Users/me/models/Qwen3.8-27B-3bit")
	if err != nil {
		t.Fatal(err)
	}
	if action != "delete_model_path" {
		t.Errorf("a path model routed to %q; the cache delete cannot remove it", action)
	}
	if !strings.Contains(string(params), "Qwen3.8-27B-3bit") {
		t.Errorf("params %s lost the model", params)
	}

	// A repo id still belongs to the cache delete.
	action, _, err = modelActionWire("mlx", "delete", "mlx-community/Llama-3.2-1B-Instruct-4bit")
	if err != nil {
		t.Fatal(err)
	}
	if action != "delete_model" {
		t.Errorf("a repo id routed to %q, want delete_model", action)
	}

	// Other engines are untouched by the MLX branch.
	if a, _, _ := modelActionWire("ollama", "delete", "llama3"); a != "delete_model" {
		t.Errorf("ollama delete routed to %q", a)
	}
}

// {models_dir} is the confinement root for the delete. Pointing MLX at LM
// Studio's directory would make every MLX delete fail the containment check.
func TestEngineModelsDirIsPerEngine(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MLX_MODELS_DIRS", root)
	if got := engineModelsDir("mlx"); got != root {
		t.Errorf("engineModelsDir(mlx) = %q, want %q", got, root)
	}
	if got := engineModelsDir("lm-studio"); got == root {
		t.Error("lm-studio resolved to the MLX models directory")
	}
}
