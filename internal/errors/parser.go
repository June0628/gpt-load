package errors

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const (
	// maxErrorBodyLength 定义存储或返回错误消息的最大长度
	maxErrorBodyLength = 2048
)

// standardErrorResponse 匹配格式如: {"error": {"message": "..."}}
type standardErrorResponse struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// vendorErrorResponse 匹配格式如: {"error_msg": "..."}
type vendorErrorResponse struct {
	ErrorMsg string `json:"error_msg"`
}

// simpleErrorResponse 匹配格式如: {"error": "..."}
type simpleErrorResponse struct {
	Error string `json:"error"`
}

// rootMessageErrorResponse 匹配格式如: {"message": "..."}
type rootMessageErrorResponse struct {
	Message string `json:"message"`
}

// ParseUpstreamError 解析上游响应中的结构化错误信息
func ParseUpstreamError(body []byte) string {
	// 1. 尝试解析标准 OpenAI/Gemini 格式
	var stdErr standardErrorResponse
	if err := json.Unmarshal(body, &stdErr); err == nil {
		if msg := strings.TrimSpace(stdErr.Error.Message); msg != "" {
			return truncateString(msg, maxErrorBodyLength)
		}
	}

	// 2. 尝试解析厂商特定格式（如百度）
	var vendorErr vendorErrorResponse
	if err := json.Unmarshal(body, &vendorErr); err == nil {
		if msg := strings.TrimSpace(vendorErr.ErrorMsg); msg != "" {
			return truncateString(msg, maxErrorBodyLength)
		}
	}

	// 3. 尝试解析简单错误格式
	var simpleErr simpleErrorResponse
	if err := json.Unmarshal(body, &simpleErr); err == nil {
		if msg := strings.TrimSpace(simpleErr.Error); msg != "" {
			return truncateString(msg, maxErrorBodyLength)
		}
	}

	// 4. 尝试解析根级别消息格式
	var rootMsgErr rootMessageErrorResponse
	if err := json.Unmarshal(body, &rootMsgErr); err == nil {
		if msg := strings.TrimSpace(rootMsgErr.Message); msg != "" {
			return truncateString(msg, maxErrorBodyLength)
		}
	}

	// 5. 优雅降级：如果所有解析都失败，返回原始（但安全的）响应体
	return truncateString(string(body), maxErrorBodyLength)
}

// truncateString 确保字符串不超过最大长度（以符文为单位）
func truncateString(s string, maxLength int) string {
	if utf8.RuneCountInString(s) > maxLength {
		runes := []rune(s)
		return string(runes[:maxLength])
	}
	return s
}
