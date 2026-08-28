// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package applog

import (
	"flag"
	"log/slog"
)

// RegisterFlag registers a --log-level flag on the given FlagSet (or the
// default one when fs == nil) and returns a resolver that, when called after
// flag.Parse, returns the effective level using this precedence:
//
//	CLI flag (if non-empty) > NVPAIR_LOG_LEVEL env var > fallback
func RegisterFlag(fs *flag.FlagSet, fallback slog.Level) func() slog.Level {
	if fs == nil {
		fs = flag.CommandLine
	}
	val := fs.String("log-level", "", "log level: debug|info|warn|error (default: $NVPAIR_LOG_LEVEL or info)")
	return func() slog.Level {
		if *val != "" {
			if lvl, err := ParseLevel(*val); err == nil {
				return lvl
			}
		}
		return LevelFromEnv(fallback)
	}
}
