package panel

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GameplanePanel/gameplane/mcp-server/internal/reader"
)

const testToken = "gpmcp_test_token"

// handlerFunc answers one request to the fake panel API.
type handlerFunc func(r *http.Request) (int, string)

// recorded is the part of one request the tests assert on.
type recorded struct {
	method  string
	path    string
	query   url.Values
	auth    string
	session string
}

// fakeAPI is a recording stand-in for the Gameplane API. Routes are keyed by
// URL path; an unknown path answers 404.
type fakeAPI struct {
	mu     sync.Mutex
	routes map[string]handlerFunc
	reqs   []recorded
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{
		method:  r.Method,
		path:    r.URL.Path,
		query:   r.URL.Query(),
		auth:    r.Header.Get("Authorization"),
		session: r.Header.Get(sessionHeader),
	})
	h := f.routes[r.URL.Path]
	f.mu.Unlock()

	if h == nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "no route for "+r.URL.Path)
		return
	}
	status, body := h(r)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (f *fakeAPI) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

// reply answers 200 with body.
func reply(body string) handlerFunc {
	return func(*http.Request) (int, string) { return http.StatusOK, body }
}

// fail answers status with body.
func fail(status int, body string) handlerFunc {
	return func(*http.Request) (int, string) { return status, body }
}

// byCluster dispatches on the cluster query parameter.
func byCluster(m map[string]handlerFunc) handlerFunc {
	return func(r *http.Request) (int, string) {
		h, ok := m[r.URL.Query().Get("cluster")]
		if !ok {
			return http.StatusBadRequest, "unexpected cluster " + r.URL.Query().Get("cluster")
		}
		return h(r)
	}
}

func newTestClient(t *testing.T, routes map[string]handlerFunc) (*Client, *fakeAPI) {
	t.Helper()
	api := &fakeAPI{routes: routes}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, testToken, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, api
}

// clusterCtx selects a cluster, so calls do not fan out.
func clusterCtx(cluster string) context.Context {
	return reader.WithTarget(context.Background(), reader.Target{Cluster: cluster})
}

// assertGETWithAuth checks that every recorded request was a GET carrying the
// bearer token and one stable session header.
func assertGETWithAuth(t *testing.T, reqs []recorded) {
	t.Helper()
	for _, r := range reqs {
		if r.method != http.MethodGet {
			t.Errorf("%s: method = %s, want GET", r.path, r.method)
		}
		if r.auth != "Bearer "+testToken {
			t.Errorf("%s: Authorization = %q, want bearer token", r.path, r.auth)
		}
		if r.session == "" {
			t.Errorf("%s: missing %s header", r.path, sessionHeader)
		}
		if len(reqs) > 0 && r.session != reqs[0].session {
			t.Errorf("%s: session %q differs from %q", r.path, r.session, reqs[0].session)
		}
	}
}

func TestReaderMethodsHitPanelPaths(t *testing.T) {
	pods := `{"items":[{"metadata":{"name":"mc-0","namespace":"games"}}]}`
	routes := map[string]handlerFunc{
		"/servers":                    reply(`{"items":[{"metadata":{"name":"mc","namespace":"games"}}]}`),
		"/backups/nightly":            reply(`{"metadata":{"name":"nightly","namespace":"games"}}`),
		"/templates/mc-java":          reply(`{"metadata":{"name":"mc-java"}}`),
		"/modules/sources":            reply(`{"items":[{"metadata":{"name":"official"}},{"metadata":{"name":"community"}}]}`),
		"/pods":                       reply(pods),
		"/servers/mc/pods":            reply(pods),
		"/pods/mc-0":                  reply(`{"metadata":{"name":"mc-0","namespace":"games"},"status":{"phase":"Running"}}`),
		"/kube-events":                reply(`{"items":[{"metadata":{"name":"ev1","namespace":"games"},"reason":"Started"}]}`),
		"/servers/mc/events":          reply(`[]`),
		"/servers/mc/logs/pod/tail":   reply("line one\nline two\n"),
		"/admin/system-logs/api":      reply("api log"),
		"/admin/system-logs/operator": reply("operator log"),
	}

	cases := []struct {
		name   string
		target reader.Target
		call   func(ctx context.Context, c *Client) error
		path   string
		query  url.Values
	}{
		{
			name:   "ListCRD GameServer",
			target: reader.Target{Cluster: "prod"},
			call: func(ctx context.Context, c *Client) error {
				_, err := c.ListCRD(ctx, "GameServer", "games", "app=mc")
				return err
			},
			path:  "/servers",
			query: url.Values{"namespace": {"games"}, "labelSelector": {"app=mc"}, "cluster": {"prod"}},
		},
		{
			name:   "GetCRD Backup",
			target: reader.Target{Cluster: "prod"},
			call: func(ctx context.Context, c *Client) error {
				_, err := c.GetCRD(ctx, "Backup", "games", "nightly")
				return err
			},
			path:  "/backups/nightly",
			query: url.Values{"namespace": {"games"}, "cluster": {"prod"}},
		},
		{
			name:   "GetCRD GameTemplate is cluster-scoped",
			target: reader.Target{Cluster: "prod"},
			call: func(ctx context.Context, c *Client) error {
				_, err := c.GetCRD(ctx, "GameTemplate", "ignored", "mc-java")
				return err
			},
			path:  "/templates/mc-java",
			query: url.Values{"cluster": {"prod"}},
		},
		{
			name:   "GetCRD ModuleSource lists and matches by name",
			target: reader.Target{Cluster: "prod"},
			call: func(ctx context.Context, c *Client) error {
				obj, err := c.GetCRD(ctx, "ModuleSource", "", "community")
				if err != nil {
					return err
				}
				if obj.GetName() != "community" {
					return errors.New("GetCRD returned " + obj.GetName() + ", want community")
				}
				return nil
			},
			path:  "/modules/sources",
			query: url.Values{"cluster": {"prod"}},
		},
		{
			name:   "ListPods by namespace",
			target: reader.Target{Cluster: "prod"},
			call: func(ctx context.Context, c *Client) error {
				_, err := c.ListPods(ctx, "games", "app=mc")
				return err
			},
			path:  "/pods",
			query: url.Values{"namespace": {"games"}, "labelSelector": {"app=mc"}, "cluster": {"prod"}},
		},
		{
			name:   "ListPods for a server",
			target: reader.Target{Cluster: "prod", Server: "mc"},
			call: func(ctx context.Context, c *Client) error {
				_, err := c.ListPods(ctx, "games", "")
				return err
			},
			path:  "/servers/mc/pods",
			query: url.Values{"namespace": {"games"}, "cluster": {"prod"}},
		},
		{
			name:   "GetPod",
			target: reader.Target{Cluster: "prod"},
			call: func(ctx context.Context, c *Client) error {
				_, err := c.GetPod(ctx, "games", "mc-0")
				return err
			},
			path:  "/pods/mc-0",
			query: url.Values{"namespace": {"games"}, "cluster": {"prod"}},
		},
		{
			name:   "ListEvents by namespace",
			target: reader.Target{Cluster: "prod"},
			call: func(ctx context.Context, c *Client) error {
				_, err := c.ListEvents(ctx, "games", "involvedObject.name=mc-0")
				return err
			},
			path:  "/kube-events",
			query: url.Values{"namespace": {"games"}, "fieldSelector": {"involvedObject.name=mc-0"}, "cluster": {"prod"}},
		},
		{
			name:   "ListEvents for a server",
			target: reader.Target{Cluster: "prod", Server: "mc"},
			call: func(ctx context.Context, c *Client) error {
				_, err := c.ListEvents(ctx, "games", "")
				return err
			},
			path:  "/servers/mc/events",
			query: url.Values{"namespace": {"games"}, "cluster": {"prod"}},
		},
		{
			name:   "PodLogs for a server",
			target: reader.Target{Cluster: "prod", Server: "mc"},
			call: func(ctx context.Context, c *Client) error {
				_, err := c.PodLogs(ctx, "games", "mc-0", "main", 50, true)
				return err
			},
			path: "/servers/mc/logs/pod/tail",
			query: url.Values{
				"namespace": {"games"}, "pod": {"mc-0"}, "container": {"main"},
				"tailLines": {"50"}, "previous": {"true"}, "cluster": {"prod"},
			},
		},
		{
			name:   "PodLogs for the API system pod",
			target: reader.Target{Cluster: "prod"},
			call: func(ctx context.Context, c *Client) error {
				_, err := c.PodLogs(ctx, "gameplane-system", "gameplane-api-7d9", "", 0, false)
				return err
			},
			path:  "/admin/system-logs/api",
			query: url.Values{"pod": {"gameplane-api-7d9"}, "tailLines": {"200"}, "cluster": {"prod"}},
		},
		{
			name:   "PodLogs for the operator system pod clamps the tail",
			target: reader.Target{Cluster: "prod"},
			call: func(ctx context.Context, c *Client) error {
				_, err := c.PodLogs(ctx, "gameplane-system", "gameplane-operator-1", "", 9999, false)
				return err
			},
			path:  "/admin/system-logs/operator",
			query: url.Values{"pod": {"gameplane-operator-1"}, "tailLines": {"5000"}, "cluster": {"prod"}},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c, api := newTestClient(t, routes)
			ctx := reader.WithTarget(context.Background(), tt.target)
			if err := tt.call(ctx, c); err != nil {
				t.Fatalf("call: %v", err)
			}
			reqs := api.requests()
			if len(reqs) != 1 {
				t.Fatalf("want 1 request, got %d: %+v", len(reqs), reqs)
			}
			if reqs[0].path != tt.path {
				t.Errorf("path = %q, want %q", reqs[0].path, tt.path)
			}
			if !reflect.DeepEqual(reqs[0].query, tt.query) {
				t.Errorf("query = %v, want %v", reqs[0].query, tt.query)
			}
			assertGETWithAuth(t, reqs)
		})
	}
}

func TestPodLogsReturnsRawText(t *testing.T) {
	c, _ := newTestClient(t, map[string]handlerFunc{
		"/servers/mc/logs/pod/tail": reply("line one\nline two\n"),
	})
	ctx := reader.WithTarget(context.Background(), reader.Target{Cluster: "prod", Server: "mc"})
	got, err := c.PodLogs(ctx, "games", "mc-0", "", 0, false)
	if err != nil {
		t.Fatalf("PodLogs: %v", err)
	}
	if got != "line one\nline two\n" {
		t.Errorf("PodLogs = %q, want the raw body", got)
	}
}

func TestPodLogsTailClamp(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "200"},
		{-5, "200"},
		{42, "42"},
		{9999, "5000"},
	}
	for _, tt := range cases {
		t.Run(tt.want, func(t *testing.T) {
			c, api := newTestClient(t, map[string]handlerFunc{
				"/servers/mc/logs/pod/tail": reply(""),
			})
			ctx := reader.WithTarget(context.Background(), reader.Target{Cluster: "prod", Server: "mc"})
			if _, err := c.PodLogs(ctx, "games", "mc-0", "", tt.in, false); err != nil {
				t.Fatalf("PodLogs: %v", err)
			}
			if got := api.requests()[0].query.Get("tailLines"); got != tt.want {
				t.Errorf("tailLines = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPodLogsRequiresServerForGameServerPods(t *testing.T) {
	c, api := newTestClient(t, map[string]handlerFunc{})
	_, err := c.PodLogs(clusterCtx("prod"), "games", "mc-0", "", 0, false)
	if err == nil || !strings.Contains(err.Error(), "server is required") {
		t.Fatalf("want server-required error, got %v", err)
	}
	if n := len(api.requests()); n != 0 {
		t.Errorf("want no API request, got %d", n)
	}
}

func TestListCRDAcrossNamespacesOfOneCluster(t *testing.T) {
	c, api := newTestClient(t, map[string]handlerFunc{
		"/namespaces": reply(`{"namespaces":["games","ops"]}`),
		"/servers": func(r *http.Request) (int, string) {
			if r.URL.Query().Get("namespace") == "games" {
				return http.StatusOK, `{"items":[{"metadata":{"name":"mc","namespace":"games"}}]}`
			}
			return http.StatusOK, `{"items":[{"metadata":{"name":"ops-mc","namespace":"ops"}}]}`
		},
	})
	list, err := c.ListCRD(clusterCtx("prod"), "GameServer", "", "")
	if err != nil {
		t.Fatalf("ListCRD: %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("want 2 items, got %d", len(list.Items))
	}
	if list.GetKind() != "GameServerList" {
		t.Errorf("kind = %q, want GameServerList", list.GetKind())
	}
	reqs := api.requests()
	if len(reqs) != 3 || reqs[0].path != "/namespaces" {
		t.Fatalf("want /namespaces then two /servers calls, got %+v", reqs)
	}
	assertGETWithAuth(t, reqs)
}

func TestGameServerForbiddenFallsBackToOwnServers(t *testing.T) {
	ownServers := reply(`{"items":[` +
		`{"metadata":{"name":"mc","namespace":"games"}},` +
		`{"metadata":{"name":"val","namespace":"games-2"}}]}`)

	t.Run("named namespace keeps only that namespace", func(t *testing.T) {
		c, api := newTestClient(t, map[string]handlerFunc{
			"/servers":          fail(http.StatusForbidden, "no access to games-2"),
			"/users/me/servers": ownServers,
		})
		list, err := c.ListCRD(clusterCtx("prod"), "GameServer", "games", "")
		if err != nil {
			t.Fatalf("ListCRD: %v", err)
		}
		if len(list.Items) != 1 || list.Items[0].GetName() != "mc" {
			t.Errorf("want only mc, got %d items", len(list.Items))
		}
		reqs := api.requests()
		if last := reqs[len(reqs)-1]; last.path != "/users/me/servers" {
			t.Errorf("last request = %s, want /users/me/servers", last.path)
		}
	})

	t.Run("all namespaces when the namespace list is forbidden", func(t *testing.T) {
		c, _ := newTestClient(t, map[string]handlerFunc{
			"/namespaces":       fail(http.StatusForbidden, "no namespace list"),
			"/users/me/servers": ownServers,
		})
		list, err := c.ListCRD(clusterCtx("prod"), "GameServer", "", "")
		if err != nil {
			t.Fatalf("ListCRD: %v", err)
		}
		if len(list.Items) != 2 {
			t.Errorf("want both own servers, got %d", len(list.Items))
		}
	})
}

func TestAPIErrorMessages(t *testing.T) {
	leak := "bad token " + testToken
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "not authenticated: the MCP token is missing, expired or revoked"},
		{http.StatusForbidden, "permission denied: bad token [redacted]"},
		{http.StatusNotFound, "not found: bad token [redacted]"},
		{http.StatusInternalServerError, "gameplane api /pods/mc-0: Internal Server Error: bad token [redacted]"},
	}
	for _, tt := range cases {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			c, _ := newTestClient(t, map[string]handlerFunc{"/pods/mc-0": fail(tt.status, leak)})
			_, err := c.GetPod(clusterCtx("prod"), "games", "mc-0")
			if err == nil {
				t.Fatal("want error")
			}
			if err.Error() != tt.want {
				t.Errorf("error = %q, want %q", err.Error(), tt.want)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Errorf("token leaked into error: %q", err.Error())
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != tt.status {
				t.Errorf("want *APIError with status %d, got %v", tt.status, err)
			}
		})
	}
}

func TestAPIErrorMessageIsBounded(t *testing.T) {
	c, _ := newTestClient(t, map[string]handlerFunc{
		"/pods/big": fail(http.StatusForbidden, strings.Repeat("x", 1000)),
	})
	_, err := c.GetPod(clusterCtx("prod"), "games", "big")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if len(apiErr.Message) > maxMessageBytes {
		t.Errorf("message is %d bytes, want at most %d", len(apiErr.Message), maxMessageBytes)
	}
}

func TestServerEventsConvertToCoreEvents(t *testing.T) {
	body := `[` +
		`{"id":"ev-1","time":"2026-03-04T05:06:07Z","type":"Warning","reason":"BackOff",` +
		`"message":"restarting","source":"kubelet","object":"Pod/mc-0","count":3},` +
		`{"id":"ev-2","time":"not-a-time","type":"Normal","reason":"Started",` +
		`"message":"ok","source":"kubelet","object":"GameServer/mc","count":1}]`
	c, _ := newTestClient(t, map[string]handlerFunc{"/servers/mc/events": reply(body)})
	ctx := reader.WithTarget(context.Background(), reader.Target{Cluster: "prod", Server: "mc"})

	list, err := c.ListEvents(ctx, "games", "")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("want 2 events, got %d", len(list.Items))
	}

	e := list.Items[0]
	if e.Name != "ev-1" || e.Namespace != "games" {
		t.Errorf("name/namespace = %s/%s, want ev-1/games", e.Name, e.Namespace)
	}
	if e.Type != "Warning" || e.Reason != "BackOff" || e.Message != "restarting" {
		t.Errorf("type/reason/message = %q/%q/%q", e.Type, e.Reason, e.Message)
	}
	if e.Source.Component != "kubelet" {
		t.Errorf("source component = %q, want kubelet", e.Source.Component)
	}
	if e.Count != 3 {
		t.Errorf("count = %d, want 3", e.Count)
	}
	if want := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC); !e.LastTimestamp.Time.Equal(want) {
		t.Errorf("last timestamp = %v, want %v", e.LastTimestamp.Time, want)
	}
	if e.InvolvedObject.Kind != "Pod" || e.InvolvedObject.Name != "mc-0" || e.InvolvedObject.Namespace != "games" {
		t.Errorf("involved object = %+v", e.InvolvedObject)
	}

	bad := list.Items[1]
	if !bad.LastTimestamp.IsZero() {
		t.Errorf("malformed time should leave the timestamp zero, got %v", bad.LastTimestamp.Time)
	}
	if bad.InvolvedObject.Kind != "GameServer" || bad.InvolvedObject.Name != "mc" {
		t.Errorf("involved object = %+v, want GameServer/mc", bad.InvolvedObject)
	}
}

func TestNewValidatesURLAndToken(t *testing.T) {
	cases := []struct {
		name  string
		url   string
		token string
		ok    bool
	}{
		{"https host", "https://panel.example.com", "gpmcp_x", true},
		{"http localhost", "http://localhost:8080", "gpmcp_x", true},
		{"http IPv4 loopback", "http://127.0.0.1:8080", "gpmcp_x", true},
		{"http IPv6 loopback", "http://[::1]:8080", "gpmcp_x", true},
		{"http remote host", "http://panel.example.com", "gpmcp_x", false},
		{"http private address", "http://10.0.0.5", "gpmcp_x", false},
		{"ftp scheme", "ftp://localhost", "gpmcp_x", false},
		{"no host", "https://", "gpmcp_x", false},
		{"credentials in URL", "https://user:pw@panel.example.com", "gpmcp_x", false},
		{"token without prefix", "https://panel.example.com", "abc", false},
		{"empty token", "https://panel.example.com", "", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(tt.url, tt.token, "")
			if (err == nil) != tt.ok {
				t.Fatalf("New(%q) error = %v, want ok=%v", tt.url, err, tt.ok)
			}
			if err == nil && c == nil {
				t.Fatal("New returned a nil client without an error")
			}
		})
	}
}

func TestNewRejectsBadCAFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := New("https://panel.example.com", testToken, filepath.Join(dir, "missing.pem")); err == nil {
		t.Error("want error for a missing CA file")
	}
	garbage := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := New("https://panel.example.com", testToken, garbage); err == nil {
		t.Error("want error for a CA file without PEM certificates")
	}
}

func TestNewTrustsPrivateCAFile(t *testing.T) {
	api := &fakeAPI{routes: map[string]handlerFunc{
		"/pods/mc-0": reply(`{"metadata":{"name":"mc-0","namespace":"games"}}`),
	}}
	srv := httptest.NewTLSServer(api)
	t.Cleanup(srv.Close)

	caPath := filepath.Join(t.TempDir(), "ca.pem")
	f, err := os.Create(caPath)
	if err != nil {
		t.Fatalf("create CA file: %v", err)
	}
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}); err != nil {
		t.Fatalf("encode CA: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close CA file: %v", err)
	}

	trusted, err := New(srv.URL, testToken, caPath)
	if err != nil {
		t.Fatalf("New with CA: %v", err)
	}
	pod, err := trusted.GetPod(clusterCtx("prod"), "games", "mc-0")
	if err != nil {
		t.Fatalf("GetPod over TLS with the CA: %v", err)
	}
	if pod.Name != "mc-0" {
		t.Errorf("pod name = %q, want mc-0", pod.Name)
	}

	untrusted, err := New(srv.URL, testToken, "")
	if err != nil {
		t.Fatalf("New without CA: %v", err)
	}
	if _, err := untrusted.GetPod(clusterCtx("prod"), "games", "mc-0"); err == nil {
		t.Error("want a TLS verification failure without the CA")
	}
}
