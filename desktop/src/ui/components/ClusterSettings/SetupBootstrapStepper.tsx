// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useMemo, useState } from 'react'
import {
    Badge,
    Button,
    Checkbox,
    Flex,
    FormField,
    Stack,
    Text,
    TextInput
} from '@nvidia/foundations-react-core'
import type {
    BootstrapArchitecture,
    BootstrapCatalogTarget,
    BootstrapLane,
    BootstrapPlatform,
    BootstrapPublicKeyIdentity,
    BootstrapRequest,
    BootstrapRole,
    BootstrapRuntimeOwner,
    BootstrapTarget,
    OnboardingAuth,
    OnboardingCandidate,
    OnboardingStartupLifetime
} from '@/shared/types/onboarding-live'
import {
    buildBootstrapAccountIdentity,
    parseBootstrapRequest,
    partitionOnboardingCandidates
} from '@/shared/utils/onboarding-live'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import { useOnboardingStore } from '@/ui/stores/onboarding.store'
import { SetupStatus, SetupStepper } from './SetupJourneyAdapters'

const SETUP_STEPS = [
    'Prepare device',
    'Verify bootstrap',
    'Authorize key',
    'Install or update PAIR'
]

function shortDigest(value: string): string {
    return `${value.slice(0, 12)}…${value.slice(-8)}`
}

function defaultRuntimeOwner(target: BootstrapTarget): BootstrapRuntimeOwner {
    return target.platform === 'linux' ? 'headless' : 'desktop'
}

function authorizedUsername(candidate: OnboardingCandidate | null, entered: string): string {
    if (!candidate?.accessAvailable) return entered
    const match = /^([A-Za-z0-9][A-Za-z0-9_.-]{0,95}) \((?:password|existing-key)\)$/u.exec(
        candidate.accessLabel ?? ''
    )
    return match?.[1] ?? entered
}

function catalogTarget(
    targets: BootstrapCatalogTarget[],
    platform: BootstrapPlatform | '',
    architecture: BootstrapArchitecture | ''
): BootstrapCatalogTarget | null {
    if (!platform || !architecture) return null
    return (
        targets.find(
            entry =>
                entry.target.platform === platform && entry.target.architecture === architecture
        ) ?? null
    )
}

function bootstrapRequestFor(
    candidate: OnboardingCandidate,
    target: BootstrapCatalogTarget,
    username: string,
    key: BootstrapPublicKeyIdentity,
    lane: BootstrapLane,
    role: BootstrapRole,
    runtimeOwner: BootstrapRuntimeOwner
): BootstrapRequest {
    return parseBootstrapRequest({
        schemaVersion: 1,
        operationId: crypto.randomUUID().replaceAll('-', ''),
        binding: {
            target: target.target,
            lane,
            role,
            runtimeOwner,
            account: buildBootstrapAccountIdentity(target.target, username),
            controllerKey: key,
            endpoint: { address: candidate.address, port: candidate.port },
            product: target.product.identity,
            helper: target.helper.identity
        }
    })
}

export default function SetupBootstrapStepper() {
    const candidates = useOnboardingStore(state => state.candidates)
    const catalog = useOnboardingStore(state => state.bootstrapCatalog)
    const controllerKeys = useOnboardingStore(state => state.bootstrapControllerKeys)
    const request = useOnboardingStore(state => state.bootstrapRequest)
    const status = useOnboardingStore(state => state.bootstrapStatus)
    const plan = useOnboardingStore(state => state.bootstrapPlan)
    const receipt = useOnboardingStore(state => state.bootstrapReceipt)
    const pending = useOnboardingStore(state => state.pending)
    const error = useOnboardingStore(state => state.error)
    const refreshCandidates = useOnboardingStore(state => state.refreshCandidates)
    const authorizeAccess = useOnboardingStore(state => state.authorizeAccess)
    const loadBootstrapMetadata = useOnboardingStore(state => state.loadBootstrapMetadata)
    const inspectBootstrap = useOnboardingStore(state => state.inspectBootstrap)
    const reviewBootstrap = useOnboardingStore(state => state.reviewBootstrap)
    const applyBootstrap = useOnboardingStore(state => state.applyBootstrap)
    const refreshBootstrap = useOnboardingStore(state => state.refreshBootstrap)
    const recoverBootstrap = useOnboardingStore(state => state.recoverBootstrap)
    const verifyBootstrap = useOnboardingStore(state => state.verifyBootstrap)
    const clearBootstrap = useOnboardingStore(state => state.clearBootstrap)

    const [candidateId, setCandidateId] = useState('')
    const [platform, setPlatform] = useState<BootstrapPlatform | ''>('')
    const [architecture, setArchitecture] = useState<BootstrapArchitecture | ''>('')
    const [lane, setLane] = useState<BootstrapLane>('quick-connect')
    const [role, setRole] = useState<BootstrapRole>('auto')
    const [runtimeOwner, setRuntimeOwner] = useState<BootstrapRuntimeOwner>('headless')
    const [keyFingerprint, setKeyFingerprint] = useState('')
    const [username, setUsername] = useState('')
    const [auth, setAuth] = useState<OnboardingAuth>('existing-key')
    const [keyPath, setKeyPath] = useState('')
    const [password, setPassword] = useState('')
    const [passphrase, setPassphrase] = useState('')
    const [elevationPassword, setElevationPassword] = useState('')
    const [startupLifetime, setStartupLifetime] = useState<OnboardingStartupLifetime>('persistent')
    const [hostKeyAccepted, setHostKeyAccepted] = useState(false)
    const [prepared, setPrepared] = useState(false)

    useEffect(() => {
        const load = async () => {
            await refreshCandidates()
            await loadBootstrapMetadata()
        }
        void load()
    }, [loadBootstrapMetadata, refreshCandidates])

    const partition = useMemo(() => partitionOnboardingCandidates(candidates), [candidates])
    const candidate = candidates.find(current => current.candidateId === candidateId) ?? null
    const candidateBootstrapPlatform = candidate?.bootstrapPlatform
    const candidateBootstrapArchitecture = candidate?.bootstrapArchitecture
    const selectedCatalogTarget = catalogTarget(catalog?.targets ?? [], platform, architecture)
    const selectedKey =
        controllerKeys?.keys.find(key => key.fingerprintSha256 === keyFingerprint) ?? null
    const accessUsername = authorizedUsername(candidate, username)

    useEffect(() => {
        if (!candidateId) return
        setPlatform(candidateBootstrapPlatform ?? '')
        setArchitecture(candidateBootstrapArchitecture ?? '')
        if (candidateBootstrapPlatform && candidateBootstrapArchitecture) {
            setRuntimeOwner(
                defaultRuntimeOwner({
                    platform: candidateBootstrapPlatform,
                    architecture: candidateBootstrapArchitecture
                })
            )
        }
        setPrepared(false)
        setHostKeyAccepted(false)
        clearBootstrap()
    }, [candidateId, candidateBootstrapArchitecture, candidateBootstrapPlatform, clearBootstrap])

    useEffect(() => {
        const first = controllerKeys?.keys[0]
        if (first && !controllerKeys.keys.some(key => key.fingerprintSha256 === keyFingerprint))
            setKeyFingerprint(first.fingerprintSha256)
    }, [controllerKeys, keyFingerprint])

    const reference =
        candidate?.accessId && candidate.hostKeySha256
            ? {
                  candidateId: candidate.candidateId,
                  accessId: candidate.accessId,
                  hostKeySha256: candidate.hostKeySha256
              }
            : null

    const readyForAccess =
        candidate?.bootstrapState === 'ssh-ready' &&
        username.length > 0 &&
        (auth === 'password' ? password.length > 0 : keyPath.trim().length > 0)
    const hostKeyReady =
        candidate?.hostKeyTrusted === true || (!!candidate?.hostKeySha256 && hostKeyAccepted)
    const busy = pending !== null
    const activeStep = receipt
        ? 3
        : plan
          ? 3
          : status
            ? status.phase === 'inspect'
                ? 2
                : 3
            : candidate?.accessAvailable
              ? 1
              : prepared || candidate?.bootstrapState === 'ssh-ready'
                ? 1
                : 0

    const authorize = async () => {
        if (!candidate) return
        try {
            await authorizeAccess({
                candidateIds: [candidate.candidateId],
                username,
                auth,
                startupLifetime,
                ...(auth === 'password' ? { password } : { keyPath, passphrase }),
                ...(elevationPassword ? { elevationPassword } : {})
            })
        } finally {
            setPassword('')
            setPassphrase('')
            setElevationPassword('')
        }
    }

    const runPrimaryAction = async () => {
        if (!candidate || !selectedCatalogTarget) return
        if (candidate.bootstrapState === 'bootstrap-required') {
            setPrepared(true)
            await copyArtifactMetadata(selectedCatalogTarget)
            return
        }
        if (!candidate.accessAvailable) {
            await authorize()
            return
        }
        if (!reference || !selectedKey || !hostKeyReady || !accessUsername) return
        if (receipt) {
            await refreshCandidates()
            return
        }
        if (plan) {
            if (status?.phase === 'apply' || status?.phase === 'verify') {
                await verifyBootstrap({ ...reference, plan })
            } else {
                await applyBootstrap({ ...reference, plan })
            }
            return
        }
        if (status?.phase === 'inspect' && request) {
            await reviewBootstrap({ ...reference, request })
            return
        }
        const nextRequest = bootstrapRequestFor(
            candidate,
            selectedCatalogTarget,
            accessUsername,
            selectedKey,
            lane,
            role,
            runtimeOwner
        )
        await inspectBootstrap({ ...reference, request: nextRequest })
    }

    const primaryDisabled =
        busy ||
        !candidate ||
        !selectedCatalogTarget ||
        (candidate.bootstrapState === 'ssh-ready' &&
            !candidate.accessAvailable &&
            !readyForAccess) ||
        (candidate.bootstrapState === 'ssh-ready' &&
            candidate.accessAvailable &&
            (!reference || !selectedKey || !hostKeyReady || !accessUsername)) ||
        plan?.decision === 'refuse-foreign'

    return (
        <Stack gap="3" className="border border-subtle-color rounded p-3">
            <Stack gap="1">
                <Text kind="body/semibold/sm">Set up a device</Text>
                <Text kind="body/regular/sm" className="text-subtle-color">
                    PAIR uses target-produced inspection, apply, recovery, and verification state.
                    Passwords and passphrases are cleared after each access attempt; controller
                    private keys remain in the operating-system agent.
                </Text>
            </Stack>

            <SetupStepper activeStep={activeStep} steps={SETUP_STEPS} />
            {error && <InlineErrorBanner severity="error" message={error} />}

            <CandidatePartition
                title="Ready for SSH enrollment"
                candidates={partition.sshReady}
                candidateId={candidateId}
                busy={busy}
                onSelect={setCandidateId}
            />
            <CandidatePartition
                title="Observed / bootstrap required"
                candidates={partition.bootstrapRequired}
                candidateId={candidateId}
                busy={busy}
                onSelect={setCandidateId}
            />

            {candidate && (
                <Stack gap="2">
                    <Text kind="body/semibold/sm">
                        {candidate.label} · {candidate.address}:{candidate.port}
                    </Text>
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        Source {candidate.bootstrapSource} · state {candidate.bootstrapState}
                    </Text>
                </Stack>
            )}

            {selectedCatalogTarget && <ArtifactMetadata target={selectedCatalogTarget} />}

            {candidate?.bootstrapState === 'ssh-ready' && !candidate.accessAvailable && (
                <AccessFields
                    busy={busy}
                    username={username}
                    setUsername={setUsername}
                    auth={auth}
                    setAuth={setAuth}
                    keyPath={keyPath}
                    setKeyPath={setKeyPath}
                    password={password}
                    setPassword={setPassword}
                    passphrase={passphrase}
                    setPassphrase={setPassphrase}
                    elevationPassword={elevationPassword}
                    setElevationPassword={setElevationPassword}
                    startupLifetime={startupLifetime}
                    setStartupLifetime={setStartupLifetime}
                />
            )}

            {candidate?.hostKeySha256 && !candidate.hostKeyTrusted && (
                <Checkbox
                    checked={hostKeyAccepted}
                    disabled={busy || candidate.bootstrapState !== 'ssh-ready'}
                    onCheckedChange={checked => setHostKeyAccepted(checked === true)}
                    slotLabel={`Accept observed SSH host fingerprint ${candidate.hostKeySha256} for this setup`}
                />
            )}

            {candidate?.accessAvailable && (
                <FormField
                    slotLabel="Controller public-key identity"
                    slotHelp="Only public fingerprints are displayed. Key material comes from the OS agent or configured .pub files."
                >
                    <select
                        aria-label="Controller public-key identity"
                        value={keyFingerprint}
                        disabled={busy || (controllerKeys?.keys.length ?? 0) === 0}
                        onChange={event => {
                            setKeyFingerprint(event.target.value)
                            clearBootstrap()
                        }}
                        className="border border-subtle-color rounded p-2"
                    >
                        {(controllerKeys?.keys ?? []).map(key => (
                            <option key={key.fingerprintSha256} value={key.fingerprintSha256}>
                                {key.algorithm} · {key.fingerprintSha256}
                            </option>
                        ))}
                    </select>
                </FormField>
            )}

            <details className="border border-subtle-color rounded p-2">
                <summary>Advanced setup controls</summary>
                <Stack gap="2" className="pt-2">
                    <Flex gap="2" wrap="wrap">
                        <FormField slotLabel="Platform override">
                            <select
                                aria-label="Platform override"
                                value={platform}
                                disabled={busy}
                                onChange={event => {
                                    const value = event.target.value
                                    setPlatform(
                                        value === 'windows' ||
                                            value === 'darwin' ||
                                            value === 'linux'
                                            ? value
                                            : ''
                                    )
                                    clearBootstrap()
                                }}
                                className="border border-subtle-color rounded p-2"
                            >
                                <option value="">Select platform</option>
                                <option value="windows">Windows</option>
                                <option value="darwin">MacOS</option>
                                <option value="linux">Linux</option>
                            </select>
                        </FormField>
                        <FormField slotLabel="Architecture override">
                            <select
                                aria-label="Architecture override"
                                value={architecture}
                                disabled={busy}
                                onChange={event => {
                                    const value = event.target.value
                                    setArchitecture(
                                        value === 'amd64' || value === 'arm64' ? value : ''
                                    )
                                    clearBootstrap()
                                }}
                                className="border border-subtle-color rounded p-2"
                            >
                                <option value="">Select architecture</option>
                                <option value="amd64">x64</option>
                                <option value="arm64">arm64</option>
                            </select>
                        </FormField>
                        <FormField slotLabel="Role">
                            <select
                                aria-label="PAIR role"
                                value={role}
                                disabled={busy}
                                onChange={event => {
                                    const value = event.target.value
                                    if (
                                        value === 'auto' ||
                                        value === 'desktop' ||
                                        value === 'headless'
                                    ) {
                                        setRole(value)
                                        if (value !== 'auto') setRuntimeOwner(value)
                                        clearBootstrap()
                                    }
                                }}
                                className="border border-subtle-color rounded p-2"
                            >
                                <option value="auto">Auto</option>
                                <option value="desktop">Desktop</option>
                                <option value="headless">Headless</option>
                            </select>
                        </FormField>
                        <FormField slotLabel="Lane">
                            <select
                                aria-label="Setup lane"
                                value={lane}
                                disabled={busy}
                                onChange={event => {
                                    setLane(
                                        event.target.value === 'zero-touch'
                                            ? 'zero-touch'
                                            : 'quick-connect'
                                    )
                                    clearBootstrap()
                                }}
                                className="border border-subtle-color rounded p-2"
                            >
                                <option value="quick-connect">Quick Connect</option>
                                <option value="zero-touch">Zero Touch</option>
                            </select>
                        </FormField>
                    </Flex>
                    <Button
                        kind="secondary"
                        size="small"
                        disabled={busy}
                        onClick={() => void loadBootstrapMetadata()}
                    >
                        Refresh catalog and public keys
                    </Button>
                </Stack>
            </details>

            <Flex gap="2" wrap="wrap">
                <Button
                    kind="primary"
                    size="small"
                    disabled={primaryDisabled}
                    onClick={() => void runPrimaryAction()}
                >
                    {busy ? 'Setting up device…' : 'Set up device'}
                </Button>
                <Button
                    kind="secondary"
                    size="small"
                    disabled={busy}
                    onClick={() => void refreshCandidates()}
                >
                    Refresh devices
                </Button>
                {reference && plan && !receipt && (
                    <Button
                        kind="secondary"
                        size="small"
                        disabled={busy}
                        onClick={() =>
                            void refreshBootstrap({
                                ...reference,
                                operationId: plan.operationId
                            })
                        }
                    >
                        Refresh setup status
                    </Button>
                )}
                {reference && plan && !receipt && (
                    <Button
                        kind="secondary"
                        size="small"
                        disabled={busy}
                        onClick={() =>
                            void recoverBootstrap({
                                ...reference,
                                operationId: plan.operationId
                            })
                        }
                    >
                        Recover setup
                    </Button>
                )}
            </Flex>

            <JourneyStatus
                candidate={candidate}
                prepared={prepared}
                pending={pending}
                status={status}
                plan={plan}
                receipt={receipt}
            />
        </Stack>
    )
}

async function copyArtifactMetadata(target: BootstrapCatalogTarget): Promise<void> {
    const metadata = [
        `target=${target.target.platform}/${target.target.architecture}`,
        `bootstrap=${target.bootstrap.fileName}`,
        `version=${target.bootstrap.identity.version}`,
        `sha256=${target.bootstrap.identity.sha256}`,
        `size=${target.bootstrap.size}`,
        `provenance=${target.bootstrap.provenance}`,
        `signature=${target.bootstrap.signature.status}/${target.bootstrap.signature.kind}`,
        `signingIdentity=${target.bootstrap.signature.identity}`,
        `notarized=${target.bootstrap.signature.notarized}`,
        `combination=${target.combination.fileName}`,
        `combinationSha256=${target.combination.sha256}`
    ].join('\n')
    await window.windowApi.window.copyToClipboard(metadata)
}

function CandidatePartition({
    title,
    candidates,
    candidateId,
    busy,
    onSelect
}: {
    title: string
    candidates: OnboardingCandidate[]
    candidateId: string
    busy: boolean
    onSelect(candidateId: string): void
}) {
    return (
        <fieldset className="border border-subtle-color rounded p-2">
            <legend>
                <Text kind="body/semibold/sm">{title}</Text>
            </legend>
            <Stack gap="1">
                {candidates.map(candidate => (
                    <label key={candidate.candidateId}>
                        <Flex align="center" gap="2">
                            <input
                                type="radio"
                                name="setup-candidate"
                                value={candidate.candidateId}
                                checked={candidateId === candidate.candidateId}
                                disabled={busy}
                                onChange={() => onSelect(candidate.candidateId)}
                            />
                            <Text kind="body/regular/sm">
                                {candidate.label} · {candidate.address}:{candidate.port}
                            </Text>
                            <Badge
                                color={
                                    candidate.bootstrapState === 'ssh-ready' ? 'green' : 'yellow'
                                }
                                kind="solid"
                            >
                                {candidate.bootstrapState}
                            </Badge>
                        </Flex>
                    </label>
                ))}
                {candidates.length === 0 && (
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        None.
                    </Text>
                )}
            </Stack>
        </fieldset>
    )
}

function ArtifactMetadata({ target }: { target: BootstrapCatalogTarget }) {
    const signed = target.bootstrap.signature.status === 'signed'
    return (
        <SetupStatus
            title={signed ? 'Signed bootstrap catalog' : 'Unsigned engineering catalog'}
            tone="neutral"
        >
            Copy: {target.bootstrap.fileName} · Download metadata: {target.bootstrap.size} bytes,
            SHA-256 {shortDigest(target.bootstrap.identity.sha256)} · Enterprise distribution:{' '}
            {target.bootstrap.provenance}. Signature: {target.bootstrap.signature.kind}
            {target.bootstrap.signature.notarized ? ', notarized' : ''}. Fixed helper{' '}
            {target.helper.identity.id}/{target.helper.identity.version}, product{' '}
            {target.product.identity.id}/{target.product.identity.version}, and combination{' '}
            {target.combination.fileName}.
        </SetupStatus>
    )
}

interface AccessFieldsProps {
    busy: boolean
    username: string
    setUsername(value: string): void
    auth: OnboardingAuth
    setAuth(value: OnboardingAuth): void
    keyPath: string
    setKeyPath(value: string): void
    password: string
    setPassword(value: string): void
    passphrase: string
    setPassphrase(value: string): void
    elevationPassword: string
    setElevationPassword(value: string): void
    startupLifetime: OnboardingStartupLifetime
    setStartupLifetime(value: OnboardingStartupLifetime): void
}

function AccessFields(props: AccessFieldsProps) {
    return (
        <Stack gap="2">
            <Text kind="body/semibold/sm">Temporary device access</Text>
            <Flex gap="2" wrap="wrap">
                <FormField slotLabel="Account">
                    <TextInput
                        value={props.username}
                        onValueChange={props.setUsername}
                        disabled={props.busy}
                    />
                </FormField>
                <FormField slotLabel="Authentication">
                    <select
                        aria-label="Authentication"
                        value={props.auth}
                        disabled={props.busy}
                        onChange={event =>
                            props.setAuth(
                                event.target.value === 'password' ? 'password' : 'existing-key'
                            )
                        }
                        className="border border-subtle-color rounded p-2"
                    >
                        <option value="existing-key">Existing SSH key</option>
                        <option value="password">Password</option>
                    </select>
                </FormField>
                <FormField slotLabel="Startup">
                    <select
                        aria-label="Startup lifetime"
                        value={props.startupLifetime}
                        disabled={props.busy}
                        onChange={event =>
                            props.setStartupLifetime(
                                event.target.value === 'session' ? 'session' : 'persistent'
                            )
                        }
                        className="border border-subtle-color rounded p-2"
                    >
                        <option value="persistent">Persistent</option>
                        <option value="session">This session</option>
                    </select>
                </FormField>
            </Flex>
            <Flex gap="2" wrap="wrap">
                {props.auth === 'existing-key' ? (
                    <>
                        <FormField slotLabel="Absolute key path">
                            <TextInput
                                value={props.keyPath}
                                onValueChange={props.setKeyPath}
                                disabled={props.busy}
                            />
                        </FormField>
                        <FormField slotLabel="Key passphrase (not retained)">
                            <TextInput
                                type="password"
                                value={props.passphrase}
                                onValueChange={props.setPassphrase}
                                disabled={props.busy}
                            />
                        </FormField>
                    </>
                ) : (
                    <FormField slotLabel="Account password (not retained)">
                        <TextInput
                            type="password"
                            value={props.password}
                            onValueChange={props.setPassword}
                            disabled={props.busy}
                        />
                    </FormField>
                )}
                <FormField slotLabel="Administrator password (not retained)">
                    <TextInput
                        type="password"
                        value={props.elevationPassword}
                        onValueChange={props.setElevationPassword}
                        disabled={props.busy}
                    />
                </FormField>
            </Flex>
        </Stack>
    )
}

function JourneyStatus({
    candidate,
    prepared,
    pending,
    status,
    plan,
    receipt
}: {
    candidate: OnboardingCandidate | null
    prepared: boolean
    pending: string | null
    status: ReturnType<typeof useOnboardingStore.getState>['bootstrapStatus']
    plan: ReturnType<typeof useOnboardingStore.getState>['bootstrapPlan']
    receipt: ReturnType<typeof useOnboardingStore.getState>['bootstrapReceipt']
}) {
    if (pending)
        return (
            <SetupStatus title="Target operation in progress" tone="progress" busy>
                {pending}
            </SetupStatus>
        )
    if (receipt)
        return (
            <SetupStatus title="PAIR setup verified" tone="success">
                Receipt {receipt.operationId} · phase {receipt.phase} · helper{' '}
                {receipt.verified.helper.ownership} at {receipt.binding.helper.path} · SSH host
                fingerprint {candidate?.hostKeySha256 ?? 'unavailable'}.
            </SetupStatus>
        )
    if (plan)
        return (
            <SetupStatus
                title={`Reviewed plan: ${plan.decision}`}
                tone={plan.decision === 'refuse-foreign' ? 'error' : 'neutral'}
            >
                {plan.actions.length > 0
                    ? `Actions: ${plan.actions.join(', ')}`
                    : 'The exact requested state is already present.'}
            </SetupStatus>
        )
    if (status)
        return (
            <SetupStatus title={`Bootstrap ${status.phase}`} tone="neutral">
                Helper {status.observed.helper.ownership} · product{' '}
                {status.observed.product.ownership} · SSH host fingerprint{' '}
                {candidate?.hostKeySha256 ?? 'unavailable'}.
            </SetupStatus>
        )
    if (candidate?.bootstrapState === 'bootstrap-required')
        return (
            <SetupStatus
                title={prepared ? 'Bootstrap metadata copied' : 'Device preparation required'}
                tone="neutral"
            >
                Run the signed target bootstrap, then refresh until the backend reports ssh-ready.
                SSH enrollment controls stay disabled in this state.
            </SetupStatus>
        )
    return (
        <SetupStatus title="Choose a device" tone="neutral">
            Select a backend-reported candidate to begin.
        </SetupStatus>
    )
}
