package utils

import (
	"fmt"

	"gorm.io/gorm"
)

// GetDatabaseTotalSizeBytes returns the total size of all tables in the current database in bytes.
func GetDatabaseTotalSizeBytes(db *gorm.DB) (int64, error) {
	dialect := db.Dialector.Name()

	switch dialect {
	case "mysql":
		var result struct {
			TotalSize int64 `gorm:"column:total_size"`
		}
		err := db.Raw("SELECT COALESCE(SUM(DATA_LENGTH + INDEX_LENGTH), 0) AS total_size FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE()").Scan(&result).Error
		if err != nil {
			return 0, fmt.Errorf("failed to query database total size: %w", err)
		}
		return result.TotalSize, nil
	default:
		// 优先使用 dbstat 虚拟表获取精确大小
		var result struct {
			TotalSize int64 `gorm:"column:total_size"`
		}
		err := db.Raw("SELECT COALESCE(SUM(pgsize), 0) AS total_size FROM dbstat WHERE name NOT LIKE 'sqlite_%'").Scan(&result).Error
		if err == nil && result.TotalSize > 0 {
			return result.TotalSize, nil
		}
		// dbstat 不可用时，使用 PRAGMA page_count * page_size 获取整个数据库文件大小
		return getSQLiteTotalSizeByPragma(db)
	}
}

// GetTableSizeBytes returns the size of a single table in bytes.
func GetTableSizeBytes(db *gorm.DB, tableName string) (int64, error) {
	dialect := db.Dialector.Name()

	switch dialect {
	case "mysql":
		var result struct {
			TotalSize int64 `gorm:"column:total_size"`
		}
		err := db.Raw("SELECT COALESCE(DATA_LENGTH + INDEX_LENGTH, 0) AS total_size FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?", tableName).Scan(&result).Error
		if err != nil {
			return 0, fmt.Errorf("failed to query table size for %s: %w", tableName, err)
		}
		return result.TotalSize, nil
	default:
		// 优先使用 dbstat 虚拟表获取单表大小
		var result struct {
			TotalSize int64 `gorm:"column:total_size"`
		}
		err := db.Raw("SELECT COALESCE(SUM(pgsize), 0) AS total_size FROM dbstat WHERE name = ?", tableName).Scan(&result).Error
		if err == nil {
			return result.TotalSize, nil
		}
		// dbstat 不可用时，无法获取单表精确大小，返回 0
		return 0, nil
	}
}

// getSQLiteTotalSizeByPragma uses PRAGMA page_count and page_size to get the total database file size.
// This is a reliable fallback when dbstat virtual table is not available.
func getSQLiteTotalSizeByPragma(db *gorm.DB) (int64, error) {
	var pageCount int64
	if err := db.Raw("PRAGMA page_count").Scan(&pageCount).Error; err != nil {
		return 0, fmt.Errorf("failed to get SQLite page_count: %w", err)
	}
	var pageSize int64
	if err := db.Raw("PRAGMA page_size").Scan(&pageSize).Error; err != nil {
		return 0, fmt.Errorf("failed to get SQLite page_size: %w", err)
	}
	return pageCount * pageSize, nil
}

// FormatBytes formats a byte count into a human-readable string.
func FormatBytes(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	if bytes >= GB {
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(GB))
	}
	if bytes >= MB {
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(MB))
	}
	if bytes >= KB {
		return fmt.Sprintf("%.2f KB", float64(bytes)/float64(KB))
	}
	return fmt.Sprintf("%d B", bytes)
}
