// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package envflag

import (
	"flag"
	"strings"
	"testing"
	"time"
)

const testEnv = "NVPAIR_TEST_ENVFLAG_DURATION"

func resolve(t *testing.T, args []string, env string) (time.Duration, error) {
	t.Helper()
	if env != "" {
		t.Setenv(testEnv, env)
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	get := RegisterDuration(fs, "wait", testEnv, 5*time.Minute, "how long to wait")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return get()
}

func TestFallbackWhenNothingIsSet(t *testing.T) {
	d, err := resolve(t, nil, "")
	if err != nil || d != 5*time.Minute {
		t.Fatalf("got %v, %v; want 5m, nil", d, err)
	}
}

func TestEnvOverridesFallback(t *testing.T) {
	d, err := resolve(t, nil, "90s")
	if err != nil || d != 90*time.Second {
		t.Fatalf("got %v, %v; want 90s, nil", d, err)
	}
}

func TestFlagOverridesEnv(t *testing.T) {
	d, err := resolve(t, []string{"--wait", "0"}, "90s")
	if err != nil || d != 0 {
		t.Fatalf("got %v, %v; want 0, nil", d, err)
	}
}

func TestBadValuesNameTheirOrigin(t *testing.T) {
	if _, err := resolve(t, nil, "soon"); err == nil || !strings.Contains(err.Error(), "$"+testEnv) {
		t.Fatalf("env: got %v; want an error naming $%s", err, testEnv)
	}
	if _, err := resolve(t, []string{"--wait", "-1s"}, ""); err == nil || !strings.Contains(err.Error(), "--wait") {
		t.Fatalf("flag: got %v; want an error naming --wait", err)
	}
}
