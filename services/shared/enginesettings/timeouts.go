// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package enginesettings

import "time"

// Settings calls wait for stop, rebind, and readiness. The longest bundled
// probe is vLLM's, which permits thirty minutes; configure adds time for stop
// and rebind, and each caller leaves headroom for the layer it waits on.
// Desktop's outer
// envelope is thirty-four minutes (modular-runtime.ts).
const (
	ConfigureBudget = 30*time.Minute + 30*time.Second
	OperationBudget = ConfigureBudget + 30*time.Second
	CallBudget      = OperationBudget + time.Minute
	RelayBudget     = CallBudget + time.Minute
)
