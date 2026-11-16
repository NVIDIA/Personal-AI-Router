// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export function buildCspHeader(): string {
    // https://cheatsheetseries.owasp.org/cheatsheets/Content_Security_Policy_Cheat_Sheet.html
    // https://www.w3.org/TR/CSP/
    return (
        [
            "default-src 'none'",
            "script-src 'self'",
            "style-src 'self' 'unsafe-inline'",
            "connect-src 'self'",
            // `blob:` is needed so the SVG rasterizer can assign a blob URL
            // to `<img>` for decode (see `extract-svg.ts`). `data:` covers
            // the inline previews we generate from base64 attachments.
            "img-src 'self' data: blob:",
            "media-src 'self' blob:",
            // The vendored Kaizen CSS resolves NVIDIA Sans through local()
            // fallbacks only, so no remote font origin is needed.
            "font-src 'self'",
            "manifest-src 'self'",
            "worker-src 'none'",
            "object-src 'none'",
            "frame-src 'none'",
            "base-uri 'self'",
            "form-action 'self'"
        ].join('; ') + ';'
    )
}
