// Package handler 提供应用程序的 HTTP 处理器
package handler

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gpt-load/internal/config"
	"gpt-load/internal/encryption"
	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/i18n"
	"gpt-load/internal/models"
	"gpt-load/internal/response"
	"gpt-load/internal/services"
	"gpt-load/internal/types"

	"github.com/gin-gonic/gin"
	"go.uber.org/dig"
	"gorm.io/gorm"
)

// Server 包含 HTTP 处理器的依赖
type Server struct {
	DB                         *gorm.DB
	config                     types.ConfigManager
	SettingsManager            *config.SystemSettingsManager
	GroupManager               *services.GroupManager
	GroupService               *services.GroupService
	AggregateGroupService      *services.AggregateGroupService
	KeyManualValidationService *services.KeyManualValidationService
	TaskService                *services.TaskService
	KeyService                 *services.KeyService
	KeyImportService           *services.KeyImportService
	KeyDeleteService           *services.KeyDeleteService
	LogService                 *services.LogService
	LogUploadService           *services.LogUploadService
	CommonHandler              *CommonHandler
	EncryptionSvc              encryption.Service
}

// NewServerParams 定义 NewServer 构造函数的依赖。
type NewServerParams struct {
	dig.In
	DB                         *gorm.DB
	Config                     types.ConfigManager
	SettingsManager            *config.SystemSettingsManager
	GroupManager               *services.GroupManager
	GroupService               *services.GroupService
	AggregateGroupService      *services.AggregateGroupService
	KeyManualValidationService *services.KeyManualValidationService
	TaskService                *services.TaskService
	KeyService                 *services.KeyService
	KeyImportService           *services.KeyImportService
	KeyDeleteService           *services.KeyDeleteService
	LogService                 *services.LogService
	LogUploadService           *services.LogUploadService
	CommonHandler              *CommonHandler
	EncryptionSvc              encryption.Service
}

// NewServer 创建新的 handler 实例，通过 dig 注入依赖。
func NewServer(params NewServerParams) *Server {
	return &Server{
		DB:                         params.DB,
		config:                     params.Config,
		SettingsManager:            params.SettingsManager,
		GroupManager:               params.GroupManager,
		GroupService:               params.GroupService,
		AggregateGroupService:      params.AggregateGroupService,
		KeyManualValidationService: params.KeyManualValidationService,
		TaskService:                params.TaskService,
		KeyService:                 params.KeyService,
		KeyImportService:           params.KeyImportService,
		KeyDeleteService:           params.KeyDeleteService,
		LogService:                 params.LogService,
		LogUploadService:           params.LogUploadService,
		CommonHandler:              params.CommonHandler,
		EncryptionSvc:              params.EncryptionSvc,
	}
}

// LoginRequest 表示登录请求参数
type LoginRequest struct {
	AuthKey string `json:"auth_key" binding:"required"`
}

// LoginResponse 表示登录响应
type LoginResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// Login 处理身份验证
func (s *Server) Login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": i18n.Message(c, "auth.invalid_request"),
		})
		return
	}

	authConfig := s.config.GetAuthConfig()

	isValid := subtle.ConstantTimeCompare([]byte(req.AuthKey), []byte(authConfig.Key)) == 1

	if isValid {
		c.JSON(http.StatusOK, LoginResponse{
			Success: true,
			Message: i18n.Message(c, "auth.authentication_successful"),
		})
	} else {
		c.JSON(http.StatusUnauthorized, LoginResponse{
			Success: false,
			Message: i18n.Message(c, "auth.authentication_failed"),
		})
	}
}

// QueryBalance 手动触发分组余额查询
// 检查余额查询开关并加锁防止与定时查询竞态
func (s *Server) QueryBalance(c *gin.Context) {
	groupID, ok := parseIDParam(c, "id", "handler.invalid_group_id")
	if !ok {
		return
	}

	group, err := s.GroupManager.GetGroupByID(groupID)
	if err != nil {
		response.Error(c, app_errors.ParseDBError(err))
		return
	}

	if group.GroupType == "aggregate" {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, i18n.Message(c, "handler.aggregate_no_balance")))
		return
	}

	// 检查是否启用了余额查询
	if !group.ShouldQueryBalance() {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, i18n.Message(c, "handler.balance_query_not_enabled")))
		return
	}

	// 加锁防止手动查询与定时查询竞态
	if !s.KeyService.TryAcquireBalanceQueryLock(group.ID) {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, i18n.Message(c, "handler.balance_query_in_progress")))
		return
	}

	// 异步执行余额查询
	go func(g *models.Group) {
		defer s.KeyService.ReleaseBalanceQueryLock(g.ID)
		s.KeyService.QueryGroupBalances(g)
	}(group)

	response.Success(c, gin.H{
		"message":    i18n.Message(c, "handler.balance_query_started"),
		"group_name": group.Name,
	})
}

// ClearTask 强制清除卡住的任务
func (s *Server) ClearTask(c *gin.Context) {
	if err := s.TaskService.ForceClearTask(); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInternalServer, i18n.Message(c, "handler.clear_task_failed")))
		return
	}
	response.Success(c, gin.H{"message": i18n.Message(c, "handler.task_cleared")})
}

// Health 处理健康检查请求
func (s *Server) Health(c *gin.Context) {
	uptime := "unknown"
	if startTime, exists := c.Get("serverStartTime"); exists {
		if st, ok := startTime.(time.Time); ok {
			uptime = time.Since(st).String()
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"status":    "healthy",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"uptime":    uptime,
	})
}

// TestLogUploadConfigRequest 测试日志上传配置的请求参数
type TestLogUploadConfigRequest struct {
	LogUploadEnabled          *bool  `json:"log_upload_enabled"`
	LogUploadProvider         string `json:"log_upload_provider"`
	LogUploadTencentSecretID  string `json:"log_upload_tencent_secret_id"`
	LogUploadTencentSecretKey string `json:"log_upload_tencent_secret_key"`
	LogUploadTencentBucket     string `json:"log_upload_tencent_bucket"`
	LogUploadTencentRegion     string `json:"log_upload_tencent_region"`
	LogUploadWebDAVURL         string `json:"log_upload_webdav_url"`
	LogUploadWebDAVUsername    string `json:"log_upload_webdav_username"`
	LogUploadWebDAVPassword    string `json:"log_upload_webdav_password"`
}

// TestLogUploadConfig 测试日志上传配置是否可以连接
// 优先使用请求体中的配置（允许测试未保存的配置），如果没有请求体则使用已保存的配置
func (s *Server) TestLogUploadConfig(c *gin.Context) {
	// 尝试解析请求体中的配置
	var req TestLogUploadConfigRequest
	hasRequestBody := c.ShouldBindJSON(&req) == nil

	// 获取已保存的配置作为基础
	settings := s.SettingsManager.GetSettings()

	// 如果请求体中有配置，则用请求体中的值覆盖
	if hasRequestBody {
		// LogUploadEnabled 使用指针类型，仅当 JSON 中明确提供该字段时才覆盖
		if req.LogUploadEnabled != nil {
			settings.LogUploadEnabled = *req.LogUploadEnabled
		}
		if req.LogUploadProvider != "" {
			settings.LogUploadProvider = req.LogUploadProvider
		}
		if req.LogUploadTencentSecretID != "" {
			settings.LogUploadTencentSecretID = req.LogUploadTencentSecretID
		}
		if req.LogUploadTencentSecretKey != "" {
			settings.LogUploadTencentSecretKey = req.LogUploadTencentSecretKey
		}
		if req.LogUploadTencentBucket != "" {
			settings.LogUploadTencentBucket = req.LogUploadTencentBucket
		}
		if req.LogUploadTencentRegion != "" {
			settings.LogUploadTencentRegion = req.LogUploadTencentRegion
		}
		if req.LogUploadWebDAVURL != "" {
			settings.LogUploadWebDAVURL = req.LogUploadWebDAVURL
		}
		if req.LogUploadWebDAVUsername != "" {
			settings.LogUploadWebDAVUsername = req.LogUploadWebDAVUsername
		}
		if req.LogUploadWebDAVPassword != "" {
			settings.LogUploadWebDAVPassword = req.LogUploadWebDAVPassword
		}
	}

	if !settings.LogUploadEnabled {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, i18n.Message(c, "handler.log_upload_not_enabled")))
		return
	}

	provider := strings.ToLower(settings.LogUploadProvider)
	result := map[string]interface{}{
		"success":   false,
		"provider":  provider,
		"message":   "",
		"test_time": time.Now().UTC().Format(time.RFC3339),
	}

	switch provider {
	case "tencent", "cos", "tencent_cos":
		// 测试腾讯云COS配置
		err := s.testTencentCOSConfig(settings)
		if err != nil {
			result["message"] = i18n.Message(c, "handler.log_upload_cos_test_failed", map[string]any{"error": err.Error()})
		} else {
			result["success"] = true
			result["message"] = i18n.Message(c, "handler.log_upload_cos_test_success")
		}
	case "webdav":
		// 测试WebDAV配置
		err := s.testWebDAVConfig(settings)
		if err != nil {
			result["message"] = i18n.Message(c, "handler.log_upload_webdav_test_failed", map[string]any{"error": err.Error()})
		} else {
			result["success"] = true
			result["message"] = i18n.Message(c, "handler.log_upload_webdav_test_success")
		}
	default:
		result["message"] = i18n.Message(c, "handler.log_upload_unsupported_provider", map[string]any{"provider": provider})
	}

	response.Success(c, result)
}

// testTencentCOSConfig 测试腾讯云COS配置
func (s *Server) testTencentCOSConfig(settings types.SystemSettings) error {
	secretID := settings.LogUploadTencentSecretID
	secretKey := settings.LogUploadTencentSecretKey
	bucket := settings.LogUploadTencentBucket
	region := settings.LogUploadTencentRegion

	// 验证必填参数
	if secretID == "" || secretKey == "" || bucket == "" || region == "" {
		return fmt.Errorf("COS configuration incomplete: check SecretId, SecretKey, Bucket and Region")
	}

	// 构建测试URL - 使用HEAD请求检查bucket是否存在
	host := fmt.Sprintf("%s.cos.%s.myqcloud.com", bucket, region)
	testURL := fmt.Sprintf("https://%s", host)

	// 生成签名
	method := "HEAD"
	uri := "/"
	authHeader := s.cosAuthorization(secretID, secretKey, method, uri, host)

	// 创建HTTP客户端
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// 创建请求
	req, err := http.NewRequest(method, testURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	// 添加认证头
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Host", host)

	// 发送请求
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to COS service: %v", err)
	}
	defer resp.Body.Close()

	// 检查状态码
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
		return nil
	} else if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("authentication failed: check SecretId and SecretKey")
	} else if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("bucket does not exist or is not accessible")
	} else {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("COS service returned error status: %d, response: %s", resp.StatusCode, string(body))
	}
}

// testWebDAVConfig 测试WebDAV配置
func (s *Server) testWebDAVConfig(settings types.SystemSettings) error {
	baseURL := settings.LogUploadWebDAVURL
	username := settings.LogUploadWebDAVUsername
	password := settings.LogUploadWebDAVPassword

	// 验证必填参数
	if baseURL == "" {
		return fmt.Errorf("WebDAV URL is not configured")
	}

	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}

	// 创建HTTP客户端
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// 测试PROPFIND请求检查服务器是否支持WebDAV
	req, err := http.NewRequest("PROPFIND", baseURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	// 添加认证信息
	if username != "" || password != "" {
		req.SetBasicAuth(username, password)
	}

	// 添加WebDAV必需的头部
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to WebDAV service: %v", err)
	}
	defer resp.Body.Close()

	// 检查响应状态码
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusMultiStatus {
		// 200/207 表示WebDAV服务可用
		return nil
	} else if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("authentication failed: check username and password")
	} else if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("access denied: check permission settings")
	} else if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("WebDAV path does not exist")
	} else {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("WebDAV service returned error status: %d, response: %s", resp.StatusCode, string(body))
	}
}

// cosAuthorization 生成腾讯云 COS v5 签名
// 参考文档：https://cloud.tencent.com/document/product/436/7778
func (s *Server) cosAuthorization(secretID, secretKey, method, uri, host string) string {
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
