// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"nvpair-shared/hostbootstrap"
)

type fixedCommandRunner struct {
	output string
	err    error
	calls  int
}

func (runner *fixedCommandRunner) Run(context.Context, commandSpec) (string, error) {
	runner.calls++
	return runner.output, runner.err
}

func TestFirewallProbeReportsApplicabilityAndUnavailability(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformDarwin,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	runner := &fixedCommandRunner{}
	platform := &runtimePlatform{target: request.Binding.Target, runner: runner}
	probe, err := platform.probeFirewallResource(context.Background(), request)
	if err != nil || !probe.NotApplicable || !probe.Exact || runner.calls != 0 {
		t.Fatalf("macOS probe = %#v calls=%d error=%v", probe, runner.calls, err)
	}
	ufw := []byte("ENABLED=yes\n### tuple ### allow tcp 22 0.0.0.0/0 any 0.0.0.0/0 in comment=NVIDIA PAIR SSH (owned)\n-A ufw-user-input -p tcp --dport 22 -j ACCEPT\n")
	present, exact, err := parseUFWOwnedRule(ufw)
	if err != nil || !present || !exact {
		t.Fatalf("UFW parse present=%t exact=%t error=%v", present, exact, err)
	}
}

func TestNativeDefinitionsSupportClosedTargetMatrix(t *testing.T) {
	for _, target := range hostbootstrap.SupportedTargets() {
		distro := ""
		if target.Platform == hostbootstrap.PlatformLinux {
			distro = "debian"
		}
		if _, err := nativeDefinitionFor(target, distro); err != nil {
			t.Fatalf("nativeDefinitionFor(%#v, %q) error = %v", target, distro, err)
		}
	}
}

func TestNativeDefinitionsUseExactCommandsWithoutShells(t *testing.T) {
	tests := []struct {
		name       string
		target     hostbootstrap.Target
		distro     string
		wantState  string
		wantLocal  localEndpoint
		wantApply  map[hostbootstrap.ResourceKind][]commandSpec
		wantRemove map[hostbootstrap.ResourceKind][]commandSpec
	}{
		{
			name: "windows-amd64",
			target: hostbootstrap.Target{
				Platform:     hostbootstrap.PlatformWindows,
				Architecture: hostbootstrap.ArchitectureAMD64,
			},
			wantState: `%ProgramData%\NVIDIA Corporation\Personal AI Router\host-bootstrap`,
			wantLocal: localEndpoint{
				Address: `\\.\pipe\nvpair-host-helper`,
				DACL:    `O:SYG:SYD:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;TARGET_SID)`,
			},
			wantApply: map[hostbootstrap.ResourceKind][]commandSpec{
				hostbootstrap.ResourceSSHService: {
					{Path: `C:\Windows\System32\dism.exe`, Args: []string{"/Online", "/Add-Capability", "/CapabilityName:OpenSSH.Server~~~~0.0.1.0", "/NoRestart"}},
					{Path: `C:\Windows\System32\sc.exe`, Args: []string{"config", "sshd", "start=", "auto"}},
					{Path: `C:\Windows\System32\sc.exe`, Args: []string{"start", "sshd"}},
				},
				hostbootstrap.ResourceHelper: {
					{Path: `C:\Windows\System32\sc.exe`, Args: []string{"create", "nvpair-host-helper", `binPath=`, `"C:\Program Files\NVIDIA Corporation\PAIR\nvpair-host-helper.exe" service`, "start=", "auto", "DisplayName=", "NVIDIA PAIR Host Helper"}},
					{Path: `C:\Windows\System32\sc.exe`, Args: []string{"start", "nvpair-host-helper"}},
				},
			},
			wantRemove: map[hostbootstrap.ResourceKind][]commandSpec{
				hostbootstrap.ResourceHelper: {
					{Path: `C:\Windows\System32\sc.exe`, Args: []string{"stop", "nvpair-host-helper"}},
					{Path: `C:\Windows\System32\sc.exe`, Args: []string{"delete", "nvpair-host-helper"}},
				},
			},
		},
		{
			name: "darwin-arm64",
			target: hostbootstrap.Target{
				Platform:     hostbootstrap.PlatformDarwin,
				Architecture: hostbootstrap.ArchitectureARM64,
			},
			wantState: "/Library/Application Support/NVIDIA/Personal AI Router/host-bootstrap",
			wantLocal: localEndpoint{
				Address: "/var/run/nvpair-host-helper.sock",
				Mode:    0660,
				Owner:   "root",
				Group:   "TARGET_GID",
			},
			wantApply: map[hostbootstrap.ResourceKind][]commandSpec{
				hostbootstrap.ResourceSSHService: {
					{Path: "/usr/sbin/systemsetup", Args: []string{"-setremotelogin", "on"}},
				},
				hostbootstrap.ResourceHelper: {
					{Path: "/bin/launchctl", Args: []string{"bootstrap", "system", "/Library/LaunchDaemons/com.nvidia.nvpair.host-helper.plist"}},
					{Path: "/bin/launchctl", Args: []string{"kickstart", "-k", "system/com.nvidia.nvpair.host-helper"}},
				},
			},
			wantRemove: map[hostbootstrap.ResourceKind][]commandSpec{
				hostbootstrap.ResourceHelper: {
					{Path: "/bin/launchctl", Args: []string{"bootout", "system/com.nvidia.nvpair.host-helper"}},
				},
			},
		},
		{
			name: "debian-amd64",
			target: hostbootstrap.Target{
				Platform:     hostbootstrap.PlatformLinux,
				Architecture: hostbootstrap.ArchitectureAMD64,
			},
			distro:    "debian",
			wantState: "/var/lib/nvpair/host-bootstrap",
			wantLocal: localEndpoint{
				Address: "/run/nvpair-host-helper.sock",
				Mode:    0660,
				Owner:   "root",
				Group:   "TARGET_GID",
			},
			wantApply: map[hostbootstrap.ResourceKind][]commandSpec{
				hostbootstrap.ResourceSSHService: {
					{Path: "/usr/bin/apt-get", Args: []string{"install", "-y", "--no-install-recommends", "openssh-server"}},
					{Path: "/usr/bin/systemctl", Args: []string{"enable", "--now", "ssh"}},
				},
				hostbootstrap.ResourceHelper: {
					{Path: "/usr/bin/systemctl", Args: []string{"daemon-reload"}},
					{Path: "/usr/bin/systemctl", Args: []string{"enable", "--now", "nvpair-host-helper.socket"}},
					{Path: "/usr/bin/systemctl", Args: []string{"start", "nvpair-host-helper.service"}},
				},
			},
			wantRemove: map[hostbootstrap.ResourceKind][]commandSpec{
				hostbootstrap.ResourceHelper: {
					{Path: "/usr/bin/systemctl", Args: []string{"disable", "--now", "nvpair-host-helper.socket", "nvpair-host-helper.service"}},
					{Path: "/usr/bin/systemctl", Args: []string{"daemon-reload"}},
				},
			},
		},
		{
			name: "ubuntu-arm64",
			target: hostbootstrap.Target{
				Platform:     hostbootstrap.PlatformLinux,
				Architecture: hostbootstrap.ArchitectureARM64,
			},
			distro:    "ubuntu",
			wantState: "/var/lib/nvpair/host-bootstrap",
			wantLocal: localEndpoint{
				Address: "/run/nvpair-host-helper.sock",
				Mode:    0660,
				Owner:   "root",
				Group:   "TARGET_GID",
			},
			wantApply: map[hostbootstrap.ResourceKind][]commandSpec{
				hostbootstrap.ResourceSSHService: {
					{Path: "/usr/bin/apt-get", Args: []string{"install", "-y", "--no-install-recommends", "openssh-server"}},
					{Path: "/usr/bin/systemctl", Args: []string{"enable", "--now", "ssh"}},
				},
				hostbootstrap.ResourceHelper: {
					{Path: "/usr/bin/systemctl", Args: []string{"daemon-reload"}},
					{Path: "/usr/bin/systemctl", Args: []string{"enable", "--now", "nvpair-host-helper.socket"}},
					{Path: "/usr/bin/systemctl", Args: []string{"start", "nvpair-host-helper.service"}},
				},
			},
			wantRemove: map[hostbootstrap.ResourceKind][]commandSpec{
				hostbootstrap.ResourceHelper: {
					{Path: "/usr/bin/systemctl", Args: []string{"disable", "--now", "nvpair-host-helper.socket", "nvpair-host-helper.service"}},
					{Path: "/usr/bin/systemctl", Args: []string{"daemon-reload"}},
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition, err := nativeDefinitionFor(test.target, test.distro)
			if err != nil {
				t.Fatalf("nativeDefinitionFor() error = %v", err)
			}
			if definition.StateDirectory != test.wantState || definition.Local != test.wantLocal {
				t.Fatalf("definition metadata = %#v", definition)
			}
			for kind, want := range test.wantApply {
				if got := definition.Apply[kind]; !reflect.DeepEqual(got, want) {
					t.Fatalf("apply %s = %#v, want %#v", kind, got, want)
				}
			}
			for kind, want := range test.wantRemove {
				if got := definition.Remove[kind]; !reflect.DeepEqual(got, want) {
					t.Fatalf("remove %s = %#v, want %#v", kind, got, want)
				}
			}
			for _, set := range []map[hostbootstrap.ResourceKind][]commandSpec{definition.Apply, definition.Remove} {
				for _, commands := range set {
					for _, command := range commands {
						base := strings.ToLower(command.Path)
						if strings.HasSuffix(base, `\cmd.exe`) ||
							strings.HasSuffix(base, `\powershell.exe`) ||
							command.Path == "/bin/sh" ||
							command.Path == "/bin/bash" ||
							command.Path == "/usr/bin/env" {
							t.Fatalf("shell command is prohibited: %#v", command)
						}
					}
				}
			}
		})
	}
}

func TestNativeDefinitionsRejectUnsupportedDistroArchitectureAndPaths(t *testing.T) {
	for _, test := range []struct {
		target hostbootstrap.Target
		distro string
	}{
		{
			target: hostbootstrap.Target{
				Platform:     hostbootstrap.PlatformLinux,
				Architecture: hostbootstrap.ArchitectureAMD64,
			},
			distro: "fedora",
		},
		{
			target: hostbootstrap.Target{
				Platform:     hostbootstrap.PlatformLinux,
				Architecture: hostbootstrap.Architecture("386"),
			},
			distro: "debian",
		},
		{
			target: hostbootstrap.Target{
				Platform:     hostbootstrap.Platform("freebsd"),
				Architecture: hostbootstrap.ArchitectureAMD64,
			},
		},
	} {
		if _, err := nativeDefinitionFor(test.target, test.distro); !errors.Is(err, ErrUnsupportedTarget) {
			t.Fatalf("nativeDefinitionFor(%#v, %q) error = %v", test.target, test.distro, err)
		}
	}

	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformWindows,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	request.Binding.Helper.Path = `C:\Temp\attacker.exe`
	if err := validateNativeBinding(request.Binding); !errors.Is(err, ErrUnsupportedIdentity) {
		t.Fatalf("arbitrary helper executable path error = %v", err)
	}
	request = testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	request.Binding.Product.Path = "/tmp/attacker"
	if err := validateNativeBinding(request.Binding); !errors.Is(err, ErrUnsupportedIdentity) {
		t.Fatalf("arbitrary product executable path error = %v", err)
	}
}

func TestHelperDefinitionsCarryExactServiceAndLocalACLMetadata(t *testing.T) {
	tests := []struct {
		target      hostbootstrap.Target
		distro      string
		wantPath    string
		wantContent string
	}{
		{
			target: hostbootstrap.Target{
				Platform:     hostbootstrap.PlatformDarwin,
				Architecture: hostbootstrap.ArchitectureAMD64,
			},
			wantPath:    "/Library/LaunchDaemons/com.nvidia.nvpair.host-helper.plist",
			wantContent: "<string>com.nvidia.nvpair.host-helper</string>",
		},
		{
			target: hostbootstrap.Target{
				Platform:     hostbootstrap.PlatformLinux,
				Architecture: hostbootstrap.ArchitectureARM64,
			},
			distro:      "ubuntu",
			wantPath:    "/etc/systemd/system/nvpair-host-helper.socket",
			wantContent: "ListenStream=/run/nvpair-host-helper.sock",
		},
	}
	for _, test := range tests {
		definition, err := nativeDefinitionFor(test.target, test.distro)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, file := range definition.ManagedFiles {
			if file.Path == test.wantPath && strings.Contains(file.Content, test.wantContent) {
				found = true
				if file.Mode != 0600 {
					t.Fatalf("%s mode = %o", file.Path, file.Mode)
				}
			}
		}
		if !found {
			t.Fatalf("managed helper definition missing %q: %#v", test.wantPath, definition.ManagedFiles)
		}
	}
}

type fakeSecureFS struct {
	files        map[string][]byte
	meta         map[string]secureMetadata
	writes       []string
	removeFailAt int
	removeCalls  int
}

func (filesystem *fakeSecureFS) EnsurePrivateDirectory(string) error {
	return nil
}

func (filesystem *fakeSecureFS) ReadNoFollow(path string, _ int64) ([]byte, secureMetadata, error) {
	data, ok := filesystem.files[path]
	if !ok {
		return nil, secureMetadata{}, fs.ErrNotExist
	}
	copy := append([]byte(nil), data...)
	return copy, filesystem.meta[path], nil
}

func (filesystem *fakeSecureFS) AtomicWriteNoFollow(path string, data []byte, _ fs.FileMode) error {
	if filesystem.files == nil {
		filesystem.files = make(map[string][]byte)
	}
	if filesystem.meta == nil {
		filesystem.meta = make(map[string]secureMetadata)
	}
	filesystem.files[path] = append([]byte(nil), data...)
	filesystem.meta[path] = secureMetadata{
		Regular: true,
		Links:   1,
		Mode:    0600,
		Owner:   rootOwner,
	}
	filesystem.writes = append(filesystem.writes, path)
	return nil
}

func (filesystem *fakeSecureFS) RemoveNoFollow(path string) error {
	filesystem.removeCalls++
	if filesystem.removeFailAt == filesystem.removeCalls {
		return errors.New("injected remove failure")
	}
	delete(filesystem.files, path)
	delete(filesystem.meta, path)
	return nil
}

func TestDiskStateRejectsUnsafeFilesAndChangedIdentity(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	plan, err := hostbootstrap.Reconcile(request, absentTestObservations())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		meta secureMetadata
	}{
		{name: "symlink", meta: secureMetadata{Regular: true, Symlink: true, Links: 1, Mode: 0600, Owner: rootOwner}},
		{name: "reparse", meta: secureMetadata{Regular: true, Reparse: true, Links: 1, Mode: 0600, Owner: rootOwner}},
		{name: "hard-link", meta: secureMetadata{Regular: true, Links: 2, Mode: 0600, Owner: rootOwner}},
		{name: "group-writable", meta: secureMetadata{Regular: true, Links: 1, Mode: 0620, Owner: rootOwner}},
		{name: "other-writable", meta: secureMetadata{Regular: true, Links: 1, Mode: 0602, Owner: rootOwner}},
		{name: "wrong-owner", meta: secureMetadata{Regular: true, Links: 1, Mode: 0600, Owner: "1000"}},
		{name: "not-regular", meta: secureMetadata{Links: 1, Mode: 0600, Owner: rootOwner}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			filesystem := &fakeSecureFS{
				files: map[string][]byte{"/state/operation.json": []byte(`{}`)},
				meta:  map[string]secureMetadata{"/state/operation.json": test.meta},
			}
			state := newDiskState("/state", filesystem)
			if _, _, err := state.LoadOperation(); !errors.Is(err, ErrUnsafeState) {
				t.Fatalf("LoadOperation() error = %v, want ErrUnsafeState", err)
			}
		})
	}

	filesystem := &fakeSecureFS{}
	state := newDiskState("/state", filesystem)
	if err := state.SaveOperation(request, plan); err != nil {
		t.Fatalf("SaveOperation() error = %v", err)
	}
	changed := request
	changed.OperationID = strings.Repeat("cd", 16)
	changedPlan, err := hostbootstrap.Reconcile(changed, absentTestObservations())
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SaveOperation(changed, changedPlan); !errors.Is(err, ErrStateIdentity) {
		t.Fatalf("changed identity error = %v, want ErrStateIdentity", err)
	}
}

func TestOperationPersistenceUsesOneAtomicEnvelope(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	plan, err := hostbootstrap.Reconcile(request, absentTestObservations())
	if err != nil {
		t.Fatal(err)
	}
	filesystem := &fakeSecureFS{}
	state := newDiskState("/state", filesystem)
	if err := state.SaveOperation(request, plan); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(
		filesystem.writes,
		[]string{"/state/operation.json"},
	) {
		t.Fatalf("operation writes = %#v", filesystem.writes)
	}
	loadedRequest, loadedPlan, err := state.LoadOperation()
	if err != nil {
		t.Fatal(err)
	}
	if loadedRequest != request || !reflect.DeepEqual(loadedPlan, plan) {
		t.Fatalf("loaded request=%#v plan=%#v", loadedRequest, loadedPlan)
	}
}

func TestDiskStateRejectsChangedMarkerAndReceipt(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureARM64,
	})
	exact := exactTestObservations(request)
	plan, err := hostbootstrap.Reconcile(request, exact)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := hostbootstrap.NewReceipt(plan, exact)
	if err != nil {
		t.Fatal(err)
	}
	filesystem := &fakeSecureFS{}
	state := newDiskState("/state", filesystem)
	if err := state.SaveOperation(request, plan); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	changedReceipt := receipt
	changedReceipt.OperationID = strings.Repeat("cd", 16)
	if err := state.SaveReceipt(changedReceipt); !errors.Is(err, ErrReceiptImmutable) {
		t.Fatalf("changed receipt error = %v", err)
	}

	marker := resourceMarker{
		SchemaVersion:  hostbootstrap.SchemaVersion,
		Kind:           hostbootstrap.ResourceProduct,
		OperationID:    request.OperationID,
		IdentitySHA256: request.Binding.Product.SHA256,
		Files: []payloadManifestFile{{
			Path:   "nvpair-tui",
			Type:   payloadTypeFile,
			Size:   3,
			Mode:   0755,
			SHA256: payloadDigest([]byte("tui")),
		}},
	}
	if err := state.SaveMarker(marker); err != nil {
		t.Fatal(err)
	}
	changedMarker := marker
	changedMarker.IdentitySHA256 = strings.Repeat("56", 32)
	if err := state.SaveMarker(changedMarker); !errors.Is(err, ErrStateIdentity) {
		t.Fatalf("changed marker error = %v", err)
	}

	next := request
	next.OperationID = strings.Repeat("cd", 16)
	nextPlan, err := hostbootstrap.Reconcile(next, exactTestObservations(next))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SaveOperation(next, nextPlan); err != nil {
		t.Fatalf("completed operation did not permit a new idempotent run: %v", err)
	}
	oldReceiptPath := "/state/receipt-" + request.OperationID + ".json"
	if _, found := filesystem.files[oldReceiptPath]; !found {
		t.Fatal("immutable receipt was not retained across operations")
	}
}

func TestUninstallTombstoneBindsRetainedReceiptAndConfirmedAbsence(
	t *testing.T,
) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	exact := exactTestObservations(request)
	plan, err := hostbootstrap.Reconcile(request, exact)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := hostbootstrap.NewReceipt(plan, exact)
	if err != nil {
		t.Fatal(err)
	}
	filesystem := &fakeSecureFS{}
	state := newDiskState("/state", filesystem)
	if err := state.SaveOperation(request, plan); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	absent := absentTestObservations()
	if err := state.SaveUninstalled(receipt, absent); err != nil {
		t.Fatal(err)
	}
	raw := filesystem.files["/state/uninstalled.json"]
	var tombstone uninstallTombstone
	if err := json.Unmarshal(raw, &tombstone); err != nil {
		t.Fatal(err)
	}
	if tombstone.ReceiptOperationID != receipt.OperationID ||
		!reflect.DeepEqual(tombstone.ConfirmedAbsent, absent) {
		t.Fatalf("tombstone = %#v", tombstone)
	}
	if err := state.RemoveOperation(); err != nil {
		t.Fatal(err)
	}
	receiptPath := "/state/receipt-" + receipt.OperationID + ".json"
	if _, found := filesystem.files[receiptPath]; !found {
		t.Fatal("immutable receipt was removed")
	}
	if _, found := filesystem.files["/state/uninstalled.json"]; !found {
		t.Fatal("uninstall tombstone was removed")
	}
	notAbsent := absent
	notAbsent.Helper.Ownership = hostbootstrap.OwnershipOwned
	notAbsent.Helper.Identity = &request.Binding.Helper
	if err := state.SaveUninstalled(
		receipt,
		notAbsent,
	); !errors.Is(err, ErrStateIdentity) {
		t.Fatalf("non-absent tombstone error = %v", err)
	}
}

func TestRemoveOperationDeletesOperationEnvelopeLast(t *testing.T) {
	for failAt := 1; failAt <= len(uninstallOrder)+4; failAt++ {
		t.Run(strconv.Itoa(failAt), func(t *testing.T) {
			request := testRequest(hostbootstrap.Target{
				Platform:     hostbootstrap.PlatformWindows,
				Architecture: hostbootstrap.ArchitectureAMD64,
			})
			exact := exactTestObservations(request)
			plan, err := hostbootstrap.Reconcile(request, exact)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := hostbootstrap.NewReceipt(plan, exact)
			if err != nil {
				t.Fatal(err)
			}
			filesystem := &fakeSecureFS{}
			state := newDiskState("/state", filesystem)
			if err := state.SaveOperation(request, plan); err != nil {
				t.Fatal(err)
			}
			if err := state.SaveReceipt(receipt); err != nil {
				t.Fatal(err)
			}
			if err := state.SaveUninstalled(
				receipt,
				absentTestObservations(),
			); err != nil {
				t.Fatal(err)
			}
			if err := state.SavePrincipal(reviewedPrincipalState{
				UID: 1000,
				GID: 1000,
			}); err != nil {
				t.Fatal(err)
			}
			filesystem.AtomicWriteNoFollow(
				"/state/pending.json",
				[]byte(`{}`),
				0600,
			)
			for _, kind := range uninstallOrder {
				marker := resourceMarker{
					SchemaVersion: hostbootstrap.SchemaVersion,
					Kind:          kind,
					OperationID:   request.OperationID,
					IdentitySHA256: resourceIdentitySHA256(
						kind,
						request.Binding,
					),
				}
				if kind == hostbootstrap.ResourceAuthorizedKey {
					marker.AuthorizedKeyLine =
						desiredAuthorizedKeyLine(request)
				}
				if kind == hostbootstrap.ResourceProduct {
					marker.Files = []payloadManifestFile{{
						Path:   "nvpair.exe",
						Type:   payloadTypeFile,
						Size:   1,
						Mode:   0755,
						SHA256: payloadDigest([]byte("x")),
					}}
				}
				if err := state.SaveMarker(marker); err != nil {
					t.Fatal(err)
				}
			}
			filesystem.removeCalls = 0
			filesystem.removeFailAt = failAt
			if err := state.RemoveOperation(); err == nil {
				t.Fatal("injected deletion failure was ignored")
			}
			if _, found := filesystem.files["/state/operation.json"]; !found {
				t.Fatal("operation removed before teardown completed")
			}
			filesystem.removeFailAt = 0
			filesystem.removeCalls = 0
			if err := state.RemoveOperation(); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{
				"operation.json",
				"principal.json",
				"pending.json",
				"uninstall-pending.json",
			} {
				if _, found := filesystem.files["/state/"+name]; found {
					t.Fatalf("%s remained after retry", name)
				}
			}
			for _, kind := range uninstallOrder {
				markerPath := state.markerPath(kind)
				if _, found := filesystem.files[markerPath]; found {
					t.Fatalf("%s marker remained after retry", kind)
				}
			}
			receiptPath := state.receiptPath(receipt.OperationID)
			if _, found := filesystem.files[receiptPath]; !found {
				t.Fatal("historical receipt was removed")
			}
			if _, found := filesystem.files["/state/uninstalled.json"]; !found {
				t.Fatal("uninstall tombstone was removed")
			}
		})
	}
}
