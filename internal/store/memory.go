package store

import (
	"fmt"
	"strconv"
	"sync"
	"time"
)

// memoryStoreItem 存储键的值和过期时间戳
type memoryStoreItem struct {
	value     []byte
	expiresAt int64 // Unix 纳秒时间戳，0 表示永不过期
}

// MemoryStore 是线程安全的内存键值存储
type MemoryStore struct {
	mu            sync.RWMutex
	data          map[string]any
	muExpiries    sync.RWMutex
	expiries      map[string]int64 // key -> Unix 纳秒过期时间，用于非 memoryStoreItem 类型（hash/list/set）
	muSubscribers sync.RWMutex
	subscribers   map[string]map[chan *Message]struct{}
	stopCleanup   chan struct{}
}

// NewMemoryStore 创建并返回新的 MemoryStore 实例
func NewMemoryStore() *MemoryStore {
	s := &MemoryStore{
		data:        make(map[string]any),
		expiries:    make(map[string]int64),
		subscribers: make(map[string]map[chan *Message]struct{}),
		stopCleanup: make(chan struct{}),
	}
	go s.cleanupExpired()
	return s
}

// cleanupExpired 定期清理过期的 key
func (s *MemoryStore) cleanupExpired() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.deleteExpired()
		case <-s.stopCleanup:
			return
		}
	}
}

// deleteExpired 删除所有过期的 key（包括 memoryStoreItem 和通过 Expire 设置了 TTL 的 hash/list/set）
func (s *MemoryStore) deleteExpired() {
	now := time.Now().UnixNano()

	// 清理 memoryStoreItem 类型的过期 key
	s.mu.Lock()
	for key, rawItem := range s.data {
		if item, ok := rawItem.(memoryStoreItem); ok {
			if item.expiresAt > 0 && now > item.expiresAt {
				delete(s.data, key)
			}
		}
	}
	s.mu.Unlock()

	// 清理通过 Expire 设置了 TTL 的 hash/list/set 类型
	s.muExpiries.Lock()
	var expiredKeys []string
	for key, expiresAt := range s.expiries {
		if expiresAt > 0 && now > expiresAt {
			expiredKeys = append(expiredKeys, key)
			delete(s.expiries, key)
		}
	}
	s.muExpiries.Unlock()

	if len(expiredKeys) > 0 {
		s.mu.Lock()
		for _, key := range expiredKeys {
			delete(s.data, key)
		}
		s.mu.Unlock()
	}
}

// Close 释放资源
func (s *MemoryStore) Close() error {
	close(s.stopCleanup)
	return nil
}

// Set 存储键值对
func (s *MemoryStore) Set(key string, value []byte, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var expiresAt int64
	if ttl > 0 {
		expiresAt = time.Now().UnixNano() + ttl.Nanoseconds()
	}

	s.data[key] = memoryStoreItem{
		value:     value,
		expiresAt: expiresAt,
	}
	return nil
}

// Get 根据键检索值
func (s *MemoryStore) Get(key string) ([]byte, error) {
	s.mu.RLock()
	rawItem, exists := s.data[key]
	s.mu.RUnlock()

	if !exists {
		return nil, ErrNotFound
	}

	item, ok := rawItem.(memoryStoreItem)
	if !ok {
		return nil, fmt.Errorf("type mismatch: key '%s' holds a different data type", key)
	}

	if item.expiresAt > 0 && time.Now().UnixNano() > item.expiresAt {
		s.mu.Lock()
		delete(s.data, key)
		s.mu.Unlock()
		return nil, ErrNotFound
	}

	return item.value, nil
}

// Delete 根据键删除值，同时清理对应的过期条目
func (s *MemoryStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.muExpiries.Lock()
	defer s.muExpiries.Unlock()
	delete(s.data, key)
	delete(s.expiries, key)
	return nil
}

// Del 根据多个键删除值，同时清理对应的过期条目
func (s *MemoryStore) Del(keys ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.muExpiries.Lock()
	defer s.muExpiries.Unlock()
	for _, key := range keys {
		delete(s.data, key)
		delete(s.expiries, key)
	}
	return nil
}

// Exists 检查键是否存在
func (s *MemoryStore) Exists(key string) (bool, error) {
	s.mu.RLock()
	rawItem, exists := s.data[key]
	s.mu.RUnlock()

	if !exists {
		return false, nil
	}

	if item, ok := rawItem.(memoryStoreItem); ok {
		if item.expiresAt > 0 && time.Now().UnixNano() > item.expiresAt {
			s.mu.Lock()
			delete(s.data, key)
			s.mu.Unlock()
			return false, nil
		}
	}

	return true, nil
}

// SetNX 在键不存在时设置键值对
func (s *MemoryStore) SetNX(key string, value []byte, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rawItem, exists := s.data[key]
	if exists {
		if item, ok := rawItem.(memoryStoreItem); ok {
			if item.expiresAt == 0 || time.Now().UnixNano() < item.expiresAt {
				return false, nil
			}
		} else {
			// 键存在但不是简单的 K/V 项，视为已存在
			return false, nil
		}
	}

	// 键不存在或已过期，可以设置
	var expiresAt int64
	if ttl > 0 {
		expiresAt = time.Now().UnixNano() + ttl.Nanoseconds()
	}
	s.data[key] = memoryStoreItem{
		value:     value,
		expiresAt: expiresAt,
	}
	return true, nil
}

// Expire 为已存在的键设置过期时间
func (s *MemoryStore) Expire(key string, ttl time.Duration) error {
	s.muExpiries.Lock()
	defer s.muExpiries.Unlock()
	s.expiries[key] = time.Now().UnixNano() + ttl.Nanoseconds()
	return nil
}

// isExpiredByExpiry 检查通过 Expire 设置了 TTL 的 key 是否已过期
func (s *MemoryStore) isExpiredByExpiry(key string) bool {
	s.muExpiries.RLock()
	expiresAt, ok := s.expiries[key]
	s.muExpiries.RUnlock()
	if !ok {
		return false
	}
	if expiresAt > 0 && time.Now().UnixNano() > expiresAt {
		return true
	}
	return false
}

// isExpiredByExpiryLocked 检查通过 Expire 设置了 TTL 的 key 是否已过期
// 调用者必须已持有 s.mu（读或写锁），此函数内部自行获取 muExpiries 读锁
func (s *MemoryStore) isExpiredByExpiryLocked(key string) bool {
	return s.isExpiredByExpiry(key)
}

// --- HASH 操作 ---

func (s *MemoryStore) HSet(key string, values map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var hash map[string]string
	rawHash, exists := s.data[key]
	if !exists || s.isExpiredByExpiryLocked(key) {
		hash = make(map[string]string)
		s.data[key] = hash
		if exists {
			s.muExpiries.Lock()
			delete(s.expiries, key)
			s.muExpiries.Unlock()
		}
	} else {
		var ok bool
		hash, ok = rawHash.(map[string]string)
		if !ok {
			return fmt.Errorf("type mismatch: key '%s' holds a different data type", key)
		}
	}

	for field, value := range values {
		hash[field] = fmt.Sprint(value)
	}
	return nil
}

func (s *MemoryStore) HGetAll(key string) (map[string]string, error) {
	if s.isExpiredByExpiry(key) {
		s.mu.Lock()
		delete(s.data, key)
		s.mu.Unlock()
		s.muExpiries.Lock()
		delete(s.expiries, key)
		s.muExpiries.Unlock()
		return make(map[string]string), nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	rawHash, exists := s.data[key]
	if !exists {
		return make(map[string]string), nil
	}

	hash, ok := rawHash.(map[string]string)
	if !ok {
		return nil, fmt.Errorf("type mismatch: key '%s' holds a different data type", key)
	}

	result := make(map[string]string, len(hash))
	for k, v := range hash {
		result[k] = v
	}

	return result, nil
}

func (s *MemoryStore) HIncrBy(key, field string, incr int64) (int64, error) {
	if s.isExpiredByExpiry(key) {
		s.mu.Lock()
		delete(s.data, key)
		s.mu.Unlock()
		s.muExpiries.Lock()
		delete(s.expiries, key)
		s.muExpiries.Unlock()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var hash map[string]string
	rawHash, exists := s.data[key]
	if !exists {
		hash = make(map[string]string)
		s.data[key] = hash
	} else {
		var ok bool
		hash, ok = rawHash.(map[string]string)
		if !ok {
			return 0, fmt.Errorf("type mismatch: key '%s' holds a different data type", key)
		}
	}

	currentVal, _ := strconv.ParseInt(hash[field], 10, 64)
	newVal := currentVal + incr
	hash[field] = strconv.FormatInt(newVal, 10)

	return newVal, nil
}

// --- LIST 操作 ---

func (s *MemoryStore) LPush(key string, values ...any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var list []string
	rawList, exists := s.data[key]
	if !exists || s.isExpiredByExpiryLocked(key) {
		list = make([]string, 0)
		if exists {
			s.muExpiries.Lock()
			delete(s.expiries, key)
			s.muExpiries.Unlock()
		}
	} else {
		var ok bool
		list, ok = rawList.([]string)
		if !ok {
			return fmt.Errorf("type mismatch: key '%s' holds a different data type", key)
		}
	}

	strValues := make([]string, len(values))
	for i, v := range values {
		strValues[i] = fmt.Sprint(v)
	}

	s.data[key] = append(strValues, list...) // 前插
	return nil
}

func (s *MemoryStore) LRem(key string, count int64, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rawList, exists := s.data[key]
	if !exists {
		return nil
	}
	if s.isExpiredByExpiryLocked(key) {
		delete(s.data, key)
		s.muExpiries.Lock()
		delete(s.expiries, key)
		s.muExpiries.Unlock()
		return nil
	}

	list, ok := rawList.([]string)
	if !ok {
		return fmt.Errorf("type mismatch: key '%s' holds a different data type", key)
	}

	strValue := fmt.Sprint(value)
	newList := make([]string, 0, len(list))

	if count != 0 {
		return fmt.Errorf("LRem with non-zero count is not implemented in MemoryStore")
	}

	for _, item := range list {
		if item != strValue {
			newList = append(newList, item)
		}
	}
	s.data[key] = newList
	return nil
}

func (s *MemoryStore) Rotate(key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rawList, exists := s.data[key]
	if !exists {
		return "", ErrNotFound
	}
	if s.isExpiredByExpiryLocked(key) {
		delete(s.data, key)
		s.muExpiries.Lock()
		delete(s.expiries, key)
		s.muExpiries.Unlock()
		return "", ErrNotFound
	}

	list, ok := rawList.([]string)
	if !ok {
		return "", fmt.Errorf("type mismatch: key '%s' holds a different data type", key)
	}

	if len(list) == 0 {
		return "", ErrNotFound
	}

	lastIndex := len(list) - 1
	item := list[lastIndex]

	// "LPUSH"
	newList := append([]string{item}, list[:lastIndex]...)
	s.data[key] = newList

	return item, nil
}

// LLen 返回列表的长度
func (s *MemoryStore) LLen(key string) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rawList, exists := s.data[key]
	if !exists {
		return 0, nil
	}

	list, ok := rawList.([]string)
	if !ok {
		return 0, fmt.Errorf("type mismatch: key '%s' holds a different data type", key)
	}

	// 检查过期，持有读锁时不能删除，只能返回 0
	// 写操作（如 LRem、LPop）时会持有写锁清理过期 key
	if s.isExpiredByExpiryLocked(key) {
		return 0, nil
	}

	return int64(len(list)), nil
}

// --- SET 操作 ---

// SAdd 向集合添加成员
func (s *MemoryStore) SAdd(key string, members ...any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var set map[string]struct{}
	rawSet, exists := s.data[key]
	if !exists || s.isExpiredByExpiryLocked(key) {
		set = make(map[string]struct{})
		s.data[key] = set
		if exists {
			s.muExpiries.Lock()
			delete(s.expiries, key)
			s.muExpiries.Unlock()
		}
	} else {
		var ok bool
		set, ok = rawSet.(map[string]struct{})
		if !ok {
			return fmt.Errorf("type mismatch: key '%s' holds a different data type", key)
		}
	}

	for _, member := range members {
		set[fmt.Sprint(member)] = struct{}{}
	}
	return nil
}

// SPopN 从集合中随机移除并返回指定数量的成员
func (s *MemoryStore) SPopN(key string, count int64) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rawSet, exists := s.data[key]
	if !exists {
		return []string{}, nil
	}
	if s.isExpiredByExpiryLocked(key) {
		delete(s.data, key)
		s.muExpiries.Lock()
		delete(s.expiries, key)
		s.muExpiries.Unlock()
		return []string{}, nil
	}

	set, ok := rawSet.(map[string]struct{})
	if !ok {
		return nil, fmt.Errorf("type mismatch: key '%s' holds a different data type", key)
	}

	if count > int64(len(set)) {
		count = int64(len(set))
	}

	popped := make([]string, 0, count)
	for member := range set {
		if int64(len(popped)) >= count {
			break
		}
		popped = append(popped, member)
		delete(set, member)
	}

	return popped, nil
}

// --- Pub/Sub 操作 ---

// memorySubscription 实现内存存储的 Subscription 接口
type memorySubscription struct {
	store   *MemoryStore
	channel string
	msgChan chan *Message
}

// Channel 返回订阅的消息通道
func (ms *memorySubscription) Channel() <-chan *Message {
	return ms.msgChan
}

// Close 从存储中移除订阅
func (ms *memorySubscription) Close() error {
	ms.store.muSubscribers.Lock()
	defer ms.store.muSubscribers.Unlock()

	if subs, ok := ms.store.subscribers[ms.channel]; ok {
		delete(subs, ms.msgChan)
		if len(subs) == 0 {
			delete(ms.store.subscribers, ms.channel)
		}
	}
	close(ms.msgChan)
	return nil
}

// Publish 向通道的所有订阅者发送消息
func (s *MemoryStore) Publish(channel string, message []byte) error {
	s.muSubscribers.RLock()
	defer s.muSubscribers.RUnlock()

	msg := &Message{
		Channel: channel,
		Payload: message,
	}

	if subs, ok := s.subscribers[channel]; ok {
		for subCh := range subs {
			go func(c chan *Message) {
				select {
				case c <- msg:
				case <-time.After(1 * time.Second):
				}
			}(subCh)
		}
	}
	return nil
}

// Subscribe 监听指定通道的消息
func (s *MemoryStore) Subscribe(channel string) (Subscription, error) {
	s.muSubscribers.Lock()
	defer s.muSubscribers.Unlock()

	msgChan := make(chan *Message, 10) // 带缓冲的通道

	if _, ok := s.subscribers[channel]; !ok {
		s.subscribers[channel] = make(map[chan *Message]struct{})
	}
	s.subscribers[channel][msgChan] = struct{}{}

	sub := &memorySubscription{
		store:   s,
		channel: channel,
		msgChan: msgChan,
	}

	return sub, nil
}

// Clear 清除所有数据
func (s *MemoryStore) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.muExpiries.Lock()
	defer s.muExpiries.Unlock()

	// 清除所有数据
	s.data = make(map[string]any)
	s.expiries = make(map[string]int64)

	return nil
}
