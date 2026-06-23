package utils

import (
	"encoding/json"
	"strings"
)

// AgentFileContent 表示从请求中提取的文件内容
type AgentFileContent struct {
	Type     string `json:"type"`      // 内容类型：image, file, etc.
	Name     string `json:"name,omitempty"`     // 文件名（如果有）
	MimeType string `json:"mime_type,omitempty"` // MIME类型
	Data     string `json:"data"`      // base64编码的数据或文本内容
}

// ExtractAgentFiles 从OpenAI兼容的请求体中提取文件内容 (image_url/file/text等)
func ExtractAgentFiles(requestBody []byte) []AgentFileContent {
	if len(requestBody) == 0 {
		return nil
	}

	var req map[string]any
	if err := json.Unmarshal(requestBody, &req); err != nil {
		return nil
	}

	messages, ok := req["messages"].([]any)
	if !ok {
		return nil
	}

	var files []AgentFileContent

	for _, msg := range messages {
		msgMap, ok := msg.(map[string]any)
		if !ok {
			continue
		}

		content, ok := msgMap["content"]
		if !ok {
			continue
		}

		// content 可能是字符串或数组
		switch c := content.(type) {
		case string:
			// 字符串内容，检查是否包含base64数据
			// 这种情况较少见，但可以处理嵌入的base64数据
			extractBase64FromString(c, &files)
		case []any:
			// 数组形式的内容（多模态）
			for _, item := range c {
				itemMap, ok := item.(map[string]any)
				if !ok {
					continue
				}
				extractFileFromContentItem(itemMap, &files)
			}
		}
	}

	if len(files) == 0 {
		return nil
	}

	return files
}

// extractFileFromContentItem 从单个content item中提取文件
func extractFileFromContentItem(item map[string]any, files *[]AgentFileContent) {
	contentType, _ := item["type"].(string)

	switch contentType {
	case "image_url":
		// OpenAI格式的图片URL
		imageURL, ok := item["image_url"].(map[string]any)
		if !ok {
			return
		}
		url, _ := imageURL["url"].(string)
		extractBase64FromURL(url, "image", files)

	case "image":
		// 某些API使用的图片格式
		if data, ok := item["data"].(string); ok {
			*files = append(*files, AgentFileContent{
				Type: "image",
				Data: data,
			})
		}

	case "file":
		// 文件类型
		name, _ := item["name"].(string)
		mimeType, _ := item["mime_type"].(string)
		if mimeType == "" {
			mimeType, _ = item["mimeType"].(string)
		}
		data, _ := item["data"].(string)
		if data != "" {
			*files = append(*files, AgentFileContent{
				Type:     "file",
				Name:     name,
				MimeType: mimeType,
				Data:     data,
			})
		}

	case "document":
		// 文档类型（某些API使用）
		name, _ := item["name"].(string)
		data, _ := item["data"].(string)
		if data != "" {
			*files = append(*files, AgentFileContent{
				Type: "document",
				Name: name,
				Data: data,
			})
		}

	case "text":
		// 文本类型（如Cline插件发送的代码文件内容）
		text, _ := item["text"].(string)
		if text != "" {
			*files = append(*files, AgentFileContent{
				Type: "text",
				Data: text,
			})
		}
	}
}

// extractBase64FromURL 从 data URL 中提取 base64 数据
func extractBase64FromURL(url, contentType string, files *[]AgentFileContent) {
	if url == "" {
		return
	}

	// 检查是否是data URL
	if strings.HasPrefix(url, "data:") {
		// 解析data URL
		// 格式: data:[<mediatype>];base64,<data>
		parts := strings.SplitN(url, ",", 2)
		if len(parts) != 2 {
			return
		}

		header := parts[0]
		data := parts[1]

		// 提取MIME类型
		mimeType := ""
		if strings.HasPrefix(header, "data:") {
			mimeType = strings.TrimPrefix(header, "data:")
			mimeType = strings.TrimSuffix(mimeType, ";base64")
		}

		*files = append(*files, AgentFileContent{
			Type:     contentType,
			MimeType: mimeType,
			Data:     data,
		})
	}
}

// extractBase64FromString 从字符串中提取可能的base64数据
func extractBase64FromString(s string, files *[]AgentFileContent) {
	// 查找可能的data URL
	idx := strings.Index(s, "data:")
	if idx == -1 {
		return
	}

	// 从找到的位置开始提取
	sub := s[idx:]
	extractBase64FromURL(sub, "unknown", files)
}

// AgentToolCall 表示从请求中提取的工具调用信息
type AgentToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`     // 通常为 "function"
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"` // 通常是 JSON 字符串
	} `json:"function"`
}

// ExtractAgentToolCalls 从 OpenAI 兼容的请求体中提取历史消息中的工具调用
// （assistant 的 tool_calls，以及 tool/-function 角色的回复结果）
func ExtractAgentToolCalls(requestBody []byte) []AgentToolCall {
	if len(requestBody) == 0 {
		return nil
	}

	var req map[string]any
	if err := json.Unmarshal(requestBody, &req); err != nil {
		return nil
	}

	messages, ok := req["messages"].([]any)
	if !ok {
		return nil
	}

	var calls []AgentToolCall

	for _, msg := range messages {
		msgMap, ok := msg.(map[string]any)
		if !ok {
			continue
		}

		// 1) 标准 OpenAI tool_calls（assistant 角色）
		if rawCalls, ok := msgMap["tool_calls"].([]any); ok {
			for _, raw := range rawCalls {
				callMap, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				call := AgentToolCall{}
				if idx, ok := callMap["index"].(float64); ok {
					ii := int(idx)
					call.Index = &ii
				}
				call.ID, _ = callMap["id"].(string)
				call.Type, _ = callMap["type"].(string)
				if fn, ok := callMap["function"].(map[string]any); ok {
					call.Function.Name, _ = fn["name"].(string)
					call.Function.Arguments, _ = fn["arguments"].(string)
				}
				if call.Function.Name != "" || call.Function.Arguments != "" {
					calls = append(calls, call)
				}
			}
		}

		// 2) tool / function 角色消息：content 即为工具执行结果
		role, _ := msgMap["role"].(string)
		if role == "tool" || role == "function" {
			contentStr := ""
			switch c := msgMap["content"].(type) {
			case string:
				contentStr = c
			case []any:
				// 数组形式，取所有 text 项拼接
				for _, item := range c {
					if m, ok := item.(map[string]any); ok {
						if t, _ := m["text"].(string); t != "" {
							contentStr += t
						}
					}
				}
			}
			if contentStr != "" {
				call := AgentToolCall{}
				call.ID, _ = msgMap["tool_call_id"].(string)
				call.Function.Name, _ = msgMap["name"].(string)
				call.Function.Arguments = contentStr
				calls = append(calls, call)
			}
		}
	}

	if len(calls) == 0 {
		return nil
	}

	return calls
}

// AgentToolCallsToJSON 将工具调用转为 JSON 字符串，超出 16MB 时使用二分查找截断
func AgentToolCallsToJSON(calls []AgentToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	return truncateJSONSlice(calls, len(calls), func(n int) ([]byte, error) {
		return json.Marshal(calls[:n])
	})
}

// AgentFilesToJSON 将文件内容转为 JSON 字符串，超出 16MB 时使用二分查找截断
func AgentFilesToJSON(files []AgentFileContent) string {
	if len(files) == 0 {
		return ""
	}
	return truncateJSONSlice(files, len(files), func(n int) ([]byte, error) {
		return json.Marshal(files[:n])
	})
}

const maxAgentJSONSize = 16 * 1024 * 1024 // 16MB

// truncateJSONSlice 使用二分查找找到不超过 maxAgentJSONSize 的最大元素数
func truncateJSONSlice[T any](_ []T, total int, marshal func(n int) ([]byte, error)) string {
	data, err := marshal(total)
	if err != nil {
		return ""
	}
	if len(data) <= maxAgentJSONSize {
		return string(data)
	}

	// 二分查找：在 [1, total) 中找最大的 n 使得序列化后 ≤ maxAgentJSONSize
	lo, hi := 1, total
	var best []byte
	for lo < hi {
		mid := (lo + hi) / 2
		data, err := marshal(mid)
		if err != nil {
			return ""
		}
		if len(data) <= maxAgentJSONSize {
			best = data
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if len(best) == 0 {
		return ""
	}
	return string(best)
}
