// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

var exitStatusPrefixRe = regexp.MustCompile(`^exit status \d+: `)

// Classify bounded vendor stderr without publishing cache paths, URLs, tokens,
// or arbitrary third-party text through the local/paired error log.
func llamaDownloadError(err error) error {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return fmt.Errorf("cannot start llama downloader: %w", err)
	}
	detail := exit.Stderr
	if len(detail) > 16384 {
		detail = detail[:16384]
	}
	s := strings.ToLower(string(detail))
	message := "the vendor downloader exited unsuccessfully"
	switch {
	case strings.Contains(s, "no space left"), strings.Contains(s, "disk full"):
		message = "the model cache disk is full"
	case strings.Contains(s, "error opening"), strings.Contains(s, "failed to open"), strings.Contains(s, "cannot open"):
		message = "cannot open the model cache file; check free space, path length and write permissions"
	case strings.Contains(s, "401"), strings.Contains(s, "403"):
		message = "the model source refused access; check repository access and authentication"
	case strings.Contains(s, "404"), strings.Contains(s, "not found"):
		message = "the requested repository, quantization or model file was not found"
	case strings.Contains(s, "certificate"), strings.Contains(s, "ssl"):
		message = "TLS verification failed while contacting the model source"
	case strings.Contains(s, "timed out"), strings.Contains(s, "timeout"):
		message = "the model source or network timed out; the download can be retried"
	}
	return fmt.Errorf("llama model download failed: %s", message)
}

// formatEnginePullError renders a user-facing message for a model-pull failure
// attributable to the engine (CLI stderr, engine HTTP response, etc.).
func formatEnginePullError(displayName string, err error) string {
	detail := unwrapPullCause(err)
	if detail == "" {
		return fmt.Sprintf("%s experienced an error while downloading a model.", displayName)
	}
	return fmt.Sprintf("%s experienced an error while downloading a model: %s", displayName, detail)
}

// unwrapPullCause strips internal wrappers so the engine's own diagnostic text
// is shown to the user.
func unwrapPullCause(err error) string {
	if err == nil {
		return ""
	}
	s := strings.TrimSpace(err.Error())
	const actionFailed = "action command failed: "
	if strings.HasPrefix(s, actionFailed) {
		s = strings.TrimPrefix(s, actionFailed)
	}
	s = exitStatusPrefixRe.ReplaceAllString(s, "")
	s = strings.TrimPrefix(s, "Error: ")
	return strings.TrimSpace(s)
}
