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

// buildUnionQuery 构建跨表查询的 UNION ALL 语句
func (s *LogService) buildUnionQuery(tables []string, whereClause string) string {
	if len(tables) == 0 {
		return ""
	}

	var queryParts []string
	for _, table := range tables {
		part := fmt.Sprintf("SELECT * FROM %s", table)
		if whereClause != "" {
			part += " WHERE " + whereClause
		}
		queryParts = append(queryParts, part)
	}

	return strings.Join(queryParts, " UNION ALL ")
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
				tables = utils.GetLogTablesForDateRange(startTime, endTime)
			}
		}
	}

	// 如果无法获取时间范围或只有一张表，使用默认查询
	if len(tables) <= 1 {
		return s.DB.Model(&models.RequestLog{}).Scopes(s.logFiltersScope(c))
	}

	// 多表查询：构建 UNION ALL 查询
	// 构建 WHERE 子句和参数
	var args []interface{}
	whereConditions := []string{}

	// 重新构建 WHERE 条件
	if parentGroupName := c.Query("parent_group_name"); parentGroupName != "" {
		whereConditions = append(whereConditions, "parent_group_name LIKE ?")
		args = append(args, "%"+parentGroupName+"%")
	}
	if groupName := c.Query("group_name"); groupName != "" {
		whereConditions = append(whereConditions, "group_name LIKE ?")
		args = append(args, "%"+groupName+"%")
	}
	if keyValue := c.Query("key_value"); keyValue != "" {
		keyHash := s.EncryptionSvc.Hash(keyValue)
		whereConditions = append(whereConditions, "key_hash = ?")
		args = append(args, keyHash)
	}
	if model := c.Query("model"); model != "" {
		whereConditions = append(whereConditions, "model LIKE ?")
		args = append(args, "%"+model+"%")
	}
	if isSuccessStr := c.Query("is_success"); isSuccessStr != "" {
		if isSuccess, err := strconv.ParseBool(isSuccessStr); err == nil {
			whereConditions = append(whereConditions, "is_success = ?")
			args = append(args, isSuccess)
		}
	}
	if requestType := c.Query("request_type"); requestType != "" {
		whereConditions = append(whereConditions, "request_type = ?")
		args = append(args, requestType)
	}
	if statusCodeStr := c.Query("status_code"); statusCodeStr != "" {
		if statusCode, err := strconv.Atoi(statusCodeStr); err == nil {
			whereConditions = append(whereConditions, "status_code = ?")
			args = append(args, statusCode)
		}
	}
	if sourceIP := c.Query("source_ip"); sourceIP != "" {
		whereConditions = append(whereConditions, "source_ip = ?")
		args = append(args, sourceIP)
	}
	if errorContains := c.Query("error_contains"); errorContains != "" {
		whereConditions = append(whereConditions, "error_message LIKE ?")
		args = append(args, "%"+errorContains+"%")
	}

	whereClause := ""
	if len(whereConditions) > 0 {
		whereClause = strings.Join(whereConditions, " AND ")
	}

	unionSQL := s.buildUnionQuery(tables, whereClause)

	// 使用 UNION ALL 查询所有表
	return s.DB.Raw(unionSQL, args...)
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

	baseQuery := s.DB.Model(&models.RequestLog{}).Scopes(s.logFiltersScope(c)).Where("key_hash IS NOT NULL AND key_hash != ''")

	// 使用窗口函数获取每个 key_hash 的最新记录（避免同一密钥因多次加密产生重复）
	err := s.DB.Raw(`
		SELECT
			key_value,
			group_name,
			status_code
		FROM (
			SELECT
				key_value,
				key_hash,
				group_name,
				status_code,
				ROW_NUMBER() OVER (PARTITION BY key_hash ORDER BY timestamp DESC) as rn
			FROM (?) as filtered_logs
		) ranked
		WHERE rn = 1
		ORDER BY key_hash
	`, baseQuery).Scan(&results).Error

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
