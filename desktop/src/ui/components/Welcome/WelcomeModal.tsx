// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { ModalContent, ModalDialog, ModalRoot, Stack, Text } from '@nvidia/foundations-react-core'
import { DialogHeader } from '@/ui/components/DialogHeader'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import { useBlurOnOpen } from '@/ui/hooks/useBlurOnOpen'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import { useErrorsStore } from '@/ui/stores/errors.store'
import { useOverviewUiStore } from '@/ui/stores/overview-ui.store'
import type { EngineType } from '@/shared/types/engines'
import {
    getWelcomeEngineCandidates,
    WELCOME_ENGINE_DEFAULT_SELECTED,
    WELCOME_STEP_HEADINGS,
    WELCOME_STEP_SUB_HEADINGS
} from '@/ui/constants/welcome'
import { WelcomeIntroStep } from './WelcomeIntroStep'
import { WelcomeModalActions } from './WelcomeModalActions'
import { WelcomeEnginesStep } from './WelcomeEnginesStep'
import {
    areWelcomeEnginesInstalled,
    getWelcomeInstallOutcome,
    isWelcomeEngineInstalled,
    isWelcomeEngineInstallable,
    isTargetInstallError,
    targetForInstallError
} from '@/ui/utils/welcome-install'

interface WelcomeModalProps {
    open: boolean
    onOpenChange: (open: boolean) => void
    selfId: string
}

export function WelcomeModal({ open, onOpenChange, selfId }: WelcomeModalProps) {
    useBlurOnOpen(open)
    const statusByNode = useEngineStatusStore(s => s.statusByNode)
    const errors = useErrorsStore(s => s.errors)
    const focusNodeEngineSettings = useOverviewUiStore(s => s.focusNodeEngineSettings)

    const [step, setStep] = useState(0)
    const [engineSelections, setEngineSelections] = useState<Partial<
        Record<EngineType, boolean>
    > | null>(null)
    const [installing, setInstalling] = useState(false)
    const [installTargets, setInstallTargets] = useState<EngineType[]>([])
    const [error, setError] = useState<string | null>(null)
    const startedTargets = useRef(new Set<EngineType>())
    const failedTargets = useRef(new Set<EngineType>())
    const knownErrors = useRef(new Map<string, number>())

    const candidates = useMemo(() => getWelcomeEngineCandidates(window.windowApi.platform), [])
    const allEnginesInstalled = useMemo(
        () => areWelcomeEnginesInstalled(statusByNode, selfId, candidates),
        [candidates, selfId, statusByNode]
    )

    useEffect(() => {
        if (!open) {
            setStep(0)
            setEngineSelections(null)
            setError(null)
            setInstalling(false)
            setInstallTargets([])
            startedTargets.current.clear()
            failedTargets.current.clear()
        }
    }, [open])

    useEffect(() => {
        if (!open || step !== 1) return
        if (engineSelections !== null) return
        const initial: Partial<Record<EngineType, boolean>> = {}
        for (const t of candidates) {
            initial[t] = WELCOME_ENGINE_DEFAULT_SELECTED[t] ?? false
        }
        setEngineSelections(initial)
    }, [open, step, candidates, engineSelections])

    const isEngineInstalled = useCallback(
        (t: EngineType) => {
            const status = statusByNode.get(selfId)?.get(t)?.processStatus
            return isWelcomeEngineInstalled(status)
        },
        [statusByNode, selfId]
    )

    const isEngineInstallable = useCallback(
        (t: EngineType) => {
            const status = statusByNode.get(selfId)?.get(t)?.processStatus
            return isWelcomeEngineInstallable(status)
        },
        [statusByNode, selfId]
    )

    /**
     * Release `open` in the same tick the close is requested. The modal's
     * `open` prop must never lag the dialog's own close sequence: while it
     * does, the primitive finishes closing, sees `open` still true, and
     * re-opens the dialog — replaying the backdrop animation.
     */
    const finishWelcome = useCallback(
        (focusSettings: boolean) => {
            onOpenChange(false)
            if (focusSettings) focusNodeEngineSettings(selfId)
            void window.windowApi.completeFirstRun()
        },
        [focusNodeEngineSettings, onOpenChange, selfId]
    )

    /** An in-flight install keeps running in the background after dismissal. */
    const handleDismiss = useCallback(() => {
        finishWelcome(false)
    }, [finishWelcome])

    const handleOpenChange = useCallback(
        (nextOpen: boolean) => {
            if (!nextOpen) handleDismiss()
        },
        [handleDismiss]
    )

    const handleEngineToggle = useCallback((engineType: EngineType, checked: boolean) => {
        setEngineSelections(prev => ({
            ...(prev ?? {}),
            [engineType]: checked
        }))
    }, [])

    const handleInstall = useCallback(() => {
        if (!engineSelections) {
            return
        }

        setError(null)
        const targets = candidates.filter(t => engineSelections[t] && isEngineInstallable(t))
        if (targets.length === 0) {
            finishWelcome(true)
            return
        }
        setInstalling(true)
        startedTargets.current.clear()
        failedTargets.current.clear()
        knownErrors.current = new Map(errors.map(item => [item.id, item.timestamp]))
        setInstallTargets(targets)
        targets.forEach(t => window.pairApi.engines.install(t, selfId))
    }, [candidates, engineSelections, selfId, isEngineInstallable, finishWelcome, errors])

    useEffect(() => {
        if (!installing || installTargets.length === 0) return

        const statuses = installTargets.map(engineType => ({
            engineType,
            status: statusByNode.get(selfId)?.get(engineType)?.processStatus ?? 'not-installed'
        }))
        for (const { engineType, status } of statuses) {
            if (status === 'installing') startedTargets.current.add(engineType)
        }

        const newInstallErrors = errors.filter(
            item =>
                (knownErrors.current.get(item.id) ?? -1) < item.timestamp &&
                isTargetInstallError(item, selfId, installTargets)
        )
        for (const installError of newInstallErrors) {
            knownErrors.current.set(installError.id, installError.timestamp)
            const target = targetForInstallError(installError, installTargets)
            if (target) failedTargets.current.add(target)
            else installTargets.forEach(engineType => failedTargets.current.add(engineType))
        }
        if (newInstallErrors[0]) setError(newInstallErrors[0].message)

        const outcome = getWelcomeInstallOutcome(
            statuses,
            startedTargets.current,
            failedTargets.current
        )
        if (outcome === 'failed') {
            setInstalling(false)
            setError(current => current ?? 'Engine installation failed. Please try again.')
            return
        }
        if (outcome !== 'complete') return

        setInstalling(false)
        finishWelcome(true)
    }, [errors, finishWelcome, installTargets, installing, selfId, statusByNode])

    const heading = WELCOME_STEP_HEADINGS[step] ?? WELCOME_STEP_HEADINGS[0]
    const subHeading = WELCOME_STEP_SUB_HEADINGS[step]

    return (
        <ModalRoot open={open} onOpenChange={handleOpenChange} hideCloseButton>
            <ModalDialog>
                <ModalContent className="no-drag-elements max-w-xl">
                    <DialogHeader onClose={handleDismiss}>
                        <Stack gap="0">
                            <Text kind="title/sm" className="pr-6">
                                {heading}
                            </Text>
                            {subHeading && (
                                <Text kind="body/regular/sm" className="text-subtle-color">
                                    {subHeading}
                                </Text>
                            )}
                        </Stack>
                    </DialogHeader>
                    <Stack gap="4" className="max-h-[min(70vh,520px)] overflow-y-auto pt-2">
                        {error && (
                            <InlineErrorBanner message={error} onClose={() => setError(null)} />
                        )}

                        {step === 0 && (
                            <WelcomeIntroStep allEnginesInstalled={allEnginesInstalled} />
                        )}

                        {step === 1 && (
                            <WelcomeEnginesStep
                                candidates={candidates}
                                nodeId={selfId}
                                engineSelections={engineSelections}
                                installing={installing}
                                onEngineToggle={handleEngineToggle}
                                isEngineInstalled={isEngineInstalled}
                            />
                        )}

                        <WelcomeModalActions
                            step={step}
                            installing={installing}
                            allEnginesInstalled={allEnginesInstalled}
                            onStepIntroNext={() => setStep(1)}
                            onClose={handleDismiss}
                            onInstall={handleInstall}
                        />
                    </Stack>
                </ModalContent>
            </ModalDialog>
        </ModalRoot>
    )
}
