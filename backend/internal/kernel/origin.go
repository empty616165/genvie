package kernel

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// NormalizeOrigin 把 Origin 归一化为 scheme://host[:port]，并折叠默认端口。
// 只接受 http/https、无 userinfo/路径/查询/片段的来源；浏览器 Origin 头本身就满足这个形态。
func NormalizeOrigin(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("origin is empty")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("origin must be an http or https origin")
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", fmt.Errorf("origin must not contain a path, query, or fragment")
	}
	scheme := strings.ToLower(parsed.Scheme)
	// Hostname() 会拆掉 IPv6 方括号，重建时补回，避免出现 http://::1 这种非法来源。
	hostname := strings.ToLower(parsed.Hostname())
	if hostname == "" {
		return "", fmt.Errorf("origin must include a host")
	}
	if strings.Contains(hostname, ":") && !strings.HasPrefix(hostname, "[") {
		hostname = "[" + hostname + "]"
	}
	port := parsed.Port()
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", fmt.Errorf("origin port is invalid")
		}
		// 默认端口折叠，避免 http://h 与 http://h:80 被当成不同来源。
		if scheme == "http" && number == 80 || scheme == "https" && number == 443 {
			port = ""
		}
	}
	return scheme + "://" + joinOriginHost(hostname, port), nil
}

// ForwardedOrigin 由转发 Host 组装来源。端口只能来自 Host 本身：
// 外层代理没有剥掉端口时，host 形如 demo.example.com:8089；端口已被剥掉时无从还原，
// 由显式白名单兜底。不读独立的 X-Forwarded-Port——反向代理不负责重写它，
// 客户端伪造的值会直接把任意端口伪装成同源（同主机不同端口共享 Cookie）。
func ForwardedOrigin(scheme, host string) (string, bool) {
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	if scheme == "" {
		scheme = "http"
	}
	host = FirstHeaderValue(host)
	if host == "" {
		return "", false
	}
	origin, err := NormalizeOrigin(scheme + "://" + host)
	if err != nil {
		return "", false
	}
	return origin, true
}

// CompareOrigin 比较两个来源：scheme、主机名和端口都必须一致（默认端口已折叠）。
// 端口在代理链路丢失时不做"同主机名即同源"的放宽——同主机的其他端口属于不同 Origin，
// 但会共享同一份 Host Cookie，放宽等于允许它们读取登录态响应。
func CompareOrigin(left, right string) bool {
	normalizedLeft, err := NormalizeOrigin(left)
	if err != nil {
		return false
	}
	normalizedRight, err := NormalizeOrigin(right)
	if err != nil {
		return false
	}
	return normalizedLeft == normalizedRight
}

// ForwardedRequest 是重建请求对外来源所需的元数据，不依赖 gin。
type ForwardedRequest struct {
	// TLS 表示后端这条连接本身是否已是 TLS；反向代理终止 TLS 时用 ForwardedProto 还原。
	TLS bool
	// RemoteAddr 是直连对端的 RemoteAddr，不是客户端自己声明的 X-Forwarded-For。
	RemoteAddr string
	// Host 是请求的 Host 头。
	Host string
	// ForwardedHost/ForwardedProto 只在直连对端可信任时才参与重建。
	ForwardedHost  string
	ForwardedProto string
}

// TrustedProxy 表示直连对端是否有资格决定请求的对外来源。
// 生产 Compose 里 nginx 经容器网络访问 backend，backend 端口不对外发布。
func (r ForwardedRequest) TrustedProxy() bool {
	return IsTrustedProxyPeer(RemotePeerIP(r.RemoteAddr))
}

// Origin 重建浏览器实际访问的来源。转发 Host 只有在主机名与请求 Host 一致时才采信，
// 否则伪造的头能把任意外部 Origin 伪装成同源；本机回环代理（Vite dev）允许两侧都是回环地址。
func (r ForwardedRequest) Origin() (string, bool) {
	scheme := "http"
	if r.TLS {
		scheme = "https"
	}
	host := strings.TrimSpace(r.Host)
	if r.TrustedProxy() {
		if forwarded := FirstHeaderValue(r.ForwardedProto); forwarded == "http" || forwarded == "https" {
			scheme = forwarded
		}
		if forwarded := FirstHeaderValue(r.ForwardedHost); forwarded != "" && forwardedHostTrusted(forwarded, host) {
			host = forwarded
		}
	}
	return ForwardedOrigin(scheme, host)
}

// SameHostname 比较两个 host[:port] 的主机名。
func SameHostname(left, right string) bool {
	leftHost, _ := splitOriginHost(left)
	rightHost, _ := splitOriginHost(right)
	return strings.EqualFold(strings.Trim(leftHost, "[]"), strings.Trim(rightHost, "[]"))
}

// IsLoopbackHost 判断 host[:port] 是否指向本机回环地址。
func IsLoopbackHost(host string) bool {
	name, _ := splitOriginHost(host)
	name = strings.Trim(name, "[]")
	if strings.EqualFold(name, "localhost") {
		return true
	}
	ip := net.ParseIP(name)
	return ip != nil && ip.IsLoopback()
}

// RemotePeerIP 从 RemoteAddr 解析直连对端地址。转发头是否可信只取决于这条连接，
// 不能采信客户端自己声明的 X-Forwarded-For。
func RemotePeerIP(remoteAddr string) net.IP {
	addr := strings.TrimSpace(remoteAddr)
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	}
	return net.ParseIP(strings.Trim(addr, "[]"))
}

// IsTrustedProxyPeer 只有本机或内网对端有资格决定对外来源。
func IsTrustedProxyPeer(ip net.IP) bool {
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

// FirstHeaderValue 取多级代理用逗号拼接时的最外层值。
func FirstHeaderValue(raw string) string {
	return strings.ToLower(strings.TrimSpace(strings.Split(raw, ",")[0]))
}

// forwardedHostTrusted 决定是否采信转发 Host。主机名必须与请求 Host 一致，
// 且不能借转发头把请求换成同主机的另一个端口：反向代理会用 `Host $host` 丢掉端口，
// 所以请求 Host 不带端口时才允许转发 Host 补端口；请求 Host 自带端口时必须两侧一致。
// 本机回环代理（Vite dev）允许两侧都是回环地址。
func forwardedHostTrusted(forwarded, host string) bool {
	if IsLoopbackHost(forwarded) && IsLoopbackHost(host) {
		return true
	}
	if !SameHostname(forwarded, host) {
		return false
	}
	_, forwardedPort := splitOriginHost(forwarded)
	_, hostPort := splitOriginHost(host)
	if hostPort == "" {
		return true
	}
	return forwardedPort == "" || forwardedPort == hostPort
}

// splitOriginHost 拆分 host[:port]，同时支持 [::1]:8080 这样的 IPv6 字面量。
func splitOriginHost(host string) (string, string) {
	host = strings.TrimSpace(host)
	if strings.HasPrefix(host, "[") {
		if end := strings.LastIndexByte(host, ']'); end > 0 {
			name := host[:end+1]
			rest := host[end+1:]
			if !strings.HasPrefix(rest, ":") {
				return name, ""
			}
			return name, strings.TrimSpace(rest[1:])
		}
		return host, ""
	}
	// 裸 IPv6（多个冒号且没有方括号）没有端口信息。
	if strings.Count(host, ":") != 1 {
		return host, ""
	}
	index := strings.LastIndexByte(host, ':')
	return host[:index], host[index+1:]
}

func joinOriginHost(host, port string) string {
	host = strings.TrimSpace(host)
	port = strings.TrimSpace(port)
	if port == "" {
		return host
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		return host + ":" + port
	}
	if strings.Contains(host, ":") {
		return net.JoinHostPort(host, port)
	}
	return host + ":" + port
}
