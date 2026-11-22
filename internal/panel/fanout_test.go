package panel

import (
	"context"
	"net/http"
	"testing"

	"github.com/GameplanePanel/gameplane/mcp-server/internal/reader"
)

const clustersReply = `{"items":[{"name":"east"},{"name":"west"}]}`

func TestFanOutMergesAndAnnotatesClusters(t *testing.T) {
	c, api := newTestClient(t, map[string]handlerFunc{
		"/clusters": reply(clustersReply),
		"/pods": byCluster(map[string]handlerFunc{
			"east": reply(`{"items":[{"metadata":{"name":"mc-east","namespace":"games"}}]}`),
			"west": reply(`{"items":[{"metadata":{"name":"mc-west","namespace":"games"}}]}`),
		}),
	})

	pods, err := c.ListPods(context.Background(), "games", "")
	if err != nil {
		t.Fatalf("ListPods: %v", err)
	}
	if len(pods.Items) != 2 {
		t.Fatalf("want 2 pods, got %d", len(pods.Items))
	}
	wantCluster := map[string]string{"mc-east": "east", "mc-west": "west"}
	for _, p := range pods.Items {
		if got := p.Annotations[clusterAnnotation]; got != wantCluster[p.Name] {
			t.Errorf("pod %s annotation = %q, want %q", p.Name, got, wantCluster[p.Name])
		}
	}
	if pods.ResourceVersion != "" {
		t.Errorf("nothing unavailable, want empty resource version, got %q", pods.ResourceVersion)
	}

	reqs := api.requests()
	clusterCalls := 0
	for _, r := range reqs {
		if r.path == "/clusters" {
			clusterCalls++
		}
	}
	if clusterCalls != 1 {
		t.Errorf("want one /clusters call, got %d", clusterCalls)
	}
	assertGETWithAuth(t, reqs)
}

func TestFanOutAnnotatesUnstructuredAndEvents(t *testing.T) {
	c, _ := newTestClient(t, map[string]handlerFunc{
		"/clusters": reply(clustersReply),
		"/templates": byCluster(map[string]handlerFunc{
			"east": reply(`{"items":[{"metadata":{"name":"mc-java"}}]}`),
			"west": reply(`{"items":[{"metadata":{"name":"mc-bedrock"}}]}`),
		}),
		"/kube-events": byCluster(map[string]handlerFunc{
			"east": reply(`{"items":[{"metadata":{"name":"ev1","namespace":"games"},"reason":"Started"}]}`),
			"west": reply(`{"items":[]}`),
		}),
	})
	ctx := context.Background()

	tmpls, err := c.ListCRD(ctx, "GameTemplate", "", "")
	if err != nil {
		t.Fatalf("ListCRD: %v", err)
	}
	if len(tmpls.Items) != 2 {
		t.Fatalf("want 2 templates, got %d", len(tmpls.Items))
	}
	wantCluster := map[string]string{"mc-java": "east", "mc-bedrock": "west"}
	for _, it := range tmpls.Items {
		if got := it.GetAnnotations()[clusterAnnotation]; got != wantCluster[it.GetName()] {
			t.Errorf("template %s annotation = %q, want %q", it.GetName(), got, wantCluster[it.GetName()])
		}
	}

	events, err := c.ListEvents(ctx, "games", "")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events.Items) != 1 {
		t.Fatalf("want 1 event, got %d", len(events.Items))
	}
	if got := events.Items[0].Annotations[clusterAnnotation]; got != "east" {
		t.Errorf("event annotation = %q, want east", got)
	}
}

func TestFanOutReportsUnavailableClusters(t *testing.T) {
	c, _ := newTestClient(t, map[string]handlerFunc{
		"/clusters": reply(`{"items":[{"name":"east"},{"name":"bad"}]}`),
		"/pods": byCluster(map[string]handlerFunc{
			"east": reply(`{"items":[{"metadata":{"name":"mc-east","namespace":"games"}}]}`),
			"bad":  fail(http.StatusInternalServerError, "boom"),
		}),
	})

	pods, err := c.ListPods(context.Background(), "games", "")
	if err != nil {
		t.Fatalf("ListPods: %v", err)
	}
	if len(pods.Items) != 1 || pods.Items[0].Name != "mc-east" {
		t.Errorf("want only mc-east, got %d items", len(pods.Items))
	}
	if want := "unavailable clusters: bad"; pods.ResourceVersion != want {
		t.Errorf("resource version = %q, want %q", pods.ResourceVersion, want)
	}
}

func TestFanOutSkipsForbiddenAndMissingClustersSilently(t *testing.T) {
	c, _ := newTestClient(t, map[string]handlerFunc{
		"/clusters": reply(`{"items":[{"name":"east"},{"name":"locked"},{"name":"gone"}]}`),
		"/pods": byCluster(map[string]handlerFunc{
			"east":   reply(`{"items":[{"metadata":{"name":"mc-east","namespace":"games"}}]}`),
			"locked": fail(http.StatusForbidden, "no access"),
			"gone":   fail(http.StatusNotFound, "no such cluster"),
		}),
	})

	pods, err := c.ListPods(context.Background(), "games", "")
	if err != nil {
		t.Fatalf("ListPods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Errorf("want 1 pod, got %d", len(pods.Items))
	}
	if pods.ResourceVersion != "" {
		t.Errorf("403 and 404 clusters are skipped silently, want empty resource version, got %q", pods.ResourceVersion)
	}
}

func TestExplicitClusterIsASingleRequest(t *testing.T) {
	c, api := newTestClient(t, map[string]handlerFunc{
		"/clusters": reply(clustersReply),
		"/pods":     reply(`{"items":[{"metadata":{"name":"mc-east","namespace":"games"}}]}`),
	})

	pods, err := c.ListPods(clusterCtx("east"), "games", "")
	if err != nil {
		t.Fatalf("ListPods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Errorf("want 1 pod, got %d", len(pods.Items))
	}
	reqs := api.requests()
	if len(reqs) != 1 {
		t.Fatalf("want exactly 1 request, got %d: %+v", len(reqs), reqs)
	}
	if reqs[0].path != "/pods" || reqs[0].query.Get("cluster") != "east" {
		t.Errorf("request = %s cluster=%q, want /pods cluster=east", reqs[0].path, reqs[0].query.Get("cluster"))
	}
}

func TestFanOutReturnsClusterListError(t *testing.T) {
	c, _ := newTestClient(t, map[string]handlerFunc{
		"/clusters": fail(http.StatusInternalServerError, "registry down"),
	})
	if _, err := c.ListPods(context.Background(), "games", ""); err == nil {
		t.Fatal("want the /clusters error to be returned")
	}
}

// Ensure the fan-out context keeps the server target from the caller.
func TestFanOutKeepsServerTarget(t *testing.T) {
	c, api := newTestClient(t, map[string]handlerFunc{
		"/clusters": reply(`{"items":[{"name":"east"}]}`),
		"/servers/mc/pods": byCluster(map[string]handlerFunc{
			"east": reply(`{"items":[{"metadata":{"name":"mc-0","namespace":"games"}}]}`),
		}),
	})
	ctx := reader.WithTarget(context.Background(), reader.Target{Server: "mc"})
	pods, err := c.ListPods(ctx, "games", "")
	if err != nil {
		t.Fatalf("ListPods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Errorf("want 1 pod, got %d", len(pods.Items))
	}
	for _, r := range api.requests() {
		if r.path == "/pods" {
			t.Errorf("server target was dropped: request went to /pods")
		}
	}
}
