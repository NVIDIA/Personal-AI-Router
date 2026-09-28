// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package hostbootstrap defines and validates the closed, target-local contract
// for an idempotent PAIR and SSH bootstrap. It is platform-neutral and has no
// side effects, process ownership, caller-controlled commands, or secret-bearing
// fields. Its helper wire admits only the package's fixed action enum.
package hostbootstrap
