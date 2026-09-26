// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

// Cable and temporary fabric setup are Linux-only. Other supported PAIR hosts
// keep the field absent instead of fabricating native physical-port facts.
func observeConnections() *ConnectionInfo { return nil }
