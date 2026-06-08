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

// NewCronChecker is responsible for periodically validating invalid keys.
type CronChecker struct {
	DB              *gorm.DB
	SettingsManager *config.SystemSettingsManager
	Validator       *KeyValidator
	EncryptionSvc   encryption.Service
	stopChan        chan struct{}
	wg              sync.WaitGroup
}

// NewCronChecker creates a new CronChecker.
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

// Start begins the cron job execution.
func (s *CronChecker) Start() {
	logrus.Debug("Starting CronChecker...")
	s.wg.Add(1)
	go s.runLoop()
}

// Stop stops the cron job, respecting the context for shutdown timeout.
func (s *CronChecker) Stop(ctx context.Context) {
	close(s.stopChan)

	// Wait for the goroutine to finish, or for the shutdown to time out.
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
			logrus.Debug("CronChecker: Running as Master, submitting validation jobs.")
			s.submitValidationJobs()
			logrus.Debug("CronChecker: Running as Master, submitting balance query jobs.")
			s.submitBalanceQueryJobs()
		case <-s.stopChan:
			return
		}
	}
}

// submitValidationJobs finds groups whose keys need validation and validates them concurrently.
func (s *CronChecker) submitValidationJobs() {
	var groups []models.Group
	if err := s.DB.Where("group_type != ? OR group_type IS NULL", "aggregate").Find(&groups).Error; err != nil {
		logrus.Errorf("CronChecker: Failed to get groups: %v", err)
		return
	}

	validationStartTime := time.Now()
	var wg sync.WaitGroup

	for i := range groups {
		group := &groups[i]
		group.EffectiveConfig = s.SettingsManager.GetEffectiveConfig(group.Config)
		interval := time.Duration(group.EffectiveConfig.KeyValidationIntervalMinutes) * time.Minute

		if group.LastValidatedAt == nil || validationStartTime.Sub(*group.LastValidatedAt) > interval {
			wg.Add(1)
			g := group
			go func() {
				defer wg.Done()
				s.validateGroupKeys(g)
			}()
		}
	}

	wg.Wait()
}

// validateGroupKeys validates all invalid keys for a single group concurrently.
func (s *CronChecker) validateGroupKeys(group *models.Group) {
	groupProcessStart := time.Now()

	var invalidKeys []models.APIKey
	err := s.DB.Where("group_id = ? AND status = ?", group.ID, models.KeyStatusInvalid).Find(&invalidKeys).Error
	if err != nil {
		logrus.Errorf("CronChecker: Failed to get invalid keys for group %s: %v", group.Name, err)
		return
	}

	if len(invalidKeys) == 0 {
		if err := s.DB.Model(group).Update("last_validated_at", time.Now()).Error; err != nil {
			logrus.Errorf("CronChecker: Failed to update last_validated_at for group %s: %v", group.Name, err)
		}
		logrus.Infof("CronChecker: Group '%s' has no invalid keys to check.", group.Name)
		return
	}

	var becameValidCount int32
	var keyWg sync.WaitGroup
	jobs := make(chan *models.APIKey, len(invalidKeys))

	concurrency := group.EffectiveConfig.KeyValidationConcurrency
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

					// Decrypt the key before validation
					decryptedKey, err := s.EncryptionSvc.Decrypt(key.KeyValue)
					if err != nil {
						logrus.WithError(err).WithField("key_id", key.ID).Error("CronChecker: Failed to decrypt key for validation, skipping")
						continue
					}

					// Create a copy with decrypted value for validation
					keyForValidation := *key
					keyForValidation.KeyValue = decryptedKey

					isValid, _ := s.Validator.ValidateSingleKey(&keyForValidation, group)
					if isValid {
						atomic.AddInt32(&becameValidCount, 1)
					}
				case <-s.stopChan:
					return
				}
			}
		}()
	}

DistributeLoop:
	for i := range invalidKeys {
		select {
		case jobs <- &invalidKeys[i]:
		case <-s.stopChan:
			break DistributeLoop
		}
	}
	close(jobs)

	keyWg.Wait()

	if err := s.DB.Model(group).Update("last_validated_at", time.Now()).Error; err != nil {
		logrus.Errorf("CronChecker: Failed to update last_validated_at for group %s: %v", group.Name, err)
	}

	duration := time.Since(groupProcessStart)
	logrus.Infof(
		"CronChecker: Group '%s' validation finished. Total checked: %d, became valid: %d. Duration: %s.",
		group.Name,
		len(invalidKeys),
		becameValidCount,
		duration.String(),
	)
}

// submitBalanceQueryJobs finds groups with balance query enabled and queries balances.
// 复用 KeyValidationIntervalMinutes 配置作为余额查询间隔。
func (s *CronChecker) submitBalanceQueryJobs() {
	var groups []models.Group
	if err := s.DB.Where("group_type != ? OR group_type IS NULL", "aggregate").Find(&groups).Error; err != nil {
		logrus.Errorf("CronChecker: Failed to get groups for balance query: %v", err)
		return
	}

	queryStartTime := time.Now()
	var wg sync.WaitGroup

	for i := range groups {
		group := &groups[i]
		group.EffectiveConfig = s.SettingsManager.GetEffectiveConfig(group.Config)
		interval := time.Duration(group.EffectiveConfig.KeyValidationIntervalMinutes) * time.Minute

		// 只有启用了余额查询且到达查询间隔的分组才执行
		if group.ShouldQueryBalance() && (group.LastValidatedAt == nil || queryStartTime.Sub(*group.LastValidatedAt) > interval) {
			wg.Add(1)
			g := group
			go func() {
				defer wg.Done()
				s.queryGroupBalances(g)
			}()
		}
	}

	wg.Wait()
}

// queryGroupBalances queries balances for all active keys in a group.
func (s *CronChecker) queryGroupBalances(group *models.Group) {
	groupProcessStart := time.Now()

	var activeKeys []models.APIKey
	err := s.DB.Where("group_id = ? AND status = ?", group.ID, models.KeyStatusActive).Find(&activeKeys).Error
	if err != nil {
		logrus.Errorf("CronChecker: Failed to get active keys for balance query in group %s: %v", group.Name, err)
		return
	}

	if len(activeKeys) == 0 {
		logrus.Debugf("CronChecker: Group '%s' has no active keys for balance query.", group.Name)
		return
	}

	var successCount int32
	var keyWg sync.WaitGroup
	jobs := make(chan *models.APIKey, len(activeKeys))

	concurrency := group.EffectiveConfig.KeyValidationConcurrency
	if concurrency <= 0 {
		concurrency = 5
	}

	// 限流：每个 worker 之间增加固定延迟，避免并发过高导致上游限流
	rateLimitDelay := 200 * time.Millisecond

	for i := range concurrency {
		keyWg.Add(1)
		go func(workerID int) {
			defer keyWg.Done()
			// 每个 worker 启动时错开时间，避免瞬间高并发
			time.Sleep(time.Duration(workerID) * rateLimitDelay)
			for {
				select {
				case key, ok := <-jobs:
					if !ok {
						return
					}

					// Decrypt the key before querying balance
					decryptedKey, err := s.EncryptionSvc.Decrypt(key.KeyValue)
					if err != nil {
						logrus.WithError(err).WithField("key_id", key.ID).Error("CronChecker: Failed to decrypt key for balance query, skipping")
						continue
					}

					// Create a copy with decrypted value
					keyForQuery := *key
					keyForQuery.KeyValue = decryptedKey

					// Query balance directly without validation
					balanceInfo, err := s.Validator.balanceService.QueryBalance(context.Background(), group, &keyForQuery)
					if err != nil {
						logrus.WithError(err).WithField("key_id", key.ID).Debug("CronChecker: Balance query failed")
						continue
					}

					if balanceInfo != nil && balanceInfo.Success {
						s.Validator.keypoolProvider.UpdateBalance(&keyForQuery, group, balanceInfo)
						atomic.AddInt32(&successCount, 1)
						logrus.WithFields(logrus.Fields{
							"key_id":  key.ID,
							"balance": balanceInfo.BalanceTotal,
						}).Debug("CronChecker: Balance query successful")
					}

					// 每个请求之间增加延迟，避免触发上游限流
					time.Sleep(rateLimitDelay)
				case <-s.stopChan:
					return
				}
			}
		}(i)
	}

DistributeBalanceLoop:
	for i := range activeKeys {
		select {
		case jobs <- &activeKeys[i]:
		case <-s.stopChan:
			break DistributeBalanceLoop
		}
	}
	close(jobs)

	keyWg.Wait()

	duration := time.Since(groupProcessStart)
	logrus.Infof(
		"CronChecker: Group '%s' balance query finished. Total checked: %d, success: %d. Duration: %s.",
		group.Name,
		len(activeKeys),
		successCount,
		duration.String(),
	)
}
