// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io/fs"

	"nvpair-shared/hostbootstrap"
)

const (
	windowsStateDirectory = `%ProgramData%\NVIDIA Corporation\Personal AI Router\host-bootstrap`
	darwinStateDirectory  = "/Library/Application Support/NVIDIA/Personal AI Router/host-bootstrap"
	linuxStateDirectory   = "/var/lib/nvpair/host-bootstrap"

	windowsProductPath = `C:\Program Files\NVIDIA Corporation\PAIR\product`
	windowsHelperPath  = `C:\Program Files\NVIDIA Corporation\PAIR\nvpair-host-helper.exe`
	windowsSSHDPath    = `C:\Windows\System32\OpenSSH\sshd.exe`
	darwinProductPath  = "/Applications/NVPAIR.app"
	darwinHelperPath   = "/Library/PrivilegedHelperTools/nvpair-host-helper"
	linuxProductPath   = "/opt/nvpair/product"
	linuxHelperPath    = "/usr/libexec/nvpair-host-helper"
)

type commandSpec struct {
	Path string
	Args []string
}

type localEndpoint struct {
	Address string
	DACL    string
	Mode    fs.FileMode
	Owner   string
	Group   string
}

type managedFile struct {
	Path    string
	Content string
	Mode    fs.FileMode
}

type nativeDefinition struct {
	StateDirectory string
	Local          localEndpoint
	Apply          map[hostbootstrap.ResourceKind][]commandSpec
	Remove         map[hostbootstrap.ResourceKind][]commandSpec
	ManagedFiles   []managedFile
}

func nativeDefinitionFor(
	target hostbootstrap.Target,
	distro string,
) (nativeDefinition, error) {
	if target.Architecture != hostbootstrap.ArchitectureAMD64 &&
		target.Architecture != hostbootstrap.ArchitectureARM64 {
		return nativeDefinition{}, ErrUnsupportedTarget
	}
	switch target.Platform {
	case hostbootstrap.PlatformWindows:
		return windowsNativeDefinition(), nil
	case hostbootstrap.PlatformDarwin:
		return darwinNativeDefinition(), nil
	case hostbootstrap.PlatformLinux:
		if distro != "debian" && distro != "ubuntu" {
			return nativeDefinition{}, ErrUnsupportedTarget
		}
		return linuxNativeDefinition(), nil
	default:
		return nativeDefinition{}, ErrUnsupportedTarget
	}
}

func validateNativeBinding(binding hostbootstrap.Binding) error {
	var productPath, helperPath string
	switch binding.Target.Platform {
	case hostbootstrap.PlatformWindows:
		productPath, helperPath = windowsProductPath, windowsHelperPath
	case hostbootstrap.PlatformDarwin:
		productPath, helperPath = darwinProductPath, darwinHelperPath
	case hostbootstrap.PlatformLinux:
		productPath, helperPath = linuxProductPath, linuxHelperPath
	default:
		return ErrUnsupportedTarget
	}
	if binding.Endpoint.Port != 22 ||
		binding.Product.ID != "nvpair" ||
		binding.Product.Path != productPath ||
		binding.Helper.ID != "nvpair-host-helper" ||
		binding.Helper.Path != helperPath {
		return ErrUnsupportedIdentity
	}
	return nil
}

func windowsNativeDefinition() nativeDefinition {
	return nativeDefinition{
		StateDirectory: windowsStateDirectory,
		Local: localEndpoint{
			Address: `\\.\pipe\nvpair-host-helper`,
			DACL:    `O:SYG:SYD:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;TARGET_SID)`,
		},
		Apply: map[hostbootstrap.ResourceKind][]commandSpec{
			hostbootstrap.ResourceSSHService: {
				{
					Path: `C:\Windows\System32\dism.exe`,
					Args: []string{"/Online", "/Add-Capability", "/CapabilityName:OpenSSH.Server~~~~0.0.1.0", "/NoRestart"},
				},
				{
					Path: `C:\Windows\System32\sc.exe`,
					Args: []string{"config", "sshd", "start=", "auto"},
				},
				{
					Path: `C:\Windows\System32\sc.exe`,
					Args: []string{"start", "sshd"},
				},
			},
			hostbootstrap.ResourceHelper: {
				{
					Path: `C:\Windows\System32\sc.exe`,
					Args: []string{"create", "nvpair-host-helper", "binPath=", `"C:\Program Files\NVIDIA Corporation\PAIR\nvpair-host-helper.exe" service`, "start=", "auto", "DisplayName=", "NVIDIA PAIR Host Helper"},
				},
				{
					Path: `C:\Windows\System32\sc.exe`,
					Args: []string{"start", "nvpair-host-helper"},
				},
			},
		},
		Remove: map[hostbootstrap.ResourceKind][]commandSpec{
			hostbootstrap.ResourceSSHService: {
				{Path: `C:\Windows\System32\sc.exe`, Args: []string{"stop", "sshd"}},
				{Path: `C:\Windows\System32\dism.exe`, Args: []string{"/Online", "/Remove-Capability", "/CapabilityName:OpenSSH.Server~~~~0.0.1.0", "/NoRestart"}},
			},
			hostbootstrap.ResourceHelper: {
				{Path: `C:\Windows\System32\sc.exe`, Args: []string{"stop", "nvpair-host-helper"}},
				{Path: `C:\Windows\System32\sc.exe`, Args: []string{"delete", "nvpair-host-helper"}},
			},
		},
	}
}

func darwinNativeDefinition() nativeDefinition {
	const launchdPath = "/Library/LaunchDaemons/com.nvidia.nvpair.host-helper.plist"
	return nativeDefinition{
		StateDirectory: darwinStateDirectory,
		Local: localEndpoint{
			Address: "/var/run/nvpair-host-helper.sock",
			Mode:    0660,
			Owner:   "root",
			Group:   "TARGET_GID",
		},
		Apply: map[hostbootstrap.ResourceKind][]commandSpec{
			hostbootstrap.ResourceSSHService: {
				{Path: "/usr/sbin/systemsetup", Args: []string{"-setremotelogin", "on"}},
			},
			hostbootstrap.ResourceHelper: {
				{Path: "/bin/launchctl", Args: []string{"bootstrap", "system", launchdPath}},
				{Path: "/bin/launchctl", Args: []string{"kickstart", "-k", "system/com.nvidia.nvpair.host-helper"}},
			},
		},
		Remove: map[hostbootstrap.ResourceKind][]commandSpec{
			hostbootstrap.ResourceSSHService: {
				{Path: "/usr/sbin/systemsetup", Args: []string{"-setremotelogin", "off"}},
			},
			hostbootstrap.ResourceHelper: {
				{Path: "/bin/launchctl", Args: []string{"bootout", "system/com.nvidia.nvpair.host-helper"}},
			},
		},
		ManagedFiles: []managedFile{{
			Path: launchdPath,
			Mode: 0600,
			Content: "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
				"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
				"<plist version=\"1.0\"><dict>\n" +
				"<key>Label</key><string>com.nvidia.nvpair.host-helper</string>\n" +
				"<key>ProgramArguments</key><array><string>/Library/PrivilegedHelperTools/nvpair-host-helper</string><string>service</string></array>\n" +
				"<key>KeepAlive</key><true/><key>RunAtLoad</key><true/>\n" +
				"</dict></plist>\n",
		}},
	}
}

func linuxNativeDefinition() nativeDefinition {
	const (
		servicePath = "/etc/systemd/system/nvpair-host-helper.service"
		socketPath  = "/etc/systemd/system/nvpair-host-helper.socket"
	)
	return nativeDefinition{
		StateDirectory: linuxStateDirectory,
		Local: localEndpoint{
			Address: "/run/nvpair-host-helper.sock",
			Mode:    0660,
			Owner:   "root",
			Group:   "TARGET_GID",
		},
		Apply: map[hostbootstrap.ResourceKind][]commandSpec{
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
		Remove: map[hostbootstrap.ResourceKind][]commandSpec{
			hostbootstrap.ResourceSSHService: {
				{Path: "/usr/bin/systemctl", Args: []string{"disable", "--now", "ssh"}},
				{Path: "/usr/bin/apt-get", Args: []string{"remove", "-y", "openssh-server"}},
			},
			hostbootstrap.ResourceHelper: {
				{Path: "/usr/bin/systemctl", Args: []string{"disable", "--now", "nvpair-host-helper.socket", "nvpair-host-helper.service"}},
				{Path: "/usr/bin/systemctl", Args: []string{"daemon-reload"}},
			},
		},
		ManagedFiles: []managedFile{
			{
				Path: servicePath,
				Mode: 0600,
				Content: "[Unit]\nDescription=NVIDIA PAIR Host Helper\nRequires=nvpair-host-helper.socket\nAfter=nvpair-host-helper.socket\n\n" +
					"[Service]\nType=simple\nExecStart=/usr/libexec/nvpair-host-helper service\nUser=root\nGroup=root\nNoNewPrivileges=true\nPrivateTmp=true\nProtectSystem=strict\nProtectHome=false\n" +
					"ReadOnlyPaths=/home /root /usr/libexec/nvpair-host-bootstrap /usr/libexec/payload\n" +
					"ReadWritePaths=/var/lib/nvpair/host-bootstrap /opt/nvpair /etc/ssh /etc/ufw /var/lib/ufw -/etc/systemd/system/nvpair-host-helper.service -/etc/systemd/system/nvpair-host-helper.socket -/etc/systemd/system/nvidia-pair-headless.service -TARGET_PRODUCT -TARGET_HELPER -TARGET_HOME/.ssh -TARGET_AUTHORIZED_KEYS_DIR -TARGET_AUTHORIZED_KEYS\n\n",
			},
			{
				Path: socketPath,
				Mode: 0600,
				Content: "[Unit]\nDescription=NVIDIA PAIR Host Helper Socket\n\n" +
					"[Socket]\nListenStream=/run/nvpair-host-helper.sock\nSocketMode=0660\nSocketUser=root\nSocketGroup=TARGET_GROUP\nRemoveOnStop=true\n\n" +
					"[Install]\nWantedBy=sockets.target\n",
			},
		},
	}
}
