package services

import (
	"context"
	"fmt"
	"gpt-load/internal/config"
	"gpt-load/internal/utils"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// LogCleanupService 负责清理过期的请求日志
type LogCleanupService struct {
	db              *gorm.DB
	settingsManager *config.SystemSettingsManager
	stopCh          chan struct{}
	wg              sync.WaitGroup
}

// NewLogCleanupService 创建新的日志清理服务
func NewLogCleanupService(db *gorm.DB, settingsManager *config.SystemSettingsManager) *LogCleanupService {
	return &LogCleanupService{
		db:              db,
		settingsManager: settingsManager,
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

	// 获取需要删除的日期范围内的所有表
	// 我们删除 cutoffDate 之前的所有表
	veryOldDate := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	tablesToDelete := utils.GetLogTablesForDateRange(veryOldDate, cutoffDate.AddDate(0, 0, -1))

	if len(tablesToDelete) == 0 {
		logrus.Debug("No old log tables to cleanup")
		return
	}

	dialect := s.db.Dialector.Name()
	deletedCount := 0

	for _, tableName := range tablesToDelete {
		// 检查表是否存在
		if !s.tableExists(tableName) {
			continue
		}

		// 统计表中的记录数
		var count int64
		if err := s.db.Table(tableName).Count(&count).Error; err != nil {
			logrus.WithError(err).WithField("table", tableName).Debug("Failed to count records in table")
			continue
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
}

// tableExists 检查表是否存在
func (s *LogCleanupService) tableExists(tableName string) bool {
	return s.db.Migrator().HasTable(tableName)
}
