package services

import (
	"compress/gzip"
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
	"gpt-load/internal/utils"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// ErrEmptyTableSkipped 表示表为空，上传被跳过（不应被视为上传成功，删除路径需据此跳过删除）
var ErrEmptyTableSkipped = fmt.Errorf("table is empty, upload skipped")

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
	logrus.Debug("日志上传服务已启动（上传由清理服务协调）")
}

// Stop 停止日志上传服务
func (s *LogUploadService) Stop(ctx context.Context) {
	logrus.Info("日志上传服务已停止")
}

// UploadTable 将指定日志表导出为 CSV 并流式上传到外部存储
// 由 LogCleanupService 在删除表之前调用
func (s *LogUploadService) UploadTable(tableName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.uploadTableLocked(tableName)
}

// UploadAndDeleteTable 在同一个锁内执行上传 + 删除操作，防止并发竞态
// 用于手动上传后自动删除的场景
// 仅当上传真正成功时才删除表，失败不删除以避免数据丢失。
func (s *LogUploadService) UploadAndDeleteTable(tableName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 上传
	if err := s.uploadTableLocked(tableName); err != nil {
		// 空表跳过：不视为错误，但也不删除表（交由调用方决定）
		if err == ErrEmptyTableSkipped {
			logrus.WithField("table", tableName).Info("表为空，跳过上传且不删除表")
			return err
		}
		return err
	}

	// 删除表
	dialect := s.db.Dialector.Name()
	var dropSQL string
	if dialect == "mysql" {
		dropSQL = fmt.Sprintf("DROP TABLE IF EXISTS `%s`", tableName)
	} else {
		dropSQL = fmt.Sprintf("DROP TABLE IF EXISTS \"%s\"", tableName)
	}
	if err := s.db.Exec(dropSQL).Error; err != nil {
		logrus.WithError(err).WithField("table", tableName).Error("上传后删除表失败")
		return fmt.Errorf("上传成功但删除表失败: %w", err)
	}

	logrus.WithField("table", tableName).Info("日志表上传并删除成功")
	return nil
}

// uploadTableLocked 内部实现，调用者需持有 s.mu 锁
// 使用流式上传，不再生成临时文件
func (s *LogUploadService) uploadTableLocked(tableName string) error {
	// 防御性校验：只允许 request_logs_YYYYMMDD 格式的表名，避免 SQL 注入
	if !utils.ValidateLogTableName(tableName) {
		return fmt.Errorf("invalid log table name: %s", tableName)
	}

	settings := s.settingsManager.GetSettings()

	if !settings.LogUploadEnabled {
		return fmt.Errorf("日志上传未启用")
	}

	// 先检查表是否有数据
	var count int64
	if err := s.db.Table(tableName).Count(&count).Error; err != nil {
		return fmt.Errorf("统计表 %s 失败: %w", tableName, err)
	}

	if count == 0 {
		// 空表返回哨兵错误，避免被误当作上传成功
		logrus.WithField("table", tableName).Info("表为空，跳过上传")
		return ErrEmptyTableSkipped
	}

	// 生成文件名
	filename := s.generateFilename(tableName, settings)

	// 根据提供商选择上传方式（流式上传）
	provider := strings.ToLower(settings.LogUploadProvider)
	switch provider {
	case "tencent", "cos", "tencent_cos":
		return s.uploadTableToTencentCOSStream(tableName, filename, settings)
	case "webdav":
		return s.uploadTableToWebDAVStream(tableName, filename, settings)
	default:
		return fmt.Errorf("未知上传提供商: %s", provider)
	}
}

// exportTableToCSVStream 将表数据流式导出为 CSV，通过回调函数逐行处理
// 避免将整个 CSV 保存在内存或临时文件中
// 包含所有字段（包括 request_body 和 agent_files）
func (s *LogUploadService) exportTableToCSVStream(tableName string, onRow func([]string) error) (int, error) {
	rows, err := s.db.Table(tableName).Rows()
	if err != nil {
		return 0, fmt.Errorf("查询表 %s 失败: %w", tableName, err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return 0, fmt.Errorf("获取表 %s 列信息失败: %w", tableName, err)
	}

	// 写入表头（包含所有字段）
	if err := onRow(columns); err != nil {
		return 0, fmt.Errorf("写入 CSV 表头失败: %w", err)
	}

	values := make([]interface{}, len(columns))
	valuePtrs := make([]interface{}, len(columns))
	for i := range values {
		valuePtrs[i] = &values[i]
	}

	rowCount := 0
	for rows.Next() {
		if err := rows.Scan(valuePtrs...); err != nil {
			return 0, fmt.Errorf("扫描行失败: %w", err)
		}

		record := make([]string, len(columns))
		for i, val := range values {
			if val == nil {
				record[i] = ""
				continue
			}
			switch v := val.(type) {
			case []byte:
				record[i] = string(v)
			case time.Time:
				record[i] = v.Format(time.RFC3339)
			default:
				record[i] = fmt.Sprintf("%v", v)
			}
			// 防止 CSV 公式注入：以 = + - @ 开头的单元格会被 Excel/Sheets 解释为公式
			record[i] = sanitizeCSVCell(record[i])
		}

		if err := onRow(record); err != nil {
			return 0, fmt.Errorf("写入 CSV 行失败: %w", err)
		}
		rowCount++
	}

	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("行迭代错误: %w", err)
	}

	logrus.WithFields(logrus.Fields{
		"table":     tableName,
		"row_count": rowCount,
		"col_count": len(columns),
	}).Debug("表数据流式导出为 CSV（包含所有字段包括大字段）")

	return rowCount, nil
}

// exportTableToCSVFile 将表数据流式导出为 CSV 临时文件，返回临时文件路径和行数
func (s *LogUploadService) exportTableToCSVFile(tableName string) (string, int, error) {
	// 创建临时文件
	tmpFile, err := os.CreateTemp("", "gpt-load-csv-*.csv")
	if err != nil {
		return "", 0, fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpPath := tmpFile.Name()

	writer := csv.NewWriter(tmpFile)
	rowCount := 0

	// 使用流式导出
	rowCount, err = s.exportTableToCSVStream(tableName, func(row []string) error {
		return writer.Write(row)
	})
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return "", 0, err
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return "", 0, fmt.Errorf("CSV 写入器刷新错误: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return "", 0, fmt.Errorf("关闭临时文件失败: %w", err)
	}

	logrus.WithFields(logrus.Fields{
		"table":     tableName,
		"row_count": rowCount,
		"tmp_file":  tmpPath,
	}).Debug("表数据导出到 CSV 临时文件")

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

// uploadTableToTencentCOSStream 流式上传表数据到腾讯云 COS，不生成临时文件
// 如果流式上传失败，会自动降级到基于临时文件的上传
func (s *LogUploadService) uploadTableToTencentCOSStream(tableName, objectKey string, settings types.SystemSettings) error {
	secretID := settings.LogUploadTencentSecretID
	secretKey := settings.LogUploadTencentSecretKey
	bucket := settings.LogUploadTencentBucket
	region := settings.LogUploadTencentRegion

	if secretID == "" || secretKey == "" || bucket == "" {
		return fmt.Errorf("腾讯云 COS 凭证未配置")
	}

	// 创建 pipe 用于流式传输
	reader, writer, err := os.Pipe()
	if err != nil {
		// pipe 创建失败，直接使用临时文件方式
		logrus.WithError(err).Warn("创建 pipe 失败，回退到临时文件上传")
		return s.uploadTableToTencentCOSWithTempFile(tableName, objectKey, settings)
	}
	defer reader.Close()

	host := fmt.Sprintf("%s.cos.%s.myqcloud.com", bucket, region)
	encodedKey := encodeObjectKey(objectKey)
	endpoint := fmt.Sprintf("https://%s/%s", host, encodedKey)

	// 创建上传请求，使用 pipe reader 作为请求体
	req, err := http.NewRequest("PUT", endpoint, reader)
	if err != nil {
		writer.Close()
		return fmt.Errorf("创建 COS 上传请求失败: %w", err)
	}

	// 注意：COS v5 单次签名要求请求具有确定的 Content-Length。
	// 此前使用 Transfer-Encoding: chunked 会与单次签名不匹配，导致 COS 拒绝请求。
	// 因此这里移除了显式的 chunked 设置，由 net/http 自动处理分块传输。
	req.Header.Set("Host", host)
	req.Header.Set("Content-Type", "text/csv")

	// 签名中使用编码后的路径
	authorization := s.cosAuthorization(secretID, secretKey, "put", "/"+encodedKey, host)
	req.Header.Set("Authorization", authorization)

	// 使用独立变量捕获写入错误，避免与外层 err 竞态
	writeDone := make(chan error, 1)
	go func() {
		defer writer.Close()

		csvWriter := csv.NewWriter(writer)
		rowCount := 0

		// 流式导出到 pipe
		_, writeErr := s.exportTableToCSVStream(tableName, func(row []string) error {
			if err := csvWriter.Write(row); err != nil {
				return err
			}
			rowCount++

			// 每 10000 行刷新一次缓冲区
			if rowCount%10000 == 0 {
				csvWriter.Flush()
				if err := csvWriter.Error(); err != nil {
					return err
				}
			}
			return nil
		})

		if writeErr != nil {
			writeDone <- fmt.Errorf("导出表数据为 CSV 失败: %w", writeErr)
			return
		}

		csvWriter.Flush()
		if err := csvWriter.Error(); err != nil {
			writeDone <- fmt.Errorf("CSV 写入器刷新错误: %w", err)
			return
		}

		logrus.WithFields(logrus.Fields{
			"table":      tableName,
			"row_count":  rowCount,
			"object_key": objectKey,
		}).Info("CSV 数据流式传输到 COS 完成")
		writeDone <- nil
	}()

	// 执行上传请求
	client := &http.Client{Timeout: 60 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		// client.Do 失败时 reader 端无人读取，writer goroutine 会阻塞在 writer.Write。
		// 关闭 reader 触发 writer 写错误使其退出，避免 <-writeDone 永久阻塞（死锁）。
		reader.Close()
		<-writeDone // 等待 writer goroutine 结束
		logrus.WithError(err).Warn("流式上传失败，回退到临时文件上传")
		return s.uploadTableToTencentCOSWithTempFile(tableName, objectKey, settings)
	}
	defer resp.Body.Close()

	// 等待写入完成
	writeErr := <-writeDone
	if writeErr != nil {
		// 写入失败，也尝试降级
		logrus.WithError(writeErr).Warn("CSV 导出失败，回退到临时文件上传")
		return s.uploadTableToTencentCOSWithTempFile(tableName, objectKey, settings)
	}

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		errMsg := fmt.Errorf("COS 上传失败，状态码 %d: %s", resp.StatusCode, string(body))

		// 4xx 客户端错误（如 401/403 签名或权限错误）使用的是同一套凭证和签名算法，
		// 回退到临时文件上传必然再失败一次，白白浪费流量和时间，因此直接返回错误。
		// 仅对 5xx 服务端错误或网络错误回退到临时文件重试。
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			logrus.WithError(errMsg).Warn("COS 流式上传返回 4xx，不回退到临时文件（凭证/签名错误）")
			return errMsg
		}
		logrus.WithError(errMsg).Warn("COS 上传失败，回退到临时文件上传")
		return s.uploadTableToTencentCOSWithTempFile(tableName, objectKey, settings)
	}

	logrus.WithField("object_key", objectKey).Info("腾讯云 COS 上传成功（流式）")
	return nil
}

// uploadTableToTencentCOSWithTempFile 使用临时文件方式上传到腾讯云 COS（后备方案）
func (s *LogUploadService) uploadTableToTencentCOSWithTempFile(tableName, objectKey string, settings types.SystemSettings) error {
	logrus.WithField("table", tableName).Info("使用临时文件回退上传到 COS（大表可能耗时较长）")

	// 生成临时 CSV 文件
	tmpFile, rowCount, err := s.exportTableToCSVFile(tableName)
	if err != nil {
		return fmt.Errorf("导出表数据为 CSV 失败: %w", err)
	}
	defer os.Remove(tmpFile)

	if rowCount == 0 {
		logrus.WithField("table", tableName).Info("表为空，跳过上传")
		return ErrEmptyTableSkipped
	}

	logrus.WithFields(logrus.Fields{
		"table":     tableName,
		"row_count": rowCount,
		"tmp_file":  tmpFile,
	}).Info("已导出到临时文件，开始上传")

	// 上传到 COS
	if err := s.uploadFileToTencentCOS(tmpFile, objectKey, settings); err != nil {
		return fmt.Errorf("临时文件上传也失败: %w", err)
	}

	return nil
}

// uploadFileToTencentCOS 从文件流式上传到腾讯云 COS
func (s *LogUploadService) uploadFileToTencentCOS(filePath, objectKey string, settings types.SystemSettings) error {
	secretID := settings.LogUploadTencentSecretID
	secretKey := settings.LogUploadTencentSecretKey
	bucket := settings.LogUploadTencentBucket
	region := settings.LogUploadTencentRegion

	if secretID == "" || secretKey == "" || bucket == "" {
		return fmt.Errorf("腾讯云 COS 凭证未配置")
	}

	// 打开文件获取大小和 reader
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("打开 COS 上传文件失败: %w", err)
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("获取 COS 上传文件信息失败: %w", err)
	}

	host := fmt.Sprintf("%s.cos.%s.myqcloud.com", bucket, region)

	// 对 objectKey 的每段路径进行 URL 编码（保留 '/' 分隔符）
	encodedKey := encodeObjectKey(objectKey)
	endpoint := fmt.Sprintf("https://%s/%s", host, encodedKey)

	req, err := http.NewRequest("PUT", endpoint, file)
	if err != nil {
		return fmt.Errorf("创建 COS 上传请求失败: %w", err)
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
		return fmt.Errorf("COS 上传请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("COS 上传失败，状态码 %d: %s", resp.StatusCode, string(body))
	}

	logrus.WithField("object_key", objectKey).Info("腾讯云 COS 上传成功")
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

// uploadTableToWebDAVStream 流式上传表数据到 WebDAV 服务器，不生成临时文件
// 如果流式上传失败，会自动降级到基于临时文件的上传
func (s *LogUploadService) uploadTableToWebDAVStream(tableName, filename string, settings types.SystemSettings) error {
	baseURL := settings.LogUploadWebDAVURL

	if baseURL == "" {
		return fmt.Errorf("WebDAV URL 未配置")
	}

	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}

	// 对于大表，直接导出到 gzip 压缩的临时文件，然后上传
	return s.uploadTableToWebDAVWithGzipTempFile(tableName, filename, settings)
}

// uploadTableToWebDAVWithGzipTempFile 导出表数据为 CSV 并压缩成 gzip，然后上传
// 适用于大表上传，可显著减少上传时间和流量
// 上传后通过 HEAD 请求验证文件存在且大小匹配
func (s *LogUploadService) uploadTableToWebDAVWithGzipTempFile(tableName, filename string, settings types.SystemSettings) error {
	logrus.WithField("table", tableName).Info("使用 gzip 压缩临时文件上传到 WebDAV")

	// 生成临时 CSV 文件
	tmpCSV, rowCount, err := s.exportTableToCSVFile(tableName)
	if err != nil {
		return fmt.Errorf("导出表数据为 CSV 失败: %w", err)
	}
	defer os.Remove(tmpCSV)

	if rowCount == 0 {
		logrus.WithField("table", tableName).Info("表为空，跳过上传")
		return ErrEmptyTableSkipped
	}

	// 压缩为 gzip
	gzipFile := tmpCSV + ".gz"
	if err := s.compressGzip(tmpCSV, gzipFile); err != nil {
		return fmt.Errorf("gzip 压缩失败: %w", err)
	}
	defer os.Remove(gzipFile)

	// 获取文件大小用于日志和校验
	csvInfo, _ := os.Stat(tmpCSV)
	gzInfo, err := os.Stat(gzipFile)
	if err != nil {
		return fmt.Errorf("获取 gzip 文件信息失败: %w", err)
	}
	if csvInfo != nil && gzInfo != nil {
		compressionRatio := float64(gzInfo.Size()) / float64(csvInfo.Size()) * 100
		logrus.WithFields(logrus.Fields{
			"table":             tableName,
			"row_count":         rowCount,
			"csv_size_mb":       fmt.Sprintf("%.2f", float64(csvInfo.Size())/1024/1024),
			"gzip_size_mb":      fmt.Sprintf("%.2f", float64(gzInfo.Size())/1024/1024),
			"compression_ratio": fmt.Sprintf("%.1f%%", compressionRatio),
		}).Info("CSV 已压缩为 gzip")
	}

	// 修改文件名为 .csv.gz
	gzipFilename := filename + ".gz"

	// 上传到 WebDAV
	expectedSize := gzInfo.Size()
	if err := s.uploadFileToWebDAV(gzipFile, gzipFilename, settings); err != nil {
		return fmt.Errorf("gzip 文件上传失败: %w", err)
	}

	// 事后验证：通过 HEAD 请求确认文件已写入且大小匹配
	if err := s.verifyWebDAVUpload(gzipFilename, expectedSize, settings); err != nil {
		return fmt.Errorf("WebDAV 上传事后验证失败，不删除源数据: %w", err)
	}

	logrus.WithFields(logrus.Fields{
		"table":         tableName,
		"filename":      gzipFilename,
		"expected_size": expectedSize,
	}).Info("WebDAV 上传成功并通过事后验证")
	return nil
}

// verifyWebDAVUpload 通过 HEAD 请求验证上传的文件存在且大小匹配
// verifyWebDAVUpload 通过 HEAD 请求验证文件存在且大小匹配
func (s *LogUploadService) verifyWebDAVUpload(filename string, expectedSize int64, settings types.SystemSettings) error {
	baseURL := settings.LogUploadWebDAVURL
	username := settings.LogUploadWebDAVUsername
	password := settings.LogUploadWebDAVPassword

	if baseURL == "" {
		return fmt.Errorf("WebDAV URL 未配置")
	}
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}

	filename = strings.TrimPrefix(filename, "/")
	verifyURL := baseURL + filename

	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("HEAD", verifyURL, nil)
	if err != nil {
		return fmt.Errorf("创建 WebDAV 验证请求失败: %w", err)
	}
	if username != "" || password != "" {
		req.SetBasicAuth(username, password)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("WebDAV 验证请求失败: %w", err)
	}
	defer resp.Body.Close()

	// HEAD 请求成功状态码：200 或 207（PROPFIND 风格）
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("WebDAV 验证失败，文件不存在，状态码 %d", resp.StatusCode)
	}

	// 校验文件大小（部分 WebDAV 服务器可能不返回 Content-Length，此时仅校验存在性）
	if resp.ContentLength >= 0 && resp.ContentLength != expectedSize {
		return fmt.Errorf("WebDAV 验证失败，文件大小不匹配: 期望 %d, 实际 %d", expectedSize, resp.ContentLength)
	}

	return nil
}

// compressGzip 将源文件压缩为 gzip 格式
func (s *LogUploadService) compressGzip(srcPath, dstPath string) error {
	srcFile, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("打开源文件失败: %w", err)
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("创建目标文件失败: %w", err)
	}
	defer dstFile.Close()

	gzWriter := gzip.NewWriter(dstFile)

	if _, err := io.Copy(gzWriter, srcFile); err != nil {
		gzWriter.Close()
		return fmt.Errorf("gzip 压缩写入失败: %w", err)
	}

	if err := gzWriter.Close(); err != nil {
		return fmt.Errorf("gzip 关闭失败（可能 footer 写入错误）: %w", err)
	}

	return nil
}

// uploadFileToWebDAV 从文件流式上传到 WebDAV 服务器
func (s *LogUploadService) uploadFileToWebDAV(filePath, filename string, settings types.SystemSettings) error {
	baseURL := settings.LogUploadWebDAVURL
	username := settings.LogUploadWebDAVUsername
	password := settings.LogUploadWebDAVPassword

	if baseURL == "" {
		return fmt.Errorf("WebDAV URL 未配置")
	}

	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}

	client := &http.Client{Timeout: 30 * time.Minute}

	// 打开文件
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("打开 WebDAV 上传文件失败: %w", err)
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("获取 WebDAV 上传文件信息失败: %w", err)
	}

	// 避免双斜杠
	filename = strings.TrimPrefix(filename, "/")
	uploadURL := baseURL + filename

	req, err := http.NewRequest("PUT", uploadURL, file)
	if err != nil {
		return fmt.Errorf("创建 WebDAV 上传请求失败: %w", err)
	}

	req.ContentLength = fileInfo.Size()

	// 根据文件扩展名设置正确的 Content-Type
	if strings.HasSuffix(strings.ToLower(filename), ".gz") {
		req.Header.Set("Content-Type", "application/gzip")
	} else {
		req.Header.Set("Content-Type", "text/csv")
	}

	if username != "" || password != "" {
		req.SetBasicAuth(username, password)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("WebDAV 上传请求失败: %w", err)
	}
	defer resp.Body.Close()

	// PUT 失败时自动通过 MKCOL 创建目录后重试一次
	if resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		logrus.WithField("url", uploadURL).WithField("status", resp.StatusCode).Warn("WebDAV PUT failed, attempting to create target directory via MKCOL and retry; body=" + string(body))
		if mkcolErr := s.ensureWebDAVDirectory(filename, settings); mkcolErr != nil {
			return fmt.Errorf("WebDAV 上传失败(状态码 %d)，且自动创建目录失败: %w", resp.StatusCode, mkcolErr)
		}

		// 重新打开文件用于重试
		retryFile, retryErr := os.Open(filePath)
		if retryErr != nil {
			return fmt.Errorf("重新打开上传文件失败: %w", retryErr)
		}
		defer retryFile.Close()

		retryReq, retryErr := http.NewRequest("PUT", uploadURL, retryFile)
		if retryErr != nil {
			return fmt.Errorf("创建 WebDAV 重试上传请求失败: %w", retryErr)
		}
		retryReq.ContentLength = fileInfo.Size()
		if strings.HasSuffix(strings.ToLower(filename), ".gz") {
			retryReq.Header.Set("Content-Type", "application/gzip")
		} else {
			retryReq.Header.Set("Content-Type", "text/csv")
		}
		if username != "" || password != "" {
			retryReq.SetBasicAuth(username, password)
		}

		retryResp, retryErr := client.Do(retryReq)
		if retryErr != nil {
			return fmt.Errorf("WebDAV 重试上传请求失败: %w", retryErr)
		}
		defer retryResp.Body.Close()

		if retryResp.StatusCode < 200 || retryResp.StatusCode >= 300 {
			body, _ := io.ReadAll(retryResp.Body)
			return fmt.Errorf("WebDAV 重试上传仍失败，状态码 %d: %s", retryResp.StatusCode, string(body))
		}

		logrus.WithField("url", uploadURL).Info("WebDAV 上传成功（MKCOL 创建目录后重试）")
		return nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("WebDAV 上传失败，状态码 %d: %s", resp.StatusCode, string(body))
	}

	logrus.WithField("url", uploadURL).Info("WebDAV 上传成功")
	return nil
}

// ensureWebDAVDirectory 通过 MKCOL 创建 filename 所在的父目录（支持多级）。
// ensureWebDAVDirectory 通过 MKCOL 逐级创建父目录
func (s *LogUploadService) ensureWebDAVDirectory(filename string, settings types.SystemSettings) error {
	baseURL := settings.LogUploadWebDAVURL
	username := settings.LogUploadWebDAVUsername
	password := settings.LogUploadWebDAVPassword

	if baseURL == "" {
		return fmt.Errorf("WebDAV URL 未配置")
	}
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}

	filename = strings.TrimPrefix(filename, "/")
	// 提取目录部分（去掉最后的文件名）
	idx := strings.LastIndex(filename, "/")
	if idx <= 0 {
		// 没有子目录，无需创建
		return nil
	}
	dirPath := filename[:idx] // 例如 "backup/sub"

	client := &http.Client{Timeout: 30 * time.Second}

	// 逐级创建目录：backup -> backup/sub
	parts := strings.Split(dirPath, "/")
	current := ""
	for _, part := range parts {
		if part == "" {
			continue
		}
		current = current + "/" + part
		mkcolURL := baseURL + strings.TrimPrefix(current, "/") + "/"

		req, err := http.NewRequest("MKCOL", mkcolURL, nil)
		if err != nil {
			return fmt.Errorf("创建 MKCOL 请求失败: %w", err)
		}
		if username != "" || password != "" {
			req.SetBasicAuth(username, password)
		}

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("MKCOL 请求失败: %w", err)
		}
		// 201 Created 表示新建成功；405 Method Not Allowed 通常表示目录已存在，可忽略
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusMethodNotAllowed {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return fmt.Errorf("MKCOL 创建目录 %s 失败，状态码 %d: %s", mkcolURL, resp.StatusCode, string(body))
		}
		resp.Body.Close()
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

// sanitizeCSVCell 防止 CSV 公式注入：以 = + - @ 开头的单元格加单引号前缀
func sanitizeCSVCell(val string) string {
	if len(val) == 0 {
		return val
	}
	switch val[0] {
	case '=', '+', '-', '@':
		return "'" + val
	}
	return val
}

// ============================================================
// 工具方法
// ============================================================
