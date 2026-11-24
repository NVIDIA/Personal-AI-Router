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
	"strconv"
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

// Each upstream has one entry. A new tagged release replaces the old entry.
// The gate lets concurrent clients share a lookup and cancel their own wait.
type cachedGitHubRelease struct {
	gate    chan struct{}
	ref     string
	release *githubRelease
	expires time.Time
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
	e.mu.Lock()
	entry := e.githubReleases[repo]
	if entry == nil {
		entry = &cachedGitHubRelease{gate: make(chan struct{}, 1)}
		e.githubReleases[repo] = entry
	}
	e.mu.Unlock()
	select {
	case entry.gate <- struct{}{}:
		defer func() { <-entry.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if entry.ref == ref && time.Now().Before(entry.expires) {
		return entry.release, nil
	}
	e.mu.Lock()
	retryAt := e.githubRetryAt
	e.mu.Unlock()
	if time.Now().Before(retryAt) {
		return nil, githubRateLimitError(retryAt)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+repo+"/releases/"+ref, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("check %s releases: %w", repo, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if retryAt := githubRateLimitReset(resp); !retryAt.IsZero() {
			e.mu.Lock()
			if retryAt.After(e.githubRetryAt) {
				e.githubRetryAt = retryAt
			}
			e.mu.Unlock()
			return nil, fmt.Errorf("check %s releases: HTTP %d: %w", repo, resp.StatusCode, githubRateLimitError(retryAt))
		}
		if resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("GitHub denied access to %s release metadata (HTTP 403). Check this node's access to api.github.com", repo)
		}
		return nil, fmt.Errorf("check %s releases: HTTP %d", repo, resp.StatusCode)
	}
	var release githubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&release); err != nil {
		return nil, fmt.Errorf("check %s releases: %w", repo, err)
	}
	if release.Tag == "" || release.Draft || (!tagged && release.Prerelease) || (tagged && release.Tag != tag) {
		return nil, fmt.Errorf("%s did not return the requested release %q", repo, ref)
	}
	// The vendor's stable pointer is still read on each llama.cpp check.
	// Reuse its exact build and checksums through the six-hour check window.
	ttl := 15 * time.Minute
	if tagged {
		ttl = 6 * time.Hour
	}
	entry.ref, entry.release, entry.expires = ref, &release, time.Now().Add(ttl)
	return entry.release, nil
}

func githubRateLimitError(retryAt time.Time) error {
	return fmt.Errorf("GitHub's API rate limit was reached. Retry after %s", retryAt.UTC().Format(time.RFC1123))
}

func githubRateLimitReset(resp *http.Response) time.Time {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return time.Time{}
	}
	now := time.Now()
	if seconds, err := strconv.ParseUint(resp.Header.Get("Retry-After"), 10, 32); err == nil {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	exhausted := resp.Header.Get("X-RateLimit-Remaining") == "0"
	if exhausted {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && reset > now.Unix() {
			return time.Unix(reset, 0)
		}
	}
	// GitHub can report a secondary limit in its JSON message without a
	// Retry-After header. Keep unrelated HTTP 403 responses distinguishable.
	if !exhausted && resp.StatusCode == http.StatusForbidden {
		var details struct{ Message string }
		if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&details) != nil || !strings.Contains(strings.ToLower(details.Message), "rate limit") {
			return time.Time{}
		}
	}
	return now.Add(time.Minute)
}
