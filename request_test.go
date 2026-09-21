package httpkit

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestResolveURL(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		ref     string
		want    string
		wantErr string
	}{
		{name: "relative path", base: "https://api.example.com", ref: "users", want: "https://api.example.com/users"},
		{name: "rooted path", base: "https://api.example.com", ref: "/users", want: "https://api.example.com/users"},
		{
			// Concatenating by hand is where the double slash comes from, and
			// it is the reason this helper exists.
			name: "slash on both sides", base: "https://api.example.com/", ref: "/users",
			want: "https://api.example.com/users",
		},
		{name: "base with a path prefix", base: "https://api.example.com/v1", ref: "users", want: "https://api.example.com/v1/users"},
		{name: "query is kept", base: "https://api.example.com", ref: "users?limit=10", want: "https://api.example.com/users?limit=10"},
		{name: "fragment is kept", base: "https://api.example.com", ref: "docs#intro", want: "https://api.example.com/docs#intro"},
		{name: "empty ref is the base itself", base: "https://api.example.com/v1", ref: "", want: "https://api.example.com/v1"},
		{
			// A caller holding full URLs can route them through the same path
			// as relative ones -- which is what parser-kit's placeholder
			// BaseURL was working around.
			name: "absolute ref overrides the base", base: "https://api.example.com", ref: "https://other.example.org/raw",
			want: "https://other.example.org/raw",
		},
		{name: "absolute ref with no base", base: "", ref: "https://other.example.org/raw", want: "https://other.example.org/raw"},
		{name: "relative ref with no base", base: "", ref: "users", wantErr: "no BaseURL"},
		{name: "unparseable ref", base: "https://api.example.com", ref: "http://a b.com/\x7f", wantErr: "invalid URL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := mustClient(t, &Options{BaseURL: tt.base})
			got, err := client.ResolveURL(tt.ref)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ResolveURL(%q) = %q, want an error mentioning %q", tt.ref, got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveURL(%q): %v", tt.ref, err)
			}
			if got != tt.want {
				t.Errorf("ResolveURL(%q) = %q, want %q", tt.ref, got, tt.want)
			}
		})
	}
}

func TestNewRequest(t *testing.T) {
	t.Run("resolves against the base URL and carries the User-Agent", func(t *testing.T) {
		client := mustClient(t, &Options{BaseURL: "https://api.example.com/v1", UserAgent: "kit/2"})

		req, err := client.NewRequest(context.Background(), http.MethodPost, "users", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		if got := req.URL.String(); got != "https://api.example.com/v1/users" {
			t.Errorf("URL = %q, want %q", got, "https://api.example.com/v1/users")
		}
		if got := req.Header.Get("User-Agent"); got != "kit/2" {
			t.Errorf("User-Agent = %q, want %q", got, "kit/2")
		}
		if req.Method != http.MethodPost {
			t.Errorf("Method = %q, want POST", req.Method)
		}
	})

	t.Run("carries the caller's context", func(t *testing.T) {
		client := mustClient(t, &Options{BaseURL: "https://api.example.com"})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		req, err := client.NewRequest(ctx, http.MethodGet, "users", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		if req.Context().Err() == nil {
			t.Error("the request did not carry the cancelled context")
		}
	})

	t.Run("a replayable body gets a GetBody", func(t *testing.T) {
		// DoRequestWithRetry refuses to replay a body without GetBody, so a
		// helper that dropped it would quietly make every POST single-shot.
		client := mustClient(t, &Options{BaseURL: "https://api.example.com"})
		req, err := client.NewRequest(context.Background(), http.MethodPut, "users/1", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		if !canRetryBody(req) {
			t.Error("the request body cannot be replayed; NewRequest lost GetBody")
		}
	})

	t.Run("reports an unresolvable reference", func(t *testing.T) {
		client := mustClient(t, &Options{})
		if _, err := client.NewRequest(context.Background(), http.MethodGet, "users", nil); err == nil {
			t.Error("expected an error for a relative path with no BaseURL")
		}
	})

	t.Run("reports an invalid method", func(t *testing.T) {
		client := mustClient(t, &Options{BaseURL: "https://api.example.com"})
		if _, err := client.NewRequest(context.Background(), "GE T", "users", nil); err == nil {
			t.Error("expected an error for a method with a space in it")
		}
	})
}

// TestClientWithoutBaseURL is the regression test for BaseURL being required.
// A caller that builds absolute URLs itself -- parser-kit does -- had to pass
// a placeholder that nothing ever read.
func TestClientWithoutBaseURL(t *testing.T) {
	srv := newHeaderRecorder(t, nil)
	client := mustClient(t, &Options{UserAgent: "kit/2"})

	if client.GetBaseURL() != "" {
		t.Errorf("GetBaseURL() = %q, want empty", client.GetBaseURL())
	}

	req, err := client.NewRequest(context.Background(), http.MethodGet, srv.URL+"/data", nil)
	if err != nil {
		t.Fatalf("NewRequest with an absolute URL: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
