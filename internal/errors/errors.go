package errors

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

// APIError 定义API响应的标准错误结构
type APIError struct {
	HTTPStatus int
	Code       string
	Message    string
}

// Error 实现error接口
func (e *APIError) Error() string {
	return e.Message
}

// 预定义的API错误
var (
	ErrBadRequest         = &APIError{HTTPStatus: http.StatusBadRequest, Code: "BAD_REQUEST", Message: "Invalid request parameters"}
	ErrInvalidJSON        = &APIError{HTTPStatus: http.StatusBadRequest, Code: "INVALID_JSON", Message: "Invalid JSON format"}
	ErrValidation         = &APIError{HTTPStatus: http.StatusBadRequest, Code: "VALIDATION_FAILED", Message: "Input validation failed"}
	ErrDuplicateResource  = &APIError{HTTPStatus: http.StatusConflict, Code: "DUPLICATE_RESOURCE", Message: "Resource already exists"}
	ErrResourceNotFound   = &APIError{HTTPStatus: http.StatusNotFound, Code: "NOT_FOUND", Message: "Resource not found"}
	ErrInternalServer     = &APIError{HTTPStatus: http.StatusInternalServerError, Code: "INTERNAL_SERVER_ERROR", Message: "An unexpected error occurred"}
	ErrDatabase           = &APIError{HTTPStatus: http.StatusInternalServerError, Code: "DATABASE_ERROR", Message: "Database operation failed"}
	ErrUnauthorized       = &APIError{HTTPStatus: http.StatusUnauthorized, Code: "UNAUTHORIZED", Message: "Authentication failed"}
	ErrForbidden          = &APIError{HTTPStatus: http.StatusForbidden, Code: "FORBIDDEN", Message: "You do not have permission to access this resource"}
	ErrTaskInProgress     = &APIError{HTTPStatus: http.StatusConflict, Code: "TASK_IN_PROGRESS", Message: "A task is already in progress"}
	ErrBadGateway         = &APIError{HTTPStatus: http.StatusBadGateway, Code: "BAD_GATEWAY", Message: "Upstream service error"}
	ErrNoActiveKeys       = &APIError{HTTPStatus: http.StatusServiceUnavailable, Code: "NO_ACTIVE_KEYS", Message: "No active API keys available for this group"}
	ErrMaxRetriesExceeded = &APIError{HTTPStatus: http.StatusBadGateway, Code: "MAX_RETRIES_EXCEEDED", Message: "Request failed after maximum retries"}
	ErrNoKeysAvailable    = &APIError{HTTPStatus: http.StatusServiceUnavailable, Code: "NO_KEYS_AVAILABLE", Message: "No API keys available to process the request"}
)

// NewAPIError 创建带有自定义消息的新APIError
func NewAPIError(base *APIError, message string) *APIError {
	return &APIError{
		HTTPStatus: base.HTTPStatus,
		Code:       base.Code,
		Message:    message,
	}
}

// NewAPIErrorWithUpstream 专门用于包装原始上游错误的APIError
func NewAPIErrorWithUpstream(statusCode int, code string, upstreamMessage string) *APIError {
	return &APIError{
		HTTPStatus: statusCode,
		Code:       code,
		Message:    upstreamMessage,
	}
}

// ParseDBError 智能地将GORM错误转换为标准APIError
func ParseDBError(err error) *APIError {
	if err == nil {
		return nil
	}

	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrResourceNotFound
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code == "23505" { // unique_violation
			return ErrDuplicateResource
		}
	}

	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		if mysqlErr.Number == 1062 { // Duplicate entry
			return ErrDuplicateResource
		}
	}

	// SQLite 通用检查
	if strings.Contains(strings.ToLower(err.Error()), "unique constraint failed") {
		return ErrDuplicateResource
	}

	return ErrDatabase
}
