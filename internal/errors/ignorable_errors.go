package errors

import (
	"strings"
)

// ignorableErrorSubstrings 包含可安全忽略的错误子字符串列表，这些错误通常发生在客户端提前断开连接时
var ignorableErrorSubstrings = []string{
	"context canceled",
	"connection reset by peer",
	"broken pipe",
	"use of closed network connection",
	"request canceled",
}

// IsIgnorableError 检查给定错误是否为客户端断开连接时可能发生的常见非关键错误，用于防止记录不必要的错误和避免将密钥标记为失败
func IsIgnorableError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	for _, sub := range ignorableErrorSubstrings {
		if strings.Contains(errStr, sub) {
			return true
		}
	}
	return false
}
