package gopay

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestConfiguredHTTPClientNormalizesBareHTTPProxies(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantHost string
		wantUser string
		wantPass string
	}{
		{name: "host and port", input: "proxy.example:8080", wantHost: "proxy.example:8080"},
		{name: "host first credentials", input: "proxy.example:8081:user:pass", wantHost: "proxy.example:8081", wantUser: "user", wantPass: "pass"},
		{name: "credentials first", input: "user:pass@proxy.example:8082", wantHost: "proxy.example:8082", wantUser: "user", wantPass: "pass"},
		{name: "host first at", input: "proxy.example:8083@user:pass", wantHost: "proxy.example:8083", wantUser: "user", wantPass: "pass"},
		{name: "password with colon", input: "proxy.example:8084:user:p:a:ss", wantHost: "proxy.example:8084", wantUser: "user", wantPass: "p:a:ss"},
		{name: "IPv6", input: "[::1]:8085", wantHost: "[::1]:8085"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, err := configuredHTTPClient(nil, test.input, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			transport, ok := client.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("transport type = %T", client.Transport)
			}
			proxyURL, err := transport.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "target.example"}})
			if err != nil {
				t.Fatal(err)
			}
			if proxyURL == nil || proxyURL.Scheme != "http" || proxyURL.Host != test.wantHost {
				t.Fatalf("proxy URL = %v, want http://%s", proxyURL, test.wantHost)
			}
			if test.wantUser == "" {
				if proxyURL.User != nil {
					t.Fatalf("unexpected proxy user = %v", proxyURL.User)
				}
				return
			}
			password, _ := proxyURL.User.Password()
			if proxyURL.User.Username() != test.wantUser || password != test.wantPass {
				t.Fatalf("proxy credentials = %q/%q", proxyURL.User.Username(), password)
			}
		})
	}
}

func TestConfiguredHTTPClientPreservesExplicitProxySchemes(t *testing.T) {
	for _, scheme := range []string{"http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			client, err := configuredHTTPClient(nil, scheme+"://proxy.example:8080", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			transport := client.Transport.(*http.Transport)
			proxyURL, err := transport.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "target.example"}})
			if err != nil {
				t.Fatal(err)
			}
			if proxyURL == nil || proxyURL.Scheme != scheme {
				t.Fatalf("proxy URL = %v, want scheme %s", proxyURL, scheme)
			}
		})
	}

	for _, scheme := range []string{"socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			client, err := configuredHTTPClient(nil, scheme+"://user:pass@127.0.0.1:65535", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			transport := client.Transport.(*http.Transport)
			if transport.Proxy != nil {
				t.Fatal("SOCKS transport unexpectedly configured an HTTP proxy callback")
			}
			if transport.DialContext == nil {
				t.Fatal("SOCKS transport has no context dialer")
			}
		})
	}
}

func TestConfiguredHTTPClientNormalizesExplicitSchemeCase(t *testing.T) {
	client, err := configuredHTTPClient(nil, "HTTP://proxy.example:8080", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.Transport.(*http.Transport)
	proxyURL, err := transport.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "target.example"}})
	if err != nil {
		t.Fatal(err)
	}
	if proxyURL == nil || proxyURL.Scheme != "http" {
		t.Fatalf("proxy URL = %v, want lower-case http scheme", proxyURL)
	}
}

func TestNewClientUsesAndNormalizesSessionProxy(t *testing.T) {
	requests := make(chan *http.Request, 1)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests <- request.Clone(request.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer proxyServer.Close()

	bareProxy := strings.TrimPrefix(proxyServer.URL, "http://")
	session := Session{ProxyURL: bareProxy}
	client, err := NewClient(Config{
		Session: &session,
		// A session proxy must win over a newly supplied proxy because the
		// persisted login is bound to its original network identity.
		ProxyURL: "127.0.0.1:1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := client.State().ProxyURL; got != proxyServer.URL {
		t.Fatalf("session proxy = %q, want %q", got, proxyServer.URL)
	}

	response, err := client.httpClient.Get("http://target.invalid/session-proxy")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	select {
	case request := <-requests:
		if request.URL.Host != "target.invalid" || request.URL.Path != "/session-proxy" {
			t.Fatalf("proxied request URL = %s", request.URL)
		}
	case <-time.After(time.Second):
		t.Fatal("session proxy did not receive the request")
	}
}

func TestPreflightProxyStillUsesHTTPSConnect(t *testing.T) {
	requests := make(chan *http.Request, 1)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests <- request.Clone(request.Context())
		http.Error(w, "fixture stops CONNECT", http.StatusBadGateway)
	}))
	defer proxyServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := PreflightProxy(ctx, strings.TrimPrefix(proxyServer.URL, "http://"))
	if err == nil {
		t.Fatal("PreflightProxy() unexpectedly succeeded")
	}
	select {
	case request := <-requests:
		if request.Method != http.MethodConnect || request.Host != "api.ipify.org:443" {
			t.Fatalf("preflight request = %s %s, want CONNECT api.ipify.org:443", request.Method, request.Host)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy did not receive HTTPS CONNECT")
	}
}

func TestPreflightProxyExplainsExplicitHTTPSSchemeMismatch(t *testing.T) {
	plainServer := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	plainServer.Config.ErrorLog = log.New(io.Discard, "", 0)
	plainServer.Start()
	defer plainServer.Close()

	proxyURL := "https://" + strings.TrimPrefix(plainServer.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := PreflightProxy(ctx, proxyURL)
	if err == nil {
		t.Fatal("PreflightProxy() unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "must use http:// or omit the scheme") {
		t.Fatalf("PreflightProxy() error lacks scheme guidance: %v", err)
	}
}
