// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// packRow is one model pack as the engine's own inventory reports it: the name it
// calls the pack, and the directory that holds it.
//
// Both are carried because neither one alone identifies a pack. One MTPLX answers
// /health with "mtplx-flash-next-optimized-speed" while its own listing calls the
// same pack "Youssofal/Qwen3.8-Flash-Next-MTPLX-Optimized-Speed" and keeps it in a
// directory spelled with a double dash; a second machine caches three packs under a
// different root, with the first machine's root sitting there empty. A name alone
// cannot tell those apart, and a path alone cannot be checked against what a request
// named, so a lookup that keeps only one of them eventually has to guess.
type packRow struct {
	repoID string
	path   string
}

// residencyDeclaration is the join a manifest declared on its loaded_models action,
// or nil when the manifest declares no join at all — which is the same as an engine
// whose packs PAIR cannot identify, not an engine with nothing cached.
//
// It hangs off loaded_models because that is the only action a manifest may declare a
// residency on: ActionResidency.validate refuses it elsewhere, on the grounds that the
// residency question is about whatever is being served.
func residencyDeclaration(st *engineState) *ActionResidency {
	loaded, ok := st.manifest.Actions["loaded_models"]
	if !ok || loaded.Result == nil {
		return nil
	}
	return loaded.Result.Residency
}

// inventoryAnswer is one read of an engine's inventory, kept whole and named by the
// action it came from, so a caller that needs two things out of one listing — the names
// it carries and the one row a running engine has mapped — asks the engine once.
//
// Naming the action is what keeps the reuse honest: an answer is reusable for a join
// only when it is the answer that join was declared against.
type inventoryAnswer struct {
	action string
	raw    json.RawMessage
	// names and namesOK are what one spec found in raw, for a reader that asked for
	// names on its way through. A reader that only has the body leaves them zero.
	names   []string
	namesOK bool
	// read says there is an answer here at all, which is the question a join asks
	// before it reuses a body instead of reading the engine again.
	read bool
}

// packRowLookup is where one action's answer keeps its pack rows: which array holds
// them, which field keys a row, and which field says where that pack sits. It is a
// recipe rather than three loose arguments because two of the three are field names in
// the same answer, and a join that read a row's key out of its path field would fail as
// quietly as it succeeded.
type packRowLookup struct {
	array     string
	keyField  string
	pathField string
}

// packRowLookupFor reads that recipe off the action carrying the rows and the join
// declared on it.
func packRowLookupFor(listing *ActionResult, residency *ActionResidency) packRowLookup {
	keyField := residency.RowKeyField
	if keyField == "" {
		// The rows are keyed on whatever the listing is keyed on, which is why
		// row_key_field is usually not written at all.
		keyField = listing.Field
	}
	return packRowLookup{array: listing.Array, keyField: keyField, pathField: residency.RowPathField}
}

// rowsFrom turns one action's answer into rows, or says it could not.
//
// The size limit is checked on every answer, including one handed over by a caller,
// because the action machinery lets a bigger body through (8 MiB for an HTTP action,
// all of stdout for a cmd one), and a listing that was cut off names some of the packs
// and silently omits the rest — including, possibly, the one being asked about.
func (l packRowLookup) rowsFrom(engine, action string, raw json.RawMessage) ([]packRow, bool) {
	if len(raw) > maxProbeJSONBytes {
		slog.Debug("engine:residency inventory was larger than PAIR reads", "engine", engine, "action", action, "bytes", len(raw))
		return nil, false
	}
	rows, ok := extractPackRows(raw, l.array, l.keyField, l.pathField)
	if !ok {
		slog.Debug("engine:residency inventory was not a readable listing", "engine", engine, "action", action)
	}
	return rows, ok
}

// packInventoryFrom reads what an engine holds on disk, in the engine's terms, reusing an
// answer the caller has already got when it came from the action this join was declared
// against, and reading that action itself otherwise. It is how PAIR finds out which packs
// a machine has; which one a running engine has loaded is found by joining the engine's own
// loaded-path answer onto these rows (see residentJoin), so a model name is not mistaken
// for a model just because they are spelled alike.
//
// The second result separates what an engine's cache holds from what PAIR managed to find
// out, because they lead to different advice. ok=false means the inventory was not read: the
// source action failed, its answer was not a listing, or it was larger than the body PAIR
// reads. ok=true with no rows is the answer "nothing is cached", so the two are never
// collapsed into each other.
//
// Reuse is not a shortcut past a read the engine would rather not do twice: it is one
// question about one fact, and for MTPLX the read is `mtplx models --json` — a subprocess,
// whose cost is decided by how long its output is waited on, rather than a request with a
// bound on it. Rows from any other action are not the rows the join was declared against, so
// a join that names another source gets its own read, and the listing the sweep read is
// still read only once.
//
// The read runs through the action machinery, so the caller's context is what bounds it:
// one probe read for the eject (see ejectableNames), the caller's whole request for the
// model sweep. A command that has stopped answering is ended at that bound — killed if it
// is still running, and given up on one commandDrainTimeout after it exits if it left
// something behind holding its output — so "the read failed" arrives within the time the
// caller set instead of on the schedule of whatever the engine left running. Whether an
// engine answers at all is the source action's business: HTTP actions are gated on a running
// engine and cmd actions deliberately are not (see dispatchAction), which is why MTPLX's
// manifest describes its `mtplx models --json` read as needing no running engine. So "the
// read failed" is not the same statement as "the engine is down" — and the fixture's HTTP
// stand-in answers less than a stopped MTPLX would.
//
// What is this function's business is keeping the lookup a lookup: a residency
// source that also declared stop_engine, restart_after or launch_model would turn a
// read into a stop, a restart or a launch-settings rewrite, so Manifest.Validate
// rejects that manifest at load instead of a runtime branch declining to obey it.
func (e *Executor) packInventoryFrom(ctx context.Context, engine string, already inventoryAnswer) ([]packRow, bool) {
	st, err := e.state(engine)
	if err != nil {
		return nil, false
	}
	residency := residencyDeclaration(st)
	if residency == nil {
		return nil, false
	}
	source, ok := st.manifest.Actions[residency.Source]
	if !ok || source.Result == nil {
		return nil, false
	}
	rows := packRowLookupFor(source.Result, residency)
	if already.read && already.action == residency.Source {
		return rows.rowsFrom(engine, residency.Source, already.raw)
	}

	raw, err := e.Action(ctx, engine, residency.Source, nil)
	if err != nil {
		slog.Debug("engine:residency inventory read failed", "engine", engine, "action", residency.Source)
		return nil, false
	}
	return rows.rowsFrom(engine, residency.Source, raw)
}

// packRowForKey finds the row holding the pack at exactly this path.
//
// The comparison is exact: no case folding, no separator rewriting, no stripping a
// leading /private, no symlink resolution. That is deliberate. MTPLX keys its own
// records by more than one spelling of the same pack — the served name, the repo id,
// and the directory name differ on a single machine — and the two Macs measured keep
// their packs under different roots, one of them with the other's root present and
// empty. Normalising a path here would merge rows the engine itself keeps apart, on
// a filesystem that already folds case and would let the wrong one read as found.
func packRowForKey(rows []packRow, path string) (packRow, bool) {
	for _, row := range rows {
		if row.path == path {
			return row, true
		}
	}
	return packRow{}, false
}

// residentPackFrom answers which pack a running engine has mapped, as the inventory row
// the engine itself gives for it, for a caller that has already read the inventory the join
// joins against. The answer is reused only when it came from the action the join names;
// anything else, including no answer at all, reads that action once.
//
// The false this returns is residentJoin's "not named", which covers both "that pack is not
// the resident one" and "PAIR could not ask". A caller that only needs a name can take that
// at face value; a caller about to act on the difference, like an eject weighing whether to
// refuse, asks residentJoin, which keeps the two apart. Everything the join reads, and what
// each read costs, is on residentJoin.
func (e *Executor) residentPackFrom(ctx context.Context, engine string, inventory inventoryAnswer) (packRow, bool) {
	answer := e.residentJoin(ctx, engine, inventory)
	return answer.row, answer.named
}

// residencyAnswer is one residency join's finding, keeping apart the two things a
// caller cannot tell from a pack row alone.
//
// "No row" is not one answer. It is what PAIR says when the engine told it where the
// model was loaded from and the cache listing held no row for that directory — a real
// fact about a real pack, which rules the listing's own packs out — and it is also what
// PAIR says when it never got an answer at all, which rules nothing out. Callers that
// only want a name can take (row, named) from residentPackFrom; a caller about to act on
// the difference, like an eject that is weighing refusing, has to look at complete.
type residencyAnswer struct {
	// row is the cached pack the engine says it has mapped, when it named one this
	// machine has a row for.
	row packRow
	// named says row is that pack: the engine answered, the listing answered, and a
	// row in it holds the directory in the answer.
	named bool
	// complete says this is all PAIR could learn from this engine about what it has
	// mapped: either both halves of the join answered, or the manifest declares no
	// residency read to answer with. It is false only where there was a read to make
	// and no answer came — the one case in which "that pack is not the resident one"
	// is a statement PAIR has no evidence for.
	complete bool
}

// residentJoin answers which pack a running engine has mapped, as the inventory row the
// engine itself gives for it, keeping apart the cases a caller cannot tell from a row alone.
//
// It is a join rather than a read, because the two halves answer different questions in
// different spellings and only one of them needs a running server: packInventoryFrom reads
// what the cache holds, which for MTPLX is a CLI read that works with the engine stopped,
// and the residency read says which directory the running server loaded. Where those agree —
// one directory, string for string — PAIR has a pack it can name, locate and act on. Where
// they do not, it has one or the other and has to say so rather than choose.
//
// The row is empty for every case where PAIR does not know, and that is never the answer
// "nothing is resident". Nothing here could mean that: an engine that serves exactly one
// pack has to be running to have one, and each empty case — no engine running, no read
// declared to ask it by, an answer that never arrived or was larger than PAIR reads, an
// answer from something that is not the engine, a directory no row holds — leaves what is
// loaded genuinely open. Reading the empty row as "nothing is resident" would move a pack
// from loaded to not loaded on the strength of a request that failed, which is the same
// mistake as naming the wrong row, travelling the other way. Those cases are not alike in
// what they rule out, and complete is what keeps them apart: see residencyAnswer, and
// residentPackFrom for a caller that only wants a name.
//
// Nothing here is cached: each half is read once per call, which makes a call two reads
// rather than the one its signature suggests. They are not alike in what they cost, and the
// difference is what a caller has to plan around. The first is the probe read — 3 s, against
// a server that is answering — for where the model was loaded from. The second is the
// inventory that answer joins against: it runs through the action machinery, so the bound on
// it is the caller's own rather than the 3 s, and for MTPLX it is a `mtplx models --json`
// subprocess rather than a request. That is why the join takes an inventory answer it did not
// read itself: the model sweep has that listing in hand for its rows, and one sweep should
// run the CLI once. A caller that wants the row together with the served names it pairs with
// is paying three reads, not the two this join makes: asked once per row of a listing, this
// starts one CLI run per row. Re-reading is correct because an engine changes what it has
// mapped only across a load or an eject — both of which PAIR drives itself. Correct is not
// the same as cheap, and a caller that cannot spend that on a name — an eject, which is a
// click — bounds the join itself. See ejectableNames.
func (e *Executor) residentJoin(ctx context.Context, engine string, inventory inventoryAnswer) residencyAnswer {
	st, err := e.state(engine)
	if err != nil {
		return residencyAnswer{}
	}
	residency := residencyDeclaration(st)
	if residency == nil {
		// Nothing is declared to ask about what is mapped, so there is nothing here
		// left unanswered: the answer is complete, and empty.
		return residencyAnswer{complete: true}
	}
	if residency.Read == nil {
		// The manifest has not said where this engine answers where its model lives,
		// so it cannot be asked, and there is no second place worth guessing at. An
		// engine that carried the path inside its loaded_models answer would need
		// that declared, not inferred from whatever that answer happens to hold.
		slog.Debug("engine:residency no loaded-path read declared", "engine", engine)
		return residencyAnswer{complete: true}
	}
	st.mu.Lock()
	running, port := st.running, st.port
	st.mu.Unlock()
	if !running {
		// A clean unknown, not an error, and nothing asked. The join's second half
		// needs a server; the first half is readable right now, which is exactly why
		// the answer cannot be read off it — rows that exist on disk say nothing
		// about which of them a stopped process has mapped. Nothing was asked, so
		// nothing is ruled out.
		return residencyAnswer{}
	}

	// The loaded path is asked first: one bounded GET against the server that is
	// answering, while the inventory may be a CLI run. Finding out that an engine
	// says nothing about its own model should not cost a subprocess.
	loadedPath, ok := e.loadedPackPath(ctx, engine, port, residency)
	if !ok {
		return residencyAnswer{}
	}
	rows, ok := e.packInventoryFrom(ctx, engine, inventory)
	if !ok {
		return residencyAnswer{}
	}
	// Both halves have answered from here, so what follows is evidence.
	row, found := packRowForKey(rows, loadedPath)
	if !found {
		// The engine has a directory mapped that its own listing does not name: a
		// pack handed over as a pair bundle, a cache that moved, a pack deleted from
		// under a server still serving it. That is a real served thing, and no row
		// here may stand in for it — the pack this machine does cache, shown as
		// loaded, is how an eject or a delete ends up aimed at the wrong directory.
		slog.Debug("engine:residency loaded path matched no cached pack", "engine", engine, "rows", len(rows))
		return residencyAnswer{complete: true}
	}
	return residencyAnswer{row: row, named: true, complete: true}
}

// loadedPackPath asks a running engine which directory it loaded its model from, at
// the answer its manifest named (ActionResidency.Read) and in the field it named
// there (PathField).
//
// The read is the one every probe answer is read under — 3 s bound, the
// maxProbeJSONBytes cap, drain-and-close on every path — because this is a health
// check being asked one more question, and two bounds on the same request are two
// chances to disagree about whether an engine is answering. The cap is why a body
// bigger than PAIR reads answers unknown rather than half-parsed: readProbeBody
// returns no body at all for an answer it could not read whole, so there is nothing
// here to mistake for a partial answer.
//
// No status code is consulted, deliberately: the field is the evidence, and an answer
// that names a loaded path is an engine talking about its model whatever it arrived
// with. An error answer that names none is unknown like every other non-answer.
func (e *Executor) loadedPackPath(ctx context.Context, engine string, port int, residency *ActionResidency) (string, bool) {
	path, err := resolvePlaceholders(residency.Read.Path, map[string]string{"port": strconv.Itoa(port)})
	if err != nil {
		slog.Debug("engine:residency loaded-path read names an unresolvable path", "engine", engine, "path", residency.Read.Path)
		return "", false
	}
	requestURL := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	pctx, cancel := context.WithTimeout(ctx, probeReadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, strings.ToUpper(residency.Read.Method), requestURL, nil)
	if err != nil {
		return "", false
	}
	// The probe marker, so PAIR's own facade answers this itself instead of
	// forwarding a residency read through to whatever it fronts.
	req.Header.Set(engineIdentityProbeHeader, "1")
	resp, err := e.client.Do(req)
	if err != nil {
		slog.Debug("engine:residency loaded-path read failed", "engine", engine, "path", path)
		return "", false
	}
	body := readProbeBody(resp.Body)
	if body == "" {
		// Either nothing arrived or more arrived than PAIR reads; readProbeBody
		// answers both with no body, and the join does not care which it was.
		slog.Debug("engine:residency loaded-path answer was empty or larger than PAIR reads", "engine", engine, "path", path)
		return "", false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		slog.Debug("engine:residency loaded-path answer was not a JSON object", "engine", engine, "path", path, "bytes", len(body))
		return "", false
	}
	value, ok := resolveObjectPath(obj, residency.PathField)
	if !ok {
		slog.Debug("engine:residency loaded-path answer named no loaded path", "engine", engine, "path", path, "field", residency.PathField)
		return "", false
	}
	var loaded string
	if err := json.Unmarshal(value, &loaded); err != nil || loaded == "" {
		slog.Debug("engine:residency loaded-path answer held no path PAIR can read", "engine", engine, "path", path, "field", residency.PathField)
		return "", false
	}
	return loaded, true
}

// servedNames answers what an engine answers requests as, in the names a request
// would use: for MTPLX "mtplx-flash-next-optimized-speed", where its own inventory
// calls the same pack Youssofal/Qwen3.8-Flash-Next-MTPLX-Optimized-Speed.
//
// It is the read the model list already makes, offered next to residentJoin for a
// caller that needs both halves of one answer: the row is what a cached pack's eject
// or delete acts on, and the served name is what a request has to be routed by, so
// neither can be derived from the other and a caller showing both has to have both.
// ok is false when the engine did not answer or answered with something that is not
// a listing — which, like an empty residentJoin, is not "it serves nothing".
func (e *Executor) servedNames(ctx context.Context, engine string) ([]string, bool) {
	st, err := e.state(engine)
	if err != nil {
		return nil, false
	}
	loaded, ok := st.manifest.Actions["loaded_models"]
	if !ok || loaded.Result == nil {
		return nil, false
	}
	return e.queryModelList(ctx, engine, "loaded_models", loaded.Result)
}
