package handler

import (
	"fmt"
	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/i18n"
	"gpt-load/internal/models"
	"gpt-load/internal/response"
	"log"
	"time"

	"github.com/gin-gonic/gin"
)

// LogResponse 定义 API 响应中日志条目的结构
type LogResponse struct {
	models.RequestLog
}

// GetLogs 处理带过滤和分页的请求日志获取。
func (s *Server) GetLogs(c *gin.Context) {
	query := s.LogService.GetLogsQuery(c)

	var logs []models.RequestLog
	query = query.Order("timestamp desc")

	// 默认跳过 COUNT 查询，避免跨多表时 COUNT(*) 开销大
	// 前端不传时间范围时走无限滚动模式
	enableCount := c.Query("enable_count") == "true"
	pagination, err := response.Paginate(c, query, &logs, enableCount)
	if err != nil {
		response.Error(c, app_errors.ParseDBError(err))
		return
	}

	// 批量解密所有日志中的密钥，比逐条解密效率更高
	// 将切片转换为指针切片
	logPtrs := make([]*models.RequestLog, len(logs))
	for i := range logs {
		logPtrs[i] = &logs[i]
	}
	s.LogService.BatchDecryptLogs(logPtrs)

	pagination.Items = logs
	response.Success(c, pagination)
}

// ExportLogs 处理将过滤后的日志密钥导出为 CSV 文件。
func (s *Server) ExportLogs(c *gin.Context) {
	filename := fmt.Sprintf("log_keys_export_%s.csv", time.Now().Format("20060102150405"))
	c.Header("Content-Disposition", "attachment; filename="+filename)
	c.Header("Content-Type", "text/csv; charset=utf-8")

	// 流式响应
	err := s.LogService.StreamLogKeysToCSV(c, c.Writer)
	if err != nil {
		log.Printf("Failed to stream log keys to CSV: %v", err)
		c.JSON(500, gin.H{"error": i18n.Message(c, "error.export_logs")})
		return
	}
}
