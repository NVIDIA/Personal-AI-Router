// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// {cli} names the engine's own command line tool, and it is the one placeholder
// whose value depends on how the engine reached the machine. A manifest can only
// write that path as a guess: MTPLX's names the private copy its own installer
// makes, while the same tool arrives in Homebrew's bin directory from a package
// install, inside the downloaded app, or in whatever prefix another user's install
// chose. Resolving it from the manifest literal therefore breaks every route that
// was not the one the manifest imagined, and it breaks the route the manifest did
// imagine as soon as a second one is present and shadows it in the detect list.
//
// So {cli} follows what is actually on disk, exactly as {bin} already does, and
// the literal is the fallback for a manifest that declares no detect paths and so
// has nothing to have found. The three places that run a command and the two that
// display or validate one all resolve it from the same observation, so a custom
// launch string is checked against the command that would really be run.

// engineCLI resolves {cli} from an observation. `found` is the engine's command as
// it exists on this machine; when nothing was found, the manifest's literal is
// used, which is also how a manifest with no detect paths reaches its tool.
func engineCLI(rt Runtime, found string) string {
	if found != "" {
		return found
	}
	if rt.CLI == "" {
		return ""
	}
	return expandPath(rt.CLI)
}

// observedEngineCLI answers {cli} for one operation, from the engine's detect
// paths. It re-reads them instead of taking the engine's cached binary because
// that may have been filled by a port probe with the executable of whatever was
// found serving the engine's port. {cli} is a command this service runs as itself,
// so it has to be the engine's own tool, from a path the manifest lists.
func observedEngineCLI(st *engineState) string {
	_, found := detectPath(st)
	return engineCLI(st.plat.Runtime, found)
}
