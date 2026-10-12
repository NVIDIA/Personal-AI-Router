// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseLaunchText(t *testing.T) {
	test := func(name, text string, want []string) {
		t.Run(name, func(t *testing.T) {
			got, err := parseLaunchText(text)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("got %#v, %v; want %#v", got, err, want)
			}
			normalized, err := formatLaunchText(got)
			if err != nil {
				t.Fatal(err)
			}
			again, err := parseLaunchText(normalized)
			if err != nil || !reflect.DeepEqual(again, want) {
				t.Fatalf("round trip changed arguments: %#v, %v", again, err)
			}
		})
	}
	test("empty arguments", "", nil)
	test("blank arguments", " \t\r\n", nil)
	test("env and Windows executable", `OLLAMA_HOST=127.0.0.1:11435 "C:\Program Files\Ollama\ollama.exe" serve`, []string{"OLLAMA_HOST=127.0.0.1:11435", `C:\Program Files\Ollama\ollama.exe`, "serve"})
	test("Windows backslashes", `C:\tools\lms.exe --path 'C:\new\tools\'`, []string{`C:\tools\lms.exe`, "--path", `C:\new\tools\`})
	test("UNC", `'\\server\share\lms.exe' server start`, []string{`\\server\share\lms.exe`, "server", "start"})
	test("quoted segments", `lms --name="two words" --empty=''`, []string{"lms", "--name=two words", "--empty="})
	test("empty arguments", `lms "" ''`, []string{"lms", "", ""})
	test("escaped quotes", `lms "a\"b\\c"`, []string{"lms", `a"b\c`})
	test("multiline", "lms\r\nserver\tstart --name '日本語\n模型'", []string{"lms", "server", "start", "--name", "日本語\n模型"})
	test("literal expansions", `lms $HOME %USERPROFILE% {port} '~' '$(touch nope)'`, []string{"lms", "$HOME", "%USERPROFILE%", "{port}", "~", "$(touch nope)"})
	test("literal operators", "lms '|&;<>`'", []string{"lms", "|&;<>`"})
	test("terminator", `lms -- --port=9000`, []string{"lms", "--", "--port=9000"})
}

func TestParseLaunchTextRejectsInvalidInput(t *testing.T) {
	for _, text := range []string{
		`lms "unterminated`, `lms 'unterminated`, "lms \x00argument",
		"lms \x7f", "lms \xff", `lms | other`, `lms > file`, `lms < file`,
		`lms && other`, `lms ; other`, "lms `other`", `lms $(other)`,
		strings.Repeat("x", maxLaunchTextBytes+1), strings.Repeat("x ", maxLaunchTokens+1),
	} {
		if _, err := parseLaunchText(text); err == nil {
			t.Fatalf("invalid input accepted (%d bytes)", len(text))
		}
	}
	if _, err := parseLaunchText(strings.Repeat("x", maxLaunchTextBytes)); err != nil {
		t.Fatalf("exact byte limit rejected: %v", err)
	}
	if _, err := parseLaunchText(strings.Repeat("x ", maxLaunchTokens)); err != nil {
		t.Fatalf("exact token limit rejected: %v", err)
	}
}

func FuzzLaunchTextRoundTrip(f *testing.F) {
	for _, seed := range []string{
		`lms server start --port=1235`, `"C:\Program Files\Ollama\ollama.exe" serve`,
		`lms '' "日本語" '$HOME' '{port}'`, `lms --name="two words"`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		tokens, err := parseLaunchText(text)
		if err != nil {
			return
		}
		formatted, err := formatLaunchText(tokens)
		if err != nil {
			// Canonical escaping may push a valid input beyond the byte limit.
			return
		}
		again, err := parseLaunchText(formatted)
		if err != nil || !reflect.DeepEqual(again, tokens) {
			t.Fatalf("round trip changed tokens: %#v -> %#v (%v)", tokens, again, err)
		}
		stable, err := formatLaunchText(again)
		if err != nil || stable != formatted {
			t.Fatalf("normalization is not stable: %v", err)
		}
	})
}
