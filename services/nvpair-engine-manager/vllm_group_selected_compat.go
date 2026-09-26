// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// This file is the explicit boundary between the sealed serving-group owner
// and the selected managed-vLLM 0.29 lifecycle. It aliases existing owned I/O
// and validation primitives rather than importing the sealed 0.28 installer.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	vllmManagedVersion       = managedVLLMVersion
	vllmResourceSettingsFile = "resource-settings.json"
)

var vllmGPUUUIDPattern = regexp.MustCompile(`^GPU-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func remoteModelID(value string) bool {
	if strings.HasPrefix(value, "local:") && vllmGroupDigest.MatchString(strings.TrimPrefix(value, "local:")) {
		return true
	}
	parts := strings.Split(value, "@")
	return len(parts) == 2 && strings.Count(parts[0], "/") == 1 && len(parts[1]) == 40 && regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`).MatchString(parts[0]) && regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(parts[1])
}

func decodeActionParams(raw json.RawMessage, out any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	return strictDiagnosticJSON(raw, out)
}

func readVLLMJSON(root, path string, limit int64, out any) error {
	return readManagedVLLMJSON(root, path, limit, out)
}

func writeVLLMJSON(root, path string, value any) error {
	return writeManagedVLLMJSON(root, path, value)
}

func validateVLLMOwnedPath(root, target string) error {
	return validateManagedVLLMLexicalPath(root, target)
}

type vllmResourceSettings struct {
	GPUMemoryUtilization *float64 `json:"gpu_memory_utilization"`
	MaxModelLen          *int32   `json:"max_model_len"`
	GPUUUID              *string  `json:"gpu_uuid,omitempty"`
	TensorParallelSize   *int32   `json:"tensor_parallel_size,omitempty"`
}

func readVLLMResourceSettings(st *engineState) (vllmResourceSettings, error) {
	var raw json.RawMessage
	err := readVLLMJSON(st.installDir, filepath.Join(st.installDir, "resource-settings.json"), 4096, &raw)
	if os.IsNotExist(err) {
		return vllmResourceSettings{}, nil
	}
	if err != nil {
		return vllmResourceSettings{}, err
	}
	settings, err := decodeVLLMResourceSettings(raw)
	if err == nil {
		err = settings.validateParallelism()
	}
	return settings, err
}

func decodeVLLMResourceSettings(raw json.RawMessage) (vllmResourceSettings, error) {
	var settings vllmResourceSettings
	if err := decodeActionParams(raw, &settings); err != nil {
		return settings, err
	}
	if value := settings.GPUMemoryUtilization; value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value <= 0 || *value > 1) {
		return settings, errors.New("gpu_memory_utilization must be greater than 0 and at most 1")
	}
	if settings.MaxModelLen != nil && *settings.MaxModelLen <= 0 {
		return settings, errors.New("max_model_len must be positive")
	}
	if settings.GPUUUID != nil && !vllmGPUUUIDPattern.MatchString(*settings.GPUUUID) {
		return settings, errors.New("gpu_uuid must be one full NVIDIA GPU UUID")
	}
	if settings.TensorParallelSize != nil && *settings.TensorParallelSize < 1 {
		return settings, errors.New("tensor_parallel_size must be positive")
	}
	return settings, nil
}

func (s vllmResourceSettings) validateParallelism() error {
	if s.TensorParallelSize != nil && *s.TensorParallelSize != 1 {
		return errors.New("a serving-group rank owns exactly one selected GPU")
	}
	return nil
}

func appendVLLMResourceArgs(args []string, settings vllmResourceSettings) []string {
	if settings.GPUMemoryUtilization != nil {
		args = append(args, "--gpu-memory-utilization", strconv.FormatFloat(*settings.GPUMemoryUtilization, 'g', -1, 64))
	}
	if settings.MaxModelLen != nil {
		args = append(args, "--max-model-len", strconv.Itoa(int(*settings.MaxModelLen)))
	}
	return args
}

func vllmGPUSelection(value string) ([]string, error) {
	parts := strings.Split(value, ",")
	seen := map[string]bool{}
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if !vllmGPUUUIDPattern.MatchString(part) {
			return nil, errors.New("GPU selection requires full NVIDIA GPU UUIDs")
		}
		part = "GPU-" + strings.ToLower(part[4:])
		if seen[part] {
			return nil, errors.New("GPU selection contains a duplicate UUID")
		}
		seen[part], parts[i] = true, part
	}
	return parts, nil
}

type vllmStartupDiagnostics struct {
	mu         sync.Mutex
	tail       []byte
	runtimeDir string
}

func newVLLMStartupDiagnostics() *vllmStartupDiagnostics { return &vllmStartupDiagnostics{} }
func (d *vllmStartupDiagnostics) onLine(stream, line string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tail = append(d.tail, line...)
	d.tail = append(d.tail, '\n')
	if len(d.tail) > 32<<10 {
		d.tail = append([]byte(nil), d.tail[len(d.tail)-(32<<10):]...)
	}
}
func (d *vllmStartupDiagnostics) stop() { d.mu.Lock(); d.tail = nil; d.mu.Unlock() }
func (d *vllmStartupDiagnostics) failure(_ *managedProc, cause error) error {
	return fmt.Errorf("managed group rank startup failed: %w", cause)
}

type vllmModelFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type vllmModel struct {
	ID             string          `json:"id"`
	DisplayName    string          `json:"displayName,omitempty"`
	Source         string          `json:"source"`
	Revision       string          `json:"revision,omitempty"`
	License        string          `json:"license"`
	MetadataSHA256 string          `json:"metadataSha256,omitempty"`
	Digest         string          `json:"digest"`
	Bytes          int64           `json:"bytes"`
	Files          []vllmModelFile `json:"files,omitempty"`
}

func vllmModelPath(st *engineState, id string) (string, error) {
	if id == "" || len(id) > 512 || strings.ContainsAny(id, "\x00\r\n") {
		return "", errors.New("exact vLLM model id is required")
	}
	sum := sha256.Sum256([]byte(id))
	dir := filepath.Join(st.modelDir, hex.EncodeToString(sum[:]))
	if err := validateVLLMOwnedPath(st.modelDir, dir); err != nil {
		return "", err
	}
	return dir, nil
}

func readVLLMModel(st *engineState, id string) (vllmModel, string, error) {
	var model vllmModel
	dir, err := vllmModelPath(st, id)
	if err != nil {
		return model, "", err
	}
	if err := readVLLMJSON(dir, filepath.Join(dir, "pair-model.json"), 8<<20, &model); err != nil {
		return model, "", errors.New("vLLM model is not in the retained library")
	}
	if model.ID != id || len(model.Files) == 0 || len(model.Files) > 20000 {
		return model, "", errors.New("vLLM model ownership record does not match")
	}
	return model, dir, nil
}

func verifyVLLMModel(ctx context.Context, st *engineState, id string) (vllmModel, string, error) {
	model, dir, err := readVLLMModel(st, id)
	if err != nil {
		return model, "", err
	}
	if err := validateRetainedVLLMModelReceipt(ctx, st, dir, model); err != nil {
		return model, "", err
	}
	if _, err := hashVLLMModelFiles(ctx, dir, model); err != nil {
		return model, "", err
	}
	return model, dir, nil
}

// hashVLLMModelFiles proves every retained file against its receipt and returns
// the identity of each file it actually read.
func hashVLLMModelFiles(ctx context.Context, dir string, model vllmModel) ([]os.FileInfo, error) {
	hashed := make([]os.FileInfo, 0, len(model.Files))
	for _, entry := range model.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, err := os.Open(filepath.Join(dir, filepath.FromSlash(entry.Path)))
		if err != nil {
			return nil, errors.New("vLLM model snapshot is incomplete or changed")
		}
		info, statErr := file.Stat()
		hash := sha256.New()
		read, copyErr := io.Copy(hash, vllmContextReader{ctx: ctx, reader: file})
		closeErr := file.Close()
		if statErr != nil || copyErr != nil || closeErr != nil || read != entry.Size || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
			return nil, errors.New("vLLM retained model content changed")
		}
		hashed = append(hashed, info)
	}
	return hashed, nil
}

// sameVLLMModelFiles reports whether each retained path still names the exact,
// unmodified file hashVLLMModelFiles read.
func sameVLLMModelFiles(dir string, model vllmModel, hashed []os.FileInfo) bool {
	if len(hashed) != len(model.Files) {
		return false
	}
	for index, entry := range model.Files {
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(entry.Path)))
		if err != nil || !os.SameFile(info, hashed[index]) || info.Size() != hashed[index].Size() || !info.ModTime().Equal(hashed[index].ModTime()) {
			return false
		}
	}
	return true
}

func vllmPythonBuildPrerequisiteReason(vllmPythonFacts) string { return "" }

func (e *Executor) vllmActiveCLI(st *engineState, name string) (string, error) {
	if name != "vllm" {
		return "", errors.New("unsupported managed vLLM CLI")
	}
	record, err := readVLLMRuntimeRecord(st)
	if err != nil || record.Active == "" || record.Removing {
		return "", errors.New("vLLM has no active PAIR-owned runtime")
	}
	cli, _, err := validateVLLMEnvironment(st, record.Active)
	return cli, err
}

func (e *Executor) vllmChildEnv(st *engineState, runtimeDir string) ([]string, func(), error) {
	return managedVLLMRuntimeEnv(st, runtimeDir), func() {}, nil
}

func (e *Executor) vllmServingEnv(env []string) []string { return env }

func (e *Executor) runVLLMCommand(ctx context.Context, bin string, args, env []string) ([]byte, error) {
	return runManagedVLLMCommand(ctx, filepath.Dir(bin), bin, args, env)
}

func (e *Executor) vllmNvidiaSMIPath() string { return "/usr/bin/nvidia-smi" }

func stopVLLMProcess(proc *managedProc, grace time.Duration) error {
	if proc == nil {
		return errors.New("no owned vLLM process")
	}
	return proc.stop(grace)
}

func (e *Executor) vllmHTTP(ctx context.Context, port int, method, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("vLLM %s returned HTTP %d", path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func (e *Executor) probeVLLM(ctx context.Context, port int) bool {
	st, err := e.state("vllm")
	if err != nil {
		return false
	}
	st.mu.Lock()
	model, version, proc := st.servingModel, st.version, st.proc
	st.mu.Unlock()
	if model == "" || proc == nil {
		return false
	}
	if e.vllmOwnsListener != nil && !e.vllmOwnsListener(proc, port) {
		return false
	}
	var observed struct {
		Version string `json:"version"`
	}
	if e.vllmHTTP(ctx, port, http.MethodGet, "/version", nil, &observed) != nil || observed.Version != version {
		return false
	}
	if e.vllmHTTP(ctx, port, http.MethodGet, "/health", nil, nil) != nil {
		return false
	}
	var models struct {
		Object string `json:"object"`
		Data   []struct {
			ID    string `json:"id"`
			Owner string `json:"owned_by"`
		} `json:"data"`
	}
	return e.vllmHTTP(ctx, port, http.MethodGet, "/v1/models", nil, &models) == nil && models.Object == "list" && len(models.Data) == 1 && models.Data[0].ID == model && models.Data[0].Owner == "vllm"
}

func (e *Executor) verifyVLLMGeneration(ctx context.Context, st *engineState, port int) error {
	st.mu.Lock()
	model, proc := st.servingModel, st.proc
	st.mu.Unlock()
	if model == "" || proc == nil {
		return errors.New("vLLM has no owned selected model")
	}
	body, _ := json.Marshal(map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "Reply OK"}}, "max_tokens": 1, "temperature": 0, "stream": false})
	var response struct {
		Model   string            `json:"model"`
		Choices []json.RawMessage `json:"choices"`
	}
	if err := e.vllmHTTP(ctx, port, http.MethodPost, "/v1/chat/completions", body, &response); err != nil {
		return err
	}
	if response.Model != model || len(response.Choices) != 1 {
		return errors.New("vLLM generation readiness did not identify the selected model")
	}
	return nil
}

func (e *Executor) vllmCurrentModels(ctx context.Context, st *engineState) (json.RawMessage, error) {
	st.mu.Lock()
	rank := st.vllmRank
	running, healthy, port, model, proc := st.running, st.healthy, st.port, st.servingModel, st.proc
	st.mu.Unlock()
	if rank != nil {
		rank.mu.Lock()
		closed, rankIndex := rank.receipt.CleanupConfirmed, rank.binding.Rank
		rank.mu.Unlock()
		if !closed {
			if rankIndex != 0 {
				return json.Marshal(map[string]any{"object": "list", "data": []any{}})
			}
			return nil, errors.New("vLLM group residency is not independently routable")
		}
	}
	if !running || !healthy || proc == nil || model == "" {
		return json.Marshal(map[string]any{"object": "list", "data": []any{}})
	}
	if !e.probeVLLM(ctx, port) {
		return nil, errors.New("vLLM residency health is unknown")
	}
	return json.Marshal(map[string]any{"object": "list", "data": []map[string]string{{"id": model, "owned_by": "vllm"}}})
}

func (e *Executor) usesVLLMWSL(string) bool { return false }

func (e *Executor) beginVLLMMutation(ctx context.Context, st *engineState) (context.Context, func(), error) {
	release, err := e.admitEngineMutation(st, "engine actions")
	if err != nil {
		return nil, nil, err
	}
	child, cancel := context.WithCancel(ctx)
	st.mu.Lock()
	if err := ctx.Err(); err != nil || e.shuttingDown.Load() || st.stopPending > 0 {
		st.mu.Unlock()
		cancel()
		release()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, context.Canceled
	}
	previous := st.mutationCancel
	st.mutationCancel = cancel
	st.mu.Unlock()
	return child, func() {
		cancel()
		st.mu.Lock()
		st.mutationCancel = previous
		st.mu.Unlock()
		release()
	}, nil
}

type vllmPairInventory struct{ HostUUID string }

func (e *Executor) readVLLMPairInventory(context.Context) (vllmPairInventory, error) {
	if !cableIdentifier(e.vllmNodeID, 128) {
		return vllmPairInventory{}, errors.New("local PAIR node identity is unavailable")
	}
	return vllmPairInventory{HostUUID: e.vllmNodeID}, nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
