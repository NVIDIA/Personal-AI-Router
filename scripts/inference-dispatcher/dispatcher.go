// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type DispatchJob struct {
	ID     int
	Topic  string
	Prompt string
}

type DispatchResult struct {
	Job      DispatchJob
	OK       bool
	Elapsed  time.Duration
	Response string
	Error    string
}

type dispatcher struct {
	cfg    Config
	client *backendClient
	model  string
	stdout io.Writer
	stderr io.Writer
	rng    *rand.Rand
	nextID int
}

func newDispatcher(cfg Config, client *backendClient, stdout, stderr io.Writer) *dispatcher {
	seed := time.Now().UnixNano()
	if cfg.Seed != nil {
		seed = *cfg.Seed
	}
	return &dispatcher{
		cfg: cfg, client: client, stdout: stdout, stderr: stderr,
		rng: rand.New(rand.NewSource(seed)), nextID: 1,
	}
}

func (d *dispatcher) jobs(batch int) []DispatchJob {
	if len(d.cfg.Prompts) > 0 {
		jobs := make([]DispatchJob, len(d.cfg.Prompts))
		for i, prompt := range d.cfg.Prompts {
			jobs[i] = DispatchJob{ID: d.nextID, Topic: "custom", Prompt: prompt}
			d.nextID++
		}
		return jobs
	}
	jobs := make([]DispatchJob, d.cfg.Count)
	for i := range jobs {
		topicIndex := (d.nextID - 1) % len(d.cfg.Topics)
		if d.cfg.RandomTopics {
			topicIndex = d.rng.Intn(len(d.cfg.Topics))
		}
		topic := d.cfg.Topics[topicIndex]
		jobs[i] = DispatchJob{
			ID:     d.nextID,
			Topic:  topic,
			Prompt: strings.ReplaceAll(d.cfg.PromptTemplate, "{topic}", topic),
		}
		d.nextID++
	}
	return jobs
}

func (d *dispatcher) send(ctx context.Context, job DispatchJob) DispatchResult {
	started := time.Now()
	response, err := d.client.infer(ctx, d.model, job.Prompt)
	result := DispatchResult{Job: job, OK: err == nil, Elapsed: time.Since(started), Response: response}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

func (d *dispatcher) dispatch(ctx context.Context, jobs []DispatchJob) []DispatchResult {
	if d.cfg.Mode == "series" {
		results := make([]DispatchResult, 0, len(jobs))
		for _, job := range jobs {
			if ctx.Err() != nil {
				break
			}
			results = append(results, d.send(ctx, job))
		}
		return results
	}

	limit := d.cfg.Concurrency
	if limit <= 0 || limit > len(jobs) {
		limit = len(jobs)
	}
	results := make([]DispatchResult, len(jobs))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = DispatchResult{Job: job, Error: ctx.Err().Error()}
				return
			}
			results[i] = d.send(ctx, job)
		}()
	}
	wg.Wait()
	return results
}

func (d *dispatcher) run(ctx context.Context) int {
	model, models, err := d.client.resolveModel(ctx)
	if err != nil {
		d.logStartupError(err.Error())
		fmt.Fprintf(d.stderr, "Model selection failed: %v\n", err)
		return 2
	}
	d.model = model
	if len(models) > 0 {
		fmt.Fprintf(d.stdout, "Auto-selected available model %q from %d registered model(s).\n", model, len(models))
	}
	fmt.Fprintln(d.stdout, "Inference dispatcher ready. Stop the process to cancel.")

	batch := 1
	hadErrors := false
	for {
		if ctx.Err() != nil {
			fmt.Fprintln(d.stdout, "Stop requested. No new jobs will be sent.")
			return 0
		}
		jobs := d.jobs(batch)
		fmt.Fprintf(
			d.stdout,
			"\nBatch %d: sending %d prompt(s) in %s to %s at %s using model %s\n",
			batch, len(jobs), d.cfg.Mode, d.cfg.Backend, d.client.base.String(), d.model,
		)
		for _, job := range jobs {
			// Prompt text is never echoed: proxy-inference-routing.mdc forbids
			// logging prompts, request bodies, or responses. The digest is
			// enough to correlate a job across the transcript and the logs.
			fmt.Fprintf(d.stdout, "  #%d topic=%q prompt=%s\n", job.ID, job.Topic, promptDigest(job.Prompt))
		}
		results := d.dispatch(ctx, jobs)
		for _, result := range results {
			status := "OK"
			if !result.OK {
				status = "ERROR"
				hadErrors = true
			}
			fmt.Fprintf(d.stdout, "\n[%s] #%d topic=%q elapsed=%.2fs\n", status, result.Job.ID, result.Job.Topic, result.Elapsed.Seconds())
			if result.OK {
				// Responses are described, never printed. stdout is captured
				// into terminal scrollback and CI logs just like a file is.
				fmt.Fprintf(d.stdout, "  response=%s bytes=%d\n", digest(result.Response), len(result.Response))
			} else {
				fmt.Fprintln(d.stderr, result.Error)
				d.logError(result)
			}
			d.logResult(batch, result)
		}
		if ctx.Err() != nil {
			fmt.Fprintln(d.stdout, "Stop requested. No new jobs will be sent.")
			return 0
		}
		if !d.cfg.Loop {
			break
		}
		batch++
		if d.cfg.LoopDelaySeconds > 0 {
			timer := time.NewTimer(time.Duration(d.cfg.LoopDelaySeconds * float64(time.Second)))
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				fmt.Fprintln(d.stdout, "Stop requested. No new jobs will be sent.")
				return 0
			}
		}
	}
	if d.cfg.FailOnError && hadErrors {
		return 1
	}
	return 0
}

func compact(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= limit {
		return value
	}
	if limit <= 3 {
		return value[:limit]
	}
	return value[:limit-3] + "..."
}

func appendLine(path string, value []byte) error {
	if path == "" {
		return nil
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(value); err != nil {
		return err
	}
	_, err = file.Write([]byte("\n"))
	return err
}

func (d *dispatcher) baseRecord() map[string]any {
	return map[string]any{
		"timestamp":       time.Now().UTC().Format(time.RFC3339Nano),
		"pid":             os.Getpid(),
		"backend":         d.cfg.Backend,
		"base_url":        d.client.base.String(),
		"model":           d.model,
		"timeout_seconds": d.cfg.TimeoutSeconds,
		"max_tokens":      d.cfg.MaxTokens,
		"temperature":     d.cfg.Temperature,
		"ollama_think":    d.cfg.OllamaThink,
		"seed":            d.cfg.Seed,
	}
}

// digest identifies inference content without reproducing it. Prompts, request
// bodies, and responses must never reach stdout or a log file
// (proxy-inference-routing.mdc); a short hash is enough to tell two payloads
// apart and to confirm the same one was reused.
func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

func promptDigest(value string) string {
	return fmt.Sprintf("%s len=%d", digest(value), len(value))
}

func (d *dispatcher) logResult(batch int, result DispatchResult) {
	if d.cfg.ResultLog == "" {
		return
	}
	record := d.baseRecord()
	record["batch"] = batch
	record["job_id"] = result.Job.ID
	record["topic"] = result.Job.Topic
	// Digest and size only. The result log is durable and unrotated, so writing
	// prompt or response text here would put inference content on disk
	// indefinitely — prohibited by proxy-inference-routing.mdc.
	record["prompt_digest"] = promptDigest(result.Job.Prompt)
	record["ok"] = result.OK
	record["elapsed_seconds"] = result.Elapsed.Seconds()
	record["response_bytes"] = len(result.Response)
	record["error"] = result.Error
	data, err := json.Marshal(record)
	if err == nil {
		err = appendLine(d.cfg.ResultLog, data)
	}
	if err != nil {
		fmt.Fprintf(d.stderr, "Result log error: %v\n", err)
	}
}

func (d *dispatcher) logError(result DispatchResult) {
	if !d.cfg.DebugErrors {
		return
	}
	fields := []string{
		time.Now().UTC().Format(time.RFC3339),
		fmt.Sprintf("pid=%d", os.Getpid()),
		"backend=" + d.cfg.Backend,
		"base_url=" + d.client.base.String(),
		"model=" + d.model,
		fmt.Sprintf("job=%d", result.Job.ID),
		"topic=" + compact(result.Job.Topic, 200),
		fmt.Sprintf("elapsed=%.2fs", result.Elapsed.Seconds()),
		"error=" + compact(result.Error, 1000),
		"prompt=" + promptDigest(result.Job.Prompt),
	}
	if err := appendLine(d.cfg.DebugErrorLog, []byte(strings.Join(fields, "\t"))); err != nil {
		fmt.Fprintf(d.stderr, "Debug log error: %v\n", err)
	}
}

func (d *dispatcher) logStartupError(message string) {
	if d.cfg.ResultLog != "" {
		record := d.baseRecord()
		record["batch"] = 0
		record["job_id"] = "startup"
		record["ok"] = false
		record["error"] = message
		if data, err := json.Marshal(record); err == nil {
			_ = appendLine(d.cfg.ResultLog, data)
		}
	}
	if d.cfg.DebugErrors {
		_ = appendLine(d.cfg.DebugErrorLog, []byte(strings.Join([]string{
			time.Now().UTC().Format(time.RFC3339),
			fmt.Sprintf("pid=%d", os.Getpid()),
			"backend=" + d.cfg.Backend,
			"model=" + d.cfg.Model,
			"job=startup",
			"error=" + compact(message, 1000),
		}, "\t")))
	}
}
