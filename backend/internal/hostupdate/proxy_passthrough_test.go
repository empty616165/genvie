package hostupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposeBackendsReceiveOutboundProxyEnvironment(t *testing.T) {
	for _, name := range []string{
		"docker-compose.yml",
		"docker-compose.local.yml",
		"docker-compose.server.yml",
		"docker-compose.deploy.yml",
		"docker-compose.dev.yml",
	} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "..", name))
			if err != nil {
				t.Fatal(err)
			}
			compose := string(data)
			start := strings.Index(compose, "\n  backend:\n")
			end := strings.Index(compose[start+1:], "\n  web:\n")
			if start < 0 || end < 0 {
				t.Fatalf("%s backend/web service blocks not found", name)
			}
			backend := compose[start : start+1+end]
			for _, declaration := range []string{
				`HTTP_PROXY: "${HTTP_PROXY:-}"`,
				`HTTPS_PROXY: "${HTTPS_PROXY:-}"`,
				`NO_PROXY: "${NO_PROXY:-localhost,127.0.0.1,::1,postgres,redis,yingce-agent,backend}"`,
			} {
				if !strings.Contains(backend, declaration) {
					t.Errorf("%s backend is missing %s", name, declaration)
				}
			}
		})
	}
}
