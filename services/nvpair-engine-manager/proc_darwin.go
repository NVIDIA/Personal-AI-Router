// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
)

// procImage resolves a PID's executable path from what the kernel mapped, not
// from what the process's caller wrote: lsof's txt entries list the mapped
// executables, and the first is always the one that was exec'd (the dynamic
// linker and shared libraries follow). Unlike ps — which echoes argv[0] as it
// was last written and so can be told to name any file — this reports the
// vnode the kernel opened at exec time, so a foreign listener cannot present
// itself as our managed binary. -Fn asks for just the path fields, one per
// line, so paths with spaces need no column parsing. macOS answers in
// canonical form (/private/var/...); isOurEngineImage folds the symlink
// spellings together. An exited PID, a process we may not introspect, or a
// lookup that resolves nothing all answer "" and the caller fails closed.
func procImage(pid int) string {
	if pid <= 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), portLookupTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-Fn").Output()
	if err != nil && len(out) == 0 {
		return ""
	}
	return parseLsofExecName(string(out))
}

// parseLsofExecName returns the first n-field of lsof -F output — the path of
// the first txt entry, which is the executed binary. It answers "" for output
// with no name fields, keeping an unresolved lookup on the fail-closed path.
func parseLsofExecName(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		if name, ok := strings.CutPrefix(line, "n"); ok && name != "" {
			return name
		}
	}
	return ""
}

// procRunsScript reports whether pid was started as an interpreter running the
// engine script at binPath. The honest answer needs the process's own command
// line, and macOS exposes only views of it that echo what the process's
// caller wrote, which the identity gate must not trust. So an engine
// installed as a script — vLLM's venv/bin/vllm — is not recognised here:
// Stop declines it and says so, exactly as before macOS image lookups
// existed. Fail closed, like every other unresolvable identity.
func procRunsScript(pid int, binPath string) bool {
	return false
}
