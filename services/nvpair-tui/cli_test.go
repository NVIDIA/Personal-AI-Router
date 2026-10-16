// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"nvpair-tui/control"
)

const cliTestPIN = "402199"

// fakeEndpoint stands in for a running TUI's control socket, recording what
// each subcommand asked for and answering with whatever the test scripted.
type fakeEndpoint struct {
	mu      sync.Mutex
	calls   []endpointCall
	answers map[string][]answer
	dialErr error
}

type endpointCall struct {
	method string
	params map[string]any
}

type answer struct {
	result string
	err    error
}

func newFakeEndpoint() *fakeEndpoint {
	return &fakeEndpoint{answers: map[string][]answer{}}
}

// on queues one answer for method. Queued answers are consumed in order, so a
// polling loop can be given a sequence; the last one repeats.
func (f *fakeEndpoint) on(method, result string) *fakeEndpoint {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[method] = append(f.answers[method], answer{result: result})
	return f
}

func (f *fakeEndpoint) onError(method string, err error) *fakeEndpoint {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[method] = append(f.answers[method], answer{err: err})
	return f
}

func (f *fakeEndpoint) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	recorded := endpointCall{method: method}
	if m, ok := params.(map[string]any); ok {
		recorded.params = m
	}
	f.calls = append(f.calls, recorded)

	queued := f.answers[method]
	if len(queued) == 0 {
		return nil, fmt.Errorf("no scripted answer for %q", method)
	}
	next := queued[0]
	if len(queued) > 1 {
		f.answers[method] = queued[1:]
	}
	if next.err != nil {
		return nil, next.err
	}
	return json.RawMessage(next.result), nil
}

func (f *fakeEndpoint) Close() error { return nil }

func (f *fakeEndpoint) paramsFor(t *testing.T, method string) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.method == method {
			return c.params
		}
	}
	t.Fatalf("%s was never called; calls = %+v", method, f.calls)
	return nil
}

func (f *fakeEndpoint) countOf(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.method == method {
			n++
		}
	}
	return n
}

// run executes one subcommand against the fake endpoint and reports its exit
// code together with what it wrote.
type runResult struct {
	code   int
	stdout string
	stderr string
}

func (r runResult) all() string { return r.stdout + r.stderr }

func run(t *testing.T, endpoint *fakeEndpoint, env map[string]string, args ...string) runResult {
	t.Helper()
	var out, errOut bytes.Buffer
	// A fixed clock keeps the age column and every --wait deadline
	// deterministic; the poll loops still advance it through fakeClock.
	clock := &fakeClock{now: time.UnixMilli(1716998460000)}
	cli := &cliEnv{
		out:    &out,
		errOut: &errOut,
		getenv: func(k string) string { return env[k] },
		now:    clock.Now,
		poll:   time.Millisecond,
		dial: func(string) (controlClient, error) {
			if endpoint.dialErr != nil {
				return nil, endpoint.dialErr
			}
			return endpoint, nil
		},
	}
	code := runSubcommand(cli, args)
	return runResult{code: code, stdout: out.String(), stderr: errOut.String()}
}

// fakeClock advances a little on every read, so a polling loop bounded by a
// deadline terminates without the test sleeping.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Second)
	return c.now
}

func TestSubcommandNameOnlyMatchesAVerb(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{nil, ""},
		{[]string{}, ""},
		{[]string{"--version"}, ""},
		{[]string{"-log-level", "debug"}, ""},
		{[]string{"--control-socket", "/tmp/x.sock"}, ""},
		{[]string{"invite", "10.0.0.5"}, "invite"},
		{[]string{"members"}, "members"},
		{[]string{"bogus"}, "bogus"},
	}
	for _, tc := range tests {
		if got := subcommandName(tc.args); got != tc.want {
			t.Errorf("subcommandName(%v) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestParseArgsAcceptsFlagsAroundThePositional(t *testing.T) {
	for _, args := range [][]string{
		{"10.0.0.5", "--port", "14399", "--json"},
		{"--port", "14399", "10.0.0.5", "--json"},
		{"--json", "--port", "14399", "10.0.0.5"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			port := fs.Int("port", 0, "")
			asJSON := fs.Bool("json", false, "")
			positional, err := parseArgs(fs, args)
			if err != nil {
				t.Fatalf("parseArgs: %v", err)
			}
			if len(positional) != 1 || positional[0] != "10.0.0.5" {
				t.Errorf("positional = %v, want the address", positional)
			}
			if *port != 14399 || !*asJSON {
				t.Errorf("port = %d, json = %v; flags on either side of the positional must all bind", *port, *asJSON)
			}
		})
	}
}

func TestUnknownCommandPrintsUsage(t *testing.T) {
	got := run(t, newFakeEndpoint(), nil, "bogus")
	if got.code != exitFailure {
		t.Errorf("exit = %d, want %d", got.code, exitFailure)
	}
	for _, want := range []string{`unknown command "bogus"`, "nvpair-tui invite", "Exit codes"} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("stderr does not mention %q:\n%s", want, got.stderr)
		}
	}
}

func TestNoRunningInstanceExitsWithAnExplanation(t *testing.T) {
	endpoint := newFakeEndpoint()
	endpoint.dialErr = &control.NotRunningError{Path: "/run/nvpair/tui.sock", Err: fmt.Errorf("connect: no such file or directory")}

	for _, args := range [][]string{{"members"}, {"pending"}, {"invite", "10.0.0.5"}, {"decline"}} {
		t.Run(args[0], func(t *testing.T) {
			got := run(t, endpoint, map[string]string{pinEnvVar: cliTestPIN}, args...)
			if got.code != exitFailure {
				t.Errorf("exit = %d, want %d", got.code, exitFailure)
			}
			if !strings.Contains(got.stderr, "no nvpair-tui is listening") {
				t.Errorf("stderr does not say nothing is listening:\n%s", got.stderr)
			}
			if !strings.Contains(got.stderr, "Start the terminal interface") {
				t.Errorf("stderr does not say what to do about it:\n%s", got.stderr)
			}
		})
	}
}

func TestInvitePrintsThePINOnStdout(t *testing.T) {
	endpoint := newFakeEndpoint().on("pair:invite",
		`{"inviteId":"inv-1","state":"pending","pin":"`+cliTestPIN+`"}`)

	got := run(t, endpoint, nil, "invite", "10.0.0.5", "--port", "14399")
	if got.code != exitOK {
		t.Fatalf("exit = %d (%s)", got.code, got.all())
	}
	if !strings.Contains(got.stdout, cliTestPIN) {
		t.Errorf("the PIN is not on stdout:\n%s", got.stdout)
	}
	if !strings.Contains(got.stdout, "inv-1") {
		t.Errorf("the invite id is not on stdout:\n%s", got.stdout)
	}
	params := endpoint.paramsFor(t, "pair:invite")
	if params["address"] != "10.0.0.5" || params["port"] != 14399 {
		t.Errorf("params = %v, want the address and the port asked for", params)
	}
}

func TestInviteWithoutAPortSendsNone(t *testing.T) {
	endpoint := newFakeEndpoint().on("pair:invite", `{"inviteId":"inv-1","state":"pending","pin":"`+cliTestPIN+`"}`)
	run(t, endpoint, nil, "invite", "gpu-box.tail1234.ts.net")
	params := endpoint.paramsFor(t, "pair:invite")
	if _, ok := params["port"]; ok {
		t.Errorf("params = %v, want no port so the manager appends its own", params)
	}
}

func TestInviteJSONPrintsTheRawResult(t *testing.T) {
	raw := `{"inviteId":"inv-1","state":"pending","pin":"` + cliTestPIN + `","unmodelled":true}`
	endpoint := newFakeEndpoint().on("pair:invite", raw)

	got := run(t, endpoint, nil, "invite", "10.0.0.5", "--json")
	if got.code != exitOK {
		t.Fatalf("exit = %d (%s)", got.code, got.all())
	}
	if strings.TrimSpace(got.stdout) != raw {
		t.Errorf("stdout = %q, want the endpoint's raw result", got.stdout)
	}
}

func TestInviteRequiresExactlyOneAddress(t *testing.T) {
	for _, args := range [][]string{{"invite"}, {"invite", "a", "b"}} {
		got := run(t, newFakeEndpoint(), nil, args...)
		if got.code != exitFailure {
			t.Errorf("%v: exit = %d, want %d", args, got.code, exitFailure)
		}
		if !strings.Contains(got.stderr, "exactly one address") {
			t.Errorf("%v: stderr = %q", args, got.stderr)
		}
	}
}

func TestInviteReportsARejection(t *testing.T) {
	endpoint := newFakeEndpoint().on("pair:invite",
		`{"inviteId":"inv-1","state":"rejected","reason":"already-clustered","pin":null}`)

	got := run(t, endpoint, nil, "invite", "10.0.0.5")
	if got.code != exitFailure {
		t.Errorf("exit = %d, want %d", got.code, exitFailure)
	}
	if !strings.Contains(got.stderr, "already in a cluster") {
		t.Errorf("stderr does not explain the rejection:\n%s", got.stderr)
	}
}

func TestInviteWaitExitsOnTheFinalState(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   int
	}{
		{"paired", `{"inviteId":"inv-1","state":"paired"}`, exitOK},
		{"declined", `{"inviteId":"inv-1","state":"declined"}`, exitRefused},
		{"wrong pin", `{"inviteId":"inv-1","state":"failed","reason":"incorrect-pin"}`, exitRefused},
		{"unreachable", `{"inviteId":"inv-1","state":"failed"}`, exitFailure},
		{"expired", `{"inviteId":"inv-1","state":"expired"}`, exitFailure},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := newFakeEndpoint().
				on("pair:invite", `{"inviteId":"inv-1","state":"pending","pin":"`+cliTestPIN+`"}`).
				on("pair:invite-status", tc.status)

			got := run(t, endpoint, nil, "invite", "10.0.0.5", "--wait")
			if got.code != tc.want {
				t.Errorf("exit = %d, want %d (%s)", got.code, tc.want, got.all())
			}
			if !strings.Contains(got.stdout, cliTestPIN) {
				t.Errorf("--wait must still print the PIN so it can be read out:\n%s", got.stdout)
			}
		})
	}
}

// TestInviteWaitJSONPrintsOnlyJSON checks that --json means machine-readable
// output all the way through: two documents, one per line, and no prose
// mixed in for a parser to trip over.
func TestInviteWaitJSONPrintsOnlyJSON(t *testing.T) {
	created := `{"inviteId":"inv-1","state":"pending","pin":"` + cliTestPIN + `"}`
	final := `{"inviteId":"inv-1","state":"paired"}`
	endpoint := newFakeEndpoint().on("pair:invite", created).on("pair:invite-status", final)

	got := run(t, endpoint, nil, "invite", "10.0.0.5", "--wait", "--json")
	if got.code != exitOK {
		t.Fatalf("exit = %d (%s)", got.code, got.all())
	}
	lines := strings.Split(strings.TrimSpace(got.stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout = %q, want the created invite and its final state, one per line", got.stdout)
	}
	if lines[0] != created {
		t.Errorf("first line = %q, want the invite as created (it carries the PIN)", lines[0])
	}
	if lines[1] != final {
		t.Errorf("second line = %q, want the final invite", lines[1])
	}
	for _, line := range lines {
		var probe map[string]any
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			t.Errorf("line %q is not JSON: %v", line, err)
		}
	}
	if got.stderr != "" {
		t.Errorf("stderr = %q, want nothing on a successful --json run", got.stderr)
	}
}

func TestInviteWaitPollsUntilTheInviteLeavesPending(t *testing.T) {
	endpoint := newFakeEndpoint().
		on("pair:invite", `{"inviteId":"inv-1","state":"pending","pin":"`+cliTestPIN+`"}`).
		on("pair:invite-status", `{"inviteId":"inv-1","state":"pending"}`).
		on("pair:invite-status", `{"inviteId":"inv-1","state":"pending"}`).
		on("pair:invite-status", `{"inviteId":"inv-1","state":"paired"}`)

	got := run(t, endpoint, nil, "invite", "10.0.0.5", "--wait")
	if got.code != exitOK {
		t.Fatalf("exit = %d (%s)", got.code, got.all())
	}
	if n := endpoint.countOf("pair:invite-status"); n != 3 {
		t.Errorf("polled %d times, want it to keep asking until the invite settled", n)
	}
}

func TestAcceptExitCodes(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     int
	}{
		{"paired", `{"inviteId":"inv-1","state":"paired","fromNodeName":"Lab desk A"}`, exitOK},
		{"wrong pin", `{"inviteId":"inv-1","state":"failed","reason":"incorrect-pin"}`, exitRefused},
		{"expired", `{"inviteId":"inv-1","state":"expired"}`, exitFailure},
		{"other failure", `{"inviteId":"inv-1","state":"failed"}`, exitFailure},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := newFakeEndpoint().on("pair:respond", tc.response)
			got := run(t, endpoint, nil, "accept", "--pin", cliTestPIN)
			if got.code != tc.want {
				t.Errorf("exit = %d, want %d (%s)", got.code, tc.want, got.all())
			}
		})
	}
}

func TestAcceptReadsThePINFromTheEnvironment(t *testing.T) {
	endpoint := newFakeEndpoint().on("pair:respond", `{"inviteId":"inv-1","state":"paired"}`)
	got := run(t, endpoint, map[string]string{pinEnvVar: " " + cliTestPIN + " "}, "accept")
	if got.code != exitOK {
		t.Fatalf("exit = %d (%s)", got.code, got.all())
	}
	params := endpoint.paramsFor(t, "pair:respond")
	if params["pin"] != cliTestPIN {
		t.Errorf("pin = %v, want the environment value trimmed and forwarded", params["pin"])
	}
	if params["accept"] != true {
		t.Errorf("accept = %v, want true", params["accept"])
	}
}

func TestAcceptWithoutAPINSaysWhereToPutOne(t *testing.T) {
	got := run(t, newFakeEndpoint(), nil, "accept")
	if got.code != exitFailure {
		t.Errorf("exit = %d, want %d", got.code, exitFailure)
	}
	if !strings.Contains(got.stderr, pinEnvVar) {
		t.Errorf("stderr does not point at %s:\n%s", pinEnvVar, got.stderr)
	}
}

func TestAcceptPassesAnExplicitInviteID(t *testing.T) {
	endpoint := newFakeEndpoint().on("pair:respond", `{"inviteId":"inv-2","state":"paired"}`)
	run(t, endpoint, nil, "accept", "--pin", cliTestPIN, "--invite", "inv-2")
	if got := endpoint.paramsFor(t, "pair:respond")["inviteId"]; got != "inv-2" {
		t.Errorf("inviteId = %v, want the one named", got)
	}
}

func TestAcceptOmitsTheInviteIDSoTheEndpointResolvesIt(t *testing.T) {
	endpoint := newFakeEndpoint().on("pair:respond", `{"inviteId":"inv-1","state":"paired"}`)
	run(t, endpoint, nil, "accept", "--pin", cliTestPIN)
	if _, ok := endpoint.paramsFor(t, "pair:respond")["inviteId"]; ok {
		t.Error("an unnamed invite must be resolved by the endpoint, not guessed here")
	}
}

func TestAcceptSurfacesTheAmbiguousInviteError(t *testing.T) {
	endpoint := newFakeEndpoint().onError("pair:respond", &control.Error{
		Code:    control.CodeAmbiguousInvite,
		Message: "2 invites are pending; name one with --invite: inv-1 (from Lab desk A), inv-2 (from Lab desk B)",
	})
	got := run(t, endpoint, nil, "accept", "--pin", cliTestPIN)
	if got.code != exitFailure {
		t.Errorf("exit = %d, want %d", got.code, exitFailure)
	}
	for _, want := range []string{"inv-1", "inv-2", "--invite"} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("stderr does not mention %q:\n%s", want, got.stderr)
		}
	}
}

func TestAcceptWaitsForAnInviteToArrive(t *testing.T) {
	endpoint := newFakeEndpoint().
		on("pair:pending", `{"invites":[]}`).
		on("pair:pending", `{"invites":[]}`).
		on("pair:pending", `{"invites":[{"inviteId":"inv-late","fromNodeName":"Lab desk A","state":"pending"}]}`).
		on("pair:respond", `{"inviteId":"inv-late","state":"paired"}`)

	got := run(t, endpoint, nil, "accept", "--pin", cliTestPIN, "--wait", "2m")
	if got.code != exitOK {
		t.Fatalf("exit = %d (%s)", got.code, got.all())
	}
	if id := endpoint.paramsFor(t, "pair:respond")["inviteId"]; id != "inv-late" {
		t.Errorf("inviteId = %v, want the invite that arrived while waiting", id)
	}
}

func TestAcceptGivesUpWhenNoInviteArrives(t *testing.T) {
	endpoint := newFakeEndpoint().on("pair:pending", `{"invites":[]}`)
	got := run(t, endpoint, nil, "accept", "--pin", cliTestPIN, "--wait", "5s")
	if got.code != exitFailure {
		t.Errorf("exit = %d, want %d", got.code, exitFailure)
	}
	if !strings.Contains(got.stderr, "no invitation arrived") {
		t.Errorf("stderr = %q, want it to say the wait ran out", got.stderr)
	}
	if endpoint.countOf("pair:respond") != 0 {
		t.Error("a timed-out wait must not go on to answer anything")
	}
}

func TestAcceptRejectsAStrayArgument(t *testing.T) {
	got := run(t, newFakeEndpoint(), nil, "accept", "--pin", cliTestPIN, "402199")
	if got.code != exitFailure {
		t.Errorf("exit = %d, want %d", got.code, exitFailure)
	}
	if !strings.Contains(got.stderr, "unexpected argument") {
		t.Errorf("stderr = %q", got.stderr)
	}
}

func TestDeclineExitCodes(t *testing.T) {
	endpoint := newFakeEndpoint().on("pair:respond", `{"inviteId":"inv-1","state":"declined"}`)
	got := run(t, endpoint, nil, "decline")
	if got.code != exitOK {
		t.Fatalf("exit = %d (%s)", got.code, got.all())
	}
	params := endpoint.paramsFor(t, "pair:respond")
	if params["accept"] != false {
		t.Errorf("accept = %v, want false", params["accept"])
	}
	if _, ok := params["pin"]; ok {
		t.Error("a decline sent a pin; there is nothing to prove on a decline")
	}
}

func TestDeclineReportsAnUnexpectedState(t *testing.T) {
	endpoint := newFakeEndpoint().on("pair:respond", `{"inviteId":"inv-1","state":"expired"}`)
	got := run(t, endpoint, nil, "decline")
	if got.code != exitFailure {
		t.Errorf("exit = %d, want %d", got.code, exitFailure)
	}
}

func TestPendingListsInvitesWithTheirAge(t *testing.T) {
	// receivedAt is 30 s before the clock the fake environment starts on.
	endpoint := newFakeEndpoint().
		on("pair:pending", `{"invites":[{"inviteId":"inv-1","fromNodeName":"Lab desk A","fromNodeUuid":"uuid-a","state":"pending","receivedAt":1716998430000}]}`).
		on("pair:members", `{"clusterId":"c","members":[{"id":"NODE-A","nodeUuid":"uuid-a","name":"Lab desk A","ipAddress":"10.0.0.5","port":14321,"state":"pending-inbound"}]}`)

	got := run(t, endpoint, nil, "pending")
	if got.code != exitOK {
		t.Fatalf("exit = %d (%s)", got.code, got.all())
	}
	for _, want := range []string{"inv-1", "Lab desk A", "10.0.0.5", "ago"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("stdout does not show %q:\n%s", want, got.stdout)
		}
	}
}

func TestPendingWithNothingWaiting(t *testing.T) {
	endpoint := newFakeEndpoint().on("pair:pending", `{"invites":[]}`)
	got := run(t, endpoint, nil, "pending")
	if got.code != exitOK {
		t.Fatalf("exit = %d (%s)", got.code, got.all())
	}
	if !strings.Contains(got.stdout, "No invitations") {
		t.Errorf("stdout = %q", got.stdout)
	}
	if endpoint.countOf("pair:members") != 0 {
		t.Error("an empty list must not go looking up addresses")
	}
}

func TestPendingStillListsWhenTheAddressLookupFails(t *testing.T) {
	endpoint := newFakeEndpoint().
		on("pair:pending", `{"invites":[{"inviteId":"inv-1","fromNodeName":"Lab desk A","state":"pending","receivedAt":1716998430000}]}`).
		onError("pair:members", fmt.Errorf("cluster manager unavailable"))

	got := run(t, endpoint, nil, "pending")
	if got.code != exitOK {
		t.Fatalf("exit = %d (%s); the address is a convenience, not a requirement", got.code, got.all())
	}
	if !strings.Contains(got.stdout, "inv-1") {
		t.Errorf("stdout = %q", got.stdout)
	}
}

func TestMembersPrintsTheClusterAndItsRoster(t *testing.T) {
	endpoint := newFakeEndpoint().on("pair:members",
		`{"clusterId":"cluster-xyz","clusterFriendlyName":"Lab 3 desks","nodeId":"NODE-A","members":[`+
			`{"id":"NODE-B","name":"Lab desk B","ipAddress":"10.0.0.5","port":14321,"state":"member"},`+
			`{"id":"NODE-A","name":"Lab desk A","ipAddress":"10.0.0.4","port":14321,"state":"member"}]}`)

	got := run(t, endpoint, nil, "members")
	if got.code != exitOK {
		t.Fatalf("exit = %d (%s)", got.code, got.all())
	}
	for _, want := range []string{"cluster-xyz", "Lab 3 desks", "NODE-A", "NODE-B", "10.0.0.5"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("stdout does not show %q:\n%s", want, got.stdout)
		}
	}
	if strings.Index(got.stdout, "NODE-A") > strings.Index(got.stdout, "NODE-B") {
		t.Errorf("members are not in a stable order:\n%s", got.stdout)
	}
}

func TestMembersOfAnUnclusteredNode(t *testing.T) {
	endpoint := newFakeEndpoint().on("pair:members", `{"clusterId":"","members":[]}`)
	got := run(t, endpoint, nil, "members")
	if got.code != exitOK {
		t.Fatalf("exit = %d (%s)", got.code, got.all())
	}
	if !strings.Contains(got.stdout, "not in a cluster") {
		t.Errorf("stdout = %q", got.stdout)
	}
}

func TestMembersRejectsAnArgument(t *testing.T) {
	got := run(t, newFakeEndpoint(), nil, "members", "extra")
	if got.code != exitFailure {
		t.Errorf("exit = %d, want %d", got.code, exitFailure)
	}
}

// TestNoSubcommandLeaksThePINIntoItsOwnDiagnostics guards the one thing that
// must never travel: every failure path is exercised with a PIN in hand, and
// none of the messages may repeat it back except the deliberate invite line
// that exists to be read out loud.
func TestNoSubcommandLeaksThePINIntoItsOwnDiagnostics(t *testing.T) {
	env := map[string]string{pinEnvVar: cliTestPIN}
	cases := []struct {
		name     string
		endpoint *fakeEndpoint
		args     []string
	}{
		{
			name:     "a wrong PIN",
			endpoint: newFakeEndpoint().on("pair:respond", `{"inviteId":"inv-1","state":"failed","reason":"incorrect-pin"}`),
			args:     []string{"accept"},
		},
		{
			name:     "nothing pending",
			endpoint: newFakeEndpoint().onError("pair:respond", &control.Error{Code: control.CodeNoPendingInvite, Message: "no invite is pending on this node"}),
			args:     []string{"accept"},
		},
		{
			name:     "a transport failure",
			endpoint: newFakeEndpoint().onError("pair:respond", fmt.Errorf("pair:respond: connection closed")),
			args:     []string{"accept"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := run(t, tc.endpoint, env, tc.args...)
			if strings.Contains(got.all(), cliTestPIN) {
				t.Errorf("the PIN was echoed back:\nstdout=%s\nstderr=%s", got.stdout, got.stderr)
			}
		})
	}
}
