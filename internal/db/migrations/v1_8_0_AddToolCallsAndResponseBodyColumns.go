package db

import (
	"fmt"
	"gpt-load/internal/utils"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// V1_8_0_AddToolCallsAndResponseBodyColumns 为所有已存在的日志分表
// 补充 tool_calls 与 response_body 列，用于记录工具调用信息和模型响应体
func V1_8_0_AddToolCallsAndResponseBodyColumns(db *gorm.DB) error {
	logrus.Info("Starting migration v1.8.0: Add tool_calls and response_body columns to log tables...")

	dialect := db.Dialector.Name()

	// 收集所有需要处理的表：request_logs（若仍存在）+ 所有 request_logs_YYYYMMDD 分表
	tables := collectAllLogTables(db, dialect)

	columnsToAdd := map[string]string{
		"tool_calls":    "LONGTEXT",
		"response_body": "LONGTEXT",
	}

	if dialect != "mysql" {
		columnsToAdd["tool_calls"] = "TEXT"
		columnsToAdd["response_body"] = "TEXT"
	}

	for _, table := range tables {
		if err := addColumnsIfMissingV18(db, dialect, table, columnsToAdd); err != nil {
			return fmt.Errorf("failed to migrate table %s: %w", table, err)
		}
	}

	logrus.Info("Migration v1.8.0 completed: tool_calls and response_body columns ensured")
	return nil
}

// collectAllLogTables 收集 request_logs 旧表以及所有合法的分表名
func collectAllLogTables(db *gorm.DB, dialect string) []string {
	var rawNames []string

	switch dialect {
	case "mysql":
		var results []struct {
			TableName string `gorm:"column:TABLE_NAME"`
		}
		dbName := ""
		db.Raw("SELECT DATABASE()").Scan(&dbName)
		db.Raw("SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME LIKE 'request_logs%'", dbName).Scan(&results)
		for _, r := range results {
			rawNames = append(rawNames, r.TableName)
		}
	default:
		var results []struct {
			Name string `gorm:"column:name"`
		}
		db.Raw("SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'request_logs%'").Scan(&results)
		for _, r := range results {
			rawNames = append(rawNames, r.Name)
		}
	}

	// 过滤：保留 request_logs 旧表与合法分表 request_logs_YYYYMMDD
	var tables []string
	for _, name := range rawNames {
		if name == "request_logs" {
			tables = append(tables, name)
			continue
		}
		if utils.ValidateLogTableName(name) {
			tables = append(tables, name)
		}
	}
	return tables
}

// addColumnsIfMissingV18 为指定表添加缺失的列
func addColumnsIfMissingV18(db *gorm.DB, dialect, table string, columns map[string]string) error {
	for col, def := range columns {
		exists, err := columnExistsV18(db, dialect, table, col)
		if err != nil {
			return fmt.Errorf("failed to check column %s on %s: %w", col, table, err)
		}
		if exists {
			continue
		}

		alterSQL := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col, def)
		if err := db.Exec(alterSQL).Error; err != nil {
			return fmt.Errorf("failed to add column %s to %s: %w", col, table, err)
		}
		logrus.Infof("Added column %s to table %s", col, table)
	}
	return nil
}

// columnExistsV18 检查指定表的列是否存在
func columnExistsV18(db *gorm.DB, dialect, table, column string) (bool, error) {
	var count int64
	if dialect == "mysql" {
		err := db.Raw(`
			SELECT COUNT(*)
			FROM information_schema.COLUMNS
			WHERE TABLE_SCHEMA = DATABASE()
			AND TABLE_NAME = ?
			AND COLUMN_NAME = ?
		`, table, column).Scan(&count).Error
		return count > 0, err
	}
	// SQLite
	err := db.Raw(fmt.Sprintf(`
		SELECT COUNT(*)
		FROM pragma_table_info('%s')
		WHERE name = ?
	`, table), column).Scan(&count).Error
	return count > 0, err
}
