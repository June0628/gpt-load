package handler

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"gpt-load/internal/db"
	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/i18n"
	"gpt-load/internal/models"
	"gpt-load/internal/response"
	"gpt-load/internal/utils"

	"github.com/gin-gonic/gin"
)

// GetSettings handles the GET /api/settings request.
// It retrieves all system settings, groups them by category, and returns them.
func (s *Server) GetSettings(c *gin.Context) {
	currentSettings := s.SettingsManager.GetSettings()
	settingsInfo := utils.GenerateSettingsMetadata(&currentSettings)

	// Translate settings info
	for i := range settingsInfo {
		if strings.HasPrefix(settingsInfo[i].Name, "config.") {
			settingsInfo[i].Name = i18n.Message(c, settingsInfo[i].Name)
		}
		if strings.HasPrefix(settingsInfo[i].Description, "config.") {
			settingsInfo[i].Description = i18n.Message(c, settingsInfo[i].Description)
		}
		if strings.HasPrefix(settingsInfo[i].Category, "config.") {
			settingsInfo[i].Category = i18n.Message(c, settingsInfo[i].Category)
		}
	}

	// Group settings by category while preserving order
	categorized := make(map[string][]models.SystemSettingInfo)
	var categoryOrder []string
	for _, s := range settingsInfo {
		if _, exists := categorized[s.Category]; !exists {
			categoryOrder = append(categoryOrder, s.Category)
		}
		categorized[s.Category] = append(categorized[s.Category], s)
	}

	// Create the response structure in the correct order
	var responseData []models.CategorizedSettings
	for _, categoryName := range categoryOrder {
		responseData = append(responseData, models.CategorizedSettings{
			CategoryName: categoryName,
			Settings:     categorized[categoryName],
		})
	}

	response.Success(c, responseData)
}

// UpdateSettings handles the PUT /api/settings request.
func (s *Server) UpdateSettings(c *gin.Context) {
	var settingsMap map[string]any
	if err := c.ShouldBindJSON(&settingsMap); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	if len(settingsMap) == 0 {
		response.Success(c, nil)
		return
	}

	// Sanitize proxy_keys input
	if proxyKeys, ok := settingsMap["proxy_keys"]; ok {
		if proxyKeysStr, ok := proxyKeys.(string); ok {
			cleanedKeys := utils.SplitAndTrim(proxyKeysStr, ",")
			settingsMap["proxy_keys"] = strings.Join(cleanedKeys, ",")
		}
	}

	// 更新配置
	if err := s.SettingsManager.UpdateSettings(settingsMap); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrDatabase, err.Error()))
		return
	}

	time.Sleep(100 * time.Millisecond) // 等待异步更新配置

	response.SuccessI18n(c, "settings.update_success", nil)
}

// LogTableInfo 日志表信息
type LogTableInfo struct {
	TableName string `json:"table_name"`
	Date      string `json:"date"`
	RowCount  int64  `json:"row_count"`
}

// GetLogTables handles GET /api/settings/log-tables
// 获取所有存在的日志表列表
func (s *Server) GetLogTables(c *gin.Context) {
	var allTables []string
	dialect := db.DB.Dialector.Name()

	switch dialect {
	case "mysql":
		var results []struct {
			TableName string `gorm:"column:TABLE_NAME"`
		}
		dbName := ""
		db.DB.Raw("SELECT DATABASE()").Scan(&dbName)
		db.DB.Raw("SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME LIKE 'request_logs_%'", dbName).Scan(&results)
		for _, r := range results {
			allTables = append(allTables, r.TableName)
		}
	default:
		var results []struct {
			Name string `gorm:"column:name"`
		}
		db.DB.Raw("SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'request_logs_%'").Scan(&results)
		for _, r := range results {
			allTables = append(allTables, r.Name)
		}
	}

	sort.Sort(sort.Reverse(sort.StringSlice(allTables)))

	var tableInfos []LogTableInfo
	for _, table := range allTables {
		// 严格验证表名格式（前缀 + 8位纯数字）
		if !utils.ValidateLogTableName(table) {
			continue
		}

		// 排除当天正在写入的日志表
		if utils.IsTodayLogTable(table) {
			continue
		}

		suffix := strings.TrimPrefix(table, "request_logs_")
		dateStr := suffix[:4] + "-" + suffix[4:6] + "-" + suffix[6:8]

		var count int64
		if err := db.DB.Table(table).Count(&count).Error; err != nil {
			count = 0
		}

		tableInfos = append(tableInfos, LogTableInfo{
			TableName: table,
			Date:      dateStr,
			RowCount:  count,
		})
	}

	response.Success(c, tableInfos)
}

// ManualUploadRequest 手动上传请求
type ManualUploadRequest struct {
	TableName string `json:"table_name" binding:"required"`
}

// ManualUploadLogTable handles POST /api/settings/log-tables/upload
// 手动上传指定日志表到外部存储
func (s *Server) ManualUploadLogTable(c *gin.Context) {
	var req ManualUploadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.ErrorI18n(c, http.StatusBadRequest, "VALIDATION_ERROR", "log_backup.table_name_required")
		return
	}

	// 验证表名格式，防止 SQL 注入（前缀 + 8位纯数字后缀）
	tableName := req.TableName
	if !utils.ValidateLogTableName(tableName) {
		response.ErrorI18n(c, http.StatusBadRequest, "VALIDATION_ERROR", "log_backup.invalid_table_name")
		return
	}

	// 禁止操作当天正在写入的日志表
	if utils.IsTodayLogTable(tableName) {
		response.ErrorI18n(c, http.StatusBadRequest, "VALIDATION_ERROR", "log_backup.today_table_forbidden")
		return
	}

	// 检查表是否存在
	if !db.DB.Migrator().HasTable(tableName) {
		response.ErrorI18n(c, http.StatusNotFound, "NOT_FOUND", "log_backup.table_not_found")
		return
	}

	// 如果启用了手动上传后自动删除，使用原子性的上传+删除操作防止并发竞态
	settings := s.SettingsManager.GetSettings()
	deleted := false
	if settings.LogUploadDeleteAfterManual {
		if err := s.LogUploadService.UploadAndDeleteTable(tableName); err != nil {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrDatabase, i18n.Message(c, "log_backup.upload_failed")+": "+err.Error()))
			return
		}
		deleted = true
	} else {
		// 仅上传，不删除
		if err := s.LogUploadService.UploadTable(tableName); err != nil {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrDatabase, i18n.Message(c, "log_backup.upload_failed")+": "+err.Error()))
			return
		}
	}

	// 发送飞书 Webhook 通知（手动上传也发送通知）
	go func() {
		webhookURL := settings.FeishuWebhookURL
		if webhookURL == "" {
			return
		}
		title := "📦 GPT-Load 日志手动备份通知"
		content := "**手动日志备份操作完成**\n\n"
		content += "- **日志表**: `" + tableName + "`\n"
		if deleted {
			content += "- **操作**: 上传并删除\n"
		} else {
			content += "- **操作**: 仅上传\n"
		}
		_ = utils.SendFeishuWebhook(webhookURL, title, content)
	}()

	// 根据实际操作返回不同的成功消息
	if deleted {
		response.SuccessI18n(c, "log_backup.upload_and_delete_success", nil)
	} else {
		response.SuccessI18n(c, "log_backup.upload_success", nil)
	}
}
