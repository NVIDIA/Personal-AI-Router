// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// manifestcatalog.go is a manifest's own model list: the models an engine's
// maintainers publish for it by repository rather than by tag. Colibri's
// release carries such a list (its `coli setup` catalog), and several of its
// models are the vendors' original checkpoints, which no Hugging Face tag
// search finds. engine:catalog shows these entries beside the tagged ones, and
// an hf_snapshot pull of one of these repositories follows the entry's rules:
// a pinned revision, files the engine never reads, and steps that finish the
// model directory once the files are in.

import (
	"errors"
	"fmt"
	"math"
	"path"
	"regexp"
	"strings"
)

// CatalogEntry is one model a manifest lists: a public Hugging Face repository
// and how to download it.
type CatalogEntry struct {
	Repo string `json:"repo"`
	Name string `json:"name"`
	// Parameters is a display label such as "284B".
	Parameters string `json:"parameters,omitempty"`
	// SizeGB is the download size in gigabytes (10^9 bytes), for display. The
	// pull measures the real size from the repository.
	SizeGB float64 `json:"size_gb,omitempty"`
	// Revision pins the commit downloaded. Empty is the main branch.
	Revision string `json:"revision,omitempty"`
	// Exclude lists repository paths that are not downloaded, as shell patterns
	// in which "*" also matches "/": the rule of Python's fnmatch, which
	// Colibri's own downloader applies to the same lists.
	Exclude []string `json:"exclude,omitempty"`
	// AfterDownload finishes the model directory before it is listed.
	AfterDownload []AfterDownloadStep `json:"after_download,omitempty"`
}

// AfterDownloadStep runs a Python 3 script from the engine's install directory
// with the model directory as its one argument. It is done once the file it
// creates is in the model directory, so a step is never run twice.
type AfterDownloadStep struct {
	// Python is the script, relative to the install directory, "/" separated.
	Python string `json:"python"`
	// Creates is the file the script writes in the model directory.
	Creates string `json:"creates"`
}

// commitPattern is a full git commit id, the only revision an entry may pin: a
// branch or tag can move, and then the listed size and rules no longer match.
var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// modelTypePattern is a normalized config.json model_type.
var modelTypePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// normalizeModelType lowercases and trims a model_type, the way Colibri's
// family registry compares them.
func normalizeModelType(modelType string) string {
	return strings.ToLower(strings.TrimSpace(modelType))
}

// servesModelType reports a model_type the engine runs. A manifest that
// declares no model_types runs whatever it is given, as far as PAIR can tell.
func (m *Manifest) servesModelType(modelType string) bool {
	if len(m.ModelTypes) == 0 {
		return true
	}
	modelType = normalizeModelType(modelType)
	for _, served := range m.ModelTypes {
		if served == modelType {
			return true
		}
	}
	return false
}

// validateModelTypes checks the model_types list. They are what a downloaded
// repository is held to, so they only mean something with an hf_snapshot pull.
func (m *Manifest) validateModelTypes() error {
	if len(m.ModelTypes) == 0 {
		return nil
	}
	if act, ok := m.Actions[pullModelAction]; !ok || act.HFSnapshot == nil {
		return errors.New("model_types requires a pull_model action with hf_snapshot")
	}
	seen := make(map[string]bool, len(m.ModelTypes))
	for _, modelType := range m.ModelTypes {
		if !modelTypePattern.MatchString(modelType) {
			return fmt.Errorf("model_types: %q must be a lowercase config.json model_type", modelType)
		}
		if seen[modelType] {
			return fmt.Errorf("model_types: %q is listed twice", modelType)
		}
		seen[modelType] = true
	}
	return nil
}

// ref is the revision to download.
func (c CatalogEntry) ref() string {
	if c.Revision == "" {
		return "main"
	}
	return c.Revision
}

// catalogEntry returns the listed entry for a repository.
func (m *Manifest) catalogEntry(repo string) (CatalogEntry, bool) {
	for _, entry := range m.Catalog {
		if entry.Repo == repo {
			return entry, true
		}
	}
	return CatalogEntry{}, false
}

// validateCatalog checks the manifest's model list. A list only means something
// to an engine that downloads models by repository.
func (m *Manifest) validateCatalog() error {
	if len(m.Catalog) == 0 {
		return nil
	}
	if act, ok := m.Actions[pullModelAction]; !ok || act.HFSnapshot == nil {
		return errors.New("catalog requires a pull_model action with hf_snapshot")
	}
	seen := make(map[string]bool, len(m.Catalog))
	for i, entry := range m.Catalog {
		if !hfRepoPattern.MatchString(entry.Repo) {
			return fmt.Errorf("catalog[%d]: repo %q is not a Hugging Face repository id (owner/name)", i, entry.Repo)
		}
		if seen[entry.Repo] {
			return fmt.Errorf("catalog[%d]: %s is listed twice", i, entry.Repo)
		}
		seen[entry.Repo] = true
		if strings.TrimSpace(entry.Name) == "" {
			return fmt.Errorf("catalog %s: name is required", entry.Repo)
		}
		if entry.SizeGB < 0 || math.IsNaN(entry.SizeGB) || math.IsInf(entry.SizeGB, 0) {
			return fmt.Errorf("catalog %s: size_gb must be a size in gigabytes", entry.Repo)
		}
		if entry.Revision != "" && !commitPattern.MatchString(entry.Revision) {
			return fmt.Errorf("catalog %s: revision must be a full commit id", entry.Repo)
		}
		for _, pattern := range entry.Exclude {
			if !validRepoPattern(pattern) {
				return fmt.Errorf("catalog %s: exclude pattern %q must be a relative repository path", entry.Repo, pattern)
			}
		}
		for _, step := range entry.AfterDownload {
			if !safeRepoFileName(step.Python) || path.Ext(step.Python) != ".py" {
				return fmt.Errorf("catalog %s: after_download python %q must be a .py path inside the install directory", entry.Repo, step.Python)
			}
			if !modelSegmentPattern.MatchString(step.Creates) {
				return fmt.Errorf("catalog %s: after_download creates %q must be a file name", entry.Repo, step.Creates)
			}
		}
	}
	return nil
}

// validRepoPattern accepts a pattern over "/"-separated repository paths that
// cannot climb out of the repository.
func validRepoPattern(pattern string) bool {
	if strings.TrimSpace(pattern) != pattern || pattern == "" || strings.HasPrefix(pattern, "/") || strings.Contains(pattern, `\`) {
		return false
	}
	for _, segment := range strings.Split(pattern, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	_, err := fnmatchRegexp(pattern)
	return err == nil
}

// excluded reports a repository path that one of the patterns matches.
func excluded(name string, patterns []string) bool {
	for _, pattern := range patterns {
		if re, err := fnmatchRegexp(pattern); err == nil && re.MatchString(name) {
			return true
		}
	}
	return false
}

// fnmatchRegexp translates a shell pattern the way Python's fnmatch does: "*"
// is any run of characters, "/" included; "?" is one character; "[...]" is a
// class, "[!...]" a negated one. Everything else is literal.
func fnmatchRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch ch := pattern[i]; ch {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '[':
			end := strings.IndexByte(pattern[i+1:], ']')
			if end < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := pattern[i+1 : i+1+end]
			if negated, ok := strings.CutPrefix(class, "!"); ok {
				class = "^" + negated
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += end + 1
		default:
			b.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
