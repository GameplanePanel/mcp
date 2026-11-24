# mcp-server

> This repository is checked out as the `mcp-server/` submodule of [GameplanePanel/Gameplane](https://github.com/GameplanePanel/Gameplane). Paths such as `docs/`, `operator/`, `api/`, `charts/` and `web/` below refer to that repository.

The **mcp-server** [optional] is a strictly **read-only** [Model Context Protocol](https://modelcontextprotocol.io/)
(MCP) server for Gameplane. It lets an AI assistant read state — 7 of
Gameplane's 9 CRDs (not Cluster or NetworkCapture), Pods, Events, and pod
logs — and get a suggested fix as plain text (YAML and/or `kubectl` commands)
for a human operator to review and run.

This repository is checked out as the `mcp-server/` submodule of
[GameplanePanel/Gameplane](https://github.com/GameplanePanel/Gameplane), which
builds and ships the image and Helm chart (`mcpServer.enabled`). Lint, tests,
coverage and govulncheck for this module run in this repository's own CI.

## Two modes

The server runs in one of two modes, chosen by `serve` at start-up. Both use
the same tools and the same read-only interface.

### Per-user mode (recommended)

Set `GAMEPLANE_PANEL_URL` and the server reads through the Gameplane API as
the user who owns the token. Results follow that user's roles, namespace
grants and per-cluster grants, across every cluster the user may read. The
process needs no Kubernetes credentials.

1. In the Gameplane dashboard, open **Settings → MCP tokens** and create a
   token. Tokens start with `gpmcp_`.
2. Start the server:

   ```sh
   GAMEPLANE_PANEL_URL=https://<panel> GAMEPLANE_MCP_TOKEN=gpmcp_… mcp-server serve
   ```

   Or with the image:

   ```sh
   docker run --rm -i -e GAMEPLANE_PANEL_URL -e GAMEPLANE_MCP_TOKEN \
     ghcr.io/gameplanepanel/gameplane/mcp-server:edge serve
   ```

3. Point your MCP client at it. For example:

   ```json
   {
     "mcpServers": {
       "gameplane": {
         "command": "mcp-server",
         "args": ["serve"],
         "env": {
           "GAMEPLANE_PANEL_URL": "https://panel.example.com",
           "GAMEPLANE_MCP_TOKEN": "gpmcp_…"
         }
       }
     }
   }
   ```

   The Docker form runs the image instead: set `"command": "docker"` and
   `"args": ["run", "--rm", "-i", "-e", "GAMEPLANE_PANEL_URL", "-e", "GAMEPLANE_MCP_TOKEN", "ghcr.io/gameplanepanel/gameplane/mcp-server:edge", "serve"]`,
   keeping the same `env` block.

Options for `serve` (each also has an environment variable):

- `--panel-url` (`GAMEPLANE_PANEL_URL`) — the Gameplane API base URL. Setting
  it selects per-user mode. The URL must use `https`; `http` is accepted only
  for `localhost` or a loopback address.
- `--panel-ca` (`GAMEPLANE_PANEL_CA`) — a PEM CA bundle added to the system
  roots, for a panel served with a private certificate authority.
- `GAMEPLANE_MCP_TOKEN` — the MCP token. Read from the environment only (there
  is no flag). Required when `GAMEPLANE_PANEL_URL` is set; it must start with
  `gpmcp_`. The token is never written to logs or error messages. A token without `GAMEPLANE_PANEL_URL` is a start-up error: `serve` refuses to start rather than fall back to break-glass mode.

Every request the server makes is a `GET`. A token with a revoked or expired
status gets a "not authenticated" error on the next call.

### Break-glass mode (admin only)

Leave `GAMEPLANE_PANEL_URL` unset and `serve` reads the cluster directly with
the Kubernetes credentials of the process (in-cluster ServiceAccount, or the
kubeconfig). It is for administrators debugging the cluster, and it works
while the Gameplane API is down.

- There is no Gameplane user check. Anyone who can `kubectl exec` into the
  `gameplane-mcp-server` pod can use it.
- It reads the **local cluster** only. Passing a `cluster` argument is an
  error in this mode.
- Reads are limited to an allow-list of namespaces when `GAMEPLANE_MCP_NAMESPACES`
  is set (a comma-separated list): in the chart, the release namespace plus the
  managed game namespaces. Unset means unrestricted, and the process's own
  Kubernetes permissions apply.

```sh
kubectl exec -i deploy/gameplane-mcp-server -n gameplane-system -- /mcp-server serve
```

## Security model

It **never** creates, updates, patches, deletes, or applies anything. That
is a hard invariant, held by two layers:

1. **The handler boundary**: every MCP tool handler (`tools.go`,
   `fixadvice.go`) lives in `package main` and takes a `reader.Reader`
   (`internal/reader`). Both implementations expose only List/Get-shaped
   methods (`ListCRD`, `GetCRD`, `ListPods`, `GetPod`, `ListEvents`,
   `PodLogs`):
   - the break-glass `*kube.Client` (`internal/kube`), whose typed and dynamic
     clientsets are **unexported fields**;
   - the per-user `*panel.Client` (`internal/panel`), which builds every
     request in one helper that issues only `GET`.

   A handler that holds a `reader.Reader` therefore cannot call
   `Create`/`Update`/`Delete`/`Patch`/`Apply` through it. The boundary is a
   code convention enforced by the method sets, not a compile-time guarantee:
   `runServe` loads the `*rest.Config` itself. Tripwires are
   `main_test.go`'s `TestClientHasNoMutatingMethods`, `panel_invariants_test.go`'s
   `TestPanelClientHasNoMutatingMethods`, and `internal/panel/source_test.go`,
   which fails if the panel package contains any HTTP method other than GET.
2. **By RBAC or by the API**:
   - **Break-glass**: the Helm chart's `mcpServer.enabled` ServiceAccount holds
     only `get`/`list`/`watch` (plus `get` on `pods/log`). Those grants are
     namespaced Roles in the release namespace and in each managed game
     namespace, plus a ClusterRole for the cluster-scoped kinds. See
     [RBAC scope](#rbac-scope). There is no `create`/`update`/`patch`/`delete`
     verb anywhere in them. This is the **authoritative** layer for break-glass.
   - **Per-user**: the process holds no Kubernetes permissions. The Gameplane
     API applies the user's roles, and it accepts only `GET` from an MCP token.

   In both modes the API server or the Gameplane API rejects a write, whatever
   client makes it. A standalone run against a kubeconfig has that kubeconfig
   user's permissions instead, so point it at a read-only identity.

A third, cosmetic layer: every tool this server installs (`tools.go`)
carries the MCP spec's `readOnlyHint: true` annotation, and `main_test.go`'s
`TestRegisteredToolsAreReadOnly` asserts the registered tool set never
contains a mutating verb — this makes the guarantee visible to MCP clients,
it doesn't enforce it.

## Tools

| Tool | Reads |
|---|---|
| `list_gameplane_resources` | One Gameplane CRD kind (GameServer, GameTemplate, Backup, BackupSchedule, Restore, Module, ModuleSource), optionally scoped to a namespace and/or label selector |
| `get_gameplane_resource` | A single Gameplane CRD object by kind/namespace/name |
| `list_pods` | Core Pods in a namespace (or all namespaces), optionally by label selector |
| `get_pod` | A single Pod's spec and status |
| `list_events` | Core Events in a namespace (or all namespaces), optionally by field/label selector. Bounded to the most-recent 100 events (sorted newest-first); responses include a truncation notice if more exist. |
| `get_pod_logs` | A bounded tail (default 200 lines, capped at 5000) of a container's logs; retains the newest 256 KiB from the complete response, with a notice when truncated |
| `propose_fix` | Given a resource reference + a free-text symptom, returns suggested YAML/kubectl text, grounded in a best-effort read of the resource's current status. Never applies anything itself. |

Every tool except `propose_fix` also takes these optional arguments:

- `cluster` — a registered cluster ID. **Per-user mode only**; break-glass
  mode returns an error for any value. With it empty, list tools cover every
  cluster the user may read, and each returned item is annotated
  `gameplane.local/cluster`. Get tools and log reads use the local cluster
  when `cluster` is empty.
- `server` — a GameServer name. **Per-user mode only**; it is ignored in
  break-glass mode. It is accepted by `list_pods`, `list_events` and
  `get_pod_logs`. In per-user mode it routes pod, event and log reads through
  that server (`/servers/{name}/…`). `get_pod_logs` requires it for game
  server pods. Without it, logs are readable only for the API and operator
  system pods.

`propose_fix` accepts `cluster` but not `server`. Its reads follow the same
rules as the get tools.

In per-user mode, a cluster that fails for any reason other than 403 or 404
during a list is reported in the response: its `metadata.resourceVersion`
(or the list's `resourceVersion`) reads `unavailable clusters: <ids>`. Clusters
that return 403 or 404 are skipped silently.

Example: `get_gameplane_resource` with `{"kind": "GameServer", "namespace": "games", "name": "my-server"}` returns the object as stored, including `spec.security` (the per-server uid/gid overrides) and `status.security` (the desired and applied security read model). Only `metadata.managedFields` is removed; no security field is redacted.

## Transport: stdio only, no network port

This server speaks MCP (JSON-RPC 2.0) over stdio — it never opens an HTTP
or TCP port. It has two subcommands:

- `idle` (default, no args) — block until terminated. This is what the
  bundled Helm Deployment runs as its long-lived container process, purely
  so there is always a live container to `kubectl exec` into.
- `serve` — run one MCP stdio session. Flags and environment are listed under
  [Two modes](#two-modes). Each session is independent, so concurrent sessions
  from different users or hosts don't interfere with each other or with the
  idle placeholder process. Diagnostics go to stderr; stdout is the MCP stream.

## Run via the Helm chart

Set `mcpServer.enabled=true`. The chart deploys a Deployment (no Service —
there is no network port to expose) and a dedicated ServiceAccount. It runs
break-glass mode: `serve` reads the local cluster with that ServiceAccount.
The chart does not grant cluster-wide reads of pods, events or pod logs. See
[RBAC scope](#rbac-scope).

```yaml
mcpServer:
  enabled: true
  replicas: 1
  resources:
    requests: { cpu: 50m, memory: 32Mi }
    limits:   { cpu: 200m, memory: 128Mi }
```

## RBAC scope

- **Per-user mode** needs no Kubernetes permissions. The API applies the
  token's user, namespace grants and per-cluster grants.
- **Break-glass mode** uses the chart's read-only grants. These are namespaced
  Roles in the release namespace and in each managed game namespace. Those
  Roles cover pods, events and `pods/log`, and in game namespaces also
  GameServer, Backup, BackupSchedule and Restore. A ClusterRole is granted only
  for the cluster-scoped kinds: GameTemplate, Module and ModuleSource. Every
  grant is `get`/`list`/`watch`, except `get` on `pods/log`.

## Run standalone (against a kubeconfig)

```sh
KUBECONFIG=~/.kube/config docker run --rm -i \
  --user "$(id -u):$(id -g)" --network host \
  -v ~/.kube/config:/kubeconfig:ro -e KUBECONFIG=/kubeconfig \
  ghcr.io/gameplanepanel/gameplane/mcp-server:edge serve
```

This runs break-glass mode with `GAMEPLANE_MCP_NAMESPACES` unset, so reads are
unrestricted and the kubeconfig's own permissions apply.

`serve` builds its Kubernetes client via `ctrl.GetConfig()`, so it works
in-cluster (via the pod's ServiceAccount) or locally against `KUBECONFIG`/
`~/.kube/config`, same as any other controller-runtime-based Gameplane
component. Two flags matter for the standalone case above:

- `--user "$(id -u):$(id -g)"` — the image runs as a fixed non-root UID
  (65532), which normally can't read a kubeconfig with the usual `0600`
  permissions owned by your own user. Running the container as your own
  UID/GID lets it read the mounted file without loosening its permissions
  or copying it elsewhere.
- `--network host` — only needed when the kubeconfig's server address is a
  loopback address (e.g. a local kind/k3d cluster at `https://127.0.0.1:<port>`).
  Without it, `127.0.0.1` inside the container resolves to the container
  itself, not the host, and every tool call fails with a connection error.
  It's not needed against a real (non-loopback) API server address.

## Response size optimization

Every tool that returns Kubernetes objects strips `metadata.managedFields`
from the response to reduce payload size (~33% of unstripped responses). This
field carries an audit trail of every manager that has written to the object;
it is rarely needed for debugging and wastes LLM context tokens.

`list_events` additionally bounds responses to the most-recent 100 events
(sorted by `lastTimestamp`, falling back to `eventTime`, newest first). When more events exist in the
cluster than can fit in 100, the response's `metadata.resourceVersion` field
is set to a truncation notice (e.g. `"truncated (showing newest 100 of 342
events)"`). This prevents large event logs from exhausting the LLM context.
