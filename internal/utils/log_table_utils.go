package utils

import (
	"fmt"
	"time"
)

// GetDailyLogTableName 根据日期获取日志表名
func GetDailyLogTableName(date time.Time) string {
	return fmt.Sprintf("request_logs_%s", date.Format("20060102"))
}

// GetLogTablesForDateRange 获取指定日期范围内所有日志表名
func GetLogTablesForDateRange(startTime, endTime time.Time) []string {
	var tables []string
	current := startTime.Truncate(24 * time.Hour)
	end := endTime.Truncate(24 * time.Hour)

	for !current.After(end) {
		tables = append(tables, GetDailyLogTableName(current))
		current = current.Add(24 * time.Hour)
	}

	return tables
}
