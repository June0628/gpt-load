package utils

import (
	"fmt"
	"regexp"
	"sort"
	"time"

	"gorm.io/gorm"
)

// logTableNameRegex 匹配合法的日志表名：request_logs_ 后跟8位数字 (YYYYMMDD)
var logTableNameRegex = regexp.MustCompile(`^request_logs_\d{8}$`)

// ValidateLogTableName 验证日志表名格式是否合法，防止 SQL 注入
// 合法格式：request_logs_YYYYMMDD，其中 YYYYMMDD 为8位数字
func ValidateLogTableName(tableName string) bool {
	return logTableNameRegex.MatchString(tableName)
}

// IsTodayLogTable 检查给定的表名是否是当天正在写入的日志表
func IsTodayLogTable(tableName string) bool {
	todayTable := GetDailyLogTableName(time.Now())
	return tableName == todayTable
}

// GetDailyLogTableName 根据日期获取日志表名
func GetDailyLogTableName(date time.Time) string {
	return fmt.Sprintf("request_logs_%s", date.Format("20060102"))
}

// GetLogTablesForDateRange 获取指定日期范围内所有日志表名
func GetLogTablesForDateRange(startTime, endTime time.Time) []string {
	var tables []string
	// 使用本地日期来正确获取每一天的起始时间
	// time.Truncate(24 * time.Hour) 会基于 UTC 截断，导致使用非本地时区的日期
	// 使用 time.Date() 创建本地日期 00:00:00 的时间，确保正确处理跨时区情况
	startY, startM, startD := startTime.Date()
	endY, endM, endD := endTime.Date()
	loc := startTime.Location()

	current := time.Date(startY, startM, startD, 0, 0, 0, 0, loc)
	end := time.Date(endY, endM, endD, 0, 0, 0, 0, loc)

	for !current.After(end) {
		tables = append(tables, GetDailyLogTableName(current))
		current = current.AddDate(0, 0, 1)
	}

	return tables
}

// GetExistingExpiredLogTables 从数据库中查询实际存在的过期日志表
// cutoffDate 之前（不含 cutoffDate 当天）的表都视为过期
func GetExistingExpiredLogTables(db *gorm.DB, cutoffDate time.Time) []string {
	// 过期表名的上限（不含 cutoffDate 当天）
	cutoffTableName := GetDailyLogTableName(cutoffDate)

	var allTables []string
	dialect := db.Dialector.Name()

	switch dialect {
	case "mysql":
		// MySQL: 从 information_schema 中查询以 request_logs_ 开头的表
		var results []struct {
			TableName string `gorm:"column:TABLE_NAME"`
		}
		dbName := ""
		db.Raw("SELECT DATABASE()").Scan(&dbName)
		db.Raw("SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME LIKE 'request_logs_%'", dbName).Scan(&results)
		for _, r := range results {
			allTables = append(allTables, r.TableName)
		}
	default:
		// SQLite: 从 sqlite_master 中查询
		var results []struct {
			Name string `gorm:"column:name"`
		}
		db.Raw("SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'request_logs_%'").Scan(&results)
		for _, r := range results {
			allTables = append(allTables, r.Name)
		}
	}

	// 过滤出过期的表（表名字典序小于 cutoffTableName 的表）
	var expiredTables []string
	for _, table := range allTables {
		// 严格验证表名格式（request_logs_ + 8位纯数字），防止 SQL 注入
		if !ValidateLogTableName(table) {
			continue
		}
		// 表名字典序比较：request_logs_20260101 < request_logs_20260418
		if table < cutoffTableName {
			expiredTables = append(expiredTables, table)
		}
	}

	sort.Strings(expiredTables)
	return expiredTables
}
