package panel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

// TestClientDoesNotFollowRedirects serves a 302 from a GET route and checks
// the client returns that 302 as an API error instead of following it to the
// redirect target.
func TestClientDoesNotFollowRedirects(t *testing.T) {
	var targetHit atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/servers", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, r *http.Request) {
		targetHit.Store(true)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := New(srv.URL, testToken, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var body string
	err = c.get(context.Background(), "/servers", nil, &body)
	if err == nil {
		t.Fatal("get succeeded on a 302; the redirect must not be followed to a 200")
	}
	if got := statusOf(err); got != http.StatusFound {
		t.Errorf("status = %d, want %d (the redirect returned as-is)", got, http.StatusFound)
	}
	if targetHit.Load() {
		t.Error("redirect target was requested; the client must not follow redirects")
	}
}

func TestKeepNewestLog(t *testing.T) {
	t.Run("oversized keeps newest bytes behind the notice", func(t *testing.T) {
		in := strings.Repeat("a", 300<<10) + "END"
		got := keepNewestLog(in)
		if !strings.HasPrefix(got, truncatedLogNotice) {
			t.Fatalf("output does not start with the truncation notice: %q", got[:min(len(got), 80)])
		}
		if limit := maxLogBytes + len(truncatedLogNotice); len(got) > limit {
			t.Errorf("len = %d, want <= %d", len(got), limit)
		}
		if !strings.HasSuffix(got, "END") {
			t.Error("output does not end with the newest bytes")
		}
	})

	t.Run("cut lands on a rune boundary", func(t *testing.T) {
		// Three-byte runes put the naive cut mid-rune, so the cut must move forward.
		in := strings.Repeat("€", 150<<10)
		got := keepNewestLog(in)
		payload := strings.TrimPrefix(got, truncatedLogNotice)
		if !utf8.ValidString(payload) {
			t.Error("truncated output splits a multi-byte rune")
		}
		if len(got) > maxLogBytes+len(truncatedLogNotice) {
			t.Errorf("len = %d, want <= %d", len(got), maxLogBytes+len(truncatedLogNotice))
		}
	})

	t.Run("short input unchanged", func(t *testing.T) {
		in := "line 1\nline 2\n"
		if got := keepNewestLog(in); got != in {
			t.Errorf("keepNewestLog(%q) = %q, want it unchanged", in, got)
		}
	})
}
