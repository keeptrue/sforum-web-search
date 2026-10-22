package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 出站守卫：本插件的请求只会发往运营者配置的搜索后端端点（默认是四个写死的
// 官方域名），因此它比 fetch-url 的主机守卫窄得多——不需要白名单，只需要保证
// 「不会因为一个写错的地址把请求发到内网」。
//
// 四层，全部不可通过「关掉设置」之外的方式绕过：
//
//  1. scheme 层：只允许 https；http 需要运营者显式打开 allow_http。
//  2. userinfo 层：拒绝带用户名/密码的地址（凭据只能走 Authorization 头）。
//  3. 字面量层：拒绝私有/回环/link-local/保留地址与 localhost / *.local / *.internal。
//     allow_private_endpoint 只跳过这一层，用于本地自建网关或测试端点。
//  4. 传输层：不跟随重定向（3xx 视为失败）、响应体上限 1 MiB、支持 http/https/socks5 代理。
//
// 刻意不做 DNS 解析层校验：目标端点由运营者配置，不是任意用户给的链接；
// 解析结果随部署环境变化（代理、内网 DNS），在本机做公网校验只会制造误报。

const (
	// maxProviderResponseBytes 是搜索响应的读取上限。搜索响应是结构化 JSON，
	// 1 MiB 足够容纳任何一家的正常返回；超过它说明端点返回了意料之外的东西。
	maxProviderResponseBytes = 1 << 20
	// providerUserAgent 只用于标识来源；不含任何站点信息。
	providerUserAgent = "sforum-web-search/1.0.0 (+https://github.com/zhuchunshu/sforum-web-search)"
)

// forbiddenHostSuffixes 是内部专用域名后缀（RFC 6761/6762/8375 及常见内网约定）。
var forbiddenHostSuffixes = []string{".localhost", ".local", ".internal"}

// forbiddenExactHosts 是永远不放行的主机名。
var forbiddenExactHosts = map[string]struct{}{
	"localhost": {}, "localhost.localdomain": {}, "ip6-localhost": {}, "ip6-loopback": {},
}

// blockedNetworks 覆盖 Go 标准库判定之外的保留段。
// 标准库的 IsPrivate/IsLoopback/IsLinkLocal* 已经处理了 10/8、172.16/12、
// 192.168/16、127/8、169.254/16、fc00::/7、fe80::/10、::1；这里补上
// 「看起来像公网但其实不是」的那些段。
var blockedNetworks = mustParseCIDRs(
	"100.64.0.0/10",      // CGNAT
	"192.0.0.0/24",       // IETF 协议分配
	"192.0.2.0/24",       // TEST-NET-1
	"198.18.0.0/15",      // 基准测试
	"198.51.100.0/24",    // TEST-NET-2
	"203.0.113.0/24",     // TEST-NET-3
	"240.0.0.0/4",        // 保留
	"255.255.255.255/32", // 广播
	"64:ff9b::/96",       // NAT64 翻译前缀
	"2001:db8::/32",      // 文档用
)

func mustParseCIDRs(values ...string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, block, err := net.ParseCIDR(value)
		if err != nil {
			panic(fmt.Sprintf("invalid blocked network %q: %v", value, err))
		}
		nets = append(nets, block)
	}
	return nets
}

// endpointError 是「端点配置不可用」的失败：它不重试、不切换，直接作为
// 可解释失败回给调用方——换一个后端也修不好一个写错的地址。
type endpointError struct{ message string }

func (e endpointError) Error() string { return e.message }

func endpointFail(format string, args ...any) error {
	return endpointError{message: fmt.Sprintf(format, args...)}
}

// responseTooLargeError 表示响应体超过上限。它属于上游条件（切换），
// 而不是配置问题。
type responseTooLargeError struct{ limit int64 }

func (e responseTooLargeError) Error() string {
	return fmt.Sprintf("响应体超过 %d 字节上限", e.limit)
}

// checkEndpoint 校验一个搜索后端端点地址。
func checkEndpoint(raw string, allowHTTP, allowPrivate bool) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, endpointFail("搜索后端地址为空。")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, endpointFail("搜索后端地址不合法：%s", trimmed)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
	case "http":
		if !allowHTTP {
			return nil, endpointFail("搜索后端必须使用 https；若确实需要明文 http，请让管理员在插件设置里打开「允许明文 http」。")
		}
	default:
		return nil, endpointFail("搜索后端只支持 http 与 https，当前是 %q。", parsed.Scheme)
	}
	if parsed.User != nil {
		return nil, endpointFail("搜索后端地址不允许携带用户名或密码。")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return nil, endpointFail("搜索后端地址缺少主机名：%s", trimmed)
	}
	if allowPrivate {
		return parsed, nil
	}
	if reason, blocked := blockedHost(host); blocked {
		return nil, endpointFail("%s", reason)
	}
	return parsed, nil
}

// blockedHost 是字面量层判定：不查 DNS，只看主机名本身。
func blockedHost(host string) (string, bool) {
	if _, ok := forbiddenExactHosts[host]; ok {
		return fmt.Sprintf("拒绝访问本机地址 %s。", host), true
	}
	for _, suffix := range forbiddenHostSuffixes {
		if strings.HasSuffix(host, suffix) {
			return fmt.Sprintf("拒绝访问内部域名 %s。", host), true
		}
	}
	if ip := net.ParseIP(host); ip != nil {
		if !isBlockedIP(ip) {
			return "", false
		}
		return fmt.Sprintf("拒绝访问非公网地址 %s。", host), true
	}
	return "", false
}

// isBlockedIP 判定一个字面量 IP 是否落在禁止访问的段里。
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsPrivate() {
		return true
	}
	for _, block := range blockedNetworks {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

// parseProxyURL 只接受 http / https / socks5 三种代理 scheme。
func parseProxyURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("无法解析代理地址")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("代理 scheme %q 不受支持（只支持 http、https、socks5）", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("代理地址缺少主机与端口")
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("暂不支持代理认证（地址里带了用户名或密码）")
	}
	return parsed, nil
}

// newProviderClient 构造出站客户端。代理地址非法不会让进程起不来：它会被记下来，
// 在每次调用时变成一条可解释的失败——插件启动失败会让整个插件（包括设置页）
// 不可用，那对运营者是最难排查的形态。
func newProviderClient(config Config) (*http.Client, error) {
	transport := &http.Transport{
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   4 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if trimmed := strings.TrimSpace(config.ProxyURL); trimmed != "" {
		proxy, err := parseProxyURL(trimmed)
		if err != nil {
			return nil, err
		}
		transport.Proxy = http.ProxyURL(proxy)
	}
	return &http.Client{
		Transport: transport,
		// 客户端超时只是兜底：每次尝试还有更短的 context 预算。
		Timeout: config.Timeout,
		// 不跟随重定向：搜索后端返回 3xx 说明端点配置已经变了，跟着跳过去
		// 只会把请求发给一个运营者没有配置过的地址。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// outbound 是四个适配器共用的出站执行器：守卫 + 请求 + 有界读取。
type outbound struct {
	client *http.Client
	config Config
}

// do 发一次出站请求，返回状态码与响应体。错误只可能是传输层/守卫层错误，
// 状态码由调用方翻译成失败语义（各家错误体的形状不同）。
func (o *outbound) do(ctx context.Context, method, endpoint string, header http.Header, payload []byte) (int, []byte, error) {
	if o == nil || o.client == nil {
		return 0, nil, endpointFail("搜索后端客户端不可用。")
	}
	parsed, err := checkEndpoint(endpoint, o.config.AllowHTTP, o.config.AllowPrivateEndpoint)
	if err != nil {
		return 0, nil, err
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, parsed.String(), body)
	if err != nil {
		return 0, nil, endpointFail("无法构造搜索请求。")
	}
	for key, values := range header {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	if request.Header.Get("User-Agent") == "" {
		request.Header.Set("User-Agent", providerUserAgent)
	}
	response, err := o.client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	read, err := io.ReadAll(io.LimitReader(response.Body, maxProviderResponseBytes+1))
	if err != nil {
		return response.StatusCode, nil, err
	}
	if int64(len(read)) > maxProviderResponseBytes {
		return response.StatusCode, nil, responseTooLargeError{limit: maxProviderResponseBytes}
	}
	return response.StatusCode, read, nil
}
