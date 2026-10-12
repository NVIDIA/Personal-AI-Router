// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package enginesettings

import "time"

// Settings calls wait for stop, rebind, and readiness. The bundled Ollama
// probe permits ten minutes; configure adds time for stop and rebind, and
// each caller leaves headroom for the layer it waits on. Desktop's outer
// envelope is fourteen minutes (modular-runtime.ts).
const (
	ConfigureBudget = 10*time.Minute + 30*time.Second
	OperationBudget = ConfigureBudget + 30*time.Second
	CallBudget      = OperationBudget + time.Minute
	RelayBudget     = CallBudget + time.Minute
)
