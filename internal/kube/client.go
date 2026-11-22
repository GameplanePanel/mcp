// Package kube is the strictly read-only Kubernetes access layer for the
// Gameplane MCP server's break-glass mode. Client exposes only List/Get-shaped
// methods (ListCRD, GetCRD, ListPods, GetPod, ListEvents, PodLogs); the typed
// and dynamic clientsets backing those methods are unexported fields on Client.
// A caller that holds only a *Client therefore cannot reach a mutating verb
// (Create/Update/Delete/Patch/Apply) through it. Client satisfies
// reader.Reader, so every MCP tool handler in package main receives it as a
// reader.Reader; the per-user panel client is the other implementation.
//
// Break-glass Client reads the local cluster with the kubeconfig's own
// credentials. WithNamespaces optionally restricts it to an allow-list of
// namespaces (GAMEPLANE_MCP_NAMESPACES); cluster selection is rejected because
// only per-user mode can reach other clusters.
//
// See main.go's package doc comment (in the parent module) for the other
// layer: the namespaced Roles and get/list/watch-only ClusterRole the Helm
// chart installs, which is the only layer that stops mutation by any code in
// the process.
package kube

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	"github.com/GameplanePanel/gameplane/mcp-server/internal/reader"
)

// gameplaneGroupVersion is the Gameplane CRD API group, mirroring
// operator/api/v1alpha1.GroupVersion. Redeclared here (rather than imported)
// so this module stays standalone, like its audit-syslog-bridge and
// telemetry-receiver siblings: no dependency on the operator module's Go
// types, no coupling to their release cadence.
var gameplaneGroupVersion = schema.GroupVersion{Group: "gameplane.local", Version: "v1alpha1"}

// errUnknownKind is returned when a tool is asked to operate on a kind name
// outside the fixed set of Gameplane CRDs this server knows how to read.
var errUnknownKind = errors.New("unknown Gameplane resource kind")

// errClusterNotAvailable is returned when a call names a cluster. Break-glass
// mode reads only the local cluster; cluster selection needs per-user mode.
var errClusterNotAvailable = errors.New("cluster selection needs per-user mode (GAMEPLANE_PANEL_URL); break-glass mode reads only the local cluster")

// ErrNamespaceNotInScope is returned when a call names a namespace outside the
// allow-list set with WithNamespaces.
var ErrNamespaceNotInScope = errors.New("namespace not in scope")

// errNamespaceRequired is returned when an allow-listed client is asked for a
// namespaced object without naming the namespace.
var errNamespaceRequired = errors.New("namespace is required when the namespace allow-list is set")

// CRDKind describes one of the 7 Gameplane CRDs (of 9 total; Cluster and
// NetworkCapture are not exposed) this server can list/get.
// Exported (along with CRDKinds below) so callers outside this package —
// e.g. this module's own tests, which need to build a matching fake
// dynamic client — can enumerate the registry without this package growing
// a test-only export.
type CRDKind struct {
	GVR        schema.GroupVersionResource
	Namespaced bool
}

// CRDKinds is the fixed, read-only registry of Gameplane CRDs. Keyed by the
// CamelCase Kind name a caller passes in (e.g. "GameServer"), so tool
// arguments read the same as `kubectl get <kind>`. Scope (namespaced vs
// cluster-scoped) mirrors the +kubebuilder:resource:scope markers in
// operator/api/v1alpha1/*_types.go and the RBAC comments in
// charts/gameplane/templates/mcp-server.yaml.
var CRDKinds = map[string]CRDKind{
	"GameServer":     {GVR: gameplaneGroupVersion.WithResource("gameservers"), Namespaced: true},
	"GameTemplate":   {GVR: gameplaneGroupVersion.WithResource("gametemplates"), Namespaced: false},
	"Backup":         {GVR: gameplaneGroupVersion.WithResource("backups"), Namespaced: true},
	"BackupSchedule": {GVR: gameplaneGroupVersion.WithResource("backupschedules"), Namespaced: true},
	"Restore":        {GVR: gameplaneGroupVersion.WithResource("restores"), Namespaced: true},
	"Module":         {GVR: gameplaneGroupVersion.WithResource("modules"), Namespaced: false},
	"ModuleSource":   {GVR: gameplaneGroupVersion.WithResource("modulesources"), Namespaced: false},
}

// maxLogBytes bounds a single pod-logs response so a runaway container can't
// stream an unbounded amount of text back through the tool result.
const maxLogBytes = 256 << 10

// truncatedLogNotice prefixes a PodLogs result when the requested tail
// exceeded maxLogBytes and was trimmed to its newest bytes, so a caller
// (and propose_fix's CrashLoop advice) knows the output was cut and which
// end was kept (F-204).
const truncatedLogNotice = "[gameplane-mcp: log output truncated to the newest 256 KiB]\n"

// defaultTailLines is applied when a get_pod_logs call doesn't specify one.
const defaultTailLines = 200

// maxTailLines caps an explicit request so a caller can't ask for the entire
// log history of a long-lived pod.
const maxTailLines = 5000

// NewScheme builds a runtime.Scheme that knows the corev1 types (for the
// typed Pod/Event reads) plus the 7 exposed Gameplane CRD kinds, registered against
// unstructured.Unstructured/UnstructuredList. Client's methods use the
// dynamic client directly and don't strictly need a scheme to do so, but
// building one here keeps the process's view of "what a Gameplane resource
// is" explicit and gives future typed-decode paths (or fake-client tests) a
// scheme to work against, without importing operator/api/v1alpha1's
// generated types (see the comment on gameplaneGroupVersion).
func NewScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	// Best-effort: corev1 registration into client-go's own scheme package
	// cannot fail for a fixed set of built-in types.
	_ = clientgoscheme.AddToScheme(scheme)

	for kind := range CRDKinds {
		gvk := gameplaneGroupVersion.WithKind(kind)
		if !scheme.Recognizes(gvk) {
			scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		}
		listGVK := gameplaneGroupVersion.WithKind(kind + "List")
		if !scheme.Recognizes(listGVK) {
			scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
		}
	}
	metav1.AddToGroupVersion(scheme, gameplaneGroupVersion)
	return scheme
}

// Client is a strictly read-only view of a Kubernetes cluster: it exposes
// List/Get-shaped methods only, and the clientsets that could do more
// (typed, dynamic) are unexported — no code outside this package can reach
// them, so no code outside this package can call Create/Update/Delete/
// Patch/Apply through this type, structurally, regardless of what tools get
// registered against it. See the package doc comment above for how this
// combines with the RBAC Roles and ClusterRole to form the full read-only
// guarantee.
type Client struct {
	typed      kubernetes.Interface
	dynamic    dynamic.Interface
	Scheme     *runtime.Scheme
	namespaces []string // allow-list; empty = unrestricted
}

// Client is the break-glass implementation of reader.Reader.
var _ reader.Reader = (*Client)(nil)

// New builds a Client from a rest.Config, e.g. the one returned by
// ctrl.GetConfig() (in-cluster config, falling back to KUBECONFIG/
// ~/.kube/config for local runs against a dev cluster).
func New(cfg *rest.Config) (*Client, error) {
	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build typed clientset: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build dynamic client: %w", err)
	}
	return NewFrom(typed, dyn, NewScheme()), nil
}

// NewFrom wraps an already-constructed typed clientset and dynamic client in
// a read-only Client. Production code should prefer New; this constructor
// exists so tests (in this package and others, e.g. the MCP tool-handler
// tests in ../../main_test.go) can inject client-go fakes without this
// package needing a test-only export. Wrapping a full-capability clientset
// here doesn't weaken the read-only guarantee: the returned Client still
// only exposes ListCRD/GetCRD/ListPods/GetPod/ListEvents/PodLogs, so a
// caller holding only the *Client still cannot mutate anything through it.
func NewFrom(typed kubernetes.Interface, dyn dynamic.Interface, scheme *runtime.Scheme) *Client {
	return &Client{typed: typed, dynamic: dyn, Scheme: scheme}
}

// WithNamespaces restricts the client to an allow-list of namespaces. Names
// are trimmed; empties and duplicates are dropped. An empty list leaves the
// client unrestricted. It returns c for chaining.
func (c *Client) WithNamespaces(ns []string) *Client {
	c.namespaces = dedupeNamespaces(ns)
	return c
}

// ParseNamespaces splits a comma-separated namespace list (e.g. the value of
// GAMEPLANE_MCP_NAMESPACES) into trimmed, non-empty, de-duplicated names in
// their original order.
func ParseNamespaces(s string) []string {
	return dedupeNamespaces(strings.Split(s, ","))
}

func dedupeNamespaces(in []string) []string {
	var out []string
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		ns := strings.TrimSpace(raw)
		if ns == "" {
			continue
		}
		if _, dup := seen[ns]; dup {
			continue
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	return out
}

// checkTarget rejects a cluster selection, which break-glass mode cannot honour.
// Server is ignored here.
func checkTarget(ctx context.Context) error {
	if reader.TargetFrom(ctx).Cluster != "" {
		return errClusterNotAvailable
	}
	return nil
}

// checkNamespace enforces the allow-list on an explicitly named namespace.
// An empty namespace is not checked here; callers handle it.
func (c *Client) checkNamespace(ns string) error {
	if ns == "" || len(c.namespaces) == 0 || slices.Contains(c.namespaces, ns) {
		return nil
	}
	return fmt.Errorf("namespace %q not in scope: %w", ns, ErrNamespaceNotInScope)
}

// requireNamespace rejects an empty namespace for a single-object read when
// the allow-list is set, since there is no safe namespace to fall back to.
func (c *Client) requireNamespace(ns string) error {
	if ns == "" && len(c.namespaces) > 0 {
		return errNamespaceRequired
	}
	return nil
}

// skippable reports whether a per-namespace list error means the namespace is
// absent or not visible to this client, so the merged list skips it.
func skippable(err error) bool {
	return apierrors.IsNotFound(err) || apierrors.IsForbidden(err)
}

// skippedNote is the resource-version annotation that records skipped
// namespaces on a merged list.
func skippedNote(skipped []string) string {
	return "skipped namespaces: " + strings.Join(skipped, ", ")
}

func kindOf(kind string) (CRDKind, error) {
	k, ok := CRDKinds[kind]
	if !ok {
		return CRDKind{}, fmt.Errorf("%w: %q (known: %s)", errUnknownKind, kind, knownKindsList())
	}
	return k, nil
}

// resourceInterface returns the dynamic client scoped to k's GVR (and to
// namespace, when k is namespaced and namespace is non-empty).
func (c *Client) resourceInterface(k CRDKind, namespace string) dynamic.ResourceInterface {
	ri := c.dynamic.Resource(k.GVR)
	if k.Namespaced && namespace != "" {
		return ri.Namespace(namespace)
	}
	return ri
}

func knownKindsList() string {
	out := ""
	for kind := range CRDKinds {
		if out != "" {
			out += ", "
		}
		out += kind
	}
	return out
}

// ListCRD lists objects of a Gameplane CRD kind. namespace is ignored for
// cluster-scoped kinds; an empty namespace on a namespaced kind lists across
// all namespaces (or, with an allow-list set, across the allowed namespaces).
// labelSelector is passed through verbatim to the API server.
func (c *Client) ListCRD(ctx context.Context, kind, namespace, labelSelector string) (*unstructured.UnstructuredList, error) {
	if err := checkTarget(ctx); err != nil {
		return nil, err
	}
	k, err := kindOf(kind)
	if err != nil {
		return nil, err
	}
	if k.Namespaced {
		if err := c.checkNamespace(namespace); err != nil {
			return nil, err
		}
		if namespace == "" && len(c.namespaces) > 0 {
			return c.listCRDAcross(ctx, kind, k, labelSelector)
		}
	}
	list, err := c.resourceInterface(k, namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", kind, err)
	}
	return list, nil
}

// listCRDAcross lists kind in each allow-listed namespace, in order, and
// merges the items. Namespaces that are absent or forbidden are skipped and
// recorded on the merged list's resource version.
func (c *Client) listCRDAcross(ctx context.Context, kind string, k CRDKind, labelSelector string) (*unstructured.UnstructuredList, error) {
	merged := &unstructured.UnstructuredList{}
	merged.SetGroupVersionKind(gameplaneGroupVersion.WithKind(kind + "List"))
	var skipped []string
	for _, ns := range c.namespaces {
		list, err := c.dynamic.Resource(k.GVR).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			if skippable(err) {
				skipped = append(skipped, ns)
				continue
			}
			return nil, fmt.Errorf("list %s in %q: %w", kind, ns, err)
		}
		merged.Items = append(merged.Items, list.Items...)
	}
	if len(skipped) > 0 {
		merged.SetResourceVersion(skippedNote(skipped))
	}
	return merged, nil
}

// GetCRD fetches a single Gameplane CRD object by name. namespace is
// required for namespaced kinds and ignored for cluster-scoped ones.
func (c *Client) GetCRD(ctx context.Context, kind, namespace, name string) (*unstructured.Unstructured, error) {
	if err := checkTarget(ctx); err != nil {
		return nil, err
	}
	k, err := kindOf(kind)
	if err != nil {
		return nil, err
	}
	if k.Namespaced && namespace == "" {
		return nil, fmt.Errorf("%s is namespace-scoped: namespace is required", kind)
	}
	if k.Namespaced {
		if err := c.checkNamespace(namespace); err != nil {
			return nil, err
		}
	}
	obj, err := c.resourceInterface(k, namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get %s %s/%s: %w", kind, namespace, name, err)
	}
	return obj, nil
}

// ListPods lists core Pods in namespace (empty namespace lists across all
// namespaces, or across the allow-list when set), optionally filtered by a
// label selector.
func (c *Client) ListPods(ctx context.Context, namespace, labelSelector string) (*corev1.PodList, error) {
	if err := checkTarget(ctx); err != nil {
		return nil, err
	}
	if err := c.checkNamespace(namespace); err != nil {
		return nil, err
	}
	if namespace == "" && len(c.namespaces) > 0 {
		merged := &corev1.PodList{}
		var skipped []string
		for _, ns := range c.namespaces {
			list, err := c.typed.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
			if err != nil {
				if skippable(err) {
					skipped = append(skipped, ns)
					continue
				}
				return nil, fmt.Errorf("list pods in %q: %w", ns, err)
			}
			merged.Items = append(merged.Items, list.Items...)
		}
		if len(skipped) > 0 {
			merged.ResourceVersion = skippedNote(skipped)
		}
		return merged, nil
	}
	list, err := c.typed.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, fmt.Errorf("list pods in %q: %w", namespace, err)
	}
	return list, nil
}

// GetPod fetches a single Pod by namespace and name.
func (c *Client) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	if err := checkTarget(ctx); err != nil {
		return nil, err
	}
	if err := c.checkNamespace(namespace); err != nil {
		return nil, err
	}
	if err := c.requireNamespace(namespace); err != nil {
		return nil, err
	}
	pod, err := c.typed.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get pod %s/%s: %w", namespace, name, err)
	}
	return pod, nil
}

// ListEvents lists core Events in namespace (empty namespace lists across
// all namespaces, or across the allow-list when set), optionally filtered by
// a field selector (e.g. "involvedObject.name=my-server,involvedObject.kind=GameServer").
func (c *Client) ListEvents(ctx context.Context, namespace, fieldSelector string) (*corev1.EventList, error) {
	if err := checkTarget(ctx); err != nil {
		return nil, err
	}
	if err := c.checkNamespace(namespace); err != nil {
		return nil, err
	}
	if namespace == "" && len(c.namespaces) > 0 {
		merged := &corev1.EventList{}
		var skipped []string
		for _, ns := range c.namespaces {
			list, err := c.typed.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: fieldSelector})
			if err != nil {
				if skippable(err) {
					skipped = append(skipped, ns)
					continue
				}
				return nil, fmt.Errorf("list events in %q: %w", ns, err)
			}
			merged.Items = append(merged.Items, list.Items...)
		}
		if len(skipped) > 0 {
			merged.ResourceVersion = skippedNote(skipped)
		}
		return merged, nil
	}
	list, err := c.typed.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: fieldSelector})
	if err != nil {
		return nil, fmt.Errorf("list events in %q: %w", namespace, err)
	}
	return list, nil
}

// PodLogs fetches (a bounded tail of) a pod's logs. tailLines <= 0 applies
// defaultTailLines; a request above maxTailLines is capped rather than
// rejected, so an overly broad ask degrades gracefully instead of failing.
func (c *Client) PodLogs(ctx context.Context, namespace, pod, container string, tailLines int64, previous bool) (string, error) {
	if err := checkTarget(ctx); err != nil {
		return "", err
	}
	if err := c.checkNamespace(namespace); err != nil {
		return "", err
	}
	if err := c.requireNamespace(namespace); err != nil {
		return "", err
	}
	tail := tailLines
	switch {
	case tail <= 0:
		tail = defaultTailLines
	case tail > maxTailLines:
		tail = maxTailLines
	}
	opts := &corev1.PodLogOptions{
		Container: container,
		Previous:  previous,
		TailLines: &tail,
	}
	stream, err := c.typed.CoreV1().Pods(namespace).GetLogs(pod, opts).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("stream logs for pod %s/%s: %w", namespace, pod, err)
	}
	defer func() { _ = stream.Close() }()

	data, truncated, err := readLogTail(stream)
	if err != nil {
		return "", fmt.Errorf("read logs for pod %s/%s: %w", namespace, pod, err)
	}
	// Keep the newest maxLogBytes, not the oldest: the tail is what the
	// CrashLoop advice in propose_fix actually needs (F-204).
	if truncated {
		return truncatedLogNotice + string(data), nil
	}
	return string(data), nil
}
