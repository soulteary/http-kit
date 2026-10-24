package outbound

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientValidatesEveryRequestWithoutPreflight(t *testing.T) {
	p := mustPolicy(t, Options{})
	p.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	var dials atomic.Int32
	p.dial = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("unexpected network access")
	}
	client := p.Client()
	defer client.CloseIdleConnections()
	for _, target := range []string{
		"http://webhook.example/hook",
		"https://user:secret@webhook.example/hook",
		"https://webhook.example/hook#fragment",
		"ftp://webhook.example/hook",
		"https://127.0.0.1/hook",
		"https://169.254.169.254/latest/meta-data",
	} {
		t.Run(target, func(t *testing.T) {
			body := &trackedBody{}
			req, err := http.NewRequest(http.MethodPost, target, body)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(req)
			if response != nil {
				response.Body.Close()
			}
			if !errors.Is(err, ErrBlocked) {
				t.Fatalf("Do returned %v, want ErrBlocked", err)
			}
			if body.reads.Load() != 0 || body.closes.Load() != 1 {
				t.Fatalf("denied body reads=%d closes=%d", body.reads.Load(), body.closes.Load())
			}
		})
	}
	if dials.Load() != 0 {
		t.Fatalf("denied requests caused %d dials", dials.Load())
	}
}

type trackedBody struct{ reads, closes atomic.Int32 }

func (b *trackedBody) Read([]byte) (int, error) { b.reads.Add(1); return 0, io.EOF }
func (b *trackedBody) Close() error             { b.closes.Add(1); return nil }

func TestClientRejectsURLFieldsHiddenByString(t *testing.T) {
	p := mustPolicy(t, Options{AllowedHosts: []string{"127.0.0.1"}})
	var dials atomic.Int32
	var lookups atomic.Int32
	p.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		lookups.Add(1)
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	p.dial = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("unexpected dial")
	}
	client := p.Client()
	defer client.CloseIdleConnections()
	for _, test := range []struct {
		name   string
		target *url.URL
	}{
		{name: "opaque-hides-userinfo", target: &url.URL{Scheme: "http", Host: "127.0.0.1", Opaque: "//127.0.0.1/hook", User: url.UserPassword("user", "secret")}},
		{name: "opaque-without-userinfo", target: &url.URL{Scheme: "http", Host: "127.0.0.1", Opaque: "//127.0.0.1/hook"}},
		{name: "raw-fragment-without-fragment", target: &url.URL{Scheme: "http", Host: "127.0.0.1", Path: "/hook", RawFragment: "hidden"}},
		{name: "path-cannot-supply-missing-host", target: &url.URL{Scheme: "http", Path: "other.example/hook"}},
		{name: "scheme-must-match-original-request", target: &url.URL{Scheme: "HTTP", Host: "127.0.0.1", Path: "/hook"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &trackedBody{}
			// Construct the request directly: URL.String hides these URL fields,
			// so reparsing a serialized URL cannot prove this request is safe.
			req := &http.Request{Method: http.MethodPost, URL: test.target, Header: make(http.Header), Body: body}
			response, err := client.Do(req)
			if response != nil {
				response.Body.Close()
			}
			if !errors.Is(err, ErrBlocked) || dials.Load() != 0 || lookups.Load() != 0 {
				t.Fatalf("hidden URL field reached transport: err=%v dials=%d lookups=%d", err, dials.Load(), lookups.Load())
			}
			if body.reads.Load() != 0 || body.closes.Load() != 1 {
				t.Fatalf("denied body reads=%d closes=%d", body.reads.Load(), body.closes.Load())
			}
		})
	}
}

func TestClientExplicitLocalhostAndOriginalRequest(t *testing.T) {
	var requests atomic.Int32
	const requestURI = "/hook%2Fsegment?value=1&token=a%2Bb&value=2"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("X-Caller") != "original" || r.RequestURI != requestURI {
			t.Errorf("request changed at server: %s %v", r.URL, r.Header)
		}
		_, _ = io.WriteString(w, "delivered")
	}))
	defer server.Close()
	p := mustPolicy(t, Options{AllowedHosts: []string{"localhost"}})
	p.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	var dials atomic.Int32
	p.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, "http://localhost:"+port+requestURI, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Caller", "original")
	originalURL, originalContext, originalHeader := *req.URL, req.Context(), req.Header.Clone()
	client := p.Client()
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(data) != "delivered" || requests.Load() != 1 {
		t.Fatalf("response=%q err=%v requests=%d", data, err, requests.Load())
	}
	if !reflect.DeepEqual(*req.URL, originalURL) || req.Context() != originalContext || !reflect.DeepEqual(req.Header, originalHeader) {
		t.Fatal("client policy mutated the caller's request")
	}
	client.CloseIdleConnections()
	response, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if err != nil || dials.Load() != 2 || requests.Load() != 2 {
		t.Fatalf("CloseIdleConnections did not release pooled connection: err=%v dials=%d requests=%d", err, dials.Load(), requests.Load())
	}
}

func TestClientDoesNotFollowRedirect(t *testing.T) {
	var destinationHits atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/redirected", http.StatusFound)
	}))
	defer origin.Close()
	client := mustPolicy(t, Options{AllowedHosts: []string{"127.0.0.1"}}).Client()
	defer client.CloseIdleConnections()
	response, err := client.Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound || response.Header.Get("Location") != destination.URL+"/redirected" {
		t.Fatalf("redirect response lost: %d %v", response.StatusCode, response.Header)
	}
	if destinationHits.Load() != 0 {
		t.Fatal("client followed a redirect to another trusted URL")
	}
}

func TestClientIgnoresEnvironmentProxy(t *testing.T) {
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
		_, _ = io.WriteString(w, "proxy")
	}))
	defer proxy.Close()
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(name, proxy.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	var originHits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		originHits.Add(1)
		_, _ = io.WriteString(w, "origin")
	}))
	defer origin.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(origin.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	p := mustPolicy(t, Options{AllowedHosts: []string{"proxy-bypass.example"}})
	p.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	client := p.Client()
	defer client.CloseIdleConnections()
	response, err := client.Get("http://proxy-bypass.example:" + port + "/hook")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(data) != "origin" || originHits.Load() != 1 || proxyHits.Load() != 0 {
		t.Fatalf("body=%q err=%v origin=%d proxy=%d", data, err, originHits.Load(), proxyHits.Load())
	}
}

func TestClientRechecksDNSBeforeReusingConnection(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	serverAddress := strings.TrimPrefix(server.URL, "http://")
	_, port, err := net.SplitHostPort(serverAddress)
	if err != nil {
		t.Fatal(err)
	}
	p := mustPolicy(t, Options{AllowedCIDRs: []string{"8.8.8.8/32"}})
	var rebound atomic.Bool
	var lookups, dials atomic.Int32
	p.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		lookups.Add(1)
		if rebound.Load() {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	p.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		if address != net.JoinHostPort("8.8.8.8", port) {
			t.Errorf("dial was not pinned to validated numeric destination: %q", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, serverAddress)
	}
	client := p.Client()
	defer client.CloseIdleConnections()
	target := "http://rebind.example:" + port + "/hook"
	for range 2 {
		response, err := client.Get(target)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if dials.Load() != 1 || requests.Load() != 2 || lookups.Load() < 3 {
		t.Fatalf("keepalive path dials=%d requests=%d lookups=%d", dials.Load(), requests.Load(), lookups.Load())
	}
	before := lookups.Load()
	rebound.Store(true)
	response, err := client.Get(target)
	if response != nil {
		response.Body.Close()
	}
	if !errors.Is(err, ErrBlocked) || lookups.Load() <= before || requests.Load() != 2 || dials.Load() != 1 {
		t.Fatalf("rebound request err=%v requests=%d dials=%d lookups=%d", err, requests.Load(), dials.Load(), lookups.Load())
	}
}

func TestClientRebindingBetweenValidationAndDial(t *testing.T) {
	for _, test := range []struct {
		name, scheme, initial, rebound string
		options                        Options
	}{
		{name: "https-public-to-private", scheme: "https", initial: "8.8.8.8", rebound: "127.0.0.1"},
		{name: "http-trusted-to-untrusted-public", scheme: "http", initial: "10.1.2.3", rebound: "8.8.8.8", options: Options{AllowedCIDRs: []string{"10.0.0.0/8"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := mustPolicy(t, test.options)
			var lookups, dials atomic.Int32
			p.lookup = func(context.Context, string) ([]net.IPAddr, error) {
				if lookups.Add(1) == 1 {
					return []net.IPAddr{{IP: net.ParseIP(test.initial)}}, nil
				}
				return []net.IPAddr{{IP: net.ParseIP(test.rebound)}}, nil
			}
			p.dial = func(context.Context, string, string) (net.Conn, error) {
				dials.Add(1)
				return nil, errors.New("unexpected dial")
			}
			client := p.Client()
			defer client.CloseIdleConnections()
			response, err := client.Get(test.scheme + "://rebind.example/hook")
			if response != nil {
				response.Body.Close()
			}
			if !errors.Is(err, ErrBlocked) || lookups.Load() < 2 || dials.Load() != 0 {
				t.Fatalf("rebind err=%v lookups=%d dials=%d", err, lookups.Load(), dials.Load())
			}
		})
	}
}

func TestClientDNSCancellationReachesCaller(t *testing.T) {
	p := mustPolicy(t, Options{})
	entered := make(chan struct{})
	p.lookup = func(ctx context.Context, _ string) ([]net.IPAddr, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://cancel.example/hook", nil)
	if err != nil {
		t.Fatal(err)
	}
	client := p.Client()
	defer client.CloseIdleConnections()
	done := make(chan error, 1)
	go func() { _, err := client.Do(req); done <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("DNS lookup did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not stop after cancellation")
	}
}

func TestPolicyDialTriesOnlyValidatedNumericAddresses(t *testing.T) {
	p := mustPolicy(t, Options{})
	p.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("2606:4700:4700::1111")}}, nil
	}
	var addresses []string
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	p.dial = func(_ context.Context, _, address string) (net.Conn, error) {
		addresses = append(addresses, address)
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := netip.ParseAddr(host); err != nil {
			t.Fatal("dial delegated hostname resolution", address)
		}
		if len(addresses) == 1 {
			return nil, errors.New("first address unavailable")
		}
		return left, nil
	}
	conn, err := p.dialContext(context.Background(), "tcp", "fallback.example:443")
	if err != nil || conn != left {
		t.Fatalf("fallback result %v %v", conn, err)
	}
	want := []string{"8.8.8.8:443", "[2606:4700:4700::1111]:443"}
	if !reflect.DeepEqual(addresses, want) {
		t.Fatalf("numeric destinations=%v want %v", addresses, want)
	}
}

// Arbitrary credential and fragment strings must never be normalized away by
// the HTTP client before its policy is applied. The resolver is deterministic
// and the dial hook never opens a socket, including during fuzzing.
func FuzzClientRejectsUserinfoAndFragment(f *testing.F) {
	for _, seed := range []string{"", "secret", "@/#?%", "\x00\n", "用户", "localhost:443"} {
		f.Add(seed)
	}
	p, err := NewPolicy(Options{AllowedHosts: []string{"127.0.0.1"}})
	if err != nil {
		f.Fatal(err)
	}
	p.dial = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("unexpected dial")
	}
	client := p.Client()
	f.Cleanup(client.CloseIdleConnections)
	f.Fuzz(func(t *testing.T, value string) {
		for _, u := range []*url.URL{
			{Scheme: "http", Host: "127.0.0.1", User: url.UserPassword(value, value), Path: "/hook"},
			{Scheme: "http", Host: "127.0.0.1", Fragment: "fragment" + value, Path: "/hook"},
		} {
			req := &http.Request{Method: http.MethodGet, URL: u, Header: make(http.Header)}
			response, err := client.Do(req)
			if response != nil {
				response.Body.Close()
			}
			if !errors.Is(err, ErrBlocked) {
				t.Fatalf("request with userinfo or fragment escaped policy: %v", err)
			}
		}
	})
}
