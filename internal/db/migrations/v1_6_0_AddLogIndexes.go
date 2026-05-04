package db

import (
	"fmt"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// V1_6_0_AddLogIndexes 为日志表添加缺失的索引以提升查询性能
// 添加索引的字段：status_code, source_ip
func V1_6_0_AddLogIndexes(db *gorm.DB) error {
	logrus.Info("Starting migration v1.6.0: Adding indexes to log tables...")

	// 获取所有以 request_logs_ 开头的表
	var tables []string
	dialect := db.Dialector.Name()

	if dialect == "mysql" {
		var results []struct {
			TableName string `gorm:"column:TABLE_NAME"`
		}
		dbName := ""
		db.Raw("SELECT DATABASE()").Scan(&dbName)
		db.Raw("SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME LIKE 'request_logs_%'", dbName).Scan(&results)
		for _, r := range results {
			tables = append(tables, r.TableName)
		}
	} else {
		// SQLite
		var results []struct {
			Name string `gorm:"column:name"`
		}
		db.Raw("SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'request_logs_%'").Scan(&results)
		for _, r := range results {
			tables = append(tables, r.Name)
		}
	}

	if len(tables) == 0 {
		logrus.Info("No log tables found, skipping index creation")
		return nil
	}

	// 为每个日志表添加索引
	for _, tableName := range tables {
		if err := addIndexesToTable(db, dialect, tableName); err != nil {
			logrus.WithError(err).WithField("table", tableName).Error("Failed to add indexes to table")
			return fmt.Errorf("failed to add indexes to %s: %w", tableName, err)
		}
		logrus.Infof("Added indexes to table: %s", tableName)
	}

	logrus.Info("Migration v1.6.0 completed: Added indexes to log tables")
	return nil
}

// addIndexesToTable 为指定表添加缺失的索引
func addIndexesToTable(db *gorm.DB, dialect, tableName string) error {
	if dialect == "mysql" {
		// MySQL: 添加 status_code 和 source_ip 索引
		indexes := []struct {
			indexName string
			column    string
		}{
			{fmt.Sprintf("idx_%s_status_code", tableName), "status_code"},
			{fmt.Sprintf("idx_%s_source_ip", tableName), "source_ip"},
		}

		for _, idx := range indexes {
			// 检查索引是否已存在
			var count int64
			err := db.Raw(`
				SELECT COUNT(*) FROM information_schema.STATISTICS
				WHERE TABLE_SCHEMA = DATABASE()
				AND TABLE_NAME = ?
				AND INDEX_NAME = ?
			`, tableName, idx.indexName).Scan(&count).Error

			if err != nil {
				return fmt.Errorf("failed to check index existence: %w", err)
			}

			if count == 0 {
				// 索引不存在，创建它
				createSQL := fmt.Sprintf("CREATE INDEX %s ON %s (%s)", idx.indexName, tableName, idx.column)
				if err := db.Exec(createSQL).Error; err != nil {
					return fmt.Errorf("failed to create index %s: %w", idx.indexName, err)
				}
				logrus.Infof("Created index %s on %s(%s)", idx.indexName, tableName, idx.column)
			} else {
				logrus.Infof("Index %s already exists, skipping", idx.indexName)
			}
		}
	} else {
		// SQLite: 添加 status_code 和 source_ip 索引
		indexes := []struct {
			indexName string
			column    string
		}{
			{fmt.Sprintf("idx_%s_status_code", tableName), "status_code"},
			{fmt.Sprintf("idx_%s_source_ip", tableName), "source_ip"},
		}

		for _, idx := range indexes {
			// 检查索引是否已存在
			var count int64
			err := db.Raw(`
				SELECT COUNT(*) FROM sqlite_master
				WHERE type='index' AND name=?
			`, idx.indexName).Scan(&count).Error

			if err != nil {
				return fmt.Errorf("failed to check index existence: %w", err)
			}

			if count == 0 {
				// 索引不存在，创建它
				createSQL := fmt.Sprintf("CREATE INDEX %s ON %s (%s)", idx.indexName, tableName, idx.column)
				if err := db.Exec(createSQL).Error; err != nil {
					return fmt.Errorf("failed to create index %s: %w", idx.indexName, err)
				}
				logrus.Infof("Created index %s on %s(%s)", idx.indexName, tableName, idx.column)
			} else {
				logrus.Infof("Index %s already exists, skipping", idx.indexName)
			}
		}
	}

	return nil
}
