# services JSON-RPC surface (GENERATED)

> **Do not edit by hand.** Regenerate with `npm run service-contracts:write`
> (or `tsx scripts/verify-service-contracts.ts --write`). Extracts the
> current sibling `services/` tree and cross-checks it against the
> Electron bridge. Integration notes live in `docs/services-backend.md`;
> capability status lives in `docs/services-parity.md`.

- **Source tree**: `services/` in this monorepo
- **Versions**: see `services/versions.json` (product, installer, and per-component)
- **Legend**: ✅ referenced by the bridge · ❌ MISSING (no consumer/caller) · ➖ ignored (see `docs/service-contract-exceptions.json`)

## Drift summary

### Notifications emitted but NOT consumed by the bridge
- none ✅

### Requests the backend handles but the bridge never calls (unused capability)
- ⚠️ lmstudio-proxy → node/selected
- ⚠️ lmstudio-proxy → node/set-local-backend
- ⚠️ nvpair-engine-manager → engine:describe
- ⚠️ nvpair-engine-manager → engine:errors
- ⚠️ nvpair-engine-manager → engine:logs
- ⚠️ nvpair-engine-manager → engine:restart
- ⚠️ nvpair-engine-manager → internal:set-reserved-port
- ⚠️ nvpair-job-scheduler → scheduler:get-interval
- ⚠️ nvpair-job-scheduler → scheduler:get-status
- ⚠️ nvpair-job-scheduler → scheduler:set-interval
- ⚠️ nvpair-job-scheduler → scheduler:tick
- ⚠️ nvpair-node-info → n/a
- ⚠️ nvpair-node-settings → settings/get-cluster-auto-sync
- ⚠️ nvpair-node-settings → settings/get-force-ports
- ⚠️ nvpair-node-settings → settings/set-cluster-auto-sync
- ⚠️ nvpair-node-settings → settings/set-force-ports
- ⚠️ nvpair-ui-broker → discovery:unsubscribe
- ⚠️ nvpair-ui-broker → engine:set-reserved-port
- ⚠️ nvpair-ui-broker → engine:unsubscribe
- ⚠️ nvpair-ui-broker → internal:set-reserved-port
- ⚠️ nvpair-ui-broker → proxy:get-status
- ⚠️ nvpair-ui-broker → proxy:unsubscribe
- ⚠️ nvpair-ui-broker → workloads:unsubscribe
- ⚠️ ollama-proxy → node/selected
- ⚠️ ollama-proxy → node/set-local-backend

### Backend binaries not listed in `modular-binaries.ts`
- none ✅

## lmstudio-proxy

| Method | Direction | In bridge? |
|---|---|---|
| `error` | notification (we consume) | ✅ yes |
| `errors:clear` | notification (we consume) | ✅ yes |
| `errors:report` | notification (we consume) | ✅ yes |
| `node/discovered` | notification (we consume) | ✅ yes |
| `node/removed` | notification (we consume) | ✅ yes |
| `node/selection-changed` | notification (we consume) | ➖ ignored |
| `node/updated` | notification (we consume) | ✅ yes |
| `proxy/request` | notification (we consume) | ✅ yes |
| `proxy/request-started` | notification (we consume) | ➖ ignored |
| `ready` | notification (we consume) | ✅ yes |
| `node/add-manual` | request (we call) | ✅ yes |
| `node/remove-manual` | request (we call) | ✅ yes |
| `node/select` | request (we call) | ✅ yes |
| `node/selected` | request (we call) | ⚠️ not called |
| `node/set-local-backend` | request (we call) | ⚠️ not called |
| `node/set-priority` | request (we call) | ✅ yes |
| `nodes/list` | request (we call) | ✅ yes |

**Dynamic / unresolved notify sites (verify by hand — `npm run service-contracts` prints the line numbers):**
- `method (var)  (proxy.go)`

## nvpair-cluster-manager

| Method | Direction | In bridge? |
|---|---|---|
| `cluster:identity-changed` | notification (we consume) | ✅ yes |
| `cluster:invite-canceled` | notification (we consume) | ✅ yes |
| `cluster:invite-expired` | notification (we consume) | ✅ yes |
| `cluster:invite-received` | notification (we consume) | ✅ yes |
| `cluster:trust-changed` | notification (we consume) | ➖ ignored |
| `nodes:changed` | notification (we consume) | ✅ yes |
| `cluster:cancel-invite` | request (we call) | ✅ yes |
| `cluster:create` | request (we call) | ✅ yes |
| `cluster:get-node-id` | request (we call) | ✅ yes |
| `cluster:invite-node` | request (we call) | ✅ yes |
| `cluster:invite-status` | request (we call) | ✅ yes |
| `cluster:leave` | request (we call) | ✅ yes |
| `cluster:respond-to-invite` | request (we call) | ✅ yes |
| `cluster:set-identity` | request (we call) | ✅ yes |
| `nodes:get-initial` | request (we call) | ✅ yes |
| `nodes:remove` | request (we call) | ✅ yes |

**Dynamic / unresolved notify sites (verify by hand — `npm run service-contracts` prints the line numbers):**
- `notifyMethod (var)  (httpserver.go, 2 sites)`

## nvpair-engine-manager

| Method | Direction | In bridge? |
|---|---|---|
| `engine:install-progress` | notification (we consume) | ✅ yes |
| `engine:models-changed` | notification (we consume) | ✅ yes |
| `engine:pull-progress` | notification (we consume) | ✅ yes |
| `engine:ready` | notification (we consume) | ✅ yes |
| `engine:remote-progress` | notification (we consume) | ✅ yes |
| `engine:state-changed` | notification (we consume) | ✅ yes |
| `errors:clear` | notification (we consume) | ✅ yes |
| `errors:report` | notification (we consume) | ✅ yes |
| `engine:action` | request (we call) | ✅ yes |
| `engine:describe` | request (we call) | ⚠️ not called |
| `engine:errors` | request (we call) | ⚠️ not called |
| `engine:get-installed` | request (we call) | ✅ yes |
| `engine:install` | request (we call) | ✅ yes |
| `engine:logs` | request (we call) | ⚠️ not called |
| `engine:models` | request (we call) | ✅ yes |
| `engine:prepare-shutdown` | request (we call) | ✅ yes |
| `engine:remote-delete-model` | request (we call) | ✅ yes |
| `engine:remote-get-installed` | request (we call) | ✅ yes |
| `engine:remote-install` | request (we call) | ✅ yes |
| `engine:remote-load-model` | request (we call) | ✅ yes |
| `engine:remote-pull-model` | request (we call) | ✅ yes |
| `engine:remote-start` | request (we call) | ✅ yes |
| `engine:remote-stop` | request (we call) | ✅ yes |
| `engine:remote-unload-model` | request (we call) | ✅ yes |
| `engine:restart` | request (we call) | ⚠️ not called |
| `engine:set-port` | request (we call) | ✅ yes |
| `engine:start` | request (we call) | ✅ yes |
| `engine:status` | request (we call) | ✅ yes |
| `engine:stop` | request (we call) | ✅ yes |
| `engine:uninstall` | request (we call) | ✅ yes |
| `error` | request (we call) | ✅ yes |
| `internal:set-reserved-port` | request (we call) | ⚠️ not called |

**Dynamic / unresolved notify sites (verify by hand — `npm run service-contracts` prints the line numbers):**
- `method (var)  (executor.go)`

## nvpair-errors

| Method | Direction | In bridge? |
|---|---|---|
| `errors:update` | notification (we consume) | ✅ yes |
| `ready` | notification (we consume) | ✅ yes |
| `errors:clear` | request (we call) | ✅ yes |
| `errors:get-initial` | request (we call) | ✅ yes |
| `errors:report` | request (we call) | ✅ yes |

## nvpair-job-scheduler

| Method | Direction | In bridge? |
|---|---|---|
| `ready` | notification (we consume) | ✅ yes |
| `schedule:priority` | notification (we consume) | ✅ yes |
| `discovery:nodes-changed` | request (we call) | ✅ yes |
| `scheduler:get-interval` | request (we call) | ⚠️ not called |
| `scheduler:get-status` | request (we call) | ⚠️ not called |
| `scheduler:set-interval` | request (we call) | ⚠️ not called |
| `scheduler:tick` | request (we call) | ⚠️ not called |
| `workloads:remove` | request (we call) | ✅ yes |
| `workloads:upsert` | request (we call) | ✅ yes |

## nvpair-manual-nodes

| Method | Direction | In bridge? |
|---|---|---|
| `errors:clear` | notification (we consume) | ✅ yes |
| `errors:report` | notification (we consume) | ✅ yes |
| `node/discovered` | notification (we consume) | ✅ yes |
| `node/removed` | notification (we consume) | ✅ yes |
| `node/updated` | notification (we consume) | ✅ yes |
| `ready` | notification (we consume) | ✅ yes |
| `node/add` | request (we call) | ✅ yes |
| `node/remove` | request (we call) | ✅ yes |
| `nodes/list` | request (we call) | ✅ yes |

## nvpair-node-info

| Method | Direction | In bridge? |
|---|---|---|
| `n/a` | request (we call) | ⚠️ not called |

## nvpair-node-scanner

| Method | Direction | In bridge? |
|---|---|---|
| `ready` | notification (we consume) | ✅ yes |

**Dynamic / unresolved notify sites (verify by hand — `npm run service-contracts` prints the line numbers):**
- `method (var)  (daemon.go, 3 sites)`

## nvpair-node-settings

| Method | Direction | In bridge? |
|---|---|---|
| `connection/cluster-auto-sync` | notification (we consume) | ➖ ignored |
| `connection/cluster-identity` | notification (we consume) | ✅ yes |
| `ready` | notification (we consume) | ✅ yes |
| `settings/get-cluster-auto-sync` | request (we call) | ⚠️ not called |
| `settings/get-cluster-friendly-name` | request (we call) | ✅ yes |
| `settings/get-cluster-id` | request (we call) | ✅ yes |
| `settings/get-force-ports` | request (we call) | ⚠️ not called |
| `settings/set-cluster-auto-sync` | request (we call) | ⚠️ not called |
| `settings/set-cluster-friendly-name` | request (we call) | ✅ yes |
| `settings/set-cluster-id` | request (we call) | ✅ yes |
| `settings/set-force-ports` | request (we call) | ⚠️ not called |

## nvpair-tui

| Method | Direction | In bridge? |
|---|---|---|
| `cluster:identity-changed` | request (we call) | ✅ yes |
| `cluster:invite-received` | request (we call) | ✅ yes |
| `engine:install-progress` | request (we call) | ✅ yes |
| `engine:pull-progress` | request (we call) | ✅ yes |
| `engine:state-changed` | request (we call) | ✅ yes |
| `error` | request (we call) | ✅ yes |
| `nodes:changed` | request (we call) | ✅ yes |
| `workloads:remove` | request (we call) | ✅ yes |
| `workloads:upsert` | request (we call) | ✅ yes |

## nvpair-ui-broker

| Method | Direction | In bridge? |
|---|---|---|
| `app:ready` | notification (we consume) | ✅ yes |
| `discovery:nodes-changed` | notification (we consume) | ✅ yes |
| `engine:restore-enabled` | notification (we consume) | ➖ ignored |
| `errors:clear` | notification (we consume) | ✅ yes |
| `errors:report` | notification (we consume) | ✅ yes |
| `errors:update` | notification (we consume) | ✅ yes |
| `proxy:ready` | notification (we consume) | ✅ yes |
| `workloads:upsert` | notification (we consume) | ✅ yes |
| `connection/cluster-auto-sync` | request (we call) | ➖ ignored |
| `connection/cluster-identity` | request (we call) | ✅ yes |
| `discovery:get-nodes` | request (we call) | ✅ yes |
| `discovery:subscribe` | request (we call) | ✅ yes |
| `discovery:unsubscribe` | request (we call) | ⚠️ not called |
| `engine:install` | request (we call) | ✅ yes |
| `engine:set-port` | request (we call) | ✅ yes |
| `engine:set-reserved-port` | request (we call) | ⚠️ not called |
| `engine:start` | request (we call) | ✅ yes |
| `engine:subscribe` | request (we call) | ✅ yes |
| `engine:unsubscribe` | request (we call) | ⚠️ not called |
| `errors:get-initial` | request (we call) | ✅ yes |
| `internal:set-reserved-port` | request (we call) | ⚠️ not called |
| `node/add` | request (we call) | ✅ yes |
| `node/discovered` | request (we call) | ✅ yes |
| `node/remove` | request (we call) | ✅ yes |
| `node/removed` | request (we call) | ✅ yes |
| `node/updated` | request (we call) | ✅ yes |
| `nodes/list` | request (we call) | ✅ yes |
| `proxy:get-status` | request (we call) | ⚠️ not called |
| `proxy:set-port` | request (we call) | ✅ yes |
| `proxy:subscribe` | request (we call) | ✅ yes |
| `proxy:unsubscribe` | request (we call) | ⚠️ not called |
| `ready` | request (we call) | ✅ yes |
| `workloads:get-initial` | request (we call) | ✅ yes |
| `workloads:remove` | request (we call) | ✅ yes |
| `workloads:subscribe` | request (we call) | ✅ yes |
| `workloads:unsubscribe` | request (we call) | ⚠️ not called |

**Dynamic / unresolved notify sites (verify by hand — `npm run service-contracts` prints the line numbers):**
- `method (var)  (broker.go, 5 sites)`
- `proxy:*  (broker.go)`
- `method (var)  (clustermanager.go)`
- `method (var)  (errors.go)`
- `lmstudio-proxy:*  (lmstudioproxy.go)`
- `method (var)  (proxy.go)`
- `method (var)  (rpcworker.go, 2 sites)`

## nvpair-workload-manager

| Method | Direction | In bridge? |
|---|---|---|
| `ready` | notification (we consume) | ✅ yes |
| `workloads:remove` | notification (we consume) | ✅ yes |
| `workloads:upsert` | notification (we consume) | ✅ yes |

## ollama-proxy

| Method | Direction | In bridge? |
|---|---|---|
| `error` | notification (we consume) | ✅ yes |
| `errors:clear` | notification (we consume) | ✅ yes |
| `errors:report` | notification (we consume) | ✅ yes |
| `node/discovered` | notification (we consume) | ✅ yes |
| `node/removed` | notification (we consume) | ✅ yes |
| `node/selection-changed` | notification (we consume) | ➖ ignored |
| `node/updated` | notification (we consume) | ✅ yes |
| `proxy/request` | notification (we consume) | ✅ yes |
| `proxy/request-started` | notification (we consume) | ➖ ignored |
| `ready` | notification (we consume) | ✅ yes |
| `node/add-manual` | request (we call) | ✅ yes |
| `node/remove-manual` | request (we call) | ✅ yes |
| `node/select` | request (we call) | ✅ yes |
| `node/selected` | request (we call) | ⚠️ not called |
| `node/set-local-backend` | request (we call) | ⚠️ not called |
| `node/set-priority` | request (we call) | ✅ yes |
| `nodes/list` | request (we call) | ✅ yes |

**Dynamic / unresolved notify sites (verify by hand — `npm run service-contracts` prints the line numbers):**
- `method (var)  (proxy.go)`

