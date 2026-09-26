// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"reflect"
	"strings"
	"testing"

	"nvpair-shared/hostbootstrap"
)

func TestNativeOwnerManifestIsClosedPerPlatform(t *testing.T) {
	tests := []struct {
		target       hostbootstrap.Target
		wantIdentity string
		wantTUI      string
		wantBroker   string
		wantCommand  []string
	}{
		{
			target:       hostbootstrap.Target{Platform: hostbootstrap.PlatformWindows, Architecture: hostbootstrap.ArchitectureAMD64},
			wantIdentity: "nvpair-headless",
			wantTUI:      `C:\Program Files\NVIDIA Corporation\PAIR\nvpair-host-helper.exe`,
			wantBroker:   `C:\Program Files\NVIDIA Corporation\PAIR\product\nvpair-ui-broker.exe`,
			wantCommand: []string{
				`C:\Program Files\NVIDIA Corporation\PAIR\nvpair-host-helper.exe`,
				"headless-service",
			},
		},
		{
			target:       hostbootstrap.Target{Platform: hostbootstrap.PlatformDarwin, Architecture: hostbootstrap.ArchitectureARM64},
			wantIdentity: "com.nvidia.nvpair.headless",
			wantTUI:      "/Applications/NVPAIR.app/Contents/Resources/cli-bin/nvpair-tui",
			wantBroker:   "/Applications/NVPAIR.app/Contents/Resources/cli-bin/nvpair-ui-broker",
		},
		{
			target:       hostbootstrap.Target{Platform: hostbootstrap.PlatformLinux, Architecture: hostbootstrap.ArchitectureARM64},
			wantIdentity: "nvidia-pair-headless.service",
			wantTUI:      "/opt/nvpair/product/nvpair-tui",
			wantBroker:   "/opt/nvpair/product/nvpair-ui-broker",
		},
	}
	for _, test := range tests {
		manifest, err := nativeOwnerManifestFor(test.target)
		if err != nil {
			t.Fatalf("nativeOwnerManifestFor(%#v) error = %v", test.target, err)
		}
		if manifest.HeadlessIdentity != test.wantIdentity ||
			manifest.TUIPath != test.wantTUI ||
			manifest.BrokerPath != test.wantBroker {
			t.Fatalf("manifest = %#v", manifest)
		}
		wantOwnerCommand := test.wantCommand
		if wantOwnerCommand == nil {
			wantOwnerCommand = []string{
				manifest.TUIPath,
				"--headless",
				"--broker-path",
				manifest.BrokerPath,
			}
		}
		if !reflect.DeepEqual(manifest.OwnerCommand, wantOwnerCommand) {
			t.Fatalf("owner command = %#v, want %#v", manifest.OwnerCommand, wantOwnerCommand)
		}
		for _, commands := range [][]commandSpec{manifest.Start, manifest.Stop, manifest.Remove, manifest.Inspect} {
			for _, command := range commands {
				joined := strings.ToLower(command.Path + " " + strings.Join(command.Args, " "))
				if strings.Contains(joined, "cmd.exe") ||
					strings.Contains(joined, "powershell") ||
					strings.Contains(joined, "/bin/sh") ||
					strings.Contains(joined, "sudo") {
					t.Fatalf("owner manifest contains shell/elevation command: %#v", command)
				}
			}
		}
	}
}

func TestDesktopSelectionNeverLaunchesGUIAsRoot(t *testing.T) {
	for _, target := range hostbootstrap.SupportedTargets() {
		manifest, err := nativeOwnerManifestFor(target)
		if err != nil {
			t.Fatal(err)
		}
		if len(manifest.DesktopStart) != 0 {
			t.Fatalf("%#v desktop commands = %#v", target, manifest.DesktopStart)
		}
	}
}

func TestHeadlessOwnerDefinitionsUseOnlyFixedCommand(t *testing.T) {
	for _, target := range hostbootstrap.SupportedTargets() {
		manifest, err := nativeOwnerManifestFor(target)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, file := range manifest.ManagedFiles {
			if strings.Contains(file.Content, manifest.TUIPath) &&
				strings.Contains(file.Content, "--headless") &&
				strings.Contains(file.Content, manifest.BrokerPath) {
				found = true
			}
		}
		if target.Platform != hostbootstrap.PlatformWindows && !found {
			t.Fatalf("%#v missing exact headless owner definition: %#v", target, manifest.ManagedFiles)
		}
	}
}

func TestLinuxHeadlessOwnerUsesFixedSystemService(t *testing.T) {
	manifest, err := nativeOwnerManifestFor(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.ManagedFiles) != 1 ||
		manifest.ManagedFiles[0].Path !=
			"/etc/systemd/system/nvidia-pair-headless.service" {
		t.Fatalf("managed files = %#v", manifest.ManagedFiles)
	}
	content := manifest.ManagedFiles[0].Content
	for _, required := range []string{
		"User=TARGET_ACCOUNT",
		"Group=TARGET_GROUP",
		"RuntimeDirectory=nvpair-headless",
		"Environment=XDG_RUNTIME_DIR=/run/nvpair-headless",
		manifest.TUIPath + " --headless --broker-path " +
			manifest.BrokerPath,
	} {
		if !strings.Contains(content, required) {
			t.Fatalf("unit omits %q:\n%s", required, content)
		}
	}
	if strings.Contains(content, ".config/systemd") {
		t.Fatalf("unit uses per-user systemd path:\n%s", content)
	}
	if !reflect.DeepEqual(manifest.Start, []commandSpec{
		{Path: "/usr/bin/systemctl", Args: []string{"daemon-reload"}},
		{Path: "/usr/bin/systemctl", Args: []string{"enable", "--now", "nvidia-pair-headless.service"}},
	}) ||
		!reflect.DeepEqual(manifest.Stop, []commandSpec{{
			Path: "/usr/bin/systemctl",
			Args: []string{"disable", "--now", "nvidia-pair-headless.service"},
		}}) ||
		!reflect.DeepEqual(manifest.Remove, []commandSpec{{
			Path: "/usr/bin/systemctl",
			Args: []string{"daemon-reload"},
		}}) {
		t.Fatalf("system lifecycle start=%#v stop=%#v remove=%#v", manifest.Start, manifest.Stop, manifest.Remove)
	}
	for _, command := range append(
		append([]commandSpec{}, manifest.Start...),
		append(manifest.Stop, manifest.Remove...)...,
	) {
		if command.Path == "/usr/sbin/runuser" ||
			strings.Contains(
				strings.Join(command.Args, " "),
				"--user",
			) {
			t.Fatalf("user-bus command = %#v", command)
		}
	}
}

func TestDarwinHeadlessCombinesPlistAndLoadedJobIndependently(t *testing.T) {
	manifest, err := nativeOwnerManifestFor(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformDarwin,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	if err != nil {
		t.Fatal(err)
	}
	output := "system/" + manifest.HeadlessIdentity + " = {\n" +
		"program = " + manifest.TUIPath + "\n" +
		"arguments = {\n" +
		strings.Join(manifest.OwnerCommand, "\n") + "\n}\n" +
		"username = pairuser\nstate = running\n}\n"
	present, identity, lifecycle, valid := parseLaunchdJob(
		output,
		manifest.HeadlessIdentity,
		manifest.TUIPath,
		manifest.OwnerCommand,
		"pairuser",
	)
	if !present || !identity || !lifecycle || !valid {
		t.Fatalf("parsed launchd present=%t identity=%t lifecycle=%t valid=%t", present, identity, lifecycle, valid)
	}
	_, identity, _, valid = parseLaunchdJob(
		strings.Replace(output, "--headless\n--broker-path", "--broker-path\n--headless", 1),
		manifest.HeadlessIdentity,
		manifest.TUIPath,
		manifest.OwnerCommand,
		"pairuser",
	)
	if identity || !valid {
		t.Fatal("reordered launchd arguments were accepted")
	}
	_, _, _, valid = parseLaunchdJob(
		strings.Replace(
			output,
			"username = pairuser",
			"username = pairuser\nusername = pairuser",
			1,
		),
		manifest.HeadlessIdentity,
		manifest.TUIPath,
		manifest.OwnerCommand,
		"pairuser",
	)
	if valid {
		t.Fatal("duplicate launchd username was accepted")
	}
	_, _, _, valid = parseLaunchdJob(
		strings.Replace(output, "\n}\nusername", "\nusername", 1),
		manifest.HeadlessIdentity,
		manifest.TUIPath,
		manifest.OwnerCommand,
		"pairuser",
	)
	if valid {
		t.Fatal("truncated launchd argument block was accepted")
	}
	_, _, _, valid = parseLaunchdJob(
		"",
		manifest.HeadlessIdentity,
		manifest.TUIPath,
		manifest.OwnerCommand,
		"pairuser",
	)
	if valid {
		t.Fatal("empty successful launchd output was accepted")
	}
	if _, valid := parseLaunchdDisabledState(
		"disabled services = {\n\"com.nvidia.nvpair.headless\" => false\n",
		"com.nvidia.nvpair.headless",
	); valid {
		t.Fatal("unclosed print-disabled output was accepted")
	}
	exactJob := darwinHeadlessInspection{
		Present:        true,
		IdentityExact:  true,
		LifecycleExact: true,
	}
	present, identity, lifecycle = combineDarwinHeadlessInspection(
		false,
		false,
		exactJob,
	)
	if !present || !identity || lifecycle {
		t.Fatalf("job-only present=%t identity=%t lifecycle=%t", present, identity, lifecycle)
	}
	present, identity, lifecycle = combineDarwinHeadlessInspection(
		true,
		true,
		darwinHeadlessInspection{},
	)
	if !present || !identity || lifecycle {
		t.Fatalf("plist-only present=%t identity=%t lifecycle=%t", present, identity, lifecycle)
	}
	present, identity, lifecycle = combineDarwinHeadlessInspection(
		false,
		false,
		darwinHeadlessInspection{},
	)
	if present || !identity || lifecycle {
		t.Fatalf("absent present=%t identity=%t lifecycle=%t", present, identity, lifecycle)
	}
	exactJob.IdentityExact = false
	present, identity, lifecycle = combineDarwinHeadlessInspection(
		false,
		false,
		exactJob,
	)
	if !present || identity || lifecycle {
		t.Fatalf("foreign job present=%t identity=%t lifecycle=%t", present, identity, lifecycle)
	}
}
