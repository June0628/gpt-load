package db

import (
	"fmt"
	"gpt-load/internal/utils"
	"time"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// V1_4_0_CreateDailyLogTables 创建按日期分表的日志表结构
// 将原来的单一 request_logs 表拆分为每天一张表：request_logs_YYYYMMDD
func V1_4_0_CreateDailyLogTables(db *gorm.DB) error {
	logrus.Info("Starting migration v1.4.0: Creating daily log tables...")

	// 获取当前数据库类型
	dialect := db.Dialector.Name()

	// 获取当前日期，创建今天的表
	today := time.Now()
	tableName := utils.GetDailyLogTableName(today)

	// 创建今天的日志表
	if err := createDailyLogTable(db, dialect, tableName); err != nil {
		return fmt.Errorf("failed to create today's log table: %w", err)
	}

	logrus.Infof("Created daily log table: %s", tableName)

	// 如果是 MySQL，还需要创建历史数据迁移
	if dialect == "mysql" {
		// 将旧表的数据迁移到按日期分表中
		if err := migrateLegacyData(db); err != nil {
			return fmt.Errorf("failed to migrate legacy data: %w", err)
		}
	}

	logrus.Info("Migration v1.4.0 completed: Daily log tables created")
	return nil
}

// createDailyLogTable 创建指定日期的日志表
func createDailyLogTable(db *gorm.DB, dialect string, tableName string) error {
	var createSQL string

	if dialect == "mysql" {
		createSQL = fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS %s (
				id VARCHAR(36) PRIMARY KEY,
				timestamp DATETIME(3) NOT NULL,
				group_id BIGINT UNSIGNED NOT NULL,
				group_name VARCHAR(255),
				parent_group_id BIGINT UNSIGNED,
				parent_group_name VARCHAR(255),
				key_value TEXT,
				key_hash VARCHAR(128),
				model VARCHAR(255),
				is_success TINYINT(1) NOT NULL DEFAULT 0,
				source_ip VARCHAR(64),
				status_code INT NOT NULL,
				request_path VARCHAR(500),
				duration_ms BIGINT NOT NULL,
				error_message TEXT,
				user_agent VARCHAR(512),
				request_type VARCHAR(20) NOT NULL DEFAULT 'final',
				upstream_addr VARCHAR(500),
				is_stream TINYINT(1) NOT NULL DEFAULT 0,
				request_body MEDIUMTEXT,
				agent_files LONGTEXT,
				INDEX idx_timestamp (timestamp),
				INDEX idx_group_id (group_id),
				INDEX idx_group_name (group_name),
				INDEX idx_parent_group_id (parent_group_id),
				INDEX idx_parent_group_name (parent_group_name),
				INDEX idx_key_hash (key_hash),
				INDEX idx_model (model),
				INDEX idx_request_type (request_type)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
		`, tableName)
	} else {
		// SQLite
		createSQL = fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS %s (
				id TEXT PRIMARY KEY,
				timestamp TEXT NOT NULL,
				group_id INTEGER NOT NULL,
				group_name TEXT,
				parent_group_id INTEGER,
				parent_group_name TEXT,
				key_value TEXT,
				key_hash TEXT,
				model TEXT,
				is_success INTEGER NOT NULL DEFAULT 0,
				source_ip TEXT,
				status_code INTEGER NOT NULL,
				request_path TEXT,
				duration_ms INTEGER NOT NULL,
				error_message TEXT,
				user_agent TEXT,
				request_type TEXT NOT NULL DEFAULT 'final',
				upstream_addr TEXT,
				is_stream INTEGER NOT NULL DEFAULT 0,
				request_body TEXT,
				agent_files TEXT
			)
		`, tableName)
	}

	return db.Exec(createSQL).Error
}

// ensureLegacyTableColumns 确保旧表中有所有必需的列
func ensureLegacyTableColumns(db *gorm.DB) error {
	logrus.Info("Checking and adding missing columns to legacy request_logs table...")

	// 定义需要检查的列及其定义
	requiredColumns := map[string]string{
		"duration_ms":    "BIGINT NOT NULL DEFAULT 0",
		"request_path":   "VARCHAR(500)",
		"error_message":  "TEXT",
		"user_agent":     "VARCHAR(512)",
		"request_type":   "VARCHAR(20) NOT NULL DEFAULT 'final'",
		"upstream_addr":   "VARCHAR(500)",
		"is_stream":      "TINYINT(1) NOT NULL DEFAULT 0",
		"agent_files":    "LONGTEXT",
	}

	for columnName, columnDef := range requiredColumns {
		var columnExists int64
		err := db.Raw(`
			SELECT COUNT(*)
			FROM information_schema.COLUMNS
			WHERE TABLE_SCHEMA = DATABASE()
			AND TABLE_NAME = 'request_logs'
			AND COLUMN_NAME = ?
		`, columnName).Scan(&columnExists).Error

		if err != nil {
			return fmt.Errorf("failed to check column %s existence: %w", columnName, err)
		}

		if columnExists == 0 {
			logrus.Infof("Adding missing column %s to request_logs table", columnName)
			alterSQL := fmt.Sprintf("ALTER TABLE request_logs ADD COLUMN %s %s", columnName, columnDef)
			if err := db.Exec(alterSQL).Error; err != nil {
				return fmt.Errorf("failed to add column %s: %w", columnName, err)
			}
			logrus.Infof("Successfully added column %s", columnName)
		}
	}

	return nil
}

// migrateLegacyData 将旧表的数据迁移到按日期分表中
func migrateLegacyData(db *gorm.DB) error {
	logrus.Info("Migrating legacy data from request_logs to daily tables...")

	// 检查旧表是否存在
	var tableExists int64
	err := db.Raw(`
		SELECT COUNT(*)
		FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE()
		AND TABLE_NAME = 'request_logs'
	`).Scan(&tableExists).Error

	if err != nil {
		return fmt.Errorf("failed to check legacy table existence: %w", err)
	}

	if tableExists == 0 {
		logrus.Info("Legacy request_logs table does not exist, skipping data migration")
		return nil
	}

	// 检查并添加缺失的列
	if err := ensureLegacyTableColumns(db); err != nil {
		return fmt.Errorf("failed to ensure legacy table columns: %w", err)
	}

	// 检查是否有数据
	var count int64
	if err := db.Table("request_logs").Count(&count).Error; err != nil {
		return fmt.Errorf("failed to count legacy data: %w", err)
	}

	if count == 0 {
		logrus.Info("Legacy request_logs table is empty, skipping data migration")
		return nil
	}

	logrus.Infof("Found %d records in legacy request_logs table, starting migration...", count)

	// 按日期分组迁移数据
	// 获取所有不同的日期
	var dates []time.Time
	if err := db.Table("request_logs").
		Distinct("DATE(timestamp) as date").
		Pluck("date", &dates).Error; err != nil {
		return fmt.Errorf("failed to get distinct dates: %w", err)
	}

	for _, date := range dates {
		dateStr := date.Format("2006-01-02")
		tableName := utils.GetDailyLogTableName(date)

		// 创建该日期的表
		if err := createDailyLogTable(db, "mysql", tableName); err != nil {
			return fmt.Errorf("failed to create table %s: %w", tableName, err)
		}

		// 迁移该日期的数据
		insertSQL := fmt.Sprintf(`
			INSERT INTO %s (
				id, timestamp, group_id, group_name, parent_group_id, parent_group_name,
				key_value, key_hash, model, is_success, source_ip, status_code,
				request_path, duration_ms, error_message, user_agent, request_type,
				upstream_addr, is_stream, request_body, agent_files
			)
			SELECT
				id, timestamp, group_id, group_name, parent_group_id, parent_group_name,
				key_value, key_hash, model, is_success, source_ip, status_code,
				request_path, duration_ms, error_message, user_agent, request_type,
				upstream_addr, is_stream, request_body, agent_files
			FROM request_logs
			WHERE DATE(timestamp) = ?
		`, tableName)

		result := db.Exec(insertSQL, dateStr)
		if result.Error != nil {
			return fmt.Errorf("failed to migrate data for date %s: %w", dateStr, result.Error)
		}

		logrus.Infof("Migrated %d records for date %s to table %s", result.RowsAffected, dateStr, tableName)
	}

	// 重命名旧表为备份表
	backupTableName := fmt.Sprintf("request_logs_backup_%s", time.Now().Format("20060102150405"))
	if err := db.Exec(fmt.Sprintf("RENAME TABLE request_logs TO %s", backupTableName)).Error; err != nil {
		return fmt.Errorf("failed to rename legacy table: %w", err)
	}

	logrus.Infof("Legacy table renamed to %s", backupTableName)
	logrus.Info("Legacy data migration completed successfully")

	return nil
}
