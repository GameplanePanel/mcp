package panel

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The tripwire reads this package's own non-test sources and fails on any
// request shape other than a GET issued from get.
var (
	httpMethodRef  = regexp.MustCompile(`http\.Method\w*`)
	bannedVerbCall = regexp.MustCompile(`\.(Post|PostForm|Put|Patch|Delete|Head)\(`)
	doCall         = regexp.MustCompile(`\.Do\(`)
	newRequestCall = regexp.MustCompile(`http\.NewRequest\(`)
	funcDecl       = regexp.MustCompile(`^func (?:\([^)]*\)\s*)?(\w+)`)
)

func TestPanelSourceOnlyIssuesGET(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		current := ""
		for i, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "func ") {
				if m := funcDecl.FindStringSubmatch(line); m != nil {
					current = m[1]
				}
			}
			where := name + ":" + strconv.Itoa(i+1)
			for _, ref := range httpMethodRef.FindAllString(line, -1) {
				if ref != "http.MethodGet" {
					t.Errorf("%s uses %s; the panel client may only issue GET", where, ref)
				}
			}
			if newRequestCall.MatchString(line) {
				t.Errorf("%s calls http.NewRequest; use http.NewRequestWithContext in get", where)
			}
			if m := bannedVerbCall.FindString(line); m != "" {
				t.Errorf("%s calls %s; the panel client may only issue GET", where, m)
			}
			if doCall.MatchString(line) && current != "get" {
				t.Errorf("%s calls Do outside get (in %s)", where, current)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test sources found; the tripwire checked nothing")
	}
}
