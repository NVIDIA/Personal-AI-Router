// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestCurrentTwelveBinaryEnrollmentPackageRemainsAccepted(t *testing.T) {
	want := append([]string(nil), onboardingBinaries...)
	slices.Sort(want)
	if len(want) != 12 ||
		!slices.Contains(want, "nvpair-proxy") ||
		slices.Contains(want, "nvpair-host-bootstrap") ||
		slices.Contains(want, "nvpair-host-helper") {
		t.Fatalf("product enrollment inventory changed: %v", want)
	}
	// The receiver and post-install inspector are separate wire consumers;
	// compare their explicit allowlists with the actual service catalog.
	pattern := regexp.MustCompile(`(?m)^\s*names=\{([^}\r\n]+)\}`)
	for name, script := range map[string]string{"receiver": onboardingReceiveScript, "installed-inspector": onboardingOwnedScript} {
		t.Run(name, func(t *testing.T) {
			matches := pattern.FindAllStringSubmatch(script, -1)
			if len(matches) != 1 {
				t.Fatal("missing or ambiguous installer inventory")
			}
			actual := strings.Split(matches[0][1], ",")
			for i, value := range actual {
				if len(value) < 3 || value[0] != '\'' || value[len(value)-1] != '\'' {
					t.Fatal("unsupported inventory literal")
				}
				actual[i] = value[1 : len(value)-1]
			}
			slices.Sort(actual)
			if !slices.Equal(actual, want) {
				t.Fatalf("%s drops or substitutes a required component: %v", name, actual)
			}
		})
	}
	file, artifact := writeOnboardingFixture(
		t,
		onboardingFixtureEntries(183),
	)
	if _, _, err := verifyOnboardingArchive(file, artifact); err != nil {
		t.Fatalf("current twelve-binary enrollment package rejected: %v", err)
	}
}

func TestOnboardingRejectsOlderOrIncompleteEngineBundle(t *testing.T) {
	for _, omitted := range [][]string{{"nvpair-proxy"}} {
		t.Run(strings.Join(omitted, "+"), func(t *testing.T) {
			entries := onboardingFixtureEntries(183)
			filtered := make([]onboardingArchiveFixtureEntry, 0, len(entries))
			for _, entry := range entries {
				if slices.Contains(omitted, filepath.Base(entry.name)) {
					continue
				}
				if strings.HasSuffix(entry.name, "/manifest.json") {
					var manifest onboardingBuildManifest
					if err := json.Unmarshal(entry.body, &manifest); err != nil {
						t.Fatal(err)
					}
					manifest.Files = slices.DeleteFunc(manifest.Files, func(item struct {
						FileName string `json:"fileName"`
						Size     int64  `json:"size"`
						SHA256   string `json:"sha256"`
					}) bool {
						return slices.Contains(omitted, item.FileName)
					})
					for _, name := range omitted {
						delete(manifest.Components, name)
					}
					var err error
					entry.body, err = json.Marshal(manifest)
					if err != nil {
						t.Fatal(err)
					}
				}
				filtered = append(filtered, entry)
			}
			file, artifact := writeOnboardingFixture(t, filtered)
			if _, _, err := verifyOnboardingArchive(file, artifact); err == nil {
				t.Fatal("self-consistent older/incomplete package was accepted for both-engine enrollment")
			}
		})
	}
}
