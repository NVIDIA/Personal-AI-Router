// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func diagnosticQuickOutputFixture() string {
	var out strings.Builder
	out.WriteString("NCCL version 2.30.7+cuda13.0\n# fixed float/sum correctness rows\n")
	for size := 8; size <= 65536; size *= 2 {
		fmt.Fprintf(&out, "%d %d float sum -1 12.3 1.2 1.3 0 12.4 1.1 1.2 0\n", size, size/4)
	}
	return out.String()
}

func TestDiagnosticMPIQuickOutputContract(t *testing.T) {
	quick := diagnosticQuickOutputFixture()
	lines := strings.Split(strings.TrimSuffix(quick, "\n"), "\n")
	changeFirstRow := func(field int, value string) string {
		fields := strings.Fields(lines[2])
		fields[field] = value
		return strings.Replace(quick, lines[2], strings.Join(fields, " "), 1)
	}
	tests := []struct {
		name, stdout, stderr, recipe string
		rows                         int
	}{
		{"quick exact banner", quick, "", diagnosticMPIQuickRecipe, 14},
		{"quick benign UCX stderr", quick, "[fixture] UCX WARN unused environment variable\n", diagnosticMPIQuickRecipe, 14},
		{"missing first row", strings.Replace(quick, lines[2]+"\n", "", 1), "", diagnosticMPIQuickRecipe, 0},
		{"missing last row", strings.Replace(quick, lines[len(lines)-1]+"\n", "", 1), "", diagnosticMPIQuickRecipe, 0},
		{"duplicate row", strings.Replace(quick, lines[2]+"\n", lines[2]+"\n"+lines[2]+"\n", 1), "", diagnosticMPIQuickRecipe, 0},
		{"out of order", strings.Replace(quick, lines[2]+"\n"+lines[3], lines[3]+"\n"+lines[2], 1), "", diagnosticMPIQuickRecipe, 0},
		{"wrong out of place", changeFirstRow(8, "1"), "", diagnosticMPIQuickRecipe, 0},
		{"wrong in place", changeFirstRow(12, "1"), "", diagnosticMPIQuickRecipe, 0},
		{"wrong datatype", changeFirstRow(2, "double"), "", diagnosticMPIQuickRecipe, 0},
		{"wrong reduction", changeFirstRow(3, "prod"), "", diagnosticMPIQuickRecipe, 0},
		{"wrong count", changeFirstRow(1, "3"), "", diagnosticMPIQuickRecipe, 0},
		{"wrong root", changeFirstRow(4, "0"), "", diagnosticMPIQuickRecipe, 0},
		{"fatal timeout stderr", quick, "Test timeout (10s)\n", diagnosticMPIQuickRecipe, 0},
		{"fatal failure stderr", quick, "Test failure\n", diagnosticMPIQuickRecipe, 0},
		{"fatal rank stderr", quick, "owned NCCL rank failed\n", diagnosticMPIQuickRecipe, 0},
		{"unknown version banner", strings.Replace(quick, "2.30.7", "2.30.8", 1), "", diagnosticMPIQuickRecipe, 0},
		{"unknown CUDA banner", strings.Replace(quick, "cuda13.0", "cuda12.8", 1), "", diagnosticMPIQuickRecipe, 0},
		{"UCX stdout remains invalid", "UCX WARN fixture\n" + quick, "", diagnosticMPIQuickRecipe, 0},
		{"unknown stdout noise", quick + "unrecognized output\n", "", diagnosticMPIQuickRecipe, 0},
		{"unknown recipe", quick, "", "foreign-recipe", 0},
		{"empty live recipe", quick, "", "", 0},
		{"legacy 21 rows", diagnosticTestOutput(), "", diagnosticMPILegacyRecipe, 21},
		{"legacy rejects 14 rows", strings.TrimPrefix(quick, lines[0]+"\n"), "", diagnosticMPILegacyRecipe, 0},
		{"quick rejects 21 rows", diagnosticTestOutput(), "", diagnosticMPIQuickRecipe, 0},
	}
	for _, test := range tests {
		if test.recipe == diagnosticMPIQuickRecipe {
			test.name, test.recipe = "triple "+test.name, diagnosticMPITripleRecipe
			tests = append(tests, test)
		}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			samples, err := parseDiagnosticMPIOutput(tt.stdout, tt.stderr, tt.recipe)
			if tt.rows == 0 {
				if err == nil {
					t.Fatal("invalid output was accepted")
				}
				return
			}
			if err != nil || len(samples) != tt.rows {
				t.Fatalf("got %d rows, error %v; want %d", len(samples), err, tt.rows)
			}
			for i, sample := range samples {
				if sample.Bytes != uint64(8)<<i || sample.Wrong != 0 {
					t.Fatalf("row %d violates ordered sizes or correctness: %+v", i, sample)
				}
			}
		})
	}
}

func TestDiagnosticMPIQuickRecipeArgsRemainFixedAndIndependent(t *testing.T) {
	if diagnosticMPIQuickRecipe != "pair-two-spark-nccl-socket-correctness-v2" || diagnosticMPILegacyRecipe != "pair-two-spark-nccl-socket-smoke-v1" || diagnosticMPITripleRecipe != "pair-three-spark-nccl-socket-correctness-v3" {
		t.Fatal("fixed recipe identities changed")
	}
	legacyBefore := append([]string(nil), diagnosticNCCLArgs...)
	wantQuick := []string{"-b", "8", "-e", "65536", "-f", "2", "-g", "1", "-t", "1", "-n", "3", "-w", "1", "-c", "1", "-N", "1", "-T", "10", "-d", "float", "-o", "sum"}
	for _, tt := range []struct {
		recipe  string
		args    []string
		maximum uint64
		ranks   int
	}{{diagnosticMPIQuickRecipe, wantQuick, 65536, 2}, {diagnosticMPILegacyRecipe, legacyBefore, 8388608, 2}, {diagnosticMPITripleRecipe, wantQuick, 65536, 3}} {
		args, maximum, err := diagnosticMPIRecipeArgs(tt.recipe)
		if err != nil || maximum != tt.maximum || !reflect.DeepEqual(args, tt.args) || diagnosticMPIRecipeRanks(tt.recipe) != tt.ranks {
			t.Fatalf("recipe %s: args=%v maximum=%d error=%v", tt.recipe, args, maximum, err)
		}
		args[0] = "caller mutation"
		again, _, err := diagnosticMPIRecipeArgs(tt.recipe)
		if err != nil || !reflect.DeepEqual(again, tt.args) || !reflect.DeepEqual(diagnosticNCCLArgs, legacyBefore) {
			t.Fatal("returned args aliased a fixed recipe or changed legacy globals")
		}
	}
	for _, recipe := range []string{"", "foreign-recipe", "pair-two-spark-nccl-socket-correctness-v3"} {
		if _, _, err := diagnosticMPIRecipeArgs(recipe); err == nil {
			t.Fatalf("unknown recipe %q was accepted", recipe)
		}
	}
}

func TestDiagnosticMPIParallelCleanupRequiresEveryAttemptedMember(t *testing.T) {
	members := []diagnosticMember{{NodeID: "node-a"}, {NodeID: "node-b"}, {NodeID: "node-c"}}
	for _, outcome := range []string{"complete", "foreign identity", "unclean", "error"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			started, release, done := make(chan string, 3), make(chan struct{}), make(chan bool, 1)
			go func() {
				done <- cleanupDiagnosticMPIMembers(ctx, members, func(got context.Context, member diagnosticMember) (diagnosticParticipantResult, error) {
					if got != ctx {
						return diagnosticParticipantResult{}, errors.New("cleanup deadline was replaced")
					}
					started <- member.NodeID
					select {
					case <-release:
					case <-got.Done():
						return diagnosticParticipantResult{}, got.Err()
					}
					result := diagnosticParticipantResult{NodeID: member.NodeID, CleanupConfirmed: true}
					if member.NodeID == "node-c" {
						switch outcome {
						case "foreign identity":
							result.NodeID = "node-b"
						case "unclean":
							result.CleanupConfirmed = false
						case "error":
							return result, errors.New("cleanup fixture failure")
						}
					}
					return result, nil
				})
			}()
			seen := map[string]bool{}
			for range members {
				select {
				case id := <-started:
					seen[id] = true
				case <-ctx.Done():
					t.Fatal("cleanup serialized before contacting every attempted participant")
				}
			}
			if len(seen) != 3 {
				t.Fatal("cleanup did not contact the exact three participants")
			}
			close(release)
			if clean := <-done; clean != (outcome == "complete") {
				t.Fatalf("cleanup %q incorrectly returned %v", outcome, clean)
			}
		})
	}
}

func TestDiagnosticMPIParallelCleanupDeadlineCannotPromoteLateAcknowledgments(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	members := []diagnosticMember{{NodeID: "node-a"}, {NodeID: "node-b"}, {NodeID: "node-c"}}
	started, done := make(chan struct{}, 3), make(chan bool, 1)
	go func() {
		done <- cleanupDiagnosticMPIMembers(ctx, members, func(ctx context.Context, member diagnosticMember) (diagnosticParticipantResult, error) {
			started <- struct{}{}
			<-ctx.Done()
			return diagnosticParticipantResult{NodeID: member.NodeID, CleanupConfirmed: true}, nil
		})
	}()
	for range members {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("all cleanup callbacks did not start")
		}
	}
	stop()
	select {
	case clean := <-done:
		if clean {
			t.Fatal("post-deadline acknowledgments became confirmed cleanup")
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup did not respect the shared deadline")
	}
}
