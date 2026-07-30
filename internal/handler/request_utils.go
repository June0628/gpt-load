package handler

import (
	"strconv"

	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/response"

	"github.com/gin-gonic/gin"
)

// bindJSON 绑定请求体 JSON，失败时向客户端返回错误并返回 false。
func bindJSON(c *gin.Context, req any) bool {
	if err := c.ShouldBindJSON(req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return false
	}
	return true
}

// parseUintParam 将路径参数解析为正整数，解析失败时返回 0 和 false（不发送响应）。
func parseUintParam(c *gin.Context, param string) (uint, bool) {
	value, err := strconv.Atoi(c.Param(param))
	if err != nil || value <= 0 {
		return 0, false
	}
	return uint(value), true
}

// parseIDParam 将路径参数解析为正整数 ID，失败时以 msgID 返回本地化错误并返回 false。
func parseIDParam(c *gin.Context, param, msgID string) (uint, bool) {
	id, ok := parseUintParam(c, param)
	if !ok {
		response.ErrorI18nFromAPIError(c, app_errors.ErrBadRequest, msgID)
		return 0, false
	}
	return id, true
}

// parseGroupIDParam 解析路径参数 id 中的分组 ID。
func parseGroupIDParam(c *gin.Context) (uint, bool) {
	return parseIDParam(c, "id", "validation.invalid_group_id")
}

// parseSubGroupIDParam 解析路径参数 subGroupId 中的子分组 ID。
func parseSubGroupIDParam(c *gin.Context) (uint, bool) {
	return parseIDParam(c, "subGroupId", "validation.invalid_sub_group_id")
}
