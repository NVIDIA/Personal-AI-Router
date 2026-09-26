// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
//go:build !windows

package main

import "path/filepath"

func onboardingLocalArtifactPath(file string) bool { return filepath.IsAbs(file) }
