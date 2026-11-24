package main

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// 复现生产链路：浏览器访问 http://demo.deep-learn.com:8089，
// nginx 转发时 Host 丢端口，靠 X-Forwarded-Host 携带端口。
func TestRegistrationCORSOnNonDefaultPort(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name           string
		env            string
		origin         string
		host           string
		forwardedHost  string
		forwardedPort  string
		forwardedProto string
		remoteAddr     string
		want           bool
	}{
		{"forwarded host keeps port", "", "http://demo.deep-learn.com:8089", "demo.deep-learn.com", "demo.deep-learn.com:8089", "", "http", "172.18.0.5:52000", true},
		// 代理丢端口时不再靠"同主机名"放宽，改为依赖显式白名单（fail closed）。
		{"port lost requires whitelist", "", "http://39.170.51.205:8089", "39.170.51.205", "39.170.51.205", "", "http", "172.18.0.5:52000", false},
		{"port lost recovered by whitelist", "http://39.170.51.205:8089", "http://39.170.51.205:8089", "39.170.51.205", "39.170.51.205", "", "http", "172.18.0.5:52000", true},
		{"whitelisted origin", "http://demo.deep-learn.com:8089", "http://demo.deep-learn.com:8089", "demo.deep-learn.com", "demo.deep-learn.com:8089", "", "http", "172.18.0.5:52000", true},
		{"other port rejected", "", "http://demo.deep-learn.com:1888", "demo.deep-learn.com", "demo.deep-learn.com:8089", "", "http", "172.18.0.5:52000", false},
		{"https on http site rejected", "", "https://demo.deep-learn.com:8089", "demo.deep-learn.com", "demo.deep-learn.com:8089", "", "http", "172.18.0.5:52000", false},
		{"foreign origin rejected", "", "http://evil.example.com:8089", "demo.deep-learn.com", "demo.deep-learn.com:8089", "", "http", "172.18.0.5:52000", false},
		{"external peer spoofing headers", "", "http://evil.example.com:8089", "demo.deep-learn.com", "evil.example.com:8089", "", "http", "39.170.51.205:40000", false},
		// 客户端自带的 X-Forwarded-Port 不得把任意端口变成同源。
		{"client forwarded port ignored", "", "http://demo.deep-learn.com:9999", "demo.deep-learn.com", "demo.deep-learn.com", "9999", "http", "172.18.0.5:52000", false},
		// 请求 Host 自带端口时，转发 Host 不得改写端口。
		{"forwarded host cannot rewrite host port", "", "http://demo.deep-learn.com:9999", "demo.deep-learn.com:1888", "demo.deep-learn.com:9999", "", "http", "172.18.0.5:52000", false},
		// TLS 在 CDN/ESA 终止：回源是明文，边缘未下发 X-Forwarded-Proto 时来源重建为 http，
		// 浏览器的 https Origin 因此不同源，只能靠显式白名单兜底。
		{"https origin without forwarded proto", "", "https://canvas.example.com", "canvas.example.com", "canvas.example.com", "", "http", "172.18.0.5:52000", false},
		{"https origin whitelisted", "https://canvas.example.com", "https://canvas.example.com", "canvas.example.com", "canvas.example.com", "", "http", "172.18.0.5:52000", true},
		{"https origin forwarded proto", "", "https://canvas.example.com", "canvas.example.com", "canvas.example.com", "", "https", "172.18.0.5:52000", true},
		// 补上转发 scheme 后仍不能用其他端口冒充同源。
		{"https other port with forwarded proto", "", "https://canvas.example.com:8443", "canvas.example.com", "canvas.example.com", "", "https", "172.18.0.5:52000", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CANVAS_CORS_ORIGINS", test.env)
			context, _ := gin.CreateTestContext(httptest.NewRecorder())
			context.Request = httptest.NewRequest("POST", "http://"+test.host+"/api/auth/register", nil)
			context.Request.RemoteAddr = test.remoteAddr
			context.Request.Header.Set("Origin", test.origin)
			context.Request.Header.Set("X-Forwarded-Host", test.forwardedHost)
			context.Request.Header.Set("X-Forwarded-Port", test.forwardedPort)
			context.Request.Header.Set("X-Forwarded-Proto", test.forwardedProto)
			if got := allowedOrigin(context, test.origin); got != test.want {
				t.Fatalf("allowedOrigin = %v, want %v", got, test.want)
			}
		})
	}
}
