// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/** Vite `?raw` imports (markdown, etc.); must live under paths included by both node and web tsconfigs. */
declare module '*?raw' {
    const content: string
    export default content
}

/** Vite `?inline` assets → data URL string (base64 for binary images). */
declare module '*?inline' {
    const src: string
    export default src
}
