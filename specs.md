# mcp-server — Specification

> This repository is checked out as the `mcp-server/` submodule of [GameplanePanel/Gameplane](https://github.com/GameplanePanel/Gameplane). Paths such as `docs/`, `operator/`, `api/`, `charts/` and `web/` below refer to that repository.

**Status:** pre-v1 (v0.4.1)  
**Module / package:** `github.com/GameplanePanel/gameplane/mcp-server`

## Purpose

Optional, strictly **read-only** Model Context Protocol (MCP) server that exposes Gameplane state to an AI assistant. It reads 7 of Gameplane's 9 CRDs (not Cluster or NetworkCapture), Pods, Events, and pod logs, and returns suggested fixes as plain text (YAML and/or `kubectl` commands) for a human operator to review and apply. It runs in one of two modes:

- **Per-user** (`GAMEPLANE_PANEL_URL` set): reads through the Gameplane API as the owner of an MCP token, across the clusters that user may read. The process holds no Kubernetes credentials.
- **Break-glass** (no panel URL): reads the local cluster directly with the process's Kubernetes credentials, optionally limited to a namespace allow-list. For administrators; works while the API is down.
- A `GAMEPLANE_MCP_TOKEN` without `GAMEPLANE_PANEL_URL` is a start-up error. `serve` refuses to start rather than fall back to break-glass mode with a token set.

It speaks MCP (JSON-RPC 2.0) over stdio, not a network port.

## Responsibilities

1. **Expose read-only MCP tools** — 7 tools listed below, all List/Get-shaped or text-generating.
2. **Guarantee read-only operation** — through layered enforcement (see Key Invariants).
3. **Index 7 of Gameplane's 9 CRDs** — GameServer, GameTemplate, Backup, BackupSchedule, Restore, Module, ModuleSource (not Cluster or NetworkCapture), by name and optional label selector.
4. **Expose core Kubernetes resources** — Pods, Events, and pod logs, scoped to namespace (or all namespaces in the selected scope).
5. **Select the read scope per call** — an optional `cluster` argument (per-user mode) and `server` argument carried to the readers through `reader.Target` in the request context.
6. **Generate fix suggestions** — the `propose_fix` tool matches symptoms against heuristic keyword rules and returns suggested diagnostics and remediation steps as plain text.
7. **Support two subcommands** — `idle` (the default, long-lived Deployment process) and `serve` (per-client MCP session over stdio, in either mode).

## Non-goals / boundaries

- **No mutations** — never creates, updates, patches, deletes, or applies anything. Guarded structurally, by RBAC (break-glass) and by the API's token guard (per-user).
- **No HTTP port** — stdio-only transport, no listening socket. The per-user client is an outbound HTTPS client only.
- **No LLM calls** — fix suggestions are deterministic, keyword-matched heuristic text.
- **No config management** — reads Kubernetes objects and API responses as-is; does not validate them, enforce schemas, or trigger reconciliation.
- **No cross-cluster writes or gets** — cross-cluster fan-out applies to lists only.
- For run instructions and Helm values, see `mcp-server/README.md`.

## Directory & package layout

```
mcp-server/
├── main.go                   # Entry point; idle/serve subcommands (serve flags, mode selection); newMCPServer
├── tools.go                  # Tool registration; 7 handler functions taking reader.Reader; readOnlyTool helper
├── fixadvice.go              # Heuristic advice text; buildFixSuggestion; matchFixRules
├── main_test.go              # Tool-set registration tests; kube client invariant tripwire
├── panel_invariants_test.go  # Panel client invariant tripwire; both clients satisfy reader.Reader
├── internal/reader/
│   └── reader.go             # Reader interface (six read methods); Target and WithTarget/TargetFrom context helpers
├── internal/kube/
│   ├── client.go             # Break-glass Client; namespace allow-list (WithNamespaces, ParseNamespaces); List/Get methods; CRDKinds registry
│   ├── allowlist_test.go     # Allow-list and cluster-rejection tests (fake clientsets)
│   └── client_test.go        # Fake client tests
├── internal/panel/
│   ├── client.go             # Per-user Client; New; single GET request helper (get); kind→path table; per-kind reads
│   ├── fanout.go             # Cross-cluster fan-out for list calls; cluster annotation; unavailable-cluster notice
│   ├── client_test.go        # httptest-based path, header, error and conversion tests
│   ├── fanout_test.go        # Fan-out merge, unavailable and skipped-cluster tests
│   └── source_test.go        # Source tripwire: GET-only HTTP usage in the package
├── go.mod                    # Dependencies
├── go.sum
├── .testcoverage.yml         # Coverage gate (70%)
└── README.md                 # Run instructions; two modes; RBAC scope; transport details
```

## External interface / contracts

### Subcommands

- **`idle`** (default, no args) — blocks indefinitely until signaled (SIGTERM/SIGINT). Performs no cluster or API access. Used as the Helm Deployment's container process so a live pod exists for `kubectl exec` to attach to.
- **`serve [--panel-url URL] [--panel-ca FILE]`** — runs one MCP session over stdio until the client disconnects. Flags are parsed with `flag.NewFlagSet("serve", flag.ContinueOnError)`.
  - `--panel-url` (env `GAMEPLANE_PANEL_URL`): when set, per-user mode. The URL is parsed and must use `https`, or `http` for a `localhost` or loopback host. Userinfo and query are rejected or stripped. Logged with the query removed.
  - `--panel-ca` (env `GAMEPLANE_PANEL_CA`): PEM CA bundle appended to the system cert pool, TLS 1.2 minimum. Per-user mode only.
  - `GAMEPLANE_MCP_TOKEN` (environment only, no flag): required in per-user mode and must start with `gpmcp_`. Its absence is a start-up error. It never appears in logs or errors.
  - Break-glass (no panel URL): builds the Kubernetes client via `ctrl.GetConfig()` (in-cluster or kubeconfig) and applies the namespace allow-list from `GAMEPLANE_MCP_NAMESPACES` (comma-separated; unset means unrestricted).
  - Logging: `slog` writes to stderr, since stdout is the MCP stream. `main` sets the default handler first.
  - Usage example: `kubectl exec -i deploy/gameplane-mcp-server -n <ns> -- /mcp-server serve` (break-glass), or `GAMEPLANE_PANEL_URL=… GAMEPLANE_MCP_TOKEN=gpmcp_… mcp-server serve` (per-user).

### Registered tools (7 total)

From `tools.go` `registeredToolNames`:

| Tool | Input | Output |
|---|---|---|
| `list_gameplane_resources` | `kind` (e.g., "GameServer"), optional `namespace`, optional `labelSelector`, optional `cluster` | Unstructured list of CRD objects as JSON |
| `get_gameplane_resource` | `kind`, optional `namespace`, `name`, optional `cluster` | Single Gameplane CRD object as JSON |
| `list_pods` | Optional `namespace`, optional `labelSelector`, optional `cluster`, optional `server` | Core Pod list as JSON |
| `get_pod` | `namespace`, `name`, optional `cluster` | Single Pod (spec+status) as JSON |
| `list_events` | Optional `namespace`, optional `fieldSelector`, optional `labelSelector`, optional `cluster`, optional `server` | Core Event list as JSON |
| `get_pod_logs` | `namespace`, `pod`, optional `container`, optional `tailLines` (capped 5000), optional `previous`, optional `cluster`, optional `server` | Log text (up to 256 KiB of the newest bytes; when truncated, prefixed with a notice) |
| `propose_fix` | Optional `kind`/`namespace`/`name`, optional `cluster`, required `symptom` (free text) | Suggested diagnostics + fix text (never applies anything) |

**Scope arguments** (all optional, carried to the reader as `reader.Target`):

- `cluster` — a registered cluster ID. Per-user mode only. Break-glass mode rejects any non-empty value with `errClusterNotAvailable`. Empty: list calls fan out across every cluster the user may read, each item annotated `gameplane.local/cluster`; get calls and log reads use the local cluster.
- `server` — a GameServer name. Accepted by `list_pods`, `list_events` and `get_pod_logs`. Per-user mode uses it to route pod, event and log reads through `/servers/{name}/…`. Break-glass mode ignores it. In per-user mode `get_pod_logs` requires it for game server pods.

Pod logs read the complete finite Kubernetes tail response while retaining only a 256 KiB rolling buffer. Long lines can make the requested tail exceed 4 MiB; the returned suffix still includes the actual final bytes. Request cancellation and stream read failures return errors instead of presenting partial output as the newest logs. In per-user mode, logs come from the API's server-log route (`/servers/{name}/logs/pod/tail`), or from `/admin/system-logs/{api|operator}` for the Gameplane API and operator pods. Tail length is clamped: 0 becomes 200, values above 5000 become 5000.

**Artifact scope:**
- **7 of Gameplane's 9 CRDs** (not Cluster or NetworkCapture): GameServer, GameTemplate, Backup, BackupSchedule, Restore, Module, ModuleSource (4 namespaced, 3 cluster-scoped; verified against `CRDKinds` in `internal/kube/client.go`).
- **Core resources**: `v1` Pods, Events, `pods/log` subresource.
- **No write verbs**: no Create/Update/Patch/Delete/Apply anywhere.

### Reader contract (`internal/reader`)

- `Reader` interface: `ListCRD`, `GetCRD`, `ListPods`, `GetPod`, `ListEvents`, `PodLogs`. Implemented by `*kube.Client` and `*panel.Client`. Both satisfy it, checked by `var _` assertions and `TestReadersImplementInterface`.
- `Target{Cluster, Server}` travels in the context (`WithTarget` / `TargetFrom`, unexported context key). Each tool handler sets it from its input before calling the reader.

### Per-user API contract (`internal/panel`)

- Every request is a `GET` to the Gameplane API, with `Authorization: Bearer <token>`, an `X-Gameplane-MCP-Session` header (random per process), and `Accept: application/json`. `cluster=<id>` is added to the query whenever the target names one.
- Kind to path: GameServer `servers`, GameTemplate `templates`, Backup `backups`, BackupSchedule `schedules`, Restore `restores`, Module `modules`, ModuleSource `modules/sources` (list-only; `GetCRD` lists and matches by name). Unknown kinds return an error listing the known kinds.
- Namespaced kinds: GameServer, Backup, BackupSchedule, Restore. An empty namespace on a namespaced kind loops over `/namespaces` for the selected cluster. For GameServer, a 403 on that namespace listing falls back to `/users/me/servers`, filtered to the namespace when one is given.
- Pods and events: `/pods`, `/servers/{name}/pods`, `/pods/{name}`, `/kube-events`, and `/servers/{name}/events`. The last returns rows that are converted to `corev1.Event`. With a `server` argument and no namespace, the server routes are called once without a namespace, so the API uses its default games namespace; pass `namespace` for a server in another managed namespace.
- Non-2xx responses become `*APIError{Status, Path, Message}`. Message is the body trimmed to 300 bytes, with the token redacted if echoed. 401 reads "not authenticated: the MCP token is missing, expired or revoked"; 403 and 404 prefix the message with "permission denied" and "not found"; other statuses use the generic form.
- Responses are capped at 32 MiB.
- Redirects are not followed. The client's `CheckRedirect` returns `http.ErrUseLastResponse`, so a 3xx comes back as an `*APIError`. The API never redirects these GETs, and following one could resend the bearer token to another scheme or path.
- Per-user pod logs are capped client-side to the newest 256 KiB, with the same truncation notice as break-glass mode, even when the API returns more.

### Transport

**MCP over stdio only**. The server uses `mcp.StdioTransport` and never opens a listening socket. The per-user client makes outbound HTTPS requests only. A client connects by spawning the process with stdin/stdout/stderr piped to the MCP JSON-RPC stream.

## Key invariants

The read-only guarantee rests on layered enforcement: the handler boundary (method sets), the authoritative backstop (RBAC in break-glass mode, the API's token guard in per-user mode), and test tripwires.

### 1. Handler boundary (`reader.Reader`)

**Source**: `mcp-server/internal/reader/reader.go`, `mcp-server/internal/kube/client.go`, `mcp-server/internal/panel/client.go`, `mcp-server/main.go` (package doc comment and `runServe`).

- Every tool handler (`tools.go`, `fixadvice.go`) takes a `reader.Reader`. Its method set is the six List/Get-shaped methods only.
- **Break-glass `kube.Client`** holds two unexported fields, `typed kubernetes.Interface` and `dynamic dynamic.Interface`, and its only exported methods are the six reader methods plus `New`, `NewFrom`, `WithNamespaces` and `ParseNamespaces`.
- **Per-user `panel.Client`** holds an HTTP client, base URL, token and session ID. Its only exported methods are the six reader methods plus `New`. Every request is built in one unexported helper, `get`, which uses `http.NewRequestWithContext(ctx, http.MethodGet, …)`.
- **What the boundary enforces**: a caller that holds only a `reader.Reader` cannot reach the Kubernetes clientsets or the panel's request builder, so no tool handler can call a mutating method through it.
- **What it does not enforce**: the boundary is a method-set convention, not a compile-time guarantee. `runServe` loads the `*rest.Config` itself (`ctrl.GetConfig()`). Layer 2 is what stops mutation.

### 2. Backstop

**Break-glass — RBAC (authoritative)**: **Source**: `charts/gameplane/templates/mcp-server.yaml`.

- Namespaced Roles in the release namespace and in each managed game namespace grant pods, events and `pods/log` reads. In game namespaces they also grant GameServer, Backup, BackupSchedule and Restore reads. A ClusterRole covers only GameTemplate, Module and ModuleSource.
- Every verb is `get`/`list`/`watch`, except `get` on `pods/log`. No create/update/patch/delete verb appears anywhere.
- The ServiceAccount (`gameplane-mcp-server`) is bound to these grants. Any write is rejected by the API server with 403 Forbidden, whatever code issues it.
- A standalone run against a kubeconfig has that kubeconfig user's permissions instead.

**Per-user — Gameplane API (authoritative)**: the process holds no Kubernetes permissions. The API applies the token user's roles, namespace grants and per-cluster grants. Its MCP-token guard accepts only `GET` on a fixed route list. The panel client's own `GET`-only rule is a defence in depth on top of that.

### 3. Break-glass namespace allow-list

**Source**: `mcp-server/internal/kube/client.go`.

- `WithNamespaces` sets an allow-list (trimmed, empties and duplicates dropped). `ParseNamespaces` splits the comma-separated environment value. An empty list means unrestricted, which is today's behaviour.
- When the list is set, an explicit namespace outside it returns `ErrNamespaceNotInScope` (wrapped with `%w`) before any API call. That applies to namespaced `ListCRD`/`GetCRD`, `ListPods`, `GetPod`, `ListEvents` and `PodLogs`.
- An empty namespace on `ListPods`, `ListEvents` and namespaced `ListCRD` loops over the allow-list in order and merges the items. A namespace that returns NotFound or Forbidden is skipped. If any were skipped, the merged list's resource version reads `skipped namespaces: <names>`. Other errors are returned wrapped.
- With an allow-list set, `GetPod` and `PodLogs` need an explicit namespace (`namespace is required`), because a get has no namespace to loop over.
- Cluster-scoped kinds are unaffected.
- Break-glass rejects a non-empty `cluster` target with `errClusterNotAvailable` on every method. The `server` target is ignored.

### 4. Per-user read rules

**Source**: `mcp-server/internal/panel/client.go`, `mcp-server/internal/panel/fanout.go`.

- Cross-cluster fan-out (`ListCRD`, `ListPods`, `ListEvents` with no `cluster` target): `/clusters` lists the clusters, and the same list runs for each at most 4 at a time, with a 10-second timeout per cluster. Items are annotated `gameplane.local/cluster`. A cluster that returns 403 or 404 is skipped silently. Any other failure marks the cluster unavailable, and the merged list's resource version reads `unavailable clusters: <ids>`. An error from `/clusters` itself is returned.
- Gets and logs never fan out. They use the selected cluster, or the local one when none is named.
- With an explicit `cluster`, a list is one request, with no `/clusters` call.

### 5. Test tripwires (visible guarantees)

- **`TestClientHasNoMutatingMethods`** (`main_test.go`): reflects over `kube.Client`'s method set and asserts no exported method name starts with Create, Update, Delete, Patch or Apply.
- **`TestPanelClientHasNoMutatingMethods`** (`panel_invariants_test.go`): the same check over `panel.Client`'s method set.
- **`TestReadersImplementInterface`** (`panel_invariants_test.go`): asserts both clients satisfy `reader.Reader`.
- **`internal/panel/source_test.go`**: reads the package's non-test `.go` files and fails if any contains an `http.Method` constant other than `http.MethodGet`, an `http.NewRequest(` call without `WithContext`, or a `.Post(`, `.PostForm(` or `.Do(` call outside the `get` helper.
- **`TestRegisteredToolsAreReadOnly`** (`main_test.go`): connects a real MCP client over in-memory transports; asserts:
  1. Every registered tool name does not contain a mutating verb (case-insensitive substring search).
  2. Every tool carries `Annotations.ReadOnlyHint = true`.
  3. No tool has `DestructiveHint = true`.
  4. The tool set matches `registeredToolNames` exactly (from `tools.go`).

These tests are **visible guarantees** (for auditing), not enforcement. The method sets keep handlers on read-only operations; RBAC (break-glass) and the API's token guard (per-user) are what stop mutation.

## Dependencies

From `go.mod`:

| Dependency | Version | Purpose |
|---|---|---|
| `github.com/modelcontextprotocol/go-sdk` | v1.8.0 | MCP protocol implementation (Server, tools, stdio transport) |
| `k8s.io/api` | v0.37.0 | Kubernetes API types (corev1) |
| `k8s.io/apimachinery` | v0.37.0 | Kubernetes meta types, label/field selectors, unstructured objects |
| `k8s.io/client-go` | v0.37.0 | Kubernetes typed clientset, dynamic client, rest.Config |
| `sigs.k8s.io/controller-runtime` | v0.25.1 | `ctrl.GetConfig()` for kubeconfig loading |

The per-user client uses only the standard library (`net/http`, `crypto/tls`, `crypto/x509`).

**Why these versions?** Kept in step with the Gameplane operator's Kubernetes dependencies; Dependabot in this repository proposes updates.

## Security considerations

1. **Break-glass blast radius**: bounded by the chart's namespaced Roles (release namespace and managed game namespaces) and the ClusterRole for cluster-scoped kinds only. Setting `GAMEPLANE_MCP_NAMESPACES` further narrows reads in the process. A standalone run with no allow-list uses the kubeconfig's own permissions. Pod logs in any namespace the process can read may contain application secrets (API keys, connection strings); this is an accepted tradeoff on an opt-in, admin-only component.
2. **Per-user exposure**: the process holds a bearer MCP token (`gpmcp_…`) that carries the user's read access. Anyone with the token can read what that user can. The token is read from the environment only, never logged, and never echoed in errors. Revoke or expire it in the dashboard if it leaks.
3. **Transport security**: the panel URL must be `https`, except for a loopback host. A private CA is supported with `--panel-ca`. TLS minimum is 1.2.
4. **No listening port**: No network surface to expose. Reachable only via `kubectl exec` (break-glass) or the MCP host's spawned process (per-user).
5. **Deployment model**: Opt-in via `mcpServer.enabled=true` in the Helm chart (break-glass). Per-user mode runs wherever the operator chooses, with a token. Admin responsibility to decide if each is acceptable for their cluster.
6. **See [`docs/security.md`](https://github.com/GameplanePanel/Gameplane/blob/master/docs/security.md)** for the full threat model and per-cluster decisions (especially if cohabiting with unrelated sensitive workloads).
7. **GameServer security fields**: the generic get and list tools return `spec.security` and `status.security` unredacted, because the only transform is `metadata.managedFields` stripping; `security_roundtrip_test.go` asserts this.

## Testing & coverage

### Coverage gate

- **Threshold**: 70% (from `.testcoverage.yml`).
- **Measured against**: package-level aggregate (not per-file).
- **What's covered**: tool registration, read-only invariant tripwires, CRD/Pod/Event/log helpers against fake clientsets (break-glass), namespace allow-list behaviour, the per-user HTTP client against `httptest` servers, cross-cluster fan-out, propose_fix heuristic advice text.
- **Uncovered**: main()/run() process wiring (signal handling, stdio transport) — not unit-testable.

### Test files

- `main_test.go` — tool registration, registry, read-only tripwires, idle/serve wiring.
- `panel_invariants_test.go` — panel method-set tripwire and interface assertions.
- `internal/kube/allowlist_test.go` — out-of-scope namespace per method returns `ErrNamespaceNotInScope`; empty namespace merges only allowed namespaces; `Target.Cluster` set returns `errClusterNotAvailable`; `ParseNamespaces` normalises input; no allow-list lists across all namespaces.
- `internal/kube/client_test.go`, `log_tail_test.go` — break-glass client against fake clientsets; log tail helpers.
- `internal/panel/client_test.go` — each reader method's path and query; bearer and session headers; GET only; 401/403/404 messages; token absent from errors; server event conversion; GameServer 403 fallback to `/users/me/servers`; `New` URL and token validation; log tail clamp and system-logs routing; server-required error.
- `internal/panel/fanout_test.go` — two clusters merged and annotated; a 500 cluster listed as unavailable; a 403 cluster skipped silently; explicit `cluster` gives a single request with no `/clusters` call.
- `internal/panel/source_test.go` — source-level GET-only tripwire.

### Test commands

Run from this repository's root (CI runs the same through Gameplane's shared `go-component.yaml` workflow):

```sh
go test -race ./...
```

### Key test cases

- `TestClientHasNoMutatingMethods` / `TestPanelClientHasNoMutatingMethods` — lint tripwires on both clients' exported method sets.
- `TestRegisteredToolsAreReadOnly` — over-the-wire check: tool names, ReadOnlyHint, exact registry.
- `TestToolsListAndGetHappyPath` — happy-path tests for list/get tools using fake clientsets.
- `TestProposeFixTool` — propose_fix heuristic matching and fallback advice.
- `TestRunIdleReturnsOnCancel` — idle subcommand cancellation.

## References

- **[`README.md`](README.md)** — run instructions, two modes, RBAC scope, transport details, tool descriptions.
- **`charts/gameplane/templates/mcp-server.yaml`** — Helm template (ServiceAccount, namespaced Roles, ClusterRole, Deployment).
- **[`docs/security.md`](https://github.com/GameplanePanel/Gameplane/blob/master/docs/security.md)** — threat model, mcp-server section, admin decision guidance.
- **Model Context Protocol spec** — https://modelcontextprotocol.io/
