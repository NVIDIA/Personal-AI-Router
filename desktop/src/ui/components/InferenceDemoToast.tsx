// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import {
    Button,
    ToastActions,
    ToastContent,
    ToastRoot,
    ToastText
} from '@nvidia/foundations-react-core'
import { useInferenceDemoStore } from '@/ui/stores/inference-demo.store'
import { useErrorsStore } from '@/ui/stores/errors.store'
import getErrorString from '@/shared/utils/get-error-string'
import type { DemoState } from '@/shared/types/inference-demo'

/**
 * Persistent status toast for a running Inference Demo.
 *
 * Mounted once at the app root so it survives tab changes — the user starts the
 * demo from Settings, gets moved to Overview to watch job activity, and the
 * toast follows them either way. It carries the only Stop affordance during a run.
 *
 * Deliberately minimal: the spec forbids showing prompts, responses, findings,
 * scores, or any result summary, so this reports nothing but "what is running".
 * It disappears the moment the demo ends, whether that was Stop or the end of
 * the schedule — there is no "finishing up" tail.
 *
 * Built from the composed Toast primitives rather than the `Toast` shorthand
 * specifically to drop the status icon: `Toast` resolves its icon as
 * `slotIcon || <ToastIcon/>`, so a falsy `slotIcon` still falls back to the
 * default. Omitting `ToastIcon` here is the only way to render none.
 */

/** Visible label. One word plus progress; no counter before a schedule exists. */
function label(state: DemoState): string {
    if (state.status === 'preparing') return 'Working'
    return `Working (${state.submitted}/${state.planned})`
}

/** Full detail for the hover title. */
function detail(state: DemoState): string {
    if (state.status === 'preparing') return 'Checking local engines for available models'
    const targets = `${state.targetCount} model${state.targetCount === 1 ? '' : 's'} across ${state.engineCount} engine${state.engineCount === 1 ? '' : 's'}`
    return `Inference demo: ${state.submitted} of ${state.planned} requests submitted to ${targets}`
}

export function InferenceDemoToast() {
    const state = useInferenceDemoStore(store => store.state)
    const addLocalError = useErrorsStore(store => store.addLocalError)

    const handleStop = async () => {
        try {
            await window.windowApi.inferenceDemo.stop()
        } catch (err) {
            // Toasts must not carry errors, so this goes to the ordinary surface.
            addLocalError(getErrorString(err))
        }
    }

    if (state.status === 'idle') return null

    return (
        <div className="fixed bottom-6 left-1/2 z-50 w-90 max-w-[calc(100%-3rem)] -translate-x-1/2">
            {/*
             * ToastRoot renders a bare div with no ARIA, so the running demo is
             * otherwise silent to assistive tech — and this toast holds the only
             * Stop control in the app. Announce the coarse transition only: the
             * submitted counter changes ~60 times a run and would flood the
             * announcement queue, so it is hidden from the a11y tree below.
             */}
            <span className="sr-only" role="status" aria-live="polite">
                {state.status === 'preparing'
                    ? 'Inference demo starting'
                    : 'Inference demo running'}
            </span>
            <ToastRoot
                status="working"
                role="group"
                aria-label="Inference demo"
                title={detail(state)}
            >
                <ToastContent>
                    <ToastText aria-hidden="true">{label(state)}</ToastText>
                </ToastContent>
                <ToastActions>
                    <Button
                        kind="secondary"
                        color="danger"
                        size="small"
                        aria-label="Stop inference demo"
                        onClick={() => void handleStop()}
                    >
                        Stop
                    </Button>
                </ToastActions>
            </ToastRoot>
        </div>
    )
}
