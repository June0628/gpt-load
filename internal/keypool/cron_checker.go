package keypool

import (
	"context"
	"gpt-load/internal/config"
	"gpt-load/internal/encryption"
	"gpt-load/internal/models"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// BalanceQueryLocker 定义余额查询互斥锁接口，防止手动查询与定时查询竞态
type BalanceQueryLocker interface {
	TryAcquireBalanceQueryLock(groupID uint) bool
	ReleaseBalanceQueryLock(groupID uint)
}

// CronChecker 负责定期验证无效密钥
type CronChecker struct {
	DB              *gorm.DB
	SettingsManager *config.SystemSettingsManager
	Validator       *KeyValidator
	EncryptionSvc   encryption.Service
	balanceLocker   BalanceQueryLocker
	stopChan        chan struct{}
	wg              sync.WaitGroup
}

// NewCronChecker 创建一个新的CronChecker
func NewCronChecker(
	db *gorm.DB,
	settingsManager *config.SystemSettingsManager,
	validator *KeyValidator,
	encryptionSvc encryption.Service,
) *CronChecker {
	return &CronChecker{
		DB:              db,
		SettingsManager: settingsManager,
		Validator:       validator,
		EncryptionSvc:   encryptionSvc,
		stopChan:        make(chan struct{}),
	}
}

// SetBalanceQueryLocker 注入余额查询互斥锁，用于防止手动查询与定时查询竞态
func (s *CronChecker) SetBalanceQueryLocker(locker BalanceQueryLocker) {
	s.balanceLocker = locker
}

// Start 开始执行定时任务
func (s *CronChecker) Start() {
	logrus.Debug("Starting CronChecker...")
	s.wg.Add(1)
	go s.runLoop()
}

// Stop 停止定时任务，尊重上下文的关闭超时
func (s *CronChecker) Stop(ctx context.Context) {
	close(s.stopChan)

	// 等待goroutine完成，或等待关闭超时
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		logrus.Info("CronChecker stopped gracefully.")
	case <-ctx.Done():
		logrus.Warn("CronChecker stop timed out.")
	}
}

func (s *CronChecker) runLoop() {
	defer s.wg.Done()

	s.submitValidationJobs()

	validationTicker := time.NewTicker(5 * time.Minute)
	defer validationTicker.Stop()

	for {
		select {
		case <-validationTicker.C:
			logrus.Debug("CronChecker: running as master, submitting validation jobs.")
			s.submitValidationJobs()
			logrus.Debug("CronChecker: running as master, submitting balance query jobs.")
			s.submitBalanceQueryJobs()
		case <-s.stopChan:
			return
		}
	}
}

// forEachGroup 加载非聚合分组并并发执行给定函数，基于配置的间隔判断是否需要处理
// useBalanceTimestamp: true 表示使用 LastBalanceQueriedAt 判断，false 表示使用 LastValidatedAt。
// 余额查询使用独立间隔配置
func (s *CronChecker) forEachGroup(actionName string, useBalanceTimestamp bool, fn func(group *models.Group)) {
	var groups []models.Group
	if err := s.DB.Where("group_type != ? OR group_type IS NULL", "aggregate").Find(&groups).Error; err != nil {
		logrus.Errorf("CronChecker[%s]: failed to get groups: %v", actionName, err)
		return
	}

	startTime := time.Now()
	var wg sync.WaitGroup

	// 限制同时处理的 group 数量，避免 goroutine 爆炸
	const maxConcurrentGroups = 10
	sem := make(chan struct{}, maxConcurrentGroups)

	for i := range groups {
		group := &groups[i]
		group.EffectiveConfig = s.SettingsManager.GetEffectiveConfig(group.Config)

		// 余额查询使用独立间隔配置
		var interval time.Duration
		if useBalanceTimestamp {
			// 优先使用分组级别的余额查询间隔，其次使用全局配置
			intervalMinutes := group.EffectiveConfig.BalanceQueryIntervalMinutes
			if intervalMinutes <= 0 {
				intervalMinutes = group.EffectiveConfig.KeyValidationIntervalMinutes
			}
			interval = time.Duration(intervalMinutes) * time.Minute
		} else {
			interval = time.Duration(group.EffectiveConfig.KeyValidationIntervalMinutes) * time.Minute
		}

		var lastProcessedAt *time.Time
		if useBalanceTimestamp {
			lastProcessedAt = group.LastBalanceQueriedAt
		} else {
			lastProcessedAt = group.LastValidatedAt
		}

		if lastProcessedAt == nil || startTime.Sub(*lastProcessedAt) > interval {
			wg.Add(1)
			sem <- struct{}{} // 获取信号量
			g := group
			go func() {
				defer wg.Done()
				defer func() { <-sem }() // 释放信号量
				fn(g)
			}()
		}
	}

	wg.Wait()
}

// decryptKeyForUse 解密密钥并返回仅包含处理所需字段的新结构体，避免修改原始数据，消除隐式副作用
// decryptKeyForUse 复制完整密钥对象并替换为解密后的 KeyValue，避免后续使用零值
func (s *CronChecker) decryptKeyForUse(key *models.APIKey) (*models.APIKey, error) {
	decryptedKey, err := s.EncryptionSvc.Decrypt(key.KeyValue)
	if err != nil {
		return nil, err
	}
	keyForUse := *key
	keyForUse.KeyValue = decryptedKey
	return &keyForUse, nil
}

// updateGroupTimestamp 更新分组的时间戳列
// columnName: "last_validated_at" 或 "last_balance_queried_at"
func (s *CronChecker) updateGroupTimestamp(group *models.Group, columnName string) error {
	if err := s.DB.Model(group).Update(columnName, time.Now()).Error; err != nil {
		logrus.Errorf("CronChecker: failed to update %s for group %s: %v", columnName, group.Name, err)
		return err
	}
	return nil
}

// runWorkerPool 使用工作池模式处理密钥切片，支持优雅关闭
// 解密每个密钥，运行processFn，返回成功结果的数量
// rateLimiter 可选，用于控制所有worker的总请求速率（如balance查询限流）
func (s *CronChecker) runWorkerPool(
	keys []models.APIKey,
	concurrency int,
	actionName string,
	processFn func(key *models.APIKey) bool,
	rateLimiter <-chan time.Time,
) int32 {
	if concurrency <= 0 {
		concurrency = 5
	}

	var successCount int32
	var keyWg sync.WaitGroup
	jobs := make(chan *models.APIKey, len(keys))

	for range concurrency {
		keyWg.Add(1)
		go func() {
			defer keyWg.Done()
			for {
				select {
				case key, ok := <-jobs:
					if !ok {
						return
					}

					// 等待速率限制器（如果有）
					if rateLimiter != nil {
						select {
						case <-rateLimiter:
						case <-s.stopChan:
							return
						}
					}

					// 处理前解密密钥
					keyForUse, err := s.decryptKeyForUse(key)
					if err != nil {
						logrus.WithError(err).WithField("key_id", key.ID).Errorf("CronChecker[%s]: failed to decrypt key, skipping", actionName)
						continue
					}

					if processFn(keyForUse) {
						atomic.AddInt32(&successCount, 1)
					}
				case <-s.stopChan:
					return
				}
			}
		}()
	}

DistributeLoop:
	for i := range keys {
		select {
		case jobs <- &keys[i]:
		case <-s.stopChan:
			break DistributeLoop
		}
	}
	close(jobs)

	keyWg.Wait()
	return successCount
}

// submitValidationJobs 查找需要验证密钥的分组并并发验证
func (s *CronChecker) submitValidationJobs() {
	s.forEachGroup("validation", false, func(group *models.Group) {
		s.validateGroupKeys(group)
	})
}

// validateGroupKeys 并发验证单个分组的所有无效密钥
func (s *CronChecker) validateGroupKeys(group *models.Group) {
	groupProcessStart := time.Now()

	var invalidKeys []models.APIKey
	err := s.DB.WithContext(context.Background()).Where("group_id = ? AND status = ?", group.ID, models.KeyStatusInvalid).Find(&invalidKeys).Error
	if err != nil {
		logrus.Errorf("CronChecker: failed to get invalid keys for group %s: %v", group.Name, err)
		return
	}

	if len(invalidKeys) == 0 {
		if err := s.updateGroupTimestamp(group, "last_validated_at"); err != nil {
			logrus.Warnf("CronChecker: group '%s' validation timestamp not updated, will retry next cycle", group.Name)
		}
		logrus.Debugf("CronChecker: group '%s' has no invalid keys to check.", group.Name)
		return
	}

	becameValidCount := s.runWorkerPool(
		invalidKeys,
		group.EffectiveConfig.KeyValidationConcurrency,
		"validation",
		func(key *models.APIKey) bool {
			isValid, validationErr := s.Validator.ValidateSingleKey(key, group)
			if validationErr != nil {
				logrus.WithError(validationErr).WithField("key_id", key.ID).Debug("CronChecker[validation]: key validation error")
			}
			return isValid
		},
		nil, // validation不需要限流
	)

	if err := s.updateGroupTimestamp(group, "last_validated_at"); err != nil {
		logrus.Warnf("CronChecker: group '%s' validation finished but timestamp not updated, may cause duplicate processing next cycle", group.Name)
	}

	duration := time.Since(groupProcessStart)
	logrus.Infof(
		"CronChecker: group '%s' validation finished. total checked: %d, became valid: %d. duration: %s.",
		group.Name,
		len(invalidKeys),
		becameValidCount,
		duration.String(),
	)
}

// submitBalanceQueryJobs 查找启用余额查询的分组并查询余额
// 使用独立余额查询间隔并加锁防止与手动查询竞态
func (s *CronChecker) submitBalanceQueryJobs() {
	s.forEachGroup("balance", true, func(group *models.Group) {
		if !group.ShouldQueryBalance() {
			return
		}
		// 定时查询获取锁，避免与手动查询同时发起
		if s.balanceLocker != nil && !s.balanceLocker.TryAcquireBalanceQueryLock(group.ID) {
			logrus.Debugf("CronChecker[balance]: group '%s' 正在被手动查询，跳过本次定时查询", group.Name)
			return
		}
		defer func() {
			if s.balanceLocker != nil {
				s.balanceLocker.ReleaseBalanceQueryLock(group.ID)
			}
		}()
		s.queryGroupBalances(group)
	})
}

// queryGroupBalances 查询分组内所有活跃密钥的余额
func (s *CronChecker) queryGroupBalances(group *models.Group) {
	groupProcessStart := time.Now()

	var activeKeys []models.APIKey
	err := s.DB.WithContext(context.Background()).Where("group_id = ? AND status = ?", group.ID, models.KeyStatusActive).Find(&activeKeys).Error
	if err != nil {
		logrus.Errorf("CronChecker: failed to get active keys for balance query in group %s: %v", group.Name, err)
		return
	}

	if len(activeKeys) == 0 {
		if err := s.updateGroupTimestamp(group, "last_balance_queried_at"); err != nil {
			logrus.Warnf("CronChecker: group '%s' balance timestamp not updated, will retry next cycle", group.Name)
		}
		logrus.Debugf("CronChecker: group '%s' has no active keys for balance query.", group.Name)
		return
	}

	// 限流：所有worker共享一个ticker，控制整体请求速率
	rateLimitDelay := 200 * time.Millisecond
	rateLimiter := time.NewTicker(rateLimitDelay)
	defer rateLimiter.Stop()

	// 防御性检查：余额查询依赖未导出字段，若未注入则跳过避免 panic
	if s.Validator == nil || s.Validator.balanceService == nil || s.Validator.keypoolProvider == nil {
		logrus.Errorf("CronChecker[balance]: validator/balanceService/keypoolProvider 未初始化，跳过分组 %s", group.Name)
		return
	}

	successCount := s.runWorkerPool(
		activeKeys,
		group.EffectiveConfig.KeyValidationConcurrency,
		"balance",
		func(key *models.APIKey) bool {
			// 余额查询加超时，避免单个请求无限阻塞
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(group.EffectiveConfig.KeyValidationTimeoutSeconds)*time.Second)
			balanceInfo, err := s.Validator.balanceService.QueryBalance(ctx, group, key)
			cancel()
			if err != nil {
				logrus.WithError(err).WithField("key_id", key.ID).Debug("CronChecker[balance]: balance query failed")
				return false
			}

			if balanceInfo != nil && balanceInfo.Success {
				// 同步更新余额，失败时记录错误
				if updateErr := s.Validator.keypoolProvider.UpdateBalanceSync(key, group, balanceInfo); updateErr != nil {
					logrus.WithError(updateErr).WithField("key_id", key.ID).Error("CronChecker[balance]: 余额更新失败")
					return false
				}
				logrus.WithFields(logrus.Fields{
					"key_id":  key.ID,
					"balance": balanceInfo.BalanceTotal,
				}).Debug("CronChecker[balance]: balance query successful")
				return true
			}
			return false
		},
		rateLimiter.C,
	)

	if err := s.updateGroupTimestamp(group, "last_balance_queried_at"); err != nil {
		logrus.Warnf("CronChecker: group '%s' balance query finished but timestamp not updated, may cause duplicate processing next cycle", group.Name)
	}

	duration := time.Since(groupProcessStart)
	logrus.Infof(
		"CronChecker: group '%s' balance query finished. total checked: %d, success: %d. duration: %s.",
		group.Name,
		len(activeKeys),
		successCount,
		duration.String(),
	)
}
