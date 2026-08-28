// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { spawn, type ChildProcessWithoutNullStreams } from 'child_process'
import readline from 'readline'
import { EventEmitter } from 'events'
import getErrorString from '@/shared/utils/get-error-string'
import { createStructuredLogger } from '@/shared/utils/log'
import { redactSensitiveLogText } from '@/electron/redact-log'

export type JsonValue = string | number | boolean | null | JsonObject | JsonValue[]

export interface JsonObject {
    [key: string]: JsonValue
}

interface JsonRpcError {
    code: number
    message: string
    data?: JsonValue
}

export class JsonRpcResponseError extends Error {}

type JsonRpcId = number | string

interface JsonRpcMessage {
    jsonrpc: '2.0'
    id?: JsonRpcId
    method?: string
    params?: JsonValue
    result?: JsonValue
    error?: JsonRpcError
}

export interface JsonRpcNotification {
    source: string
    method: string
    params?: JsonValue
}

/**
 * A request a child subprocess sent to us (it carries both `id` and `method`).
 * The owner answers via `respond` / `respondError` exactly once. No broker-owned
 * worker currently sends inbound requests; `handleChildRequest` replies
 * method-not-found.
 */
export interface JsonRpcInboundRequest {
    source: string
    method: string
    params?: JsonValue
    id: JsonRpcId
}

interface PendingCall {
    resolve: (value: JsonValue | undefined) => void
    reject: (error: Error) => void
    timeout?: ReturnType<typeof setTimeout>
}

interface JsonRpcSubprocessEvents {
    notification: [JsonRpcNotification]
    request: [JsonRpcInboundRequest]
    log: [{ source: string; stream: 'stdout' | 'stderr'; text: string }]
    exit: [{ source: string; code: number | null }]
}

const log = createStructuredLogger('service-bridge')

export class JsonRpcSubprocess extends EventEmitter<JsonRpcSubprocessEvents> {
    readonly name: string
    readonly binaryPath: string

    private child: ChildProcessWithoutNullStreams | null = null
    private pending = new Map<number, PendingCall>()
    private nextId = 0
    private writeTail: Promise<void> = Promise.resolve()

    constructor(name: string, binaryPath: string) {
        super()
        this.name = name
        this.binaryPath = binaryPath
    }

    get running(): boolean {
        return this.child !== null
    }

    start(args: string[] = []): void {
        if (this.child) return

        const child = spawn(this.binaryPath, args, {
            stdio: ['pipe', 'pipe', 'pipe'],
            detached: false
        })
        this.child = child

        readline.createInterface({ input: child.stdout }).on('line', line => {
            this.handleStdout(line)
        })

        // Emitting is all this owes the line. The owner listening on 'log'
        // records it — at the severity the line reports — and writing here as
        // well is what put every service message in the log twice.
        readline.createInterface({ input: child.stderr }).on('line', line => {
            if (!line) return
            this.emit('log', {
                source: this.name,
                stream: 'stderr',
                text: redactSensitiveLogText(line)
            })
        })

        child.on('exit', code => {
            this.child = null
            this.failPending(new Error(`${this.name} exited before replying`))
            this.emit('exit', { source: this.name, code })
        })

        child.on('error', err => {
            this.child = null
            this.failPending(err)
            log.error({
                sublevel: this.name,
                message: `Failed to start ${this.name}: ${getErrorString(err)}`
            })
            // A spawn failure never fires 'exit', so emit it here too: callers
            // (e.g. the supervisor's broker-crash handler) treat a failure to
            // launch the same as an unexpected exit.
            this.emit('exit', { source: this.name, code: null })
        })
    }

    async call(
        method: string,
        params?: JsonValue,
        timeoutMs: number | null = 10_000
    ): Promise<JsonValue | undefined> {
        const child = this.child
        if (!child) throw new Error(`${this.name} is not running`)

        const id = ++this.nextId
        const message: JsonRpcMessage = { jsonrpc: '2.0', id, method, params }

        return new Promise((resolve, reject) => {
            const timeout =
                timeoutMs === null
                    ? undefined
                    : setTimeout(() => {
                          this.pending.delete(id)
                          reject(new Error(`${this.name} ${method} timed out`))
                      }, timeoutMs)
            this.pending.set(id, { resolve, reject, timeout })
            this.write(message).catch(err => {
                if (timeout) clearTimeout(timeout)
                this.pending.delete(id)
                reject(err)
            })
        })
    }

    /**
     * Fire a request after its stdin write without waiting for the response.
     * Use {@link sendWithResponse} only when a caller must observe admission.
     * The backend runs it in its own goroutine and reports through notifications
     * (`engine:install-progress`, `engine:state-changed`, `errors:report`) per
     * the reactive command model — so there is no result to await and, crucially,
     * no short request timeout to fabricate a failure on long-running operations.
     */
    send(method: string, params?: JsonValue): Promise<void> {
        const id = ++this.nextId
        return this.write({ jsonrpc: '2.0', id, method, params })
    }

    /** Observe a long-running request's eventual response without imposing a timeout. */
    sendWithResponse(method: string, params?: JsonValue): Promise<void> {
        return this.call(method, params, null).then(() => undefined)
    }

    notify(method: string, params?: JsonValue): Promise<void> {
        return this.write({ jsonrpc: '2.0', method, params })
    }

    /** Reply to a child-initiated request (see {@link JsonRpcInboundRequest}). */
    respond(id: JsonRpcId, result: JsonValue): Promise<void> {
        return this.write({ jsonrpc: '2.0', id, result })
    }

    respondError(id: JsonRpcId, code: number, message: string): Promise<void> {
        return this.write({ jsonrpc: '2.0', id, error: { code, message } })
    }

    /**
     * Gracefully stop the subprocess: send `shutdown`, close stdin, then wait up
     * to `graceMs` for a clean exit before `SIGKILL`. `graceMs` exists because a
     * worker may need to tear down its own children on exit — notably
     * `nvpair-engine-manager`, which runs `StopAll()` to stop any engines it
     * launched (e.g. Ollama) so they aren't orphaned. SIGKILLing it mid-StopAll
     * would leave those engine processes running, so it gets a longer grace.
     *
     * The wait is reported because the grace is the only thing standing between a
     * blocked main process and a quit that looks like a backend hang: a starved
     * timer fires as soon as the loop resumes, so an instant shutdown can present
     * as the full grace elapsing. Logging the measured wait, and whether the child
     * was actually signalled, is what tells those two apart in a field log.
     */
    async stop(graceMs = 5_000): Promise<void> {
        const child = this.child
        if (!child) return

        try {
            await this.call('shutdown', null, 2_000)
        } catch {
            /* some helpers only shut down when stdin closes */
        }

        const startedAt = Date.now()
        child.stdin.end()
        const exited = await this.waitForExit(child, graceMs)
        const waitedMs = Date.now() - startedAt

        if (!exited) {
            child.kill('SIGKILL')
            log.warn({
                sublevel: this.name,
                message: `${this.name} did not exit within its ${graceMs}ms shutdown grace (waited ${waitedMs}ms); sent SIGKILL`
            })
        } else if (waitedMs > graceMs) {
            log.warn({
                sublevel: this.name,
                message: `${this.name} exited cleanly but the wait took ${waitedMs}ms, past its ${graceMs}ms grace — the main process was blocked, not the child`
            })
        } else {
            log.info({
                sublevel: this.name,
                message: `${this.name} exited in ${waitedMs}ms`
            })
        }
        this.child = null
    }

    /**
     * Resolve true if the child exited within `graceMs`, false if it is still
     * running and should be signalled.
     *
     * The `setImmediate` is the point. When the main process stalls, this timer
     * runs late with the child's `exit` already queued behind it, and killing
     * from the timer callback blames a process that has already stopped. Yielding
     * once lands in the check phase after the poll phase that delivers that exit,
     * so a pending exit is always seen before the escalation.
     */
    private waitForExit(child: ChildProcessWithoutNullStreams, graceMs: number): Promise<boolean> {
        // The child may already be gone: `exit` fires during the shutdown call
        // above, or while an earlier process in the teardown loop is being joined,
        // and a listener attached after that never fires again. Without this the
        // wait runs the entire grace and then reports a child that left early as a
        // blocked main process — pointing the new diagnostic at the wrong side.
        if (child.exitCode !== null || child.signalCode !== null) return Promise.resolve(true)

        return new Promise<boolean>(resolve => {
            let settled = false
            const finish = (exited: boolean): void => {
                if (settled) return
                settled = true
                clearTimeout(timer)
                child.removeListener('exit', onExit)
                resolve(exited)
            }
            const onExit = (): void => finish(true)
            const timer = setTimeout(() => {
                setImmediate(() => {
                    finish(child.exitCode !== null || child.signalCode !== null)
                })
            }, graceMs)
            child.once('exit', onExit)
        })
    }

    private async write(message: JsonRpcMessage): Promise<void> {
        const child = this.child
        if (!child) throw new Error(`${this.name} is not running`)

        this.writeTail = this.writeTail.then(
            () =>
                new Promise<void>((resolve, reject) => {
                    child.stdin.write(`${JSON.stringify(message)}\n`, err => {
                        if (err) {
                            reject(err)
                            return
                        }
                        resolve()
                    })
                })
        )
        return this.writeTail
    }

    private handleStdout(line: string): void {
        if (!line) return
        // Redact only what is logged; `line` stays intact for the parse below so
        // the pairing flow still receives the real PIN it has to display.
        this.emit('log', {
            source: this.name,
            stream: 'stdout',
            text: redactSensitiveLogText(line)
        })

        let message: JsonRpcMessage
        try {
            message = JSON.parse(line) as JsonRpcMessage
        } catch {
            // Already emitted above; the owner logs it.
            return
        }

        if (message.id !== undefined && message.method) {
            this.emit('request', {
                source: this.name,
                method: message.method,
                params: message.params,
                id: message.id
            })
            return
        }

        if (message.id !== undefined) {
            this.deliverResponse(message)
            return
        }

        if (message.method) {
            this.emit('notification', {
                source: this.name,
                method: message.method,
                params: message.params
            })
        }
    }

    private deliverResponse(message: JsonRpcMessage): void {
        if (typeof message.id !== 'number') return
        const pending = this.pending.get(message.id)
        if (!pending) return

        this.pending.delete(message.id)
        if (pending.timeout) clearTimeout(pending.timeout)

        if (message.error) {
            pending.reject(
                new JsonRpcResponseError(`${message.error.code}: ${message.error.message}`)
            )
            return
        }

        pending.resolve(message.result)
    }

    private failPending(error: Error): void {
        for (const pending of Array.from(this.pending.values())) {
            if (pending.timeout) clearTimeout(pending.timeout)
            pending.reject(error)
        }
        this.pending.clear()
    }
}
