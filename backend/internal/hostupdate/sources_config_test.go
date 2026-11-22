package hostupdate

import (
	"errors"
	"strings"
	"testing"
)

// 公共加速域名会失效（gh-proxy.com 已整体返回 403），因此前缀必须可由宿主机覆盖，
// 且默认值仍要通过 HTTPS 绝对地址校验，不能把任意 URL 拼进请求。
func TestGitHubProxyPrefixNormalizedAndValidated(t *testing.T) {
	cases := map[string]string{
		"gh-proxy.org":         "https://gh-proxy.org/",
		"https://mirror.test/": "https://mirror.test/",
		"  https://a.b/c  ":    "https://a.b/c/",
	}
	for input, want := range cases {
		if got := normalizeProxyPrefix(input); got != want {
			t.Errorf("normalizeProxyPrefix(%q) = %q, want %q", input, got, want)
		}
	}
	for _, invalid := range []string{"http://mirror.test/", "https://user:pw@mirror.test/", "https://mirror.test:8080/", "https://mirror.test/?x=1", ""} {
		if err := validateProxyPrefix(invalid); err == nil {
			t.Errorf("proxy prefix %q was accepted", invalid)
		}
	}
}

// 容器镜像源只接受主机名；带协议或路径会拼出无效的镜像引用。
func TestImageHostNormalizedAndValidated(t *testing.T) {
	if got := normalizeImageHost("https://ghcr.example.com/"); got != "ghcr.example.com" {
		t.Fatalf("normalizeImageHost = %q", got)
	}
	// 带协议时只取主机名；镜像引用本身不带协议，由 Docker 决定使用 HTTPS。
	if got := normalizeImageHost("http://ghcr.example.com"); got != "ghcr.example.com" {
		t.Fatalf("normalizeImageHost kept scheme: %q", got)
	}
	for _, invalid := range []string{"", "ghcr.example.com/path", "ghcr.example.com:443", "user@ghcr.example.com"} {
		if err := validateImageHost(normalizeImageHost(invalid)); err == nil {
			t.Errorf("image host %q was accepted", invalid)
		}
	}
}

// 镜像源来自配置，而不是写死 ghcr.nju.edu.cn。
func TestSourcesImagesUseConfiguredMirror(t *testing.T) {
	m := &Manager{config: Config{ImageMirror: "ghcr.example.com", GitHubProxy: DefaultGitHubProxy}}
	images := Sources{"direct", "nju"}.imagesWithin(m, "empty616165/genvie", "v2.0.0")
	for _, image := range []string{images.backend, images.web, images.agent} {
		if !strings.HasPrefix(image, "ghcr.example.com/empty616165/genvie-canvas-") {
			t.Fatalf("unexpected image reference: %s", image)
		}
	}
	if got := (Sources{"gh_proxy", "nju"}).descriptionFor(m); !strings.Contains(got, "ghcr.example.com") {
		t.Fatalf("description did not reflect configured mirror: %s", got)
	}
}

// 只有可恢复的层写入/传输错误才重试；确定性失败应立即中止，避免拖长停服前等待。
func TestRetryableImagePullClassification(t *testing.T) {
	retryable := []string{
		`failed commit on ref "layer-sha256:abc": commit failed: unexpected commit digest sha256:b58a, expected sha256:d95b: failed precondition`,
		`write /var/lib/docker/tmp/x: unexpected EOF`,
		`Get "https://ghcr.io/v2/...": read tcp: connection reset by peer`,
		`context deadline exceeded`,
	}
	for _, message := range retryable {
		if !retryableImagePull(errors.New(message)) {
			t.Errorf("expected retry for %q", message)
		}
	}
	deterministic := []string{
		`denied: requested access to the resource is denied`,
		`manifest unknown`,
		`no matching manifest for linux/riscv64 in the manifest list entries`,
	}
	for _, message := range deterministic {
		if retryableImagePull(errors.New(message)) {
			t.Errorf("unexpected retry for %q", message)
		}
	}
}

// 管理员看到的必须是可执行动作，而不是只夹带层哈希的原始报错。
func TestExplainDockerFailureAddsActionableHint(t *testing.T) {
	hint := explainDockerFailure("commit failed: unexpected commit digest sha256:b58a, expected sha256:d95b: failed precondition")
	if !strings.Contains(hint, "docker system prune -f") || !strings.Contains(hint, "prune -a") {
		t.Fatalf("missing cleanup guidance: %s", hint)
	}
	if !strings.Contains(explainDockerFailure("no space left on device"), "空间不足") {
		t.Fatal("disk exhaustion was not explained")
	}
	if !strings.Contains(explainDockerFailure("no matching manifest for linux/arm64"), "架构") {
		t.Fatal("architecture mismatch was not explained")
	}
	if got := explainDockerFailure("some unrelated failure"); got != "some unrelated failure" {
		t.Fatalf("unknown error was rewritten: %s", got)
	}
}

// 代理跳转到自己父域下的子域名应放行，跳到无关站点必须拒绝。
func TestSameRegistrableDomain(t *testing.T) {
	allowed := [][2]string{
		{"gh-proxy.org", "gh-proxy.org"},
		{"ww19.gh-proxy.net", "gh-proxy.net"},
		{"gh-proxy.net", "ww19.gh-proxy.net"},
	}
	for _, pair := range allowed {
		if !sameRegistrableDomain(pair[0], pair[1]) {
			t.Errorf("%s -> %s should be allowed", pair[0], pair[1])
		}
	}
	denied := [][2]string{
		{"evil.test", "gh-proxy.org"},
		{"gh-proxy.org.evil.test", "gh-proxy.org"},
		{"evilgh-proxy.org", "gh-proxy.org"},
		{"raw.githubusercontent.com", "gh-proxy.org"},
		// 多段公共后缀不能被"最后两段相等"蒙混过去。
		{"evil.com.cn", "gh-proxy.com.cn"},
		{"gh-proxy.com.cn.attacker.test", "gh-proxy.com.cn"},
		// 单段主机名不得因为共享顶级域而被当作同一站点。
		{"org", "gh-proxy.org"},
		{"gh-proxy.org", "org"},
	}
	for _, pair := range denied {
		if sameRegistrableDomain(pair[0], pair[1]) {
			t.Errorf("%s -> %s should be rejected", pair[0], pair[1])
		}
	}
}
