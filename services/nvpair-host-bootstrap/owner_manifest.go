// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"

	"nvpair-shared/hostbootstrap"
)

type nativeOwnerManifest struct {
	HeadlessIdentity  string
	TUIPath           string
	BrokerPath        string
	OwnerCommand      []string
	DesktopIdentities []string
	DesktopStart      []commandSpec
	Start             []commandSpec
	Stop              []commandSpec
	Remove            []commandSpec
	Inspect           []commandSpec
	ManagedFiles      []managedFile
}

func nativeOwnerManifestFor(
	target hostbootstrap.Target,
) (nativeOwnerManifest, error) {
	if target.Architecture != hostbootstrap.ArchitectureAMD64 &&
		target.Architecture != hostbootstrap.ArchitectureARM64 {
		return nativeOwnerManifest{}, ErrUnsupportedTarget
	}
	switch target.Platform {
	case hostbootstrap.PlatformWindows:
		return windowsOwnerManifest(), nil
	case hostbootstrap.PlatformDarwin:
		return darwinOwnerManifest(), nil
	case hostbootstrap.PlatformLinux:
		return linuxOwnerManifest(), nil
	default:
		return nativeOwnerManifest{}, ErrUnsupportedTarget
	}
}

func windowsOwnerManifest() nativeOwnerManifest {
	const (
		helper = `C:\Program Files\NVIDIA Corporation\PAIR\nvpair-host-helper.exe`
		broker = `C:\Program Files\NVIDIA Corporation\PAIR\product\nvpair-ui-broker.exe`
	)
	owner := []string{helper, "headless-service"}
	binaryPath := `"` + helper + `" headless-service`
	return nativeOwnerManifest{
		HeadlessIdentity: "nvpair-headless",
		TUIPath:          helper,
		BrokerPath:       broker,
		OwnerCommand:     owner,
		DesktopIdentities: []string{
			`C:\Program Files\NVIDIA Corporation\PAIR\product\NVPAIR.exe`,
			broker,
		},
		Start: []commandSpec{
			{
				Path: `C:\Windows\System32\sc.exe`,
				Args: []string{"create", "nvpair-headless", "binPath=", binaryPath, "start=", "auto", "DisplayName=", "NVIDIA PAIR Headless"},
			},
			{Path: `C:\Windows\System32\sc.exe`, Args: []string{"start", "nvpair-headless"}},
		},
		Stop: []commandSpec{
			{Path: `C:\Windows\System32\sc.exe`, Args: []string{"stop", "nvpair-headless"}},
		},
		Remove: []commandSpec{
			{Path: `C:\Windows\System32\sc.exe`, Args: []string{"delete", "nvpair-headless"}},
		},
	}
}

func darwinOwnerManifest() nativeOwnerManifest {
	const (
		tui       = "/Applications/NVPAIR.app/Contents/Resources/cli-bin/nvpair-tui"
		broker    = "/Applications/NVPAIR.app/Contents/Resources/cli-bin/nvpair-ui-broker"
		plistPath = "/Library/LaunchDaemons/com.nvidia.nvpair.headless.plist"
	)
	owner := []string{tui, "--headless", "--broker-path", broker}
	arguments := "<string>" + strings.Join(owner, "</string><string>") + "</string>"
	return nativeOwnerManifest{
		HeadlessIdentity: "com.nvidia.nvpair.headless",
		TUIPath:          tui,
		BrokerPath:       broker,
		OwnerCommand:     owner,
		DesktopIdentities: []string{
			"/Applications/NVPAIR.app/Contents/MacOS/NVPAIR",
			broker,
		},
		Start: []commandSpec{
			{Path: "/bin/launchctl", Args: []string{"bootstrap", "system", plistPath}},
			{Path: "/bin/launchctl", Args: []string{"kickstart", "-k", "system/com.nvidia.nvpair.headless"}},
		},
		Stop: []commandSpec{
			{Path: "/bin/launchctl", Args: []string{"bootout", "system/com.nvidia.nvpair.headless"}},
		},
		Inspect: []commandSpec{
			{Path: "/bin/launchctl", Args: []string{"print", "system/com.nvidia.nvpair.headless"}},
		},
		ManagedFiles: []managedFile{{
			Path: plistPath,
			Mode: 0600,
			Content: "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
				"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
				"<plist version=\"1.0\"><dict>\n" +
				"<key>Label</key><string>com.nvidia.nvpair.headless</string>\n" +
				"<key>UserName</key><string>TARGET_ACCOUNT</string>\n" +
				"<key>ProgramArguments</key><array>" + arguments + "</array>\n" +
				"<key>KeepAlive</key><true/><key>RunAtLoad</key><true/>\n" +
				"</dict></plist>\n",
		}},
	}
}

func linuxOwnerManifest() nativeOwnerManifest {
	const (
		tui      = "/opt/nvpair/product/nvpair-tui"
		broker   = "/opt/nvpair/product/nvpair-ui-broker"
		unit     = "nvidia-pair-headless.service"
		unitPath = "/etc/systemd/system/" + unit
	)
	owner := []string{tui, "--headless", "--broker-path", broker}
	return nativeOwnerManifest{
		HeadlessIdentity: unit,
		TUIPath:          tui,
		BrokerPath:       broker,
		OwnerCommand:     owner,
		DesktopIdentities: []string{
			"/opt/nvpair/product/nvpair",
			broker,
		},
		Start: []commandSpec{
			{Path: "/usr/bin/systemctl", Args: []string{"daemon-reload"}},
			{Path: "/usr/bin/systemctl", Args: []string{"enable", "--now", unit}},
		},
		Stop: []commandSpec{
			{Path: "/usr/bin/systemctl", Args: []string{"disable", "--now", unit}},
		},
		Remove: []commandSpec{
			{Path: "/usr/bin/systemctl", Args: []string{"daemon-reload"}},
		},
		Inspect: []commandSpec{
			{
				Path: "/usr/bin/systemctl",
				Args: []string{
					"show",
					unit,
					"--property=LoadState",
					"--property=ActiveState",
					"--property=UnitFileState",
					"--property=FragmentPath",
					"--property=DropInPaths",
					"--property=ExecStart",
					"--property=User",
					"--property=Group",
					"--property=Environment",
				},
			},
		},
		ManagedFiles: []managedFile{{
			Path: unitPath,
			Mode: 0600,
			Content: "[Unit]\nDescription=NVIDIA Personal AI Router headless backend\nAfter=network.target\n\n" +
				"[Service]\nType=simple\nUser=TARGET_ACCOUNT\nGroup=TARGET_GROUP\n" +
				"RuntimeDirectory=nvpair-headless\nRuntimeDirectoryMode=0700\n" +
				"Environment=XDG_RUNTIME_DIR=/run/nvpair-headless\n" +
				"ExecStart=" + tui + " --headless --broker-path " + broker + "\n" +
				"Restart=on-failure\nRestartSec=2\nTimeoutStopSec=25\nKillMode=mixed\nUMask=0077\nNoNewPrivileges=true\nPrivateTmp=true\n\n" +
				"[Install]\nWantedBy=multi-user.target\n",
		}},
	}
}
