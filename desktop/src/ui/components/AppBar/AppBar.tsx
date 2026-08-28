// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * AppBar Component
 *
 * A reusable application bar component that provides a title bar with a close button.
 * This component is typically used at the top of application windows to provide
 * window controls and title display.
 */

import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react'
import { Button, Divider, Flex, Text } from '@nvidia/foundations-react-core'
import { Close, Minimize, RestoreWindow, Square } from '@/ui/components/icons'

import './AppBar.css'

/**
 * Props for the AppBar component
 * @interface AppBarProps
 * @property {string} title - The title text to display in the app bar
 * @property {() => void} [onClose] - Optional callback function to handle window close action
 */
interface AppBarProps {
    title: string
    slotTitleRight?: ReactNode
    slotTitleLeft?: ReactNode
    onClose?: () => void
    divider?: boolean
    dividerFullWidth?: boolean
}

/**
 * AppBar Component
 *
 * Renders a title bar with a draggable region and close button.
 * The close button can either trigger a custom onClose callback or
 * use the default window close behavior.
 *
 * @param {AppBarProps} props - The component props
 * @returns The rendered AppBar component
 */
export function AppBar({
    title,
    slotTitleRight,
    slotTitleLeft,
    onClose,
    divider,
    dividerFullWidth = false
}: AppBarProps) {
    const isElectron = useMemo(() => !!window && typeof window.windowApi !== 'undefined', [])
    const headerClassName = useMemo(() => (isElectron ? 'title-bar drag-region' : ''), [isElectron])
    const nonDraggableClassName = useMemo(
        () => (isElectron ? 'no-drag-elements' : ''),
        [isElectron]
    )
    const showCloseButton = useMemo(() => (isElectron ? true : false), [isElectron])
    const [isMaximized, setIsMaximized] = useState(false)

    useEffect(() => {
        if (!isElectron) return
        const api = window.windowApi?.window
        if (!api?.isMaximized || !api.onMaximizedChanged) return
        void api.isMaximized().then(setIsMaximized)
        return api.onMaximizedChanged(setIsMaximized)
    }, [isElectron])

    const handleClose = useCallback(() => {
        if (typeof onClose === 'function') {
            onClose()
        } else {
            window?.windowApi?.window?.close?.()
        }
    }, [onClose])

    const handleMinimize = useCallback(() => {
        void window.windowApi?.window?.minimize?.()
    }, [])

    const handleToggleMaximize = useCallback(() => {
        const w = window.windowApi?.window
        if (!w) return
        if (isMaximized) void w.unmaximize()
        else void w.maximize()
    }, [isMaximized])

    return (
        <header className={`${headerClassName} w-full px-6 pt-6 pb-5 relative box-border`}>
            <Flex justify="between" align="center" gap="3">
                {/* Draggable region for window movement */}
                <Flex align="center" gap="2" className="grow min-w-0">
                    {slotTitleLeft && (
                        <Flex align="center" className={nonDraggableClassName}>
                            {slotTitleLeft}
                        </Flex>
                    )}
                    <Text kind="body/bold/lg" className="capitalize truncate">
                        {title}
                    </Text>
                    {slotTitleRight && (
                        <Flex align="center" className={nonDraggableClassName}>
                            {slotTitleRight}
                        </Flex>
                    )}
                </Flex>

                <Flex align="center" justify="end" gap="0" className="pointer-events-auto -mt-1">
                    {showCloseButton && (
                        <Button
                            type="button"
                            className={`${nonDraggableClassName} cursor-pointer `}
                            kind="tertiary"
                            onClick={handleMinimize}
                            size="small"
                            title="Minimize"
                            aria-label="Minimize"
                        >
                            <Minimize style={{ fontSize: 16 }} />
                        </Button>
                    )}

                    {showCloseButton && !isMaximized && (
                        <Button
                            type="button"
                            className={`${nonDraggableClassName} cursor-pointer `}
                            kind="tertiary"
                            onClick={handleToggleMaximize}
                            size="small"
                            title="Maximize"
                            aria-label="Maximize"
                        >
                            <Square style={{ fontSize: 14 }} />
                        </Button>
                    )}

                    {showCloseButton && isMaximized && (
                        <Button
                            type="button"
                            className={`${nonDraggableClassName} cursor-pointer`}
                            kind="tertiary"
                            onClick={handleToggleMaximize}
                            size="small"
                            title="Restore down"
                            aria-label="Restore down"
                        >
                            <RestoreWindow style={{ fontSize: 14 }} />
                        </Button>
                    )}

                    {showCloseButton && (
                        <Button
                            type="button"
                            className={`${nonDraggableClassName} cursor-pointer`}
                            kind="tertiary"
                            onClick={handleClose}
                            size="small"
                            title="Close"
                            aria-label="Close"
                        >
                            <Close style={{ fontSize: 16 }} />
                        </Button>
                    )}
                </Flex>
            </Flex>
            {divider && (
                <Divider
                    className={
                        dividerFullWidth ? 'w-[calc(100%+48px)] mx-[-24px] mb-[-12px] mt-2' : ''
                    }
                />
            )}
        </header>
    )
}
