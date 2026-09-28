// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useMemo, useState } from 'react'
import { Badge, Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import type { ServiceVersions } from '@/shared/types/ipc-channels'
import type { NodeItem } from '@/shared/types/nodes'
import type { VllmModelReceipt, VllmRuntimePrepareResult } from '@/shared/types/vllm-model-journey'
import type { OnboardingCandidate } from '@/shared/types/onboarding-live'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { useOnboardingStore } from '@/ui/stores/onboarding.store'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'

const DGX_SPARK_UPDATE_GUIDE = 'https://docs.nvidia.com/dgx/dgx-spark/os-and-component-update.html'

interface Props {
    modelReceipts: Record<string, VllmModelReceipt>
    runtimeResults: Record<string, VllmRuntimePrepareResult>
    initialVersions?: ServiceVersions
}

function short(value?: string): string {
    if (!value) return 'Not reported'
    return value.length > 18 ? `${value.slice(0, 12)}…` : value
}

function exactCandidateForNode(
    node: NodeItem,
    candidates: OnboardingCandidate[]
): OnboardingCandidate | null {
    const addresses = new Set([node.ipAddress, ...node.allIpAddresses].filter(Boolean))
    const matches = candidates.filter(candidate => addresses.has(candidate.address))
    return matches.length === 1 ? matches[0]! : null
}

function modelRevision(model?: string): string | undefined {
    const revision = model?.split('@')[1]
    return revision && /^[0-9a-f]{40}$/.test(revision) ? revision : undefined
}

export default function FleetCompatibilityCard({
    modelReceipts,
    runtimeResults,
    initialVersions
}: Props) {
    const selfId = useConnectionStore(state => state.selfId)
    const nodes = useNodesStore(state => state.nodes)
    const statusByNode = useEngineStatusStore(state => state.statusByNode)
    const onboarding = useOnboardingStore()
    const [versions, setVersions] = useState<ServiceVersions | null>(initialVersions ?? null)
    const [selected, setSelected] = useState<string[]>([])
    const [artifactId, setArtifactId] = useState('')

    useEffect(() => {
        if (initialVersions) return
        let current = true
        window.windowApi.service
            .getVersions()
            .then(value => {
                if (current) setVersions(value)
            })
            .catch(() => {
                if (current) setVersions(null)
            })
        return () => {
            current = false
        }
    }, [initialVersions])

    const activeNodes = useMemo(
        () =>
            Array.from(nodes.values()).filter(
                node => node.id === selfId || node.status === 'active'
            ),
        [nodes, selfId]
    )
    const artifact = onboarding.artifacts.find(item => item.artifactId === artifactId)
    useEffect(() => {
        if (artifact || onboarding.artifacts.length === 0) return
        setArtifactId(onboarding.artifacts[0]!.artifactId)
    }, [artifact, onboarding.artifacts])
    useEffect(() => {
        if (onboarding.operation?.state !== 'running') return
        const timer = setInterval(() => {
            if (!useOnboardingStore.getState().pending)
                void useOnboardingStore.getState().refreshOperation()
        }, 2000)
        return () => clearInterval(timer)
    }, [onboarding.operation?.operationId, onboarding.operation?.state])

    const localEngineManager = versions?.binaries.find(
        binary => binary.name === 'nvpair-engine-manager'
    )?.version
    const reviewByNode = new Map(
        (onboarding.review?.targets ?? [])
            .filter(target => target.existingInstallation?.nodeId)
            .map(target => [target.existingInstallation!.nodeId, target])
    )
    const rows = activeNodes.map(node => {
        const candidate = exactCandidateForNode(node, onboarding.candidates)
        const review = reviewByNode.get(node.id)
        const operation = onboarding.operation
        const completedUpdate =
            operation?.reviewId === onboarding.review?.reviewId &&
            operation?.state === 'completed' &&
            operation.targets.some(
                target =>
                    target.nodeId === node.id &&
                    target.stage === 'paired' &&
                    target.cleanupConfirmed
            )
        const engine = statusByNode.get(node.id)?.get('vllm')
        const runtime = runtimeResults[node.id]
        const receipt = modelReceipts[node.id]
        const model = receipt?.id ?? engine?.selectedModel
        return {
            node,
            candidate,
            pairVersion:
                node.id === selfId
                    ? versions?.modularServices || versions?.appVersion
                    : completedUpdate
                      ? review?.artifact?.version
                      : review?.existingInstallation?.version,
            engineManagerVersion: node.id === selfId ? localEngineManager : undefined,
            vllmVersion: engine?.installedVersion,
            providerRevision: runtime?.provider.observedClosureSha256,
            providerQualified: runtime?.provider.qualified,
            modelRevision: receipt?.revision ?? modelRevision(model)
        }
    })
    const known = (key: 'pairVersion' | 'vllmVersion' | 'providerRevision' | 'modelRevision') =>
        new Set(rows.map(row => row[key]).filter((value): value is string => !!value))
    const pairDrift = known('pairVersion').size > 1
    const vllmDrift = known('vllmVersion').size > 1
    const providerDrift =
        known('providerRevision').size > 1 || rows.some(row => row.providerQualified === false)
    const modelDrift = known('modelRevision').size > 1
    const drift = pairDrift || vllmDrift || providerDrift || modelDrift
    const selectable = (candidate: OnboardingCandidate | null) =>
        Boolean(
            candidate &&
            candidate.bootstrapState === 'ssh-ready' &&
            candidate.accessAvailable &&
            candidate.hostKeyTrusted
        )
    const selectedRows = rows.filter(row =>
        row.candidate ? selected.includes(row.candidate.candidateId) : false
    )
    const selectionReady =
        selectedRows.length > 0 &&
        selectedRows.length <= 4 &&
        !!artifact &&
        !onboarding.pending &&
        onboarding.operation?.state !== 'running'

    const toggle = (candidateId: string, checked: boolean) => {
        onboarding.clearReview()
        setSelected(current =>
            checked
                ? [...current.filter(id => id !== candidateId), candidateId].slice(0, 4)
                : current.filter(id => id !== candidateId)
        )
    }

    const reviewUpdates = () => {
        if (!selectionReady || !artifact) return
        void onboarding.inspect({
            candidateIds: selectedRows.map(row => row.candidate!.candidateId),
            artifactId: artifact.artifactId,
            acceptedHostKeys: []
        })
    }

    return (
        <Stack gap="3" className="border border-subtle-color rounded p-3">
            <Flex justify="between" align="center" gap="2" wrap="wrap">
                <Stack gap="0">
                    <Text kind="body/semibold/sm">Fleet compatibility</Text>
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        Current product and serving revisions. Provider values are last-prepared
                        session evidence; group review and Start recheck live provider facts. Not
                        reported is unknown, never assumed equal.
                    </Text>
                </Stack>
                <Badge color={drift ? 'yellow' : 'green'} kind="solid">
                    {drift ? 'Prepared drift detected' : 'No prepared drift known'}
                </Badge>
            </Flex>

            <div className="overflow-x-auto">
                <table className="w-full text-left text-sm" aria-label="Fleet compatibility matrix">
                    <thead>
                        <tr>
                            <th className="p-1">Update</th>
                            <th className="p-1">Node</th>
                            <th className="p-1">PAIR</th>
                            <th className="p-1">Engine Manager</th>
                            <th className="p-1">vLLM</th>
                            <th className="p-1">Provider (last prepare)</th>
                            <th className="p-1">Model revision</th>
                        </tr>
                    </thead>
                    <tbody>
                        {rows.map(row => {
                            const canSelect = row.node.id !== selfId && selectable(row.candidate)
                            const checked = row.candidate
                                ? selected.includes(row.candidate.candidateId)
                                : false
                            return (
                                <tr key={row.node.id} className="border-t border-subtle-color">
                                    <td className="p-1">
                                        <input
                                            type="checkbox"
                                            aria-label={`Select ${row.node.name || row.node.id} for PAIR update`}
                                            checked={checked}
                                            disabled={
                                                !canSelect || (!checked && selected.length >= 4)
                                            }
                                            onChange={event =>
                                                row.candidate &&
                                                toggle(
                                                    row.candidate.candidateId,
                                                    event.target.checked
                                                )
                                            }
                                        />
                                    </td>
                                    <td className="p-1">{row.node.name || row.node.id}</td>
                                    <td className="p-1">{short(row.pairVersion)}</td>
                                    <td className="p-1">{short(row.engineManagerVersion)}</td>
                                    <td className="p-1">{short(row.vllmVersion)}</td>
                                    <td className="p-1" title={row.providerRevision}>
                                        {short(row.providerRevision)}
                                    </td>
                                    <td className="p-1" title={row.modelRevision}>
                                        {short(row.modelRevision)}
                                    </td>
                                </tr>
                            )
                        })}
                    </tbody>
                </table>
            </div>

            <Text kind="body/regular/sm" className="text-subtle-color">
                Peer PAIR versions appear after the typed inspection. Per-peer Engine Manager
                component revisions are not reported by the current onboarding receipt, so PAIR
                leaves them unknown instead of inferring them from the product version.
            </Text>

            {onboarding.error && <InlineErrorBanner severity="error" message={onboarding.error} />}
            {providerDrift && (
                <Flex gap="2" align="center" wrap="wrap">
                    <Text kind="body/regular/sm">
                        Provider drift needs the supported DGX system update path; PAIR does not run
                        arbitrary operating-system updates.
                    </Text>
                    <Button
                        kind="secondary"
                        size="small"
                        onClick={() =>
                            void window.windowApi.window.openExternal(DGX_SPARK_UPDATE_GUIDE)
                        }
                    >
                        Open DGX Dashboard update guide
                    </Button>
                </Flex>
            )}

            <Flex gap="2" align="end" wrap="wrap">
                <label>
                    <Text kind="body/semibold/sm">PAIR package</Text>
                    <select
                        aria-label="PAIR update package"
                        value={artifactId}
                        disabled={!!onboarding.pending}
                        onChange={event => {
                            onboarding.clearReview()
                            setArtifactId(event.target.value)
                        }}
                    >
                        <option value="">Choose a verified package</option>
                        {onboarding.artifacts.map(item => (
                            <option key={item.artifactId} value={item.artifactId}>
                                {item.version} · {item.platform}/{item.arch}
                            </option>
                        ))}
                    </select>
                </label>
                <Button
                    kind="secondary"
                    size="small"
                    disabled={!selectionReady}
                    title="Creates the exact PAIR product update review; it does not update the DGX operating system."
                    onClick={reviewUpdates}
                >
                    {onboarding.pending === 'inspect'
                        ? 'Inspecting selected nodes…'
                        : 'Update selected nodes'}
                </Button>
            </Flex>
            <Text kind="body/regular/sm" className="text-subtle-color">
                This entry uses PAIR&apos;s typed package inspection and reviewed update operation.
                Nothing changes until the exact review is approved.
            </Text>

            {onboarding.review && (
                <Stack gap="2" className="bg-surface-sunken rounded p-2">
                    <Text kind="body/semibold/sm">
                        PAIR update review · {onboarding.review.canApprove ? 'ready' : 'held'}
                    </Text>
                    {onboarding.review.targets.map(target => (
                        <Text key={target.candidateId} kind="body/regular/sm">
                            {target.label} · {target.action ?? 'install'} ·{' '}
                            {target.existingInstallation?.version ?? 'not installed'} →{' '}
                            {target.artifact?.version ?? 'no verified artifact'} · {target.status}
                            {target.reason ? ` · ${target.reason}` : ''}
                        </Text>
                    ))}
                    <Button
                        kind="primary"
                        size="small"
                        disabled={!onboarding.review.canApprove || !!onboarding.pending}
                        onClick={() => void onboarding.approve()}
                    >
                        {onboarding.pending === 'approve'
                            ? 'Starting reviewed updates…'
                            : 'Approve reviewed PAIR updates'}
                    </Button>
                </Stack>
            )}

            {onboarding.operation && (
                <Stack gap="2" className="bg-surface-sunken rounded p-2" role="status">
                    <Flex justify="between" align="center" gap="2" wrap="wrap">
                        <Text kind="body/semibold/sm">
                            PAIR update operation · {onboarding.operation.state}
                        </Text>
                        <Button
                            kind="secondary"
                            size="small"
                            disabled={!!onboarding.pending}
                            onClick={() => void onboarding.refreshOperation()}
                        >
                            Refresh
                        </Button>
                    </Flex>
                    {onboarding.operation.targets.map(target => (
                        <Flex
                            key={target.candidateId}
                            justify="between"
                            align="center"
                            gap="2"
                            wrap="wrap"
                        >
                            <Text kind="body/regular/sm">
                                {target.nodeId || target.candidateId} · {target.stage}
                                {target.message ? ` · ${target.message}` : ''}
                            </Text>
                            <Flex gap="1">
                                {target.canRetry && (
                                    <Button
                                        kind="secondary"
                                        size="small"
                                        disabled={!!onboarding.pending}
                                        onClick={() => void onboarding.retry(target.candidateId)}
                                    >
                                        Retry
                                    </Button>
                                )}
                                {target.canCancel && (
                                    <Button
                                        kind="secondary"
                                        color="danger"
                                        size="small"
                                        disabled={!!onboarding.pending}
                                        onClick={() => void onboarding.cancel(target.candidateId)}
                                    >
                                        Cancel
                                    </Button>
                                )}
                            </Flex>
                        </Flex>
                    ))}
                </Stack>
            )}
        </Stack>
    )
}
