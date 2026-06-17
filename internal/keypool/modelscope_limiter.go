package keypool

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// ModelScopeHeaderRemaining 魔塔响应头：模型维度剩余请求次数
const ModelScopeHeaderRemaining = "modelscope-ratelimit-model-requests-remaining"

// ModelScopeLimiter 管理魔塔平台模型维度的请求限流
// 仅针对上游地址包含 api-inference.modelscope.cn 的情况
// 使用内存存储，按天自动过期
type ModelScopeLimiter struct {
	mu     sync.RWMutex
	data   map[string]int       // key 格式: "{keyID}:{model}"
	expiry map[uint]time.Time   // 每个 key 的过期时间（按天）
	stopCh chan struct{}        // 停止清理 goroutine
}

// NewModelScopeLimiter 创建新的魔塔限流器
func NewModelScopeLimiter() *ModelScopeLimiter {
	l := &ModelScopeLimiter{
		data:   make(map[string]int),
		expiry: make(map[uint]time.Time),
		stopCh: make(chan struct{}),
	}
	go l.cleanupLoop()
	return l
}

// Stop 停止后台清理 goroutine
func (l *ModelScopeLimiter) Stop() {
	close(l.stopCh)
}

// IsModelScopeUpstream 判断上游地址是否为魔塔平台
func IsModelScopeUpstream(upstreamURL string) bool {
	return strings.Contains(upstreamURL, "api-inference.modelscope.cn")
}

// TryAcquire 尝试获取指定密钥对指定模型的一次请求配额
// 返回 true 表示成功获取配额（可以继续请求）
// 返回 false 表示该模型次数已用完或数据已过期需重新同步
func (l *ModelScopeLimiter) TryAcquire(keyID uint, model string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	// 检查是否已过期（跨天了），过期则清理并放行
	if expiry, ok := l.expiry[keyID]; ok && time.Now().After(expiry) {
		l.clearKeyData(keyID)
		return true
	}

	key := l.buildKey(keyID, model)
	remaining, ok := l.data[key]
	if !ok {
		// 未知状态，放行（首次请求或数据未同步）
		return true
	}

	if remaining <= 0 {
		return false
	}

	l.data[key] = remaining - 1
	return true
}

// UpdateModelRemaining 更新指定密钥对指定模型的剩余可用次数
func (l *ModelScopeLimiter) UpdateModelRemaining(keyID uint, model string, remaining int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// 设置过期时间为当天结束
	now := time.Now()
	endOfDay := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 0, now.Location())
	l.expiry[keyID] = endOfDay

	key := l.buildKey(keyID, model)
	oldRemaining, exists := l.data[key]
	l.data[key] = remaining

	logrus.WithFields(logrus.Fields{
		"keyID":         keyID,
		"model":         model,
		"old_remaining": oldRemaining,
		"new_remaining": remaining,
		"exists":        exists,
	}).Debug("ModelScope model remaining updated")
}

// UpdateModelRemainingFromHeader 从响应头解析并更新剩余次数
func (l *ModelScopeLimiter) UpdateModelRemainingFromHeader(keyID uint, model string, headerValue string) {
	if headerValue == "" {
		return
	}

	remaining, err := strconv.Atoi(headerValue)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"keyID":        keyID,
			"model":        model,
			"header_value": headerValue,
			"error":        err,
		}).Warn("Failed to parse ModelScope rate limit header")
		return
	}

	l.UpdateModelRemaining(keyID, model, remaining)
}

// buildKey 构建存储键
func (l *ModelScopeLimiter) buildKey(keyID uint, model string) string {
	return strconv.FormatUint(uint64(keyID), 10) + ":" + model
}

// clearKeyData 清理指定 keyID 的所有数据（调用者需持有写锁）
func (l *ModelScopeLimiter) clearKeyData(keyID uint) {
	delete(l.expiry, keyID)
	prefix := strconv.FormatUint(uint64(keyID), 10) + ":"
	for key := range l.data {
		if strings.HasPrefix(key, prefix) {
			delete(l.data, key)
		}
	}
}

// cleanupLoop 定期清理过期的数据
func (l *ModelScopeLimiter) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			l.cleanup()
		case <-l.stopCh:
			return
		}
	}
}

// cleanup 清理过期的数据
func (l *ModelScopeLimiter) cleanup() {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	expiredKeys := make([]uint, 0)

	for keyID, expiry := range l.expiry {
		if now.After(expiry) {
			expiredKeys = append(expiredKeys, keyID)
		}
	}

	for _, keyID := range expiredKeys {
		l.clearKeyData(keyID)
	}

	if len(expiredKeys) > 0 {
		logrus.WithField("expired_keys", len(expiredKeys)).Debug("ModelScope limiter cleaned up expired entries")
	}
}

// GetStats 获取当前内存使用统计（用于监控）
func (l *ModelScopeLimiter) GetStats() map[string]interface{} {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return map[string]interface{}{
		"total_entries": len(l.data),
		"tracked_keys":  len(l.expiry),
	}
}
