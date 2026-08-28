// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export function buildCspHeader(): string {
    const connectSrc = ["'self'", 'http://127.0.0.1:*', 'http://localhost:*']
    return (
        [
            "default-src 'none'",
            "script-src 'self'",
            "style-src 'self' 'unsafe-inline'",
            `connect-src ${connectSrc.join(' ')}`,
            // `blob:` is needed so the SVG rasterizer can assign a blob URL
            // to `<img>` for decode (see `extract-svg.ts`). `data:` covers
            // the inline previews we generate from base64 attachments.
            "img-src 'self' data: blob:",
            "media-src 'self' blob:",
            // The vendored Kaizen CSS resolves NVIDIA Sans through local()
            // fallbacks only, so no remote font origin is needed.
            "font-src 'self'",
            "manifest-src 'self'",
            // `'self'` covers the Vite-served pdf.js worker module; `blob:`
            // is the pdf.js fallback when the worker module can't be loaded
            // directly (some Chromium builds spin up a blob-URL shim).
            "worker-src 'self' blob:",
            "object-src 'none'",
            "frame-src 'none'",
            "base-uri 'self'",
            "form-action 'self'"
        ].join('; ') + ';'
    )
}
