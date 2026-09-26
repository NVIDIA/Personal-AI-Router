// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"regexp"
	"strings"
	"testing"
)

type qwen38ProviderFileFixture struct {
	hash  string
	bytes int64
}

func qwen38ProviderProfileIO(profile vllmQwen38ProviderProfile) (vllmQwen38ProviderIO, map[string]qwen38ProviderFileFixture, map[string]string) {
	files := make(map[string]qwen38ProviderFileFixture)
	packages := make(map[string]string)
	for _, provider := range profile.Providers {
		files[provider.Path] = qwen38ProviderFileFixture{hash: provider.SHA256, bytes: provider.Bytes}
		for _, pkg := range provider.Packages {
			packages[pkg.Name] = pkg.Identity
		}
	}
	return vllmQwen38ProviderIO{
		hashFile: func(_ context.Context, path string) (string, int64, error) {
			value, ok := files[path]
			if !ok {
				return "", 0, os.ErrNotExist
			}
			return value.hash, value.bytes, nil
		},
		packageIdentity: func(_ context.Context, name string) (string, error) {
			value, ok := packages[name]
			if !ok {
				return "", os.ErrNotExist
			}
			return value, nil
		},
	}, files, packages
}

func TestQwen38DGX76ProviderProfileHasExactClosedIdentity(t *testing.T) {
	profile := qwen38DGX76ProviderProfile()
	providerIO, _, _ := qwen38ProviderProfileIO(profile)
	observed := observeQwen38ProviderProfile(context.Background(), profile, providerIO)
	if !observed.Qualified || observed.ProfileID != vllmQwen38ProviderProfileDGX76 || observed.ObservedClosureSHA256 != vllmQwen38ProviderClosureDGX76 || len(observed.Mismatches) != 0 {
		t.Fatalf("DGX OS 7.6 provider profile changed: %+v", observed)
	}
	wire, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(wire, &fields); err != nil || string(fields["allowedClosureSha256"]) != "[]" || string(fields["mismatches"]) != "[]" {
		t.Fatalf("clean provider omitted required empty arrays: %s %v", wire, err)
	}
	recipe, err := qwen38RuntimeRecipe()
	if err != nil {
		t.Fatal(err)
	}
	profiles := qwen38ProviderProfiles(recipe)
	if len(profiles) != 3 || profiles[0].ID != vllmQwen38ProviderProfileA || profiles[1].ID != vllmQwen38ProviderProfileBC || profiles[2].ID != vllmQwen38ProviderProfileDGX76 {
		t.Fatalf("closed provider catalog changed: %+v", profiles)
	}
	for _, closure := range []string{vllmQwen38ProviderClosureSHA256, vllmQwen38ProviderClosureBC, vllmQwen38ProviderClosureDGX76} {
		if !knownQwen38ProviderClosure(closure) {
			t.Fatalf("cataloged provider closure refused: %s", closure)
		}
	}
	if knownQwen38ProviderClosure(strings.Repeat("0", 64)) {
		t.Fatal("uncataloged provider closure admitted")
	}
}

// The root rank owner re-checks a prepared runtime's provider closure at start,
// so its admitted set must match the catalog preparation uses exactly.
func TestQwen38RankOwnerAdmitsExactlyTheCatalogedProviderProfiles(t *testing.T) {
	source := strings.ReplaceAll(vllmRankSystemWorkerSource, "\r\n", "\n")
	block := func(name string) string {
		start := strings.Index(source, "\n"+name+" = {")
		if start < 0 {
			t.Fatalf("rank owner lacks %s", name)
		}
		end := strings.Index(source[start+1:], "\n}")
		return source[start+1 : start+1+end]
	}
	reference := regexp.MustCompile(`(?m)^QWEN_REFERENCE_PROVIDER_CLOSURE = "([0-9a-f]{64})"$`).FindStringSubmatch(source)
	if reference == nil {
		t.Fatal("rank owner lacks its reference provider closure")
	}
	resolve := func(value string) string {
		if value == "QWEN_REFERENCE_PROVIDER_CLOSURE" {
			return reference[1]
		}
		return strings.Trim(value, `"`)
	}
	ownerProfiles := map[string]string{}
	for _, match := range regexp.MustCompile(`"([a-z0-9.-]+)": ("[0-9a-f]{64}"|QWEN_REFERENCE_PROVIDER_CLOSURE),`).FindAllStringSubmatch(block("QWEN_PROVIDER_PROFILES"), -1) {
		ownerProfiles[match[1]] = resolve(match[2])
	}
	ownerClosures := map[string]bool{}
	for _, match := range regexp.MustCompile(`(?m)^\s+("[0-9a-f]{64}"|QWEN_REFERENCE_PROVIDER_CLOSURE),$`).FindAllStringSubmatch(block("QWEN_PROVIDER_CLOSURES"), -1) {
		ownerClosures[resolve(match[1])] = true
	}
	recipe, err := qwen38RuntimeRecipe()
	if err != nil {
		t.Fatal(err)
	}
	catalog := qwen38ProviderProfiles(recipe)
	catalogProfiles, catalogClosures := map[string]string{}, map[string]bool{}
	for _, profile := range catalog {
		catalogProfiles[profile.ID] = profile.ClosureSHA256
		catalogClosures[profile.ClosureSHA256] = true
	}
	if !maps.Equal(ownerProfiles, catalogProfiles) || !maps.Equal(ownerClosures, catalogClosures) {
		t.Fatalf("rank owner admits profiles %v closures %v; catalog has %v %v", ownerProfiles, ownerClosures, catalogProfiles, catalogClosures)
	}
}

func TestQwen38DGX76ProviderProfileRefusesDrift(t *testing.T) {
	profile := qwen38DGX76ProviderProfile()
	for name, drift := range map[string]func(map[string]qwen38ProviderFileFixture, map[string]string){
		"file hash": func(files map[string]qwen38ProviderFileFixture, _ map[string]string) {
			value := files[profile.Providers[0].Path]
			value.hash = strings.Repeat("0", 64)
			files[profile.Providers[0].Path] = value
		},
		"file bytes": func(files map[string]qwen38ProviderFileFixture, _ map[string]string) {
			value := files[profile.Providers[0].Path]
			value.bytes++
			files[profile.Providers[0].Path] = value
		},
		"package identity": func(_ map[string]qwen38ProviderFileFixture, packages map[string]string) {
			packages[profile.Providers[0].Packages[0].Name] = "install ok installed|changed|arm64"
		},
	} {
		t.Run(name, func(t *testing.T) {
			providerIO, files, packages := qwen38ProviderProfileIO(profile)
			drift(files, packages)
			observed := observeQwen38ProviderProfile(context.Background(), profile, providerIO)
			if observed.Qualified || observed.ObservedClosureSHA256 == vllmQwen38ProviderClosureDGX76 || len(observed.Mismatches) == 0 {
				t.Fatalf("drifted DGX OS 7.6 provider admitted: %+v", observed)
			}
		})
	}
}
