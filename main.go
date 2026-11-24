// Command mcp-server is a strictly read-only Model Context Protocol (MCP)
// server for Gameplane clusters: it lets an AI assistant read cluster state
// (7 of Gameplane's 9 CRDs — not Cluster or NetworkCapture — plus Pods,
// Events, pod logs) and propose fixes as plain text — SUGGESTED YAML or
// kubectl commands for a human operator to review
// and run. It never creates, updates, patches, deletes, or applies anything.
//
// That is a hard invariant, held by two layers:
//  1. The handler boundary: every tool handler in this package (tools.go,
//     fixadvice.go) takes a reader.Reader (internal/reader). Its two
//     implementations expose only List/Get-shaped methods (ListCRD, GetCRD,
//     ListPods, GetPod, ListEvents, PodLogs): the break-glass *kube.Client
//     (internal/kube) and the per-user *panel.Client (internal/panel), which
//     issues only GET requests against the Gameplane API. A handler that
//     holds a reader.Reader cannot call Create/Update/Delete/Patch/Apply
//     through it. This is a code convention enforced by the method sets,
//     not a compile-time guarantee: runServe loads the *rest.Config itself.
//     main_test.go's TestClientHasNoMutatingMethods and
//     panel_invariants_test.go's TestPanelClientHasNoMutatingMethods are
//     tripwires on the two exported method sets.
//  2. By RBAC or by the API: in break-glass mode the Helm chart's
//     mcp-server ServiceAccount holds only get/list/watch (plus get on
//     pods/log), as namespaced Roles in the release namespace and each managed
//     game namespace plus a ClusterRole for the cluster-scoped kinds — see
//     charts/gameplane/templates/mcp-server.yaml. In per-user mode the process
//     holds no Kubernetes permissions at all: the Gameplane API applies the
//     token's user, namespace and per-cluster grants, and accepts only GET
//     from the MCP token. The API server (or the Gameplane API) rejects a
//     write whatever client makes it.
//
// A third, cosmetic layer: every tool this server installs (tools.go)
// carries ReadOnlyHint: true, and main_test.go's
// TestRegisteredToolsAreReadOnly asserts the registered tool set never
// contains a mutating verb — this makes the guarantee visible to MCP
// clients, it doesn't enforce it.
//
// It speaks MCP (JSON-RPC 2.0) over stdio only — never a network port — and
// is deliberately standalone (own module, like audit-syslog-bridge and
// telemetry-receiver) so it can run anywhere: in-cluster via the Helm
// chart's mcpServer.enabled, or locally against a kubeconfig for a dev
// cluster, or on a laptop against a Gameplane API in per-user mode.
//
// Two subcommands:
//
//	idle   (default, no args) — block until terminated. This is what the
//	       Helm Deployment runs as its long-lived container process, purely
//	       so there is always a live container to `kubectl exec` into.
//	serve  — run the actual MCP stdio session for one client. Flags:
//	         --panel-url  (env GAMEPLANE_PANEL_URL) Gameplane API base URL.
//	                      Set: per-user mode, token from GAMEPLANE_MCP_TOKEN.
//	                      Unset: break-glass mode against the kubeconfig.
//	         --panel-ca   (env GAMEPLANE_PANEL_CA) PEM CA bundle for a private
//	                      panel certificate (per-user mode).
//	       Break-glass mode also reads GAMEPLANE_MCP_NAMESPACES, a
//	       comma-separated namespace allow-list; unset means unrestricted.
//	       Point an MCP host's launcher at, for example:
//	         kubectl exec -i deploy/gameplane-mcp-server -n <ns> -- /mcp-server serve
//	       Each exec spawns an independent, isolated session sharing the
//	       pod's ServiceAccount credentials and network access — concurrent
//	       sessions don't interfere with each other or with the idle
//	       placeholder process.
//
// Stdout is the MCP stream, so every log line goes to stderr.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GameplanePanel/gameplane/mcp-server/internal/kube"
	"github.com/GameplanePanel/gameplane/mcp-server/internal/panel"
	"github.com/GameplanePanel/gameplane/mcp-server/internal/reader"
)

// Version is set at build time via -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	// stdout carries the MCP stream; diagnostics must go to stderr.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := "idle"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	var err error
	switch cmd {
	case "idle":
		runIdle(ctx)
	case "serve":
		err = runServe(ctx, os.Args[2:])
	default:
		slog.Error("unknown subcommand", "name", cmd)
		os.Exit(2)
	}
	if err != nil {
		slog.Error("mcp-server exited", "cmd", cmd, "err", err)
		os.Exit(1)
	}
}

// runIdle blocks until ctx is cancelled. It performs no cluster access and
// opens no client, so the Deployment's steady-state replica carries no
// standing credentials use beyond existing — real reads happen only inside
// a `serve` session.
func runIdle(ctx context.Context) {
	slog.Info("mcp-server idle: waiting for `kubectl exec -i ... -- /mcp-server serve` sessions", "version", Version)
	<-ctx.Done()
	slog.Info("mcp-server idle: shutting down")
}

// runServe picks a reader for the session (per-user panel client when a panel
// URL is set, otherwise break-glass kube client), then runs one MCP session
// over stdio until the client disconnects or ctx is cancelled.
func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	panelURL := fs.String("panel-url", os.Getenv("GAMEPLANE_PANEL_URL"), "Gameplane API base URL; when set, serve in per-user mode")
	panelCA := fs.String("panel-ca", os.Getenv("GAMEPLANE_PANEL_CA"), "PEM CA bundle for a private Gameplane API certificate (per-user mode)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse serve flags: %w", err)
	}

	var rd reader.Reader
	if *panelURL == "" && os.Getenv("GAMEPLANE_MCP_TOKEN") != "" {
		return errors.New("GAMEPLANE_MCP_TOKEN is set but GAMEPLANE_PANEL_URL is not; refusing to fall back to break-glass mode")
	}

	if *panelURL != "" {
		token := os.Getenv("GAMEPLANE_MCP_TOKEN")
		if token == "" {
			return errors.New("GAMEPLANE_MCP_TOKEN is required when GAMEPLANE_PANEL_URL is set")
		}
		pc, err := panel.New(*panelURL, token, *panelCA)
		if err != nil {
			return fmt.Errorf("build panel client: %w", err)
		}
		slog.Info("mcp-server serve: per-user mode", "panel", redactURL(*panelURL))
		rd = pc
	} else {
		cfg, err := ctrl.GetConfig()
		if err != nil {
			return fmt.Errorf("load kubeconfig: %w", err)
		}
		client, err := kube.New(cfg)
		if err != nil {
			return fmt.Errorf("build kubernetes client: %w", err)
		}
		nss := kube.ParseNamespaces(os.Getenv("GAMEPLANE_MCP_NAMESPACES"))
		client.WithNamespaces(nss)
		scope := "unrestricted"
		if len(nss) > 0 {
			scope = strings.Join(nss, ",")
		}
		slog.Info("mcp-server serve: break-glass mode", "namespaces", scope)
		rd = client
	}

	server := newMCPServer(rd)
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("mcp session: %w", err)
	}
	return nil
}

// redactURL returns raw without its query, fragment or user info, for logging.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid URL>"
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.User = nil
	return u.String()
}

// newMCPServer builds the MCP server and installs every read-only tool
// against c.
func newMCPServer(c reader.Reader) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "gameplane-mcp-server",
		Version: Version,
	}, &mcp.ServerOptions{
		Instructions: "Strictly read-only access to a Gameplane cluster: list/get 7 of " +
			"Gameplane's 9 CRDs (not Cluster or NetworkCapture), Pods, Events, and pod " +
			"logs, and get suggested fixes as text via propose_fix. No tool here ever " +
			"creates, updates, patches, deletes, or applies anything. In per-user mode " +
			"reads go through the Gameplane API as the token's user, across the clusters " +
			"that user may read (pass cluster to pick one; list results are annotated " +
			"gameplane.local/cluster). In break-glass mode reads go to the local " +
			"cluster's Gameplane namespaces only.",
	})
	registerTools(server, c)
	return server
}
