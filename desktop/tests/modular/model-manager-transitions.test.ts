// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { act, createElement, type ComponentProps, type ReactNode } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ButtonProps, DropdownProps } from '@nvidia/foundations-react-core'
import { EngineDisplayNames, EngineTypes } from '@/shared/constants/engines'
import type { EngineProcessStatus, EngineType, ModelItemStatus } from '@/shared/types/engines'
import type { BackendInfo } from '@/ui/types/engine-info'
import type { IEngineApi } from '@/ui/api/engine-api'
import type { ConfirmModal } from '@/ui/components/ConfirmModal'
import type { ModelHubModal } from '@/ui/components/ModelHub/ModelHubModal'
import { ModelManager } from '@/ui/components/ModelManager/ModelManager'
import { useEngineProgressStore } from '@/ui/stores/engine-progress.store'
import { usePendingActionsStore } from '@/ui/stores/pending-actions.store'

// Keep React state and the model components real. Replace library presentation
// and network-backed dialogs with native controls so assertions cover the
// component's lifecycle and commands without depending on popover positioning.
vi.mock('@nvidia/foundations-react-core', () => {
    const container = ({ children }: { children?: ReactNode }) =>
        createElement('div', null, children)
    return {
        Flex: container,
        Stack: container,
        Text: container,
        Divider: () => createElement('hr'),
        Button: ({ children, disabled, onClick }: ButtonProps) =>
            createElement('button', { disabled, onClick }, children),
        Dropdown: ({ items, children, disabled, 'aria-label': label }: DropdownProps) =>
            createElement(
                'div',
                null,
                createElement('button', { disabled, 'aria-label': label }, children),
                items.map((item, index) => {
                    if (typeof item === 'string' || !('onSelect' in item)) return null
                    return createElement(
                        'button',
                        {
                            key: index,
                            disabled: disabled || item.disabled,
                            onClick: () => item.onSelect?.(new Event('select'))
                        },
                        item.children
                    )
                })
            )
    }
})

vi.mock('@/ui/components/ConfirmModal', () => ({
    ConfirmModal: ({ open, onOpenChange, onConfirm }: ComponentProps<typeof ConfirmModal>) =>
        open
            ? createElement(
                  'div',
                  { role: 'dialog', 'aria-label': 'Delete confirmation' },
                  createElement(
                      'button',
                      {
                          onClick: () => {
                              onOpenChange(false)
                              onConfirm()
                          }
                      },
                      'Delete and restart'
                  )
              )
            : null
}))

vi.mock('@/ui/components/ModelHub/ModelHubModal', () => ({
    ModelHubModal: ({ engine, onSubmit }: ComponentProps<typeof ModelHubModal>) =>
        engine
            ? createElement(
                  'div',
                  { role: 'dialog', 'aria-label': 'Model catalog' },
                  createElement(
                      'button',
                      {
                          onClick: () =>
                              onSubmit([
                                  {
                                      id: 'org/new-model',
                                      name: 'org/new-model',
                                      author: 'org',
                                      url: 'https://example.invalid/model',
                                      updatedAt: new Date('2026-01-01')
                                  }
                              ])
                      },
                      'Download selected model'
                  )
              )
            : null
}))

const commands = {
    loadModel: vi.fn<IEngineApi['loadModel']>(),
    unloadModel: vi.fn<IEngineApi['unloadModel']>(),
    deleteModel: vi.fn<IEngineApi['deleteModel']>(),
    pullModel: vi.fn<IEngineApi['pullModel']>(),
    setModelExpiry: vi.fn<IEngineApi['setModelExpiry']>()
}
let host: HTMLDivElement
let root: Root

beforeEach(() => {
    Object.defineProperty(globalThis, 'IS_REACT_ACT_ENVIRONMENT', {
        configurable: true,
        value: true
    })
    Object.defineProperty(window, 'pairApi', { configurable: true, value: { engines: commands } })
    host = document.createElement('div')
    document.body.append(host)
    root = createRoot(host)
})

afterEach(async () => {
    await act(() => root.unmount())
    host.remove()
    useEngineProgressStore.setState({ progress: new Map() })
    usePendingActionsStore.setState({ pending: new Map() })
    Reflect.deleteProperty(window, 'pairApi')
    Reflect.deleteProperty(globalThis, 'IS_REACT_ACT_ENVIRONMENT')
})

function backend(
    type: EngineType,
    processStatus: EngineProcessStatus,
    status: ModelItemStatus
): BackendInfo {
    return {
        type,
        displayName: EngineDisplayNames[type],
        source: 'installed',
        processStatus,
        port: 8001,
        proxyPort: 8000,
        modelsKnown: true,
        models: [
            {
                name: 'org/cached-model',
                size: 1024,
                downloaded: true,
                status,
                parameterSize: '1B',
                quantization: '',
                family: '',
                digest: '',
                sizeVram: null,
                expiresAt: null,
                expiry: '10m',
                capabilities: []
            }
        ]
    }
}

async function render(
    type: EngineType,
    processStatus: EngineProcessStatus,
    status: ModelItemStatus = 'idle'
) {
    await act(() =>
        root.render(
            createElement(ModelManager, {
                backend: backend(type, processStatus, status),
                nodeId: 'test-node'
            })
        )
    )
}

function button(label: string): HTMLButtonElement {
    const found = Array.from(host.querySelectorAll('button')).find(b => b.textContent === label)
    if (!found) throw new Error(`Missing button: ${label}`)
    return found
}

async function click(label: string) {
    await act(() => button(label).click())
}

describe.each(EngineTypes)('%s model lifecycle controls', engine => {
    it.each<EngineProcessStatus>(['stopped', 'stopping'])(
        'forgets an open catalog after %s, so restarting does not reopen it',
        async status => {
            await render(engine, 'running')
            await click('Add model')
            expect(host.querySelector('[aria-label="Model catalog"]')).not.toBeNull()

            await render(engine, status)
            expect(host.querySelector('[role="dialog"]')).toBeNull()
            await render(engine, 'running')

            expect(host.querySelector('[role="dialog"]')).toBeNull()
            await click('Add model')
            expect(host.querySelector('[aria-label="Model catalog"]')).not.toBeNull()
        }
    )

    it.each<EngineProcessStatus>(['starting', 'stopping', 'installing', 'uninstalling'])(
        'disables model controls during %s without sending commands',
        async status => {
            await render(engine, status)
            const controls = Array.from(host.querySelectorAll('button'))
            expect(controls.length).toBeGreaterThan(0)
            expect(controls.every(control => control.disabled)).toBe(true)

            await act(() => controls.forEach(control => control.click()))
            for (const command of Object.values(commands)) expect(command).not.toHaveBeenCalled()
            expect(host.querySelector('[role="dialog"]')).toBeNull()
        }
    )

    it('reenables Load after starting and sends the selected model', async () => {
        await render(engine, 'starting')
        await render(engine, 'running')
        expect(button('Load').disabled).toBe(false)
        await click('Load')
        expect(commands.loadModel).toHaveBeenCalledExactlyOnceWith(
            engine,
            'test-node',
            'org/cached-model'
        )
    })

    it('keeps Eject available only for a loaded model on a running engine', async () => {
        await render(engine, 'stopping', 'loaded')
        expect(button('Eject').disabled).toBe(true)
        await render(engine, 'running', 'loaded')
        expect(button('Eject').disabled).toBe(false)
        expect(button('Load').disabled).toBe(true)
        await click('Eject')
        expect(commands.unloadModel).toHaveBeenCalledExactlyOnceWith(
            engine,
            'test-node',
            'org/cached-model'
        )
    })

    it('downloads a selected model while running', async () => {
        await render(engine, 'running')
        await click('Add model')
        await click('Download selected model')
        expect(commands.pullModel).toHaveBeenCalledExactlyOnceWith(
            engine,
            'test-node',
            'org/new-model'
        )
        expect(host.querySelector('[role="dialog"]')).toBeNull()
    })
})

describe('LM Studio delete confirmation', () => {
    it.each<EngineProcessStatus>(['stopped', 'stopping'])(
        'forgets a pending deletion after %s',
        async status => {
            await render('lm-studio', 'running')
            await click('Delete')
            expect(host.querySelector('[aria-label="Delete confirmation"]')).not.toBeNull()

            await render('lm-studio', status)
            await render('lm-studio', 'running')

            expect(host.querySelector('[role="dialog"]')).toBeNull()
            expect(commands.deleteModel).not.toHaveBeenCalled()
        }
    )

    it('still deletes the chosen model after explicit confirmation', async () => {
        await render('lm-studio', 'running')
        await click('Delete')
        expect(commands.deleteModel).not.toHaveBeenCalled()
        await click('Delete and restart')
        expect(commands.deleteModel).toHaveBeenCalledExactlyOnceWith(
            'lm-studio',
            'test-node',
            'org/cached-model'
        )
        expect(host.querySelector('[role="dialog"]')).toBeNull()
    })
})

describe.each<EngineType>(['ollama', 'llama-cpp', 'vllm'])('%s direct deletion', engine => {
    it('deletes a downloaded model without a restart confirmation', async () => {
        await render(engine, 'running')
        await click('Delete')
        expect(commands.deleteModel).toHaveBeenCalledExactlyOnceWith(
            engine,
            'test-node',
            'org/cached-model'
        )
        expect(host.querySelector('[role="dialog"]')).toBeNull()
    })
})

describe('vLLM model controls', () => {
    it('keeps Load and Add model available with an unloaded model while running', async () => {
        await render('vllm', 'running')
        expect(button('Load').disabled).toBe(false)
        expect(button('Add model').disabled).toBe(false)
        await click('Load')
        expect(commands.loadModel).toHaveBeenCalledExactlyOnceWith(
            'vllm',
            'test-node',
            'org/cached-model'
        )
    })

    it('does not offer idle-timeout editing removed by the engine', async () => {
        await render('vllm', 'stopping')
        expect(host.textContent).not.toContain('Expires')
        await render('vllm', 'running')
        expect(host.textContent).not.toContain('Expires')
        expect(commands.setModelExpiry).not.toHaveBeenCalled()
    })

    it('continues to prevent deletion of the loaded model', async () => {
        await render('vllm', 'running', 'loaded')
        expect(button('Delete').disabled).toBe(true)
        await click('Delete')
        expect(commands.deleteModel).not.toHaveBeenCalled()
    })
})
