package db

import (
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// V1_2_0_AlterRequestBodyToMediumText 修改 request_logs 表的 request_body 字段从 TEXT 改为 MEDIUMTEXT
// TEXT 最大 64KB, MEDIUMTEXT 最大 16MB
func V1_2_0_AlterRequestBodyToMediumText(db *gorm.DB) error {
	if db.Dialector.Name() == "mysql" {
		// 检查字段类型是否已经是 MEDIUMTEXT
		var columnType string
		err := db.Raw(`
			SELECT DATA_TYPE
			FROM information_schema.COLUMNS
			WHERE TABLE_SCHEMA = DATABASE()
			AND TABLE_NAME = 'request_logs'
			AND COLUMN_NAME = 'request_body'
		`).Scan(&columnType).Error

		if err != nil {
			logrus.WithError(err).Warn("Failed to check request_body column type, attempting migration anyway")
		} else if columnType == "mediumtext" {
			logrus.Info("request_body column is already MEDIUMTEXT, skipping migration...")
			return nil
		}

		logrus.Info("Altering request_logs.request_body from TEXT to MEDIUMTEXT...")

		if err := db.Exec("ALTER TABLE request_logs MODIFY COLUMN request_body MEDIUMTEXT").Error; err != nil {
			return err
		}

		logrus.Info("Migration v1.2.0 completed: request_body column altered to MEDIUMTEXT")
	} else {
		// SQLite 不支持 MEDIUMTEXT，使用 TEXT 即可（SQLite TEXT 最大 1GB）
		logrus.Info("Skipping MEDIUMTEXT migration for non-MySQL database (SQLite TEXT type is sufficient)")
	}

	return nil
}
