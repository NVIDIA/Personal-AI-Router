// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type engineUpdateSnapshot struct {
	binPath     string
	processPath string
	port        int
	running     bool
	adopted     bool
}

type githubReleaseAsset struct {
	Name               string `json:"name"`
	Digest             string `json:"digest"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type githubRelease struct {
	Tag        string               `json:"tag_name"`
	Assets     []githubReleaseAsset `json:"assets"`
	Draft      bool                 `json:"draft"`
	Prerelease bool                 `json:"prerelease"`
}

// A version probe can read a local executable or listener and then wait on a
// remote release feed. Keep that I/O outside the lifecycle lock and discard
// the result if the installation changed while the check was in flight.
func (e *Executor) checkInstalledEngineUpdate(
	ctx context.Context,
	engine string,
	check func(context.Context, *engineState, engineUpdateSnapshot) (*EngineUpdateInfo, error),
) (*EngineUpdateInfo, error) {
	manifest, ok := e.reg.Get(engine)
	if !ok {
		return nil, nil
	}
	if _, ok := manifest.HostPlatform(); !ok {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	st, err := e.state(engine)
	if err != nil {
		return nil, err
	}
	if _, err := e.statusWithContext(ctx, engine, 0, true); err != nil {
		return nil, err
	}
	st.mu.Lock()
	if st.opDone != nil {
		st.mu.Unlock()
		return nil, fmt.Errorf("%s is changing state", engine)
	}
	if !st.installed {
		st.mu.Unlock()
		return nil, nil
	}
	snapshot := engineUpdateSnapshot{binPath: st.binPath, port: st.port, running: st.running, adopted: st.adopted}
	if st.proc != nil {
		snapshot.processPath = st.proc.cmd.Path
	}
	revision := st.revision
	st.mu.Unlock()

	result, err := check(ctx, st, snapshot)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.revision != revision {
		return nil, fmt.Errorf("%s changed during the update check", engine)
	}
	return result, nil
}

func readEngineVersionJSON(client *http.Client, req *http.Request, result any) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(result)
}

// Use the node's original loopback port. A PAIR proxy marks identity probes
// as unsupported, and a redirect could supply a different engine's version.
func (e *Executor) readRunningEngineVersion(ctx context.Context, port int, path string, result any) error {
	endpoint := url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", port), Path: path}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set(engineIdentityProbeHeader, "1")
	client := *e.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return readEngineVersionJSON(&client, req, result)
}

func (e *Executor) latestGitHubRelease(ctx context.Context, repo string) (*githubRelease, error) {
	return e.githubRelease(ctx, repo, "latest")
}

// Providers choose their upstream repository. A provider that resolves an
// exact vendor-recommended tag may accept a release GitHub labels prerelease.
func (e *Executor) githubRelease(ctx context.Context, repo, ref string) (*githubRelease, error) {
	tag, tagged := strings.CutPrefix(ref, "tags/")
	if ref != "latest" && (!tagged || tag == "" || tag == "." || tag == ".." || url.PathEscape(tag) != tag) {
		return nil, fmt.Errorf("invalid GitHub release reference %q", ref)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+repo+"/releases/"+ref, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	var release githubRelease
	if err := readEngineVersionJSON(e.client, req, &release); err != nil {
		return nil, fmt.Errorf("check %s releases: %w", repo, err)
	}
	if release.Tag == "" || release.Draft || (!tagged && release.Prerelease) || (tagged && release.Tag != tag) {
		return nil, fmt.Errorf("%s did not return the requested release %q", repo, ref)
	}
	return &release, nil
}
