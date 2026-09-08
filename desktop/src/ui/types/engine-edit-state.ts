// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export type EditState = {
    serverPort: string
    proxyPort: string
    /**
     * Draft of the model an engine should serve, for an engine that runs one
     * model per process. Empty clears the choice.
     */
    servedModel: string
}
