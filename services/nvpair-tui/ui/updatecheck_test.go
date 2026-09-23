// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// withReleaseVersion stamps a version for one test and restores it after.
func withReleaseVersion(t *testing.T, v string) {
	t.Helper()
	prev := ReleaseVersion
	ReleaseVersion = v
	t.Cleanup(func() { ReleaseVersion = prev })
}

func TestNewerVersionComparesReleaseNumbers(t *testing.T) {
	cases := []struct {
		running, latest string
		want            bool
		why             string
	}{
		{"0.91.7", "0.91.8", true, "patch bump"},
		{"0.91.7", "0.92.0", true, "minor bump"},
		{"0.91.7", "1.0.0", true, "major bump"},
		{"0.91.7", "0.91.7", false, "same version"},
		{"0.91.8", "0.91.7", false, "feed behind this build"},
		// Numeric, not lexical: "10" sorts before "9" as a string.
		{"0.9.0", "0.10.0", true, "double-digit minor beats single"},
		{"0.10.0", "0.9.0", false, "and not the other way"},
		// Prerelease isolation, the same rule the desktop feed follows: a
		// suffix-free build is a stable release and must not be offered a
		// prerelease of the version it is already on.
		{"0.91.7", "0.91.7-dev", false, "prerelease of the running version"},
		{"0.91.7-dev", "0.91.7", false, "the release this prerelease became"},
		{"0.91.7", "0.91.8-rc1", true, "prerelease of a genuinely later version"},
		// A leading v is how the tag is written.
		{"0.91.7", "v0.91.8", true, "tag keeps its v"},
		// Unparseable answers false. A missed notice is harmless; a false one
		// sends someone looking for a release that does not exist.
		{"0.91.7", "", false, "empty feed answer"},
		{"0.91.7", "nightly", false, "non-numeric tag"},
		{"dev", "0.91.8", false, "unstamped build"},
		{"", "0.91.8", false, "empty running version"},
		// Differing component counts compare as if the shorter were zero-padded.
		{"1.2", "1.2.1", true, "shorter running version"},
		{"1.2.0", "1.2", false, "shorter latest version"},
	}
	for _, tc := range cases {
		if got := newerVersion(tc.running, tc.latest); got != tc.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v (%s)",
				tc.running, tc.latest, got, tc.want, tc.why)
		}
	}
}

func TestUpdateCheckIsDisabledByEnvironment(t *testing.T) {
	// A server reaching the internet unasked is a real objection, so the opt-out
	// has to actually stop the request being built at all.
	withReleaseVersion(t, "0.91.7")
	if !updateCheckEnabled() {
		t.Fatal("check is disabled with nothing set")
	}

	t.Setenv(disableUpdateCheckEnv, "1")
	if updateCheckEnabled() {
		t.Error("check still enabled with the opt-out set")
	}
	if checkUpdateCmd() != nil {
		t.Error("a command was still issued with the opt-out set")
	}
	if updateCheckTickCmd() != nil {
		t.Error("the check was still re-armed with the opt-out set")
	}
}

func TestUpdateCheckIsSkippedForAnUnstampedBuild(t *testing.T) {
	// Running from source has nothing to compare, and a developer does not want
	// to be told to go download a release.
	withReleaseVersion(t, "dev")
	if updateCheckEnabled() {
		t.Error("check enabled for an unstamped build")
	}
	if checkUpdateCmd() != nil {
		t.Error("a command was issued for an unstamped build")
	}
}

func TestFetchLatestReleaseReadsTheTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept header = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v0.92.0","draft":false,"prerelease":false}`))
	}))
	defer srv.Close()

	got, err := fetchLatestRelease(srv.URL)
	if err != nil {
		t.Fatalf("fetchLatestRelease: %v", err)
	}
	if got != "0.92.0" {
		t.Errorf("got %q, want the tag without its v", got)
	}
}

func TestFetchLatestReleaseIgnoresDraftsAndPrereleases(t *testing.T) {
	for _, body := range []string{
		`{"tag_name":"v0.92.0","draft":true,"prerelease":false}`,
		`{"tag_name":"v0.92.0","draft":false,"prerelease":true}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		got, err := fetchLatestRelease(srv.URL)
		srv.Close()
		if err != nil {
			t.Fatalf("fetchLatestRelease: %v", err)
		}
		if got != "" {
			t.Errorf("body %s offered %q; drafts and prereleases must be ignored", body, got)
		}
	}
}

func TestFetchLatestReleaseHandlesNoReleases(t *testing.T) {
	// A repository with nothing published answers 404. There is nothing newer,
	// and nothing to report.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := fetchLatestRelease(srv.URL); err == nil {
		t.Error("a 404 was treated as a successful answer")
	}
}

func TestFetchLatestReleaseRefusesRedirects(t *testing.T) {
	// A redirect could point this at an arbitrary host, and a release feed has
	// no legitimate reason to issue one.
	var reached bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9"}`))
	}))
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()

	got, _ := fetchLatestRelease(srv.URL)
	if reached {
		t.Error("the redirect was followed")
	}
	if got == "9.9.9" {
		t.Error("a redirected body was accepted")
	}
}

// send drives one message through the shell and hands the model back.
func send(m Model, msg tea.Msg) Model {
	next, _ := m.Update(msg)
	updated, _ := next.(Model)
	return updated
}

func TestBannerAnnouncesOnlyANewerRelease(t *testing.T) {
	withReleaseVersion(t, "0.91.7")

	m := newTestModel(defaultViews(nil)...)
	m.width, m.height = 120, 30

	if got := m.banner(); got != "" {
		t.Errorf("a banner appeared before any check: %q", got)
	}

	// Neither an equal nor an older release is an update.
	m = send(m, updateCheckMsg{latest: "0.91.7"})
	m = send(m, updateCheckMsg{latest: "0.90.0"})
	if got := m.banner(); got != "" {
		t.Errorf("announced %q for a release that is not newer", got)
	}

	// A failure is dropped rather than shown.
	m = send(m, updateCheckMsg{err: errStub{}})
	if got := m.banner(); got != "" {
		t.Errorf("a failed check produced %q; it should be silent", got)
	}

	// A newer one names both versions, where to get it, and how to dismiss it.
	m = send(m, updateCheckMsg{latest: "0.92.0"})
	banner := m.banner()
	for _, want := range []string{"0.92.0", "0.91.7", updateReleasesPage, "ctrl+x"} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner %q does not mention %q", banner, want)
		}
	}
}

func TestBannerShowsOnEveryTabAndDismissesEverywhere(t *testing.T) {
	// The whole point of moving it out of the Service tab: the operator it is
	// for is the one who never opens that tab.
	withReleaseVersion(t, "0.91.7")

	m := newTestModel(defaultViews(nil)...)
	m.width, m.height = 120, 30
	m = send(m, updateCheckMsg{latest: "0.92.0"})

	for i := range m.views {
		m.selectTab(i)
		if !strings.Contains(m.View(), "0.92.0") {
			t.Errorf("tab %d (%s) does not show the notice", i+1, m.views[i].Title())
		}
	}

	// Dismissing from one tab clears it on all of them, and it stays gone.
	m.selectTab(1)
	m = send(m, tea.KeyMsg{Type: tea.KeyCtrlX})
	for i := range m.views {
		m.selectTab(i)
		if strings.Contains(m.View(), "0.92.0") {
			t.Errorf("tab %d (%s) still shows the notice after dismissal", i+1, m.views[i].Title())
		}
	}

	// A repeat of the same release does not bring it back.
	m = send(m, updateCheckMsg{latest: "0.92.0"})
	if got := m.banner(); got != "" {
		t.Errorf("the dismissed release came back: %q", got)
	}

	// A newer one does, because that is not what was acknowledged.
	m = send(m, updateCheckMsg{latest: "0.93.0"})
	if !strings.Contains(m.banner(), "0.93.0") {
		t.Error("a release newer than the dismissed one was suppressed")
	}
}

func TestBannerKeepsTheDismissHintAtEveryWidth(t *testing.T) {
	// The hint is the rightmost thing on the row, and the frame is clamped to
	// the terminal — so an over-long banner loses exactly the one key that
	// closes it, leaving a notice the operator cannot get rid of. The full
	// sentence stops fitting at 118 columns, well inside the widths people use.
	//
	// banner() assembles longest-first for that reason; this sweep is what stops
	// a later, tidier single-line version from quietly putting it back.
	//
	// A stub view because the row depends on the width and the versions alone.
	withReleaseVersion(t, "0.91.7")

	const hint = "ctrl+x to dismiss"
	for w := minTerminalWidth; w <= 200; w++ {
		m := newTestModel(&stubView{title: "T", rows: 1})
		m.width = w
		m = send(m, updateCheckMsg{latest: "0.92.0"})

		// Clamped the way View() clamps the frame, since that is what truncates.
		row := lipgloss.NewStyle().MaxWidth(w).Render(m.banner())
		if !strings.Contains(row, hint) {
			t.Fatalf("width %d: %q does not keep %q", w, row, hint)
		}
	}
}

func TestBannerComesOutOfTheContentBudget(t *testing.T) {
	// A row added to the frame without coming out of the budget is a row the
	// shell deletes from the bottom of the active view — and the bottom is where
	// every view keeps its messages.
	withReleaseVersion(t, "0.91.7")

	m := newTestModel(defaultViews(nil)...)
	m.width, m.height = 120, 30

	before := m.contentHeight()
	m = send(m, updateCheckMsg{latest: "0.92.0"})
	after := m.contentHeight()

	if after != before-1 {
		t.Errorf("budget went from %d to %d; the banner's row was not taken from it",
			before, after)
	}

	m = send(m, tea.KeyMsg{Type: tea.KeyCtrlX})
	if got := m.contentHeight(); got != before {
		t.Errorf("budget is %d after dismissal, want the original %d", got, before)
	}
}
