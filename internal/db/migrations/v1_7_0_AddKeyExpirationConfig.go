package db

import (
	"gpt-load/internal/models"

	"gorm.io/gorm"
)

// GroupV170 用于迁移的临时结构体
type GroupV170 struct {
	KeyNeverExpires   bool `gorm:"column:key_never_expires"`
	DailyRequestLimit int  `gorm:"column:daily_request_limit"`
}

func (GroupV170) TableName() string {
	return "groups"
}

// V1_7_0_AddKeyExpirationConfig 添加密钥失效配置相关字段
func V1_7_0_AddKeyExpirationConfig(db *gorm.DB) error {
	// 添加 key_never_expires 字段（使用 GORM Migrator，兼容 MySQL/SQLite/PostgreSQL）
	if !db.Migrator().HasColumn(&GroupV170{}, "key_never_expires") {
		if err := db.Migrator().AddColumn(&GroupV170{}, "key_never_expires"); err != nil {
			return err
		}
		// 确保默认值为 false（兼容部分数据库）
		if err := db.Migrator().AlterColumn(&GroupV170{}, "key_never_expires"); err != nil {
			return err
		}
	}

	// 添加 daily_request_limit 字段
	if !db.Migrator().HasColumn(&GroupV170{}, "daily_request_limit") {
		if err := db.Migrator().AddColumn(&GroupV170{}, "daily_request_limit"); err != nil {
			return err
		}
		// 确保默认值为 0（兼容部分数据库）
		if err := db.Migrator().AlterColumn(&GroupV170{}, "daily_request_limit"); err != nil {
			return err
		}
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
