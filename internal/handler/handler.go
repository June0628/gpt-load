// Package handler 提供应用程序的 HTTP 处理器
package handler

import (
	"crypto/subtle"
	"net/http"
	"strconv"
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
	groupIDStr := c.Param("id")
	groupID, err := strconv.Atoi(groupIDStr)
	if err != nil || groupID <= 0 {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, "无效的分组 ID"))
		return
	}

	group, err := s.GroupManager.GetGroupByID(uint(groupID))
	if err != nil {
		response.Error(c, app_errors.ParseDBError(err))
		return
	}

	if group.GroupType == "aggregate" {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, "聚合分组不支持余额查询"))
		return
	}

	// 检查是否启用了余额查询
	if !group.ShouldQueryBalance() {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, "该分组未启用余额查询"))
		return
	}

	// 加锁防止手动查询与定时查询竞态
	if !s.KeyService.TryAcquireBalanceQueryLock(group.ID) {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, "该分组正在进行余额查询，请稍后再试"))
		return
	}

	// 异步执行余额查询
	go func(g *models.Group) {
		defer s.KeyService.ReleaseBalanceQueryLock(g.ID)
		s.KeyService.QueryGroupBalances(g)
	}(group)

	response.Success(c, gin.H{
		"message":    "余额查询已启动",
		"group_name": group.Name,
	})
}

// ClearTask 强制清除卡住的任务
func (s *Server) ClearTask(c *gin.Context) {
	if err := s.TaskService.ForceClearTask(); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInternalServer, "清除任务失败"))
		return
	}
	response.Success(c, gin.H{"message": "任务已清除"})
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
