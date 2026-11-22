package hostupdate

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 公共加速前缀与全球匿名请求共用 GitHub API 配额，繁忙时高频 403
// （API rate limit exceeded）。版本发现必须能退到直连，否则「有更新」
// 会被误报成「检查失败」——这正是线上检测更新全挂的直接表现。
func TestLatestReleaseFallsBackToDirectWhenProxyIsRateLimited(t *testing.T) {
	m := sourceManager(t)
	var proxied, direct int
	m.httpClient = &http.Client{Transport: sourceTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Host, "gh-proxy.org") {
			proxied++
			return sourceResponse(http.StatusForbidden, `{"message":"API rate limit exceeded for user ID 10094017."}`), nil
		}
		direct++
		return sourceResponse(http.StatusOK, `[{"tag_name":"v2.0.0","name":"v2.0.0"}]`), nil
	})}
	release, err := m.latestRelease(context.Background(), Sources{"gh_proxy", "nju"})
	if err != nil {
		t.Fatalf("proxy rate limit should fall back to direct: %v", err)
	}
	if release.Version != "v2.0.0" {
		t.Fatalf("release = %+v", release)
	}
	if proxied != 1 || direct != 1 {
		t.Fatalf("expected one attempt each, proxied=%d direct=%d", proxied, direct)
	}
}

// 直连来源不应额外尝试，避免无意义的两倍请求。
func TestLatestReleaseDirectOnlyAttemptsOnce(t *testing.T) {
	m := sourceManager(t)
	attempts := 0
	m.httpClient = &http.Client{Transport: sourceTransport(func(*http.Request) (*http.Response, error) {
		attempts++
		return sourceResponse(http.StatusOK, `[{"tag_name":"v2.0.0","name":"v2.0.0"}]`), nil
	})}
	if _, err := m.latestRelease(context.Background(), Sources{"direct", "ghcr"}); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("direct source made %d attempts", attempts)
	}
}

// 回退到直连时必须改用直连语义：可以带 Token，且不再受代理重定向限制。
// 这条用例防止把代理的来源参数错误地复用到直连请求上。
func TestFallbackToDirectCarriesTokenAndProxyDoesNot(t *testing.T) {
	m := sourceManager(t)
	m.config.GitHubToken = "secret-token"
	seen := map[string]string{}
	m.httpClient = &http.Client{Transport: sourceTransport(func(r *http.Request) (*http.Response, error) {
		seen[r.URL.Host] = r.Header.Get("Authorization")
		if strings.HasPrefix(r.URL.Host, "gh-proxy.org") {
			return sourceResponse(http.StatusForbidden, `{"message":"API rate limit exceeded"}`), nil
		}
		return sourceResponse(http.StatusOK, `[{"tag_name":"v2.0.0","name":"v2.0.0"}]`), nil
	})}
	if _, err := m.latestRelease(context.Background(), Sources{"gh_proxy", "nju"}); err != nil {
		t.Fatal(err)
	}
	if got := seen["gh-proxy.org"]; got != "" {
		t.Fatalf("proxy received credentials: %q", got)
	}
	if got := seen["api.github.com"]; got != "Bearer secret-token" {
		t.Fatalf("direct fallback lost token: %q", got)
	}
}

// 回退必须拥有独立预算。首个出口把 ctx 预算耗尽后，如果共用同一个 ctx，
// 回退分支会在真正发请求前就以 deadline exceeded 失败，等于回退形同虚设。
func TestLatestReleaseFallbackSurvivesFirstEgressTimeout(t *testing.T) {
	m := sourceManager(t)
	reachedDirect := false
	m.httpClient = &http.Client{Transport: sourceTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Host, "gh-proxy.org") {
			// 模拟卡死直到调用方取消。
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		reachedDirect = true
		return sourceResponse(http.StatusOK, `[{"tag_name":"v2.0.0","name":"v2.0.0"}]`), nil
	})}
	// 用极短预算暴露预算共用问题：若回退共用同一 ctx，它拿不到任何时间。
	m.config.ReleaseAttemptTimeout = 200 * time.Millisecond
	release, err := m.latestRelease(context.Background(), Sources{"gh_proxy", "nju"})
	if err != nil {
		t.Fatalf("fallback should survive first egress hanging: %v", err)
	}
	if !reachedDirect || release.Version != "v2.0.0" {
		t.Fatalf("direct fallback not reached: %+v", release)
	}
}

// 两个出口都失败时，错误必须同时列出两边原因，管理员才能判断是限流还是网络不通。
func TestLatestReleaseReportsEveryEgressFailure(t *testing.T) {
	m := sourceManager(t)
	m.httpClient = &http.Client{Transport: sourceTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Host, "gh-proxy.org") {
			return sourceResponse(http.StatusForbidden, `{"message":"API rate limit exceeded"}`), nil
		}
		return sourceResponse(http.StatusBadGateway, `upstream down`), nil
	})}
	_, err := m.latestRelease(context.Background(), Sources{"gh_proxy", "nju"})
	if err == nil {
		t.Fatal("expected failure when every egress fails")
	}
	message := err.Error()
	if !strings.Contains(message, "gh-proxy.org") || !strings.Contains(message, "GitHub 直连") {
		t.Fatalf("failure did not name both egresses: %s", message)
	}
}

// 版本列表只过滤草稿与非版本 tag，保持既有选择语义不变。
func TestLatestReleasePicksHighestStableVersion(t *testing.T) {
	m := sourceManager(t)
	m.httpClient = &http.Client{Transport: sourceTransport(func(*http.Request) (*http.Response, error) {
		return sourceResponse(http.StatusOK, `[
			{"tag_name":"v1.9.0","name":"old"},
			{"tag_name":"v1.10.0","name":"newer"},
			{"tag_name":"v2.0.0","name":"draft","draft":true},
			{"tag_name":"nightly","name":"not a release"}
		]`), nil
	})}
	release, err := m.latestRelease(context.Background(), Sources{"direct", "ghcr"})
	if err != nil {
		t.Fatal(err)
	}
	if release.Version != "v1.10.0" {
		t.Fatalf("version = %s, want v1.10.0 (semver, not lexicographic; drafts excluded)", release.Version)
	}
}
