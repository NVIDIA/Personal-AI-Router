// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hostbootstrap

import (
	"errors"
	"reflect"
	"testing"
)

func TestFirewallOwnershipHasExactPlatformSemantics(t *testing.T) {
	windows := contractRequest(
		Target{Platform: PlatformWindows, Architecture: ArchitectureAMD64},
		LaneQuickConnect,
		RoleDesktop,
		RoleDesktop,
	)
	windowsObserved := exactObservations(windows)
	windowsObserved.Firewall = EndpointObservation{Ownership: OwnershipNotApplicable}
	if err := windowsObserved.Validate(windows.Binding.Target); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Windows not-applicable firewall error = %v", err)
	}

	darwin := contractRequest(
		Target{Platform: PlatformDarwin, Architecture: ArchitectureARM64},
		LaneQuickConnect,
		RoleDesktop,
		RoleDesktop,
	)
	darwinObserved := exactObservations(darwin)
	darwinObserved.Firewall = EndpointObservation{Ownership: OwnershipNotApplicable}
	if err := darwinObserved.Validate(darwin.Binding.Target); err != nil {
		t.Fatalf("macOS not-applicable firewall error = %v", err)
	}
	plan, err := Reconcile(darwin, darwinObserved)
	if err != nil || plan.Decision != DecisionNoOp || len(plan.Actions) != 0 {
		t.Fatalf("macOS exact firewall plan = %#v, %v", plan, err)
	}
	receipt, err := NewReceipt(plan, darwinObserved)
	if err != nil || receipt.Validate() != nil {
		t.Fatalf("macOS receipt = %#v, %v", receipt, err)
	}

	linux := contractRequest(
		Target{Platform: PlatformLinux, Architecture: ArchitectureAMD64},
		LaneZeroTouch,
		RoleHeadless,
		RoleHeadless,
	)
	linuxObserved := exactObservations(linux)
	linuxObserved.Firewall = EndpointObservation{Ownership: OwnershipNotApplicable}
	plan, err = Reconcile(linux, linuxObserved)
	if err != nil || plan.Decision != DecisionNoOp || len(plan.Actions) != 0 {
		t.Fatalf("Linux inactive-firewall plan = %#v, %v", plan, err)
	}
}

func TestUnavailableFirewallBlocksAndNeverReceipts(t *testing.T) {
	for _, platform := range []Platform{PlatformWindows, PlatformLinux} {
		request := contractRequest(
			Target{Platform: platform, Architecture: ArchitectureARM64},
			LaneQuickConnect,
			RoleHeadless,
			RoleHeadless,
		)
		observed := exactObservations(request)
		observed.Firewall = EndpointObservation{Ownership: OwnershipUnavailable}
		plan, err := Reconcile(request, observed)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Decision != DecisionRefuseForeign || !reflect.DeepEqual(plan.Actions, []ResourceKind{}) {
			t.Fatalf("%s unavailable firewall plan = %#v", platform, plan)
		}
		if _, err := NewReceipt(plan, observed); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s unavailable receipt error = %v", platform, err)
		}
	}
}

func TestApplicabilityOwnershipCarriesNoEndpoint(t *testing.T) {
	for _, ownership := range []Ownership{OwnershipNotApplicable, OwnershipUnavailable} {
		if err := (EndpointObservation{Ownership: ownership}).validate(); err != nil {
			t.Fatalf("%q without endpoint error = %v", ownership, err)
		}
		endpoint := SSHEndpoint{Address: "192.0.2.10", Port: 22}
		if err := (EndpointObservation{Ownership: ownership, Endpoint: &endpoint}).validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%q with endpoint error = %v", ownership, err)
		}
	}
}
