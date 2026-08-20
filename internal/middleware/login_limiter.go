package middleware

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/response"
	"gpt-load/internal/store"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// formatLockoutValue 将锁定截止时间编码为存储值
func formatLockoutValue(lockedUntil time.Time) []byte {
	return []byte(strconv.FormatInt(lockedUntil.Unix(), 10))
}

// parseLockoutValue 从存储值解析锁定截止时间，失败返回零值
func parseLockoutValue(raw []byte) time.Time {
	if len(raw) == 0 {
		return time.Time{}
	}
	ts, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(ts, 0)
}

// isStoreNotFound 判断存储错误是否为 key 不存在
func isStoreNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}

const (
	// loginMaxAttempts 触发锁定前允许的连续失败次数
	loginMaxAttempts = 10
	// loginAttemptWindow 统计失败次数的窗口时长
	loginAttemptWindow = 5 * time.Minute
	// loginLockoutDuration 达到失败上限后的锁定时长
	loginLockoutDuration = 15 * time.Minute

	// loginFailKeyPrefix 失败计数 key 前缀，后接来源 IP
	loginFailKeyPrefix = "login:fail:"
	// loginLockKeyPrefix 锁定标记 key 前缀，后接来源 IP
	loginLockKeyPrefix = "login:lock:"
	// loginFailField hash 中失败计数的字段名
	loginFailField = "count"
)

// LoginRateLimiter 限制单个来源 IP 的登录尝试频率，缓解认证密钥爆破。
// 计数与锁定状态存储在 store 中（Redis 或内存），多实例部署时可共享计数。
func LoginRateLimiter(s store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		source := c.ClientIP()
		lockKey := loginLockKeyPrefix + source
		failKey := loginFailKeyPrefix + source

		// 检查是否处于锁定状态
		lockedUntil := time.Time{}
		raw, err := s.Get(lockKey)
		if err != nil && !isStoreNotFound(err) {
			// 存储异常（非 key 不存在）时不阻断登录，仅记录日志
			logrus.WithError(err).WithField("source", source).Warn("Failed to check login lock status")
		} else if err == nil {
			lockedUntil = parseLockoutValue(raw)
		}

		if !lockedUntil.IsZero() && time.Now().Before(lockedUntil) {
			retryAfter := int(time.Until(lockedUntil).Seconds()) + 1
			if retryAfter < 1 {
				retryAfter = 1
			}
			c.Header("Retry-After", strconv.Itoa(retryAfter))
			response.Error(c, app_errors.ErrTooManyRequests)
			c.Abort()
			return
		}

		c.Next()

		switch c.Writer.Status() {
		case http.StatusOK:
			// 登录成功，清除失败计数
			if err := s.Delete(failKey); err != nil {
				logrus.WithError(err).WithField("source", source).Warn("Failed to clear login fail count")
			}
		case http.StatusUnauthorized:
			// 登录失败，原子递增计数
			count, err := s.HIncrBy(failKey, loginFailField, 1)
			if err != nil {
				logrus.WithError(err).WithField("source", source).Warn("Failed to increment login fail count")
				return
			}
			// 每次失败都刷新窗口过期时间，确保计数不会因 TTL 过期而丢失
			if err := s.Expire(failKey, loginAttemptWindow); err != nil {
				logrus.WithError(err).WithField("source", source).Warn("Failed to set login fail window TTL")
			}
			// 达到阈值，写入锁定标记（存储锁定截止时间戳，便于计算剩余时间）
			if count >= loginMaxAttempts {
				lockedUntil := time.Now().Add(loginLockoutDuration)
				if err := s.Set(lockKey, formatLockoutValue(lockedUntil), loginLockoutDuration); err != nil {
					logrus.WithError(err).WithField("source", source).Warn("Failed to set login lock")
				} else {
					logrus.WithField("source", source).Warn("Too many failed login attempts, temporarily locking out source")
				}
				// 清除计数，锁定结束后重新计数
				if err := s.Delete(failKey); err != nil {
					logrus.WithError(err).WithField("source", source).Warn("Failed to clear login fail count after lockout")
				}
			}
		}
	}
}
