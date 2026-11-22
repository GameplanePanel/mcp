// Package reader defines the read-only surface every MCP tool handler uses.
//
// Two implementations satisfy Reader: kube.Client (break-glass mode, reads the
// local cluster with the kubeconfig's own credentials) and panel.Client
// (per-user mode, reads through the Gameplane API as the MCP token's user).
// Both expose only List/Get-shaped methods, so a handler that holds a Reader
// cannot issue a mutating call through it.
package reader

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Target carries per-call selection that the tool arguments supply. It
// travels in the context so the Reader signatures stay unchanged.
type Target struct {
	Cluster string // registered cluster ID; empty = all permitted clusters (lists) or the local cluster (gets)
	Server  string // GameServer name the call is about; empty = none
}

type targetKey struct{}

// WithTarget returns a copy of ctx that carries t.
func WithTarget(ctx context.Context, t Target) context.Context {
	return context.WithValue(ctx, targetKey{}, t)
}

// TargetFrom returns the Target carried by ctx, or the zero Target.
func TargetFrom(ctx context.Context) Target {
	t, _ := ctx.Value(targetKey{}).(Target)
	return t
}

// Reader is implemented by kube.Client (break-glass) and panel.Client (per-user).
type Reader interface {
	ListCRD(ctx context.Context, kind, namespace, labelSelector string) (*unstructured.UnstructuredList, error)
	GetCRD(ctx context.Context, kind, namespace, name string) (*unstructured.Unstructured, error)
	ListPods(ctx context.Context, namespace, labelSelector string) (*corev1.PodList, error)
	GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error)
	ListEvents(ctx context.Context, namespace, fieldSelector string) (*corev1.EventList, error)
	PodLogs(ctx context.Context, namespace, pod, container string, tailLines int64, previous bool) (string, error)
}
