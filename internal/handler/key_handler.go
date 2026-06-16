package handler

import (
	"fmt"
	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/models"
	"gpt-load/internal/response"
	"io"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// sanitizeFilename 移除或替换文件名中不安全的字符，防止 HTTP 头注入。
func sanitizeFilename(name string) string {
	// 替换路径分隔符和危险字符
	replacer := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		"..", "_",
		"\x00", "",
		"\r", "",
		"\n", "",
		"\"", "'",
	)
	sanitized := replacer.Replace(name)
	// 限制长度
	if utf8.RuneCountInString(sanitized) > 100 {
		sanitized = string([]rune(sanitized)[:100])
	}
	return sanitized
}

// validateGroupIDFromQuery 从查询参数中验证并解析分组 ID，验证失败返回 0 和 false（错误已发送给客户端）
func validateGroupIDFromQuery(c *gin.Context) (uint, bool) {
	groupIDStr := c.Query("group_id")
	if groupIDStr == "" {
		response.ErrorI18nFromAPIError(c, app_errors.ErrBadRequest, "validation.group_id_required")
		return 0, false
	}

	groupID, err := strconv.Atoi(groupIDStr)
	if err != nil || groupID <= 0 {
		response.ErrorI18nFromAPIError(c, app_errors.ErrBadRequest, "validation.invalid_group_id_format")
		return 0, false
	}

	return uint(groupID), true
}

// validateKeysText 验证密钥文本输入，验证失败返回 false（错误已发送给客户端）
func validateKeysText(c *gin.Context, keysText string) bool {
	if strings.TrimSpace(keysText) == "" {
		response.ErrorI18nFromAPIError(c, app_errors.ErrValidation, "validation.keys_text_empty")
		return false
	}

	return true
}

// findGroupByID 根据 ID 查找分组的辅助函数。
func (s *Server) findGroupByID(c *gin.Context, groupID uint) (*models.Group, bool) {
	var group models.Group
	if err := s.DB.First(&group, groupID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			response.Error(c, app_errors.ErrResourceNotFound)
		} else {
			response.Error(c, app_errors.ParseDBError(err))
		}
		return nil, false
	}
	return &group, true
}

// KeyTextRequest 定义需要分组 ID 和密钥文本块的操作的通用请求参数。
type KeyTextRequest struct {
	GroupID  uint   `json:"group_id" binding:"required"`
	KeysText string `json:"keys_text" binding:"required"`
}

// GroupIDRequest 定义仅需要分组 ID 的操作的通用请求参数。
type GroupIDRequest struct {
	GroupID uint `json:"group_id" binding:"required"`
}

// ValidateGroupKeysRequest 定义验证分组中密钥的请求参数。
type ValidateGroupKeysRequest struct {
	GroupID uint   `json:"group_id" binding:"required"`
	Status  string `json:"status,omitempty"`
}

// AddMultipleKeys 处理从文本块在指定分组中批量创建密钥。
func (s *Server) AddMultipleKeys(c *gin.Context) {
	var req KeyTextRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	if _, ok := s.findGroupByID(c, req.GroupID); !ok {
		return
	}

	if !validateKeysText(c, req.KeysText) {
		return
	}

	result, err := s.KeyService.AddMultipleKeys(req.GroupID, req.KeysText)
	if err != nil {
		if strings.Contains(err.Error(), "batch size exceeds the limit") {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, err.Error()))
		} else if err.Error() == "no valid keys found in the input text" {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, err.Error()))
		} else {
			response.Error(c, app_errors.ParseDBError(err))
		}
		return
	}

	response.Success(c, result)
}

// AddMultipleKeysAsync 处理从文本块或文件在指定分组中异步批量创建密钥。
func (s *Server) AddMultipleKeysAsync(c *gin.Context) {
	var groupID uint
	var keysText string

	// 检查内容类型以判断是文件上传还是 JSON 请求
	contentType := c.ContentType()

	if strings.Contains(contentType, "multipart/form-data") {
		// 处理文件上传
		groupIDStr := c.PostForm("group_id")
		if groupIDStr == "" {
			response.ErrorI18nFromAPIError(c, app_errors.ErrBadRequest, "validation.group_id_required")
			return
		}

		groupIDInt, err := strconv.Atoi(groupIDStr)
		if err != nil || groupIDInt <= 0 {
			response.ErrorI18nFromAPIError(c, app_errors.ErrBadRequest, "validation.invalid_group_id_format")
			return
		}
		groupID = uint(groupIDInt)

		// 获取上传的文件
		file, err := c.FormFile("file")
		if err != nil {
			response.ErrorI18nFromAPIError(c, app_errors.ErrBadRequest, "validation.file_required")
			return
		}

		// 验证文件扩展名
		ext := strings.ToLower(filepath.Ext(file.Filename))
		if ext != ".txt" {
			response.ErrorI18nFromAPIError(c, app_errors.ErrValidation, "validation.only_txt_supported")
			return
		}

		// 读取文件内容
		fileContent, err := file.Open()
		if err != nil {
			response.ErrorI18nFromAPIError(c, app_errors.ErrBadRequest, "validation.failed_to_open_file")
			return
		}
		defer fileContent.Close()

		// 使用 io.ReadAll 将文件内容读取为字符串
		buf, err := io.ReadAll(fileContent)
		if err != nil {
			response.ErrorI18nFromAPIError(c, app_errors.ErrBadRequest, "validation.failed_to_read_file")
			return
		}
		keysText = string(buf)
	} else {
		// 处理 JSON 请求（原始行为）
		var req KeyTextRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
			return
		}
		groupID = req.GroupID
		keysText = req.KeysText
	}

	group, ok := s.findGroupByID(c, groupID)
	if !ok {
		return
	}

	if !validateKeysText(c, keysText) {
		return
	}

	taskStatus, err := s.KeyImportService.StartImportTask(group, keysText)
	if err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrTaskInProgress, err.Error()))
		return
	}

	response.Success(c, taskStatus)
}

// ListKeysInGroup 处理分页获取指定分组中的所有密钥。
func (s *Server) ListKeysInGroup(c *gin.Context) {
	groupID, ok := validateGroupIDFromQuery(c)
	if !ok {
		return
	}

	if _, ok := s.findGroupByID(c, groupID); !ok {
		return
	}

	statusFilter := c.Query("status")
	if statusFilter != "" && statusFilter != models.KeyStatusActive && statusFilter != models.KeyStatusInvalid {
		response.ErrorI18nFromAPIError(c, app_errors.ErrValidation, "validation.invalid_status_filter")
		return
	}

	searchKeyword := c.Query("key_value")
	searchHash := ""
	if searchKeyword != "" {
		searchHash = s.EncryptionSvc.Hash(searchKeyword)
	}

	query := s.KeyService.ListKeysInGroupQuery(groupID, statusFilter, searchHash)

	var keys []models.APIKey
	paginatedResult, err := response.Paginate(c, query, &keys)
	if err != nil {
		response.Error(c, app_errors.ParseDBError(err))
		return
	}

	// 解密所有密钥以便展示
	for i := range keys {
		decryptedValue, err := s.EncryptionSvc.Decrypt(keys[i].KeyValue)
		if err != nil {
			logrus.WithError(err).WithField("key_id", keys[i].ID).Error("Failed to decrypt key value for listing")
			keys[i].KeyValue = "failed-to-decrypt"
		} else {
			keys[i].KeyValue = decryptedValue
		}
	}
	paginatedResult.Items = keys

	response.Success(c, paginatedResult)
}

// DeleteMultipleKeys 处理从文本块在指定分组中批量删除密钥。
func (s *Server) DeleteMultipleKeys(c *gin.Context) {
	var req KeyTextRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	if _, ok := s.findGroupByID(c, req.GroupID); !ok {
		return
	}

	if !validateKeysText(c, req.KeysText) {
		return
	}

	result, err := s.KeyService.DeleteMultipleKeys(req.GroupID, req.KeysText)
	if err != nil {
		if strings.Contains(err.Error(), "batch size exceeds the limit") {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, err.Error()))
		} else if err.Error() == "no valid keys found in the input text" {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, err.Error()))
		} else {
			response.Error(c, app_errors.ParseDBError(err))
		}
		return
	}

	response.Success(c, result)
}

// DeleteMultipleKeysAsync 处理通过异步任务从文本块在指定分组中批量删除密钥。
func (s *Server) DeleteMultipleKeysAsync(c *gin.Context) {
	var req KeyTextRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	group, ok := s.findGroupByID(c, req.GroupID)
	if !ok {
		return
	}

	if !validateKeysText(c, req.KeysText) {
		return
	}

	taskStatus, err := s.KeyDeleteService.StartDeleteTask(group, req.KeysText)
	if err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrTaskInProgress, err.Error()))
		return
	}

	response.Success(c, taskStatus)
}

// RestoreMultipleKeys 处理从文本块在指定分组中批量恢复密钥。
func (s *Server) RestoreMultipleKeys(c *gin.Context) {
	var req KeyTextRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	if _, ok := s.findGroupByID(c, req.GroupID); !ok {
		return
	}

	if !validateKeysText(c, req.KeysText) {
		return
	}

	result, err := s.KeyService.RestoreMultipleKeys(req.GroupID, req.KeysText)
	if err != nil {
		if strings.Contains(err.Error(), "batch size exceeds the limit") {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, err.Error()))
		} else if err.Error() == "no valid keys found in the input text" {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, err.Error()))
		} else {
			response.Error(c, app_errors.ParseDBError(err))
		}
		return
	}

	response.Success(c, result)
}

// TestMultipleKeys 处理多密钥的一次性验证测试。
func (s *Server) TestMultipleKeys(c *gin.Context) {
	var req KeyTextRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	groupDB, ok := s.findGroupByID(c, req.GroupID)
	if !ok {
		return
	}

	group, err := s.GroupManager.GetGroupByName(groupDB.Name)
	if err != nil {
		response.ErrorI18nFromAPIError(c, app_errors.ErrResourceNotFound, "validation.group_not_found")
		return
	}

	if !validateKeysText(c, req.KeysText) {
		return
	}

	start := time.Now()
	results, err := s.KeyService.TestMultipleKeys(group, req.KeysText)
	duration := time.Since(start).Milliseconds()
	if err != nil {
		if strings.Contains(err.Error(), "batch size exceeds the limit") {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, err.Error()))
		} else if err.Error() == "no valid keys found in the input text" {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, err.Error()))
		} else {
			response.Error(c, app_errors.ParseDBError(err))
		}
		return
	}

	response.Success(c, gin.H{
		"results":        results,
		"total_duration": duration,
	})
}

// ValidateGroupKeys 发起对分组中所有密钥的手动验证任务。
func (s *Server) ValidateGroupKeys(c *gin.Context) {
	var req ValidateGroupKeysRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	// 如果提供了状态则进行验证
	if req.Status != "" && req.Status != models.KeyStatusActive && req.Status != models.KeyStatusInvalid {
		response.ErrorI18nFromAPIError(c, app_errors.ErrValidation, "validation.invalid_status_value")
		return
	}

	groupDB, ok := s.findGroupByID(c, req.GroupID)
	if !ok {
		return
	}

	group, err := s.GroupManager.GetGroupByName(groupDB.Name)
	if err != nil {
		response.ErrorI18nFromAPIError(c, app_errors.ErrResourceNotFound, "validation.group_not_found")
		return
	}

	taskStatus, err := s.KeyManualValidationService.StartValidationTask(group, req.Status)
	if err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrTaskInProgress, err.Error()))
		return
	}

	response.Success(c, taskStatus)
}

// RestoreAllInvalidKeys 将分组中所有"失效"密钥的状态恢复为"有效"。
func (s *Server) RestoreAllInvalidKeys(c *gin.Context) {
	var req GroupIDRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	if _, ok := s.findGroupByID(c, req.GroupID); !ok {
		return
	}

	rowsAffected, err := s.KeyService.RestoreAllInvalidKeys(req.GroupID)
	if err != nil {
		response.Error(c, app_errors.ParseDBError(err))
		return
	}

	response.SuccessI18n(c, "success.keys_restored", nil, map[string]any{"count": rowsAffected})
}

// ClearAllInvalidKeys 删除分组中所有"失效"密钥。
func (s *Server) ClearAllInvalidKeys(c *gin.Context) {
	var req GroupIDRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	if _, ok := s.findGroupByID(c, req.GroupID); !ok {
		return
	}

	rowsAffected, err := s.KeyService.ClearAllInvalidKeys(req.GroupID)
	if err != nil {
		response.Error(c, app_errors.ParseDBError(err))
		return
	}

	response.SuccessI18n(c, "success.invalid_keys_cleared", nil, map[string]any{"count": rowsAffected})
}

// ClearAllKeys 删除分组中的所有密钥。
func (s *Server) ClearAllKeys(c *gin.Context) {
	var req GroupIDRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	if _, ok := s.findGroupByID(c, req.GroupID); !ok {
		return
	}

	rowsAffected, err := s.KeyService.ClearAllKeys(req.GroupID)
	if err != nil {
		response.Error(c, app_errors.ParseDBError(err))
		return
	}

	response.SuccessI18n(c, "success.all_keys_cleared", nil, map[string]any{"count": rowsAffected})
}

// ExportKeys 处理将密钥导出为文本文件。
func (s *Server) ExportKeys(c *gin.Context) {
	groupID, ok := validateGroupIDFromQuery(c)
	if !ok {
		return
	}

	statusFilter := c.Query("status")
	if statusFilter == "" {
		statusFilter = "all"
	}

	switch statusFilter {
	case "all", models.KeyStatusActive, models.KeyStatusInvalid:
	default:
		response.ErrorI18nFromAPIError(c, app_errors.ErrValidation, "validation.invalid_status_filter")
		return
	}

	group, ok := s.findGroupByID(c, groupID)
	if !ok {
		return
	}

	filename := fmt.Sprintf("keys-%s-%s.txt", sanitizeFilename(group.Name), statusFilter)
	c.Header("Content-Disposition", "attachment; filename=\""+filename+"\"")
	c.Header("Content-Type", "text/plain; charset=utf-8")

	if err := s.KeyService.StreamKeysToWriter(groupID, statusFilter, c.Writer); err != nil {
		log.Printf("Failed to stream keys: %v", err)
	}
}

// UpdateKeyNotesRequest 定义更新密钥备注的请求参数。
type UpdateKeyNotesRequest struct {
	Notes string `json:"notes"`
}

// UpdateKeyNotes 处理更新指定 API 密钥的备注。
func (s *Server) UpdateKeyNotes(c *gin.Context) {
	keyIDStr := c.Param("id")
	keyID, err := strconv.Atoi(keyIDStr)
	if err != nil || keyID <= 0 {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, "invalid key ID format"))
		return
	}

	var req UpdateKeyNotesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	// 标准化并强制检查长度
	req.Notes = strings.TrimSpace(req.Notes)
	if utf8.RuneCountInString(req.Notes) > 255 {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, "notes length must be <= 255 characters"))
		return
	}

	// 检查密钥是否存在并更新其备注
	var key models.APIKey
	if err := s.DB.First(&key, keyID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			response.Error(c, app_errors.ErrResourceNotFound)
		} else {
			response.Error(c, app_errors.ParseDBError(err))
		}
		return
	}

	// 更新备注
	if err := s.DB.Model(&key).Update("notes", req.Notes).Error; err != nil {
		response.Error(c, app_errors.ParseDBError(err))
		return
	}

	response.Success(c, nil)
}
