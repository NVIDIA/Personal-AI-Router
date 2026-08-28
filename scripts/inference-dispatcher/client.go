// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 16 << 20

type RegisteredModel struct {
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities,omitempty"`
	Type         string   `json:"type,omitempty"`
}

type backendClient struct {
	cfg    Config
	client *http.Client
	base   *url.URL
}

func newBackendClient(cfg Config) (*backendClient, error) {
	base := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(defaultHost, strconv.Itoa(effectivePort(cfg))),
	}
	return &backendClient{
		cfg: cfg,
		client: &http.Client{
			Timeout: time.Duration(cfg.TimeoutSeconds * float64(time.Second)),
		},
		base: base,
	}, nil
}

func (c *backendClient) endpoint(path string) string {
	copy := *c.base
	copy.Path = strings.TrimRight(copy.Path, "/") + path
	return copy.String()
}

func (c *backendClient) request(ctx context.Context, method, path string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.endpoint(path), body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "inference-dispatcher/"+Version)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("timed out after %g seconds", c.cfg.TimeoutSeconds)
		}
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer response.Body.Close()

	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("response exceeded %d bytes", maxResponseBytes)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// The body is described, never quoted. An OpenAI-compatible endpoint
		// echoes the offending request back in its error envelope, so relaying
		// the text would put prompt content into the result log and the debug
		// error log — both durable and unrotated.
		return nil, fmt.Errorf(
			"HTTP %s (%d bytes, %s)",
			response.Status, len(data), digest(string(data)),
		)
	}
	return data, nil
}

func decodeObject(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON from server: %w", err)
	}
	return nil
}

func (c *backendClient) listModels(ctx context.Context) ([]RegisteredModel, error) {
	var models []RegisteredModel
	var err error
	if c.cfg.Backend == "lmstudio" {
		models, err = c.listLMStudioModels(ctx)
	} else {
		models, err = c.listOllamaModels(ctx)
	}
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("no registered %s models were found", c.cfg.Backend)
	}
	sort.SliceStable(models, func(i, j int) bool {
		return strings.ToLower(models[i].Name) < strings.ToLower(models[j].Name)
	})
	return models, nil
}

func (c *backendClient) listOllamaModels(ctx context.Context) ([]RegisteredModel, error) {
	data, err := c.request(ctx, http.MethodGet, "/api/tags", nil)
	if err != nil {
		return nil, fmt.Errorf("query Ollama models: %w", err)
	}
	var response struct {
		Models []struct {
			Name         string   `json:"name"`
			Model        string   `json:"model"`
			Capabilities []string `json:"capabilities"`
		} `json:"models"`
	}
	if err := decodeObject(data, &response); err != nil {
		return nil, err
	}
	models := make([]RegisteredModel, 0, len(response.Models))
	for _, model := range response.Models {
		name := strings.TrimSpace(model.Name)
		if name == "" {
			name = strings.TrimSpace(model.Model)
		}
		if name == "" {
			continue
		}
		models = append(models, RegisteredModel{
			Name:         name,
			Capabilities: normalizedStrings(model.Capabilities),
		})
	}
	return models, nil
}

// listLMStudioModels asks the OpenAI-compatible `/v1/models` first. When the
// endpoint belongs to a Personal AI Router proxy, that route fans out across the
// cluster and answers with the combined inventory, while the native
// `/api/v1/models` is forwarded to a single node — so preferring the native list
// would silently hide every model that lives on another machine.
//
// The OpenAI list carries no type or capability fields, which the generation
// filter needs, so the native list is still consulted and merged in by name.
// Whatever it reports describes the same model wherever it is loaded.
func (c *backendClient) listLMStudioModels(ctx context.Context) ([]RegisteredModel, error) {
	openAIData, openAIErr := c.request(ctx, http.MethodGet, "/v1/models", nil)
	var models []RegisteredModel
	if openAIErr == nil {
		parsed, err := parseLMStudioOpenAIModels(openAIData)
		if err != nil {
			openAIErr = err
		} else if len(parsed) == 0 {
			openAIErr = errors.New("no models returned")
		} else {
			models = parsed
		}
	}

	nativeData, nativeErr := c.request(ctx, http.MethodGet, "/api/v1/models", nil)
	var native []RegisteredModel
	if nativeErr == nil {
		native, nativeErr = parseLMStudioNativeModels(nativeData)
	}

	if models == nil {
		if nativeErr != nil || len(native) == 0 {
			return nil, fmt.Errorf(
				"LM Studio model query failed through /v1/models (%v) and /api/v1/models (%v)",
				openAIErr, nativeErr,
			)
		}
		return native, nil
	}
	return mergeLMStudioMetadata(models, native), nil
}

// mergeLMStudioMetadata copies the type and capability fields the native
// endpoint reports onto the cluster-wide list, matched by name. Names the native
// endpoint does not know about keep their empty metadata, which
// supportsGeneration treats as eligible.
func mergeLMStudioMetadata(models, native []RegisteredModel) []RegisteredModel {
	if len(native) == 0 {
		return models
	}
	byName := make(map[string]RegisteredModel, len(native))
	for _, model := range native {
		byName[model.Name] = model
	}
	for i, model := range models {
		detail, ok := byName[model.Name]
		if !ok {
			continue
		}
		models[i].Type = detail.Type
		models[i].Capabilities = detail.Capabilities
	}
	return models
}

func parseLMStudioNativeModels(data []byte) ([]RegisteredModel, error) {
	var response struct {
		Models []map[string]any `json:"models"`
	}
	if err := decodeObject(data, &response); err != nil {
		return nil, err
	}
	models := make([]RegisteredModel, 0, len(response.Models))
	for _, raw := range response.Models {
		name := firstString(raw, "key", "id", "model", "display_name")
		if name == "" {
			continue
		}
		model := RegisteredModel{Name: name, Type: strings.ToLower(firstString(raw, "type"))}
		if capabilities, ok := raw["capabilities"].(map[string]any); ok {
			for key, value := range capabilities {
				if truthy(value) {
					model.Capabilities = append(model.Capabilities, strings.ToLower(key))
				}
			}
			sort.Strings(model.Capabilities)
		}
		models = append(models, model)
	}
	return models, nil
}

func parseLMStudioOpenAIModels(data []byte) ([]RegisteredModel, error) {
	var response struct {
		Data []struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		} `json:"data"`
	}
	if err := decodeObject(data, &response); err != nil {
		return nil, err
	}
	models := make([]RegisteredModel, 0, len(response.Data))
	for _, raw := range response.Data {
		name := strings.TrimSpace(raw.ID)
		if name == "" {
			name = strings.TrimSpace(raw.Model)
		}
		if name != "" {
			models = append(models, RegisteredModel{Name: name})
		}
	}
	return models, nil
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func truthy(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		parsed, err := strconv.ParseBool(v)
		return err == nil && parsed
	default:
		return false
	}
}

func normalizedStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func supportsGeneration(model RegisteredModel) bool {
	if model.Type != "" {
		return model.Type == "llm"
	}
	if len(model.Capabilities) == 0 {
		return true
	}
	for _, capability := range model.Capabilities {
		if capability == "completion" || capability == "chat" || capability == "generate" {
			return true
		}
	}
	return false
}

func (c *backendClient) resolveModel(ctx context.Context) (string, []RegisteredModel, error) {
	if model := strings.TrimSpace(c.cfg.Model); model != "" && !strings.EqualFold(model, "auto") {
		return model, nil, nil
	}
	models, err := c.listModels(ctx)
	if err != nil {
		return "", nil, err
	}
	for _, model := range models {
		if c.cfg.AllowNonGenerativeModels || supportsGeneration(model) {
			return model.Name, models, nil
		}
	}
	return "", models, errors.New("no available model advertises text-generation support")
}

func (c *backendClient) inferencePath() string {
	if c.cfg.Backend == "lmstudio" {
		return "/v1/chat/completions"
	}
	return "/api/generate"
}

func (c *backendClient) infer(ctx context.Context, model, prompt string) (string, error) {
	var payload map[string]any
	if c.cfg.Backend == "lmstudio" {
		payload = map[string]any{
			"model":    model,
			"messages": []map[string]string{{"role": "user", "content": prompt}},
			"stream":   false,
		}
		if c.cfg.MaxTokens > 0 {
			payload["max_tokens"] = c.cfg.MaxTokens
		}
		if c.cfg.Temperature != nil {
			payload["temperature"] = *c.cfg.Temperature
		}
	} else {
		payload = map[string]any{"model": model, "prompt": prompt, "stream": false}
		options := map[string]any{}
		if c.cfg.MaxTokens > 0 {
			options["num_predict"] = c.cfg.MaxTokens
		}
		if c.cfg.Temperature != nil {
			options["temperature"] = *c.cfg.Temperature
		}
		if len(options) > 0 {
			payload["options"] = options
		}
		switch c.cfg.OllamaThink {
		case "true":
			payload["think"] = true
		case "false":
			payload["think"] = false
		case "low", "medium", "high":
			payload["think"] = c.cfg.OllamaThink
		}
	}
	data, err := c.request(ctx, http.MethodPost, c.inferencePath(), payload)
	if err != nil {
		return "", err
	}
	if c.cfg.Backend == "lmstudio" {
		return parseLMStudioResponse(data)
	}
	var response struct {
		Response string `json:"response"`
	}
	if err := decodeObject(data, &response); err != nil {
		return "", err
	}
	return strings.TrimSpace(response.Response), nil
}

func parseLMStudioResponse(data []byte) (string, error) {
	var response struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
			Text string `json:"text"`
		} `json:"choices"`
	}
	if err := decodeObject(data, &response); err != nil {
		return "", err
	}
	if len(response.Choices) == 0 {
		return "", errors.New("LM Studio response contained no choices")
	}
	choice := response.Choices[0]
	switch content := choice.Message.Content.(type) {
	case string:
		return strings.TrimSpace(content), nil
	case []any:
		var builder strings.Builder
		for _, item := range content {
			switch part := item.(type) {
			case string:
				builder.WriteString(part)
			case map[string]any:
				if text, ok := part["text"].(string); ok {
					builder.WriteString(text)
				}
			}
		}
		return strings.TrimSpace(builder.String()), nil
	}
	return strings.TrimSpace(choice.Text), nil
}
