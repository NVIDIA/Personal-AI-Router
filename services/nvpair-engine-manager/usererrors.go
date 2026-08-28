// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"regexp"
	"strings"
)

var exitStatusPrefixRe = regexp.MustCompile(`^exit status \d+: `)

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
