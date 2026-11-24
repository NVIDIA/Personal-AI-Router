// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { act, createElement, type ComponentProps, type ReactNode } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { BannerProps, ButtonProps, DropdownProps } from '@nvidia/foundations-react-core'
import { EngineDisplayNames } from '@/shared/constants/engines'
import type { EngineProcessStatus, EngineType, ModelItemStatus } from '@/shared/types/engines'
import type { BackendInfo } from '@/ui/types/engine-info'
import type { IEngineApi } from '@/ui/api/engine-api'
import type { ConfirmModal } from '@/ui/components/ConfirmModal'
import type { ModelHubModal } from '@/ui/components/ModelHub/ModelHubModal'
import { ModelManager } from '@/ui/components/ModelManager/ModelManager'
import { usePendingActionsStore } from '@/ui/stores/pending-actions.store'

/**
 * What a model row offers for Delete, and what the confirmation says when it has
 * to ask. `modelDeleteMode` decides that from three inputs — the engine's declared
 * capabilities, whether the engine answered what it holds in memory, and whether
 * it answered *this* model — so these cases are the whole matrix, held together by
 * `tests/modular/delete-model-restart.test.ts` for the manifest half.
 *
 * The library dropdown is replaced with plain buttons so the menu items are
 * readable, and `ConfirmModal` with a passthrough that prints its own message and
 * label: the wording is a product decision and is asserted exactly, not by
 * `toContain`.
 */
vi.mock('@nvidia/foundations-react-core', () => {
    const container = ({ children }: { children?: ReactNode }) =>
        createElement('div', null, children)
    return {
        Flex: container,
        Stack: container,
        Text: container,
        Banner: ({ children, role }: BannerProps) => createElement('div', { role }, children),
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
    ConfirmModal: ({
        open,
        message,
        confirmLabel,
        onOpenChange,
        onConfirm
    }: ComponentProps<typeof ConfirmModal>) =>
        open
            ? createElement(
                  'div',
                  { role: 'dialog', 'aria-label': 'Delete confirmation' },
                  createElement('p', null, message),
                  createElement(
                      'button',
                      {
                          onClick: () => {
                              onOpenChange(false)
                              onConfirm()
                          }
                      },
                      confirmLabel
                  )
              )
            : null
}))

vi.mock('@/ui/components/ModelHub/ModelHubModal', () => ({
    ModelHubModal: ({ engine }: ComponentProps<typeof ModelHubModal>) =>
        engine ? createElement('div', { role: 'dialog', 'aria-label': 'Model catalog' }) : null
}))

const commands = {
    loadModel: vi.fn<IEngineApi['loadModel']>(),
    unloadModel: vi.fn<IEngineApi['unloadModel']>(),
    deleteModel: vi.fn<IEngineApi['deleteModel']>(),
    pullModel: vi.fn<IEngineApi['pullModel']>(),
    setModelExpiry: vi.fn<IEngineApi['setModelExpiry']>()
}

const NODE_ID = 'test-node'
/**
 * One pack name for every case. It has no `/`, so MTPLX renders it as written and
 * the confirmation copy can be asserted exactly; LM Studio's formatter turns its
 * dash into a space, which is what its own assertion spells out.
 */
const PACK = 'mtplx-lite'

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
    vi.clearAllMocks()
    usePendingActionsStore.setState({ pending: new Map() })
    Reflect.deleteProperty(window, 'pairApi')
    Reflect.deleteProperty(globalThis, 'IS_REACT_ACT_ENVIRONMENT')
})

/**
 * One row, one engine. `residencyKnown` is the third input and the one the rest of
 * the suite never varies: false means no source answered what the engine holds,
 * which is not the same answer as "holds nothing".
 */
async function renderRow(options: {
    type: EngineType
    residencyKnown: boolean
    status: ModelItemStatus
    processStatus?: EngineProcessStatus
}) {
    const backend: BackendInfo = {
        type: options.type,
        displayName: EngineDisplayNames[options.type],
        source: 'installed',
        processStatus: options.processStatus ?? 'running',
        port: 8101,
        proxyPort: 8100,
        modelsKnown: true,
        residencyKnown: options.residencyKnown,
        models: [
            {
                name: PACK,
                size: 1024,
                downloaded: true,
                status: options.status,
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
    await act(() => root.render(createElement(ModelManager, { backend, nodeId: NODE_ID })))
}

/** Menu items and dialogs both render as buttons; keep the two apart. */
function rowButtons(): HTMLButtonElement[] {
    return Array.from(host.querySelectorAll('button')).filter(
        button => !button.closest('[role="dialog"]')
    )
}

function rowButton(label: string): HTMLButtonElement {
    const found = rowButtons().find(button => button.textContent === label)
    if (!found) throw new Error(`Missing row button: ${label}`)
    return found
}

function dialog(): Element | null {
    return host.querySelector('[aria-label="Delete confirmation"]')
}

function dialogConfirm(): HTMLButtonElement {
    const button = dialog()?.querySelector('button')
    if (!button) throw new Error('Missing confirmation button')
    return button
}

async function clickRow(label: string) {
    await act(() => rowButton(label).click())
}

const QUIT_LABEL = 'Quit MTPLX and delete'
const QUIT_MESSAGE =
    `Deleting ${PACK} stops MTPLX first and leaves it down: it serves one model ` +
    'per process, so PAIR will not remove a pack it cannot rule out being the one served.'

describe('MTPLX delete affordance', () => {
    it('offers a one-click Delete for a pack the running engine says it is not serving', async () => {
        await renderRow({ type: 'mtplx', residencyKnown: true, status: 'idle' })

        expect(rowButton('Delete').disabled).toBe(false)
        await clickRow('Delete')

        expect(commands.deleteModel).toHaveBeenCalledExactlyOnceWith('mtplx', NODE_ID, PACK)
        expect(dialog()).toBeNull()
    })

    it('names the quit on the row whose pack the engine is serving', async () => {
        await renderRow({ type: 'mtplx', residencyKnown: true, status: 'loaded' })

        // Not disabled: unlike vLLM, MTPLX does remove a served pack, and the
        // label says so in the same breath as the warning.
        expect(rowButton(QUIT_LABEL).disabled).toBe(false)

        await clickRow(QUIT_LABEL)
        expect(commands.deleteModel).not.toHaveBeenCalled()

        await act(() => dialogConfirm().click())
        expect(commands.deleteModel).toHaveBeenCalledExactlyOnceWith('mtplx', NODE_ID, PACK)
        expect(dialog()).toBeNull()
    })

    it('refuses a one-click Delete on a row whose residency was never reported', async () => {
        await renderRow({ type: 'mtplx', residencyKnown: false, status: 'idle' })

        expect(() => rowButton('Delete')).toThrow('Missing row button: Delete')
        await clickRow(QUIT_LABEL)
        expect(commands.deleteModel).not.toHaveBeenCalled()
        expect(dialog()).not.toBeNull()
    })

    it('gives the unreadable residency the same affordance as the readable one', async () => {
        // The two states the service treats alike — `residencyMappedHere` and
        // `residencyUnknown` both stop the engine — must not split in the UI.
        await renderRow({ type: 'mtplx', residencyKnown: false, status: 'loaded' })
        expect(dialog()).toBeNull()
        const cannotTell = rowButtons().map(button => button.textContent)

        await renderRow({ type: 'mtplx', residencyKnown: true, status: 'loaded' })
        expect(rowButtons().map(button => button.textContent)).toEqual(cannotTell)
    })

    it('says what the delete does to the engine without claiming the pack is loaded', async () => {
        await renderRow({ type: 'mtplx', residencyKnown: false, status: 'idle' })
        await clickRow(QUIT_LABEL)

        expect(dialog()?.textContent).toContain(QUIT_MESSAGE)
        expect(dialogConfirm().textContent).toBe(QUIT_LABEL)
        // Nothing here asserts the pack is in memory, because on this row it may
        // not be, and it does not offer an eject or a restart that does not exist.
        expect(dialog()?.textContent).not.toMatch(/loaded|restart|eject/i)
    })
})

describe('engines that do not quit to delete', () => {
    it('keeps vLLM Delete disabled on the model it serves, without quit wording', async () => {
        await renderRow({ type: 'vllm', residencyKnown: true, status: 'loaded' })

        expect(rowButton('Delete').disabled).toBe(true)
        await clickRow('Delete')
        expect(commands.deleteModel).not.toHaveBeenCalled()
        expect(dialog()).toBeNull()
    })

    it('keeps LM Studio on the restart wording, which is its own kind of engine change', async () => {
        await renderRow({ type: 'lm-studio', residencyKnown: false, status: 'idle' })

        // Asks first, but its delete restarts rather than quits, so it keeps the
        // plain label and the restart sentence. The name is what the list shows,
        // which for LM Studio turns the pack's dash into a space.
        await clickRow('Delete')
        expect(dialog()?.textContent).toContain(
            'Deleting mtplx lite requires LM Studio to be restarted.'
        )
        expect(dialogConfirm().textContent).toBe('Delete and restart')

        await act(() => dialogConfirm().click())
        expect(commands.deleteModel).toHaveBeenCalledExactlyOnceWith('lm-studio', NODE_ID, PACK)
    })

    it.each<EngineType>(['ollama', 'llama-cpp', 'vllm'])(
        'lets %s delete straight away even when residency cannot be read',
        async type => {
            await renderRow({ type, residencyKnown: false, status: 'idle' })

            await clickRow('Delete')
            expect(commands.deleteModel).toHaveBeenCalledExactlyOnceWith(type, NODE_ID, PACK)
            expect(dialog()).toBeNull()
        }
    )
})

describe('a delete in flight', () => {
    it('locks the MTPLX row that is already deleting', async () => {
        usePendingActionsStore.setState({
            pending: new Map([
                [
                    `${NODE_ID}:mtplx:model:${PACK}`,
                    {
                        nodeId: NODE_ID,
                        engineType: 'mtplx',
                        action: 'deleteModel',
                        model: PACK,
                        expiresAt: Date.now() + 60_000
                    }
                ]
            ])
        })
        await renderRow({ type: 'mtplx', residencyKnown: true, status: 'loaded' })

        expect(rowButton(QUIT_LABEL).disabled).toBe(true)
        await clickRow(QUIT_LABEL)
        expect(commands.deleteModel).not.toHaveBeenCalled()
    })
})
