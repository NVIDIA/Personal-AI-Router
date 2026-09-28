// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"errors"
	"testing"
)

func TestVLLMPostPolicyFailureCodes(t *testing.T) {
	for _, code := range []string{"policy_record_write_failed", "release_send_failed", "supervisor_identity_missing", "supervisor_identity_unavailable", "supervisor_identity_invalid"} {
		err := classifyVLLMRankProcess(errors.New("unretained process error"), 0, []byte("PAIR_RANK_FAILURE:"+code))
		var typed *vllmRankStartError
		if !errors.As(err, &typed) || !validVLLMRankStartFailure(&typed.Failure) || typed.Failure.Code != code || typed.Failure.StderrCode != "worker_rejected" {
			t.Fatal("post-policy code lost or rejected", code)
		}
	}
}
