package hostupdate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

var ErrBusy = errors.New("更新、版本检查或来源检测正在进行，请稍后重试")

// errRedirectRejected 表示重定向被安全策略拒绝，重试不会改变结果。
var errRedirectRejected = errors.New("更新资源重定向不符合安全策略")

func (s Sources) Validate() error {
	if s.GitHubSource != "direct" && s.GitHubSource != "gh_proxy" {
		return errors.New("GitHub 来源必须为 direct 或 gh_proxy")
	}
	if s.ImageSource != "ghcr" && s.ImageSource != "nju" {
		return errors.New("容器镜像来源必须为 ghcr 或 nju")
	}
	return nil
}

// normalizeProxyPrefix 允许管理员只填域名，补齐协议与结尾斜杠。
func normalizeProxyPrefix(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	if !strings.HasSuffix(value, "/") {
		value += "/"
	}
	return value
}

// validateProxyPrefix 只接受 HTTPS 绝对地址；代理路径由更新器拼接，不能带 query 或 fragment。
func validateProxyPrefix(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Port() != "" {
		return fmt.Errorf("GitHub 加速前缀必须是 HTTPS 地址，例如 %s", DefaultGitHubProxy)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("GitHub 加速前缀不能包含查询参数或片段")
	}
	return nil
}

// normalizeImageHost 接受主机名或含协议的地址，统一成主机名，避免拼出双协议引用。
func normalizeImageHost(value string) string {
	value = strings.TrimSpace(value)
	if parsed, err := url.Parse(value); err == nil && parsed.Hostname() != "" {
		value = parsed.Hostname()
	}
	return strings.Trim(strings.TrimSuffix(value, "/"), " ")
}

// validateImageHost 只允许纯主机名，避免把任意 URL 拼进镜像引用。
func validateImageHost(value string) error {
	if value == "" {
		return fmt.Errorf("容器镜像源必须是主机名，例如 %s", DefaultImageMirror)
	}
	if strings.ContainsAny(value, "/:@ ") {
		return fmt.Errorf("容器镜像源必须是主机名，例如 %s", DefaultImageMirror)
	}
	return nil
}

func (m *Manager) proxyPrefix() string   { return m.config.GitHubProxy }
func (m *Manager) imageRegistry() string { return m.config.ImageMirror }

// proxyHost 返回加速前缀的主机名，重定向校验用它判断是否仍在同一代理域内。
func (m *Manager) proxyHost() string {
	parsed, err := url.Parse(m.config.GitHubProxy)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// sameRegistrableDomain 允许代理在同一站点内部跳转（父域与子域互换），
// 例如 gh-proxy.net 与 ww19.gh-proxy.net；其余一律拒绝。
// 刻意不用“最后两段相等”的写法：对 .com.cn、.co.uk 这类多段公共后缀，
// 那会把 evil.com.cn 误判成与 gh-proxy.com.cn 同域，放行跨站重定向。
func sameRegistrableDomain(target, base string) bool {
	target = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(target), "."))
	base = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(base), "."))
	if target == "" || base == "" {
		return false
	}
	if target == base {
		return true
	}
	// 两侧都必须是至少两段的主机名。否则目标为单段名（例如 org）时，
	// HasSuffix(base, ".org") 会成立，等于把任意同名顶级域都当作同一站点。
	if strings.Count(target, ".") < 1 || strings.Count(base, ".") < 1 {
		return false
	}
	return strings.HasSuffix(target, "."+base) || strings.HasSuffix(base, "."+target)
}

func (s Sources) descriptionFor(m *Manager) string {
	github, registry := "GitHub 直连", "ghcr.io"
	if s.GitHubSource == "gh_proxy" {
		github = strings.TrimSuffix(strings.TrimPrefix(m.proxyPrefix(), "https://"), "/")
	}
	if s.ImageSource == "nju" {
		registry = m.imageRegistry()
	}
	return github + " / " + registry
}

func (s Sources) imagesWithin(m *Manager, repository, version string) deploymentImages {
	images := immutableImageRefs(repository, version)
	if s.ImageSource == "nju" {
		registry := m.imageRegistry()
		images.backend = strings.Replace(images.backend, "ghcr.io/", registry+"/", 1)
		images.web = strings.Replace(images.web, "ghcr.io/", registry+"/", 1)
		images.agent = strings.Replace(images.agent, "ghcr.io/", registry+"/", 1)
	}
	return images
}

func (m *Manager) busyLocked() bool {
	return m.state.Operation.Phase.Active() || (m.state.SourceProbe != nil && m.state.SourceProbe.Status == "running")
}

func (m *Manager) SaveSources(sources Sources) (Status, error) {
	if err := sources.Validate(); err != nil {
		return m.Snapshot(), err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busyLocked() {
		return m.snapshotLocked(), ErrBusy
	}
	if sources == m.state.Sources {
		return m.snapshotLocked(), nil
	}
	previous := m.state
	m.state.Sources = sources
	m.state.LatestRelease = nil
	m.state.SourceProbe = nil
	if m.state.Operation.Phase != PhaseManualIntervention {
		m.state.Operation = Operation{Phase: PhaseIdle, Logs: []LogEntry{}}
	}
	m.appendLogLocked(m.state.Operation.Phase, "更新来源已保存，请重新检查版本："+sources.descriptionFor(m))
	if err := m.saveStateLocked(); err != nil {
		m.state = previous
		return m.snapshotLocked(), fmt.Errorf("保存更新来源：%w", err)
	}
	return m.snapshotLocked(), nil
}

// 只转换更新器构造的公开 GitHub 地址，不接收任意代理地址或转发管理员凭据。
func (m *Manager) githubGet(ctx context.Context, original string, sources Sources, accept string) (*http.Response, error) {
	parsed, err := url.Parse(original)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Port() != "" {
		return nil, errors.New("无效的 GitHub 更新资源地址")
	}
	switch parsed.Hostname() {
	case "api.github.com", "github.com", "raw.githubusercontent.com":
	default:
		return nil, errors.New("更新资源必须来自 GitHub")
	}
	address := original
	proxy := sources.GitHubSource == "gh_proxy"
	if proxy {
		address = m.proxyPrefix() + original
	}
	newRequest := func() (*http.Request, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("User-Agent", "open-ai-canvas-host-updater")
		if accept != "" {
			request.Header.Set("Accept", accept)
		}
		if parsed.Hostname() == "api.github.com" {
			request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
			if !proxy && m.config.GitHubToken != "" {
				request.Header.Set("Authorization", "Bearer "+m.config.GitHubToken)
			}
		}
		return request, nil
	}
	client := *m.httpClient
	proxyHost := ""
	if proxy {
		proxyHost = m.proxyHost()
	}
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("更新资源重定向次数过多")
		}
		host := next.URL.Hostname()
		if next.URL.Scheme != "https" || next.URL.User != nil || next.URL.Port() != "" {
			return fmt.Errorf("%w：地址无效", errRedirectRejected)
		}
		if proxy {
			next.Header.Del("Authorization")
			// 公共代理常见行为是 302 到自己的子域名（例如 ww19.gh-proxy.net），
			// 因此允许同一父域下的跳转，但不允许跳到无关第三方。
			if !sameRegistrableDomain(host, proxyHost) {
				return fmt.Errorf("%w：镜像源重定向离开 %s，请检查来源或切换直连", errRedirectRejected, proxyHost)
			}
		} else {
			if host != "api.github.com" {
				next.Header.Del("Authorization")
			}
			if host != "github.com" && host != "api.github.com" && !strings.HasSuffix(host, ".githubusercontent.com") {
				return fmt.Errorf("%w：重定向离开 GitHub", errRedirectRejected)
			}
		}
		return nil
	}
	// 到 GitHub 的链路在部分网络下约有半数连接是半死的：TCP 建立后读不到响应，
	// 表现为 TLS RST 或首包超时。GET 请求无 body，重放安全，因此做有限多次尝试。
	// 策略性重定向拒绝重试无意义，直接返回。
	const attempts = 6
	var response *http.Response
	for attempt := 0; attempt < attempts; attempt++ {
		request, err := newRequest()
		if err != nil {
			return nil, err
		}
		response, err = client.Do(request)
		if err == nil {
			break
		}
		if attempt == attempts-1 || ctx.Err() != nil || errors.Is(err, errRedirectRejected) {
			return nil, err
		}
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, sourceHTTPError(response.StatusCode)
	}
	return response, nil
}

func sourceHTTPError(status int) error {
	switch status {
	case http.StatusForbidden, http.StatusTooManyRequests:
		return fmt.Errorf("来源返回 HTTP %d：请求被拒绝或达到限流，请稍后重试或切换来源", status)
	case http.StatusNotFound:
		return errors.New("来源返回 HTTP 404：目标版本或资源不存在，镜像源可能尚未同步")
	default:
		return fmt.Errorf("来源返回 HTTP %d", status)
	}
}
