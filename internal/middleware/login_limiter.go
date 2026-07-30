package middleware

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/response"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

const (
	// loginMaxAttempts 触发锁定前允许的连续失败次数
	loginMaxAttempts = 10
	// loginAttemptWindow 统计失败次数的滑动窗口
	loginAttemptWindow = 5 * time.Minute
	// loginLockoutDuration 达到失败上限后的锁定时长
	loginLockoutDuration = 15 * time.Minute
	// loginTrackerMaxEntries 触发过期清理的条目阈值
	loginTrackerMaxEntries = 1024
)

type loginAttempt struct {
	count       int
	windowStart time.Time
	lockedUntil time.Time
}

type loginAttemptTracker struct {
	mu       sync.Mutex
	attempts map[string]*loginAttempt
}

func newLoginAttemptTracker() *loginAttemptTracker {
	return &loginAttemptTracker{attempts: make(map[string]*loginAttempt)}
}

// lockedUntil 返回该来源的锁定截止时间，未锁定时返回零值
func (t *loginAttemptTracker) lockedUntil(source string) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()

	attempt, exists := t.attempts[source]
	if !exists {
		return time.Time{}
	}
	if time.Now().Before(attempt.lockedUntil) {
		return attempt.lockedUntil
	}
	return time.Time{}
}

func (t *loginAttemptTracker) recordFailure(source string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	attempt, exists := t.attempts[source]
	if !exists || now.Sub(attempt.windowStart) > loginAttemptWindow {
		attempt = &loginAttempt{windowStart: now}
		t.attempts[source] = attempt
	}

	attempt.count++
	if attempt.count >= loginMaxAttempts {
		attempt.lockedUntil = now.Add(loginLockoutDuration)
		attempt.count = 0
		attempt.windowStart = now
		logrus.WithField("source", source).Warn("Too many failed login attempts, temporarily locking out source")
	}

	if len(t.attempts) > loginTrackerMaxEntries {
		t.pruneLocked(now)
	}
}

func (t *loginAttemptTracker) recordSuccess(source string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.attempts, source)
}

// pruneLocked 清理已过期的记录，调用者需持有锁
func (t *loginAttemptTracker) pruneLocked(now time.Time) {
	for source, attempt := range t.attempts {
		if now.Before(attempt.lockedUntil) {
			continue
		}
		if now.Sub(attempt.windowStart) > loginAttemptWindow {
			delete(t.attempts, source)
		}
	}
}

// LoginRateLimiter 限制单个来源 IP 的登录尝试频率，缓解认证密钥爆破
func LoginRateLimiter() gin.HandlerFunc {
	tracker := newLoginAttemptTracker()

	return func(c *gin.Context) {
		source := c.ClientIP()

		if lockedUntil := tracker.lockedUntil(source); !lockedUntil.IsZero() {
			retryAfter := int(time.Until(lockedUntil).Seconds()) + 1
			c.Header("Retry-After", strconv.Itoa(retryAfter))
			response.Error(c, app_errors.ErrTooManyRequests)
			c.Abort()
			return
		}

		c.Next()

		switch c.Writer.Status() {
		case http.StatusOK:
			tracker.recordSuccess(source)
		case http.StatusUnauthorized:
			tracker.recordFailure(source)
		}
	}
}
