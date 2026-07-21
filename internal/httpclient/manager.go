package httpclient

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
)

// Config 定义创建 HTTP 客户端的参数。
// 此结构体用于生成唯一指纹以实现客户端复用。
type Config struct {
	ConnectTimeout        time.Duration
	RequestTimeout        time.Duration
	IdleConnTimeout       time.Duration
	MaxIdleConns          int
	MaxIdleConnsPerHost   int
	ResponseHeaderTimeout time.Duration
	DisableCompression    bool
	WriteBufferSize       int
	ReadBufferSize        int
	ForceAttemptHTTP2     bool
	TLSHandshakeTimeout   time.Duration
	ExpectContinueTimeout time.Duration
	// ProxyURL 支持单个代理地址或逗号分隔的多个代理地址。
	// 多个代理地址会通过轮询方式使用，例如：
	//   "http://proxy1:8080,socks5://proxy2:1080"
	// 为空时使用环境变量（HTTP_PROXY/HTTPS_PROXY）配置。
	ProxyURL string
}

// ProxyClientPool 管理一个或多个 HTTP 客户端，支持轮询选择。
// 当配置了多个代理地址时，每个代理地址对应一个独立的 http.Client，
// 通过原子计数器实现平滑轮询。
type ProxyClientPool struct {
	clients []*http.Client
	counter uint64
}

// GetClient 通过原子轮询返回下一个 HTTP 客户端。
// 如果池中只有一个客户端，直接返回该客户端（零开销）。
func (p *ProxyClientPool) GetClient() *http.Client {
	if len(p.clients) == 1 {
		return p.clients[0]
	}
	idx := atomic.AddUint64(&p.counter, 1)
	return p.clients[idx%uint64(len(p.clients))]
}

// Close 关闭池中所有客户端的底层连接。
func (p *ProxyClientPool) Close() {
	for _, client := range p.clients {
		if transport, ok := client.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
}

// HTTPClientManager 管理 HTTP 客户端池的生命周期。
// 基于配置指纹创建和缓存客户端池，确保相同配置的池被复用。
type HTTPClientManager struct {
	pools map[string]*ProxyClientPool
	lock  sync.RWMutex
}

// RemoveClient 根据指纹关闭并移除缓存的客户端池。
// 如果未找到池则返回 false。
func (m *HTTPClientManager) RemoveClient(fingerprint string) bool {
	m.lock.Lock()
	defer m.lock.Unlock()
	pool, exists := m.pools[fingerprint]
	if !exists {
		return false
	}
	pool.Close()
	delete(m.pools, fingerprint)
	return true
}

// Close 关闭所有缓存的客户端池并释放底层连接。
func (m *HTTPClientManager) Close() {
	m.lock.Lock()
	defer m.lock.Unlock()
	for _, pool := range m.pools {
		pool.Close()
	}
	m.pools = make(map[string]*ProxyClientPool)
}

// NewHTTPClientManager 创建新的客户端管理器。
func NewHTTPClientManager() *HTTPClientManager {
	return &HTTPClientManager{
		pools: make(map[string]*ProxyClientPool),
	}
}

// GetClient 返回匹配给定配置的 HTTP 客户端池。
// 如果缓存中已存在匹配的池则直接返回。
// 否则创建新池、缓存并返回。
//
// 当 ProxyURL 包含逗号分隔的多个地址时，会为每个代理地址
// 创建独立的 http.Client 并封装到池中，通过轮询方式使用。
func (m *HTTPClientManager) GetClient(config *Config) *ProxyClientPool {
	fingerprint := config.getFingerprint()

	// 快速路径：读锁
	m.lock.RLock()
	pool, exists := m.pools[fingerprint]
	m.lock.RUnlock()
	if exists {
		return pool
	}

	// 慢速路径：写锁
	m.lock.Lock()
	defer m.lock.Unlock()

	// 双重检查，防止等待锁期间其他 goroutine 已创建池。
	if pool, exists = m.pools[fingerprint]; exists {
		return pool
	}

	// 解析代理 URL 列表
	proxyURLs := parseProxyURLs(config.ProxyURL)

	// 为每个代理 URL 创建一个 http.Client
	clients := make([]*http.Client, 0, len(proxyURLs))
	for _, proxyURL := range proxyURLs {
		client := m.createClient(config, proxyURL)
		clients = append(clients, client)
	}

	pool = &ProxyClientPool{clients: clients}
	m.pools[fingerprint] = pool
	return pool
}

// createClient 根据配置和单个代理 URL 创建 HTTP 客户端。
func (m *HTTPClientManager) createClient(config *Config, proxyURL string) *http.Client {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   config.ConnectTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     config.ForceAttemptHTTP2,
		MaxIdleConns:          config.MaxIdleConns,
		MaxIdleConnsPerHost:   config.MaxIdleConnsPerHost,
		IdleConnTimeout:       config.IdleConnTimeout,
		TLSHandshakeTimeout:   config.TLSHandshakeTimeout,
		ExpectContinueTimeout: config.ExpectContinueTimeout,
		ResponseHeaderTimeout: config.ResponseHeaderTimeout,
		DisableCompression:    config.DisableCompression,
		WriteBufferSize:       config.WriteBufferSize,
		ReadBufferSize:        config.ReadBufferSize,
	}

	// 设置 HTTP 代理
	if proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			logrus.Warnf("Invalid proxy URL '%s' provided, falling back to environment settings: %v", proxyURL, err)
			transport.Proxy = http.ProxyFromEnvironment
		} else {
			transport.Proxy = http.ProxyURL(parsed)
		}
	} else {
		transport.Proxy = http.ProxyFromEnvironment
	}

	return &http.Client{
		Transport:     transport,
		Timeout:       config.RequestTimeout,
		CheckRedirect: stripSensitiveOnCrossHostRedirect,
	}
}

// parseProxyURLs 将逗号分隔的代理 URL 字符串解析为地址列表。
// 空字符串返回 [""], 表示不使用代理（回退到环境变量）。
// 无效的地址会被跳过并记录警告。
func parseProxyURLs(proxyURLField string) []string {
	if proxyURLField == "" {
		return []string{""}
	}

	parts := strings.Split(proxyURLField, ",")
	var urls []string
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		// 验证 URL 格式
		if _, err := url.Parse(trimmed); err != nil {
			logrus.Warnf("Skipping invalid proxy URL '%s': %v", trimmed, err)
			continue
		}
		urls = append(urls, trimmed)
	}

	// 如果所有地址都无效，回退到环境变量
	if len(urls) == 0 {
		logrus.Warn("All proxy URLs are invalid, falling back to environment settings")
		return []string{""}
	}

	return urls
}

// sensitiveProxyHeaders are custom-named credential headers that proxy channels
// attach to upstream requests (e.g. x-api-key set by the messages-format
// channel's ModifyRequest). Unlike the standard Authorization header, net/http
// does NOT strip these on a cross-host redirect, so without an explicit policy a
// redirect from the operator-configured upstream to another host would leak the
// operator's upstream key to that host (CWE-200 / CWE-522).
var sensitiveProxyHeaders = []string{
	"Authorization",
	"x-api-key",
	"api-key",
	"X-Goog-Api-Key",
	"X-Auth-Token",
}

// stripSensitiveOnCrossHostRedirect removes credential headers when a redirect
// crosses to a different host, mirroring the protection net/http already gives
// the standard Authorization header. It preserves the default redirect cap.
func stripSensitiveOnCrossHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	if len(via) == 0 {
		return nil
	}
	if req.URL.Hostname() != via[0].URL.Hostname() {
		for _, h := range sensitiveProxyHeaders {
			req.Header.Del(h)
		}
	}
	return nil
}

// getFingerprint 生成客户端配置的唯一字符串表示。
func (c *Config) getFingerprint() string {
	return fmt.Sprintf(
		"ct:%.0fs|rt:%.0fs|it:%.0fs|mic:%d|mich:%d|rht:%.0fs|dc:%t|wbs:%d|rbs:%d|fh2:%t|tlst:%.0fs|ect:%.0fs|proxy:%s",
		c.ConnectTimeout.Seconds(),
		c.RequestTimeout.Seconds(),
		c.IdleConnTimeout.Seconds(),
		c.MaxIdleConns,
		c.MaxIdleConnsPerHost,
		c.ResponseHeaderTimeout.Seconds(),
		c.DisableCompression,
		c.WriteBufferSize,
		c.ReadBufferSize,
		c.ForceAttemptHTTP2,
		c.TLSHandshakeTimeout.Seconds(),
		c.ExpectContinueTimeout.Seconds(),
		c.ProxyURL,
	)
}
