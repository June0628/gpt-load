package store

import (
	"errors"
	"time"
)

// ErrNotFound 是在存储中未找到键时返回的错误
var ErrNotFound = errors.New("store: key not found")

// Message 是接收 pub/sub 消息的结构体
type Message struct {
	Channel string
	Payload []byte
}

// Subscription 表示对 pub/sub 通道的活跃订阅
type Subscription interface {
	Channel() <-chan *Message
	Close() error
}

// Store 是通用键值存储接口
type Store interface {
	// Set 存储键值对，可选 TTL
	Set(key string, value []byte, ttl time.Duration) error

	// Get 根据键检索值
	Get(key string) ([]byte, error)

	// Delete 根据键删除值
	Delete(key string) error

	// Del 删除多个键
	Del(keys ...string) error

	// Exists 检查键是否存在于存储中
	Exists(key string) (bool, error)

	// SetNX 在键不存在时设置键值对
	SetNX(key string, value []byte, ttl time.Duration) (bool, error)

	// Expire 为已存在的键设置过期时间
	Expire(key string, ttl time.Duration) error

	// HASH 操作
	HSet(key string, values map[string]any) error
	HGetAll(key string) (map[string]string, error)
	HIncrBy(key, field string, incr int64) (int64, error)

	// LIST 操作
	LPush(key string, values ...any) error
	LRem(key string, count int64, value any) error
	Rotate(key string) (string, error)
	LLen(key string) (int64, error)

	// SET 操作
	SAdd(key string, members ...any) error
	SPopN(key string, count int64) ([]string, error)

	// Close 关闭存储并释放底层资源
	Close() error

	// Publish 向指定通道发送消息
	Publish(channel string, message []byte) error

	// Subscribe 监听指定通道的消息
	Subscribe(channel string) (Subscription, error)

	// Clear 清除所有数据
	Clear() error
}

// Pipeliner 定义批量执行命令的接口
type Pipeliner interface {
	HSet(key string, values map[string]any)
	Exec() error
}

// RedisPipeliner 是 Store 可选实现的接口，提供管道化支持
type RedisPipeliner interface {
	Pipeline() Pipeliner
}
