// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import React from 'react'
import ReactDOM from 'react-dom/client'
import { setupApis } from '@/ui/api/bootstrap'
import { connectAndInitialize } from '@/ui/stores/init'
import { FoundationsThemeProvider } from '@/ui/theme/FoundationsThemeProvider'
import App from '@/ui/components/App'
import { APP_DISPLAY_NAME } from '@/shared/constants/app'
import '@/ui/styles/index.css'

setInterval(() => {
    try {
        performance.clearMarks()
        performance.clearMeasures()
    } catch {}
}, 30_000)

function start(): void {
    setupApis()

    ReactDOM.createRoot(document.getElementById('root') as HTMLElement).render(
        <React.StrictMode>
            <FoundationsThemeProvider>
                <App />
            </FoundationsThemeProvider>
        </React.StrictMode>
    )

    connectAndInitialize().catch(err =>
        console.error(`[${APP_DISPLAY_NAME}] Connection setup failed:`, err)
    )
}

start()
