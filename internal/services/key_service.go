package services

import (
	"context"
	"encoding/json"
	"fmt"
	"gpt-load/internal/encryption"
	"gpt-load/internal/keypool"
	"gpt-load/internal/models"
	"io"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

const (
	maxRequestKeys = 5000
	chunkSize      = 500
)

// keyDelimiterRegex 预编译，避免每次调用 ParseKeysFromText 重新编译
var keyDelimiterRegex = regexp.MustCompile(`[\s,;\n\r\t]+`)

// AddKeysResult 保存批量添加密钥的结果
type AddKeysResult struct {
	AddedCount   int   `json:"added_count"`
	IgnoredCount int   `json:"ignored_count"`
	TotalInGroup int64 `json:"total_in_group"`
}

// DeleteKeysResult 保存批量删除密钥的结果
type DeleteKeysResult struct {
	DeletedCount int   `json:"deleted_count"`
	IgnoredCount int   `json:"ignored_count"`
	TotalInGroup int64 `json:"total_in_group"`
}

// RestoreKeysResult 保存批量恢复密钥的结果
type RestoreKeysResult struct {
	RestoredCount int   `json:"restored_count"`
	IgnoredCount  int   `json:"ignored_count"`
	TotalInGroup  int64 `json:"total_in_group"`
}

// KeyService 提供与 API 密钥相关的服务
type KeyService struct {
	DB            *gorm.DB
	KeyProvider   *keypool.KeyProvider
	KeyValidator  *keypool.KeyValidator
	EncryptionSvc encryption.Service

	// 余额查询互斥锁，按分组去重
	balanceQueryLocks sync.Map
}

// NewKeyService 创建新的 KeyService
func NewKeyService(db *gorm.DB, keyProvider *keypool.KeyProvider, keyValidator *keypool.KeyValidator, encryptionSvc encryption.Service) *KeyService {
	return &KeyService{
		DB:            db,
		KeyProvider:   keyProvider,
		KeyValidator:  keyValidator,
		EncryptionSvc: encryptionSvc,
	}
}

// AddMultipleKeys 批量添加 key（同步版本）
func (s *KeyService) AddMultipleKeys(groupID uint, keysText string) (*AddKeysResult, error) {
	keys := s.ParseKeysFromText(keysText)
	if len(keys) > maxRequestKeys {
		return nil, fmt.Errorf("batch size exceeds the limit of %d keys, got %d", maxRequestKeys, len(keys))
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no valid keys found in the input text")
	}

	addedCount, ignoredCount, err := s.processAndCreateKeys(groupID, keys, nil)
	if err != nil {
		return nil, err
	}

	var totalInGroup int64
	if err := s.DB.Model(&models.APIKey{}).Where("group_id = ?", groupID).Count(&totalInGroup).Error; err != nil {
		return nil, err
	}

	return &AddKeysResult{
		AddedCount:   addedCount,
		IgnoredCount: ignoredCount,
		TotalInGroup: totalInGroup,
	}, nil
}

// processAndCreateKeys 底层批量添加 key 的复用函数
func (s *KeyService) processAndCreateKeys(
	groupID uint,
	keys []string,
	progressCallback func(processed int),
) (addedCount int, ignoredCount int, err error) {
	// 1. 获取分组中已有的密钥哈希用于去重
	var existingHashes []string
	if err := s.DB.Model(&models.APIKey{}).Where("group_id = ?", groupID).Pluck("key_hash", &existingHashes).Error; err != nil {
		return 0, 0, err
	}
	existingHashMap := make(map[string]bool)
	for _, h := range existingHashes {
		existingHashMap[h] = true
	}

	// 2. 准备待创建的新密钥
	var newKeysToCreate []models.APIKey
	uniqueNewKeys := make(map[string]bool)

	for _, keyVal := range keys {
		trimmedKey := strings.TrimSpace(keyVal)
		if trimmedKey == "" || uniqueNewKeys[trimmedKey] || !s.isValidKeyFormat(trimmedKey) {
			continue
		}

		// 生成哈希用于去重检查
		keyHash := s.EncryptionSvc.Hash(trimmedKey)
		if existingHashMap[keyHash] {
			continue
		}

		encryptedKey, err := s.EncryptionSvc.Encrypt(trimmedKey)
		if err != nil {
			logrus.WithError(err).WithField("key", trimmedKey).Error("Failed to encrypt key, skipping")
			continue
		}

		uniqueNewKeys[trimmedKey] = true
		newKeysToCreate = append(newKeysToCreate, models.APIKey{
			GroupID:  groupID,
			KeyValue: encryptedKey,
			KeyHash:  keyHash,
			Status:   models.KeyStatusActive,
		})
	}

	if len(newKeysToCreate) == 0 {
		return 0, len(keys), nil
	}

	// 3. 使用 KeyProvider 分块添加密钥
	for i := 0; i < len(newKeysToCreate); i += chunkSize {
		end := i + chunkSize
		if end > len(newKeysToCreate) {
			end = len(newKeysToCreate)
		}
		chunk := newKeysToCreate[i:end]
		if err := s.KeyProvider.AddKeys(groupID, chunk); err != nil {
			return addedCount, len(keys) - addedCount, err
		}
		addedCount += len(chunk)

		if progressCallback != nil {
			progressCallback(i + len(chunk))
		}
	}

	return addedCount, len(keys) - addedCount, nil
}

// ParseKeysFromText 解析文本中的 key 列表，支持 JSON 数组和分隔符分割
func (s *KeyService) ParseKeysFromText(text string) []string {
	var keys []string

	// 首先尝试解析为 JSON 字符串数组
	if json.Unmarshal([]byte(text), &keys) == nil && len(keys) > 0 {
		return s.filterValidKeys(keys)
	}

	// 通用解析：通过分隔符分割文本
	splitKeys := keyDelimiterRegex.Split(strings.TrimSpace(text), -1)

	for _, key := range splitKeys {
		key = strings.TrimSpace(key)
		if key != "" {
			keys = append(keys, key)
		}
	}

	return s.filterValidKeys(keys)
}

// filterValidKeys 验证并过滤潜在的 API 密钥
func (s *KeyService) filterValidKeys(keys []string) []string {
	var validKeys []string
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if s.isValidKeyFormat(key) {
			validKeys = append(validKeys, key)
		}
	}
	return validKeys
}

// isValidKeyFormat 对密钥格式进行基本验证
func (s *KeyService) isValidKeyFormat(key string) bool {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return false
	}
	// 过滤明显的 URL，防止用户误输入链接导致任务卡死
	if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
		return false
	}
	return true
}

// RestoreMultipleKeys 批量恢复 key
func (s *KeyService) RestoreMultipleKeys(groupID uint, keysText string) (*RestoreKeysResult, error) {
	keysToRestore := s.ParseKeysFromText(keysText)
	if len(keysToRestore) > maxRequestKeys {
		return nil, fmt.Errorf("batch size exceeds the limit of %d keys, got %d", maxRequestKeys, len(keysToRestore))
	}
	if len(keysToRestore) == 0 {
		return nil, fmt.Errorf("no valid keys found in the input text")
	}

	var totalRestoredCount int64
	for i := 0; i < len(keysToRestore); i += chunkSize {
		end := i + chunkSize
		if end > len(keysToRestore) {
			end = len(keysToRestore)
		}
		chunk := keysToRestore[i:end]
		restoredCount, err := s.KeyProvider.RestoreMultipleKeys(groupID, chunk)
		if err != nil {
			return nil, err
		}
		totalRestoredCount += restoredCount
	}

	ignoredCount := len(keysToRestore) - int(totalRestoredCount)

	var totalInGroup int64
	if err := s.DB.Model(&models.APIKey{}).Where("group_id = ?", groupID).Count(&totalInGroup).Error; err != nil {
		return nil, err
	}

	return &RestoreKeysResult{
		RestoredCount: int(totalRestoredCount),
		IgnoredCount:  ignoredCount,
		TotalInGroup:  totalInGroup,
	}, nil
}

// RestoreAllInvalidKeys 将分组中所有非活跃密钥的状态恢复为活跃
func (s *KeyService) RestoreAllInvalidKeys(groupID uint) (int64, error) {
	return s.KeyProvider.RestoreKeys(groupID)
}

// ClearAllInvalidKeys 删除分组中所有非活跃密钥
func (s *KeyService) ClearAllInvalidKeys(groupID uint) (int64, error) {
	return s.KeyProvider.RemoveInvalidKeys(groupID)
}

// ClearAllKeys 删除分组中的所有密钥
func (s *KeyService) ClearAllKeys(groupID uint) (int64, error) {
	return s.KeyProvider.RemoveAllKeys(groupID)
}

// DeleteMultipleKeys 批量删除 key
func (s *KeyService) DeleteMultipleKeys(groupID uint, keysText string) (*DeleteKeysResult, error) {
	keysToDelete := s.ParseKeysFromText(keysText)
	if len(keysToDelete) > maxRequestKeys {
		return nil, fmt.Errorf("batch size exceeds the limit of %d keys, got %d", maxRequestKeys, len(keysToDelete))
	}
	if len(keysToDelete) == 0 {
		return nil, fmt.Errorf("no valid keys found in the input text")
	}

	var totalDeletedCount int64
	for i := 0; i < len(keysToDelete); i += chunkSize {
		end := i + chunkSize
		if end > len(keysToDelete) {
			end = len(keysToDelete)
		}
		chunk := keysToDelete[i:end]
		deletedCount, err := s.KeyProvider.RemoveKeys(groupID, chunk)
		if err != nil {
			return nil, err
		}
		totalDeletedCount += deletedCount
	}

	ignoredCount := len(keysToDelete) - int(totalDeletedCount)

	var totalInGroup int64
	if err := s.DB.Model(&models.APIKey{}).Where("group_id = ?", groupID).Count(&totalInGroup).Error; err != nil {
		return nil, err
	}

	return &DeleteKeysResult{
		DeletedCount: int(totalDeletedCount),
		IgnoredCount: ignoredCount,
		TotalInGroup: totalInGroup,
	}, nil
}

// ListKeysInGroupQuery 构建分组 key 列表查询，支持状态过滤和搜索
func (s *KeyService) ListKeysInGroupQuery(groupID uint, statusFilter string, searchHash string) *gorm.DB {
	query := s.DB.Model(&models.APIKey{}).Where("group_id = ?", groupID)

	if statusFilter != "" {
		query = query.Where("status = ?", statusFilter)
	}

	if searchHash != "" {
		query = query.Where("key_hash = ?", searchHash)
	}

	orderBy := "last_used_at desc, id desc"
	if s.DB.Dialector.Name() == "postgres" {
		orderBy = "last_used_at desc nulls last, id desc"
	}

	query = query.Order(orderBy)

	return query
}

// TestMultipleKeys 同步验证多个 key
func (s *KeyService) TestMultipleKeys(group *models.Group, keysText string) ([]keypool.KeyTestResult, error) {
	keysToTest := s.ParseKeysFromText(keysText)
	if len(keysToTest) > maxRequestKeys {
		return nil, fmt.Errorf("batch size exceeds the limit of %d keys, got %d", maxRequestKeys, len(keysToTest))
	}
	if len(keysToTest) == 0 {
		return nil, fmt.Errorf("no valid keys found in the input text")
	}

	var allResults []keypool.KeyTestResult
	for i := 0; i < len(keysToTest); i += chunkSize {
		end := i + chunkSize
		if end > len(keysToTest) {
			end = len(keysToTest)
		}
		chunk := keysToTest[i:end]
		results, err := s.KeyValidator.TestMultipleKeys(group, chunk)
		if err != nil {
			return nil, err
		}
		allResults = append(allResults, results...)
	}

	return allResults, nil
}

// StreamKeysToWriter 批量流式写入解密后的 key
func (s *KeyService) StreamKeysToWriter(groupID uint, statusFilter string, writer io.Writer) error {
	query := s.DB.Model(&models.APIKey{}).Where("group_id = ?", groupID).Select("id, key_value")

	switch statusFilter {
	case models.KeyStatusActive, models.KeyStatusInvalid:
		query = query.Where("status = ?", statusFilter)
	case "all":
	default:
		return fmt.Errorf("invalid status filter: %s", statusFilter)
	}

	var keys []models.APIKey
	err := query.FindInBatches(&keys, chunkSize, func(tx *gorm.DB, batch int) error {
		for _, key := range keys {
			decryptedKey, err := s.EncryptionSvc.Decrypt(key.KeyValue)
			if err != nil {
				logrus.WithError(err).WithField("key_id", key.ID).Error("Failed to decrypt key for streaming, skipping")
				continue
			}
			if _, err := writer.Write([]byte(decryptedKey + "\n")); err != nil {
				return err
			}
		}
		return nil
	}).Error

	return err
}

// TryAcquireBalanceQueryLock 尝试获取指定分组的余额查询锁，成功返回 true
// 防止手动查询与定时查询竞态
func (s *KeyService) TryAcquireBalanceQueryLock(groupID uint) bool {
	_, loaded := s.balanceQueryLocks.LoadOrStore(groupID, struct{}{})
	return !loaded
}

// ReleaseBalanceQueryLock 释放指定分组的余额查询锁
func (s *KeyService) ReleaseBalanceQueryLock(groupID uint) {
	s.balanceQueryLocks.Delete(groupID)
}

// QueryGroupBalances 查询分组内所有活跃密钥的余额（手动触发）
// 修复：使用 worker pool + 限流，与定时查询保持一致，避免上游限流
func (s *KeyService) QueryGroupBalances(group *models.Group) {
	var activeKeys []models.APIKey
	if err := s.DB.Where("group_id = ? AND status = ?", group.ID, models.KeyStatusActive).Find(&activeKeys).Error; err != nil {
		logrus.Errorf("查询分组 %s 活跃密钥失败: %v", group.Name, err)
		return
	}

	if len(activeKeys) == 0 {
		logrus.Debugf("分组 %s 没有活跃密钥，跳过余额查询", group.Name)
		return
	}

	logrus.Infof("开始手动查询分组 %s 的余额，共 %d 个活跃密钥", group.Name, len(activeKeys))

	// 使用密钥验证器的余额查询服务
	validator := s.KeyValidator
	if validator == nil {
		logrus.Error("密钥验证器未初始化")
		return
	}

	balanceService := validator.GetBalanceService()
	if balanceService == nil {
		logrus.Error("余额查询服务未初始化")
		return
	}

	// 使用 worker pool + 限流，避免上游限流
	concurrency := 5
	if group.EffectiveConfig.KeyValidationConcurrency > 0 {
		concurrency = group.EffectiveConfig.KeyValidationConcurrency
	}

	// 手动查询支持取消：通过 ctx 在需要时中断限流等待和查询
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rateLimitDelay := 200 * time.Millisecond
	rateLimiter := time.NewTicker(rateLimitDelay)
	defer rateLimiter.Stop()

	// 每个请求的超时时间
	timeoutSeconds := 20
	if group.EffectiveConfig.KeyValidationTimeoutSeconds > 0 {
		timeoutSeconds = group.EffectiveConfig.KeyValidationTimeoutSeconds
	}

	var successCount int32
	var wg sync.WaitGroup
	jobs := make(chan *models.APIKey, len(activeKeys))

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for key := range jobs {
				// 限流，同时支持取消
				select {
				case <-rateLimiter.C:
				case <-ctx.Done():
					return
				}

				// 解密密钥值
				decryptedKey, decryptErr := s.EncryptionSvc.Decrypt(key.KeyValue)
				if decryptErr != nil {
					logrus.WithError(decryptErr).WithField("key_id", key.ID).Warn("解密密钥失败，跳过余额查询")
					continue
				}
				queryKey := *key
				queryKey.KeyValue = decryptedKey

				// 余额查询加超时，避免单个请求无限阻塞
				queryCtx, queryCancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
				balanceInfo, err := balanceService.QueryBalance(queryCtx, group, &queryKey)
				queryCancel()
				if err != nil {
					logrus.WithError(err).WithField("key_id", key.ID).Debug("余额查询失败")
					continue
				}

				if balanceInfo != nil && balanceInfo.Success {
					// 同步更新余额，确保错误可被感知
					if updateErr := s.KeyProvider.UpdateBalanceSync(key, group, balanceInfo); updateErr != nil {
						logrus.WithError(updateErr).WithField("key_id", key.ID).Error("余额更新失败")
						continue
					}
					atomic.AddInt32(&successCount, 1)
					logrus.WithFields(logrus.Fields{
						"key_id":  key.ID,
						"balance": balanceInfo.BalanceTotal,
					}).Debug("余额查询成功")
				} else if balanceInfo != nil {
					logrus.WithFields(logrus.Fields{
						"key_id": key.ID,
						"error":  balanceInfo.ErrorMessage,
					}).Warn("余额查询返回失败")
				}
			}
		}()
	}

	for i := range activeKeys {
		select {
		case jobs <- &activeKeys[i]:
		case <-ctx.Done():
			break
		}
	}
	close(jobs)
	wg.Wait()

	logrus.Infof("分组 %s 余额查询完成，成功: %d/%d", group.Name, successCount, len(activeKeys))
}
