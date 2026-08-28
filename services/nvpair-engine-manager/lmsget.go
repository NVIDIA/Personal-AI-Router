// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/url"
	"strings"
)

// modelResolutionLMSGet is the only supported Action.ModelResolution
// strategy. It expands a requested model into the ordered list of
// arguments `lms get` understands, working around the fact that the CLI
// routes a bare "owner/name" to the LM Studio Hub artifact registry —
// which does not have the Hugging-Face-hosted community models LM Studio's
// own Discover tab downloads. See lmsGetCandidates.
const modelResolutionLMSGet = "lms-get"

// lmsGetCandidates returns the ordered, de-duplicated list of arguments to
// try with `lms get` for a requested model. The order mirrors how LM
// Studio's app resolves a model, with safe fallbacks:
//
//  1. The value exactly as given — so an explicit huggingface.co or
//     lmstudio.ai URL is honored verbatim, first.
//  2. The LM Studio Hub artifact id (bare "owner/name").
//  3. The Hugging Face repo URL ("https://huggingface.co/owner/name").
//
// Candidates that resolve to the same `lms get` resolver (e.g. a
// lmstudio.ai URL and the bare "owner/name" it denotes) are collapsed so
// the same query never runs twice. Any "@quant" qualifier is preserved. A
// search term (no derivable owner/name) or an unrecognized URL is returned
// as a single verbatim candidate.
func lmsGetCandidates(model string) []string {
	m := strings.TrimSpace(model)
	if m == "" {
		return nil
	}

	var out []string
	seen := map[string]bool{}
	push := func(arg, key string) {
		if arg == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, arg)
	}

	// 1) Whatever they gave us, verbatim (honors an explicit URL first).
	push(m, lmsResolverKey(m))

	// 2) Hub artifact id, then 3) Hugging Face repo URL.
	if owner, name, ok := lmsOwnerName(m); ok {
		on := owner + "/" + name
		lower := strings.ToLower(on)
		push(on, "hub:"+lower)
		push("https://huggingface.co/"+on, "hf:"+lower)
	}
	return out
}

// lmsOwnerName extracts the (owner, name) pair from a model reference when
// one can be determined: a bare "owner/name", a huggingface.co URL, or an
// lmstudio.ai[/models]/owner/name URL. Any trailing "@quant" on the name
// is preserved. ok is false for search terms (no slash) and for URLs whose
// host/shape we don't recognize.
func lmsOwnerName(m string) (owner, name string, ok bool) {
	if isHTTPURL(m) {
		u, err := url.Parse(m)
		if err != nil {
			return "", "", false
		}
		segs := pathSegments(u.Path)
		switch strings.ToLower(u.Hostname()) {
		case "huggingface.co", "www.huggingface.co":
			if len(segs) >= 2 {
				return segs[0], segs[1], true
			}
		case "lmstudio.ai", "www.lmstudio.ai":
			if len(segs) >= 1 && strings.EqualFold(segs[0], "models") {
				segs = segs[1:]
			}
			if len(segs) >= 2 {
				return segs[0], segs[1], true
			}
		}
		return "", "", false
	}
	// Bare reference: exactly two non-empty path segments ("owner/name").
	if segs := pathSegments(m); len(segs) == 2 {
		return segs[0], segs[1], true
	}
	return "", "", false
}

// lmsResolverKey classifies a model reference by which `lms get` resolver
// it targets, so equivalent candidates de-duplicate. A bare "owner/name"
// and an lmstudio.ai URL both denote the same Hub artifact and share a
// key; a huggingface.co URL keys to that repo.
func lmsResolverKey(m string) string {
	if owner, name, ok := lmsOwnerName(m); ok {
		on := strings.ToLower(owner + "/" + name)
		if isHTTPURL(m) {
			if u, err := url.Parse(m); err == nil && strings.Contains(strings.ToLower(u.Hostname()), "huggingface.co") {
				return "hf:" + on
			}
		}
		return "hub:" + on
	}
	if isHTTPURL(m) {
		return "url:" + strings.ToLower(m)
	}
	return "search:" + strings.ToLower(m)
}

// isLMSResolveFailure reports whether an `lms get` error is a model
// *resolution* failure (the artifact/repo could not be found) as opposed
// to a download or other runtime error. Only resolution failures are
// worth retrying against a different source — retrying a different source
// after a partial download wouldn't help and could waste bandwidth.
func isLMSResolveFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, sig := range []string{
		"failed to resolve artifact",
		"does not exist or you do not have permission",
		"the artifact does not exist",
		"not supported in lm studio",
		"no models found",
	} {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

// isLMSTransientDownloadError reports whether an `lms get` failure looks
// like a transient download problem — a stalled or timed-out transfer, or a
// dropped connection — as opposed to a permanent one (artifact missing,
// disk full, bad auth). `lms get` resumes a partial download on re-run, so a
// transient failure is worth retrying the same invocation in place;
// permanent ones are not. The signatures are LM Studio's own CLI wording
// ("Download failed: Timed-out. Please try to resume.") plus the common
// socket/fetch error codes it surfaces. A resolution failure is never
// transient — the candidate loop tries another source for those.
func isLMSTransientDownloadError(err error) bool {
	if err == nil || isLMSResolveFailure(err) {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, sig := range []string{
		"download failed",
		"timed-out",
		"timed out",
		"try to resume",
		"resume the download",
		"connection reset",
		"econnreset",
		"etimedout",
		"esockettimedout",
		"socket hang up",
		"network error",
		"fetch failed",
	} {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

// cmdReferencesModel reports whether any arg in a cmd action templates the
// {model} placeholder — a precondition for lms-get model resolution.
func cmdReferencesModel(cmd []string) bool {
	for _, a := range cmd {
		if strings.Contains(a, "{model}") {
			return true
		}
	}
	return false
}

func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// pathSegments splits a "/"-delimited path (or bare reference) into its
// non-empty segments.
func pathSegments(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
