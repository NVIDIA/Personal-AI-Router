// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"testing"

	"nvpair-tui/rpc"
)

func TestModelInventoryRPCShapes(t *testing.T) {
	for _, tc := range []struct {
		name, engine, inventory, action string
		managed                         bool
		want                            []string
	}{
		{"LM Studio native keys", "lmstudio", `{"models":[{"key":"phi-3","loaded_instances":[]},{"key":"gemma-2b","loaded_instances":[]}]}`, "list_models", false, []string{"phi-3", "gemma-2b"}},
		{"Ollama names", "ollama", `{"models":[{"name":"llama3:8b","model":"llama3:8b"},{"name":"qwen:0.5b"}]}`, "list_models", false, []string{"llama3:8b", "qwen:0.5b"}},
		{"Ollama model field", "ollama", `{"models":[{"model":"llama3:8b"}]}`, "list_models", false, []string{"llama3:8b"}},
		{"managed llama IDs", "llamacpp", `{"data":[{"id":"owner/model:Q4"}]}`, "list_downloaded", true, []string{"owner/model:Q4"}},
		{"external llama IDs", "llamacpp", `{"data":[{"id":"owner/model:Q4"}]}`, "list_models", false, []string{"owner/model:Q4"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The real RPC client and decoder run over in-memory streams, not sockets.
			clientIn, serverOut := io.Pipe()
			serverIn, clientOut := io.Pipe()
			client := rpc.NewClient(clientIn, clientOut)
			server := rpc.NewCodec(serverIn, serverOut)
			ctx, cancel := context.WithCancel(context.Background())
			clientDone := make(chan struct{})
			go func() {
				defer close(clientDone)
				_ = client.Run(ctx)
			}()
			t.Cleanup(func() {
				cancel()
				_ = clientOut.Close()
				_ = serverOut.Close()
				_ = clientIn.Close()
				_ = serverIn.Close()
				<-clientDone
			})
			loaded, err := json.Marshal(map[string]map[string][]string{
				"loadedByEngine": {tc.engine: {tc.want[0]}},
			})
			if err != nil {
				t.Fatal(err)
			}
			serve := func() error {
				for _, response := range []struct {
					method string
					result json.RawMessage
				}{{"engine:action", json.RawMessage(tc.inventory)}, {"engine:models", loaded}} {
					req, err := server.Read()
					if err != nil {
						return err
					}
					if req.Method != response.method {
						return fmt.Errorf("method = %q, want %q", req.Method, response.method)
					}
					if req.Method == "engine:action" {
						var params map[string]string
						if err := json.Unmarshal(req.Params, &params); err != nil {
							return err
						}
						if params["engine"] != tc.engine || params["action"] != tc.action {
							return fmt.Errorf("wrong inventory action: %v", params)
						}
					}
					if err := server.Write(&rpc.Message{JSONRPC: "2.0", ID: req.ID, Result: response.result}); err != nil {
						return err
					}
				}
				return nil
			}
			serverDone := make(chan error, 1)
			go func() {
				err := serve()
				serverDone <- err
				if err != nil {
					_ = serverOut.CloseWithError(err)
				}
			}()
			v := newEnginesView(client)
			v.SetSize(90, 20)
			v.merge(engineStatus{Engine: tc.engine, Installed: true, Managed: tc.managed})
			cmd := v.loadModelsCmd()
			if cmd == nil {
				t.Fatal("selected engine has no model inventory command")
			}
			msg := cmd()
			if err := <-serverDone; err != nil {
				t.Fatal(err)
			}
			result, ok := msg.(engineModelsMsg)
			if !ok {
				t.Fatalf("inventory result = %#v", msg)
			}
			if result.err != nil {
				t.Fatal(result.err)
			}
			if !reflect.DeepEqual(result.names, tc.want) || !result.loaded[tc.want[0]] {
				t.Fatalf("inventory = %+v, want names %v with first model loaded", result, tc.want)
			}
			v.Update(result)
			if rows := v.models.Rows(); len(rows) != len(tc.want) || rows[0][0] != tc.want[0] || rows[0][1] != "loaded" {
				t.Fatalf("model rows lost identity/residency: %v", rows)
			}
		})
	}
}

func TestResidencyNotificationUsesEngineIdentity(t *testing.T) {
	for _, engine := range []string{"ollama", "lmstudio", "llamacpp"} {
		t.Run(engine, func(t *testing.T) {
			v := newEnginesView(nil)
			v.pendingLoadEngine, v.pendingLoadModel = engine, "model"
			v.Update(NotificationMsg{Msg: &rpc.Message{Method: "engine:models-changed", Params: json.RawMessage(`{"engine":"other","models":{"loadedByEngine":{"other":["model"]}}}`)}})
			if v.pendingLoadModel == "" {
				t.Fatal("another engine's residency settled this load")
			}
			params, err := json.Marshal(map[string]any{
				"engine": engine,
				"models": map[string]map[string][]string{"loadedByEngine": {engine: {"model"}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			v.Update(NotificationMsg{Msg: &rpc.Message{Method: "engine:models-changed", Params: params}})
			if v.pendingLoadModel != "" || v.status != "Model loaded: model" || v.showModels {
				t.Fatalf("real residency envelope did not settle without opening model browser: %s", v.status)
			}
		})
	}
}
