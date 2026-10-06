// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// bundledManifestSet loads the compiled-in manifests the way main.go does.
func bundledManifestSet(t *testing.T) map[string]*Manifest {
	t.Helper()
	reg := NewRegistry()
	if err := reg.LoadFS(bundledManifests, "manifests"); err != nil {
		t.Fatalf("load bundled manifests: %v", err)
	}
	out := map[string]*Manifest{}
	for _, name := range reg.Names() {
		manifest, ok := reg.Get(name)
		if !ok {
			t.Fatalf("registry lost manifest %q", name)
		}
		out[name] = manifest
	}
	if len(out) == 0 {
		t.Fatal("no bundled manifests loaded")
	}
	return out
}

// TestBundledRuntimeBinIsDetected keeps detection and launch pointing at the
// same file.
//
// A detect path is a claim about what the install produces, and nothing checks
// it at install time: the runner extracts, looks for the declared path, finds
// nothing, and reports "was not detected after install" with no way to say
// why. That is what llama.cpp did on macOS and Linux, where the manifest named
// the layout of a local cmake build (build/bin/llama-server) rather than of the
// published release archive.
//
// Whether a path matches the archive is only knowable from the archive, so the
// real check is TestLiveBundledInstallLayout, which downloads each one. What is
// checkable here is that the two paths agree: detecting one file and launching
// another lets an engine report installed and then fail to start.
//
// There is deliberately no rule about how deep a detect path may be. Archive
// shapes are the vendor's choice and they differ — llama.cpp wraps everything
// in a build-tagged directory the install has to strip, while Ollama's Linux
// archive is already bin/ and lib/ and must not be stripped. A convention
// asserted here would only encode one vendor's habit as if it were a contract.
func TestBundledRuntimeBinIsDetected(t *testing.T) {
	for name, manifest := range bundledManifestSet(t) {
		for key, platform := range manifest.Platforms {
			bin := platform.Runtime.Bin
			if bin == "" || len(platform.Detect) == 0 {
				continue
			}
			found := false
			for _, candidate := range platform.Detect {
				if candidate == bin {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s/%s: runtime.bin %q is not among the detect paths %v — the engine would report installed and then fail to start",
					name, key, bin, platform.Detect)
			}
		}
	}
}

// TestBundledInstallLayout downloads every archive the bundled manifests
// install from, extracts it the way the manifest says to, and checks the detect
// path appears. This is the check that was missing when llama.cpp shipped a
// detect path no release archive could satisfy.
//
// Gated on an environment variable rather than a build tag: the archives run to
// gigabytes, but the `live` tag does not currently compile in this package, and
// a verification nobody can run is not one.
//
// Extraction runs through the host's tar, which libarchive-backed tar handles
// for .tar.gz, .tar.zst and .zip alike, so one host can verify another
// platform's archive. The --strip-components the manifest declares is applied,
// because that flag is what decides whether the detect path resolves. A
// platform whose install is not a tar invocation (Windows llama.cpp uses
// Expand-Archive) is still covered: only the extraction mechanism differs, and
// the archive it reads is the one fetched here.
//
//	NVPAIR_LIVE_LAYOUT=1 go test -run TestBundledInstallLayout -v -timeout 3600s
func TestBundledInstallLayout(t *testing.T) {
	if os.Getenv("NVPAIR_LIVE_LAYOUT") == "" {
		t.Skip("set NVPAIR_LIVE_LAYOUT=1 to download every engine archive and verify its layout")
	}
	for name, manifest := range bundledManifestSet(t) {
		for key, platform := range manifest.Platforms {
			urls := archiveURLs(platform)
			if len(urls) == 0 {
				continue // vendor script or detect-only engine; nothing to unpack
			}
			t.Run(name+"/"+key, func(t *testing.T) {
				installDir := t.TempDir()
				strip := strings.Contains(strings.Join(platform.Install.Run, " "), "--strip-components=1")
				for _, url := range urls {
					extractArchive(t, url, installDir, strip)
				}
				for _, candidate := range platform.Detect {
					if !strings.HasPrefix(candidate, "{install_dir}") {
						continue
					}
					relative := strings.TrimLeft(strings.TrimPrefix(candidate, "{install_dir}"), `/\`)
					// Manifests spell Windows paths with backslashes; the host
					// separator is what the extracted tree uses.
					relative = filepath.FromSlash(strings.ReplaceAll(relative, `\`, "/"))
					if _, err := os.Stat(filepath.Join(installDir, relative)); err != nil {
						t.Errorf("detect path %q is absent after extracting %v (strip=%v): %v",
							candidate, urls, strip, err)
					}
				}
			})
		}
	}
}

// archiveURLs lists the downloads a platform's install unpacks, in the order
// the manifest extracts them.
func archiveURLs(platform Platform) []string {
	if platform.Install == nil || len(platform.Install.Run) == 0 {
		return nil
	}
	var urls []string
	if platform.Install.Fetch != nil {
		urls = append(urls, platform.Install.Fetch.URL)
	}
	for _, artifact := range platform.Install.Artifacts {
		urls = append(urls, artifact.URL)
	}
	return urls
}

func extractArchive(t *testing.T, url, installDir string, strip bool) {
	t.Helper()
	archive := filepath.Join(t.TempDir(), filepath.Base(url))
	// curl rather than net/http: these are large, redirected downloads and the
	// point here is the archive's interior, not the transfer.
	fetch := exec.Command("curl", "-sSL", "--fail", "--max-time", "900", "-o", archive, url)
	if out, err := fetch.CombinedOutput(); err != nil {
		t.Skipf("cannot download %s (%v): %s", url, err, strings.TrimSpace(string(out)))
	}
	args := []string{"-xf", archive, "-C", installDir}
	if strip {
		args = append(args, "--strip-components=1")
	}
	extract := exec.Command("tar", args...)
	if out, err := extract.CombinedOutput(); err != nil {
		t.Fatalf("tar %v on %s failed (%v): %s", args, runtime.GOOS, err, strings.TrimSpace(string(out)))
	}
}
