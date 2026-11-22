// Package panel is the per-user reader: it reads through the Gameplane API as
// the MCP token's user. Every request is built and sent by get, the only
// place in the package that talks to the network, and get issues GET requests
// only. The source tripwire in source_test.go enforces that.
package panel

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/GameplanePanel/gameplane/mcp-server/internal/reader"
)

const (
	requestTimeout    = 30 * time.Second
	defaultFanout     = 4
	defaultPerCluster = 10 * time.Second
	maxResponseBytes  = 32 << 20
	maxMessageBytes   = 300
	defaultTailLines  = 200
	maxTailLines      = 5000
	tokenPrefix       = "gpmcp_"
	sessionHeader     = "X-Gameplane-MCP-Session"
	clusterAnnotation = "gameplane.local/cluster"
)

// Client is the per-user reader. It holds the panel base URL, the MCP token
// and a per-process session ID.
type Client struct {
	base       *url.URL
	token      string
	session    string
	hc         *http.Client
	fanout     int
	perCluster time.Duration
}

// kindInfo maps a CRD kind onto its panel API path.
type kindInfo struct {
	kind       string
	path       string
	namespaced bool
	listOnly   bool
}

var kinds = []kindInfo{
	{kind: "GameServer", path: "servers", namespaced: true},
	{kind: "GameTemplate", path: "templates"},
	{kind: "Backup", path: "backups", namespaced: true},
	{kind: "BackupSchedule", path: "schedules", namespaced: true},
	{kind: "Restore", path: "restores", namespaced: true},
	{kind: "Module", path: "modules"},
	{kind: "ModuleSource", path: "modules/sources", listOnly: true},
}

// New builds a per-user Client. baseURL must be https, or http for a loopback
// host. token must be a Gameplane MCP token. caFile, when set, is a PEM bundle
// added to the system roots for a private certificate authority.
func New(baseURL, token, caFile string) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("parse panel URL: %w", err)
	}
	if u.Host == "" {
		return nil, errors.New("panel URL must include a host")
	}
	if u.User != nil {
		return nil, errors.New("panel URL must not contain credentials")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return nil, errors.New("panel URL must use https (http is allowed only for localhost or a loopback address)")
		}
	default:
		return nil, errors.New("panel URL must use https (http is allowed only for localhost or a loopback address)")
	}
	u.RawQuery = ""
	u.RawFragment = ""
	u.Fragment = ""

	if !strings.HasPrefix(token, tokenPrefix) {
		return nil, errors.New("GAMEPLANE_MCP_TOKEN must be a Gameplane MCP token (gpmcp_…)")
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pool, err := certPool(caFile)
		if err != nil {
			return nil, err
		}
		tlsCfg.RootCAs = pool
	}
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		tr = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	tr = tr.Clone()
	tr.TLSClientConfig = tlsCfg

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate session ID: %w", err)
	}

	return &Client{
		base:       u,
		token:      token,
		session:    base64.RawURLEncoding.EncodeToString(raw),
		hc:         &http.Client{Timeout: requestTimeout, Transport: tr},
		fanout:     defaultFanout,
		perCluster: defaultPerCluster,
	}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func certPool(caFile string) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read panel CA file: %w", err)
	}
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, errors.New("panel CA file contains no PEM certificates")
	}
	return pool, nil
}

// APIError is a non-2xx response from the Gameplane API.
type APIError struct {
	Status  int
	Path    string
	Message string
}

func (e *APIError) Error() string {
	switch e.Status {
	case http.StatusUnauthorized:
		return "not authenticated: the MCP token is missing, expired or revoked"
	case http.StatusForbidden:
		return "permission denied: " + e.Message
	case http.StatusNotFound:
		return "not found: " + e.Message
	}
	return fmt.Sprintf("gameplane api %s: %s: %s", e.Path, http.StatusText(e.Status), e.Message)
}

// statusOf returns the HTTP status carried by err, or 0 when err is not an APIError.
func statusOf(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}

// trimMessage bounds an error body to maxMessageBytes and removes the token
// if the server ever echoes it.
func (c *Client) trimMessage(body []byte) string {
	s := strings.TrimSpace(string(body))
	s = strings.ReplaceAll(s, c.token, "[redacted]")
	if len(s) > maxMessageBytes {
		cut := maxMessageBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s
}

// endpoint joins path and q onto the panel base URL.
func (c *Client) endpoint(path string, q url.Values) (string, error) {
	full := strings.TrimSuffix(c.base.String(), "/") + path
	u, err := url.Parse(full)
	if err != nil {
		return "", fmt.Errorf("gameplane api %s: build URL: %w", path, err)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// get is the only request builder in the package. It sends a GET with the
// bearer token and session header, adds the cluster selection when the
// context names one, and decodes the body into out. A *string out receives
// the raw body.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	if q == nil {
		q = url.Values{}
	}
	if cl := reader.TargetFrom(ctx).Cluster; cl != "" {
		q.Set("cluster", cl)
	}
	target, err := c.endpoint(path, q)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("gameplane api %s: build request: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set(sessionHeader, c.session)
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("gameplane api %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("gameplane api %s: read response: %w", path, err)
	}
	if len(body) > maxResponseBytes {
		return fmt.Errorf("gameplane api %s: response exceeds %d bytes", path, maxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Status: resp.StatusCode, Path: path, Message: c.trimMessage(body)}
	}
	if s, ok := out.(*string); ok {
		*s = string(body)
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("gameplane api %s: decode response: %w", path, err)
	}
	return nil
}

// lookupKind resolves a CRD kind name to its panel path.
func lookupKind(kind string) (kindInfo, error) {
	for _, k := range kinds {
		if k.kind == kind {
			return k, nil
		}
	}
	names := make([]string, 0, len(kinds))
	for _, k := range kinds {
		names = append(names, k.kind)
	}
	return kindInfo{}, fmt.Errorf("unknown kind %q; known kinds: %s", kind, strings.Join(names, ", "))
}

// unstructuredList wraps items as an UnstructuredList. The kind is set so the
// list marshals like the API's own list objects.
func unstructuredList(kind string, items []unstructured.Unstructured) *unstructured.UnstructuredList {
	list := &unstructured.UnstructuredList{Object: map[string]any{}, Items: items}
	if list.Items == nil {
		list.Items = []unstructured.Unstructured{}
	}
	list.SetKind(kind + "List")
	return list
}

// getItems fetches a {items:[...]} response as unstructured objects. Objects
// are wrapped from the raw maps, so the API response does not need a kind.
func (c *Client) getItems(ctx context.Context, path string, q url.Values) ([]unstructured.Unstructured, error) {
	var raw struct {
		Items []map[string]any `json:"items"`
	}
	if err := c.get(ctx, path, q, &raw); err != nil {
		return nil, err
	}
	items := make([]unstructured.Unstructured, 0, len(raw.Items))
	for _, m := range raw.Items {
		if m != nil {
			items = append(items, unstructured.Unstructured{Object: m})
		}
	}
	return items, nil
}

// namespaces lists the namespaces visible in the selected cluster.
func (c *Client) namespaces(ctx context.Context) ([]string, error) {
	var out struct {
		Namespaces []string `json:"namespaces"`
	}
	if err := c.get(ctx, "/namespaces", nil, &out); err != nil {
		return nil, err
	}
	return out.Namespaces, nil
}

// ListCRD lists objects of kind. With no cluster in the context it fans out
// across every registered cluster.
func (c *Client) ListCRD(ctx context.Context, kind, namespace, labelSelector string) (*unstructured.UnstructuredList, error) {
	k, err := lookupKind(kind)
	if err != nil {
		return nil, err
	}
	if reader.TargetFrom(ctx).Cluster != "" {
		return c.listCRDLocal(ctx, k, namespace, labelSelector)
	}
	return c.listCRDAcross(ctx, k, namespace, labelSelector)
}

// listCRDLocal lists kind in the selected cluster. A namespaced kind with no
// namespace loops over the cluster's namespaces.
func (c *Client) listCRDLocal(ctx context.Context, k kindInfo, ns, sel string) (*unstructured.UnstructuredList, error) {
	if !k.namespaced || ns != "" {
		return c.listCRDIn(ctx, k, ns, sel)
	}
	nss, err := c.namespaces(ctx)
	if err != nil {
		if k.kind == "GameServer" && statusOf(err) == http.StatusForbidden {
			return c.ownServers(ctx, "", sel)
		}
		return nil, err
	}
	var items []unstructured.Unstructured
	for _, n := range nss {
		part, err := c.listCRDIn(ctx, k, n, sel)
		if err != nil {
			return nil, err
		}
		items = append(items, part.Items...)
	}
	return unstructuredList(k.kind, items), nil
}

// listCRDIn lists kind in one namespace (or unscoped for cluster-scoped kinds).
// A GameServer list that the user may not make by namespace falls back to the
// user's own servers.
func (c *Client) listCRDIn(ctx context.Context, k kindInfo, ns, sel string) (*unstructured.UnstructuredList, error) {
	q := url.Values{}
	if k.namespaced && ns != "" {
		q.Set("namespace", ns)
	}
	if sel != "" {
		q.Set("labelSelector", sel)
	}
	items, err := c.getItems(ctx, "/"+k.path, q)
	if err != nil {
		if k.kind == "GameServer" && ns != "" && statusOf(err) == http.StatusForbidden {
			return c.ownServers(ctx, ns, sel)
		}
		return nil, err
	}
	return unstructuredList(k.kind, items), nil
}

// ownServers reads /users/me/servers and keeps only the items in ns when ns
// is set.
func (c *Client) ownServers(ctx context.Context, ns, sel string) (*unstructured.UnstructuredList, error) {
	q := url.Values{}
	if sel != "" {
		q.Set("labelSelector", sel)
	}
	items, err := c.getItems(ctx, "/users/me/servers", q)
	if err != nil {
		return nil, err
	}
	if ns != "" {
		kept := make([]unstructured.Unstructured, 0, len(items))
		for _, it := range items {
			if it.GetNamespace() == ns {
				kept = append(kept, it)
			}
		}
		items = kept
	}
	return unstructuredList("GameServer", items), nil
}

// GetCRD reads one object. ModuleSource has no single-object endpoint, so it
// is listed and matched by name.
func (c *Client) GetCRD(ctx context.Context, kind, namespace, name string) (*unstructured.Unstructured, error) {
	k, err := lookupKind(kind)
	if err != nil {
		return nil, err
	}
	if k.listOnly {
		list, err := c.listCRDLocal(ctx, k, "", "")
		if err != nil {
			return nil, err
		}
		for i := range list.Items {
			if list.Items[i].GetName() == name {
				return &list.Items[i], nil
			}
		}
		return nil, &APIError{Status: http.StatusNotFound, Path: "/" + k.path, Message: fmt.Sprintf("%s %q not found", k.kind, name)}
	}
	q := url.Values{}
	if k.namespaced && namespace != "" {
		q.Set("namespace", namespace)
	}
	var raw map[string]any
	if err := c.get(ctx, "/"+k.path+"/"+url.PathEscape(name), q, &raw); err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, &APIError{Status: http.StatusNotFound, Path: "/" + k.path, Message: fmt.Sprintf("%s %q not found", k.kind, name)}
	}
	return &unstructured.Unstructured{Object: raw}, nil
}

// ListPods lists pods. With no cluster in the context it fans out across every
// registered cluster.
func (c *Client) ListPods(ctx context.Context, namespace, labelSelector string) (*corev1.PodList, error) {
	if reader.TargetFrom(ctx).Cluster != "" {
		return c.listPodsLocal(ctx, namespace, labelSelector)
	}
	return c.listPodsAcross(ctx, namespace, labelSelector)
}

// listPodsLocal lists pods in the selected cluster. A server target or a
// named namespace is one request; otherwise it loops over the namespaces.
func (c *Client) listPodsLocal(ctx context.Context, ns, sel string) (*corev1.PodList, error) {
	if reader.TargetFrom(ctx).Server != "" || ns != "" {
		return c.podsIn(ctx, ns, sel)
	}
	nss, err := c.namespaces(ctx)
	if err != nil {
		return nil, err
	}
	merged := &corev1.PodList{}
	for _, n := range nss {
		part, err := c.podsIn(ctx, n, sel)
		if err != nil {
			return nil, err
		}
		merged.Items = append(merged.Items, part.Items...)
	}
	return merged, nil
}

func (c *Client) podsIn(ctx context.Context, ns, sel string) (*corev1.PodList, error) {
	q := url.Values{}
	if ns != "" {
		q.Set("namespace", ns)
	}
	path := "/pods"
	if srv := reader.TargetFrom(ctx).Server; srv != "" {
		path = "/servers/" + url.PathEscape(srv) + "/pods"
	} else if sel != "" {
		q.Set("labelSelector", sel)
	}
	out := &corev1.PodList{}
	if err := c.get(ctx, path, q, out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetPod reads one pod from the selected cluster.
func (c *Client) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	q := url.Values{}
	if namespace != "" {
		q.Set("namespace", namespace)
	}
	pod := &corev1.Pod{}
	if err := c.get(ctx, "/pods/"+url.PathEscape(name), q, pod); err != nil {
		return nil, err
	}
	return pod, nil
}

// serverEvent is one row of /servers/{name}/events.
type serverEvent struct {
	ID      string `json:"id"`
	Time    string `json:"time"`
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Source  string `json:"source"`
	Object  string `json:"object"`
	Count   int32  `json:"count"`
}

// toEvent converts a server event row to a corev1.Event. A malformed time
// leaves LastTimestamp zero rather than failing the whole list.
func (e serverEvent) toEvent(ns string) corev1.Event {
	kind, name, ok := strings.Cut(e.Object, "/")
	if !ok {
		kind, name = "", e.Object
	}
	ev := corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: e.ID, Namespace: ns},
		Type:           e.Type,
		Reason:         e.Reason,
		Message:        e.Message,
		Source:         corev1.EventSource{Component: e.Source},
		Count:          e.Count,
		InvolvedObject: corev1.ObjectReference{Kind: kind, Name: name, Namespace: ns},
	}
	if t, err := time.Parse(time.RFC3339, e.Time); err == nil {
		ev.LastTimestamp = metav1.NewTime(t)
	}
	return ev
}

// ListEvents lists events. With no cluster in the context it fans out across
// every registered cluster.
func (c *Client) ListEvents(ctx context.Context, namespace, fieldSelector string) (*corev1.EventList, error) {
	if reader.TargetFrom(ctx).Cluster != "" {
		return c.listEventsLocal(ctx, namespace, fieldSelector)
	}
	return c.listEventsAcross(ctx, namespace, fieldSelector)
}

// listEventsLocal lists events in the selected cluster, with the same
// namespace rules as listPodsLocal.
func (c *Client) listEventsLocal(ctx context.Context, ns, fsel string) (*corev1.EventList, error) {
	if reader.TargetFrom(ctx).Server != "" || ns != "" {
		return c.eventsIn(ctx, ns, fsel)
	}
	nss, err := c.namespaces(ctx)
	if err != nil {
		return nil, err
	}
	merged := &corev1.EventList{}
	for _, n := range nss {
		part, err := c.eventsIn(ctx, n, fsel)
		if err != nil {
			return nil, err
		}
		merged.Items = append(merged.Items, part.Items...)
	}
	return merged, nil
}

func (c *Client) eventsIn(ctx context.Context, ns, fsel string) (*corev1.EventList, error) {
	q := url.Values{}
	if ns != "" {
		q.Set("namespace", ns)
	}
	if srv := reader.TargetFrom(ctx).Server; srv != "" {
		var rows []serverEvent
		if err := c.get(ctx, "/servers/"+url.PathEscape(srv)+"/events", q, &rows); err != nil {
			return nil, err
		}
		out := &corev1.EventList{Items: make([]corev1.Event, 0, len(rows))}
		for _, r := range rows {
			out.Items = append(out.Items, r.toEvent(ns))
		}
		return out, nil
	}
	if fsel != "" {
		q.Set("fieldSelector", fsel)
	}
	out := &corev1.EventList{}
	if err := c.get(ctx, "/kube-events", q, out); err != nil {
		return nil, err
	}
	return out, nil
}

// PodLogs reads pod logs. A server target reads that server's pod through the
// API. Without one, the operator and API system pods are readable by name;
// any other pod needs a server.
func (c *Client) PodLogs(ctx context.Context, namespace, pod, container string, tailLines int64, previous bool) (string, error) {
	tail := tailLines
	if tail <= 0 {
		tail = defaultTailLines
	}
	if tail > maxTailLines {
		tail = maxTailLines
	}
	tailStr := strconv.FormatInt(tail, 10)

	var logs string
	if srv := reader.TargetFrom(ctx).Server; srv != "" {
		q := url.Values{}
		if namespace != "" {
			q.Set("namespace", namespace)
		}
		q.Set("pod", pod)
		if container != "" {
			q.Set("container", container)
		}
		q.Set("tailLines", tailStr)
		q.Set("previous", strconv.FormatBool(previous))
		if err := c.get(ctx, "/servers/"+url.PathEscape(srv)+"/logs/pod/tail", q, &logs); err != nil {
			return "", err
		}
		return logs, nil
	}

	var path string
	switch {
	case strings.HasPrefix(pod, "gameplane-api"):
		path = "/admin/system-logs/api"
	case strings.HasPrefix(pod, "gameplane-operator"):
		path = "/admin/system-logs/operator"
	default:
		return "", errors.New("server is required to read game server logs in per-user mode")
	}
	q := url.Values{}
	q.Set("pod", pod)
	q.Set("tailLines", tailStr)
	if err := c.get(ctx, path, q, &logs); err != nil {
		return "", err
	}
	return logs, nil
}

var _ reader.Reader = (*Client)(nil)
