// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// One input produces one output, in the format it arrived in. A log is never
// split across files: a reader who opens a sanitized log should find the same
// thing they would have found in the original, with the identifiers changed.
//
// A raw nvpair.jsonl is written back as JSONL. An exported bundle is written back
// with its markdown sections intact, so the app version, platform and node
// snapshot in its header survive alongside the records.
const recordSectionTitle = "Log Records JSONL"

type sink interface {
	// section reports a passthrough section, already sanitized.
	section(name, text string) error
	// record appends one sanitized record.
	record(rec Record) error
	// close finishes the file.
	close() error
	// written reports how many records were emitted.
	count() int
}

func newSink(path string, bundle bool) (sink, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	buf := bufio.NewWriterSize(f, 256*1024)
	if bundle {
		return &bundleSink{file: f, out: buf, enc: newRecordEncoder(buf)}, nil
	}
	return &jsonlSink{file: f, out: buf, enc: newRecordEncoder(buf)}, nil
}

// newRecordEncoder matches how the desktop logger writes a record. Go escapes
// <, > and & by default for safe embedding in HTML, which JSON.stringify does not
// do; leaving that on would both diverge from the input format and render a
// placeholder token as \u003cinstall\u003e.
func newRecordEncoder(w *bufio.Writer) *json.Encoder {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc
}

// jsonlSink writes the plain record stream.
type jsonlSink struct {
	file    *os.File
	out     *bufio.Writer
	enc     *json.Encoder
	records int
}

func (s *jsonlSink) section(string, string) error { return nil }

func (s *jsonlSink) record(rec Record) error {
	if err := s.enc.Encode(rec); err != nil {
		return err
	}
	s.records++
	return nil
}

func (s *jsonlSink) count() int { return s.records }

func (s *jsonlSink) close() error {
	if err := s.out.Flush(); err != nil {
		s.file.Close()
		return err
	}
	return s.file.Close()
}

// bundleSink reproduces the exported bundle layout.
//
// The two record sections of the original — the app log and the in-memory
// subprocess ring — are written back as one. De-duplication deliberately collapses
// the copies of an event that appear in both, so reconstructing the split would
// mean re-introducing the duplicates.
type bundleSink struct {
	file        *os.File
	out         *bufio.Writer
	enc         *json.Encoder
	records     int
	wroteHeader bool
	inRecords   bool
}

func (s *bundleSink) header() error {
	if s.wroteHeader {
		return nil
	}
	s.wroteHeader = true
	_, err := fmt.Fprintf(s.out, "# %s\n\n", "Personal AI Router Logs (sanitized)")
	return err
}

func (s *bundleSink) section(name, text string) error {
	if err := s.header(); err != nil {
		return err
	}
	if s.inRecords {
		return fmt.Errorf("section %q arrived after the record stream", name)
	}
	body := strings.TrimRight(text, "\n")
	if body == "" {
		body = "(none)"
	}
	_, err := fmt.Fprintf(s.out, "## %s\n%s\n\n", name, body)
	return err
}

func (s *bundleSink) record(rec Record) error {
	if err := s.header(); err != nil {
		return err
	}
	if !s.inRecords {
		s.inRecords = true
		if _, err := fmt.Fprintf(s.out, "## %s\n", recordSectionTitle); err != nil {
			return err
		}
	}
	if err := s.enc.Encode(rec); err != nil {
		return err
	}
	s.records++
	return nil
}

func (s *bundleSink) count() int { return s.records }

func (s *bundleSink) close() error {
	if err := s.header(); err != nil {
		s.file.Close()
		return err
	}
	if err := s.out.Flush(); err != nil {
		s.file.Close()
		return err
	}
	return s.file.Close()
}

// outputName is the file a group is written to. Naming after the anonymized
// producer is what makes a set of sanitized logs navigable: node-a.jsonl holds
// what node-a recorded.
//
// Falls back to a source number when the input carried no header identifying its
// producer, which is the case for a plain nvpair.jsonl.
func outputName(g inputGroup, index int) string {
	ext := "jsonl"
	if g.bundle {
		ext = "txt"
	}
	if node := g.node.resolve(); node != nil && node.Label != "" {
		return node.Label + "." + ext
	}
	return fmt.Sprintf("source-%d.%s", index+1, ext)
}

// uniqueOutputName keeps one file per input even when two inputs came from the
// same machine, rather than letting the second overwrite the first.
func uniqueOutputName(g inputGroup, index int, used map[string]int) string {
	name := outputName(g, index)
	used[name]++
	if n := used[name]; n > 1 {
		ext := filepath.Ext(name)
		return fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), n, ext)
	}
	return name
}
