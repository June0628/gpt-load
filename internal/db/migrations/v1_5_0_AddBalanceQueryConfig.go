package db

import (
	"gorm.io/gorm"
)

// V1_5_0_AddBalanceQueryConfig 添加余额查询配置字段到 groups 表
func V1_5_0_AddBalanceQueryConfig(db *gorm.DB) error {
	// 检查 enable_balance_query 列是否已存在
	var columnExists int64
	err := db.Raw(`
		SELECT COUNT(*)
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE()
		AND TABLE_NAME = 'groups'
		AND COLUMN_NAME = 'enable_balance_query'
	`).Scan(&columnExists).Error

	if err != nil && db.Dialector.Name() != "sqlite" {
		// 如果不是 SQLite 且查询失败，可能是表不存在或其他问题
		// 继续执行 ALTER TABLE，让 GORM 处理
	}

	if columnExists == 0 {
		// 添加 enable_balance_query 列
		if err := db.Exec("ALTER TABLE groups ADD COLUMN enable_balance_query TINYINT(1) DEFAULT 0").Error; err != nil {
			return err
		}
	}

	// 检查 balance_query_path 列是否已存在
	var balancePathColumnExists int64
	err = db.Raw(`
		SELECT COUNT(*)
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE()
		AND TABLE_NAME = 'groups'
		AND COLUMN_NAME = 'balance_query_path'
	`).Scan(&balancePathColumnExists).Error

	if err != nil && db.Dialector.Name() != "sqlite" {
		// 继续执行
	}

	if balancePathColumnExists == 0 {
		// 添加 balance_query_path 列
		if db.Dialector.Name() == "mysql" {
			if err := db.Exec("ALTER TABLE groups ADD COLUMN balance_query_path VARCHAR(500) DEFAULT ''").Error; err != nil {
				return err
			}
		} else {
			// SQLite
			if err := db.Exec("ALTER TABLE groups ADD COLUMN balance_query_path TEXT DEFAULT ''").Error; err != nil {
				return err
			}
		}
	}

	return nil
}
