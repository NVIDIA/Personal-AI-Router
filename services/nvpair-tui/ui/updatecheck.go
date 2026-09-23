// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Update awareness: notice that a newer release exists and say so.
//
// Deliberately not an updater. It downloads nothing and installs nothing, which
// is not timidity — the terminal client is not one binary. It resolves the broker
// beside its own executable and that broker spawns eleven workers from the same
// directory, each independently versioned, so replacing "the client" means
// swapping fourteen binaries atomically while they are serving inference. A
// partial swap leaves a new client driving old workers across a JSON-RPC
// contract that may have changed, which is the failure service-contracts:check
// exists to catch at build time. Telling the operator and letting them install
// the release properly is the honest half of the job.
//
// The desktop app has real auto-update, and on an app install this client is
// already carried along by it: nvpair-tui ships inside cli-bin, and the nvpair
// launcher holds an absolute path into it, so replacing the bundle replaces this
// binary too. What has no updater is a services-tarball install on a headless
// box, which is exactly where this notice is worth having.

// ProductVersion is the release this binary was built from, stamped by the build
// scripts with -X nvpair-tui/ui.ProductVersion=...
//
// The product version, not this component's own. They are different numbers —
// versions.json carries nvpair-tui at 0.7.2 inside product 0.91.7 — and the
// release feed publishes the product one, which is also what names the tarball
// (installer_build.sh resolves `.installer // .product`). Comparing the
// component version against a release tag would be meaningless.
//
// Stamped into this package rather than main so it does not have to be threaded
// through Run, New, and the model to reach the one line that shows it. "dev"
// means an unstamped build, and no check is made against that.
var ProductVersion = "dev"

// updateFeedURL is the public releases feed. The same place the README and
// services/readme.md already send people to download PAIR, so nothing new is
// being contacted, and it serves stable releases only.
const updateFeedURL = "https://api.github.com/repos/NVIDIA/Personal-AI-Router/releases/latest"

// updateReleasesPage is where the notice sends the operator: the human page,
// not the API. The same URL the README and services/readme.md already give.
const updateReleasesPage = "https://github.com/NVIDIA/Personal-AI-Router/releases"

// disableUpdateCheckEnv turns the check off.
//
// A server reaching the internet unasked is a legitimate objection — an
// air-gapped or change-controlled host must be able to refuse — so this is one
// documented variable rather than a setting to discover.
const disableUpdateCheckEnv = "NVPAIR_NO_UPDATE_CHECK"

const (
	// updateCheckTimeout bounds the request. Short: nothing waits on this, and a
	// network that blackholes the request should cost nothing.
	updateCheckTimeout = 10 * time.Second

	// updateCheckInterval matches the desktop app's six hours, so the two front
	// ends notice a release at the same cadence.
	updateCheckInterval = 6 * time.Hour

	// updateFeedMaxBytes caps the reply. The release object is a few kilobytes;
	// this is only here so a misbehaving endpoint cannot stream forever.
	updateFeedMaxBytes = 1 << 20
)

// updateClient refuses redirects for the same reason the telemetry poller does:
// a redirect could point this at an arbitrary host, and a release feed has no
// legitimate reason to issue one.
var updateClient = &http.Client{
	Timeout: updateCheckTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// updateCheckMsg carries the outcome. A failure is recorded and never shown: an
// operator who cannot reach the internet does not need to be told so every six
// hours, and this is the least important thing on the screen.
type updateCheckMsg struct {
	latest string
	err    error
}

// updateCheckEnabled reports whether to look at all.
//
// An unstamped build is skipped too: a developer running from source has nothing
// to compare and does not want a notice telling them to download a release.
func updateCheckEnabled() bool {
	if strings.TrimSpace(os.Getenv(disableUpdateCheckEnv)) != "" {
		return false
	}
	_, stamped := versionParts(ProductVersion)
	return stamped
}

// checkUpdateCmd asks the feed for the newest release.
func checkUpdateCmd() tea.Cmd {
	if !updateCheckEnabled() {
		return nil
	}
	return func() tea.Msg {
		latest, err := fetchLatestRelease(updateFeedURL)
		return updateCheckMsg{latest: latest, err: err}
	}
}

// updateCheckTickCmd re-arms the check.
func updateCheckTickCmd() tea.Cmd {
	if !updateCheckEnabled() {
		return nil
	}
	return tea.Tick(updateCheckInterval, func(time.Time) tea.Msg {
		return updateCheckDueMsg{}
	})
}

type updateCheckDueMsg struct{}

// fetchLatestRelease returns the newest release's version, without its leading v.
func fetchLatestRelease(url string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	// The documented media type for this endpoint, so a future default change
	// cannot alter the shape being parsed.
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := updateClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// A repository with no published release answers 404, which is not an error
	// worth distinguishing: there is nothing newer either way.
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("release feed returned %s", resp.Status)
	}

	var r struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, updateFeedMaxBytes)).Decode(&r); err != nil {
		return "", err
	}
	// Stable and prerelease metadata stay isolated, the same rule the desktop
	// feed follows: a suffix-free build must never be offered a prerelease.
	if r.Draft || r.Prerelease {
		return "", nil
	}
	return strings.TrimPrefix(strings.TrimSpace(r.TagName), "v"), nil
}

// newerVersion reports whether latest is a higher release than running.
//
// Compares the numeric dot-separated head and ignores any prerelease suffix, so
// 0.91.7 beats 0.91.6 and 0.91.7-dev is not offered to 0.91.7. Anything it
// cannot parse answers false: a wrong "up to date" is a missed notice, while a
// wrong "update available" sends someone looking for a release that is not there.
func newerVersion(running, latest string) bool {
	r, okR := versionParts(running)
	l, okL := versionParts(latest)
	if !okR || !okL {
		return false
	}
	for i := 0; i < len(r) || i < len(l); i++ {
		var a, b int
		if i < len(r) {
			a = r[i]
		}
		if i < len(l) {
			b = l[i]
		}
		if a != b {
			return b > a
		}
	}
	return false
}

// versionParts splits a version's numeric head into components.
func versionParts(v string) ([]int, bool) {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "v"))
	if v == "" {
		return nil, false
	}
	// Drop a prerelease or build suffix; only the release numbers are compared.
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	fields := strings.Split(v, ".")
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}
