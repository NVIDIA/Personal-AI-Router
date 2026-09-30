// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Response bytes arriving from a node's engine are the only liveness evidence
// that gets stronger as the node gets busier, which is exactly when discovery's
// probes start timing out and evicting it. These tests pin that the proxy
// actually raises it.

func waitFor(t *testing.T, rec *recRW, frame string) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec.has(frame) {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestStreamedBytesReportNodeActivity is the point of the feature: the node that
// served the response is named to the broker so discovery can keep it.
func TestStreamedBytesReportNodeActivity(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, err := w.Write([]byte(`{"done":true}`))
			assert.NoError(t, err)
		}))
		defer upstream.Close()

		rec := &recRW{}
		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "serving-node", upstream.URL, tc.advertisedModel))
		p := newTestProxy(tc.profile, NewCodec(rec), disc, tc.profile.FacadePort)

		p.soleFacade().handleHTTP(httptest.NewRecorder(), httptest.NewRequest(
			http.MethodPost, tc.inferencePath,
			strings.NewReader(fmt.Sprintf(`{"model":%q}`, tc.requestedModel))))

		require.True(t, waitFor(t, rec, `"method":"node/activity"`), "no node/activity was reported after the upstream streamed a response")
		require.True(t, waitFor(t, rec, `"hostUuid":"serving-node"`), "node/activity did not name the node that served the request")
	})
}

// A node that accepted the connection but never wrote a byte has proved nothing
// about being able to do work, so it must not be vouched for. This is the case
// that matters: an accept can succeed on a machine whose user space is wedged.
func TestNoActivityReportedWithoutUpstreamBytes(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		release := make(chan struct{})
		var releaseOnce sync.Once
		doRelease := func() { releaseOnce.Do(func() { close(release) }) }

		received := make(chan struct{}, 1)
		silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case received <- struct{}{}:
			default:
			}
			<-release
			w.WriteHeader(http.StatusOK)
		}))
		// LIFO: unblock the handler before tearing the server down, or Close
		// hangs on the live connection.
		defer silent.Close()
		defer doRelease()

		rec := &recRW{}
		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "silent-node", silent.URL, tc.advertisedModel))
		p := newTestProxy(tc.profile, NewCodec(rec), disc, tc.profile.FacadePort)

		done := make(chan struct{})
		go func() {
			defer close(done)
			p.soleFacade().handleHTTP(httptest.NewRecorder(), httptest.NewRequest(
				http.MethodPost, tc.inferencePath,
				strings.NewReader(fmt.Sprintf(`{"model":%q}`, tc.requestedModel))))
		}()

		select {
		case <-received:
		case <-time.After(5 * time.Second):
			require.FailNow(t, "upstream never received the forwarded request")
		}

		// Give the proxy the same window the positive test uses, so a report
		// would have landed by now if one were going to.
		time.Sleep(200 * time.Millisecond)
		require.NotContains(t, rec.String(), `"method":"node/activity"`, "activity was reported for a node that had not sent a single response byte")

		doRelease()
		<-done
	})
}

// A streaming generation writes hundreds of chunks. Reporting each one would put
// thousands of frames a minute on the broker pipe for no gain, since the scanner
// treats one report as good for a minute.
func TestRepeatedChunksAreCoalescedIntoOneReport(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		chunks := 40
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			for i := 0; i < chunks; i++ {
				_, err := w.Write([]byte(`{"response":"x"}` + "\n"))
				assert.NoError(t, err)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		}))
		defer upstream.Close()

		rec := &recRW{}
		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "streaming-node", upstream.URL, tc.advertisedModel))
		p := newTestProxy(tc.profile, NewCodec(rec), disc, tc.profile.FacadePort)

		p.soleFacade().handleHTTP(httptest.NewRecorder(), httptest.NewRequest(
			http.MethodPost, tc.inferencePath,
			strings.NewReader(fmt.Sprintf(`{"model":%q,"stream":true}`, tc.requestedModel))))

		require.True(t, waitFor(t, rec, `"method":"node/activity"`), "a streamed response reported no activity at all")
		require.Equal(t, 1, rec.count(`"method":"node/activity"`), "want one activity report for %d chunks within the throttle interval", chunks)
	})
}
