package outbound

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClientPinsIPWithoutBypassingTLSIdentity(t *testing.T) {
	type receivedRequest struct{ serverName, host, requestURI string }
	received := make(chan receivedRequest, 1)
	var handlerCalls atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalls.Add(1)
		received <- receivedRequest{r.TLS.ServerName, r.Host, r.RequestURI}
		_, _ = io.WriteString(w, "verified")
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	certificate := server.Certificate()
	if len(certificate.DNSNames) == 0 {
		t.Fatal("test server certificate has no DNS identity")
	}
	hostname := certificate.DNSNames[0]
	serverAddress := strings.TrimPrefix(server.URL, "https://")
	_, port, err := net.SplitHostPort(serverAddress)
	if err != nil {
		t.Fatal(err)
	}
	const requestURI = "/hook%2Fsegment?token=x%2Fy+z&value=1&value=2"
	p := mustPolicy(t, Options{})
	p.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	var dials atomic.Int32
	p.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		if address != net.JoinHostPort("8.8.8.8", port) {
			t.Errorf("TLS dial was not pinned to the validated IP: %q", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, serverAddress)
	}
	target := "https://" + net.JoinHostPort(hostname, port) + requestURI
	untrusted := p.Client()
	defer untrusted.CloseIdleConnections()
	response, err := untrusted.Get(target)
	if response != nil {
		response.Body.Close()
	}
	var authorityError x509.UnknownAuthorityError
	if !errors.As(err, &authorityError) || handlerCalls.Load() != 0 {
		t.Fatalf("untrusted certificate was accepted: err=%v handlerCalls=%d", err, handlerCalls.Load())
	}

	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	trusted := p.Client()
	defer trusted.CloseIdleConnections()
	// Only the test CA is added. ServerName and certificate verification retain
	// their normal transport behavior while DialContext connects to a pinned IP.
	trusted.Transport.(*policyTransport).transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	response, err = trusted.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "verified" {
		t.Fatalf("TLS response=%q err=%v", body, err)
	}
	got := <-received
	want := receivedRequest{hostname, net.JoinHostPort(hostname, port), requestURI}
	if got != want {
		t.Fatalf("IP pinning changed TLS/HTTP target identity: got=%+v want=%+v", got, want)
	}

	response, err = trusted.Get("https://" + net.JoinHostPort("wrong-certificate.example", port) + requestURI)
	if response != nil {
		response.Body.Close()
	}
	var hostnameError x509.HostnameError
	if !errors.As(err, &hostnameError) || handlerCalls.Load() != 1 || dials.Load() != 3 {
		t.Fatalf("trusted CA bypassed hostname verification: err=%v handlerCalls=%d dials=%d", err, handlerCalls.Load(), dials.Load())
	}
}
