// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
)

// Two input shapes are accepted:
//
//   - the raw on-disk log, nvpair.jsonl, which is pure JSONL;
//   - an exported bundle, nvpair-logs-*.txt, which wraps JSONL in markdown
//     sections (see desktop/src/electron/ipc/debug-log-export.ts).
//
// The exported bundle is the richer input because its Metadata section names the
// hostname and the user data path, which seed entity discovery.
const (
	sectionMetadata     = "Metadata"
	sectionModularState = "Current Modular State"
)

// visitor receives everything a scan produces. Sections arrive before the record
// stream in an exported bundle, since the metadata header precedes the log
// sections.
//
// A section is reported with both its raw text and, when it parses, its decoded
// form. Discovery reads the decoded form to learn identifiers from known fields.
// Output writes the raw text back, which preserves the original key order and
// formatting instead of re-serializing it.
type visitor struct {
	onRecord  func(rec Record, raw string) error
	onSection func(name, raw string, blob any) error
	// onSkip reports a non-empty line that was neither a record nor part of a
	// section. Such a line is dropped rather than sanitized, which is safe but
	// silent, so the caller is told how much was discarded.
	onSkip func(line string)
}

// scanFile streams one input file. Nothing is buffered beyond the current line
// and the current markdown section, so a multi-hundred-megabyte log is fine.
func scanFile(path string, v visitor) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, 256*1024)
	section := ""
	var pending []string

	flush := func() error {
		if section == "" || len(pending) == 0 {
			pending = nil
			return nil
		}
		defer func() { pending = nil }()
		if v.onSection == nil {
			return nil
		}
		raw := strings.Join(pending, "\n")

		// Not every section is JSON — "(none)" is written for empty ones. Those
		// are still reported so they can be written back; blob is simply nil.
		var blob any
		if err := json.Unmarshal([]byte(raw), &blob); err != nil {
			blob = nil
		}
		return v.onSection(section, raw, blob)
	}

	for {
		line, err := readLine(reader)
		if line != "" || err == nil {
			trimmed := strings.TrimRight(line, "\r")

			if header, ok := markdownSection(trimmed); ok {
				if ferr := flush(); ferr != nil {
					return ferr
				}
				section = header
				continue
			}

			if rec, ok := decodeRecord(trimmed); ok {
				if v.onRecord != nil {
					if rerr := v.onRecord(rec, trimmed); rerr != nil {
						return rerr
					}
				}
				continue
			}

			if section != "" && strings.TrimSpace(trimmed) != "" {
				pending = append(pending, trimmed)
			} else if section == "" && v.onSkip != nil {
				// Outside any section and not a record. The bundle's own title
				// line is expected; anything else is content being discarded.
				if body := strings.TrimSpace(trimmed); body != "" && !strings.HasPrefix(body, "# ") {
					v.onSkip(trimmed)
				}
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				return flush()
			}
			return err
		}
	}
}

// readLine reads a single line of unbounded length. bufio.Scanner is unsuitable
// here: JSON-RPC frames carrying model inventories exceed its token limit.
func readLine(r *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		chunk, err := r.ReadString('\n')
		sb.WriteString(strings.TrimSuffix(chunk, "\n"))
		if err != nil {
			return sb.String(), err
		}
		return sb.String(), nil
	}
}

func markdownSection(line string) (string, bool) {
	if !strings.HasPrefix(line, "## ") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "## ")), true
}

// decodeRecord accepts a line only if it is a complete JSON object carrying the
// level and time fields every LogEntry has. This is what separates record lines
// from the pretty-printed JSON of the surrounding bundle sections.
func decodeRecord(line string) (Record, bool) {
	if !strings.HasPrefix(line, "{") {
		return Record{}, false
	}
	var probe struct {
		Level *string `json:"level"`
		Time  *string `json:"time"`
	}
	if err := json.Unmarshal([]byte(line), &probe); err != nil {
		return Record{}, false
	}
	if probe.Level == nil || probe.Time == nil {
		return Record{}, false
	}
	var rec Record
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		return Record{}, false
	}
	return rec, true
}

// parseEmbedded reads a JSON frame carried inside a string field. Broker stdout
// is copied into the log verbatim, so Message is frequently a serialized
// JSON-RPC frame whose params hold the identifiers worth learning.
func parseEmbedded(s string) (any, bool) {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "{") && !strings.HasPrefix(t, "[") {
		return nil, false
	}
	var out any
	if err := json.Unmarshal([]byte(t), &out); err != nil {
		return nil, false
	}
	return out, true
}

// walkStrings visits every string in a decoded JSON tree together with the
// dotted key path that carried it. Slice elements inherit their parent's path so
// that addresses[] is visited as "addresses".
//
// The path exists so detection can suppress known-ambiguous contexts, such as
// processVersions.zlib, whose four-part version strings are indistinguishable
// from an address by shape. The path is never used to decide what a value is,
// only to decide where a shape test would be wrong.
// Object keys are visited as well as values. Some payloads are keyed by
// identifier — nodesInitial is a map from node UUID to node — so a walker that
// only visited values would leave those identifiers untouched, and a verifier
// sharing the walker would not notice.
// isKey distinguishes an object key from a value. Both are visited, but a rule
// driven by the key that carried a value must not fire on the key text itself:
// in "models":[{"model":"x"}] the key "model" arrives under the path "models",
// and would otherwise be read as a model named "model".
func walkStrings(v any, path string, visit func(path, s string, isKey bool)) {
	switch t := v.(type) {
	case string:
		visit(path, t, false)
	case []any:
		for _, item := range t {
			walkStrings(item, path, visit)
		}
	case map[string]any:
		for k, item := range t {
			visit(path, k, true)
			child := k
			if path != "" {
				child = path + "." + k
			}
			walkStrings(item, child, visit)
		}
	}
}

// mapStrings rebuilds a decoded JSON tree with every string passed through fn.
// Replacing on decoded values is what makes escaping correct: the depth of
// backslash and quote escaping in the source text is irrelevant, and
// re-marshalling restores whatever escaping the output position requires.
func mapStrings(v any, fn func(string) string) any {
	switch t := v.(type) {
	case string:
		return fn(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = mapStrings(item, fn)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[fn(k)] = mapStrings(item, fn)
		}
		return out
	default:
		return v
	}
}
