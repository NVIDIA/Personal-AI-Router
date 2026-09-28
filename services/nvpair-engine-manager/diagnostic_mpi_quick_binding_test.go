// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func mpiQuickRetag(t *testing.T, raw json.RawMessage, recipe string) (json.RawMessage, string) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if recipe == "" {
		delete(body, "recipeId")
	} else {
		body["recipeId"] = recipe
	}
	delete(body, "planDigest")
	unsigned, err := mpiCanonical(body)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(unsigned)
	digest := hex.EncodeToString(sum[:])
	body["planDigest"] = digest
	result, err := mpiCanonical(body)
	if err != nil {
		t.Fatal(err)
	}
	return result, digest
}

func TestDiagnosticMPIQuickPlanRequiresOriginalLeaseDigestAndExplicitRecipe(t *testing.T) {
	_, profile, raw, request := mpiTransportFixture(t)
	quick, digest := mpiQuickRetag(t, raw, diagnosticMPIQuickRecipe)
	if _, err := validateDiagnosticMPIBinding(profile, quick, request, true); err == nil {
		t.Fatal("changed recipe escaped the original lease's plan digest")
	}
	request.BootstrapPlanDigest = digest
	if _, err := validateDiagnosticMPIBinding(profile, quick, request, true); err != nil {
		t.Fatal(err)
	}
	for _, recipe := range []string{"", "unknown-recipe"} {
		bad, hash := mpiQuickRetag(t, raw, recipe)
		request.BootstrapPlanDigest = hash
		if _, err := validateDiagnosticMPIBinding(profile, bad, request, false); err == nil {
			t.Fatal("missing/unknown plan recipe fell back to legacy")
		}
	}
}

func TestDiagnosticMPIQuickReviewMustDeclareItsPlanRecipe(t *testing.T) {
	for _, recipe := range []string{"", diagnosticMPILegacyRecipe, diagnosticMPIQuickRecipe, "unknown"} {
		t.Run(recipe, func(t *testing.T) {
			d, original := mpiReviewFixture(t)
			bound := *original
			bound.Public.ReviewID = strings.Repeat("e", 32)
			bound.Public.RecipeID = recipe
			bound.Plan, _ = mpiQuickRetag(t, original.Plan, diagnosticMPIQuickRecipe)
			d.mu.Lock()
			err := d.publishMPIReviewLocked(&bound, mpiReviewStorage())
			d.mu.Unlock()
			if (recipe == diagnosticMPIQuickRecipe) != (err == nil) {
				t.Fatalf("recipe %q publication: %v", recipe, err)
			}
			if err == nil {
				marker, readErr := d.readMPIReviewMarker(bound.Public.ReviewID, mpiReviewStorage())
				if readErr != nil || marker.RecipeID != diagnosticMPIQuickRecipe {
					t.Fatal("published quick recipe lost its durable identity")
				}
			} else if _, statErr := os.Stat(d.mpiReviewMarkerPath(bound.Public.ReviewID)); !os.IsNotExist(statErr) {
				t.Fatal("refused review wrote a marker")
			}
		})
	}
}

func TestDiagnosticMPIQuickRecipeMismatchCannotReturnRetainedOperation(t *testing.T) {
	for _, memory := range []bool{false, true} {
		d, bound := mpiReviewFixture(t)
		op := mpiReviewTestOperation(bound)
		op.State, op.CleanupConfirmed, op.RecipeID = "failed", true, diagnosticMPIQuickRecipe
		if err := d.saveOperation(op); err != nil {
			t.Fatal(err)
		}
		if memory {
			d.operations[op.OperationID] = op
		}
		if _, err := d.approveMPIWithIO("node-b", bound.Public.ReviewID, mpiReviewStorage(), mpiReviewNoStart(t)); err == nil {
			t.Fatal("legacy marker returned a quick operation")
		}
	}
}

func TestDiagnosticMPIQuickRecoveryChecksSnapshotRecipeBeforeBuildFilter(t *testing.T) {
	d, bound, control := mpiLookupFixture(t, "failed", true)
	marker, err := d.readMPIReviewMarker(bound.Public.ReviewID, mpiReviewStorage())
	if err != nil {
		t.Fatal(err)
	}
	marker.RecipeID = diagnosticMPIQuickRecipe
	d.mu.Lock()
	err = d.persistMPIReviewMarker(marker, false, mpiReviewStorage())
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	op := mpiReviewTestOperation(bound)
	op.State, op.CleanupConfirmed, op.RecipeID = "failed", true, diagnosticMPIQuickRecipe
	if err := d.saveOperation(op); err != nil {
		t.Fatal(err)
	}
	control.BuildOperationID = strings.Repeat("f", 32)
	if _, err := d.recoverMPIAsCoordinator(context.Background(), "node-b", control); err == nil || !strings.Contains(err.Error(), "recipe") {
		t.Fatalf("contradictory snapshot was hidden by another build: %v", err)
	}
}

func TestDiagnosticMPIQuickUpgradePreservesLegacyFailedCleanBytesWithoutReplay(t *testing.T) {
	d, bound, _ := mpiLookupFixture(t, "failed", true)
	operationPath := d.operationPath(bound.Public.OperationID)
	markerPath := d.mpiReviewMarkerPath(bound.Public.ReviewID)
	before, _ := os.ReadFile(operationPath)
	markerBefore, _ := os.ReadFile(markerPath)
	d.mpiReviews = nil // A restarted controller no longer has a volatile key/review.
	op, err := d.approveMPIWithIO("node-b", bound.Public.ReviewID, mpiReviewStorage(), mpiReviewNoStart(t))
	if err != nil || op.RecipeID != "" || op.State != "failed" || !op.CleanupConfirmed {
		t.Fatalf("legacy retained result changed: %+v %v", op, err)
	}
	after, _ := os.ReadFile(operationPath)
	markerAfter, _ := os.ReadFile(markerPath)
	if !bytes.Equal(before, after) || !bytes.Equal(markerBefore, markerAfter) {
		t.Fatal("reading legacy history rewrote original bytes")
	}
}

func TestDiagnosticMPIQuickSeparateStreamsShareOriginalOutputCap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := &diagnosticOutput{cancel: cancel}
	if _, err := output.Write(bytes.Repeat([]byte{'o'}, (1<<20)-4)); err != nil {
		t.Fatal(err)
	}
	if _, err := (diagnosticStderr{output}).Write([]byte("warn")); err != nil {
		t.Fatal(err)
	}
	if output.stderr.String() != "warn" || bytes.Contains(output.data.Bytes(), []byte("warn")) {
		t.Fatal("diagnostics contaminated numeric stdout")
	}
	if _, err := output.Write([]byte{'x'}); err == nil || !output.exceeded || ctx.Err() == nil {
		t.Fatal("separating streams enlarged the combined output limit")
	}
}
