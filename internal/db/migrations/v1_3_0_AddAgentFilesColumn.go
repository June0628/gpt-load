package db

import (
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// V1_3_0_AddAgentFilesColumn 在 request_logs 表中添加 agent_files 字段
// 用于存储用户通过agent（如Cline插件）上传的文件内容
func V1_3_0_AddAgentFilesColumn(db *gorm.DB) error {
	// 检查字段是否已存在
	var columnExists bool
	if db.Dialector.Name() == "mysql" {
		err := db.Raw(`
			SELECT COUNT(*)
			FROM information_schema.COLUMNS
			WHERE TABLE_SCHEMA = DATABASE()
			AND TABLE_NAME = 'request_logs'
			AND COLUMN_NAME = 'agent_files'
		`).Scan(&columnExists).Error

		if err != nil {
			logrus.WithError(err).Warn("Failed to check agent_files column existence, attempting migration anyway")
		} else if columnExists {
			logrus.Info("agent_files column already exists, skipping migration...")
			return nil
		}
	} else {
		// SQLite
		err := db.Raw(`
			SELECT COUNT(*)
			FROM pragma_table_info('request_logs')
			WHERE name = 'agent_files'
		`).Scan(&columnExists).Error

		if err != nil {
			logrus.WithError(err).Warn("Failed to check agent_files column existence, attempting migration anyway")
		} else if columnExists {
			logrus.Info("agent_files column already exists, skipping migration...")
			return nil
		}
	}

	logrus.Info("Adding agent_files column to request_logs table...")

	// MySQL 使用 LONGTEXT 以支持大容量文件内容
	if db.Dialector.Name() == "mysql" {
		if err := db.Exec("ALTER TABLE request_logs ADD COLUMN agent_files LONGTEXT").Error; err != nil {
			return err
		}
	} else {
		// SQLite 使用 TEXT（最大1GB）
		if err := db.Exec("ALTER TABLE request_logs ADD COLUMN agent_files TEXT").Error; err != nil {
			return err
		}
	}

	logrus.Info("Migration v1.3.0 completed: agent_files column added")
	return nil
}
