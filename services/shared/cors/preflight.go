// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cors

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

type Target struct {
	URL       *url.URL
	Transport http.RoundTripper
}

// ServePreflight queries a snapshot of routable targets without discovery,
// scheduling, redirects, or cached permissions.
func ServePreflight(w http.ResponseWriter, r *http.Request, targets []Target) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	if len(targets) == 0 {
		http.Error(w, "no available engine for preflight", http.StatusBadGateway)
		return
	}
	if len(targets) == 1 {
		target := targets[0]
		proxy := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme, req.URL.Host, req.Host = target.URL.Scheme, target.URL.Host, target.URL.Host
			},
			Transport: target.Transport,
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
				http.Error(w, "engine unavailable for preflight", http.StatusBadGateway)
			},
		}
		proxy.ServeHTTP(w, r)
		return
	}
	responses := make([]http.Header, len(targets))
	unavailable := make([]bool, len(targets))
	denied := make([]bool, len(targets))
	querySlots := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Add(1)
		go func(i int, target Target) {
			defer wg.Done()
			select {
			case querySlots <- struct{}{}:
				defer func() { <-querySlots }()
			case <-ctx.Done():
				unavailable[i] = true
				return
			}
			req := r.Clone(ctx)
			req.URL.Scheme, req.URL.Host, req.Host = target.URL.Scheme, target.URL.Host, target.URL.Host
			req.RequestURI = ""
			req.Header = FanoutHeaders(r.Header)
			req.Body, req.GetBody, req.ContentLength, req.TransferEncoding = nil, nil, 0, nil
			req.Header.Del("Content-Length")
			transport := target.Transport
			if transport == nil {
				transport = http.DefaultTransport
			}
			resp, err := transport.RoundTrip(req)
			if err != nil {
				unavailable[i] = true
				return
			}
			defer resp.Body.Close()
			responses[i] = EndToEndHeaders(resp.Header)
			// A server error is an engine that could not answer, not one that
			// refused. Keeping these disjoint is what lets the filter below
			// drop silence without dropping a refusal.
			unavailable[i] = resp.StatusCode >= 500
			denied[i] = !unavailable[i] && (resp.StatusCode < 200 || resp.StatusCode >= 300)
		}(i, target)
	}
	wg.Wait()
	for _, rejected := range denied {
		if rejected {
			http.Error(w, "engine denied preflight", http.StatusForbidden)
			return
		}
	}
	// An engine that could not answer expressed no opinion, and the request
	// being preflighted would route around it anyway. Intersecting its absent
	// headers would let one offline node deny the browser every online one.
	// Only a total blackout is an error.
	answered := make([]http.Header, 0, len(responses))
	for i, h := range responses {
		if !unavailable[i] {
			answered = append(answered, h)
		}
	}
	if len(answered) == 0 {
		http.Error(w, "engine unavailable for preflight", http.StatusBadGateway)
		return
	}
	headers, ok := Combine(r, answered)
	if !ok {
		http.Error(w, "engines do not all permit this preflight", http.StatusForbidden)
		return
	}
	for key, values := range headers {
		w.Header()[key] = values
	}
	w.WriteHeader(http.StatusNoContent)
}
