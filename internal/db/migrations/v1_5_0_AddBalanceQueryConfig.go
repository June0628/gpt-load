package db

import (
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// V1_5_0_AddBalanceQueryConfig 添加余额查询配置字段到 groups 表
func V1_5_0_AddBalanceQueryConfig(db *gorm.DB) error {
	dialect := db.Dialector.Name()

	// 检查 enable_balance_query 列是否已存在
	if !columnExists(db, dialect, "groups", "enable_balance_query") {
		// 添加 enable_balance_query 列
		if err := db.Exec("ALTER TABLE groups ADD COLUMN enable_balance_query TINYINT(1) DEFAULT 0").Error; err != nil {
			return err
		}
		logrus.Info("Added column: enable_balance_query")
	} else {
		logrus.Info("Column enable_balance_query already exists, skipping")
	}

	// 检查 balance_query_path 列是否已存在
	if !columnExists(db, dialect, "groups", "balance_query_path") {
		// 添加 balance_query_path 列
		if dialect == "mysql" {
			if err := db.Exec("ALTER TABLE groups ADD COLUMN balance_query_path VARCHAR(500) DEFAULT ''").Error; err != nil {
				return err
			}
		} else {
			// SQLite
			if err := db.Exec("ALTER TABLE groups ADD COLUMN balance_query_path TEXT DEFAULT ''").Error; err != nil {
				return err
			}
		}
		logrus.Info("Added column: balance_query_path")
	} else {
		logrus.Info("Column balance_query_path already exists, skipping")
	}

	// 检查 aggregate_balance 列是否已存在
	if !columnExists(db, dialect, "groups", "aggregate_balance") {
		// 添加 aggregate_balance 列
		if err := db.Exec("ALTER TABLE groups ADD COLUMN aggregate_balance TINYINT(1) DEFAULT 0").Error; err != nil {
			return err
		}
		logrus.Info("Added column: aggregate_balance")
	} else {
		logrus.Info("Column aggregate_balance already exists, skipping")
	}

	logrus.Info("Migration v1.5.0 completed: Balance query config columns added")
	return nil
}

// columnExists 检查指定表的列是否存在
func columnExists(db *gorm.DB, dialect, tableName, columnName string) bool {
	var count int64
	var err error

	if dialect == "mysql" {
		err = db.Raw(`
			SELECT COUNT(*)
			FROM information_schema.COLUMNS
			WHERE TABLE_SCHEMA = DATABASE()
			AND TABLE_NAME = ?
			AND COLUMN_NAME = ?
		`, tableName, columnName).Scan(&count).Error
	} else {
		// SQLite: 使用 pragma_table_info
		err = db.Raw(`
			SELECT COUNT(*)
			FROM pragma_table_info(?)
			WHERE name = ?
		`, tableName, columnName).Scan(&count).Error
	}

	if err != nil {
		logrus.WithError(err).Warnf("Failed to check column %s existence, assuming it doesn't exist", columnName)
		return false
	}

	return count > 0
}
