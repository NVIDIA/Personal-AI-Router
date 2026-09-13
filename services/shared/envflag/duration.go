// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package envflag registers command-line flags whose default comes from an
// NVPAIR_* environment variable, so a value can be set on a headless install
// (pair-start.sh, a systemd unit) or on the desktop without a flag change.
package envflag

import (
	"flag"
	"fmt"
	"os"
	"time"
)

// RegisterDuration registers a --<name> duration flag on the given FlagSet
// (or the default one when fs == nil) and returns a resolver that, when called
// after flag.Parse, returns the effective value using this precedence:
//
//	CLI flag (if set) > <envVar> env var > fallback
//
// Values use Go duration syntax ("2m30s", "0"). An unparseable or negative
// value from either source is an error naming its origin, so a typo in a unit
// file is reported rather than silently replaced by the fallback.
func RegisterDuration(fs *flag.FlagSet, name, envVar string, fallback time.Duration, usage string) func() (time.Duration, error) {
	if fs == nil {
		fs = flag.CommandLine
	}
	val := fs.String(name, "", fmt.Sprintf("%s (default: $%s or %s)", usage, envVar, fallback))
	return func() (time.Duration, error) {
		if *val != "" {
			return parse("--"+name, *val)
		}
		if env := os.Getenv(envVar); env != "" {
			return parse("$"+envVar, env)
		}
		return fallback, nil
	}
}

func parse(origin, raw string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration (use e.g. \"30m\", \"90s\" or \"0\")", origin, raw)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s: %q must not be negative", origin, raw)
	}
	return d, nil
}
