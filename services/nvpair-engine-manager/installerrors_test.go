// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLMSInstallFailure_ClassifiesDownloadDiagnostics(t *testing.T) {
	test := func(name string, err error, want string) {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, want, classifyLMSInstallFailure(err))
		})
	}
	test("checksum variable at reported line", errors.New("install.sh: line 228: _checksum_value: unbound variable"), "checksum-download")
	test("checksum variable at different line", errors.New("install.sh: line 213: _checksum_value: unbound variable"), "checksum-download")
	test("timeout precedes checksum variable", errors.New("curl: (28) Operation timed out\n_checksum_value: unbound variable"), "timeout")
	test("wget timeout", errors.New("wget: connection timed out"), "timeout")
	test("downloader reset", errors.New("curl: (56) Recv failure: Connection reset by peer"), "network")
	test("wrapped timeout", fmt.Errorf("fetch: %w", context.DeadlineExceeded), "timeout")
	test("temporary DNS", &net.DNSError{IsTemporary: true}, "network")
	test("wrapped reset", fmt.Errorf("fetch: %w", syscall.ECONNRESET), "network")
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		test(fmt.Sprintf("HTTP %d", status), &downloadHTTPError{status: status}, "network")
	}
	test("HTTP request timeout", &downloadHTTPError{status: http.StatusRequestTimeout}, "timeout")
	test("HTTP not found", &downloadHTTPError{status: http.StatusNotFound}, "")
	test("permanent DNS", &net.DNSError{IsNotFound: true}, "")
	test("cancelled", context.Canceled, "")
	test("checksum mismatch overrides network wording", errors.New("checksum mismatch: download failed: timed out"), "")
	test("permission denied", errors.New("permission denied"), "")
	test("missing dependency", errors.New("need curl (command not found)"), "")
	test("unsupported platform", errors.New("no compatible downloads were found"), "")
	test("unrelated timeout", errors.New("install timeout configuring download directory"), "")
	test("different variable", errors.New("_other_value: unbound variable"), "")
	test("nil", nil, "")
}

func TestLMSInstallFailure_FormatsActionableMessages(t *testing.T) {
	for _, tc := range []struct{ kind, want string }{
		{"timeout", "LM Studio download timed out after 3 attempts. Check your connection and try again."},
		{"checksum-download", "LM Studio’s installer could not download its checksum after 3 attempts. Check your connection and try again."},
		{"network", "LM Studio download failed after 3 attempts. Check your connection and try again."},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			cause := errors.New("internal installer diagnostic")
			err := &lmsInstallFailure{cause: cause, kind: tc.kind, attempts: 3}
			require.EqualError(t, err, tc.want)
			require.ErrorIs(t, err, cause)
		})
	}
}

func TestInstallFailure_SharesFormattedReasonAcrossOutputs(t *testing.T) {
	ex := NewExecutor(NewRegistry(), NewReporter(nil), func(string, any) {}, t.TempDir())
	var failure string
	ex.emit = func(_ string, params any) { failure = params.(map[string]any)["error"].(string) }
	err := ex.reportInstallFailed("lmstudio", errors.New("curl: (28) Operation timed out"))
	require.EqualError(t, err, "LM Studio download timed out. Check your connection and try again.")
	require.Equal(t, err.Error(), failure)
	require.Equal(t, err.Error(), ex.reporter.snapshot()[0].Message)
}
