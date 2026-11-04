// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// modelstore.go serves engines that keep each model as a directory in their
// store and run one model per process (Colibri). The store layout comes from
// the manifest's scan_dir action, so the inventory, the launch and the
// launch_model action all agree on what a model is: a directory at most a
// fixed depth below models_dir that contains a marker file. Colibri's own
// `coli setup` writes models one level down (~/colibri-models/qwen36-35b) and
// PAIR's Hugging Face downloads two (~/colibri-models/<owner>/<repository>), so
// both layouts are models.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// hfRepoPattern is a Hugging Face repository id that is safe to use as a path
// and as a command-line value: owner/name, each part starting with a letter or
// digit, so nothing can read as a parent reference, an absolute path or a flag.
var hfRepoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// modelSegmentPattern is one directory name of a model id.
var modelSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// partialSuffix marks a model directory still being downloaded. The download
// renames it into place only once every file has arrived, so a listing that
// skips it never offers a half-downloaded model.
const partialSuffix = ".partial"

// defaultModelDepth is the deepest a model sits when a manifest declares no
// listing: the owner/repository layout of a Hugging Face snapshot.
const defaultModelDepth = 2

// errNoLaunchModel is a start refused because the store holds no model.
var errNoLaunchModel = errors.New("no model is downloaded")

// modelStoreLayout returns the file that marks a complete model directory and
// the deepest level below the store one may sit, from the manifest's
// list_models scan_dir action. ok is false when the manifest lists its models some other
// way.
func (m *Manifest) modelStoreLayout() (marker string, depth int, ok bool) {
	if act, found := m.Actions["list_models"]; found && act.ScanDir != nil {
		return act.ScanDir.Marker, act.ScanDir.depthOrDefault(), true
	}
	return "", defaultModelDepth, false
}

// validModelID reports a model id of one to depth "/"-separated segments,
// each a plain directory name. It is what keeps a caller's model param from
// naming anything outside the store; whether the id names a model, rather than
// a folder of models, is the marker's to say.
func validModelID(id string, depth int) bool {
	parts := strings.Split(id, "/")
	if len(parts) < 1 || len(parts) > depth {
		return false
	}
	for _, part := range parts {
		if !modelSegmentPattern.MatchString(part) || strings.HasSuffix(part, partialSuffix) {
			return false
		}
	}
	return true
}

// hasMarker reports a regular marker file directly inside dir.
func hasMarker(dir, marker string) bool {
	fi, err := os.Stat(filepath.Join(dir, marker))
	return err == nil && fi.Mode().IsRegular()
}

// scanModelDirs lists the models in a store: every directory at most depth
// levels below root that contains marker, as a "/"-joined relative id, sorted.
// A model directory is not searched for further models. Hidden directories,
// downloads in progress, names that could not be a model id, and links (a
// symlink or junction is never a directory entry here) are skipped. A missing
// or unreadable store is an empty inventory.
func scanModelDirs(root, marker string, depth int) []string {
	var out []string
	var walk func(dir string, rel []string)
	walk = func(dir string, rel []string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() || strings.HasSuffix(name, partialSuffix) || !modelSegmentPattern.MatchString(name) {
				continue
			}
			path := filepath.Join(dir, name)
			id := append(append([]string(nil), rel...), name)
			if hasMarker(path, marker) {
				out = append(out, strings.Join(id, "/"))
				continue
			}
			if len(id) < depth {
				walk(path, id)
			}
		}
	}
	if root != "" && depth > 0 {
		walk(root, nil)
	}
	sort.Strings(out)
	return out
}

// runtimeReferencesModel reports a launch that names a model, so only an
// engine that serves one model per process ever scans its store at start.
func runtimeReferencesModel(rt Runtime) bool {
	for _, arg := range rt.Args {
		if strings.Contains(arg, "{model}") {
			return true
		}
	}
	for _, value := range rt.Env {
		if strings.Contains(value, "{model}") {
			return true
		}
	}
	return false
}

// launchModel picks the model the next launch serves: the persisted
// runtime.model while it is still on disk, otherwise the first model in the
// store. A store with no model is errNoLaunchModel.
func (st *engineState) launchModel(rt Runtime) (string, error) {
	marker, depth, layout := st.manifest.modelStoreLayout()
	if !layout || st.modelsDir == "" {
		if rt.Model != "" && validModelID(rt.Model, depth) {
			return rt.Model, nil
		}
		return "", errNoLaunchModel
	}
	if rt.Model != "" && validModelID(rt.Model, depth) &&
		hasMarker(filepath.Join(st.modelsDir, filepath.FromSlash(rt.Model)), marker) {
		return rt.Model, nil
	}
	if models := scanModelDirs(st.modelsDir, marker, depth); len(models) > 0 {
		return models[0], nil
	}
	return "", errNoLaunchModel
}

// launchVars are the placeholders a launch resolves: the path placeholders,
// the listen address, the control CLI, and {model} for a launch that names
// one. With no model downloaded every other placeholder is still returned,
// {model} empty, alongside an error wrapping errNoLaunchModel: a start must
// refuse, while a settings display or preview must still render.
func (st *engineState) launchVars(rt Runtime, host string, port int) (map[string]string, error) {
	vars := st.pathVars()
	vars["host"] = host
	vars["port"] = strconv.Itoa(port)
	if rt.CLI != "" {
		vars["cli"] = expandPath(rt.CLI)
	}
	if !runtimeReferencesModel(rt) {
		return vars, nil
	}
	model, err := st.launchModel(rt)
	vars["model"] = model
	if err != nil {
		return vars, fmt.Errorf("%w: download a model before starting %s", err, st.manifest.DisplayName)
	}
	return vars, nil
}
