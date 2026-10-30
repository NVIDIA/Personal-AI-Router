// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// toolGateway puts a gateway in front of an engine that records the body of
// each request, with or without a tool-call parser.
func toolGateway(t *testing.T, parsesTools bool) (url string, received func() map[string]json.RawMessage) {
	t.Helper()
	var last []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last, _ = io.ReadAll(r.Body)
		if r.ContentLength != int64(len(last)) {
			t.Errorf("Content-Length %d does not match the %d bytes sent", r.ContentLength, len(last))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	t.Cleanup(srv.Close)
	_, backendPort, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	bp, _ := strconv.Atoi(backendPort)
	public, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	g, err := startFrontGateway("127.0.0.1", public, bp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.close)
	g.setToolCalls(parsesTools)
	return fmt.Sprintf("http://127.0.0.1:%d", public), func() map[string]json.RawMessage {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(last, &fields); err != nil {
			t.Fatalf("the engine received invalid JSON %q: %v", last, err)
		}
		return fields
	}
}

const weatherTools = `[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]`

func postChat(t *testing.T, url, path, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func TestDropUnparsableTools_RemovesToolsAnEngineWithoutAParserWouldReject(t *testing.T) {
	cases := map[string]string{
		"no tool_choice":   `{"model":"m","messages":[{"role":"user","content":"Is it <hot> & sunny?"}],"tools":` + weatherTools + `,"parallel_tool_calls":true}`,
		"tool_choice auto": `{"model":"m","messages":[{"role":"user","content":"Is it <hot> & sunny?"}],"tools":` + weatherTools + `,"tool_choice":"auto"}`,
	}
	for name, body := range cases {
		url, received := toolGateway(t, false)

		resp := postChat(t, url, "/v1/chat/completions", body)

		fields := received()
		for _, removed := range []string{"tools", "tool_choice", "parallel_tool_calls"} {
			if _, ok := fields[removed]; ok {
				t.Errorf("%s: %s reached the engine", name, removed)
			}
		}
		if string(fields["messages"]) != `[{"role":"user","content":"Is it <hot> & sunny?"}]` || string(fields["model"]) != `"m"` {
			t.Errorf("%s: the rest of the request changed: %s %s", name, fields["model"], fields["messages"])
		}
		if resp.StatusCode != http.StatusOK || resp.Header.Get(toolsUnavailableHeader) == "" {
			t.Errorf("%s: status %d, %s header %q; want 200 saying the tools were removed", name, resp.StatusCode, toolsUnavailableHeader, resp.Header.Get(toolsUnavailableHeader))
		}
	}
}

func TestDropUnparsableTools_LeavesRequestsThatDemandATool(t *testing.T) {
	cases := map[string]string{
		"required":       `"required"`,
		"named function": `{"type":"function","function":{"name":"get_weather"}}`,
		"none":           `"none"`,
	}
	for name, choice := range cases {
		url, received := toolGateway(t, false)
		body := `{"model":"m","messages":[],"tools":` + weatherTools + `,"tool_choice":` + choice + `}`

		resp := postChat(t, url, "/v1/chat/completions", body)

		fields := received()
		if _, ok := fields["tools"]; !ok || string(fields["tool_choice"]) != choice {
			t.Errorf("%s: the engine received %v; want the request unchanged", name, fields)
		}
		if resp.Header.Get(toolsUnavailableHeader) != "" {
			t.Errorf("%s: reported removed tools", name)
		}
	}
}

func TestDropUnparsableTools_ForwardsToolsToAnEngineWithAParser(t *testing.T) {
	url, received := toolGateway(t, true)
	body := `{"model":"m","messages":[],"tools":` + weatherTools + `}`

	resp := postChat(t, url, "/v1/chat/completions", body)

	if fields := received(); string(fields["tools"]) != weatherTools {
		t.Fatalf("tools = %s, want them forwarded unchanged", fields["tools"])
	}
	if resp.Header.Get(toolsUnavailableHeader) != "" {
		t.Fatal("reported removed tools")
	}
}

func TestDropUnparsableTools_LeavesOtherRequestsByteForByte(t *testing.T) {
	cases := map[string]struct{ path, body string }{
		"chat without tools":      {"/v1/chat/completions", `{"model":"m",  "messages":[]}`},
		"chat with an empty list": {"/v1/chat/completions", `{"model":"m","messages":[],"tools":[]}`},
		"completions":             {"/v1/completions", `{"model":"m","prompt":"hi","tools":` + weatherTools + `}`},
		"not JSON":                {"/v1/chat/completions", `tools`},
	}
	for name, c := range cases {
		req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")

		if dropUnparsableTools(req) {
			t.Errorf("%s: reported removed tools", name)
		}
		body, _ := io.ReadAll(req.Body)
		if !bytes.Equal(body, []byte(c.body)) {
			t.Errorf("%s: body = %q, want %q", name, body, c.body)
		}
	}
}
