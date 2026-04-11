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

// ExtractAgentFiles 从OpenAI兼容的请求体中提取文件内容
// 支持的格式：
// 1. messages[].content[].type == "image_url" -> 提取 base64 图片数据
// 2. messages[].content[].type == "file" -> 提取文件内容
// 3. 其他可能的文件类型
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
	}
}

// extractBase64FromURL 从URL中提取base64数据
// 支持格式: data:image/png;base64,iVBORw0KGgo...
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

// AgentFilesToJSON 将提取的文件内容转换为JSON字符串用于存储
func AgentFilesToJSON(files []AgentFileContent) string {
	if len(files) == 0 {
		return ""
	}

	data, err := json.Marshal(files)
	if err != nil {
		return ""
	}

	return string(data)
}
