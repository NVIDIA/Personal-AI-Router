// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import {
    act,
    createContext,
    createElement,
    Fragment,
    useContext,
    type ComponentProps,
    type ReactNode
} from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import type {
    ButtonProps,
    ModalHeadingProps,
    ModalRootProps,
    SwitchProps
} from '@nvidia/foundations-react-core'
import NodeEnginesInline from '@/ui/components/NodeList/NodeEnginesInline'
import { EngineModelSelectionModal } from '@/ui/components/ModelManager/EngineModelSelectionModal'
import type { ModelHubModal } from '@/ui/components/ModelHub/ModelHubModal'
import type {
    EngineProcessStatus,
    EngineStatusData,
    EngineType,
    ModelItem
} from '@/shared/types/engines'
import type { PreloadServiceTransport } from '@/shared/types/service-bridge'
import { createEngineApi, type IEngineApi } from '@/ui/api/engine-api'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useEngineModelsStore } from '@/ui/stores/engine-models.store'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { useOverviewUiStore } from '@/ui/stores/overview-ui.store'
import { usePendingActionsStore } from '@/ui/stores/pending-actions.store'
import { useErrorsStore } from '@/ui/stores/errors.store'

// Keep the selection flow and command API real; replace presentation and
// catalog fetching with native controls independent of browser dialog APIs.
vi.mock('@nvidia/foundations-react-core', () => {
    const headingContext = createContext<string | undefined>(undefined)
    const container = ({ children }: { children?: ReactNode }) =>
        createElement('div', null, children)
    return {
        Flex: container,
        Stack: container,
        Text: container,
        ModalDialog: ({ children }: { children?: ReactNode }) =>
            createElement(
                'div',
                { role: 'dialog', 'aria-labelledby': useContext(headingContext) },
                children
            ),
        ModalContent: container,
        ModalHeading: ({ children, id }: ModalHeadingProps) => {
            const headingId = useContext(headingContext)
            return createElement('h2', { id: id ?? headingId }, children)
        },
        ModalRoot: ({ children, id, open, onOpenChange }: ModalRootProps) =>
            open
                ? createElement(
                      headingContext.Provider,
                      { value: id ? `${id}-heading` : undefined },
                      createElement(
                          'div',
                          null,
                          children,
                          createElement(
                              'button',
                              { onClick: () => onOpenChange?.(false) },
                              'Close picker'
                          )
                      )
                  )
                : null,
        Button: ({ children, disabled, onClick, 'aria-label': label }: ButtonProps) =>
            createElement('button', { disabled, onClick, 'aria-label': label }, children),
        Switch: ({ checked, disabled, onCheckedChange, 'aria-label': label }: SwitchProps) =>
            createElement('input', {
                type: 'checkbox',
                role: 'switch',
                checked,
                disabled,
                'aria-label': label,
                onChange: event => onCheckedChange?.(event.currentTarget.checked)
            })
    }
})

vi.mock('@/ui/components/ModelHub/ModelHubModal', () => ({
    ModelHubModal: ({ engine, onOpenChange, onSubmit }: ComponentProps<typeof ModelHubModal>) =>
        engine
            ? createElement(
                  'div',
                  { role: 'dialog', 'aria-label': 'Model catalog' },
                  createElement('button', { onClick: () => onOpenChange(false) }, 'Close catalog'),
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

const nodeId = 'selection-action-node'
const transport: PreloadServiceTransport = {
    connected: true,
    invoke: vi
        .fn<PreloadServiceTransport['invoke']>()
        .mockRejectedValue(new Error('No service response')),
    subscribePush: vi.fn<PreloadServiceTransport['subscribePush']>(() => () => {}),
    onConnect: vi.fn<PreloadServiceTransport['onConnect']>(() => () => {}),
    onDisconnect: vi.fn<PreloadServiceTransport['onDisconnect']>(() => () => {}),
    onAuthFailure: vi.fn<PreloadServiceTransport['onAuthFailure']>(() => () => {}),
    destroy: vi.fn<PreloadServiceTransport['destroy']>()
}
const engineApi = createEngineApi(transport)
const commands = {
    toggle: vi.fn<IEngineApi['toggle']>(engineApi.toggle),
    install: vi.fn<IEngineApi['install']>(engineApi.install),
    stop: vi.fn<IEngineApi['stop']>(engineApi.stop),
    loadModel: vi.fn<IEngineApi['loadModel']>(engineApi.loadModel),
    pullModel: vi.fn<IEngineApi['pullModel']>(engineApi.pullModel)
}
let host: HTMLDivElement
let root: Root

beforeEach(() => {
    vi.mocked(transport.invoke).mockRejectedValue(new Error('No service response'))
    Object.defineProperty(globalThis, 'IS_REACT_ACT_ENVIRONMENT', {
        value: true,
        configurable: true
    })
    Object.defineProperty(window, 'pairApi', { configurable: true, value: { engines: commands } })
    useConnectionStore.setState({ selfId: nodeId })
    host = document.createElement('div')
    document.body.append(host)
    root = createRoot(host)
})

afterEach(async () => {
    await act(() => root.unmount())
    host.remove()
    useConnectionStore.setState({ selfId: null })
    useEngineStatusStore.setState({ statusByNode: new Map() })
    useEngineModelsStore.setState({ models: new Map() })
    useNodesStore.setState({ nodes: new Map() })
    usePendingActionsStore.setState({ pending: new Map() })
    useOverviewUiStore.setState({
        activeTab: 'overview',
        focusNodeId: null,
        modelSelectionNodeId: null
    })
    useErrorsStore.setState({ errors: [] })
    Reflect.deleteProperty(window, 'pairApi')
    Reflect.deleteProperty(globalThis, 'IS_REACT_ACT_ENVIRONMENT')
})

async function reportStatus(processStatus: EngineProcessStatus) {
    const status: EngineStatusData = {
        engineType: 'vllm',
        nodeId,
        processStatus,
        enginePort: 8001,
        proxyPort: 8000
    }
    await act(() => {
        useEngineStatusStore.setState({
            statusByNode: new Map([
                [nodeId, new Map<EngineType, EngineStatusData>([['vllm', status]])]
            ])
        })
        // A service lifecycle report also supersedes the optimistic command.
        usePendingActionsStore.setState({ pending: new Map() })
    })
}

async function render(processStatus: EngineProcessStatus) {
    await reportStatus(processStatus)
    await act(() => root.render(createElement(SelectionTestApp)))
}

// The app shell keeps selection mounted while its Overview controls disappear
// during a tab change. Keep those lifetimes separate in this focused fixture.
function SelectionTestApp() {
    const activeTab = useOverviewUiStore(state => state.activeTab)
    const selectionNodeId = useOverviewUiStore(state => state.modelSelectionNodeId)
    return createElement(
        Fragment,
        null,
        activeTab === 'overview' ? createElement(NodeEnginesInline, { nodeId }) : null,
        selectionNodeId
            ? createElement(EngineModelSelectionModal, {
                  key: selectionNodeId,
                  nodeId: selectionNodeId
              })
            : null
    )
}

function button(text: string): HTMLButtonElement {
    const found = Array.from(host.querySelectorAll('button')).find(
        element => element.textContent?.trim() === text
    )
    if (!found) throw new Error(`Missing button: ${text}`)
    return found
}

async function click(text: string) {
    await act(() => button(text).click())
}

async function turnOn() {
    await render('stopped')
    const control = host.querySelector<HTMLInputElement>('[aria-label="Start vLLM"]')
    if (!control) throw new Error('Missing Start vLLM switch')
    await act(() => control.click())
}

function cachedModel(name: string, downloaded = true): ModelItem {
    return {
        name,
        size: 1024,
        downloaded,
        status: 'idle',
        parameterSize: '1B',
        quantization: '',
        family: '',
        digest: '',
        sizeVram: null,
        expiresAt: null,
        expiry: '10m',
        capabilities: []
    }
}

function reportModels(models: ModelItem[]) {
    useEngineModelsStore.setState({
        models: new Map([
            [`${nodeId}:vllm`, { engineType: 'vllm', nodeId, models, inventoryKnown: true }]
        ])
    })
}

it('keeps restored waiting vLLM enabled without a header action or an unsolicited modal', async () => {
    await render('awaiting-model')

    expect(host.querySelector('[aria-label="Stop vLLM"]')).not.toBeNull()
    expect(host.querySelector('[aria-label="Start vLLM"]')).toBeNull()
    expect(host.textContent).not.toContain('Select a model')
    expect(host.querySelector('[role="dialog"]')).toBeNull()
})

it('opens model selection after an explicit turn-on reaches the waiting state', async () => {
    await turnOn()
    expect(commands.toggle).toHaveBeenCalledExactlyOnceWith('vllm', nodeId)
    expect(host.querySelector('[role="dialog"]')).toBeNull()

    await reportStatus('awaiting-model')

    expect(host.querySelector('h2')?.textContent).toBe('Choose a model for vLLM')
    expect(host.querySelectorAll('[role="dialog"]')).toHaveLength(1)
    const dialog = host.querySelector('[role="dialog"]')
    const heading = host.querySelector('h2')
    expect(heading?.id).toBeTruthy()
    expect(dialog?.getAttribute('aria-labelledby')).toBe(heading?.id)
    expect(document.getElementById(heading?.id ?? '')?.textContent).toBe('Choose a model for vLLM')
    expect(host.textContent).toContain('vLLM needs a loaded model')
    expect(useOverviewUiStore.getState().focusNodeId).toBe(nodeId)
})

it('offers model selection after a fresh install enables vLLM', async () => {
    await render('not-installed')
    await click('vLLM')
    expect(commands.install).toHaveBeenCalledExactlyOnceWith('vllm', nodeId)
    await reportStatus('installing')
    await reportStatus('stopped')
    await reportStatus('awaiting-model')

    expect(host.querySelector('[role="dialog"][aria-labelledby]')).not.toBeNull()
})

it.each(['Cancel', 'Close picker'])(
    '%s turns vLLM off and dismisses initial selection',
    async label => {
        await turnOn()
        await reportStatus('awaiting-model')

        await click(label)

        expect(commands.stop).toHaveBeenCalledExactlyOnceWith('vllm', nodeId)
        expect(host.querySelector('[role="dialog"]')).toBeNull()
        await reportStatus('stopped')
        await reportStatus('awaiting-model')
        expect(host.querySelector('[role="dialog"]')).toBeNull()
    }
)

it('loads the clicked downloaded model on the selected node without stopping the engine', async () => {
    useConnectionStore.setState({ selfId: 'another-node' })
    reportModels([cachedModel('org/cached-model')])
    await turnOn()
    await reportStatus('awaiting-model')
    const model = host.querySelector<HTMLButtonElement>('[aria-label="Load org/cached-model"]')
    if (!model) throw new Error('Downloaded model was not offered')

    await act(() => model.click())

    expect(commands.loadModel).toHaveBeenCalledExactlyOnceWith('vllm', nodeId, 'org/cached-model')
    expect(commands.stop).not.toHaveBeenCalled()
    expect(host.querySelector('[role="dialog"]')).toBeNull()
})

it('does not offer an incomplete download as a model to load', async () => {
    reportModels([cachedModel('org/incomplete-model', false)])
    await turnOn()
    await reportStatus('awaiting-model')

    expect(host.querySelector('[aria-label="Load org/incomplete-model"]')).toBeNull()
    expect(host.textContent).toContain('No models downloaded yet')
    expect(button('Add model').disabled).toBe(false)
})

it('skips model selection when a saved selection starts normally', async () => {
    await turnOn()
    await reportStatus('starting')
    await reportStatus('running')

    expect(host.querySelector('[role="dialog"]')).toBeNull()
    expect(useOverviewUiStore.getState().modelSelectionNodeId).toBeNull()
    expect(commands.stop).not.toHaveBeenCalled()
})

it('does not reopen model selection after Eject', async () => {
    await turnOn()
    await reportStatus('running')
    await reportStatus('awaiting-model')

    expect(host.querySelector('[role="dialog"]')).toBeNull()
    expect(commands.stop).not.toHaveBeenCalled()
})

it('dismisses selection without stopping a model loaded by a request', async () => {
    await turnOn()
    await reportStatus('awaiting-model')
    expect(host.querySelector('[role="dialog"]')).not.toBeNull()

    await reportStatus('starting')
    await reportStatus('running')

    expect(host.querySelector('[role="dialog"]')).toBeNull()
    expect(commands.stop).not.toHaveBeenCalled()
})

it('returns from the model catalog to selection without turning the engine off', async () => {
    await turnOn()
    await reportStatus('awaiting-model')

    await click('Add model')
    expect(host.querySelector('[aria-label="Model catalog"]')).not.toBeNull()
    await click('Close catalog')

    expect(host.querySelector('[role="dialog"][aria-labelledby]')).not.toBeNull()
    expect(commands.stop).not.toHaveBeenCalled()
})

it('downloads the catalog selection on the chosen node and leaves the engine enabled', async () => {
    await turnOn()
    await reportStatus('awaiting-model')
    await click('Add model')

    await click('Download selected model')

    expect(commands.pullModel).toHaveBeenCalledExactlyOnceWith('vllm', nodeId, 'org/new-model')
    expect(commands.stop).not.toHaveBeenCalled()
    expect(commands.loadModel).not.toHaveBeenCalled()
    expect(host.querySelector('[role="dialog"]')).toBeNull()
})

it('forgets selection after a failed start so later idle state cannot reopen it', async () => {
    await turnOn()

    await act(() =>
        useErrorsStore.setState({
            errors: [
                {
                    id: 'failed-vllm-start',
                    timestamp: 1,
                    message: 'Could not start vLLM',
                    engineType: 'vllm',
                    nodeId,
                    operation: 'start'
                }
            ]
        })
    )
    await reportStatus('awaiting-model')

    expect(host.querySelector('[role="dialog"]')).toBeNull()
    expect(useOverviewUiStore.getState().modelSelectionNodeId).toBeNull()
    expect(commands.stop).not.toHaveBeenCalled()
})

it('keeps selection available when an unrelated node reports a vLLM error', async () => {
    await turnOn()

    await act(() =>
        useErrorsStore.setState({
            errors: [
                {
                    id: 'another-node-failed',
                    timestamp: 1,
                    message: 'Could not start vLLM',
                    engineType: 'vllm',
                    nodeId: 'another-node',
                    operation: 'start'
                }
            ]
        })
    )
    await reportStatus('awaiting-model')

    expect(host.querySelector('[role="dialog"][aria-labelledby]')).not.toBeNull()
})

it('remembers a failed start while engine controls are unmounted so returning cannot reopen selection', async () => {
    await turnOn()
    await act(() => useOverviewUiStore.getState().setActiveTab('settings'))
    await act(() =>
        useErrorsStore.setState({
            errors: [
                {
                    id: 'failed-while-viewing-settings',
                    timestamp: 1,
                    message: 'Could not start vLLM',
                    engineType: 'vllm',
                    nodeId,
                    operation: 'start'
                }
            ]
        })
    )

    await act(() => useOverviewUiStore.getState().setActiveTab('overview'))
    await render('stopped')
    expect(useOverviewUiStore.getState().modelSelectionNodeId).toBeNull()
    await reportStatus('awaiting-model')

    expect(host.querySelector('[role="dialog"]')).toBeNull()
    expect(commands.stop).not.toHaveBeenCalled()
})

it('forgets a saved-model start while Overview is absent so Eject cannot reopen selection', async () => {
    await turnOn()
    await act(() => useOverviewUiStore.getState().setActiveTab('settings'))
    expect(host.querySelector('[role="switch"]')).toBeNull()

    await reportStatus('starting')
    await reportStatus('running')
    expect(useOverviewUiStore.getState().modelSelectionNodeId).toBeNull()

    await reportStatus('awaiting-model')
    await act(() => useOverviewUiStore.getState().setActiveTab('overview'))

    expect(host.querySelector('[aria-label="Stop vLLM"]')).not.toBeNull()
    expect(host.querySelector('[role="dialog"]')).toBeNull()
    expect(commands.stop).not.toHaveBeenCalled()
})
