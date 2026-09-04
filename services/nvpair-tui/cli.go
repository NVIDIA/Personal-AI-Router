// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// The non-interactive half of nvpair-tui. `nvpair-tui <subcommand>` does not
// start a broker or a UI: it connects to the control socket of a TUI that is
// already running on this machine and drives that TUI's pairing, so an
// operator on a headless box can pair from a script or a single SSH command
// instead of pressing keys in the Cluster tab.
//
// Everything here is presentation and exit codes. The pairing itself lives in
// nvpair-tui/pairing, on the other side of the socket, which is the same code
// the Cluster tab drives.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"nvpair-tui/control"
	"nvpair-tui/pairing"
)

// Exit codes. They are a contract: a script keys on them, so they are
// documented in the component README and in docs/terminal-interface.mdx.
const (
	// exitOK — the pairing reached the state the caller asked for.
	exitOK = 0
	// exitRefused — the other side said no: a wrong PIN, or a declined
	// invite. The command worked; the answer was negative.
	exitRefused = 1
	// exitFailure — anything else: no running TUI, a bad argument, an
	// unreachable peer, an expired invite, a transport failure.
	exitFailure = 2
)

// pinEnvVar is the scripted way to supply a PIN. A PIN passed as --pin lands
// in shell history and in every `ps` listing on the machine; the environment
// form keeps it out of both, and is the one the documentation recommends.
const pinEnvVar = "NVPAIR_PIN"

// waitPoll is how often a --wait loop re-asks. nvpair-cluster-manager expires
// an unanswered invite after five minutes, so nothing here needs to be brisk.
const waitPoll = 2 * time.Second

// inviteWaitCap bounds `invite --wait`. The invite's own TTL terminates the
// wait long before this; the cap only guarantees the command cannot hang
// forever if the manager stops answering.
const inviteWaitCap = 15 * time.Minute

// dialTimeout bounds one control-socket request. It sits above the endpoint's
// own relay budget so a slow pairing surfaces as the manager's answer rather
// than as a timeout here.
const dialTimeout = 40 * time.Second

// subcommands are the verbs that skip the UI entirely.
var subcommands = map[string]func(*cliEnv, []string) int{
	"invite":  cmdInvite,
	"pending": cmdPending,
	"accept":  cmdAccept,
	"decline": cmdDecline,
	"members": cmdMembers,
}

// controlClient is the part of control.Client the subcommands use, so their
// argument handling, output and exit codes are testable without a socket.
type controlClient interface {
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
	Close() error
}

// cliEnv is everything a subcommand touches outside itself.
type cliEnv struct {
	out    io.Writer
	errOut io.Writer
	getenv func(string) string
	now    func() time.Time
	// poll is how long a --wait loop sleeps between asks. A test shortens it
	// so the loop's logic can be exercised without the wall clock.
	poll time.Duration
	// dial opens a control connection. The default resolves the endpoint the
	// running TUI listens on; a test substitutes its own.
	dial func(path string) (controlClient, error)
}

func newCLIEnv(out, errOut io.Writer) *cliEnv {
	return &cliEnv{
		out:    out,
		errOut: errOut,
		getenv: os.Getenv,
		now:    time.Now,
		poll:   waitPoll,
		dial: func(path string) (controlClient, error) {
			return control.Dial(path)
		},
	}
}

// subcommandName returns the verb in args, or "" when args carry none. A
// leading "-" is a flag, which means the caller wants the interactive UI.
func subcommandName(args []string) string {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return ""
	}
	if _, ok := subcommands[args[0]]; ok {
		return args[0]
	}
	return args[0] // an unknown verb, reported by run
}

// runSubcommand executes the verb in args and returns the process exit code.
// It must only be called when subcommandName(args) is non-empty.
func runSubcommand(env *cliEnv, args []string) int {
	name := args[0]
	cmd, ok := subcommands[name]
	if !ok {
		fmt.Fprintf(env.errOut, "nvpair-tui: unknown command %q\n\n", name)
		printUsage(env.errOut)
		return exitFailure
	}
	return cmd(env, args[1:])
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `nvpair-tui runs the terminal interface. With a command, it instead drives the
pairing of an nvpair-tui already running on this machine, over its control socket.

  nvpair-tui                                  start the terminal interface
  nvpair-tui invite <address> [--port N] [--wait]
  nvpair-tui pending
  nvpair-tui accept --pin 123456 [--invite <id>] [--wait 2m]
  nvpair-tui decline [--invite <id>]
  nvpair-tui members

Every command takes --json to print the raw result, and --control-socket <path>
to reach a terminal interface started with a non-default endpoint.

Read the PIN from %s instead of --pin in a script: an argument is visible to
every process on the machine and is kept in shell history.

Exit codes: 0 the pairing reached the asked-for state; 1 the other side refused
(wrong PIN, or a declined invite); 2 anything else.
`, pinEnvVar)
}

// commonFlags are the two flags every subcommand shares.
type commonFlags struct {
	json   *bool
	socket *string
}

func registerCommon(fs *flag.FlagSet) commonFlags {
	return commonFlags{
		json:   fs.Bool("json", false, "print the raw JSON result instead of a human-readable line"),
		socket: fs.String("control-socket", "", "control socket of the running terminal interface (default: the per-user path)"),
	}
}

// parseArgs parses flags that may appear before, between or after positional
// arguments — Go's flag package stops at the first non-flag, so
// `invite 10.0.0.5 --port 14321` needs the parse resumed past each positional.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
}

// connect resolves the endpoint and opens a control connection to the running
// terminal interface.
func (env *cliEnv) connect(socket string) (controlClient, error) {
	path := socket
	if path == "" {
		resolved, err := control.DefaultPath()
		if err != nil {
			return nil, err
		}
		path = resolved
	}
	return env.dial(path)
}

// fail reports err and picks the exit code. A control endpoint nobody is
// listening on is the one failure worth explaining rather than just naming.
func (env *cliEnv) fail(err error) int {
	if errors.Is(err, control.ErrNotRunning) {
		fmt.Fprintf(env.errOut, "nvpair-tui: %v\n", err)
		fmt.Fprintln(env.errOut, "Start the terminal interface on this machine first (nvpair-tui, ideally inside tmux), then run this command again.")
		return exitFailure
	}
	fmt.Fprintf(env.errOut, "nvpair-tui: %v\n", err)
	return exitFailure
}

// callWithClient opens a connection, hands it to fn, and closes it.
func (env *cliEnv) callWithClient(socket string, fn func(context.Context, controlClient) int) int {
	client, err := env.connect(socket)
	if err != nil {
		return env.fail(err)
	}
	defer func() { _ = client.Close() }()
	return fn(context.Background(), client)
}

// emit prints the raw result when --json was asked for, and otherwise runs the
// human renderer. It always returns code so a caller can `return env.emit(...)`.
func (env *cliEnv) emit(asJSON bool, raw json.RawMessage, human func(), code int) int {
	if asJSON {
		fmt.Fprintln(env.out, strings.TrimSpace(string(raw)))
		return code
	}
	human()
	return code
}

// --- invite -----------------------------------------------------------------

func cmdInvite(env *cliEnv, args []string) int {
	fs := flag.NewFlagSet("invite", flag.ContinueOnError)
	fs.SetOutput(env.errOut)
	common := registerCommon(fs)
	port := fs.Int("port", 0, "pairing port on the target (default: the cluster manager's own, 14321)")
	wait := fs.Bool("wait", false, "block until the pairing is accepted, refused, or expires")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return exitFailure
	}
	if len(positional) != 1 {
		fmt.Fprintln(env.errOut, "nvpair-tui invite: exactly one address is required")
		fmt.Fprintln(env.errOut, "usage: nvpair-tui invite <address> [--port N] [--wait] [--json]")
		return exitFailure
	}
	address := positional[0]

	return env.callWithClient(*common.socket, func(ctx context.Context, client controlClient) int {
		params := map[string]any{"address": address}
		if *port != 0 {
			params["port"] = *port
		}
		callCtx, cancel := context.WithTimeout(ctx, dialTimeout)
		raw, err := client.Call(callCtx, "pair:invite", params)
		cancel()
		if err != nil {
			return env.fail(err)
		}
		var invite pairing.Invite
		if err := json.Unmarshal(raw, &invite); err != nil {
			return env.fail(fmt.Errorf("decode the invite: %w", err))
		}

		if invite.State == pairing.StateRejected {
			return env.emit(*common.json, raw, func() {
				fmt.Fprintf(env.errOut, "%s refused the invitation (%s). Remove the existing relationship on that machine first.\n",
					address, reasonText(invite.Reason))
			}, exitFailure)
		}
		if invite.PIN() == "" {
			return env.emit(*common.json, raw, func() {
				fmt.Fprintf(env.errOut, "the invitation to %s did not start (state %s%s)\n",
					address, invite.State, reasonSuffix(invite.Reason))
			}, exitFailure)
		}

		if !*wait {
			return env.emit(*common.json, raw, func() {
				// The PIN goes to the terminal the operator asked for, and
				// nowhere else. It is never logged.
				fmt.Fprintf(env.out, "PIN %s  invite %s  to %s\n", invite.PIN(), invite.InviteID, address)
				fmt.Fprintln(env.out, "Read the PIN to whoever is at that machine; they run: nvpair-tui accept --pin "+invite.PIN())
			}, exitOK)
		}

		fmt.Fprintf(env.out, "PIN %s  invite %s  to %s\n", invite.PIN(), invite.InviteID, address)
		fmt.Fprintln(env.out, "Waiting for the other machine to accept...")
		final, err := env.awaitInvite(ctx, client, invite.InviteID)
		if err != nil {
			return env.fail(err)
		}
		return env.reportOutcome(*common.json, final, "the invitation")
	})
}

// awaitInvite polls one invite until it leaves the pending state.
func (env *cliEnv) awaitInvite(ctx context.Context, client controlClient, inviteID string) (rawInvite, error) {
	deadline := env.now().Add(inviteWaitCap)
	for {
		callCtx, cancel := context.WithTimeout(ctx, dialTimeout)
		raw, err := client.Call(callCtx, "pair:invite-status", map[string]any{"inviteId": inviteID})
		cancel()
		if err != nil {
			return rawInvite{}, err
		}
		var invite pairing.Invite
		if err := json.Unmarshal(raw, &invite); err != nil {
			return rawInvite{}, fmt.Errorf("decode the invite status: %w", err)
		}
		if invite.Terminal() {
			return rawInvite{raw: raw, invite: invite}, nil
		}
		if !env.now().Before(deadline) {
			return rawInvite{}, fmt.Errorf("invite %s was still pending after %s", inviteID, inviteWaitCap)
		}
		select {
		case <-ctx.Done():
			return rawInvite{}, ctx.Err()
		case <-time.After(env.poll):
		}
	}
}

// rawInvite pairs the endpoint's verbatim answer with its decode.
type rawInvite struct {
	raw    json.RawMessage
	invite pairing.Invite
}

// reportOutcome renders a terminal invite and maps its state to an exit code.
// subject names what reached that state, for the human line.
func (env *cliEnv) reportOutcome(asJSON bool, res rawInvite, subject string) int {
	inv := res.invite
	code := exitFailure
	switch {
	case inv.State == pairing.StatePaired:
		code = exitOK
	case inv.State == pairing.StateDeclined:
		code = exitRefused
	case inv.State == pairing.StateFailed && inv.Reason == pairing.ReasonIncorrectPin:
		code = exitRefused
	}
	return env.emit(asJSON, res.raw, func() {
		line := fmt.Sprintf("%s: %s%s", subject, inv.State, reasonSuffix(inv.Reason))
		if code == exitOK {
			fmt.Fprintln(env.out, line)
			return
		}
		fmt.Fprintln(env.errOut, line)
	}, code)
}

// --- pending ----------------------------------------------------------------

func cmdPending(env *cliEnv, args []string) int {
	fs := flag.NewFlagSet("pending", flag.ContinueOnError)
	fs.SetOutput(env.errOut)
	common := registerCommon(fs)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return exitFailure
	}
	if len(positional) != 0 {
		fmt.Fprintln(env.errOut, "nvpair-tui pending: takes no arguments")
		return exitFailure
	}

	return env.callWithClient(*common.socket, func(ctx context.Context, client controlClient) int {
		callCtx, cancel := context.WithTimeout(ctx, dialTimeout)
		raw, err := client.Call(callCtx, "pair:pending", nil)
		cancel()
		if err != nil {
			return env.fail(err)
		}
		var result struct {
			Invites []pairing.Invite `json:"invites"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return env.fail(fmt.Errorf("decode the pending invites: %w", err))
		}
		return env.emit(*common.json, raw, func() {
			if len(result.Invites) == 0 {
				fmt.Fprintln(env.out, "No invitations are waiting for an answer on this machine.")
				return
			}
			// The invite itself carries no address, so the roster is asked
			// for one. It is a convenience: a lookup that fails just leaves
			// the address off the line.
			addresses := env.inviterAddresses(ctx, client)
			for _, inv := range result.Invites {
				from := inv.FromNodeName
				if from == "" {
					from = inv.FromNodeID
				}
				if addr, ok := addresses[inv.FromNodeUUID]; ok && addr != "" {
					from += " (" + addr + ")"
				}
				fmt.Fprintf(env.out, "%s  from %s  %s ago\n", inv.InviteID, from, env.since(inv))
			}
		}, exitOK)
	})
}

// inviterAddresses maps node uuid to the address the roster last saw it at,
// for the nodes whose pairing is still in flight.
func (env *cliEnv) inviterAddresses(ctx context.Context, client controlClient) map[string]string {
	callCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	raw, err := client.Call(callCtx, "pair:members", nil)
	if err != nil {
		return nil
	}
	var membership pairing.Membership
	if err := json.Unmarshal(raw, &membership); err != nil {
		return nil
	}
	out := make(map[string]string, len(membership.Members))
	for _, node := range membership.Members {
		if node.NodeUUID != "" && node.IPAddress != "" {
			out[node.NodeUUID] = node.IPAddress
		}
	}
	return out
}

// since renders how long an invite has been waiting, from this node's own
// clock. The inviter's createdAt is on a machine whose clock may differ, so it
// is only the fallback.
func (env *cliEnv) since(inv pairing.Invite) string {
	stamp := inv.ReceivedAt
	if stamp == 0 {
		stamp = inv.CreatedAt
	}
	if stamp == 0 {
		return "unknown"
	}
	age := env.now().Sub(time.UnixMilli(stamp))
	if age < 0 {
		age = 0
	}
	return age.Truncate(time.Second).String()
}

// --- accept / decline -------------------------------------------------------

func cmdAccept(env *cliEnv, args []string) int {
	fs := flag.NewFlagSet("accept", flag.ContinueOnError)
	fs.SetOutput(env.errOut)
	common := registerCommon(fs)
	pin := fs.String("pin", "", "the six-digit PIN shown on the inviting machine (prefer "+pinEnvVar+" in a script)")
	invite := fs.String("invite", "", "invite id to answer (default: the one that is pending)")
	wait := fs.Duration("wait", 0, "if nothing is pending yet, wait this long for an invitation to arrive (e.g. 2m)")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return exitFailure
	}
	if len(positional) != 0 {
		fmt.Fprintf(env.errOut, "nvpair-tui accept: unexpected argument %q\n", positional[0])
		return exitFailure
	}

	resolvedPin := *pin
	if resolvedPin == "" {
		resolvedPin = strings.TrimSpace(env.getenv(pinEnvVar))
	}
	if resolvedPin == "" {
		fmt.Fprintf(env.errOut, "nvpair-tui accept: a PIN is required; pass --pin, or set %s (which keeps it out of shell history and ps)\n", pinEnvVar)
		return exitFailure
	}

	return env.callWithClient(*common.socket, func(ctx context.Context, client controlClient) int {
		inviteID := *invite
		if inviteID == "" && *wait > 0 {
			resolved, code := env.awaitPending(ctx, client, *wait)
			if code != exitOK {
				return code
			}
			inviteID = resolved
		}
		return env.respond(ctx, client, *common.json, inviteID, true, resolvedPin)
	})
}

func cmdDecline(env *cliEnv, args []string) int {
	fs := flag.NewFlagSet("decline", flag.ContinueOnError)
	fs.SetOutput(env.errOut)
	common := registerCommon(fs)
	invite := fs.String("invite", "", "invite id to decline (default: the one that is pending)")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return exitFailure
	}
	if len(positional) != 0 {
		fmt.Fprintf(env.errOut, "nvpair-tui decline: unexpected argument %q\n", positional[0])
		return exitFailure
	}
	return env.callWithClient(*common.socket, func(ctx context.Context, client controlClient) int {
		return env.respond(ctx, client, *common.json, *invite, false, "")
	})
}

// respond answers one invite and maps the manager's verdict to an exit code.
func (env *cliEnv) respond(ctx context.Context, client controlClient, asJSON bool, inviteID string, accept bool, pin string) int {
	params := map[string]any{"accept": accept}
	if inviteID != "" {
		params["inviteId"] = inviteID
	}
	if accept {
		params["pin"] = pin
	}
	callCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	raw, err := client.Call(callCtx, "pair:respond", params)
	cancel()
	if err != nil {
		return env.fail(err)
	}
	var invite pairing.Invite
	if err := json.Unmarshal(raw, &invite); err != nil {
		return env.fail(fmt.Errorf("decode the response: %w", err))
	}
	subject := "the invitation"
	if !accept {
		if invite.State == pairing.StateDeclined {
			return env.emit(asJSON, raw, func() {
				fmt.Fprintln(env.out, "declined the invitation")
			}, exitOK)
		}
		return env.emit(asJSON, raw, func() {
			fmt.Fprintf(env.errOut, "%s: %s%s\n", subject, invite.State, reasonSuffix(invite.Reason))
		}, exitFailure)
	}
	if invite.State == pairing.StatePaired {
		return env.emit(asJSON, raw, func() {
			fmt.Fprintf(env.out, "paired with %s\n", inviterName(invite))
		}, exitOK)
	}
	return env.reportOutcome(asJSON, rawInvite{raw: raw, invite: invite}, subject)
}

// awaitPending blocks until an invitation is waiting, returning its id. It is
// what `accept --wait <duration>` uses so an operator can arm the accept
// before the other machine has sent anything.
func (env *cliEnv) awaitPending(ctx context.Context, client controlClient, budget time.Duration) (string, int) {
	deadline := env.now().Add(budget)
	for {
		callCtx, cancel := context.WithTimeout(ctx, dialTimeout)
		raw, err := client.Call(callCtx, "pair:pending", nil)
		cancel()
		if err != nil {
			return "", env.fail(err)
		}
		var result struct {
			Invites []pairing.Invite `json:"invites"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return "", env.fail(fmt.Errorf("decode the pending invites: %w", err))
		}
		switch len(result.Invites) {
		case 0:
			// keep waiting
		case 1:
			return result.Invites[0].InviteID, exitOK
		default:
			// Let the endpoint produce the message that names them all, so
			// the wording lives in one place.
			return "", exitOK
		}
		if !env.now().Before(deadline) {
			fmt.Fprintf(env.errOut, "nvpair-tui accept: no invitation arrived within %s\n", budget)
			return "", exitFailure
		}
		select {
		case <-ctx.Done():
			return "", env.fail(ctx.Err())
		case <-time.After(env.poll):
		}
	}
}

// --- members ----------------------------------------------------------------

func cmdMembers(env *cliEnv, args []string) int {
	fs := flag.NewFlagSet("members", flag.ContinueOnError)
	fs.SetOutput(env.errOut)
	common := registerCommon(fs)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return exitFailure
	}
	if len(positional) != 0 {
		fmt.Fprintln(env.errOut, "nvpair-tui members: takes no arguments")
		return exitFailure
	}
	return env.callWithClient(*common.socket, func(ctx context.Context, client controlClient) int {
		callCtx, cancel := context.WithTimeout(ctx, dialTimeout)
		raw, err := client.Call(callCtx, "pair:members", nil)
		cancel()
		if err != nil {
			return env.fail(err)
		}
		var membership pairing.Membership
		if err := json.Unmarshal(raw, &membership); err != nil {
			return env.fail(fmt.Errorf("decode the membership: %w", err))
		}
		return env.emit(*common.json, raw, func() {
			cluster := membership.ClusterID
			if cluster == "" {
				cluster = "(none - this machine is not in a cluster)"
			} else if membership.ClusterFriendlyName != "" {
				cluster += "  " + membership.ClusterFriendlyName
			}
			fmt.Fprintf(env.out, "cluster %s\n", cluster)
			if len(membership.Members) == 0 {
				fmt.Fprintln(env.out, "no members")
				return
			}
			members := append([]pairing.ClusterNode(nil), membership.Members...)
			sort.SliceStable(members, func(i, j int) bool { return members[i].ID < members[j].ID })
			for _, node := range members {
				fmt.Fprintf(env.out, "%s  %s  %s:%d  %s\n", node.ID, node.Name, node.IPAddress, node.Port, node.State)
			}
		}, exitOK)
	})
}

// --- shared rendering -------------------------------------------------------

func inviterName(inv pairing.Invite) string {
	switch {
	case inv.FromNodeName != "":
		return inv.FromNodeName
	case inv.FromNodeID != "":
		return inv.FromNodeID
	default:
		return "the inviting machine"
	}
}

// reasonText turns a machine-readable reason into something an operator reads.
func reasonText(reason string) string {
	switch reason {
	case "":
		return "no reason given"
	case "already-clustered":
		return "it is already in a cluster"
	case pairing.ReasonIncorrectPin:
		return "the PIN was wrong"
	default:
		return reason
	}
}

func reasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return " (" + reasonText(reason) + ")"
}
