package services

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gpt-load/internal/config"
	"gpt-load/internal/utils"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// LogCleanupService 负责清理过期的请求日志
type LogCleanupService struct {
	db              *gorm.DB
	settingsManager *config.SystemSettingsManager
	uploadService   *LogUploadService
	stopCh          chan struct{}
	wg              sync.WaitGroup
}

// NewLogCleanupService 创建新的日志清理服务
func NewLogCleanupService(db *gorm.DB, settingsManager *config.SystemSettingsManager, uploadService *LogUploadService) *LogCleanupService {
	return &LogCleanupService{
		db:              db,
		settingsManager: settingsManager,
		uploadService:   uploadService,
		stopCh:          make(chan struct{}),
	}
}

// Start 启动日志清理服务
func (s *LogCleanupService) Start() {
	s.wg.Add(1)
	go s.run()
	logrus.Debug("Log cleanup service started")
}

// Stop 停止日志清理服务
func (s *LogCleanupService) Stop(ctx context.Context) {
	close(s.stopCh)

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		logrus.Info("LogCleanupService stopped gracefully.")
	case <-ctx.Done():
		logrus.Warn("LogCleanupService stop timed out.")
	}
}

// run 运行日志清理的主循环
func (s *LogCleanupService) run() {
	defer s.wg.Done()
	ticker := time.NewTicker(2 * time.Hour)
	defer ticker.Stop()

	// 启动时先执行一次清理
	s.cleanupExpiredLogs()

	for {
		select {
		case <-ticker.C:
			s.cleanupExpiredLogs()
		case <-s.stopCh:
			return
		}
	}
}

// cleanupExpiredLogs 清理过期的请求日志
// 对于按日期分表的场景，直接删除旧的表比逐行删除更高效
func (s *LogCleanupService) cleanupExpiredLogs() {
	// 获取日志保留天数配置
	settings := s.settingsManager.GetSettings()
	retentionDays := settings.RequestLogRetentionDays

	if retentionDays <= 0 {
		logrus.Debug("Log retention is disabled (retention_days <= 0)")
		return
	}

	// 计算过期时间点
	cutoffDate := time.Now().AddDate(0, 0, -retentionDays)

	// 从数据库中查询实际存在的过期日志表，避免枚举大量不存在的表名
	tablesToDelete := utils.GetExistingExpiredLogTables(s.db, cutoffDate)

	if len(tablesToDelete) == 0 {
		logrus.Debug("No old log tables to cleanup")
		return
	}

	// 检查是否需要在删除前上传
	needUploadBeforeDelete := settings.LogUploadEnabled && settings.LogUploadBeforeDelete

	var failedTables []string
	var failedErrors []string

	dialect := s.db.Dialector.Name()
	deletedCount := 0

	for _, tableName := range tablesToDelete {
		// 统计表中的记录数（用于日志输出）
		var count int64
		if err := s.db.Table(tableName).Count(&count).Error; err != nil {
			logrus.WithError(err).WithField("table", tableName).Debug("Failed to count records in table")
			continue
		}

		// 如果启用了删除前上传，先执行上传
		if needUploadBeforeDelete {
			if err := s.uploadService.UploadTable(tableName); err != nil {
				logrus.WithError(err).WithField("table", tableName).Error("Failed to upload table before deletion, skipping deletion to prevent data loss")
				failedTables = append(failedTables, tableName)
				failedErrors = append(failedErrors, err.Error())
				continue // 上传失败则跳过该表的删除，防止数据丢失
			}
			logrus.WithField("table", tableName).Info("Successfully uploaded table before deletion")
		}

		// 删除表
		var dropErr error
		if dialect == "mysql" {
			dropErr = s.db.Exec(fmt.Sprintf("DROP TABLE IF EXISTS %s", tableName)).Error
		} else {
			// SQLite
			dropErr = s.db.Exec(fmt.Sprintf("DROP TABLE IF EXISTS \"%s\"", tableName)).Error
		}

		if dropErr != nil {
			logrus.WithError(dropErr).WithField("table", tableName).Error("Failed to drop old log table")
			continue
		}

		deletedCount++
		logrus.WithFields(logrus.Fields{
			"table":         tableName,
			"deleted_count": count,
		}).Info("Dropped old log table")
	}

	if deletedCount > 0 {
		logrus.WithFields(logrus.Fields{
			"dropped_tables": deletedCount,
			"cutoff_date":    cutoffDate.Format("2006-01-02"),
			"retention_days": retentionDays,
		}).Info("Successfully cleaned up old log tables")
	}

	// 如果有上传失败的表，通过飞书 Webhook 发送通知
	if len(failedTables) > 0 {
		s.sendUploadFailureNotification(failedTables, failedErrors)
	}
}

// sendUploadFailureNotification 发送日志上传失败的飞书 Webhook 通知
func (s *LogCleanupService) sendUploadFailureNotification(failedTables []string, failedErrors []string) {
	settings := s.settingsManager.GetSettings()
	webhookURL := settings.FeishuWebhookURL

	if webhookURL == "" {
		logrus.Warn("Log upload failed but Feishu webhook URL is not configured, cannot send notification")
		return
	}

	title := "⚠️ GPT-Load 日志上传失败通知"

	// 构建消息内容
	content := fmt.Sprintf("**日志上传失败，过期表无法被删除**\n\n"+
		"已配置 LogUploadEnabled=true 且 LogUploadBeforeDelete=true，但上传失败导致以下 %d 个过期日志表未被清理：\n\n", len(failedTables))

	for i, table := range failedTables {
		errMsg := ""
		if i < len(failedErrors) {
			errMsg = failedErrors[i]
		}
		content += fmt.Sprintf("- **%s**\n  错误: `%s`\n", table, errMsg)
	}

	content += "\n请检查 WebDAV/COS 配置是否正确（URL、密码等），否则这些过期表将永远无法被自动删除。"

	if err := utils.SendFeishuWebhook(webhookURL, title, content); err != nil {
		logrus.WithError(err).Error("Failed to send log upload failure notification via Feishu webhook")
	}
}
