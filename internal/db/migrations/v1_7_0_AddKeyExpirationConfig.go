package db

import (
	"gpt-load/internal/models"

	"gorm.io/gorm"
)

// V1_7_0_AddKeyExpirationConfig 添加密钥失效配置相关字段
func V1_7_0_AddKeyExpirationConfig(db *gorm.DB) error {
	// 添加分组配置字段
	if err := db.Exec(`
		ALTER TABLE groups
		ADD COLUMN IF NOT EXISTS key_never_expires BOOLEAN NOT NULL DEFAULT FALSE,
		ADD COLUMN IF NOT EXISTS daily_request_limit INT NOT NULL DEFAULT 0
	`).Error; err != nil {
		return err
	}

	// 添加密钥每日请求计数表
	if !db.Migrator().HasTable(&models.KeyDailyRequest{}) {
		if err := db.Migrator().CreateTable(&models.KeyDailyRequest{}); err != nil {
			return err
		}
	}

	// 添加密钥请求计数表索引
	if !db.Migrator().HasIndex(&models.KeyDailyRequest{}, "idx_key_daily_date") {
		if err := db.Exec(`
			CREATE UNIQUE INDEX idx_key_daily_date ON key_daily_requests (key_id, date)
		`).Error; err != nil {
			return err
		}
	}

	return nil
}
