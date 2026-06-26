package keypool

import (
	"errors"
	"fmt"
	"gpt-load/internal/config"
	"gpt-load/internal/encryption"
	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/models"
	"gpt-load/internal/store"
	"gpt-load/internal/utils"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// asyncSemaphore 限制异步操作的最大并发 goroutine 数量，防止流量高峰期出现 goroutine 风暴
var asyncSemaphore = make(chan struct{}, 100)

// syncSemaphore 限制同步余额更新操作的并发，与异步信号量隔离避免相互阻塞
var syncSemaphore = make(chan struct{}, 50)

// beijingLocation 北京时区，用于每日请求限制的日期计算
var beijingLocation = time.FixedZone("CST", 8*3600)

type KeyProvider struct {
	db                *gorm.DB
	store             store.Store
	settingsManager   *config.SystemSettingsManager
	encryptionSvc     encryption.Service
	modelScopeLimiter *ModelScopeLimiter
}

// NewProvider 创建一个新的 KeyProvider 实例。
func NewProvider(db *gorm.DB, store store.Store, settingsManager *config.SystemSettingsManager, encryptionSvc encryption.Service) *KeyProvider {
	return &KeyProvider{
		db:                db,
		store:             store,
		settingsManager:   settingsManager,
		encryptionSvc:     encryptionSvc,
		modelScopeLimiter: NewModelScopeLimiter(),
	}
}

// SelectKey 为指定的分组原子性地选择并轮换一个可用的 APIKey。
func (p *KeyProvider) SelectKey(groupID uint) (*models.APIKey, error) {
	activeKeysListKey := fmt.Sprintf("group:%d:active_keys", groupID)

	// 1. 原子性地从列表中轮换密钥ID
	keyIDStr, err := p.store.Rotate(activeKeysListKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, app_errors.ErrNoActiveKeys
		}
		return nil, fmt.Errorf("failed to rotate key from store: %w", err)
	}

	keyID, err := strconv.ParseUint(keyIDStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse key ID '%s': %w", keyIDStr, err)
	}

	// 2. 从HASH中获取密钥详情
	keyHashKey := fmt.Sprintf("key:%d", keyID)
	keyDetails, err := p.store.HGetAll(keyHashKey)
	if err != nil {
		return nil, fmt.Errorf("failed to get key details for key ID %d: %w", keyID, err)
	}

	// 3. 手动将map反序列化为APIKey结构体
	failureCount, err := strconv.ParseInt(keyDetails["failure_count"], 10, 64)
	if err != nil {
		logrus.WithFields(logrus.Fields{"keyID": keyID, "raw": keyDetails["failure_count"], "error": err}).
			Warn("Failed to parse failure_count from store cache, defaulting to 0")
		failureCount = 0
	}
	createdAt, err := strconv.ParseInt(keyDetails["created_at"], 10, 64)
	if err != nil {
		logrus.WithFields(logrus.Fields{"keyID": keyID, "raw": keyDetails["created_at"], "error": err}).
			Warn("Failed to parse created_at from store cache, defaulting to 0")
		createdAt = 0
	}

	// 解密密钥值供channel使用
	encryptedKeyValue := keyDetails["key_string"]
	decryptedKeyValue, err := p.encryptionSvc.Decrypt(encryptedKeyValue)
	if err != nil {
		// 如果解密失败，尝试直接使用原始值（兼容未加密的密钥）
		logrus.WithFields(logrus.Fields{
			"keyID": keyID,
			"error": err,
		}).Debug("Failed to decrypt key value, using as-is for backward compatibility")
		decryptedKeyValue = encryptedKeyValue
	}

	apiKey := &models.APIKey{
		ID:           uint(keyID),
		KeyValue:     decryptedKeyValue,
		Status:       keyDetails["status"],
		FailureCount: failureCount,
		GroupID:      groupID,
		CreatedAt:    time.Unix(createdAt, 0),
	}

	return apiKey, nil
}

// SelectKeyWithModelCheck 为指定的分组选择密钥，同时检查模型维度限流（仅针对魔塔平台）
// upstreamURL: 上游地址，用于判断是否魔塔平台
// model: 请求模型名称
// maxRetries: 最大重试次数
func (p *KeyProvider) SelectKeyWithModelCheck(groupID uint, upstreamURL string, model string, maxRetries int) (*models.APIKey, error) {
	// 非魔塔平台，直接走原有逻辑
	if !IsModelScopeUpstream(upstreamURL) {
		return p.SelectKey(groupID)
	}

	// 魔塔平台，需要检查模型维度限流
	// 记录本轮已尝试过的 keyID，避免 Rotate 循环返回同一批密钥
	triedKeys := make(map[uint]struct{})

	for i, trueAttempts := 0, 0; trueAttempts <= maxRetries; i++ {
		if i > maxRetries*3 {
			// 避免无限循环：所有已尝试的 key 都被 Rotate 重复返回，跳出
			logrus.WithFields(logrus.Fields{
				"groupID":   groupID,
				"model":     model,
				"triedKeys": len(triedKeys),
			}).Warn("SelectKeyWithModelCheck: all keys tried but Rotate keeps returning duplicates")
			break
		}

		apiKey, err := p.SelectKey(groupID)
		if err != nil {
			return nil, err
		}

		// 如果该 key 已尝试过，跳过但不消耗 trueAttempts（Rotate 在并发下可能重复返回同一密钥）
		if _, ok := triedKeys[apiKey.ID]; ok {
			continue
		}
		triedKeys[apiKey.ID] = struct{}{}
		trueAttempts++

		// 尝试获取配额（原子操作：检查 + 扣减）
		if p.modelScopeLimiter.TryAcquire(apiKey.ID, model) {
			return apiKey, nil
		}

		// 该密钥对该模型次数已用完，记录日志并继续尝试下一个
		logrus.WithFields(logrus.Fields{
			"keyID": apiKey.ID,
			"model": model,
			"retry": i + 1,
		}).Debug("Key has reached model-specific limit, trying next key")
	}

	return nil, app_errors.ErrNoKeysAvailable
}

// UpdateModelScopeRemaining 更新魔塔平台模型维度剩余次数（从响应头）
func (p *KeyProvider) UpdateModelScopeRemaining(apiKey *models.APIKey, model string, headerValue string) {
	if p.modelScopeLimiter == nil {
		return
	}
	p.modelScopeLimiter.UpdateModelRemainingFromHeader(apiKey.ID, model, headerValue)
}

// GetModelScopeLimiterStats 获取魔塔限流器统计信息
func (p *KeyProvider) GetModelScopeLimiterStats() map[string]interface{} {
	if p.modelScopeLimiter == nil {
		return map[string]interface{}{"enabled": false}
	}
	stats := p.modelScopeLimiter.GetStats()
	stats["enabled"] = true
	return stats
}

// UpdateStatus 异步地提交一个 Key 状态更新任务。
func (p *KeyProvider) UpdateStatus(apiKey *models.APIKey, group *models.Group, isSuccess bool, errorMessage string) {
	go func() {
		asyncSemaphore <- struct{}{}        // 获取信号量，超过限制时阻塞等待
		defer func() { <-asyncSemaphore }() // 释放信号量
		keyHashKey := fmt.Sprintf("key:%d", apiKey.ID)
		activeKeysListKey := fmt.Sprintf("group:%d:active_keys", group.ID)

		if isSuccess {
			if err := p.handleSuccess(apiKey.ID, keyHashKey, activeKeysListKey); err != nil {
				logrus.WithFields(logrus.Fields{"keyID": apiKey.ID, "error": err}).Error("Failed to handle key success")
			}
		} else {
			if app_errors.IsUnCounted(errorMessage) {
				logrus.WithFields(logrus.Fields{
					"keyID": apiKey.ID,
					"error": errorMessage,
				}).Debug("Uncounted error, skipping failure handling")
			} else {
				if err := p.handleFailure(apiKey, group, keyHashKey, activeKeysListKey); err != nil {
					logrus.WithFields(logrus.Fields{"keyID": apiKey.ID, "error": err}).Error("Failed to handle key failure")
				}
			}
		}
	}()
}

// executeTransactionWithRetry 用重试机制包装数据库事务
// 支持 SQLite (database is locked) 和 MySQL (1205 Lock wait timeout, 1213 Deadlock) 的自动重试
func (p *KeyProvider) executeTransactionWithRetry(operation func(tx *gorm.DB) error) error {
	const maxRetries = 3
	const baseDelay = 50 * time.Millisecond
	const maxJitter = 150 * time.Millisecond
	var err error

	for i := range maxRetries {
		err = p.db.Transaction(operation)
		if err == nil {
			return nil
		}

		if isRetryableDBError(err) {
			jitter := time.Duration(rand.Intn(int(maxJitter)))
			totalDelay := baseDelay + jitter
			logrus.Debugf("Database retriable error, retrying in %v... (attempt %d/%d): %v", totalDelay, i+1, maxRetries, err)
			time.Sleep(totalDelay)
			continue
		}

		break
	}

	return err
}

// isRetryableDBError 判断数据库错误是否可重试（SQLite 锁、MySQL 锁等待超时和死锁）
func isRetryableDBError(err error) bool {
	if err == nil {
		return false
	}

	// SQLite: database is locked
	if strings.Contains(err.Error(), "database is locked") {
		return true
	}

	// MySQL: 1205 (Lock wait timeout exceeded), 1213 (Deadlock found)
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		if mysqlErr.Number == 1205 || mysqlErr.Number == 1213 {
			return true
		}
	}

	return false
}

func (p *KeyProvider) handleSuccess(keyID uint, keyHashKey, activeKeysListKey string) error {
	keyDetails, err := p.store.HGetAll(keyHashKey)
	if err != nil {
		return fmt.Errorf("failed to get key details from store: %w", err)
	}

	failureCount, _ := strconv.ParseInt(keyDetails["failure_count"], 10, 64)
	isActive := keyDetails["status"] == models.KeyStatusActive

	if failureCount == 0 && isActive {
		return nil
	}

	needRecover := false
	err = p.executeTransactionWithRetry(func(tx *gorm.DB) error {
		var key models.APIKey
		if err := tx.Set("gorm:query_option", "FOR UPDATE").First(&key, keyID).Error; err != nil {
			return fmt.Errorf("failed to lock key %d for update: %w", keyID, err)
		}

		updates := map[string]any{"failure_count": 0}
		if !isActive {
			updates["status"] = models.KeyStatusActive
		}

		if err := tx.Model(&key).Updates(updates).Error; err != nil {
			return fmt.Errorf("failed to update key in DB: %w", err)
		}

		needRecover = !isActive
		return nil
	})
	if err != nil {
		return err
	}

	// 事务提交后重读 DB 最终状态，避免并发 handleFailure 的缓存写入覆盖正确的 success 状态
	var finalKey models.APIKey
	if err := p.db.First(&finalKey, keyID).Error; err != nil {
		logrus.WithFields(logrus.Fields{"keyID": keyID, "error": err}).Warn("Failed to re-read key from DB for cache sync, using transaction values")
		finalKey.FailureCount = 0
		finalKey.Status = models.KeyStatusActive
	}

	cacheUpdates := map[string]any{
		"failure_count": finalKey.FailureCount,
		"status":        finalKey.Status,
	}
	if err := p.store.HSet(keyHashKey, cacheUpdates); err != nil {
		return fmt.Errorf("failed to update key details in store: %w", err)
	}

	// 仅当 DB 最终状态为 active 且之前不是 active 时，才需要恢复到活跃列表
	if needRecover && finalKey.Status == models.KeyStatusActive {
		logrus.WithField("keyID", keyID).Debug("Key has recovered and is being restored to active pool.")
		if err := p.store.LRem(activeKeysListKey, 0, keyID); err != nil {
			return fmt.Errorf("failed to LRem key before LPush on recovery: %w", err)
		}
		if err := p.store.LPush(activeKeysListKey, keyID); err != nil {
			return fmt.Errorf("failed to LPush key back to active list: %w", err)
		}
	}

	return nil
}

func (p *KeyProvider) handleFailure(apiKey *models.APIKey, group *models.Group, keyHashKey, activeKeysListKey string) error {
	keyDetails, err := p.store.HGetAll(keyHashKey)
	if err != nil {
		return fmt.Errorf("failed to get key details from store: %w", err)
	}

	if keyDetails["status"] == models.KeyStatusInvalid {
		return nil
	}

	// 获取该分组的有效配置
	blacklistThreshold := group.EffectiveConfig.BlacklistThreshold

	var shouldBlacklist bool
	var newFailureCount int64

	err = p.executeTransactionWithRetry(func(tx *gorm.DB) error {
		var key models.APIKey
		if err := tx.Set("gorm:query_option", "FOR UPDATE").First(&key, apiKey.ID).Error; err != nil {
			return fmt.Errorf("failed to lock key %d for update: %w", apiKey.ID, err)
		}

		// 使用 DB 锁定行的最新值计算失败次数，避免缓存旧值导致的并发丢失递增
		newFailureCount = key.FailureCount + 1

		updates := map[string]any{"failure_count": newFailureCount}

		// 检查是否应该将密钥加入黑名单
		// 条件：1. 黑名单阈值 > 0 且失败次数达到阈值
		//       2. 分组没有开启"密钥不失效"选项
		shouldBlacklist = blacklistThreshold > 0 && newFailureCount >= int64(blacklistThreshold) && !group.KeyNeverExpires

		if shouldBlacklist {
			updates["status"] = models.KeyStatusInvalid
		}

		if err := tx.Model(&key).Updates(updates).Error; err != nil {
			return fmt.Errorf("failed to update key stats in DB: %w", err)
		}

		return nil
	})
	if err != nil {
		return err
	}

	// 事务提交后重读 DB 最终状态，避免并发 handleSuccess 的缓存写入覆盖正确的 failure 状态
	var finalKey models.APIKey
	if err := p.db.First(&finalKey, apiKey.ID).Error; err != nil {
		logrus.WithFields(logrus.Fields{"keyID": apiKey.ID, "error": err}).Warn("Failed to re-read key from DB for cache sync, using transaction values")
		finalKey.FailureCount = newFailureCount
		if shouldBlacklist {
			finalKey.Status = models.KeyStatusInvalid
		}
	}

	// 当密钥被拉黑时，先从活跃列表移除（LRem），再更新缓存状态（HSet）
	// 这样即使 HSet 失败，密钥也不会被后续 SelectKey 选中
	// 如果 LRem 失败，直接返回错误，下次 handleFailure 会重试 LRem
	if shouldBlacklist && finalKey.Status == models.KeyStatusInvalid {
		logrus.WithFields(logrus.Fields{"keyID": apiKey.ID, "threshold": blacklistThreshold}).Warn("Key has reached blacklist threshold, disabling.")
		if err := p.store.LRem(activeKeysListKey, 0, apiKey.ID); err != nil {
			return fmt.Errorf("failed to LRem key from active list: %w", err)
		}
	}

	// 写入 DB 的最终值到缓存以保持一致
	cacheUpdates := map[string]any{
		"failure_count": finalKey.FailureCount,
		"status":        finalKey.Status,
	}
	if err := p.store.HSet(keyHashKey, cacheUpdates); err != nil {
		logrus.WithFields(logrus.Fields{"keyID": apiKey.ID, "error": err}).Error("Failed to update key failure count in store")
	}

	if shouldBlacklist && finalKey.Status == models.KeyStatusInvalid {
		// 检查有效密钥数量是否低于阈值，发送飞书通知
		p.checkAndNotifyLowKeyCount(group, activeKeysListKey)
	} else if group.KeyNeverExpires {
		logrus.WithFields(logrus.Fields{
			"keyID":         apiKey.ID,
			"groupID":       group.ID,
			"isNeverExpire": true,
		}).Debug("Key has failures but group has key_never_expires enabled, skipping blacklist")
	}

	return nil
}

// checkAndNotifyLowKeyCount 检查分组有效密钥数量是否低于阈值，如果是则发送飞书 Webhook 通知
// 实现通知节流机制，每个分组有冷却时间，避免短时间内重复发送通知
func (p *KeyProvider) checkAndNotifyLowKeyCount(group *models.Group, activeKeysListKey string) {
	threshold := group.EffectiveConfig.InvalidKeyCountThreshold
	if threshold <= 0 {
		return
	}

	webhookURL := p.settingsManager.GetSettings().FeishuWebhookURL
	if webhookURL == "" {
		return
	}

	activeCount, err := p.store.LLen(activeKeysListKey)
	if err != nil {
		logrus.WithError(err).WithField("groupID", group.ID).Error("Failed to get active key count for threshold check")
		return
	}

	if activeCount < int64(threshold) {
		// 检查是否处于冷却期，实现通知节流
		cooldownKey := fmt.Sprintf("notify:group:%d:low_keys_cooldown", group.ID)
		cooldownDuration := 5 * time.Minute

		// 使用 SetNX 尝试设置冷却标记，如果成功说明之前没有设置或已过期
		set, err := p.store.SetNX(cooldownKey, []byte("1"), cooldownDuration)
		if err != nil {
			logrus.WithError(err).WithField("groupID", group.ID).Error("Failed to check notification cooldown")
			return
		}

		// 如果 set 为 false，说明冷却标记已存在，跳过本次通知
		if !set {
			return
		}

		groupName := group.Name
		if group.DisplayName != "" {
			groupName = group.DisplayName
		}

		title := fmt.Sprintf("⚠️ [%s] 密钥数量不足告警", groupName)
		content := fmt.Sprintf("**分组**: %10s\n**当前有效密钥数**: %d\n**告警阈值**: %d\n\n请及时补充密钥，避免服务中断。",
			groupName, activeCount, threshold)

		if err := utils.SendFeishuWebhook(webhookURL, title, content); err != nil {
			logrus.WithError(err).WithFields(logrus.Fields{
				"groupID":     group.ID,
				"activeCount": activeCount,
				"threshold":   threshold,
			}).Error("Failed to send low key count notification via Feishu webhook")
		} else {
			logrus.WithFields(logrus.Fields{
				"groupID":     group.ID,
				"activeCount": activeCount,
				"threshold":   threshold,
			}).Info("Sent low key count notification via Feishu webhook")
		}
	}
}

// LoadKeysFromDB 从数据库加载所有分组和密钥，并填充到 Store 中。
func (p *KeyProvider) LoadKeysFromDB() error {
	logrus.Debug("First time startup, loading keys from DB...")

	// 1. 分批从数据库加载并使用 Pipeline 写入 Redis
	allActiveKeyIDs := make(map[uint][]any)
	batchSize := 10000
	var batchKeys []*models.APIKey

	err := p.db.Model(&models.APIKey{}).FindInBatches(&batchKeys, batchSize, func(tx *gorm.DB, batch int) error {
		logrus.Debugf("Processing batch %d with %d keys...", batch, len(batchKeys))

		var pipeline store.Pipeliner
		if redisStore, ok := p.store.(store.RedisPipeliner); ok {
			pipeline = redisStore.Pipeline()
		}

		for _, key := range batchKeys {
			keyHashKey := fmt.Sprintf("key:%d", key.ID)
			keyDetails := p.apiKeyToMap(key)

			if pipeline != nil {
				pipeline.HSet(keyHashKey, keyDetails)
			} else {
				if err := p.store.HSet(keyHashKey, keyDetails); err != nil {
					logrus.WithFields(logrus.Fields{"keyID": key.ID, "error": err}).Error("Failed to HSet key details")
				}
			}

			if key.Status == models.KeyStatusActive {
				allActiveKeyIDs[key.GroupID] = append(allActiveKeyIDs[key.GroupID], key.ID)
			}
		}

		if pipeline != nil {
			if err := pipeline.Exec(); err != nil {
				return fmt.Errorf("failed to execute pipeline for batch %d: %w", batch, err)
			}
		}
		return nil
	}).Error

	if err != nil {
		return fmt.Errorf("failed during batch processing of keys: %w", err)
	}

	// 2. 更新所有分组的 active_keys 列表
	logrus.Info("Updating active key lists for all groups...")
	for groupID, activeIDs := range allActiveKeyIDs {
		if len(activeIDs) > 0 {
			activeKeysListKey := fmt.Sprintf("group:%d:active_keys", groupID)
			p.store.Delete(activeKeysListKey)
			if err := p.store.LPush(activeKeysListKey, activeIDs...); err != nil {
				logrus.WithFields(logrus.Fields{"groupID": groupID, "error": err}).Error("Failed to LPush active keys for group")
			}
		}
	}

	return nil
}

// AddKeys 批量添加新的 Key 到池和数据库中。
// 注意：缓存操作在 DB 事务外执行，避免 DB 提交成功但缓存失败时的数据不一致。
func (p *KeyProvider) AddKeys(groupID uint, keys []models.APIKey) error {
	if len(keys) == 0 {
		return nil
	}

	// 先写 DB
	if err := p.db.Create(&keys).Error; err != nil {
		return err
	}

	// 再写缓存，缓存失败不影响 DB 结果
	if err := p.addKeysToCacheBatch(groupID, keys); err != nil {
		logrus.WithError(err).WithField("groupID", groupID).Error("Failed to add keys to cache after DB insert")
	}

	return nil
}

// RemoveKeys 批量从池和数据库中移除 Key。
func (p *KeyProvider) RemoveKeys(groupID uint, keyValues []string) (int64, error) {
	if len(keyValues) == 0 {
		return 0, nil
	}

	var keysToDelete []models.APIKey
	var deletedCount int64

	err := p.db.Transaction(func(tx *gorm.DB) error {
		var keyHashes []string
		for _, keyValue := range keyValues {
			keyHash := p.encryptionSvc.Hash(keyValue)
			if keyHash != "" {
				keyHashes = append(keyHashes, keyHash)
			}
		}

		if len(keyHashes) == 0 {
			return nil
		}

		if err := tx.Where("group_id = ? AND key_hash IN ?", groupID, keyHashes).Find(&keysToDelete).Error; err != nil {
			return err
		}

		if len(keysToDelete) == 0 {
			return nil
		}

		keyIDsToDelete := pluckIDs(keysToDelete)

		result := tx.Where("id IN ?", keyIDsToDelete).Delete(&models.APIKey{})
		if result.Error != nil {
			return result.Error
		}
		deletedCount = result.RowsAffected

		return nil
	})

	if err != nil {
		return deletedCount, err
	}

	// 在 DB 事务提交后清理缓存，缓存失败不影响 DB 结果
	for _, key := range keysToDelete {
		if cacheErr := p.removeKeyFromStore(key.ID, key.GroupID); cacheErr != nil {
			logrus.WithFields(logrus.Fields{"keyID": key.ID, "error": cacheErr}).Error("Failed to remove key from cache after DB deletion")
		}
	}

	return deletedCount, nil
}

// RestoreKeys 恢复组内所有无效的 Key。
func (p *KeyProvider) RestoreKeys(groupID uint) (int64, error) {
	var invalidKeys []models.APIKey
	var restoredCount int64

	err := p.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("group_id = ? AND status = ?", groupID, models.KeyStatusInvalid).Find(&invalidKeys).Error; err != nil {
			return err
		}

		if len(invalidKeys) == 0 {
			return nil
		}

		updates := map[string]any{
			"status":        models.KeyStatusActive,
			"failure_count": 0,
		}
		result := tx.Model(&models.APIKey{}).Where("group_id = ? AND status = ?", groupID, models.KeyStatusInvalid).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		restoredCount = result.RowsAffected

		return nil
	})

	if err != nil {
		return restoredCount, err
	}

	// 在 DB 事务提交后更新缓存，缓存失败不影响 DB 结果
	for _, key := range invalidKeys {
		key.Status = models.KeyStatusActive
		key.FailureCount = 0
		if cacheErr := p.addKeyToStore(&key); cacheErr != nil {
			logrus.WithFields(logrus.Fields{"keyID": key.ID, "error": cacheErr}).Error("Failed to restore key in cache after DB update")
		}
	}

	return restoredCount, nil
}

// RestoreMultipleKeys 恢复指定的 Key。
func (p *KeyProvider) RestoreMultipleKeys(groupID uint, keyValues []string) (int64, error) {
	if len(keyValues) == 0 {
		return 0, nil
	}

	var keysToRestore []models.APIKey
	var restoredCount int64

	err := p.db.Transaction(func(tx *gorm.DB) error {
		var keyHashes []string
		for _, keyValue := range keyValues {
			keyHash := p.encryptionSvc.Hash(keyValue)
			if keyHash != "" {
				keyHashes = append(keyHashes, keyHash)
			}
		}

		if len(keyHashes) == 0 {
			return nil
		}

		if err := tx.Where("group_id = ? AND key_hash IN ? AND status = ?", groupID, keyHashes, models.KeyStatusInvalid).Find(&keysToRestore).Error; err != nil {
			return err
		}

		if len(keysToRestore) == 0 {
			return nil
		}

		keyIDsToRestore := pluckIDs(keysToRestore)

		updates := map[string]any{
			"status":        models.KeyStatusActive,
			"failure_count": 0,
		}
		result := tx.Model(&models.APIKey{}).Where("id IN ?", keyIDsToRestore).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		restoredCount = result.RowsAffected

		return nil
	})

	if err != nil {
		return restoredCount, err
	}

	// 在 DB 事务提交后更新缓存，缓存失败不影响 DB 结果
	for _, key := range keysToRestore {
		key.Status = models.KeyStatusActive
		key.FailureCount = 0
		if cacheErr := p.addKeyToStore(&key); cacheErr != nil {
			logrus.WithFields(logrus.Fields{"keyID": key.ID, "error": cacheErr}).Error("Failed to restore key in cache after DB update")
		}
	}

	return restoredCount, nil
}

// RemoveInvalidKeys 移除组内所有无效的 Key。
func (p *KeyProvider) RemoveInvalidKeys(groupID uint) (int64, error) {
	return p.removeKeysByStatus(groupID, models.KeyStatusInvalid)
}

// RemoveAllKeys 移除组内所有的 Key。
func (p *KeyProvider) RemoveAllKeys(groupID uint) (int64, error) {
	return p.removeKeysByStatus(groupID)
}

// removeKeysByStatus 按状态移除密钥的通用函数。如果未提供状态，则移除分组内所有密钥
func (p *KeyProvider) removeKeysByStatus(groupID uint, status ...string) (int64, error) {
	var keysToRemove []models.APIKey
	var removedCount int64

	err := p.db.Transaction(func(tx *gorm.DB) error {
		query := tx.Where("group_id = ?", groupID)
		if len(status) > 0 {
			query = query.Where("status IN ?", status)
		}

		if err := query.Find(&keysToRemove).Error; err != nil {
			return err
		}

		if len(keysToRemove) == 0 {
			return nil
		}

		deleteQuery := tx.Where("group_id = ?", groupID)
		if len(status) > 0 {
			deleteQuery = deleteQuery.Where("status IN ?", status)
		}
		result := deleteQuery.Delete(&models.APIKey{})
		if result.Error != nil {
			return result.Error
		}
		removedCount = result.RowsAffected

		return nil
	})

	if err != nil {
		return removedCount, err
	}

	// 在 DB 事务提交后清理缓存，缓存失败不影响 DB 结果
	for _, key := range keysToRemove {
		if cacheErr := p.removeKeyFromStore(key.ID, key.GroupID); cacheErr != nil {
			logrus.WithFields(logrus.Fields{"keyID": key.ID, "error": cacheErr}).Error("Failed to remove key from cache after DB deletion")
		}
	}

	return removedCount, nil
}

// RemoveKeysFromStore 直接从内存存储中移除指定的键，不涉及数据库操作
// 这个方法适用于数据库已经删除但需要清理内存存储的场景
func (p *KeyProvider) RemoveKeysFromStore(groupID uint, keyIDs []uint) error {
	if len(keyIDs) == 0 {
		return nil
	}

	activeKeysListKey := fmt.Sprintf("group:%d:active_keys", groupID)

	// 第一步：直接删除整个 active_keys 列表
	if err := p.store.Delete(activeKeysListKey); err != nil {
		logrus.WithFields(logrus.Fields{
			"groupID": groupID,
			"error":   err,
		}).Error("Failed to delete active keys list")
		return err
	}

	// 第二步：批量删除所有相关的key hash
	for _, keyID := range keyIDs {
		keyHashKey := fmt.Sprintf("key:%d", keyID)
		if err := p.store.Delete(keyHashKey); err != nil {
			logrus.WithFields(logrus.Fields{
				"keyID": keyID,
				"error": err,
			}).Error("Failed to delete key hash")
		}
	}

	logrus.WithFields(logrus.Fields{
		"groupID":  groupID,
		"keyCount": len(keyIDs),
	}).Info("Successfully cleaned up group keys from store")

	return nil
}

// addKeyToStore 将单个密钥添加到缓存的辅助函数
func (p *KeyProvider) addKeyToStore(key *models.APIKey) error {
	// 1. 将密钥详情存储到HASH
	keyHashKey := fmt.Sprintf("key:%d", key.ID)
	keyDetails := p.apiKeyToMap(key)
	if err := p.store.HSet(keyHashKey, keyDetails); err != nil {
		return fmt.Errorf("failed to HSet key details for key %d: %w", key.ID, err)
	}

	// 2. 如果是活跃状态，添加到活跃列表
	if key.Status == models.KeyStatusActive {
		activeKeysListKey := fmt.Sprintf("group:%d:active_keys", key.GroupID)
		if err := p.store.LRem(activeKeysListKey, 0, key.ID); err != nil {
			return fmt.Errorf("failed to LRem key %d before LPush for group %d: %w", key.ID, key.GroupID, err)
		}
		if err := p.store.LPush(activeKeysListKey, key.ID); err != nil {
			return fmt.Errorf("failed to LPush key %d to group %d: %w", key.ID, key.GroupID, err)
		}
	}
	return nil
}

// addKeysToCacheBatch 批量添加密钥到缓存（用于批量导入场景）
func (p *KeyProvider) addKeysToCacheBatch(groupID uint, keys []models.APIKey) error {
	if len(keys) == 0 {
		return nil
	}

	// 1. 批量HSet密钥详情
	if pipeliner, ok := p.store.(store.RedisPipeliner); ok {
		// Redis: 使用Pipeline批量操作
		pipe := pipeliner.Pipeline()
		for i := range keys {
			keyHashKey := fmt.Sprintf("key:%d", keys[i].ID)
			pipe.HSet(keyHashKey, p.apiKeyToMap(&keys[i]))
		}
		if err := pipe.Exec(); err != nil {
			return fmt.Errorf("failed to batch HSet keys: %w", err)
		}
	} else {
		// MemoryStore: 降级为逐个HSet
		for i := range keys {
			keyHashKey := fmt.Sprintf("key:%d", keys[i].ID)
			if err := p.store.HSet(keyHashKey, p.apiKeyToMap(&keys[i])); err != nil {
				return fmt.Errorf("failed to HSet key %d: %w", keys[i].ID, err)
			}
		}
	}

	// 2. 收集活跃状态的密钥ID
	activeKeysListKey := fmt.Sprintf("group:%d:active_keys", groupID)
	var activeKeyIDs []any
	for i := range keys {
		if keys[i].Status == models.KeyStatusActive {
			activeKeyIDs = append(activeKeyIDs, keys[i].ID)
		}
	}

	// 3. 批量LPush活跃密钥
	if len(activeKeyIDs) > 0 {
		if err := p.store.LPush(activeKeysListKey, activeKeyIDs...); err != nil {
			return fmt.Errorf("failed to batch LPush keys to group %d: %w", groupID, err)
		}
	}

	return nil
}

// removeKeyFromStore 从缓存中移除单个密钥的辅助函数
func (p *KeyProvider) removeKeyFromStore(keyID, groupID uint) error {
	activeKeysListKey := fmt.Sprintf("group:%d:active_keys", groupID)
	if err := p.store.LRem(activeKeysListKey, 0, keyID); err != nil {
		logrus.WithFields(logrus.Fields{"keyID": keyID, "groupID": groupID, "error": err}).Error("Failed to LRem key from active list")
	}

	keyHashKey := fmt.Sprintf("key:%d", keyID)
	if err := p.store.Delete(keyHashKey); err != nil {
		return fmt.Errorf("failed to delete key HASH for key %d: %w", keyID, err)
	}
	return nil
}

// apiKeyToMap 将APIKey模型转换为HSET使用的map
func (p *KeyProvider) apiKeyToMap(key *models.APIKey) map[string]any {
	return map[string]any{
		"id":             fmt.Sprint(key.ID),
		"key_string":     key.KeyValue,
		"status":         key.Status,
		"failure_count":  key.FailureCount,
		"group_id":       key.GroupID,
		"created_at":     key.CreatedAt.Unix(),
		"balance_total":  key.BalanceTotal,
		"balance_used":   key.BalanceUsed,
		"balance_status": key.BalanceStatus,
	}
}

// pluckIDs 从APIKey切片中提取ID
func pluckIDs(keys []models.APIKey) []uint {
	ids := make([]uint, len(keys))
	for i, key := range keys {
		ids[i] = key.ID
	}
	return ids
}

// UpdateBalanceSync 同步更新密钥的余额信息到数据库和缓存，返回错误供调用方感知
func (p *KeyProvider) UpdateBalanceSync(apiKey *models.APIKey, group *models.Group, balanceInfo *models.BalanceInfo) error {
	syncSemaphore <- struct{}{}        // 获取同步信号量（与异步信号量隔离）
	defer func() { <-syncSemaphore }() // 释放信号量
	keyHashKey := fmt.Sprintf("key:%d", apiKey.ID)

	// 更新数据库
	updates := map[string]any{
		"balance_total":  balanceInfo.BalanceTotal,
		"balance_used":   balanceInfo.BalanceUsed,
		"balance_status": balanceInfo.Status,
	}

	if err := p.db.Model(apiKey).Updates(updates).Error; err != nil {
		logrus.WithFields(logrus.Fields{
			"key_id": apiKey.ID,
			"error":  err,
		}).Error("Failed to update key balance in database")
		return fmt.Errorf("update balance in db: %w", err)
	}

	// 更新缓存，失败时重试最多 3 次
	cacheUpdates := map[string]any{
		"balance_total":  balanceInfo.BalanceTotal,
		"balance_used":   balanceInfo.BalanceUsed,
		"balance_status": balanceInfo.Status,
	}
	var cacheErr error
	for i := 0; i < 3; i++ {
		if err := p.store.HSet(keyHashKey, cacheUpdates); err != nil {
			cacheErr = err
			logrus.WithFields(logrus.Fields{
				"key_id": apiKey.ID,
				"error":  err,
				"retry":  i + 1,
			}).Warn("Failed to update key balance in store, retrying")
			time.Sleep(time.Duration(50*(i+1)) * time.Millisecond)
			continue
		}
		cacheErr = nil
		break
	}
	if cacheErr != nil {
		logrus.WithError(cacheErr).WithField("key_id", apiKey.ID).Error("Failed to update key balance in store after retries")
		return fmt.Errorf("update balance in store: %w", cacheErr)
	}

	logrus.WithFields(logrus.Fields{
		"key_id":        apiKey.ID,
		"group_id":      group.ID,
		"balance_total": balanceInfo.BalanceTotal,
		"balance_used":  balanceInfo.BalanceUsed,
	}).Debug("Key balance updated successfully")
	return nil
}

// IncrementDailyRequestCount 增加密钥的每日请求计数，并在达到限制时禁用密钥。
// 使用 Store（Redis/Memory）的 HIncrBy 原子递增，避免 DB 事务的丢失更新问题。
// 当达到限制时，同步更新 DB 状态并从活跃列表移除。
func (p *KeyProvider) IncrementDailyRequestCount(apiKey *models.APIKey, group *models.Group) {
	// 如果分组没有设置每日限制，则跳过
	if group.DailyRequestLimit <= 0 {
		return
	}

	keyHashKey := fmt.Sprintf("key:%d", apiKey.ID)
	activeKeysListKey := fmt.Sprintf("group:%d:active_keys", group.ID)

	// 使用北京时间（asia/shanghai）计算日期，与服务器时区一致
	now := time.Now().In(beijingLocation)
	dateStr := now.Format("20060102")

	// Store key: key:{keyID}:daily:{YYYYMMDD}
	dailyCountKey := fmt.Sprintf("daily_count:%d:%s", apiKey.ID, dateStr)

	// 原子递增计数
	newCount, err := p.store.HIncrBy(dailyCountKey, "count", 1)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"keyID": apiKey.ID,
			"error": err,
		}).Error("Failed to increment daily request count in store")
		return
	}

	// 首次创建时设置 TTL（当天剩余时间 + 1 小时缓冲），避免历史数据堆积
	if newCount == 1 {
		endOfDay := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 0, beijingLocation)
		ttl := time.Until(endOfDay) + time.Hour
		if err := p.store.Expire(dailyCountKey, ttl); err != nil {
			logrus.WithFields(logrus.Fields{"keyID": apiKey.ID, "error": err}).Warn("Failed to set TTL on daily count key")
		}
	}

	// 检查是否达到每日限制
	if newCount >= int64(group.DailyRequestLimit) {
		logrus.WithFields(logrus.Fields{
			"keyID":        apiKey.ID,
			"dailyLimit":   group.DailyRequestLimit,
			"requestCount": newCount,
		}).Info("Key has reached daily request limit, disabling.")

		// 先从活跃列表移除，再更新缓存和 DB 状态
		if err := p.store.LRem(activeKeysListKey, 0, apiKey.ID); err != nil {
			logrus.WithFields(logrus.Fields{"keyID": apiKey.ID, "error": err}).Error("Failed to LRem key from active list due to daily limit")
		}
		if err := p.store.HSet(keyHashKey, map[string]any{"status": models.KeyStatusInvalid}); err != nil {
			logrus.WithFields(logrus.Fields{"keyID": apiKey.ID, "error": err}).Error("Failed to update key status in store due to daily limit")
		}

		// 异步更新 DB 状态为 invalid，避免阻塞请求路径
		go func() {
			if err := p.db.Model(&models.APIKey{}).Where("id = ?", apiKey.ID).Update("status", models.KeyStatusInvalid).Error; err != nil {
				logrus.WithFields(logrus.Fields{"keyID": apiKey.ID, "error": err}).Error("Failed to update key status to invalid in DB due to daily limit")
			}
			// 同时确保 DB 中有每日计数记录（用于重启后恢复）
			today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, beijingLocation)
			p.db.Where("key_id = ? AND date = ?", apiKey.ID, today).
				Assign("count", newCount).
				FirstOrCreate(&models.KeyDailyRequest{KeyID: apiKey.ID, Date: today, Count: newCount})
		}()

		// 检查有效密钥数量是否低于阈值
		p.checkAndNotifyLowKeyCount(group, activeKeysListKey)
	}
}

// CheckDailyRequestLimit 检查密钥是否已达到每日限制，如果达到则从活跃列表中移除
// 从 Store（Redis/Memory）读取计数，避免每次请求都查询数据库
func (p *KeyProvider) CheckDailyRequestLimit(apiKey *models.APIKey, group *models.Group) bool {
	// 如果分组没有设置每日限制，则不检查
	if group.DailyRequestLimit <= 0 {
		return false
	}

	keyHashKey := fmt.Sprintf("key:%d", apiKey.ID)
	activeKeysListKey := fmt.Sprintf("group:%d:active_keys", group.ID)

	// 使用北京时间（asia/shanghai）计算日期，与服务器时区一致
	now := time.Now().In(beijingLocation)
	dateStr := now.Format("20060102")

	dailyCountKey := fmt.Sprintf("daily_count:%d:%s", apiKey.ID, dateStr)

	// 从 Store 读取计数
	countMap, err := p.store.HGetAll(dailyCountKey)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"keyID": apiKey.ID,
			"error": err,
		}).Error("Failed to get daily request count from store, allowing key usage")
		return false
	}

	if len(countMap) == 0 {
		// 今天还没有记录，可以继续使用
		return false
	}

	count, err := strconv.ParseInt(countMap["count"], 10, 64)
	if err != nil {
		return false
	}

	// 检查是否已达到每日限制
	if count >= int64(group.DailyRequestLimit) {
		logrus.WithFields(logrus.Fields{
			"keyID":        apiKey.ID,
			"dailyLimit":   group.DailyRequestLimit,
			"requestCount": count,
		}).Debug("Key has reached daily request limit, skipping.")

		// 从活跃列表中移除
		if err := p.store.LRem(activeKeysListKey, 0, apiKey.ID); err != nil {
			logrus.WithFields(logrus.Fields{"keyID": apiKey.ID, "error": err}).Error("Failed to LRem key from active list due to daily limit check")
		}
		// 更新缓存状态
		if err := p.store.HSet(keyHashKey, map[string]any{"status": models.KeyStatusInvalid}); err != nil {
			logrus.WithFields(logrus.Fields{"keyID": apiKey.ID, "error": err}).Error("Failed to update key status in store due to daily limit check")
		}

		// 异步更新 DB 状态
		go func() {
			if err := p.db.Model(&models.APIKey{}).Where("id = ?", apiKey.ID).Update("status", models.KeyStatusInvalid).Error; err != nil {
				logrus.WithFields(logrus.Fields{"keyID": apiKey.ID, "error": err}).Error("Failed to update key status to invalid in DB due to daily limit check")
			}
		}()

		return true
	}

	return false
}
