// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'

import { EnabledEngineTypes, MANIFEST_ENGINE_TYPES } from '@/shared/constants/engines'
import type {
    EngineProcessStatus,
    EngineStatusByNode,
    EngineStatusData
} from '@/shared/types/engines'
import {
    customEndpointDisplayLabel,
    customEndpointSummaryLabel,
    customEndpointsUnavailableLabel,
    validateCustomEndpointForm,
    validateCustomEndpointKeyFields,
    validateCustomProxyPort
} from '@/ui/utils/custom-endpoints'
import { engineStatusesToListRows, getEnginesForNode } from '@/ui/utils/get-engines-for-node'

/** The form's raw field text: the Port field is text until validation parses it. */
function formValues(url: string, port: string) {
    return { url, port }
}

describe('custom endpoint add-form validation', () => {
    it('accepts a plain endpoint url with an explicit port, the common case', () => {
        expect(validateCustomEndpointForm(formValues('https://alpha.example/v1', '8000'))).toEqual(
            {}
        )
    })

    it('accepts a url whose embedded port exactly agrees with the port field', () => {
        expect(
            validateCustomEndpointForm(formValues('https://alpha.example:8000/v1', '8000'))
        ).toEqual({})
    })

    it('rejects a url port that disagrees with the port field instead of quietly picking a winner', () => {
        const errors = validateCustomEndpointForm(
            formValues('https://alpha.example:9999/v1', '8000')
        )
        expect(errors.mismatch).toContain('disagree')
    })

    it('rejects an empty port because a port is never inferred', () => {
        const errors = validateCustomEndpointForm(formValues('https://alpha.example/v1', ''))
        expect(errors.port).toMatch(/required|never/i)
    })

    it('rejects a non-numeric port for the same reason', () => {
        const errors = validateCustomEndpointForm(formValues('https://alpha.example/v1', 'http'))
        expect(errors.port).toMatch(/required|never/i)
    })

    it('rejects a port outside the 1-65535 range, mirroring the broker', () => {
        expect(
            validateCustomEndpointForm(formValues('https://a.example/v1', '0')).port
        ).toBeDefined()
        expect(
            validateCustomEndpointForm(formValues('https://a.example/v1', '65536')).port
        ).toBeDefined()
    })

    it('rejects an empty url', () => {
        expect(validateCustomEndpointForm(formValues('   ', '8000')).url).toBeDefined()
    })

    it('rejects a url with no scheme instead of guessing https', () => {
        const errors = validateCustomEndpointForm(formValues('alpha.example/v1', '8000'))
        expect(errors.url).toMatch(/http/i)
    })

    it('rejects a scheme that is not http or https', () => {
        expect(
            validateCustomEndpointForm(formValues('ftp://alpha.example/v1', '8000')).url
        ).toMatch(/http/i)
    })

    it('rejects a url with no host', () => {
        expect(validateCustomEndpointForm(formValues('https://:8000/v1', '8000')).url).toMatch(
            /host/i
        )
    })
})

describe('custom endpoint display labels', () => {
    it('shows the user-chosen name over the address', () => {
        expect(
            customEndpointDisplayLabel({
                id: 'custom:https://alpha.example:8000/v1',
                name: 'Office vLLM',
                baseUrl: 'https://alpha.example:8000/v1',
                keySet: false
            })
        ).toBe('Office vLLM')
    })

    it('falls back to host and port when no name was given', () => {
        expect(
            customEndpointDisplayLabel({
                id: 'custom:https://alpha.example:8000/v1',
                baseUrl: 'https://alpha.example:8000/v1',
                keySet: false
            })
        ).toBe('alpha.example:8000')
    })

    it('shows a port-less url as just the host', () => {
        expect(
            customEndpointDisplayLabel({
                id: 'custom:https://api.example/v1',
                baseUrl: 'https://api.example/v1',
                keySet: true
            })
        ).toBe('api.example')
    })

    it('shows an unparseable baseUrl verbatim rather than blanking the line', () => {
        expect(
            customEndpointDisplayLabel({
                id: 'custom:not a url',
                baseUrl: 'not a url',
                keySet: false
            })
        ).toBe('not a url')
    })
})

describe('custom endpoint card summary line', () => {
    const now = new Date('2026-10-28T12:00:00Z')

    it('counts the served models on an online endpoint', () => {
        expect(
            customEndpointSummaryLabel(
                {
                    id: 'x',
                    baseUrl: 'https://a.example/v1',
                    keySet: false,
                    healthy: true,
                    models: ['alpha-7b', 'phi-3-mini']
                },
                now
            )
        ).toBe('Online · 2 models')
    })

    it('uses the singular for one model', () => {
        expect(
            customEndpointSummaryLabel(
                {
                    id: 'x',
                    baseUrl: 'https://a.example/v1',
                    keySet: false,
                    healthy: true,
                    models: ['alpha-7b']
                },
                now
            )
        ).toBe('Online · 1 model')
    })

    it('says only Online when the endpoint answered with nothing to route', () => {
        // "checked, answered, no models" must read as online-but-empty, and
        // never count zero models — a "0 models" tail read as a status chip.
        expect(
            customEndpointSummaryLabel(
                {
                    id: 'x',
                    baseUrl: 'https://a.example/v1',
                    keySet: false,
                    healthy: true,
                    models: []
                },
                now
            )
        ).toBe('Online')
        expect(
            customEndpointSummaryLabel(
                { id: 'x', baseUrl: 'https://a.example/v1', keySet: false, healthy: true },
                now
            )
        ).toBe('Online')
    })

    it('reads Offline without repeating any model count', () => {
        // A failed probe's model list is stale noise; the card's job here is
        // the verdict, and every offline state stays out of the models
        // suffix (the never-critical rule).
        expect(
            customEndpointSummaryLabel(
                {
                    id: 'x',
                    baseUrl: 'https://a.example/v1',
                    keySet: false,
                    healthy: false,
                    models: ['alpha-7b']
                },
                now
            )
        ).toBe('Offline')
    })

    it('reads Not checked yet when no probe has answered', () => {
        // The absence half of the wire contract: healthy arrives only when a
        // probe reported true or false, and neither reading may imply a
        // verdict.
        expect(
            customEndpointSummaryLabel(
                { id: 'x', baseUrl: 'https://a.example/v1', keySet: false },
                now
            )
        ).toBe('Not checked yet')
    })

    it('shows the age of the verdict that produced the state', () => {
        expect(
            customEndpointSummaryLabel(
                {
                    id: 'x',
                    baseUrl: 'https://a.example/v1',
                    keySet: false,
                    healthy: true,
                    models: ['alpha-7b'],
                    lastChecked: '2026-10-28T11:47:00Z'
                },
                now
            )
        ).toBe('Online · 1 model · checked 13 min ago')
    })

    it('labels a just-finished failed probe as checked now', () => {
        expect(
            customEndpointSummaryLabel(
                {
                    id: 'x',
                    baseUrl: 'https://a.example/v1',
                    keySet: false,
                    healthy: false,
                    lastChecked: '2026-10-28T11:59:30Z'
                },
                now
            )
        ).toBe('Offline · checked just now')
    })

    it('switches to hourly age past an hour, so a stale verdict never reads as fresh', () => {
        expect(
            customEndpointSummaryLabel(
                {
                    id: 'x',
                    baseUrl: 'https://a.example/v1',
                    keySet: false,
                    healthy: true,
                    lastChecked: '2026-10-28T11:00:00Z'
                },
                now
            )
        ).toBe('Online · checked 1 h ago')
    })

    it('drops an unparseable timestamp instead of printing junk', () => {
        expect(
            customEndpointSummaryLabel(
                {
                    id: 'x',
                    baseUrl: 'https://a.example/v1',
                    keySet: false,
                    healthy: true,
                    lastChecked: 'sometime'
                },
                now
            )
        ).toBe('Online')
    })
})

describe('custom endpoint unavailable banner text', () => {
    it('turns the relay cannot-serve answer into the older-version sentence', () => {
        // What the wire says is a class of failure ("offline or does not
        // support"); by the time it reaches a section that only renders for
        // connected nodes, the operator's situation is a node running a
        // version that predates the feature. The banner names that and the
        // fix; the raw code stays available as the element's tooltip.
        const label = customEndpointsUnavailableLabel(
            '-32000: custom endpoints unavailable: device is offline or does not support custom endpoint management'
        )
        expect(label).toContain('older version of NVIDIA PAIR')
        expect(label).toContain('Update NVIDIA PAIR on that node')
        expect(label).not.toContain('-32000')
        expect(label).not.toContain('does not support custom endpoint management')
    })

    it('treats the bridge empty-reply guard as the same version skew', () => {
        // A peer too old to answer the plane also answers a relayed read with
        // a body that is not an endpoint list; both readings mean the same
        // trouble and must not print as two different errors.
        expect(
            customEndpointsUnavailableLabel(
                'custom endpoints unavailable: the device returned no endpoint list'
            )
        ).toContain('older version of NVIDIA PAIR')
    })

    it('treats a peer with no Custom proxy port as the same version skew', () => {
        // A node that predates the Custom plane has no /v1/custom-endpoints
        // routes at all: engine-manager folds that into the unified unavailable
        // message, and a peer answering with a body that is not a port lands in
        // the bridge's own guard. Both mean the plane is missing, so both read
        // as the older-version sentence.
        expect(
            customEndpointsUnavailableLabel(
                'custom proxy port unavailable: the device returned no proxy port'
            )
        ).toContain('older version of NVIDIA PAIR')
    })

    it('passes an unrelated refusal through unchanged', () => {
        // A trust refusal or a timeout is not version skew; translating those
        // would misname the trouble the operator has to act on.
        expect(customEndpointsUnavailableLabel('custom endpoints unavailable: not clustered')).toBe(
            'custom endpoints unavailable: not clustered'
        )
        expect(customEndpointsUnavailableLabel('broker call timed out')).toBe(
            'broker call timed out'
        )
        // A refused port move is a real answer about a port, not a version
        // problem: it names who holds the number, and that has to be readable.
        expect(
            customEndpointsUnavailableLabel(
                'custom /v1/custom-endpoints/set-port: HTTP 409: port 9292 is already in use on that node: an inference engine is running there'
            )
        ).toContain('an inference engine is running there')
    })
})

describe('custom proxy-port field rule', () => {
    // The same rule the broker applies, run before the request is sent: Apply
    // stays off while the field cannot name a port, so neither a local call nor
    // a peer relay's whole budget is spent on a guaranteed refusal.
    it('accepts any whole port number, including one the proxy did not start on', () => {
        expect(validateCustomProxyPort('8000')).toBeUndefined()
        expect(validateCustomProxyPort(' 8137 ')).toBeUndefined()
        expect(validateCustomProxyPort('1')).toBeUndefined()
        expect(validateCustomProxyPort('65535')).toBeUndefined()
    })

    it('refuses text that never names a port', () => {
        expect(validateCustomProxyPort('')).toContain('number')
        expect(validateCustomProxyPort('http')).toContain('number')
        expect(validateCustomProxyPort('81.2')).toContain('number')
    })

    it('refuses a number outside the port range', () => {
        expect(validateCustomProxyPort('0')).toMatch(/between 1 and 65535/)
        expect(validateCustomProxyPort('65536')).toMatch(/between 1 and 65535/)
    })
})

describe('custom endpoint key-field validation', () => {
    it('refuses replacing and removing the saved key in one save', () => {
        // Both actions on one submit has no safe reading, so the form must
        // stop it before a request could pick a winner.
        expect(
            validateCustomEndpointKeyFields({
                keySet: true,
                apiKey: 'sk-new',
                clearKey: true
            }).key
        ).toMatch(/not both/i)
    })

    it('accepts a typed replacement key on its own', () => {
        expect(
            validateCustomEndpointKeyFields({
                keySet: true,
                apiKey: 'sk-new',
                clearKey: false
            }).key
        ).toBeUndefined()
    })

    it('accepts removing the saved key on its own', () => {
        expect(
            validateCustomEndpointKeyFields({
                keySet: true,
                apiKey: '',
                clearKey: true
            }).key
        ).toBeUndefined()
    })

    it('is quiet when the key field was not touched', () => {
        // The keep-key path: blank field, no checkbox, whatever keySet says.
        expect(
            validateCustomEndpointKeyFields({ keySet: true, apiKey: '', clearKey: false }).key
        ).toBeUndefined()
        expect(
            validateCustomEndpointKeyFields({ keySet: false, apiKey: '', clearKey: false }).key
        ).toBeUndefined()
    })
})

describe('compact engine lists with the fifth kind', () => {
    function status(processStatus: EngineProcessStatus): EngineStatusData {
        return {
            engineType: 'custom',
            nodeId: 'node-a',
            processStatus,
            enginePort: null,
            proxyPort: null
        }
    }

    function reportedCustom(): EngineStatusByNode {
        const inner = new Map<EngineStatusData['engineType'], EngineStatusData>()
        inner.set('ollama', { ...status('running'), engineType: 'ollama', proxyPort: 11434 })
        inner.set('custom', status('running'))
        return new Map([['node-a', inner]])
    }

    it('keeps Custom out of the manifest-engine list the tray and node cards count from', () => {
        // The set these surfaces enumerate with: custom is shipped, but it is a
        // configured facade, not an installed engine, so it is not in this list.
        expect(MANIFEST_ENGINE_TYPES).toEqual(['ollama', 'lm-studio', 'llama-cpp', 'vllm'])
        expect(EnabledEngineTypes).toContain('custom')
    })

    it('renders no Custom row even when a node actually reports custom presence', () => {
        const rows = engineStatusesToListRows(
            getEnginesForNode('node-a', reportedCustom(), MANIFEST_ENGINE_TYPES)
        )
        expect(rows.map(row => row.displayName)).not.toContain('Custom')
        // The four manifest engines still count — the denominator stays honest.
        expect(rows).toHaveLength(4)
    })
})
