package kernel

import "testing"

func TestNormalizeOriginFoldsDefaultPorts(t *testing.T) {
	for raw, want := range map[string]string{
		"https://Canvas.example.com/":  "https://canvas.example.com",
		"http://example.com:80":        "http://example.com",
		"https://example.com:443":      "https://example.com",
		"http://example.com:8089":      "http://example.com:8089",
		"http://[::1]:1888":            "http://[::1]:1888",
		"http://[::1]":                 "http://[::1]",
		"http://xn--fiqs8s.example:81": "http://xn--fiqs8s.example:81",
	} {
		got, err := NormalizeOrigin(raw)
		if err != nil || got != want {
			t.Fatalf("NormalizeOrigin(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

func TestNormalizeOriginRejectsNonOrigins(t *testing.T) {
	for _, raw := range []string{"", "   ", "example.com", "ftp://example.com", "https://user:pass@example.com", "https://example.com/app", "https://example.com?a=1", "https://example.com#x", "https://example.com:0", "https://example.com:70000"} {
		if got, err := NormalizeOrigin(raw); err == nil {
			t.Fatalf("NormalizeOrigin(%q) = %q, want error", raw, got)
		}
	}
}

func TestForwardedOrigin(t *testing.T) {
	for _, test := range []struct {
		name, scheme, host, port, want string
		ok                             bool
	}{
		{"host-with-port", "http", "demo.example.com:8089", "1888", "http://demo.example.com:8089", true},
		{"port-from-header", "http", "demo.example.com", "8089", "http://demo.example.com:8089", true},
		{"default-port-dropped", "http", "demo.example.com", "80", "http://demo.example.com", true},
		{"first-hop-only", "https", "demo.example.com, proxy.internal", "443", "https://demo.example.com", true},
		{"empty-host", "http", "", "8089", "", false},
	} {
		got, ok := ForwardedOrigin(test.scheme, test.host, test.port)
		if ok != test.ok || got != test.want {
			t.Fatalf("%s: ForwardedOrigin = %q, %v; want %q, %v", test.name, got, ok, test.want, test.ok)
		}
	}
}

func TestCompareOrigin(t *testing.T) {
	if !CompareOrigin("http://demo.example.com:8089", "http://demo.example.com:8089") {
		t.Fatal("identical origins should match")
	}
	if !CompareOrigin("http://demo.example.com:80", "http://demo.example.com") {
		t.Fatal("default port should fold")
	}
	if CompareOrigin("http://demo.example.com:8089", "http://demo.example.com") {
		t.Fatal("dropped proxy port must not widen the origin to every port")
	}
	if CompareOrigin("https://demo.example.com:8089", "http://demo.example.com:8089") {
		t.Fatal("scheme mismatch must not match")
	}
	if CompareOrigin("http://evil.example.com:8089", "http://demo.example.com:8089") {
		t.Fatal("different host must not match")
	}
	if CompareOrigin("http://demo.example.com:9999", "http://demo.example.com:8089") {
		t.Fatal("explicit different port must not match")
	}
}

func TestForwardedRequestTrustsOnlyProxyPeers(t *testing.T) {
	trusted := ForwardedRequest{TLS: false, RemoteAddr: "172.18.0.5:52000", Host: "demo.example.com", ForwardedHost: "demo.example.com", ForwardedPort: "8089", ForwardedProto: "http"}
	if origin, ok := trusted.Origin(); !ok || origin != "http://demo.example.com:8089" {
		t.Fatalf("trusted proxy origin = %q, %v", origin, ok)
	}
	untrusted := trusted
	untrusted.RemoteAddr = "39.170.51.205:40000"
	untrusted.ForwardedPort = "9999"
	untrusted.ForwardedProto = "https"
	if origin, ok := untrusted.Origin(); !ok || origin != "http://demo.example.com" {
		t.Fatalf("untrusted peer must ignore forwarded headers, got %q, %v", origin, ok)
	}
	// 伪造转发 Host 不能替换主机名，即使对端可信。
	spoofed := trusted
	spoofed.ForwardedHost = "evil.example.com"
	if origin, ok := spoofed.Origin(); !ok || origin != "http://demo.example.com" {
		t.Fatalf("spoofed forwarded host accepted, got %q, %v", origin, ok)
	}
}

func TestLoopbackForwardedHostAllowedForDevProxy(t *testing.T) {
	request := ForwardedRequest{RemoteAddr: "127.0.0.1:50000", Host: "127.0.0.1:8081", ForwardedHost: "localhost:3001", ForwardedPort: "", ForwardedProto: "http"}
	if origin, ok := request.Origin(); !ok || origin != "http://localhost:3001" {
		t.Fatalf("loopback dev proxy origin = %q, %v", origin, ok)
	}
}
