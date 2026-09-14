// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TOFU is the only integrity control available for a versionless vendor URL
// with no published checksum (LM Studio's install.sh). It has to allow the
// first sighting, allow an unchanged repeat, and refuse a change.
func TestInstallerPinTOFU(t *testing.T) {
	// appdir reads the per-user data dir from the environment; redirect it so
	// the test never touches the real one.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	const url = "https://lmstudio.ai/install.sh"
	const first = "aaaa000000000000000000000000000000000000000000000000000000000000"
	const changed = "bbbb000000000000000000000000000000000000000000000000000000000000"

	path, err := installerPinsPath()
	if err != nil {
		t.Skipf("no writable app dir in this environment: %v", err)
	}
	os.Remove(path)

	if err := checkInstallerPin("lmstudio", url, first); err != nil {
		t.Fatalf("first sighting must be allowed, got %v", err)
	}
	if err := checkInstallerPin("lmstudio", url, first); err != nil {
		t.Fatalf("unchanged repeat must be allowed, got %v", err)
	}

	err = checkInstallerPin("lmstudio", url, changed)
	if err == nil {
		t.Fatal("a changed installer must fail closed")
	}
	// The refusal is only actionable if it names both digests and the file to
	// delete; without that the operator cannot tell a vendor release from an
	// attack, or clear it.
	for _, want := range []string{first, changed, url, path} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must mention %q; got: %v", want, err)
		}
	}

	// The refusal must point at the single entry, not the whole file: accepting
	// one change must not cost every other engine its pin.
	if !strings.Contains(err.Error(), "every other engine") {
		t.Errorf("refusal should warn against deleting the whole file; got: %v", err)
	}

	// Removing just that entry is the documented confirmation, so it must work.
	pins, loadErr := loadInstallerPins(path)
	if loadErr != nil {
		t.Fatalf("load pins: %v", loadErr)
	}
	delete(pins, url)
	data, _ := json.MarshalIndent(pins, "", "  ")
	os.WriteFile(path, data, 0o600)
	if err := checkInstallerPin("lmstudio", url, changed); err != nil {
		t.Fatalf("after clearing the record the new digest must be accepted, got %v", err)
	}

	// A corrupt record must NOT read as "no pins": that would silently re-trust
	// every installer, and it is a state an attacker can create by truncating
	// the file.
	os.WriteFile(path, []byte("{not json"), 0o600)
	if err := checkInstallerPin("lmstudio", url, first); err == nil {
		t.Error("an unreadable pin record must refuse, not reset trust")
	}
	os.Remove(path)
	if err := checkInstallerPin("lmstudio", url, first); err != nil {
		t.Fatalf("a missing record is the only 'no pins' case: %v", err)
	}

	// A different URL is independent.
	if err := checkInstallerPin("other", "https://example.test/x.sh", first); err != nil {
		t.Fatalf("an unrelated url must not be affected, got %v", err)
	}
}

// A pinned manifest must never consult the TOFU record: its digest is the
// authority, and Ollama's six downloads are pinned precisely so a vendor
// release is a manifest update rather than a prompt on every machine.
func TestPinnedManifestsDoNotNeedTOFU(t *testing.T) {
	reg := NewRegistry()
	if err := reg.LoadFS(bundledManifests, "manifests"); err != nil {
		t.Fatalf("bundled manifests: %v", err)
	}
	for _, engine := range []string{"ollama", "mlx"} {
		m, ok := reg.Get(engine)
		if !ok {
			t.Fatalf("%s manifest missing", engine)
		}
		for key, plat := range m.Platforms {
			if plat.Install == nil || plat.Install.Fetch == nil {
				continue
			}
			if strings.TrimSpace(plat.Install.Fetch.SHA256) == "" {
				t.Errorf("%s %s: fetch is unpinned; %s must stay pinned", engine, key, engine)
			}
		}
	}
}
