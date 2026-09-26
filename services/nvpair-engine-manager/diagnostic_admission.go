// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "fmt"

func (e *Executor) admitDiagnosticMutation(effect string) (func(), error) {
	e.diagnosticMu.RLock()
	if e.diagnostics != nil && e.diagnostics.reserved() {
		e.diagnosticMu.RUnlock()
		return nil, fmt.Errorf("a collective diagnostic reservation prevents %s", effect)
	}
	return e.diagnosticMu.RUnlock, nil
}

// Callers hold st.opMu until release. Only synchronous nested work by that
// same owner may reuse this guard, including rollback with a fresh context.
// Nothing is stored in a context or retained after the outer release.
func (e *Executor) admitEngineMutation(st *engineState, effect string) (func(), error) {
	if err := rejectVLLMGroupOwnerMutation(st, effect); err != nil {
		return nil, err
	}
	if st.diagnosticAdmitted {
		return func() {}, nil
	}
	release, err := e.admitDiagnosticMutation(effect)
	if err != nil {
		return nil, err
	}
	st.diagnosticAdmitted = true
	return func() {
		st.diagnosticAdmitted = false
		release()
	}, nil
}
