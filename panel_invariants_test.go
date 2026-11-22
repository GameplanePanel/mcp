package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/GameplanePanel/gameplane/mcp-server/internal/kube"
	"github.com/GameplanePanel/gameplane/mcp-server/internal/panel"
	"github.com/GameplanePanel/gameplane/mcp-server/internal/reader"
)

// panelMutatingVerbs is the denylist for the per-user client's exported
// method set. The per-user client only ever reads, so none of these may
// appear as a method-name prefix.
var panelMutatingVerbs = []string{"Create", "Update", "Delete", "Patch", "Apply", "Post", "Put"}

// TestPanelClientHasNoMutatingMethods is the per-user counterpart of
// TestClientHasNoMutatingMethods: the panel client's only request builder is
// get, which issues GET (internal/panel/source_test.go enforces that), and
// its exported surface must not look like a write either.
func TestPanelClientHasNoMutatingMethods(t *testing.T) {
	typ := reflect.TypeOf(&panel.Client{})
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		for _, verb := range panelMutatingVerbs {
			if strings.HasPrefix(name, verb) {
				t.Errorf("panel.Client has mutating-looking method %q (matches verb %q)", name, verb)
			}
		}
	}
}

// TestReadersImplementInterface checks both reader implementations at compile
// time and at run time, so a signature drift in either shows up here.
func TestReadersImplementInterface(t *testing.T) {
	var _ reader.Reader = (*kube.Client)(nil)
	var _ reader.Reader = (*panel.Client)(nil)

	readerType := reflect.TypeOf((*reader.Reader)(nil)).Elem()
	for _, typ := range []reflect.Type{reflect.TypeOf(&kube.Client{}), reflect.TypeOf(&panel.Client{})} {
		if !typ.Implements(readerType) {
			t.Errorf("%s does not implement reader.Reader", typ)
		}
	}
}
