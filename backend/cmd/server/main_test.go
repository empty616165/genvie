package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"yingce/backend/internal/database"
	"yingce/backend/internal/repository"
	"yingce/backend/internal/service"

	"github.com/gin-gonic/gin"
)

func TestEnsureBuiltinWorkbenchCatalogStartupWiring(t *testing.T) {
	db, err := database.Open(database.Config{Driver: "sqlite", DSN: "file:workbench-startup?mode=memory&cache=shared"})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.MigrateSchema(db); err != nil {
		t.Fatal(err)
	}
	svc := service.New(repository.New(db), t.TempDir())
	if err := ensureBuiltinWorkbenchCatalog(svc); err != nil {
		t.Fatal(err)
	}
	tools, err := repository.New(db).WorkbenchTools("image", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) == 0 {
		t.Fatal("startup initializer did not publish image workbench tools")
	}
}

func TestAllowedOriginWildcard(t *testing.T) {
	t.Setenv("CANVAS_CORS_ORIGINS", "*")
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = httptest.NewRequest("GET", "http://backend/api/health", nil)
	if !allowedOrigin(context, "https://example.com") {
		t.Fatal("wildcard CORS should allow a valid HTTPS origin")
	}
	if allowedOrigin(context, "ftp://example.com") {
		t.Fatal("wildcard CORS should reject non-HTTP origins")
	}
}

func TestEnvironmentParsers(t *testing.T) {
	t.Setenv("CANVAS_AUTO_MIGRATE", "false")
	value, err := envBool("CANVAS_AUTO_MIGRATE", true)
	if err != nil || value {
		t.Fatalf("envBool = %v, %v", value, err)
	}
	t.Setenv("CANVAS_SHUTDOWN_TIMEOUT", "45s")
	duration, err := envDuration("CANVAS_SHUTDOWN_TIMEOUT", time.Minute)
	if err != nil || duration != 45*time.Second {
		t.Fatalf("envDuration = %v, %v", duration, err)
	}
}

func TestAllowedOriginUsesForwardedHost(t *testing.T) {
	t.Setenv("CANVAS_CORS_ORIGINS", "")
	// 生产 nginx 用 `Host $host` 转发，所以请求 Host 与 X-Forwarded-Host 主机名一致；
	// 转发头只用来恢复被代理改写的 scheme 和端口。
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = httptest.NewRequest("GET", "http://canvas.example.com/api/health", nil)
	context.Request.RemoteAddr = "172.18.0.5:52000"
	context.Request.Header.Set("X-Forwarded-Proto", "https")
	context.Request.Header.Set("X-Forwarded-Host", "canvas.example.com")
	if !allowedOrigin(context, "https://canvas.example.com") {
		t.Fatal("forwarded public host should be treated as same-origin")
	}
	if allowedOrigin(context, "http://canvas.example.com") {
		t.Fatal("https terminated by proxy must not accept an http origin")
	}
}

func TestAllowedOriginKeepsNonDefaultForwardedPort(t *testing.T) {
	t.Setenv("CANVAS_CORS_ORIGINS", "")
	// nginx 的 $host 不含端口，端口由 X-Forwarded-Port 或带端口的转发 Host 补齐。
	for _, test := range []struct {
		name            string
		forwardedHost   string
		forwardedPort   string
		requestHost     string
		origin          string
		expectedAllowed bool
	}{
		{"port-from-forwarded-port", "demo.example.com", "8089", "demo.example.com", "http://demo.example.com:8089", true},
		{"port-in-forwarded-host", "demo.example.com:8089", "", "demo.example.com", "http://demo.example.com:8089", true},
		{"wrong-port-rejected", "demo.example.com", "8089", "demo.example.com", "http://demo.example.com:1888", false},
		{"scheme-mismatch-rejected", "demo.example.com", "8089", "demo.example.com", "https://demo.example.com:8089", false},
		{"other-hostname-rejected", "evil.example.com", "8089", "demo.example.com", "http://evil.example.com:8089", false},
		{"default-port-folded", "demo.example.com", "80", "demo.example.com", "http://demo.example.com", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			context, _ := gin.CreateTestContext(httptest.NewRecorder())
			context.Request = httptest.NewRequest("GET", "http://"+test.requestHost+"/api/health", nil)
			context.Request.RemoteAddr = "172.18.0.5:52000"
			context.Request.Header.Set("X-Forwarded-Proto", "http")
			context.Request.Header.Set("X-Forwarded-Host", test.forwardedHost)
			context.Request.Header.Set("X-Forwarded-Port", test.forwardedPort)
			if got := allowedOrigin(context, test.origin); got != test.expectedAllowed {
				t.Fatalf("allowedOrigin(%q) = %v, want %v", test.origin, got, test.expectedAllowed)
			}
		})
	}
}

func TestAllowedOriginIgnoresForwardedHeadersFromUntrustedPeer(t *testing.T) {
	t.Setenv("CANVAS_CORS_ORIGINS", "")
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = httptest.NewRequest("GET", "http://demo.example.com:8089/api/health", nil)
	// 直连公网客户端伪造转发头不能把外部 Origin 变成同源。
	context.Request.RemoteAddr = "39.170.51.205:40000"
	context.Request.Header.Set("X-Forwarded-Host", "evil.example.com")
	context.Request.Header.Set("X-Forwarded-Port", "8089")
	if allowedOrigin(context, "http://evil.example.com:8089") {
		t.Fatal("untrusted peer must not authorize a foreign origin via forwarded headers")
	}
	// 但真实同源（Host 与 Origin 一致）仍然放行，代理头不参与判断。
	if !allowedOrigin(context, "http://demo.example.com:8089") {
		t.Fatal("same-origin request from an untrusted peer should still be allowed")
	}
}

func TestCORSMiddlewareRejectsWithReason(t *testing.T) {
	t.Setenv("CANVAS_CORS_ORIGINS", "https://app.example.com")
	middleware, err := cors()
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(middleware)
	request := httptest.NewRequest("POST", "http://backend/api/auth/register", nil)
	request.Header.Set("Origin", "http://evil.example.com")
	request.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	router.ServeHTTP(out, request)
	if out.Code != 403 || !strings.Contains(out.Body.String(), `"reason":"cors_origin_denied"`) {
		t.Fatalf("denied CORS response = %d %s", out.Code, out.Body.String())
	}
}

func TestParseCORSPolicyNormalizesConfiguredOrigins(t *testing.T) {
	policy, err := parseCORSPolicy(" https://Canvas.example.com/ , http://localhost:3000 ")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := policy.origins["https://canvas.example.com"]; !ok {
		t.Fatal("configured origin was not normalized")
	}
	if _, ok := policy.origins["http://localhost:3000"]; !ok {
		t.Fatal("configured port was not preserved")
	}
}

func TestParseCORSPolicyRejectsNonOriginValue(t *testing.T) {
	if _, err := parseCORSPolicy("https://example.com/app"); err == nil {
		t.Fatal("path-bearing CORS value should be rejected")
	}
	if _, err := parseCORSPolicy("ftp://example.com"); err == nil {
		t.Fatal("non-HTTP CORS value should be rejected")
	}
}

func TestAllowedOriginConfiguredListDoesNotFallbackToArbitraryLocalhost(t *testing.T) {
	t.Setenv("CANVAS_CORS_ORIGINS", "https://app.example.com")
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = httptest.NewRequest("GET", "http://backend/api/health", nil)
	if allowedOrigin(context, "http://localhost:3000") {
		t.Fatal("configured CORS list should disable the implicit localhost fallback")
	}
}

func TestRedactCanvasSharePath(t *testing.T) {
	got := redactCanvasSharePath("/api/public/canvas-shares/private-token/resources/resource-1/file")
	if got != "/api/public/canvas-shares/:token/resources/resource-1/file" {
		t.Fatalf("unexpected redacted path: %s", got)
	}
	if got := redactCanvasSharePath("/api/tasks"); got != "/api/tasks" {
		t.Fatalf("unrelated path changed: %s", got)
	}
}
