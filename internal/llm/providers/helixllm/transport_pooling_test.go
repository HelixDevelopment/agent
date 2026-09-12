package helixllm

import (
	"net/http"
	"testing"
	"time"

	agenthttp "dev.helix.agent/internal/http"
)

// T045 (spec 006 WS-B): the HelixLLM provider must NOT keep building a bare
// `&http.Transport{}` with Go's zero-value pooling. It must take its connection
// -reuse settings from the shared internal/http pool contract, so the provider
// is the first PRODUCTION consumer of that pool (it was tests-only before) and
// its connections are actually reused across requests.
//
// These are unit assertions on the constructed transport. They do not claim the
// 200-request end-to-end behaviour, which T050 covers against a live server.

func TestProviderTransportUsesPoolDefaults(t *testing.T) {
	p := NewProvider(Config{Endpoint: "http://127.0.0.1:8443", Model: "m", Timeout: time.Second})

	tr, ok := p.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", p.httpClient.Transport)
	}

	want := agenthttp.DefaultPoolConfig()
	if tr.MaxIdleConns != want.MaxIdleConns {
		t.Errorf("MaxIdleConns = %d, want pool default %d", tr.MaxIdleConns, want.MaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost != want.MaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want pool default %d", tr.MaxIdleConnsPerHost, want.MaxIdleConnsPerHost)
	}
	if tr.MaxConnsPerHost != want.MaxConnsPerHost {
		t.Errorf("MaxConnsPerHost = %d, want pool default %d", tr.MaxConnsPerHost, want.MaxConnsPerHost)
	}
	if tr.IdleConnTimeout != want.IdleConnTimeout {
		t.Errorf("IdleConnTimeout = %v, want pool default %v", tr.IdleConnTimeout, want.IdleConnTimeout)
	}
	if tr.TLSHandshakeTimeout != want.TLSHandshakeTimeout {
		t.Errorf("TLSHandshakeTimeout = %v, want pool default %v", tr.TLSHandshakeTimeout, want.TLSHandshakeTimeout)
	}
	if tr.ExpectContinueTimeout != want.ExpectContinueTimeout {
		t.Errorf("ExpectContinueTimeout = %v, want pool default %v", tr.ExpectContinueTimeout, want.ExpectContinueTimeout)
	}
	if tr.DialContext == nil {
		t.Error("DialContext is nil: no dial timeout/keep-alive is applied")
	}
	if tr.DisableKeepAlives {
		t.Error("DisableKeepAlives set: connection reuse is off, which is the opposite of pooling")
	}
}

func TestProviderTransportKeepsTLSInsecureSkipVerify(t *testing.T) {
	p := NewProvider(Config{Endpoint: "https://127.0.0.1:8443", Model: "m", Timeout: time.Second, TLSSkipVerify: true})
	tr, ok := p.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", p.httpClient.Transport)
	}
	if tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("configured TLS skip-verify was lost when the pool settings were applied")
	}
}
