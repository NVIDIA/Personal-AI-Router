// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useState } from 'react'
import {
    Button,
    Flex,
    FormField,
    Spinner,
    Stack,
    Switch,
    Text,
    TextInput
} from '@nvidia/foundations-react-core'
import type { ClusterRouterConfig, ClusterRouterStatus } from '@/shared/types/ws-channels'
import {
    MODULAR_CLUSTER_ROUTER_MAX_PORT,
    MODULAR_CLUSTER_ROUTER_MIN_PORT
} from '@/shared/constants/modular-runtime'
import getErrorString from '@/shared/utils/get-error-string'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import { isElectron } from '@/ui/api/bootstrap'

/**
 * Cluster routing endpoint card.
 *
 * Two controls for one product surface: whether this computer serves the
 * cluster routing endpoint, and the loopback port it is asked for. Both apply
 * to the running endpoint as they are used — the switch commits on change, and
 * the port on an explicit Apply, because a listener must not bind per
 * keystroke. A refused write lands nothing: the card shows the services' own
 * sentence about the port and the field returns to what is in force.
 *
 * What is bound right now is asked for above this card and handed down, because
 * the same answer decides whether the aliases that route from here are listed
 * beside it. A configured port somebody else already holds is not an endpoint, so
 * the card shows what is bound beside what is configured rather than one from the
 * other.
 *
 * `settings-card-stacked` because the section stacks its cards vertically: the
 * default `settings-card` basis is a width in the wrapping rows other settings
 * tabs use, and in a column it reads as a minimum height that pads this card out
 * below its last line.
 */

interface ClusterEndpointCardProps {
    /** The reported bound state, or `null` while nothing has answered. */
    status: ClusterRouterStatus | null
    /**
     * Ask again, which the page answers after every write, landed or refused:
     * the status line and the card gated on it re-read what is bound instead of
     * taking the click's word for it.
     */
    onChanged: () => void
}

function statusLine(status: ClusterRouterStatus | null): string {
    if (status === null) return 'Running state not reported'
    if (!status.running) return 'Not running on this computer'
    return `Running on 127.0.0.1:${status.port}`
}

export default function ClusterEndpointCard({ status, onChanged }: ClusterEndpointCardProps) {
    /** `null` until the preference read answers: there is nothing to show until what is stored is known. */
    const [config, setConfig] = useState<ClusterRouterConfig | null>(null)
    const [portDraft, setPortDraft] = useState('')
    const [busy, setBusy] = useState(false)
    const [error, setError] = useState<string | null>(null)

    const load = useCallback(async () => {
        if (!isElectron) return
        setBusy(true)
        try {
            const nextConfig = await window.pairApi.cluster.getRouterConfig()
            setConfig(nextConfig)
            setPortDraft(String(nextConfig.port))
            setError(null)
        } catch (err) {
            setError(getErrorString(err))
        } finally {
            setBusy(false)
        }
    }, [])

    // The card mounts with the Cluster tab, so this reads once per mount; every
    // write below asks the page to read again afterwards.
    useEffect(() => {
        void load()
    }, [load])

    /**
     * The one write path, for both controls: commit the patch, then let the
     * answer decide the card. A resolved write answers with the configuration
     * still or now in force, which replaces the draft; on a port that cannot be
     * used here the card adds its own sentence for picking a number. A rejected
     * write applied and stored nothing, so the services' own words about the
     * port they could not bind stand in the banner, and the draft returns to
     * what is bound. Either way the page is asked to re-read the bound state,
     * so the status line and the cards gated on it follow the endpoint rather
     * than the click.
     */
    async function write(patch: Partial<ClusterRouterConfig>): Promise<void> {
        if (!isElectron || !config) return
        const drafted = patch.port
        setBusy(true)
        setError(null)
        try {
            const written = await window.pairApi.cluster.setRouterConfig(patch)
            setConfig(written)
            if (drafted !== undefined) {
                setPortDraft(String(written.port))
                if (written.port !== drafted) {
                    setError(
                        `Port ${portDraft.trim()} cannot be used here. Pick a number from ${MODULAR_CLUSTER_ROUTER_MIN_PORT} to ${MODULAR_CLUSTER_ROUTER_MAX_PORT}.`
                    )
                }
            }
        } catch (err) {
            setError(getErrorString(err))
            if (drafted !== undefined) {
                // Nothing was applied and nothing stored, so the field returns
                // to what is in force: the bound port when the services report
                // one, the stored preference otherwise.
                setPortDraft(
                    status !== null && status.running ? String(status.port) : String(config.port)
                )
            }
        } finally {
            onChanged()
            setBusy(false)
        }
    }

    if (config === null) {
        return (
            <div className="settings-card settings-card-stacked pair-paper p-4">
                <Stack gap="2">
                    {error ? (
                        <InlineErrorBanner
                            severity="error"
                            message={error}
                            onClose={() => setError(null)}
                        />
                    ) : (
                        <Spinner size="small" aria-label="Loading cluster endpoint" />
                    )}
                    {error && (
                        <Button
                            kind="secondary"
                            size="small"
                            disabled={busy}
                            onClick={() => void load()}
                        >
                            Retry
                        </Button>
                    )}
                </Stack>
            </div>
        )
    }

    // Apply asks about a mismatch between what is stored and what is bound, not
    // about the number in the field: the comparison is against the reported
    // listener, so a machine whose configured port was taken at boot — no
    // endpoint, or one on another port — still offers the apply that could fix
    // it. An edited port always counts through the second clause.
    const unchanged =
        status !== null && status.running === config.enabled && status.port === config.port
    const applyDisabled = busy || (unchanged && portDraft.trim() === String(config.port))

    return (
        <div className="settings-card settings-card-stacked pair-paper p-4">
            <Stack gap="3">
                <Stack gap="1">
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        One loopback address that routes a model name to any engine on any node of
                        the cluster. Point applications at it instead of at one engine&apos;s own
                        address.
                    </Text>
                </Stack>

                {error && (
                    <InlineErrorBanner
                        severity="error"
                        message={error}
                        onClose={() => setError(null)}
                    />
                )}

                <Flex align="center" justify="between" gap="4" wrap="wrap">
                    <Stack gap="1">
                        <Text kind="body/regular/md">Cluster routing endpoint</Text>
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            {statusLine(status)}
                        </Text>
                    </Stack>
                    <Switch
                        checked={config.enabled}
                        onCheckedChange={checked => void write({ enabled: checked })}
                        disabled={busy}
                        size="small"
                        aria-label="Cluster routing endpoint"
                    />
                </Flex>

                <Flex align="end" gap="2" wrap="wrap">
                    <FormField slotLabel="Port">
                        <TextInput
                            size="small"
                            aria-label="Cluster routing endpoint port"
                            value={portDraft}
                            onValueChange={setPortDraft}
                            className="w-32 font-mono"
                        />
                    </FormField>
                    <Button
                        kind="secondary"
                        size="small"
                        disabled={applyDisabled}
                        onClick={() =>
                            void write({
                                enabled: config.enabled,
                                port: Number(portDraft.trim())
                            })
                        }
                    >
                        Apply
                    </Button>
                </Flex>

                <Text kind="body/regular/sm" className="text-subtle-color">
                    The endpoint stays on this computer&apos;s loopback address whatever port is set
                    here.
                </Text>
            </Stack>
        </div>
    )
}
