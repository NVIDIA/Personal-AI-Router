// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package main — portsource.go is the port-candidate layer underneath the
// engine lifecycle: which loopback ports are worth asking, what asking them
// learned, and how to say what was found.
//
// It exists because a fixed configured port is not always where an engine is.
// An engine the user started themselves listens wherever their own app keeps
// it, which is often not PAIR's default, and it is the only copy that will ever
// answer there. Both jobs the lifecycle has — deciding whether to adopt what is
// already running, and deciding whether it is safe to start another copy — need
// the same round of asking, though they read different answers off it, so the
// round and what it learns live here.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// maxPortFileBytes bounds the settings-file read that looks up an engine's own
// port. A real one is a few kilobytes; the cap keeps a pathological file, or a
// symlink to something enormous, from being pulled into memory on every poll.
const maxPortFileBytes = 64 << 10

// neverSendsKey is the half of every credential-refusal message that explains
// why a refusal is the right answer rather than a workaround PAIR could apply.
const neverSendsKey = "NVIDIA PAIR never sends an API key, so it can neither use that service nor start a second copy beside it."

// portObservation is what one round of asking an engine's candidate ports
// learned about whatever is listening there. Each fact names the port it was
// found on instead of only saying that it was found somewhere, because the ports
// a round covers are not equal: one is the port PAIR would bind, and the rest
// are only places the engine might keep its real port.
type portObservation struct {
	// bind is the port a copy of PAIR's own would use: the one the caller came
	// to check, which candidatePorts lists first. A refusal that finds no
	// credential refusal names this one, because it is the port the user was
	// told to look at.
	bind int
	// identified is a candidate that answered exactly as this engine's manifest
	// says its engine answers. It is the service to adopt, on that port.
	identified int
	// blocked is a candidate that refused PAIR for credentials: HTTP 401, which
	// means it wants an API key, or HTTP 403, which means it refuses requests
	// that carry none. It is still a live service, and PAIR can never become one
	// with it, so neither adoption nor a second copy is correct.
	blocked int
	// blockedStatus is the 401 or 403 behind blocked.
	blockedStatus int
	// stranger is a candidate that took connections and answered the probe as
	// something that is not this engine.
	stranger int
	// unsure names a candidate that could not be read at all — a dial that timed
	// out or failed for a reason other than refusal. That proves nothing about
	// whether anything listens, so it is never read as an empty port.
	unsure int
	// quiet says every candidate refused the connection: nothing is listening.
	quiet bool
}

// answered says the round found a live service that is not this engine.
func (o portObservation) answered() bool { return o.blocked != 0 || o.stranger != 0 }

// occupied says the round heard something back anywhere: the engine answering as
// itself, a stranger, a credential refusal, or a port it could not read. Both of
// the callers that ask this question are about to act on the assumption that
// nothing is there — delete the engine's files, or call its service gone — and
// neither can tell which of the ports it asked was the one it needed, so none of
// them counts as empty. Whether a port stands in the way of a second copy is the
// narrower question, and spawnBlocked answers it.
func (o portObservation) occupied() bool {
	return o.identified != 0 || o.answered() || o.unsure != 0
}

// spawnBlocked says a Start or Install must not bring up a second copy.
//
// Only the port that copy would bind is in its way. PAIR launches an engine on a
// port of its own — MTPLX is handed --port — so a stranger found while searching
// for the engine's real port is nowhere near the port a new copy needs, and
// refusing over it left the user with no engine at all because of a service they
// never asked PAIR about.
//
// A credential refusal blocks from any port, because that one is this engine, set
// up in a way PAIR cannot work with. A second copy beside it would be the
// duplicate router the user already has, on a model that is already served.
func (o portObservation) spawnBlocked() bool {
	if o.blocked != 0 {
		return true
	}
	return o.bind != 0 && (o.stranger == o.bind || o.unsure == o.bind)
}

// servicePort names the port a service that is not this engine was found on, for
// a message that has to point at a listener that is really there rather than at
// the port PAIR happened to ask about.
func (o portObservation) servicePort() int {
	switch {
	case o.stranger != 0:
		return o.stranger
	case o.unsure != 0:
		return o.unsure
	}
	return o.bind
}

// credentialRefusalClause states a credential refusal as a clause about a
// service that has already been named.
func credentialRefusalClause(status int) string {
	switch status {
	case 401:
		return "it requires an API key"
	case 403:
		return "it refuses requests that carry no credentials"
	}
	return "it will not answer NVIDIA PAIR's health check"
}

// blockedAnswer states the refusal as a clause about the service, for a
// sentence that has already named it.
func (o portObservation) blockedAnswer() string { return credentialRefusalClause(o.blockedStatus) }

// startRefusal is the whole reason a Start is refused: what was found on the
// port and what the user can do about it. The two shapes say different things,
// which is why they are not one sentence. A stranger on the port is somebody
// else's service and worth leaving alone; a credential refusal is this engine,
// set up in a way PAIR cannot work with, and telling the user to look elsewhere
// for a service that "did not identify as MTPLX" sends them hunting for a
// different engine while the right one is already running.
func (o portObservation) startRefusal(engine, displayName string) string {
	if o.blocked != 0 {
		return fmt.Sprintf("cannot start engine %q: %s %s Start %s without an API key, or choose a port nothing is listening on",
			engine, o.credentialSummary(), neverSendsKey, displayName)
	}
	return fmt.Sprintf("cannot start engine %q on port %d: the port is occupied by a service that did not identify as %s",
		engine, o.bind, displayName)
}

// installRefusal is the same for an Install, which additionally has to say that
// fetching a second copy of the engine is off the table.
func (o portObservation) installRefusal(engine, displayName string) string {
	if o.blocked != 0 {
		return fmt.Sprintf("cannot install engine %q: %s %s Install %s on a port nothing is listening on, or start it without an API key",
			engine, o.credentialSummary(), neverSendsKey, displayName)
	}
	return fmt.Sprintf("cannot install engine %q for port %d: the port is occupied by a service that did not identify as %s; use the existing service or choose another port",
		engine, o.bind, displayName)
}

// credentialSummary names the refusing port and its status, in a sentence that
// stands on its own before the advice.
func (o portObservation) credentialSummary() string {
	return fmt.Sprintf("The service already listening on port %d answers NVIDIA PAIR's health check with HTTP %d because %s.",
		o.blocked, o.blockedStatus, o.blockedAnswer())
}

// authBlockedNotice is the standing version of the same fact, for the error
// feed while a port stays in this state. It is reported once per episode, not
// once per poll: the feed holds one entry per id, and refreshing one re-raises
// a dialog the user has already dismissed.
func (o portObservation) authBlockedNotice(displayName string) string {
	return fmt.Sprintf("%s on port %d answers NVIDIA PAIR's health check with HTTP %d because %s. %s Start it without an API key, or choose a port nothing is listening on",
		displayName, o.blocked, o.blockedStatus, o.blockedAnswer(), neverSendsKey)
}

// candidatePorts lists the loopback ports worth probing for one engine, in
// probe order: the port being watched, then the port the engine's own settings
// file names when that differs.
//
// The settings file is what lets an engine the user started themselves be found
// on whatever port their own app chose, which is the only port it will ever
// answer on. It is read fresh each round, so a port the user changes in their
// own app takes effect on the next poll without restarting PAIR. An engine whose
// manifest declares no port source gets exactly the one port it probed before
// any of this existed.
func (e *Executor) candidatePorts(st *engineState, watched int) []int {
	// plat is fixed when the engine is registered, so no lock guards it.
	source := st.plat.Runtime.PortSource

	var ports []int
	add := func(port int) {
		if port > 0 && port <= 65535 && !slices.Contains(ports, port) {
			ports = append(ports, port)
		}
	}
	add(watched)
	if source != nil {
		if port, ok := settingsPort(source, st.pathVars()); ok {
			add(port)
		}
	}
	return ports
}

// settingsPort reads the port an engine keeps in its own settings file, from
// the path and key its manifest declares. A missing file, an unreadable one,
// and a key holding no usable port all mean "this engine tells us nothing",
// which is the ordinary case for every engine that serves on a fixed port.
func settingsPort(source *PortSource, vars map[string]string) (int, bool) {
	path, err := resolvePlaceholders(source.File, vars)
	if err != nil || strings.TrimSpace(path) == "" {
		return 0, false
	}
	file, err := os.Open(expandPath(path))
	if err != nil {
		return 0, false
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxPortFileBytes))
	if err != nil || len(body) == 0 {
		return 0, false
	}

	field := source.Field
	if field == "" {
		field = "port"
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return 0, false
	}
	value, ok := resolveObjectPath(obj, field)
	if !ok {
		return 0, false
	}
	// A port is normally a JSON number, but launchers have been known to write
	// the same value as a string and the engine reading it accepts both.
	// Neither form may be a reason to miss a server that is really there.
	var port int
	if err := json.Unmarshal(value, &port); err == nil {
		return port, port > 0 && port <= 65535
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return 0, false
	}
	port, err = strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		return 0, false
	}
	return port, port > 0 && port <= 65535
}

// probePortObservation asks each candidate port, cheapest check first: a
// transport dial before an HTTP request. It stops at the first port that
// identifies, because one identified service is all an engine needs and every
// later port would only be asked the same question. Each answer is filed under
// the port it came from: whether one port counts against an operation depends on
// which port it is, and a refusal has to say where the service it names is.
func (e *Executor) probePortObservation(ctx context.Context, ready *Probe, ports []int) portObservation {
	obs := portObservation{quiet: len(ports) > 0}
	if len(ports) > 0 {
		obs.bind = ports[0]
	}
	for _, port := range ports {
		switch probeListener(ctx, ready, port) {
		case listenerProbeRefused:
			continue
		case listenerProbeIndeterminate:
			obs.quiet = false
			if obs.unsure == 0 {
				obs.unsure = port
			}
			continue
		}
		// The port takes connections, so what it answers says what is behind it.
		obs.quiet = false
		answer := e.probeIdentity(ctx, ready, port)
		switch {
		case answer.outcome == probeIdentified:
			obs.identified = port
			return obs
		case answer.blocked():
			if obs.blocked == 0 {
				obs.blocked = port
				obs.blockedStatus = answer.status
			}
		default:
			if obs.stranger == 0 {
				obs.stranger = port
			}
		}
	}
	return obs
}

// confirmPortsQuiet re-probes ports until every one of them refuses three times
// in a row. That is the evidence PAIR demands before it says a service it was
// watching has stopped — both when a stop command has just run against one port
// and when a passive status round finds nothing answering. A service that moved
// to another port still answers here, so it is not reported gone, and another
// engine's listener is never read as this one leaving.
func (e *Executor) confirmPortsQuiet(parent context.Context, ready *Probe, ports []int, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	misses := 0
	for {
		if allPortsRefused(ctx, ready, ports) {
			misses++
			if misses == unavailableConfirmations {
				return true
			}
		} else {
			// A reachable listener, a timeout, or a malformed probe target
			// cannot prove shutdown. Require a fresh consecutive sequence.
			misses = 0
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// allPortsRefused reports whether every port explicitly refused a connection.
func allPortsRefused(ctx context.Context, ready *Probe, ports []int) bool {
	for _, port := range ports {
		if probeListener(ctx, ready, port) != listenerProbeRefused {
			return false
		}
	}
	return len(ports) > 0
}
