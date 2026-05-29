package httpclient

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
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
	ProxyURL              string
}

// HTTPClientManager 管理 HTTP 客户端的生命周期。
// 基于配置指纹创建和缓存客户端，确保相同配置的客户端被复用。
type HTTPClientManager struct {
	clients map[string]*http.Client
	lock    sync.RWMutex
}

// RemoveClient 根据指纹关闭并移除缓存的客户端。
// 如果未找到客户端则返回 false。
func (m *HTTPClientManager) RemoveClient(fingerprint string) bool {
	m.lock.Lock()
	defer m.lock.Unlock()
	client, exists := m.clients[fingerprint]
	if !exists {
		return false
	}
	// 关闭 transport 以释放底层连接
	if transport, ok := client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
	delete(m.clients, fingerprint)
	return true
}

// Close 关闭所有缓存的客户端并释放底层连接。
func (m *HTTPClientManager) Close() {
	m.lock.Lock()
	defer m.lock.Unlock()
	for _, client := range m.clients {
		if transport, ok := client.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
	m.clients = make(map[string]*http.Client)
}

// NewHTTPClientManager 创建新的客户端管理器。
func NewHTTPClientManager() *HTTPClientManager {
	return &HTTPClientManager{
		clients: make(map[string]*http.Client),
	}
}

// GetClient 返回匹配给定配置的 HTTP 客户端。
// 如果缓存中已存在匹配的客户端则直接返回。
// 否则创建新客户端、缓存并返回。
func (m *HTTPClientManager) GetClient(config *Config) *http.Client {
	fingerprint := config.getFingerprint()

	// 快速路径：读锁
	m.lock.RLock()
	client, exists := m.clients[fingerprint]
	m.lock.RUnlock()
	if exists {
		return client
	}

	// 慢速路径：写锁
	m.lock.Lock()
	defer m.lock.Unlock()

	// 双重检查，防止等待锁期间其他 goroutine 已创建客户端。
	if client, exists = m.clients[fingerprint]; exists {
		return client
	}

	// 使用指定配置创建新的 transport 和 client。
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

	// 设置 HTTP 代理。
	if config.ProxyURL != "" {
		proxyURL, err := url.Parse(config.ProxyURL)
		if err != nil {
			logrus.Warnf("Invalid proxy URL '%s' provided, falling back to environment settings: %v", config.ProxyURL, err)
			transport.Proxy = http.ProxyFromEnvironment
		} else {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	} else {
		transport.Proxy = http.ProxyFromEnvironment
	}

	newClient := &http.Client{
		Transport:     transport,
		Timeout:       config.RequestTimeout,
		CheckRedirect: stripSensitiveOnCrossHostRedirect,
	}

	m.clients[fingerprint] = newClient
	return newClient
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
