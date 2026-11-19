// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	launchTextFormat   = "pair-arguments-v1"
	maxLaunchTextBytes = 16 * 1024
	maxLaunchTokens    = 256
)

// parseLaunchText tokenizes PAIR's platform-independent launch notation. It
// never expands variables, paths or placeholders and never invokes a shell.
// Single quotes are literal; within double quotes a backslash only escapes
// another backslash or a double quote. Other backslashes stay literal.
// Quoted and unquoted segments may share a token (--flag="two words").
// Adapters declare the managed port/CORS controls checked after tokenization.
// Other argument meanings belong to the engine. Empty text means no user options.
func parseLaunchText(text string) ([]string, error) {
	if len(text) > maxLaunchTextBytes {
		return nil, fmt.Errorf("launch text exceeds %d bytes", maxLaunchTextBytes)
	}
	if !utf8.ValidString(text) {
		return nil, fmt.Errorf("launch text must be valid UTF-8")
	}
	for _, r := range text {
		if unicode.IsControl(r) && r != '\t' && r != '\r' && r != '\n' {
			return nil, fmt.Errorf("launch text contains an unsupported control character")
		}
	}
	var tokens []string
	var token strings.Builder
	var quote byte
	started := false
	appendToken := func() error {
		if !started {
			return nil
		}
		if len(tokens) == maxLaunchTokens {
			return fmt.Errorf("launch text exceeds %d tokens", maxLaunchTokens)
		}
		tokens = append(tokens, token.String())
		token.Reset()
		started = false
		return nil
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			} else if quote == '"' && c == '\\' && i+1 < len(text) && (text[i+1] == '"' || text[i+1] == '\\') {
				i++
				token.WriteByte(text[i])
			} else {
				token.WriteByte(c)
			}
			continue
		}
		switch c {
		case ' ', '\t', '\r', '\n':
			if err := appendToken(); err != nil {
				return nil, err
			}
		case '\'', '"':
			quote = c
			started = true
		case '|', '&', ';', '<', '>', '`':
			return nil, fmt.Errorf("shell operators are not supported; quote literal argument values")
		default:
			if c == '$' && i+1 < len(text) && text[i+1] == '(' {
				return nil, fmt.Errorf("command substitution is not supported; quote literal argument values")
			}
			started = true
			token.WriteByte(c)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("launch text has an unterminated quote")
	}
	if err := appendToken(); err != nil {
		return nil, err
	}
	return tokens, nil
}

// formatLaunchText produces a deterministic representation that parses back
// into the exact tokens. The result is PAIR syntax, not an OS shell command.
func formatLaunchText(tokens []string) (string, error) {
	quoted := make([]string, len(tokens))
	for i, token := range tokens {
		if token != "" && !strings.ContainsAny(token, " \t\r\n\"'\\|&;<>`$") {
			quoted[i] = token
		} else {
			quoted[i] = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(token) + `"`
		}
	}
	text := strings.Join(quoted, " ")
	if _, err := parseLaunchText(text); err != nil {
		return "", err
	}
	return text, nil
}
