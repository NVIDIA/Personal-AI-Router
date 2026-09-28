// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// The current B/C profile is bound to the pinned read-only matrix receipt at
// ops-harness/runs/20260923T064644Z-qwen-provider-matrix-ddd0/20-result.json
// (SHA-256 c7c4ed3904a7ffe6a64b1c9b8cdd97910d8a106bd0cd103ca9b034830deb46c9).
// It is a separate closed profile, never a rewrite of the sealed A reference.

// The DGX OS 7.6 A/B/C profile is bound to the pinned read-only matrix receipt at
// ops-harness/runs/20260923T135408Z-qwen-provider-matrix-968/20-result.json
// (SHA-256 9e6dd1b12ff8387109aa826091c84aacb490ff1aa338c6abace02436404743c7).
// It is additive; the historical A and B/C profiles remain admitted unchanged.

const (
	vllmQwen38ProviderProfileA     = "spark-a-libc8.6-r580.95.05"
	vllmQwen38ProviderProfileBC    = "spark-bc-libc8.8-r580.173.02"
	vllmQwen38ProviderClosureBC    = "0265d190e9bd5c90ac4492a1c811056dc4b0ed6814bd797c5d65cac075f5d3c2"
	vllmQwen38ProviderProfileDGX76 = "spark-abc-dgxos7.6-libc8.9-r580.178.04"
	vllmQwen38ProviderClosureDGX76 = "eb64849ee23f29a140f33630fb2d4b95e59e6acff8069a5426e27f461ca85329"
)

type vllmQwen38ProviderPackage struct{ Name, Identity string }

type vllmQwen38ProviderSpec struct {
	Name, Path, SHA256 string
	Bytes              int64
	Packages           []vllmQwen38ProviderPackage
}

type vllmQwen38ProviderProfile struct {
	ID, ClosureSHA256 string
	Providers         []vllmQwen38ProviderSpec
}

func qwen38ReferenceProviderProfile(recipe vllmQwen38Recipe) vllmQwen38ProviderProfile {
	profile := vllmQwen38ProviderProfile{ID: vllmQwen38ProviderProfileA, ClosureSHA256: vllmQwen38ProviderClosureSHA256}
	for _, provider := range recipe.HostQualification.Providers {
		spec := vllmQwen38ProviderSpec{Name: provider.Name, Path: provider.Path, SHA256: provider.SHA256, Bytes: provider.Bytes}
		for _, pkg := range provider.Packages {
			spec.Packages = append(spec.Packages, vllmQwen38ProviderPackage{Name: pkg.Name, Identity: pkg.Identity})
		}
		profile.Providers = append(profile.Providers, spec)
	}
	return profile
}

func qwen38CurrentBCProviderProfile() vllmQwen38ProviderProfile {
	pkg := func(name, identity string) []vllmQwen38ProviderPackage {
		return []vllmQwen38ProviderPackage{{Name: name, Identity: identity}}
	}
	return vllmQwen38ProviderProfile{ID: vllmQwen38ProviderProfileBC, ClosureSHA256: vllmQwen38ProviderClosureBC, Providers: []vllmQwen38ProviderSpec{
		{Name: "libc.so.6", Path: "/usr/lib/aarch64-linux-gnu/libc.so.6", SHA256: "6e3cc56b98887cb3cc2a9fe78b6dd4610184aa27bd05d592cb287db93e82d494", Bytes: 1722920, Packages: pkg("libc6:arm64", "install ok installed|2.39-0ubuntu8.8|arm64")},
		{Name: "libcuda.so.1", Path: "/usr/lib/aarch64-linux-gnu/libcuda.so.580.173.02", SHA256: "a13f3101b105930ba0796b1ab99721cbee3e19360ced21253e2a09e1a102fb99", Bytes: 96460560, Packages: pkg("libnvidia-compute-580:arm64", "install ok installed|580.173.02-0ubuntu0.24.04.1|arm64")},
		{Name: "libdl.so.2", Path: "/usr/lib/aarch64-linux-gnu/libdl.so.2", SHA256: "f196e378b444da9e982e8930e1595e9ad6a723d890e0647bc60d8fcf581a93c8", Bytes: 67440, Packages: pkg("libc6:arm64", "install ok installed|2.39-0ubuntu8.8|arm64")},
		{Name: "libgcc_s.so.1", Path: "/usr/lib/aarch64-linux-gnu/libgcc_s.so.1", SHA256: "f2d3ad2bf0b61f6bc944cc37d7b6ab7f88d2582b41ff989ca164803f56cc5f20", Bytes: 133696, Packages: pkg("libgcc-s1:arm64", "install ok installed|14.2.0-4ubuntu2~24.04.1|arm64")},
		{Name: "libgomp.so.1", Path: "/usr/lib/aarch64-linux-gnu/libgomp.so.1.0.0", SHA256: "33c8bcb7d33228fe5101bfeb201bcaff4563d4ecf09497229b9deba522ebaf3d", Bytes: 397088, Packages: pkg("libgomp1:arm64", "install ok installed|14.2.0-4ubuntu2~24.04.1|arm64")},
		{Name: "libm.so.6", Path: "/usr/lib/aarch64-linux-gnu/libm.so.6", SHA256: "24be585ff27080ba6301d8b3e1c92a54a3832829a9efde744cad4b876a73681a", Bytes: 591800, Packages: pkg("libc6:arm64", "install ok installed|2.39-0ubuntu8.8|arm64")},
		{Name: "libpthread.so.0", Path: "/usr/lib/aarch64-linux-gnu/libpthread.so.0", SHA256: "1ce5833f596169e194b95a6f288c98d80b7b907a1ded2389746cd00235b3a456", Bytes: 67440, Packages: pkg("libc6:arm64", "install ok installed|2.39-0ubuntu8.8|arm64")},
		{Name: "librt.so.1", Path: "/usr/lib/aarch64-linux-gnu/librt.so.1", SHA256: "8e38ca286d0bbb7acc9a6741ecd26c6e76924ec69eb9196ee20f70fa91350e12", Bytes: 67512, Packages: pkg("libc6:arm64", "install ok installed|2.39-0ubuntu8.8|arm64")},
		{Name: "libstdc++.so.6", Path: "/usr/lib/aarch64-linux-gnu/libstdc++.so.6.0.33", SHA256: "6e3112d35cfc86db7ee85b27e1746f67408e2837ff8628b46386c3eabd5682a4", Bytes: 2633224, Packages: pkg("libstdc++6:arm64", "install ok installed|14.2.0-4ubuntu2~24.04.1|arm64")},
	}}
}

func qwen38DGX76ProviderProfile() vllmQwen38ProviderProfile {
	pkg := func(name, identity string) []vllmQwen38ProviderPackage {
		return []vllmQwen38ProviderPackage{{Name: name, Identity: identity}}
	}
	return vllmQwen38ProviderProfile{ID: vllmQwen38ProviderProfileDGX76, ClosureSHA256: vllmQwen38ProviderClosureDGX76, Providers: []vllmQwen38ProviderSpec{
		{Name: "libc.so.6", Path: "/usr/lib/aarch64-linux-gnu/libc.so.6", SHA256: "0f1905dc27dbc6715875c70527bc8da846a10084c9c483e9d8ff76dce8d69d0e", Bytes: 1722920, Packages: pkg("libc6:arm64", "install ok installed|2.39-0ubuntu8.9|arm64")},
		{Name: "libcuda.so.1", Path: "/usr/lib/aarch64-linux-gnu/libcuda.so.580.178.04", SHA256: "b11359b073ae9e49df81dc4f5c156378bd97fb9f6197ef64edc46e22c670bed4", Bytes: 96460560, Packages: pkg("libnvidia-compute-580:arm64", "install ok installed|580.178.04-0ubuntu0.24.04.1|arm64")},
		{Name: "libdl.so.2", Path: "/usr/lib/aarch64-linux-gnu/libdl.so.2", SHA256: "1c16b2692a155191c97d7fdc5ce937d37c242435f05745046a794a9832b16af4", Bytes: 67440, Packages: pkg("libc6:arm64", "install ok installed|2.39-0ubuntu8.9|arm64")},
		{Name: "libgcc_s.so.1", Path: "/usr/lib/aarch64-linux-gnu/libgcc_s.so.1", SHA256: "f2d3ad2bf0b61f6bc944cc37d7b6ab7f88d2582b41ff989ca164803f56cc5f20", Bytes: 133696, Packages: pkg("libgcc-s1:arm64", "install ok installed|14.2.0-4ubuntu2~24.04.1|arm64")},
		{Name: "libgomp.so.1", Path: "/usr/lib/aarch64-linux-gnu/libgomp.so.1.0.0", SHA256: "33c8bcb7d33228fe5101bfeb201bcaff4563d4ecf09497229b9deba522ebaf3d", Bytes: 397088, Packages: pkg("libgomp1:arm64", "install ok installed|14.2.0-4ubuntu2~24.04.1|arm64")},
		{Name: "libm.so.6", Path: "/usr/lib/aarch64-linux-gnu/libm.so.6", SHA256: "5e0391562de36de323fb9cb0f2c1b6bd6d034c840b1dac018499f5fe4ded1b4a", Bytes: 591800, Packages: pkg("libc6:arm64", "install ok installed|2.39-0ubuntu8.9|arm64")},
		{Name: "libpthread.so.0", Path: "/usr/lib/aarch64-linux-gnu/libpthread.so.0", SHA256: "6f1ab9e7d7de142dc5e2feaf6d19faceff0a0f40d5e225c922899f5362b8c353", Bytes: 67440, Packages: pkg("libc6:arm64", "install ok installed|2.39-0ubuntu8.9|arm64")},
		{Name: "librt.so.1", Path: "/usr/lib/aarch64-linux-gnu/librt.so.1", SHA256: "93d0d5ab643b50ac1a61c2c7baf670e8aa94973fa5a2584d13db05295d5b5a0e", Bytes: 67512, Packages: pkg("libc6:arm64", "install ok installed|2.39-0ubuntu8.9|arm64")},
		{Name: "libstdc++.so.6", Path: "/usr/lib/aarch64-linux-gnu/libstdc++.so.6.0.33", SHA256: "6e3112d35cfc86db7ee85b27e1746f67408e2837ff8628b46386c3eabd5682a4", Bytes: 2633224, Packages: pkg("libstdc++6:arm64", "install ok installed|14.2.0-4ubuntu2~24.04.1|arm64")},
	}}
}

func qwen38ProviderProfiles(recipe vllmQwen38Recipe) []vllmQwen38ProviderProfile {
	return []vllmQwen38ProviderProfile{qwen38ReferenceProviderProfile(recipe), qwen38CurrentBCProviderProfile(), qwen38DGX76ProviderProfile()}
}

func knownQwen38ProviderClosure(value string) bool {
	return value == vllmQwen38ProviderClosureSHA256 || value == vllmQwen38ProviderClosureBC || value == vllmQwen38ProviderClosureDGX76
}
