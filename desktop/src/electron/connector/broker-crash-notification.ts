// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { createStructuredLogger } from '@/shared/utils/log'
import { showOverviewMessage } from '@/electron/window'
import { APP_DISPLAY_NAME } from '@/shared/constants/app'

const log = createStructuredLogger('app')

/**
 * Surface an unexpected broker exit as an in-Overview message modal. The action
 * opens the Settings Service tab, where the user can restart the service via the
 * Start/Restart buttons.
 */
export function notifyBrokerCrash(): void {
    showOverviewMessage({
        id: 'broker-crash',
        kind: 'service',
        title: `${APP_DISPLAY_NAME} service stopped`,
        body: 'The background service that powers local AI exited unexpectedly. Open Settings to restart it.',
        actionLabel: 'Show',
        action: 'open-service'
    })
    log.info({ sublevel: 'lifecycle', message: 'Surfaced broker-crash message' })
}

/**
 * A startup failure has no usable Overview data to show, so the message's action
 * takes the user to the Service settings where the logs and retry controls live.
 */
export function notifyBrokerStartupFailure(error: string): void {
    showOverviewMessage({
        id: 'broker-startup-failed',
        kind: 'service',
        title: `${APP_DISPLAY_NAME} service failed to start`,
        body: `The background service did not become ready. Review the Service settings and logs, then try again. ${error}`,
        actionLabel: 'Show',
        action: 'open-service'
    })
    log.info({ sublevel: 'lifecycle', message: 'Surfaced broker startup failure' })
}
