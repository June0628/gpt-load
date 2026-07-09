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

// DBSizeMonitorService periodically checks the database size and sends a Feishu notification if it exceeds the threshold.
type DBSizeMonitorService struct {
	db              *gorm.DB
	settingsManager *config.SystemSettingsManager
	stopCh          chan struct{}
	wg              sync.WaitGroup
	lastNotified    time.Time
}

// NewDBSizeMonitorService creates a new DBSizeMonitorService.
func NewDBSizeMonitorService(db *gorm.DB, sm *config.SystemSettingsManager) *DBSizeMonitorService {
	return &DBSizeMonitorService{db: db, settingsManager: sm, stopCh: make(chan struct{})}
}

// Start launches the database size monitor service.
func (s *DBSizeMonitorService) Start() {
	s.wg.Add(1)
	go s.run()
	logrus.Debug("Database size monitor service started")
}

// Stop gracefully stops the database size monitor service.
func (s *DBSizeMonitorService) Stop(ctx context.Context) {
	close(s.stopCh)
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		logrus.Info("DBSizeMonitorService stopped gracefully.")
	case <-ctx.Done():
		logrus.Warn("DBSizeMonitorService stop timed out.")
	}
}

// run is the main loop of the database size monitor.
func (s *DBSizeMonitorService) run() {
	defer s.wg.Done()
	s.checkDatabaseSize()
	for {
		settings := s.settingsManager.GetSettings()
		interval := settings.DBSizeMonitorIntervalHours
		if interval < 1 {
			interval = 6
		}
		ticker := time.NewTicker(time.Duration(interval) * time.Hour)
		select {
		case <-ticker.C:
			ticker.Stop()
			s.checkDatabaseSize()
		case <-s.stopCh:
			ticker.Stop()
			return
		}
	}
}

func (s *DBSizeMonitorService) checkDatabaseSize() {
	settings := s.settingsManager.GetSettings()
	if !settings.DBSizeMonitorEnabled {
		return
	}
	totalBytes, err := utils.GetDatabaseTotalSizeBytes(s.db)
	if err != nil {
		logrus.WithError(err).Error("Failed to get DB size")
		return
	}
	thresholdBytes := int64(settings.DBSizeMonitorThresholdGB) * 1024 * 1024 * 1024
	if thresholdBytes > 0 && totalBytes > thresholdBytes {
		if !s.lastNotified.IsZero() && time.Since(s.lastNotified) < time.Duration(settings.DBSizeMonitorIntervalHours)*time.Hour {
			return
		}
		s.lastNotified = time.Now()
		s.sendNotification(totalBytes, thresholdBytes)
	}
	logrus.WithField("size", utils.FormatBytes(totalBytes)).Debug("DB size checked")
}

func (s *DBSizeMonitorService) sendNotification(totalBytes, thresholdBytes int64) {
	settings := s.settingsManager.GetSettings()
	webhookURL := settings.FeishuWebhookURL
	if webhookURL == "" {
		logrus.Warn("DB size exceeds threshold but Feishu webhook not configured")
		return
	}
	title := "warning GPT-Load Database Size Exceeded"
	content := fmt.Sprintf("**Database size exceeds the configured threshold**\n\n- **Current Size**: %s\n- **Threshold**: %s\n- **Threshold Config**: %d GB\n\nPlease consider cleaning up old log tables or increasing the threshold.",
		utils.FormatBytes(totalBytes), utils.FormatBytes(thresholdBytes), settings.DBSizeMonitorThresholdGB)
	if err := utils.SendFeishuWebhook(webhookURL, title, content); err != nil {
		logrus.WithError(err).Error("Failed to send DB size notification")
	}
}
