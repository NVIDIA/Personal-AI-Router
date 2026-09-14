// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

// Headless control for the MLX engine, for driving PAIR without the desktop app.
// The Go services speak newline-delimited JSON-RPC 2.0 over stdio and expose no
// control surface to curl, so this holds the other end of that pipe.
//
//   node scripts/mlx.mjs install           install the engine (uv + venv + mlx-lm)
//   node scripts/mlx.mjs serve             run the router and the engine (Ctrl-C to stop)
//   node scripts/mlx.mjs status            installed / running / healthy / port
//   node scripts/mlx.mjs models            downloaded catalogue, marking what is resident
//   node scripts/mlx.mjs pull   <repo-id>  hf download
//   node scripts/mlx.mjs delete <repo-id>  remove from the Hugging Face cache
//   node scripts/mlx.mjs port   <n>        move the engine to a port, persistently
//   node scripts/mlx.mjs uninstall         remove the engine
//
// There is deliberately no `start`, `stop` or `load`. engine-manager owns
// mlx_lm.server as a child and SIGTERMs it on shutdown, so a one-shot invocation
// always takes the engine down with it -- "start it and exit" is not a thing
// this architecture can do. `serve` is the long-lived owner instead, and a model
// becomes resident on the first request that asks for it, which is exactly what
// mlx-proxy's on-disk fallback exists for.

import { spawn } from 'node:child_process'
import { existsSync } from 'node:fs'
import { createInterface } from 'node:readline'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

// desktop/cli-bin is what `make build-binaries` produces and what the app runs;
// services/build/bin is what `make build-services` stages. Either will do.
const BIN_DIRS = [
    path.join(ROOT, 'desktop', 'cli-bin'),
    path.join(ROOT, 'services', 'build', 'bin')
]

function binary(name) {
    const found = BIN_DIRS.map(dir => path.join(dir, name)).find(existsSync)
    if (!found) {
        console.error(`${name} not found. Build it first:\n  make build-services`)
        process.exit(1)
    }
    return found
}

// Install downloads uv, builds a virtualenv and installs mlx-lm into it; a pull
// can be many gigabytes. Neither belongs behind a short deadline.
const TIMEOUT = { install: 30 * 60_000, pull: 60 * 60_000, short: 30_000, models: 90_000 }

function progressLine(msg) {
    if (msg.method === 'engine:install-progress' || msg.method === 'engine:pull-progress') {
        const p = msg.params ?? {}
        const text = `${p.stage ?? ''} ${p.percent ?? ''} ${p.message ?? ''}`.trim()
        if (text) console.error(`  ${text}`)
        return true
    }
    return false
}

/**
 * Run `calls` in order against one short-lived worker and resolve with the last
 * result. Sequencing them inside a single process is the point: state that lives
 * in the worker (a running engine, above all) does not survive it.
 */
function session(bin, calls, timeoutMs) {
    return new Promise((resolve, reject) => {
        const child = spawn(binary(bin), [], { stdio: ['pipe', 'pipe', 'pipe'] })
        const timer = setTimeout(() => {
            child.kill()
            reject(new Error(`timed out after ${Math.round(timeoutMs / 1000)}s`))
        }, timeoutMs)

        createInterface({ input: child.stderr }).on('line', line => {
            if (/ERROR|WARN/.test(line)) console.error(`  ${line}`)
        })

        let index = 0
        const send = () => {
            const [method, params] = calls[index]
            child.stdin.write(JSON.stringify({ jsonrpc: '2.0', id: index + 1, method, params }) + '\n')
        }

        createInterface({ input: child.stdout }).on('line', line => {
            let msg
            try {
                msg = JSON.parse(line)
            } catch {
                return
            }
            if (progressLine(msg)) return
            if (msg.id !== index + 1) return
            if (msg.error) {
                clearTimeout(timer)
                child.kill()
                reject(new Error(`${calls[index][0]}: ${msg.error.message}`))
                return
            }
            index += 1
            if (index < calls.length) {
                send()
                return
            }
            clearTimeout(timer)
            child.stdin.write(JSON.stringify({ jsonrpc: '2.0', id: 999, method: 'shutdown' }) + '\n')
            child.stdin.end()
            resolve(msg.result)
        })

        child.on('error', reject)
        send()
    })
}

/**
 * Bring up the whole router and hold it. The broker spawns discovery, the
 * scheduler and all three engine proxies at startup, but it does not start an
 * engine on its own -- that is a user action in the app, and this is its
 * headless equivalent.
 */
function serve() {
    const child = spawn(binary('nvpair-ui-broker'), [], {
        stdio: ['pipe', 'pipe', 'inherit'],
        cwd: path.dirname(binary('nvpair-ui-broker'))
    })

    let proxyPort = null
    createInterface({ input: child.stdout }).on('line', line => {
        let msg
        try {
            msg = JSON.parse(line)
        } catch {
            return
        }
        if (msg.method === 'mlx-proxy:ready' && msg.params?.port) {
            proxyPort = msg.params.port
            announce()
        }
        if (msg.id === 1) {
            if (msg.error) console.error(`\nMLX engine failed to start: ${msg.error.message}\n`)
            else announce()
        }
    })

    let announced = false
    const announce = () => {
        if (announced || proxyPort === null) return
        announced = true
        console.error(
            `\n  Router up. MLX requests go to http://127.0.0.1:${proxyPort}/v1/chat/completions` +
                `\n  Try:  make mlx-ask\n  Ctrl-C to stop.\n`
        )
    }

    // Subscribing is what makes the broker relay mlx-proxy:ready, which is the
    // only place the bound port is reported -- :8080 is contended often enough
    // that assuming it would be wrong as often as right.
    child.stdin.write(JSON.stringify({ jsonrpc: '2.0', id: 2, method: 'mlx-proxy:subscribe' }) + '\n')
    child.stdin.write(
        JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'engine:start', params: { engine: 'mlx' } }) + '\n'
    )

    const stop = () => {
        child.stdin.write(JSON.stringify({ jsonrpc: '2.0', id: 998, method: 'shutdown' }) + '\n')
        setTimeout(() => child.kill(), 15_000)
    }
    process.on('SIGINT', stop)
    process.on('SIGTERM', stop)
    child.on('exit', code => process.exit(code ?? 0))
}

function requireModel(value, command) {
    if (value) return value
    console.error(`usage: node scripts/mlx.mjs ${command} <hugging-face-repo-id>`)
    console.error('example: mlx-community/Llama-3.2-1B-Instruct-4bit')
    process.exit(1)
}

const [command, argument] = process.argv.slice(2)
const model = argument
const EM = 'nvpair-engine-manager'
const action = (name, params, timeout) =>
    session(EM, [['engine:action', { engine: 'mlx', action: name, params }]], timeout)

const commands = {
    serve,
    install: () => session(EM, [['engine:install', { engine: 'mlx', start: true }]], TIMEOUT.install),
    status: () => session(EM, [['engine:status', { engine: 'mlx' }]], TIMEOUT.short),
    uninstall: () => session(EM, [['engine:uninstall', { engine: 'mlx' }]], TIMEOUT.install),
    // list_models is an HTTP action, so the engine has to be up to answer it.
    // Starting it in the same session is honest: it comes up, reports, and goes
    // back down with its parent.
    models: () =>
        session(EM, [['engine:start', { engine: 'mlx' }], ['engine:models', undefined]], TIMEOUT.models),
    pull: () => action('pull_model', { model: requireModel(model, 'pull') }, TIMEOUT.pull),
    delete: () => action('delete_model', { model: requireModel(model, 'delete') }, TIMEOUT.install),
    // Persist the engine's port as a manifest override. Two reasons to use it:
    // to get off a port something else wants, and -- the interesting one -- to
    // point PAIR at an mlx_lm.server you run yourself. engine:start ADOPTS a
    // server already answering on the configured port instead of spawning one,
    // so a hand-tuned server (its own venv, its own flags, its own model) keeps
    // running exactly as you launched it and PAIR just routes to it.
    port: () => {
        const value = Number(argument)
        if (!Number.isInteger(value) || value < 1 || value > 65535) {
            console.error('usage: node scripts/mlx.mjs port <1-65535>')
            process.exit(1)
        }
        return session(EM, [['engine:set-port', { engine: 'mlx', port: value }]], TIMEOUT.short)
    }
}

const run = commands[command]
if (!run) {
    console.error(`unknown command: ${command ?? '(none)'}`)
    console.error(`commands: ${Object.keys(commands).join(', ')}`)
    process.exit(1)
}

if (command === 'serve') {
    serve()
} else {
    try {
        const result = await run()
        // `models` is the interesting one: the split between what is downloaded
        // and what is resident is exactly what routing keys on.
        if (command === 'models') {
            const downloaded = result?.modelsByEngine?.mlx ?? []
            const loaded = result?.loadedByEngine?.mlx ?? []
            console.log(`downloaded (${downloaded.length}):`)
            for (const m of downloaded) console.log(`  ${loaded.includes(m) ? '*' : ' '} ${m}`)
            console.log(loaded.length ? `\nresident: ${loaded.join(', ')}` : '\nresident: nothing loaded')
        } else {
            console.log(JSON.stringify(result, null, 2))
        }
    } catch (err) {
        console.error(String(err.message ?? err))
        process.exit(1)
    }
}
