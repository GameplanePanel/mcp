package kube

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/GameplanePanel/gameplane/mcp-server/internal/reader"
)

// seededAllowlistClient builds a Client over fake clientsets seeded with one
// pod, one event and one GameServer in each of "games", "games-2" and
// "secret", plus a cluster-scoped GameTemplate. It is restricted to allow.
func seededAllowlistClient(allow []string) *Client {
	scheme := NewScheme()
	typed := k8sfake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "mc-0", Namespace: "games"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "mc-1", Namespace: "games-2"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "secret-0", Namespace: "secret"}},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "ev-a", Namespace: "games"}, Reason: "Started"},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "ev-b", Namespace: "games-2"}, Reason: "Started"},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "ev-s", Namespace: "secret"}, Reason: "Started"},
	)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKindMap(),
		newUnstructuredCRD("GameServer", "games", "mc", nil),
		newUnstructuredCRD("GameServer", "games-2", "val", nil),
		newUnstructuredCRD("GameServer", "secret", "hidden", nil),
		newUnstructuredCRD("GameTemplate", "", "mc-java", nil),
	)
	return NewFrom(typed, dyn, scheme).WithNamespaces(allow)
}

// failListIn returns a reactor that fails list calls against namespace ns
// with err and lets every other call through to the fake's tracker.
func failListIn(ns string, err error) k8stesting.ReactionFunc {
	return func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == ns {
			return true, nil, err
		}
		return false, nil, nil
	}
}

func podNames(list *corev1.PodList) []string {
	out := make([]string, 0, len(list.Items))
	for _, p := range list.Items {
		out = append(out, p.Name)
	}
	slices.Sort(out)
	return out
}

func eventNames(list *corev1.EventList) []string {
	out := make([]string, 0, len(list.Items))
	for _, e := range list.Items {
		out = append(out, e.Name)
	}
	slices.Sort(out)
	return out
}

func crdNames(list *unstructured.UnstructuredList) []string {
	out := make([]string, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, it.GetName())
	}
	slices.Sort(out)
	return out
}

func TestAllowlistRejectsOutOfScopeNamespace(t *testing.T) {
	ctx := context.Background()
	c := seededAllowlistClient([]string{"games", "games-2"})
	calls := []struct {
		name string
		call func() error
	}{
		{"ListCRD", func() error { _, err := c.ListCRD(ctx, "GameServer", "secret", ""); return err }},
		{"GetCRD", func() error { _, err := c.GetCRD(ctx, "GameServer", "secret", "hidden"); return err }},
		{"ListPods", func() error { _, err := c.ListPods(ctx, "secret", ""); return err }},
		{"GetPod", func() error { _, err := c.GetPod(ctx, "secret", "secret-0"); return err }},
		{"ListEvents", func() error { _, err := c.ListEvents(ctx, "secret", ""); return err }},
		{"PodLogs", func() error { _, err := c.PodLogs(ctx, "secret", "secret-0", "", 0, false); return err }},
	}
	for _, tt := range calls {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, ErrNamespaceNotInScope) {
				t.Fatalf("want ErrNamespaceNotInScope, got %v", err)
			}
		})
	}
}

func TestAllowlistRequiresNamespaceForSingleObjectReads(t *testing.T) {
	ctx := context.Background()
	c := seededAllowlistClient([]string{"games"})

	if _, err := c.GetPod(ctx, "", "mc-0"); !errors.Is(err, errNamespaceRequired) {
		t.Errorf("GetPod with empty namespace: want errNamespaceRequired, got %v", err)
	}
	if _, err := c.PodLogs(ctx, "", "mc-0", "", 0, false); !errors.Is(err, errNamespaceRequired) {
		t.Errorf("PodLogs with empty namespace: want errNamespaceRequired, got %v", err)
	}
}

func TestAllowlistAllowsInScopeReads(t *testing.T) {
	ctx := context.Background()
	c := seededAllowlistClient([]string{"games", "games-2"})

	pod, err := c.GetPod(ctx, "games", "mc-0")
	if err != nil {
		t.Fatalf("GetPod in scope: %v", err)
	}
	if pod.Name != "mc-0" {
		t.Errorf("want pod mc-0, got %q", pod.Name)
	}

	obj, err := c.GetCRD(ctx, "GameServer", "games-2", "val")
	if err != nil {
		t.Fatalf("GetCRD in scope: %v", err)
	}
	if obj.GetName() != "val" {
		t.Errorf("want GameServer val, got %q", obj.GetName())
	}

	// Cluster-scoped kinds ignore the namespace argument, so the allow-list
	// does not apply to them.
	tmpl, err := c.ListCRD(ctx, "GameTemplate", "secret", "")
	if err != nil {
		t.Fatalf("ListCRD cluster-scoped kind: %v", err)
	}
	if got := crdNames(tmpl); !slices.Equal(got, []string{"mc-java"}) {
		t.Errorf("want [mc-java], got %v", got)
	}
}

func TestAllowlistMergesOnlyAllowedNamespaces(t *testing.T) {
	ctx := context.Background()
	c := seededAllowlistClient([]string{"games", "games-2"})

	pods, err := c.ListPods(ctx, "", "")
	if err != nil {
		t.Fatalf("ListPods: %v", err)
	}
	if got := podNames(pods); !slices.Equal(got, []string{"mc-0", "mc-1"}) {
		t.Errorf("pods: want [mc-0 mc-1], got %v", got)
	}
	if pods.ResourceVersion != "" {
		t.Errorf("pods: nothing was skipped, want empty resource version, got %q", pods.ResourceVersion)
	}

	events, err := c.ListEvents(ctx, "", "")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if got := eventNames(events); !slices.Equal(got, []string{"ev-a", "ev-b"}) {
		t.Errorf("events: want [ev-a ev-b], got %v", got)
	}

	servers, err := c.ListCRD(ctx, "GameServer", "", "")
	if err != nil {
		t.Fatalf("ListCRD GameServer: %v", err)
	}
	if got := crdNames(servers); !slices.Equal(got, []string{"mc", "val"}) {
		t.Errorf("GameServers: want [mc val], got %v", got)
	}
}

func TestAllowlistSkipsMissingAndForbiddenNamespaces(t *testing.T) {
	ctx := context.Background()
	scheme := NewScheme()
	typed := k8sfake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "mc-0", Namespace: "games"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "mc-1", Namespace: "games-2"}},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "ev-a", Namespace: "games"}, Reason: "Started"},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "ev-b", Namespace: "games-2"}, Reason: "Started"},
	)
	typed.PrependReactor("list", "pods", failListIn("locked",
		apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("denied"))))
	typed.PrependReactor("list", "events", failListIn("locked",
		apierrors.NewNotFound(schema.GroupResource{Resource: "events"}, "")))

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKindMap(),
		newUnstructuredCRD("GameServer", "games", "mc", nil),
		newUnstructuredCRD("GameServer", "games-2", "val", nil),
	)
	dyn.PrependReactor("list", "gameservers", failListIn("locked",
		apierrors.NewNotFound(schema.GroupResource{Resource: "gameservers"}, "")))

	c := NewFrom(typed, dyn, scheme).WithNamespaces([]string{"games", "locked", "games-2"})

	pods, err := c.ListPods(ctx, "", "")
	if err != nil {
		t.Fatalf("ListPods: %v", err)
	}
	if got := podNames(pods); !slices.Equal(got, []string{"mc-0", "mc-1"}) {
		t.Errorf("pods: want [mc-0 mc-1], got %v", got)
	}
	if want := "skipped namespaces: locked"; pods.ResourceVersion != want {
		t.Errorf("pods resource version: want %q, got %q", want, pods.ResourceVersion)
	}

	events, err := c.ListEvents(ctx, "", "")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if got := eventNames(events); !slices.Equal(got, []string{"ev-a", "ev-b"}) {
		t.Errorf("events: want [ev-a ev-b], got %v", got)
	}
	if events.ResourceVersion != "skipped namespaces: locked" {
		t.Errorf("events resource version: want skipped note, got %q", events.ResourceVersion)
	}

	servers, err := c.ListCRD(ctx, "GameServer", "", "")
	if err != nil {
		t.Fatalf("ListCRD GameServer: %v", err)
	}
	if got := crdNames(servers); !slices.Equal(got, []string{"mc", "val"}) {
		t.Errorf("GameServers: want [mc val], got %v", got)
	}
	if servers.GetResourceVersion() != "skipped namespaces: locked" {
		t.Errorf("GameServer resource version: want skipped note, got %q", servers.GetResourceVersion())
	}
}

func TestAllowlistReturnsUnexpectedListErrors(t *testing.T) {
	scheme := NewScheme()
	typed := k8sfake.NewSimpleClientset()
	typed.PrependReactor("list", "pods", failListIn("boom",
		apierrors.NewInternalError(errors.New("apiserver exploded"))))
	c := NewFrom(typed, dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKindMap()), scheme).
		WithNamespaces([]string{"games", "boom"})

	_, err := c.ListPods(context.Background(), "", "")
	if err == nil {
		t.Fatal("want error from a non-skippable list failure")
	}
	if errors.Is(err, ErrNamespaceNotInScope) {
		t.Errorf("unexpected scope error: %v", err)
	}
}

func TestClusterSelectionRejectedInBreakGlass(t *testing.T) {
	ctx := reader.WithTarget(context.Background(), reader.Target{Cluster: "prod"})
	c := seededAllowlistClient([]string{"games"})

	calls := []struct {
		name string
		call func() error
	}{
		{"ListCRD", func() error { _, err := c.ListCRD(ctx, "GameServer", "", ""); return err }},
		{"GetCRD", func() error { _, err := c.GetCRD(ctx, "GameServer", "games", "mc"); return err }},
		{"ListPods", func() error { _, err := c.ListPods(ctx, "", ""); return err }},
		{"GetPod", func() error { _, err := c.GetPod(ctx, "games", "mc-0"); return err }},
		{"ListEvents", func() error { _, err := c.ListEvents(ctx, "", ""); return err }},
		{"PodLogs", func() error { _, err := c.PodLogs(ctx, "games", "mc-0", "", 0, false); return err }},
	}
	for _, tt := range calls {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, errClusterNotAvailable) {
				t.Fatalf("want errClusterNotAvailable, got %v", err)
			}
		})
	}
}

func TestServerTargetIgnoredInBreakGlass(t *testing.T) {
	ctx := reader.WithTarget(context.Background(), reader.Target{Server: "mc"})
	c := seededAllowlistClient([]string{"games"})

	pods, err := c.ListPods(ctx, "games", "")
	if err != nil {
		t.Fatalf("ListPods with a server target: %v", err)
	}
	if got := podNames(pods); !slices.Equal(got, []string{"mc-0"}) {
		t.Errorf("want [mc-0], got %v", got)
	}
}

func TestParseNamespaces(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{" a, ,b,a ", []string{"a", "b"}},
		{"", nil},
		{" , ,", nil},
		{"x", []string{"x"}},
		{"games,games-2", []string{"games", "games-2"}},
	}
	for _, tt := range cases {
		got := ParseNamespaces(tt.in)
		if !slices.Equal(got, tt.want) {
			t.Errorf("ParseNamespaces(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestNoNamespacesListsAcrossAllNamespaces(t *testing.T) {
	ctx := context.Background()
	c := seededAllowlistClient(ParseNamespaces(""))

	pods, err := c.ListPods(ctx, "", "")
	if err != nil {
		t.Fatalf("ListPods: %v", err)
	}
	if got := podNames(pods); !slices.Equal(got, []string{"mc-0", "mc-1", "secret-0"}) {
		t.Errorf("want every pod, got %v", got)
	}

	events, err := c.ListEvents(ctx, "", "")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if got := eventNames(events); !slices.Equal(got, []string{"ev-a", "ev-b", "ev-s"}) {
		t.Errorf("want every event, got %v", got)
	}

	servers, err := c.ListCRD(ctx, "GameServer", "", "")
	if err != nil {
		t.Fatalf("ListCRD GameServer: %v", err)
	}
	if got := crdNames(servers); !slices.Equal(got, []string{"hidden", "mc", "val"}) {
		t.Errorf("want every GameServer, got %v", got)
	}
	// The fake client sets its own resource version; only the skip note matters.
	if strings.HasPrefix(servers.GetResourceVersion(), "skipped namespaces") {
		t.Errorf("unrestricted list must not carry a skip note, got %q", servers.GetResourceVersion())
	}
}
