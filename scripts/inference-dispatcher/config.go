// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const (
	defaultHost       = "127.0.0.1"
	defaultBackend    = "ollama"
	defaultOllamaPort = 11434
	// LM Studio's own default, which is also the port PAIR's lmstudio-proxy
	// facade claims (services/lmstudio-proxy/portstore.go defaultProxyPort).
	// PAIR's managed LM Studio backend is moved behind it starting at 1235;
	// pass --port explicitly to reach that directly, which bypasses routing.
	defaultLMStudioPort = 1234
	defaultErrorLog     = "inference_errors.txt"
	maxPromptsPerBatch  = 100
)

var defaultTopics = []string{
	"dog", "cat", "food", "toys", "birds", "planes",
	"robots", "space", "gardens", "music", "mountains", "friendship",
}

// Config is intentionally self-contained. This executable is a conventional
// third-party HTTP client and has no dependency on any PAIR package or protocol.
type Config struct {
	Port                     int      `json:"port,omitempty"`
	Backend                  string   `json:"backend"`
	Model                    string   `json:"model,omitempty"`
	AllowNonGenerativeModels bool     `json:"allow_non_generative_models,omitempty"`
	Count                    int      `json:"count"`
	Mode                     string   `json:"mode"`
	Concurrency              int      `json:"concurrency,omitempty"`
	Loop                     bool     `json:"loop,omitempty"`
	LoopDelaySeconds         float64  `json:"loop_delay_seconds,omitempty"`
	Topics                   []string `json:"topics,omitempty"`
	Prompts                  []string `json:"prompts,omitempty"`
	PromptTemplate           string   `json:"prompt_template"`
	RandomTopics             bool     `json:"random_topics,omitempty"`
	Seed                     *int64   `json:"seed,omitempty"`
	TimeoutSeconds           float64  `json:"timeout_seconds"`
	MaxTokens                int      `json:"max_tokens,omitempty"`
	Temperature              *float64 `json:"temperature,omitempty"`
	OllamaThink              string   `json:"ollama_think,omitempty"`
	DebugErrors              bool     `json:"debug_errors,omitempty"`
	DebugErrorLog            string   `json:"debug_error_log,omitempty"`
	FailOnError              bool     `json:"fail_on_error,omitempty"`
	ResultLog                string   `json:"result_log,omitempty"`
	ListModels               bool     `json:"-"`
	Version                  bool     `json:"-"`
}

func defaultConfig() Config {
	return Config{
		Backend:        defaultBackend,
		Count:          1,
		Mode:           "series",
		Topics:         append([]string(nil), defaultTopics...),
		PromptTemplate: "Make me a short story about {topic}.",
		TimeoutSeconds: 120,
	}
}

type stringListFlag struct {
	values *[]string
	set    bool
}

func (f *stringListFlag) String() string {
	if f.values == nil {
		return ""
	}
	return strings.Join(*f.values, ",")
}

func (f *stringListFlag) Set(value string) error {
	if !f.set {
		*f.values = nil
		f.set = true
	}
	*f.values = append(*f.values, value)
	return nil
}

type optionalInt64Flag struct {
	value *int64
	set   *bool
}

func (f optionalInt64Flag) String() string {
	if f.value == nil || f.set == nil || !*f.set {
		return ""
	}
	return strconv.FormatInt(*f.value, 10)
}

func (f optionalInt64Flag) Set(value string) error {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid integer %q", value)
	}
	*f.value = n
	*f.set = true
	return nil
}

type optionalFloat64Flag struct {
	value *float64
	set   *bool
}

func (f optionalFloat64Flag) String() string {
	if f.value == nil || f.set == nil || !*f.set {
		return ""
	}
	return strconv.FormatFloat(*f.value, 'g', -1, 64)
}

func (f optionalFloat64Flag) Set(value string) error {
	n, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fmt.Errorf("invalid number %q", value)
	}
	*f.value = n
	*f.set = true
	return nil
}

func configPathFromArgs(args []string) (string, error) {
	path := os.Getenv("INFERENCE_DISPATCHER_CONFIG")
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--config":
			if i+1 >= len(args) {
				return "", errors.New("--config requires a path")
			}
			path = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--config="):
			path = strings.TrimPrefix(args[i], "--config=")
		}
	}
	return path, nil
}

func loadConfigFile(cfg *Config, path string) error {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config %q: %w", path, err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(cfg); err != nil {
		return fmt.Errorf("parse config %q: %w", path, err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("parse config %q: unexpected trailing JSON", path)
	}
	return nil
}

func envString(name string, dst *string) {
	if value, ok := os.LookupEnv(name); ok {
		*dst = value
	}
}

func envInt(name string, dst *int) error {
	value, ok := os.LookupEnv(name)
	if !ok {
		return nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("%s must be an integer", name)
	}
	*dst = n
	return nil
}

func envFloat(name string, dst *float64) error {
	value, ok := os.LookupEnv(name)
	if !ok {
		return nil
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fmt.Errorf("%s must be a number", name)
	}
	*dst = n
	return nil
}

func envBool(name string, dst *bool) error {
	value, ok := os.LookupEnv(name)
	if !ok {
		return nil
	}
	b, err := strconv.ParseBool(value)
	if err != nil {
		return fmt.Errorf("%s must be true or false", name)
	}
	*dst = b
	return nil
}

func applyEnvironment(cfg *Config) error {
	envString("INFERENCE_DISPATCHER_BACKEND", &cfg.Backend)
	envString("INFERENCE_DISPATCHER_MODEL", &cfg.Model)
	envString("INFERENCE_DISPATCHER_MODE", &cfg.Mode)
	envString("INFERENCE_DISPATCHER_PROMPT_TEMPLATE", &cfg.PromptTemplate)
	envString("INFERENCE_DISPATCHER_OLLAMA_THINK", &cfg.OllamaThink)
	envString("INFERENCE_DISPATCHER_DEBUG_ERROR_LOG", &cfg.DebugErrorLog)
	envString("INFERENCE_DISPATCHER_RESULT_LOG", &cfg.ResultLog)
	for name, dst := range map[string]*int{
		"INFERENCE_DISPATCHER_PORT":        &cfg.Port,
		"INFERENCE_DISPATCHER_COUNT":       &cfg.Count,
		"INFERENCE_DISPATCHER_CONCURRENCY": &cfg.Concurrency,
		"INFERENCE_DISPATCHER_MAX_TOKENS":  &cfg.MaxTokens,
	} {
		if err := envInt(name, dst); err != nil {
			return err
		}
	}
	for name, dst := range map[string]*float64{
		"INFERENCE_DISPATCHER_LOOP_DELAY": &cfg.LoopDelaySeconds,
		"INFERENCE_DISPATCHER_TIMEOUT":    &cfg.TimeoutSeconds,
	} {
		if err := envFloat(name, dst); err != nil {
			return err
		}
	}
	for name, dst := range map[string]*bool{
		"INFERENCE_DISPATCHER_LOOP":                        &cfg.Loop,
		"INFERENCE_DISPATCHER_RANDOM_TOPICS":               &cfg.RandomTopics,
		"INFERENCE_DISPATCHER_DEBUG_ERRORS":                &cfg.DebugErrors,
		"INFERENCE_DISPATCHER_FAIL_ON_ERROR":               &cfg.FailOnError,
		"INFERENCE_DISPATCHER_ALLOW_NON_GENERATIVE_MODELS": &cfg.AllowNonGenerativeModels,
	} {
		if err := envBool(name, dst); err != nil {
			return err
		}
	}
	if value, ok := os.LookupEnv("INFERENCE_DISPATCHER_TEMPERATURE"); ok {
		n, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return errors.New("INFERENCE_DISPATCHER_TEMPERATURE must be a number")
		}
		cfg.Temperature = &n
	}
	if value, ok := os.LookupEnv("INFERENCE_DISPATCHER_SEED"); ok {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return errors.New("INFERENCE_DISPATCHER_SEED must be an integer")
		}
		cfg.Seed = &n
	}
	return nil
}

func parseConfig(args []string, stderr io.Writer) (Config, error) {
	cfg := defaultConfig()
	configPath, err := configPathFromArgs(args)
	if err != nil {
		return cfg, err
	}
	if err := loadConfigFile(&cfg, configPath); err != nil {
		return cfg, err
	}
	if err := applyEnvironment(&cfg); err != nil {
		return cfg, err
	}

	fs := flag.NewFlagSet("inference-dispatcher", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var parsedConfigPath string
	fs.StringVar(&parsedConfigPath, "config", configPath, "JSON configuration file")
	fs.StringVar(&cfg.Backend, "backend", cfg.Backend, "backend: ollama or lmstudio")
	fs.StringVar(&cfg.Backend, "provider", cfg.Backend, "alias for --backend")
	fs.IntVar(&cfg.Port, "port", cfg.Port, "server port (backend default when omitted)")
	fs.StringVar(&cfg.Model, "model", cfg.Model, "model name; omitted or auto selects an available model")
	fs.BoolVar(&cfg.AllowNonGenerativeModels, "allow-non-generative-models", cfg.AllowNonGenerativeModels, "allow automatic selection of models not marked for text generation")
	var safeAuto bool
	fs.BoolVar(&safeAuto, "auto-model-safe-guard", false, "compatibility alias; safe automatic selection is already the default")
	fs.IntVar(&cfg.Count, "count", cfg.Count, "generated prompts per batch")
	fs.StringVar(&cfg.Mode, "mode", cfg.Mode, "dispatch mode: series or parallel")
	fs.IntVar(&cfg.Concurrency, "concurrency", cfg.Concurrency, "maximum parallel requests (default: prompt count)")
	fs.BoolVar(&cfg.Loop, "loop", cfg.Loop, "keep sending batches until stopped")
	fs.Float64Var(&cfg.LoopDelaySeconds, "loop-delay", cfg.LoopDelaySeconds, "seconds between looped batches")
	fs.Var(&stringListFlag{values: &cfg.Topics}, "topic", "topic to use; repeat for multiple topics")
	fs.Var(&stringListFlag{values: &cfg.Prompts}, "prompt", "exact prompt to send; repeat for multiple prompts")
	fs.StringVar(&cfg.PromptTemplate, "prompt-template", cfg.PromptTemplate, "generated prompt template containing {topic}")
	fs.BoolVar(&cfg.RandomTopics, "random-topics", cfg.RandomTopics, "choose topics randomly instead of cycling")
	var seed int64
	var seedSet bool
	if cfg.Seed != nil {
		seed = *cfg.Seed
		seedSet = true
	}
	fs.Var(optionalInt64Flag{value: &seed, set: &seedSet}, "seed", "seed for repeatable topic selection")
	fs.Float64Var(&cfg.TimeoutSeconds, "timeout", cfg.TimeoutSeconds, "per-request timeout in seconds")
	fs.IntVar(&cfg.MaxTokens, "max-tokens", cfg.MaxTokens, "optional output-token limit")
	var temperature float64
	var temperatureSet bool
	if cfg.Temperature != nil {
		temperature = *cfg.Temperature
		temperatureSet = true
	}
	fs.Var(optionalFloat64Flag{value: &temperature, set: &temperatureSet}, "temperature", "optional sampling temperature")
	fs.StringVar(&cfg.OllamaThink, "ollama-think", cfg.OllamaThink, "Ollama thinking control: true, false, low, medium, or high")
	fs.BoolVar(&cfg.DebugErrors, "debug-errors", cfg.DebugErrors, "append inference errors to a debug log")
	fs.StringVar(&cfg.DebugErrorLog, "debug-error-log", cfg.DebugErrorLog, "debug error log path")
	fs.BoolVar(&cfg.FailOnError, "fail-on-error", cfg.FailOnError, "exit nonzero if an inference request fails")
	fs.StringVar(&cfg.ResultLog, "result-log", cfg.ResultLog, "JSON Lines result log path")
	fs.BoolVar(&cfg.ListModels, "list-models", false, "print the live model inventory as JSON and exit")
	fs.BoolVar(&cfg.Version, "version", false, "print the executable version and exit")

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() != 0 {
		return cfg, fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}
	if seedSet {
		cfg.Seed = &seed
	} else {
		cfg.Seed = nil
	}
	if temperatureSet {
		cfg.Temperature = &temperature
	} else {
		cfg.Temperature = nil
	}
	if cfg.DebugErrorLog != "" {
		cfg.DebugErrors = true
	} else if cfg.DebugErrors {
		cfg.DebugErrorLog = defaultErrorLog
	}
	cfg.Backend = strings.ToLower(strings.TrimSpace(cfg.Backend))
	cfg.Mode = strings.ToLower(strings.TrimSpace(cfg.Mode))
	return cfg, validateConfig(cfg)
}

func validateConfig(cfg Config) error {
	if cfg.Backend != "ollama" && cfg.Backend != "lmstudio" {
		return errors.New("--backend must be ollama or lmstudio")
	}
	if cfg.Port < 0 || cfg.Port > 65535 {
		return errors.New("--port must be between 1 and 65535")
	}
	if cfg.Count < 1 || cfg.Count > maxPromptsPerBatch {
		return fmt.Errorf("--count must be between 1 and %d", maxPromptsPerBatch)
	}
	if len(cfg.Prompts) > maxPromptsPerBatch {
		return fmt.Errorf("at most %d --prompt values are allowed", maxPromptsPerBatch)
	}
	for _, prompt := range cfg.Prompts {
		if strings.TrimSpace(prompt) == "" {
			return errors.New("--prompt values cannot be empty")
		}
	}
	if cfg.Mode != "series" && cfg.Mode != "parallel" {
		return errors.New("--mode must be series or parallel")
	}
	if cfg.Concurrency < 0 || cfg.Concurrency > maxPromptsPerBatch {
		return fmt.Errorf("--concurrency must be between 1 and %d when set", maxPromptsPerBatch)
	}
	if cfg.LoopDelaySeconds < 0 {
		return errors.New("--loop-delay cannot be negative")
	}
	if cfg.TimeoutSeconds <= 0 {
		return errors.New("--timeout must be greater than zero")
	}
	if cfg.MaxTokens < 0 {
		return errors.New("--max-tokens cannot be negative")
	}
	if cfg.Temperature != nil && (*cfg.Temperature < 0 || *cfg.Temperature > 2) {
		return errors.New("--temperature must be between 0 and 2")
	}
	if len(cfg.Prompts) == 0 {
		if len(cfg.Topics) == 0 {
			return errors.New("at least one topic is required when no exact prompts are set")
		}
		if !strings.Contains(cfg.PromptTemplate, "{topic}") {
			return errors.New("--prompt-template must contain {topic}")
		}
	}
	switch cfg.OllamaThink {
	case "", "true", "false", "low", "medium", "high":
	default:
		return errors.New("--ollama-think must be true, false, low, medium, or high")
	}
	if cfg.Backend != "ollama" && cfg.OllamaThink != "" {
		return errors.New("--ollama-think is only valid with --backend ollama")
	}
	return nil
}

func effectivePort(cfg Config) int {
	if cfg.Port != 0 {
		return cfg.Port
	}
	if cfg.Backend == "lmstudio" {
		return defaultLMStudioPort
	}
	return defaultOllamaPort
}
