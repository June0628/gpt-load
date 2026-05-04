package services

import (
	"encoding/csv"
	"fmt"
	"gpt-load/internal/encryption"
	"gpt-load/internal/models"
	"gpt-load/internal/utils"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// ExportableLogKey defines the structure for the data to be exported to CSV.
type ExportableLogKey struct {
	KeyValue   string `gorm:"column:key_value"`
	GroupName  string `gorm:"column:group_name"`
	StatusCode int    `gorm:"column:status_code"`
}

// buildUnionSubQuery 构建跨表查询的 UNION ALL 子查询 SQL（保留用于导出等场景）
func (s *LogService) buildUnionSubQuery(tables []string) string {
	if len(tables) == 0 {
		return ""
	}

	var queryParts []string
	for _, table := range tables {
		queryParts = append(queryParts, fmt.Sprintf("SELECT * FROM %s", table))
	}

	return strings.Join(queryParts, " UNION ALL ")
}

// buildTableLatestKeysQuery 构建单个表的最新 key_hash 查询
// 使用 MAX(timestamp) + GROUP BY 获取每个 key_hash 的最新记录
// 使用 ? 占位符进行参数化查询，防止 SQL 注入
// 返回 SQL 片段和对应的参数列表
func (s *LogService) buildTableLatestKeysQuery(c *gin.Context, table string) (string, []interface{}) {
	var conditions []string
	var args []interface{}

	conditions = append(conditions, "key_hash IS NOT NULL AND key_hash != ''")

	if parentGroupName := c.Query("parent_group_name"); parentGroupName != "" {
		conditions = append(conditions, "parent_group_name LIKE ?")
		args = append(args, "%"+parentGroupName+"%")
	}
	if groupName := c.Query("group_name"); groupName != "" {
		conditions = append(conditions, "group_name LIKE ?")
		args = append(args, "%"+groupName+"%")
	}
	if keyValue := c.Query("key_value"); keyValue != "" {
		keyHash := s.EncryptionSvc.Hash(keyValue)
		conditions = append(conditions, "key_hash = ?")
		args = append(args, keyHash)
	}
	if model := c.Query("model"); model != "" {
		conditions = append(conditions, "model LIKE ?")
		args = append(args, "%"+model+"%")
	}
	if isSuccessStr := c.Query("is_success"); isSuccessStr != "" {
		if isSuccess, err := strconv.ParseBool(isSuccessStr); err == nil {
			conditions = append(conditions, "is_success = ?")
			args = append(args, isSuccess)
		}
	}
	if requestType := c.Query("request_type"); requestType != "" {
		conditions = append(conditions, "request_type = ?")
		args = append(args, requestType)
	}
	if statusCodeStr := c.Query("status_code"); statusCodeStr != "" {
		if statusCode, err := strconv.Atoi(statusCodeStr); err == nil {
			conditions = append(conditions, "status_code = ?")
			args = append(args, statusCode)
		}
	}
	if sourceIP := c.Query("source_ip"); sourceIP != "" {
		conditions = append(conditions, "source_ip = ?")
		args = append(args, sourceIP)
	}
	if errorContains := c.Query("error_contains"); errorContains != "" {
		conditions = append(conditions, "error_message LIKE ?")
		args = append(args, "%"+errorContains+"%")
	}
	if startTimeStr := c.Query("start_time"); startTimeStr != "" {
		if startTime, err := time.Parse(time.RFC3339, startTimeStr); err == nil {
			conditions = append(conditions, "timestamp >= ?")
			args = append(args, startTime)
		}
	}
	if endTimeStr := c.Query("end_time"); endTimeStr != "" {
		if endTime, err := time.Parse(time.RFC3339, endTimeStr); err == nil {
			conditions = append(conditions, "timestamp <= ?")
			args = append(args, endTime)
		}
	}

	whereClause := strings.Join(conditions, " AND ")

	// 生成带占位符的 SQL，参数在调用时通过 Raw 的第二个参数传入
	sql := fmt.Sprintf(`
		SELECT l.key_value, l.key_hash, l.group_name, l.status_code, l.timestamp
		FROM %s l
		INNER JOIN (
			SELECT key_hash, MAX(timestamp) as max_ts
			FROM %s
			WHERE %s
			GROUP BY key_hash
		) latest ON l.key_hash = latest.key_hash AND l.timestamp = latest.max_ts
	`, table, table, whereClause)

	return sql, args
}

// BatchDecryptLogs 批量解密日志中的密钥，使用加密服务的批量解密方法
// 使用指针切片避免值拷贝
func (s *LogService) BatchDecryptLogs(logs []*models.RequestLog) {
	if len(logs) == 0 {
		return
	}

	// 收集所有需要解密的密钥
	var keyValues []string
	for _, log := range logs {
		if log.KeyValue != "" {
			keyValues = append(keyValues, log.KeyValue)
		}
	}

	// 使用加密服务的批量解密方法
	decryptedMap := s.EncryptionSvc.BatchDecrypt(keyValues)

	// 更新日志记录
	for _, log := range logs {
		if log.KeyValue != "" {
			if decrypted, exists := decryptedMap[log.KeyValue]; exists {
				log.KeyValue = decrypted
			}
		}
	}
}

// LogService provides services related to request logs.
type LogService struct {
	DB            *gorm.DB
	EncryptionSvc encryption.Service
}

// NewLogService creates a new LogService.
func NewLogService(db *gorm.DB, encryptionSvc encryption.Service) *LogService {
	return &LogService{
		DB:            db,
		EncryptionSvc: encryptionSvc,
	}
}

// logFiltersScope returns a GORM scope function that applies filters from the Gin context.
func (s *LogService) logFiltersScope(c *gin.Context) func(db *gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		if parentGroupName := c.Query("parent_group_name"); parentGroupName != "" {
			db = db.Where("parent_group_name LIKE ?", "%"+parentGroupName+"%")
		}
		if groupName := c.Query("group_name"); groupName != "" {
			db = db.Where("group_name LIKE ?", "%"+groupName+"%")
		}
		if keyValue := c.Query("key_value"); keyValue != "" {
			keyHash := s.EncryptionSvc.Hash(keyValue)
			db = db.Where("key_hash = ?", keyHash)
		}
		if model := c.Query("model"); model != "" {
			db = db.Where("model LIKE ?", "%"+model+"%")
		}
		if isSuccessStr := c.Query("is_success"); isSuccessStr != "" {
			if isSuccess, err := strconv.ParseBool(isSuccessStr); err == nil {
				db = db.Where("is_success = ?", isSuccess)
			}
		}
		if requestType := c.Query("request_type"); requestType != "" {
			db = db.Where("request_type = ?", requestType)
		}
		if statusCodeStr := c.Query("status_code"); statusCodeStr != "" {
			if statusCode, err := strconv.Atoi(statusCodeStr); err == nil {
				db = db.Where("status_code = ?", statusCode)
			}
		}
		if sourceIP := c.Query("source_ip"); sourceIP != "" {
			db = db.Where("source_ip = ?", sourceIP)
		}
		if errorContains := c.Query("error_contains"); errorContains != "" {
			db = db.Where("error_message LIKE ?", "%"+errorContains+"%")
		}
		if startTimeStr := c.Query("start_time"); startTimeStr != "" {
			if startTime, err := time.Parse(time.RFC3339, startTimeStr); err == nil {
				db = db.Where("timestamp >= ?", startTime)
			}
		}
		if endTimeStr := c.Query("end_time"); endTimeStr != "" {
			if endTime, err := time.Parse(time.RFC3339, endTimeStr); err == nil {
				db = db.Where("timestamp <= ?", endTime)
			}
		}
		return db
	}
}

func (s *LogService) tableExists(tableName string) bool {
	return s.DB.Migrator().HasTable(tableName)
}

// getExistingLogTables 批量查询存在的表，使用一次 SQL 查询验证多个表是否存在
func (s *LogService) getExistingLogTables(tableNames []string) []string {
	if len(tableNames) == 0 {
		return nil
	}

	// 使用单个 SQL 查询获取所有存在的表名
	var existingTableNames []string
	dialect := s.DB.Dialector.Name()

	var query string
	switch dialect {
	case "mysql":
		// MySQL: 查询 information_schema.TABLES
		query = `SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME IN (?)`
	case "sqlite":
		// SQLite: 使用 sqlite_master
		query = `SELECT name FROM sqlite_master WHERE type='table' AND name IN (?)`
	default:
		// 降级为逐表查询
		existingTables := make([]string, 0, len(tableNames))
		for _, tableName := range tableNames {
			if s.tableExists(tableName) {
				existingTables = append(existingTables, tableName)
			}
		}
		return existingTables
	}

	if err := s.DB.Raw(query, tableNames).Scan(&existingTableNames).Error; err != nil {
		logrus.WithError(err).Warn("Failed to batch query existing tables, falling back to single queries")
		// 降级为逐表查询
		existingTables := make([]string, 0, len(tableNames))
		for _, tableName := range tableNames {
			if s.tableExists(tableName) {
				existingTables = append(existingTables, tableName)
			}
		}
		return existingTables
	}

	return existingTableNames
}

func (s *LogService) getEmptyLogsQuery() *gorm.DB {
	todayTable := utils.GetDailyLogTableName(time.Now())
	return s.DB.Table(todayTable).Where("1 = 0")
}

// GetLogsQuery returns a GORM query for fetching logs with filters.
// 支持跨多个按日期分表的日志表查询
func (s *LogService) GetLogsQuery(c *gin.Context) *gorm.DB {
	// 获取时间范围用于确定查询哪些表
	startTimeStr := c.Query("start_time")
	endTimeStr := c.Query("end_time")

	var tables []string
	if startTimeStr != "" && endTimeStr != "" {
		if startTime, err := time.Parse(time.RFC3339, startTimeStr); err == nil {
			if endTime, err := time.Parse(time.RFC3339, endTimeStr); err == nil {
				// 解析时间后，将其转换为本地时区用于计算日志表范围
				// 这样可以确保无论前端发送的是 UTC 还是本地时间，
				// 都能正确映射到服务器时区的日志表
				localStart := startTime.Local()
				localEnd := endTime.Local()
				tables = utils.GetLogTablesForDateRange(localStart, localEnd)
			}
		}
	}

	// 如果无法获取时间范围，使用默认查询（查询当天的表）
	if len(tables) == 0 {
		today := time.Now()
		tables = []string{utils.GetDailyLogTableName(today)}
	}

	existingTables := s.getExistingLogTables(tables)

	// 如果请求的是默认今天日志，但今天的表还不存在，返回空结果而不是数据库报错
	if len(existingTables) == 0 {
		return s.getEmptyLogsQuery().Scopes(s.logFiltersScope(c))
	}

	// 如果只有一张表，直接查询该表
	if len(existingTables) == 1 {
		return s.DB.Table(existingTables[0]).Scopes(s.logFiltersScope(c))
	}

	// 多表查询：使用子查询方式，将 UNION ALL 作为子查询，外层应用过滤条件
	// 这种方式允许 GORM 正确处理分页和排序
	subQuery := s.DB.Raw(s.buildUnionSubQuery(existingTables))
	return s.DB.Table("(?) as combined_logs", subQuery).Scopes(s.logFiltersScope(c))
}

// StreamLogKeysToCSV fetches unique keys from logs based on filters and streams them as a CSV.
func (s *LogService) StreamLogKeysToCSV(c *gin.Context, writer io.Writer) error {
	// Create a CSV writer
	csvWriter := csv.NewWriter(writer)
	defer csvWriter.Flush()

	// Write CSV header
	header := []string{"key_value", "group_name", "status_code"}
	if err := csvWriter.Write(header); err != nil {
		return fmt.Errorf("failed to write CSV header: %w", err)
	}

	var results []ExportableLogKey

	// 获取时间范围用于确定查询哪些表
	startTimeStr := c.Query("start_time")
	endTimeStr := c.Query("end_time")

	var tables []string
	if startTimeStr != "" && endTimeStr != "" {
		if startTime, err := time.Parse(time.RFC3339, startTimeStr); err == nil {
			if endTime, err := time.Parse(time.RFC3339, endTimeStr); err == nil {
				localStart := startTime.Local()
				localEnd := endTime.Local()
				tables = utils.GetLogTablesForDateRange(localStart, localEnd)
			}
		}
	}

	if len(tables) == 0 {
		today := time.Now()
		tables = []string{utils.GetDailyLogTableName(today)}
	}

	existingTables := s.getExistingLogTables(tables)

	if len(existingTables) == 0 {
		return nil
	}

	// 使用逐表查询 + GROUP BY + MAX 方式获取每个 key_hash 的最新记录
	// 避免在大数据量上使用窗口函数
	var subQueries []string
	var allArgs []interface{}
	for _, table := range existingTables {
		sql, args := s.buildTableLatestKeysQuery(c, table)
		subQueries = append(subQueries, sql)
		allArgs = append(allArgs, args...)
	}

	if len(subQueries) == 0 {
		return nil
	}

	// 合并所有表的最新记录，按 key_hash 去重
	query := fmt.Sprintf(`
		SELECT key_value, key_hash, group_name, status_code
		FROM (
			%s
		) combined
		GROUP BY key_hash
		ORDER BY key_hash
	`, strings.Join(subQueries, " UNION ALL "))

	err := s.DB.Raw(query, allArgs...).Scan(&results).Error

	if err != nil {
		return fmt.Errorf("failed to fetch log keys: %w", err)
	}

	// 解密并写入 CSV 数据
	for _, record := range results {
		// 解密密钥用于 CSV 导出
		decryptedKey := record.KeyValue
		if record.KeyValue != "" {
			if decrypted, err := s.EncryptionSvc.Decrypt(record.KeyValue); err != nil {
				logrus.WithError(err).WithField("key_value", record.KeyValue).Error("Failed to decrypt key for CSV export")
				decryptedKey = "failed-to-decrypt"
			} else {
				decryptedKey = decrypted
			}
		}

		csvRecord := []string{
			decryptedKey,
			record.GroupName,
			strconv.Itoa(record.StatusCode),
		}
		if err := csvWriter.Write(csvRecord); err != nil {
			return fmt.Errorf("failed to write CSV record: %w", err)
		}
	}

	return nil
}
