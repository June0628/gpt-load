package services

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"gpt-load/internal/config"
	"gpt-load/internal/types"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// LogUploadService 负责将日志表数据上传到外部存储
type LogUploadService struct {
	db              *gorm.DB
	settingsManager *config.SystemSettingsManager
	mu              sync.Mutex // 防止并发上传/删除同一张表
}

// NewLogUploadService 创建新的日志上传服务
func NewLogUploadService(db *gorm.DB, settingsManager *config.SystemSettingsManager) *LogUploadService {
	return &LogUploadService{
		db:              db,
		settingsManager: settingsManager,
	}
}

// Start 启动日志上传服务（上传逻辑由 LogCleanupService 统一调度，此处保留接口兼容性）
func (s *LogUploadService) Start() {
	logrus.Debug("Log upload service started (upload is coordinated by cleanup service)")
}

// Stop 停止日志上传服务
func (s *LogUploadService) Stop(ctx context.Context) {
	logrus.Info("Log upload service stopped.")
}

// UploadTable 将指定日志表导出为 CSV 并上传到外部存储
// 由 LogCleanupService 在删除表之前调用
func (s *LogUploadService) UploadTable(tableName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.uploadTableLocked(tableName)
}

// UploadAndDeleteTable 在同一个锁内执行上传 + 删除操作，防止并发竞态
// 用于手动上传后自动删除的场景
func (s *LogUploadService) UploadAndDeleteTable(tableName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 上传
	if err := s.uploadTableLocked(tableName); err != nil {
		return err
	}

	// 删除表
	dialect := s.db.Dialector.Name()
	var dropSQL string
	if dialect == "mysql" {
		dropSQL = fmt.Sprintf("DROP TABLE IF EXISTS %s", tableName)
	} else {
		dropSQL = fmt.Sprintf("DROP TABLE IF EXISTS \"%s\"", tableName)
	}
	if err := s.db.Exec(dropSQL).Error; err != nil {
		logrus.WithError(err).WithField("table", tableName).Error("Failed to drop table after upload")
		return fmt.Errorf("upload succeeded but failed to delete table: %w", err)
	}

	logrus.WithField("table", tableName).Info("Successfully uploaded and deleted log table")
	return nil
}

// uploadTableLocked 内部实现，调用者需持有 s.mu 锁
func (s *LogUploadService) uploadTableLocked(tableName string) error {
	settings := s.settingsManager.GetSettings()

	if !settings.LogUploadEnabled {
		return fmt.Errorf("log upload is not enabled")
	}

	// 导出表数据为 CSV 临时文件（流式写入，避免大表 OOM）
	tmpFile, rowCount, err := s.exportTableToCSVFile(tableName)
	if err != nil {
		return fmt.Errorf("failed to export table to CSV: %w", err)
	}
	// 确保临时文件在上传完成后被清理
	defer os.Remove(tmpFile)

	if rowCount == 0 {
		logrus.WithField("table", tableName).Info("Table is empty, skipping upload")
		return nil
	}

	// 生成文件名
	filename := s.generateFilename(tableName, settings)

	// 根据提供商选择上传方式
	provider := strings.ToLower(settings.LogUploadProvider)
	switch provider {
	case "tencent", "cos", "tencent_cos":
		return s.uploadFileToTencentCOS(tmpFile, filename, settings)
	case "webdav":
		return s.uploadFileToWebDAV(tmpFile, filename, settings)
	default:
		return fmt.Errorf("unknown upload provider: %s", provider)
	}
}

// exportTableToCSVFile 将表数据流式导出为 CSV 临时文件，返回临时文件路径和行数
// 使用临时文件而非内存缓冲，避免大表导致 OOM
func (s *LogUploadService) exportTableToCSVFile(tableName string) (string, int, error) {
	rows, err := s.db.Table(tableName).Rows()
	if err != nil {
		return "", 0, fmt.Errorf("failed to query table %s: %w", tableName, err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return "", 0, fmt.Errorf("failed to get columns for table %s: %w", tableName, err)
	}

	// 创建临时文件
	tmpFile, err := os.CreateTemp("", "gpt-load-csv-*.csv")
	if err != nil {
		return "", 0, fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	// 使用 CSV writer 直接写入临时文件
	writer := csv.NewWriter(tmpFile)

	if err := writer.Write(columns); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return "", 0, fmt.Errorf("failed to write CSV header: %w", err)
	}

	values := make([]interface{}, len(columns))
	valuePtrs := make([]interface{}, len(columns))
	for i := range values {
		valuePtrs[i] = &values[i]
	}

	rowCount := 0
	for rows.Next() {
		if err := rows.Scan(valuePtrs...); err != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			return "", 0, fmt.Errorf("failed to scan row: %w", err)
		}

		record := make([]string, len(columns))
		for i, val := range values {
			if val == nil {
				record[i] = ""
			} else {
				switch v := val.(type) {
				case []byte:
					record[i] = string(v)
				case time.Time:
					record[i] = v.Format(time.RFC3339)
				default:
					record[i] = fmt.Sprintf("%v", v)
				}
			}
		}

		if err := writer.Write(record); err != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			return "", 0, fmt.Errorf("failed to write CSV row: %w", err)
		}
		rowCount++
	}

	if err := rows.Err(); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return "", 0, fmt.Errorf("rows iteration error: %w", err)
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return "", 0, fmt.Errorf("CSV writer flush error: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return "", 0, fmt.Errorf("failed to close temp file: %w", err)
	}

	logrus.WithFields(logrus.Fields{
		"table":     tableName,
		"row_count": rowCount,
		"tmp_file":  tmpPath,
	}).Debug("Exported table to CSV temp file")

	return tmpPath, rowCount, nil
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

	// 从表名提取日期部分，例如 request_logs_20260418 -> 2026-04-18
	dateStr := strings.TrimPrefix(tableName, "request_logs_")
	if len(dateStr) == 8 {
		dateStr = dateStr[:4] + "-" + dateStr[4:6] + "-" + dateStr[6:8]
	}

	return fmt.Sprintf("%s%s-%s.csv", directory, prefix, dateStr)
}

// ============================================================
// 腾讯云 COS 上传（使用正确的 HMAC-SHA1 签名算法）
// ============================================================

// uploadFileToTencentCOS 从文件流式上传到腾讯云 COS
func (s *LogUploadService) uploadFileToTencentCOS(filePath, objectKey string, settings types.SystemSettings) error {
	secretID := settings.LogUploadTencentSecretID
	secretKey := settings.LogUploadTencentSecretKey
	bucket := settings.LogUploadTencentBucket
	region := settings.LogUploadTencentRegion

	if secretID == "" || secretKey == "" || bucket == "" {
		return fmt.Errorf("tencent COS credentials not configured")
	}

	// 打开文件获取大小和 reader
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file for COS upload: %w", err)
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat file for COS upload: %w", err)
	}

	host := fmt.Sprintf("%s.cos.%s.myqcloud.com", bucket, region)

	// 对 objectKey 的每段路径进行 URL 编码（保留 '/' 分隔符）
	encodedKey := encodeObjectKey(objectKey)
	endpoint := fmt.Sprintf("https://%s/%s", host, encodedKey)

	req, err := http.NewRequest("PUT", endpoint, file)
	if err != nil {
		return fmt.Errorf("failed to create COS upload request: %w", err)
	}

	req.ContentLength = fileInfo.Size()
	req.Header.Set("Host", host)
	req.Header.Set("Content-Type", "text/csv")

	// 签名中使用编码后的路径，与实际请求 URI 保持一致
	authorization := s.cosAuthorization(secretID, secretKey, "put", "/"+encodedKey, host)
	req.Header.Set("Authorization", authorization)

	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("COS upload request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("COS upload failed with status %d: %s", resp.StatusCode, string(body))
	}

	logrus.WithField("object_key", objectKey).Info("Successfully uploaded to Tencent COS")
	return nil
}

// cosAuthorization 生成腾讯云 COS v5 签名
// 参考文档：https://cloud.tencent.com/document/product/436/7778
func (s *LogUploadService) cosAuthorization(secretID, secretKey, method, uri, host string) string {
	now := time.Now()
	startTime := now.Unix()
	endTime := now.Add(1 * time.Hour).Unix()
	keyTime := fmt.Sprintf("%d;%d", startTime, endTime)

	// 1. 生成 SignKey
	signKey := hmacSHA1(secretKey, keyTime)

	// 2. 生成 HttpString
	// 格式：{method}\n{uri}\n{params}\n{headers}\n
	headerList := "host"
	headerStr := fmt.Sprintf("host=%s", strings.ToLower(host))
	httpString := fmt.Sprintf("%s\n%s\n\n%s\n", strings.ToLower(method), uri, headerStr)

	// 3. 生成 StringToSign
	// 格式：sha1\n{key_time}\n{sha1(HttpString)}\n
	httpStringSHA1 := sha1Hex(httpString)
	stringToSign := fmt.Sprintf("sha1\n%s\n%s\n", keyTime, httpStringSHA1)

	// 4. 生成 Signature
	signature := hmacSHA1(signKey, stringToSign)

	// 5. 拼接 Authorization
	return fmt.Sprintf(
		"q-sign-algorithm=sha1&q-ak=%s&q-sign-time=%s&q-key-time=%s&q-header-list=%s&q-url-param-list=&q-signature=%s",
		secretID, keyTime, keyTime, headerList, signature,
	)
}

// hmacSHA1 计算 HMAC-SHA1 并返回十六进制字符串
func hmacSHA1(key, data string) string {
	mac := hmac.New(sha1.New, []byte(key))
	mac.Write([]byte(data))
	return fmt.Sprintf("%x", mac.Sum(nil))
}

// sha1Hex 计算 SHA1 并返回十六进制字符串
func sha1Hex(data string) string {
	h := sha1.New()
	h.Write([]byte(data))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// ============================================================
// WebDAV 上传
// ============================================================

// uploadFileToWebDAV 从文件流式上传到 WebDAV 服务器
func (s *LogUploadService) uploadFileToWebDAV(filePath, filename string, settings types.SystemSettings) error {
	baseURL := settings.LogUploadWebDAVURL
	username := settings.LogUploadWebDAVUsername
	password := settings.LogUploadWebDAVPassword

	if baseURL == "" {
		return fmt.Errorf("webdav URL not configured")
	}

	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}

	client := &http.Client{Timeout: 30 * time.Minute}

	// 确保中间目录存在（使用 MKCOL 逐级创建）
	dir := dirFromPath(filename)
	if dir != "" && dir != "." {
		if err := s.webdavMkcolRecursive(client, baseURL, dir, username, password); err != nil {
			return fmt.Errorf("failed to create WebDAV directories: %w", err)
		}
	}

	// 打开文件
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file for WebDAV upload: %w", err)
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat file for WebDAV upload: %w", err)
	}

	uploadURL := baseURL + filename

	req, err := http.NewRequest("PUT", uploadURL, file)
	if err != nil {
		return fmt.Errorf("failed to create WebDAV upload request: %w", err)
	}

	req.ContentLength = fileInfo.Size()
	req.Header.Set("Content-Type", "text/csv")

	if username != "" || password != "" {
		req.SetBasicAuth(username, password)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("WebDAV upload request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("WebDAV upload failed with status %d: %s", resp.StatusCode, string(body))
	}

	logrus.WithField("url", uploadURL).Info("Successfully uploaded to WebDAV")
	return nil
}

// webdavMkcolRecursive 递归创建 WebDAV 目录
// 逐级创建路径中的每个目录，忽略已存在的目录（405 Method Not Allowed 表示已存在）
func (s *LogUploadService) webdavMkcolRecursive(client *http.Client, baseURL, dirPath, username, password string) error {
	parts := strings.Split(dirPath, "/")
	current := ""
	for _, part := range parts {
		if part == "" {
			continue
		}
		if current == "" {
			current = part
		} else {
			current = current + "/" + part
		}

		mkcolURL := baseURL + current + "/"
		req, err := http.NewRequest("MKCOL", mkcolURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create MKCOL request for %s: %w", current, err)
		}
		if username != "" || password != "" {
			req.SetBasicAuth(username, password)
		}

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("MKCOL request failed for %s: %w", current, err)
		}
		resp.Body.Close()

		// 201 Created = 成功创建，405 Method Not Allowed = 目录已存在，两者都是正常情况
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusMethodNotAllowed {
			return fmt.Errorf("MKCOL failed for %s with status %d", current, resp.StatusCode)
		}
	}
	return nil
}

// encodeObjectKey 对 objectKey 的每段路径进行 URL 编码，保留 '/' 分隔符
// 例如 "backup/gpt-load-logs-2026-04-18.csv" -> "backup/gpt-load-logs-2026-04-18.csv"
// 例如 "备份目录/日志 2026.csv" -> "%E5%A4%87%E4%BB%BD%E7%9B%AE%E5%BD%95/%E6%97%A5%E5%BF%97%202026.csv"
func encodeObjectKey(objectKey string) string {
	parts := strings.Split(objectKey, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// dirFromPath 从文件路径中提取目录部分（纯字符串操作，不依赖 filepath）
func dirFromPath(path string) string {
	// 将反斜杠统一为正斜杠
	path = strings.ReplaceAll(path, "\\", "/")
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return ""
	}
	return path[:idx]
}

// ============================================================
// 工具方法
// ============================================================

// UploadFileNow 立即上传指定文件（用于手动触发上传）
func (s *LogUploadService) UploadFileNow(filePath string) error {
	settings := s.settingsManager.GetSettings()

	if !settings.LogUploadEnabled {
		return fmt.Errorf("log upload is not enabled")
	}

	// 从文件路径提取文件名
	idx := strings.LastIndex(filePath, "/")
	var baseName string
	if idx >= 0 {
		baseName = filePath[idx+1:]
	} else {
		baseName = filePath
	}

	directory := settings.LogUploadDirectory
	if directory != "" && !strings.HasSuffix(directory, "/") {
		directory += "/"
	}
	filename := directory + baseName

	provider := strings.ToLower(settings.LogUploadProvider)
	switch provider {
	case "tencent", "cos", "tencent_cos":
		return s.uploadFileToTencentCOS(filePath, filename, settings)
	case "webdav":
		return s.uploadFileToWebDAV(filePath, filename, settings)
	default:
		return fmt.Errorf("unknown upload provider: %s", provider)
	}
}
