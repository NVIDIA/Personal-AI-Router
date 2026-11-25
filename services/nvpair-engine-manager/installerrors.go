// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
)

type downloadHTTPError struct{ status int }

func (e *downloadHTTPError) Error() string { return fmt.Sprintf("HTTP %d", e.status) }

var checksumVariableError = regexp.MustCompile(`_checksum_value: (?:unbound variable|parameter not set)`)

// lmsInstallFailure keeps the diagnostic available without exposing vendor
// script internals in the message shared by RPC, progress, and errors:report.
type lmsInstallFailure struct {
	cause    error
	kind     string
	attempts int
}

func (e *lmsInstallFailure) Unwrap() error { return e.cause }

func (e *lmsInstallFailure) Error() string {
	message := "LM Studio download failed"
	switch e.kind {
	case "timeout":
		message = "LM Studio download timed out"
	case "checksum-download":
		message = "LM Studio’s installer could not download its checksum"
	}
	if e.attempts > 1 {
		message += fmt.Sprintf(" after %d attempts", e.attempts)
	}
	return message + ". Check your connection and try again."
}

func classifyLMSInstallFailure(err error) string {
	if err == nil || errors.Is(err, context.Canceled) {
		return ""
	}
	detail := strings.ToLower(err.Error())
	// Never let incidental network text make an integrity failure retryable.
	if strings.Contains(detail, "checksum mismatch") {
		return ""
	}
	var networkError net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout()) {
		return "timeout"
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) && dnsError.IsTemporary {
		return "network"
	}
	if isInstallConnectionReset(err) {
		return "network"
	}
	var httpError *downloadHTTPError
	if errors.As(err, &httpError) {
		switch httpError.status {
		case http.StatusRequestTimeout:
			return "timeout"
		case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return "network"
		}
	}
	// Only downloader diagnostics qualify; unrelated installer failures may
	// contain words such as "download" or "timeout" too.
	for _, line := range strings.Split(detail, "\n") {
		if !strings.Contains(line, "curl:") && !strings.Contains(line, "wget:") && !strings.Contains(line, "download failed:") {
			continue
		}
		if strings.Contains(line, "timed out") || strings.Contains(line, "timed-out") || strings.Contains(line, "curl: (28)") {
			return "timeout"
		}
		if strings.Contains(line, "connection reset") || strings.Contains(line, "forcibly closed by the remote host") {
			return "network"
		}
	}
	if checksumVariableError.MatchString(detail) {
		return "checksum-download"
	}
	return ""
}
