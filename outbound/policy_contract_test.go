package outbound

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestZeroPolicyDefaultsAndNilFailsClosed(t *testing.T) {
	var zero Policy
	if err := zero.Validate(context.Background(), "https://8.8.8.8/hook"); err != nil {
		t.Fatalf("zero policy rejected public HTTPS: %v", err)
	}
	for _, target := range []string{"http://8.8.8.8/hook", "https://127.0.0.1/hook"} {
		if err := zero.Validate(context.Background(), target); !errors.Is(err, ErrBlocked) {
			t.Fatalf("zero policy accepted %q: %v", target, err)
		}
	}
	var absent *Policy
	if err := absent.Validate(context.Background(), "https://8.8.8.8/hook"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("nil policy did not fail closed: %v", err)
	}
	client := absent.Client()
	defer client.CloseIdleConnections()
	response, err := client.Get("https://8.8.8.8/hook")
	if response != nil {
		_ = response.Body.Close()
	}
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("nil client policy did not fail closed: %v", err)
	}
}

func TestValidateHonorsAlreadyCanceledContextForIPLiteral(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := mustPolicy(t, Options{})
	if err := p.Validate(ctx, "https://8.8.8.8/hook"); !errors.Is(err, context.Canceled) {
		t.Fatalf("numeric address ignored cancellation: %v", err)
	}
}

func TestOptionsAreCopiedAndExactHostTrustIsNormalized(t *testing.T) {
	options := Options{AllowedHosts: []string{"HOOK.EXAMPLE"}, AllowedCIDRs: []string{"10.2.3.4/16"}}
	p := mustPolicy(t, options)
	options.AllowedHosts[0] = "other.example"
	options.AllowedCIDRs[0] = "192.168.0.0/16"
	p.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	for _, target := range []string{"http://hook.example/hook", "http://HOOK.EXAMPLE/hook", "http://10.2.4.5/hook"} {
		if err := p.Validate(context.Background(), target); err != nil {
			t.Fatalf("copied trust was lost for %q: %v", target, err)
		}
	}
	for _, target := range []string{"http://other.example/hook", "http://sub.hook.example/hook", "http://192.168.0.1/hook"} {
		if err := p.Validate(context.Background(), target); !errors.Is(err, ErrBlocked) {
			t.Fatalf("trust unexpectedly expanded to %q: %v", target, err)
		}
	}
	// Exceptions do not become an outbound allowlist restricting other public HTTPS.
	if err := p.Validate(context.Background(), "https://8.8.4.4/hook"); err != nil {
		t.Fatal(err)
	}
}

func TestDNSFailuresAreClassifiedWithoutLeakingResolverDetails(t *testing.T) {
	for _, test := range []struct {
		name string
		ips  []net.IPAddr
		err  error
		want error
	}{
		{name: "resolver-error", err: errors.New("private resolver secret at 10.0.0.53"), want: ErrDNSResolution},
		{name: "empty-answers", want: ErrNoAddresses},
		{name: "malformed-address", ips: []net.IPAddr{{IP: net.IP{1, 2, 3}}}, want: ErrBlocked},
		{name: "scoped-address", ips: []net.IPAddr{{IP: net.ParseIP("2606:4700:4700::1111"), Zone: "eth0"}}, want: ErrBlocked},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := mustPolicy(t, Options{})
			p.lookup = func(context.Context, string) ([]net.IPAddr, error) { return test.ips, test.err }
			err := p.Validate(context.Background(), "https://sensitive.internal/hook?token=secret")
			if !errors.Is(err, test.want) {
				t.Fatalf("got %v want %v", err, test.want)
			}
			for _, sensitive := range []string{"sensitive.internal", "token=secret", "10.0.0.53", "private resolver"} {
				if strings.Contains(err.Error(), sensitive) {
					t.Fatalf("policy error exposed resolver or URL details: %v", err)
				}
			}
		})
	}
}

func TestConfigurationRejectsMalformedOrAmbiguousEntries(t *testing.T) {
	for _, host := range []string{"", "localhost.", "a..example", "-a.example", "a-.example", "a_b.example", "[::1]", "fe80::1%eth0", "user@example.com", strings.Repeat("a", 64) + ".example"} {
		if _, err := NewPolicy(Options{AllowedHosts: []string{host}}); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("allowed host %q returned %v", host, err)
		}
	}
	for _, cidr := range []string{"127.0.0.1", "10.0.0.0/33", "::ffff:127.0.0.0/120", "fe80::1%eth0/64"} {
		if _, err := NewPolicy(Options{AllowedCIDRs: []string{cidr}}); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("allowed CIDR %q returned %v", cidr, err)
		}
	}
}
