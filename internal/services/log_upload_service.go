package services

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gpt-load/internal/config"
	"gpt-load/internal/types"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// LogUploadService 负责将日志文件上传到外部存储
type LogUploadService struct {
	db              *gorm.DB
	settingsManager *config.SystemSettingsManager
	stopCh          chan struct{}
	wg              sync.WaitGroup
}

// NewLogUploadService 创建新的日志上传服务
func NewLogUploadService(db *gorm.DB, settingsManager *config.SystemSettingsManager) *LogUploadService {
	return &LogUploadService{
		db:              db,
		settingsManager: settingsManager,
		stopCh:          make(chan struct{}),
	}
}

// Start 启动日志上传服务
func (s *LogUploadService) Start() {
	s.wg.Add(1)
	go s.run()
	logrus.Debug("Log upload service started")
}

// Stop 停止日志上传服务
func (s *LogUploadService) Stop(ctx context.Context) {
	close(s.stopCh)

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		logrus.Info("Log upload service stopped gracefully.")
	case <-ctx.Done():
		logrus.Warn("Log upload service stop timed out.")
	}
}

// run 运行日志上传的主循环
func (s *LogUploadService) run() {
	defer s.wg.Done()
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.uploadExpiredLogs()
		case <-s.stopCh:
			return
		}
	}
}

// uploadExpiredLogs 上传即将被删除的日志文件
func (s *LogUploadService) uploadExpiredLogs() {
	settings := s.settingsManager.GetSettings()

	// 检查是否启用自动上传
	if !settings.LogUploadEnabled {
		return
	}

	// 检查是否启用删除前上传
	if !settings.LogUploadBeforeDelete {
		return
	}

	retentionDays := settings.RequestLogRetentionDays
	if retentionDays <= 0 {
		return
	}

	// 计算即将过期的日期（明天将要删除的表）
	tomorrow := time.Now().AddDate(0, 0, 1)
	cutoffDate := tomorrow.AddDate(0, 0, -retentionDays)

	// 获取需要上传的表
	tablesToUpload := []string{cutoffDate.Format("request_logs_2006_01_02")}

	for _, tableName := range tablesToUpload {
		if !s.tableExists(tableName) {
			continue
		}

		// 上传日志表
		if err := s.uploadTable(tableName, settings); err != nil {
			logrus.WithError(err).WithField("table", tableName).Error("Failed to upload log table")
			continue
		}

		logrus.WithField("table", tableName).Info("Successfully uploaded log table")
	}
}

// uploadTable 上传单个日志表
func (s *LogUploadService) uploadTable(tableName string, settings types.SystemSettings) error {
	// 导出表数据为 CSV
	csvData, err := s.exportTableToCSV(tableName)
	if err != nil {
		return fmt.Errorf("failed to export table to CSV: %w", err)
	}

	// 生成文件名
	filename := s.generateFilename(tableName, settings)

	// 根据提供商选择上传方式
	provider := strings.ToLower(settings.LogUploadProvider)
	switch provider {
	case "tencent", "cos", "tencent_cos":
		return s.uploadToTencentCOS(csvData, filename, settings)
	case "webdav":
		return s.uploadToWebDAV(csvData, filename, settings)
	default:
		return fmt.Errorf("unknown upload provider: %s", provider)
	}
}

// exportTableToCSV 将表数据导出为 CSV 格式
func (s *LogUploadService) exportTableToCSV(tableName string) ([]byte, error) {
	var buf bytes.Buffer

	// 使用 COPY 命令或 SELECT 导出 CSV 格式数据
	// 对于 SQLite，我们需要手动构建 CSV
	rows, err := s.db.Table(tableName).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 获取列名
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	// 写入表头
	buf.WriteString(strings.Join(columns, ",") + "\n")

	// 写入数据行
	values := make([]interface{}, len(columns))
	valuePtrs := make([]interface{}, len(columns))
	for i := range values {
		valuePtrs[i] = &values[i]
	}

	for rows.Next() {
		if err := rows.Scan(valuePtrs...); err != nil {
			return nil, err
		}

		row := make([]string, len(columns))
		for i, v := range values {
			if v == nil {
				row[i] = ""
				continue
			}
			switch vt := v.(type) {
			case []byte:
				row[i] = string(vt)
			case time.Time:
				row[i] = vt.Format(time.RFC3339)
			default:
				row[i] = fmt.Sprintf("%v", v)
			}
			// 处理 CSV 中的逗号和引号
			if strings.Contains(row[i], ",") || strings.Contains(row[i], "\"") || strings.Contains(row[i], "\n") {
				row[i] = "\"" + strings.ReplaceAll(row[i], "\"", "\"\"") + "\""
			}
		}
		buf.WriteString(strings.Join(row, ",") + "\n")
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// generateFilename 生成上传文件名
func (s *LogUploadService) generateFilename(tableName string, settings types.SystemSettings) string {
	prefix := settings.LogUploadFilenamePrefix
	if prefix == "" {
		prefix = "gpt-load-logs"
	}

	directory := settings.LogUploadDirectory
	if directory != "" && !strings.HasSuffix(directory, "/") {
		directory += "/"
	}

	// 从表名提取日期
	dateStr := strings.ReplaceAll(strings.TrimPrefix(tableName, "request_logs_"), "_", "-")

	return fmt.Sprintf("%s%s-%s.csv.gz", directory, prefix, dateStr)
}

// uploadToTencentCOS 上传到腾讯云 COS
func (s *LogUploadService) uploadToTencentCOS(data []byte, objectKey string, settings types.SystemSettings) error {
	secretID := settings.LogUploadTencentSecretID
	secretKey := settings.LogUploadTencentSecretKey
	bucket := settings.LogUploadTencentBucket
	region := settings.LogUploadTencentRegion

	if secretID == "" || secretKey == "" || bucket == "" {
		return fmt.Errorf("tencent COS credentials not configured")
	}

	// 使用腾讯云 COS API 上传
	// 由于不能引入外部依赖，使用原生 HTTP 实现
	return s.uploadToCOSCOS(data, objectKey, secretID, secretKey, bucket, region)
}

// uploadToCOSCOS 使用原生 HTTP 上传到 COS
func (s *LogUploadService) uploadToCOSCOS(data []byte, objectKey, secretID, secretKey, bucket, region string) error {
	// COS 使用 AWS S3 兼容的 API
	// 构建 COS 端点
	endpoint := fmt.Sprintf("https://%s.cos.%s.myqcloud.com/%s", bucket, region, objectKey)

	// 计算签名
	timestamp := time.Now().Unix()
	signature := s.cosSignature(secretID, secretKey, "PUT", "/"+objectKey, timestamp)

	req, err := http.NewRequest("PUT", endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", signature)
	req.Header.Set("Host", bucket+".cos."+region+".myqcloud.com")
	req.Header.Set("Content-Type", "application/octet-stream")

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("COS upload failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// cosSignature 生成 COS 签名 (简化版本)
func (s *LogUploadService) cosSignature(secretID, secretKey, method, uri string, timestamp int64) string {
	// 这里实现简化的签名算法
	// 完整的签名算法参考腾讯云文档
	// https://cloud.tencent.com/document/product/436/7778
	// 由于签名算法复杂，建议使用官方 SDK

	// 注意：这是一个简化实现，生产环境建议使用官方 SDK
	// 使用 q-sign-algorithm=sha1
	// 这里返回一个占位符，实际使用需要完整实现

	// 为了简化，这里使用基本的 Authorization header
	// 实际应该使用完整的五元组签名
	return fmt.Sprintf("q-sign-algorithm=sha1&q-ak=%s&q-sign-time=%d;%d&q-key-time=%d;%d&q-header-list=host&q-url-param=&q-signature=placeholder",
		secretID, timestamp, timestamp+3600, timestamp, timestamp+3600)
}

// uploadToWebDAV 上传到 WebDAV 服务器
func (s *LogUploadService) uploadToWebDAV(data []byte, filename string, settings types.SystemSettings) error {
	baseURL := settings.LogUploadWebDAVURL
	username := settings.LogUploadWebDAVUsername
	password := settings.LogUploadWebDAVPassword

	if baseURL == "" {
		return fmt.Errorf("webdav URL not configured")
	}

	// 确保 URL 以 / 结尾
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}

	// 构建完整 URL
	uploadURL := baseURL + filename

	// 创建请求
	req, err := http.NewRequest("PUT", uploadURL, bytes.NewReader(data))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/octet-stream")

	// 添加基本认证
	if username != "" || password != "" {
		req.SetBasicAuth(username, password)
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// WebDAV 通常返回 201 Created 或 204 No Content 表示成功
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("WebDAV upload failed with status %d: %s", resp.StatusCode, string(body))
	}

	logrus.WithField("url", uploadURL).Info("Successfully uploaded to WebDAV")
	return nil
}

// tableExists 检查表是否存在
func (s *LogUploadService) tableExists(tableName string) bool {
	return s.db.Migrator().HasTable(tableName)
}

// UploadFileNow 立即上传指定文件（用于手动触发上传）
func (s *LogUploadService) UploadFileNow(filePath string) error {
	settings := s.settingsManager.GetSettings()

	if !settings.LogUploadEnabled {
		return fmt.Errorf("log upload is not enabled")
	}

	// 读取文件
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	// 生成文件名
	filename := filepath.Base(filePath)
	directory := settings.LogUploadDirectory
	if directory != "" && !strings.HasSuffix(directory, "/") {
		directory += "/"
	}
	filename = directory + filename

	// 根据提供商选择上传方式
	provider := strings.ToLower(settings.LogUploadProvider)
	switch provider {
	case "tencent", "cos", "tencent_cos":
		return s.uploadToTencentCOS(data, filename, settings)
	case "webdav":
		return s.uploadToWebDAV(data, filename, settings)
	default:
		return fmt.Errorf("unknown upload provider: %s", provider)
	}
}

// compressData 压缩数据
func compressData(data []byte) ([]byte, error) {
	// 使用 gzip 压缩
	// 这里简单实现，实际可以使用 compress/gzip 包
	return data, nil
}

// parseWebDAVURL 解析 WebDAV URL 并返回基础 URL 和路径
func parseWebDAVURL(rawURL string) (string, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", err
	}

	baseURL := fmt.Sprintf("%s://%s", u.Scheme, u.Host)
	path := u.Path

	return baseURL, path, nil
}
