// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestEngineChildEnvironment(t *testing.T) {
	if os.Getenv("PAIR_ENV_CHILD") == "1" {
		fmt.Print(os.Getenv("PAIR_CACHE_TEST"))
		os.Exit(0)
	}
	st := &engineState{installDir: t.TempDir(), port: 8081, plat: &Platform{Runtime: Runtime{Env: map[string]string{"PAIR_ENV_CHILD": "1", "PAIR_CACHE_TEST": "{install_dir}/cache"}}}}
	env, err := childEnv(st)
	if err != nil {
		t.Fatal(err)
	}
	argv := []string{os.Args[0], "-test.run=^TestEngineChildEnvironment$"}
	e := &Executor{}
	out, err := e.runCommandOutput(context.Background(), argv, env)
	if err != nil || out != st.installDir+"/cache" {
		t.Fatalf("action environment: %q, %v", out, err)
	}
	var lines []string
	p, err := startManagedProc(argv[0], argv[1:], env, func(_, line string) { lines = append(lines, line) })
	if err != nil {
		t.Fatal(err)
	}
	<-p.done
	if strings.Join(lines, "") != out {
		t.Fatalf("serving environment differs: %q", lines)
	}
	if err := e.runCommand(context.Background(), argv, env); err != nil {
		t.Fatal(err)
	}
	st.plat.Runtime.Env["PAIR_CACHE_TEST"] = "{unresolved}"
	if _, err := childEnv(st); err == nil {
		t.Fatal("accepted unresolved environment")
	}
}
