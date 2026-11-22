package panel

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/GameplanePanel/gameplane/mcp-server/internal/reader"
)

// clusterResult is one cluster's successful list result, in /clusters order.
type clusterResult[T any] struct {
	id  string
	val T
}

// fanOut runs fn once per registered cluster, at most c.fanout at a time, each
// under its own c.perCluster timeout. It returns the successful results and the
// IDs of clusters that failed for any reason other than 403 or 404; those two
// are skipped silently. An error from /clusters itself is returned.
func fanOut[T any](ctx context.Context, c *Client, fn func(context.Context) (T, error)) ([]clusterResult[T], []string, error) {
	var reg struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := c.get(ctx, "/clusters", nil, &reg); err != nil {
		return nil, nil, err
	}

	parent := reader.TargetFrom(ctx)
	ids := make([]string, 0, len(reg.Items))
	seen := make(map[string]bool, len(reg.Items))
	for _, it := range reg.Items {
		id := strings.TrimSpace(it.Name)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}

	width := c.fanout
	if width < 1 {
		width = 1
	}
	timeout := c.perCluster
	if timeout <= 0 {
		timeout = defaultPerCluster
	}

	results := make([]clusterResult[T], len(ids))
	errs := make([]error, len(ids))
	sem := make(chan struct{}, width)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			cctx = reader.WithTarget(cctx, reader.Target{Cluster: id, Server: parent.Server})
			val, err := fn(cctx)
			results[i] = clusterResult[T]{id: id, val: val}
			errs[i] = err
		}()
	}
	wg.Wait()

	var ok []clusterResult[T]
	var unavailable []string
	for i := range ids {
		switch {
		case errs[i] == nil:
			ok = append(ok, results[i])
		case isSkippedStatus(errs[i]):
		default:
			unavailable = append(unavailable, ids[i])
		}
	}
	return ok, unavailable, nil
}

// isSkippedStatus reports whether err is a 403 or 404 from the API.
func isSkippedStatus(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Status == http.StatusForbidden || apiErr.Status == http.StatusNotFound
}

// annotateCluster sets the cluster annotation on an object's annotation map.
func annotateCluster(anns map[string]string, id string) map[string]string {
	if anns == nil {
		anns = map[string]string{}
	}
	anns[clusterAnnotation] = id
	return anns
}

// unavailableNotice is the resource version that reports clusters whose
// results are missing from a merged list.
func unavailableNotice(ids []string) string {
	return "unavailable clusters: " + strings.Join(ids, ", ")
}

// listCRDAcross merges kind across every cluster, annotating each item.
func (c *Client) listCRDAcross(ctx context.Context, k kindInfo, ns, sel string) (*unstructured.UnstructuredList, error) {
	results, unavailable, err := fanOut(ctx, c, func(ctx context.Context) (*unstructured.UnstructuredList, error) {
		return c.listCRDLocal(ctx, k, ns, sel)
	})
	if err != nil {
		return nil, err
	}
	var items []unstructured.Unstructured
	for _, r := range results {
		for i := range r.val.Items {
			it := r.val.Items[i]
			it.SetAnnotations(annotateCluster(it.GetAnnotations(), r.id))
			items = append(items, it)
		}
	}
	merged := unstructuredList(k.kind, items)
	if len(unavailable) > 0 {
		merged.SetResourceVersion(unavailableNotice(unavailable))
	}
	return merged, nil
}

// listPodsAcross merges pods across every cluster, annotating each pod.
func (c *Client) listPodsAcross(ctx context.Context, ns, sel string) (*corev1.PodList, error) {
	results, unavailable, err := fanOut(ctx, c, func(ctx context.Context) (*corev1.PodList, error) {
		return c.listPodsLocal(ctx, ns, sel)
	})
	if err != nil {
		return nil, err
	}
	merged := &corev1.PodList{}
	for _, r := range results {
		for i := range r.val.Items {
			p := r.val.Items[i]
			p.Annotations = annotateCluster(p.Annotations, r.id)
			merged.Items = append(merged.Items, p)
		}
	}
	if len(unavailable) > 0 {
		merged.ResourceVersion = unavailableNotice(unavailable)
	}
	return merged, nil
}

// listEventsAcross merges events across every cluster, annotating each event.
func (c *Client) listEventsAcross(ctx context.Context, ns, fsel string) (*corev1.EventList, error) {
	results, unavailable, err := fanOut(ctx, c, func(ctx context.Context) (*corev1.EventList, error) {
		return c.listEventsLocal(ctx, ns, fsel)
	})
	if err != nil {
		return nil, err
	}
	merged := &corev1.EventList{}
	for _, r := range results {
		for i := range r.val.Items {
			e := r.val.Items[i]
			e.Annotations = annotateCluster(e.Annotations, r.id)
			merged.Items = append(merged.Items, e)
		}
	}
	if len(unavailable) > 0 {
		merged.ResourceVersion = unavailableNotice(unavailable)
	}
	return merged, nil
}
