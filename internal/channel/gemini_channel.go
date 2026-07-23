package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/models"
	"gpt-load/internal/utils"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

func init() {
	Register("gemini", newGeminiChannel)
}

type GeminiChannel struct {
	*BaseChannel
}

func newGeminiChannel(f *Factory, group *models.Group) (ChannelProxy, error) {
	base, err := f.newBaseChannel("gemini", group)
	if err != nil {
		return nil, err
	}

	return &GeminiChannel{
		BaseChannel: base,
	}, nil
}

// ModifyRequest 为Gemini请求添加API密钥作为查询参数
func (ch *GeminiChannel) ModifyRequest(req *http.Request, apiKey *models.APIKey, group *models.Group) {
	if strings.Contains(req.URL.Path, "v1beta/openai") {
		req.Header.Set("Authorization", "Bearer "+apiKey.KeyValue)
	} else {
		q := req.URL.Query()
		q.Set("key", apiKey.KeyValue)
		req.URL.RawQuery = q.Encode()
	}
}

// IsStreamRequest 检查请求是否为流式响应
func (ch *GeminiChannel) IsStreamRequest(c *gin.Context, bodyBytes []byte) bool {
	path := c.Request.URL.Path
	if strings.HasSuffix(path, ":streamGenerateContent") {
		return true
	}

	// 同时检查标准流式标识作为备选
	if strings.Contains(c.GetHeader("Accept"), "text/event-stream") {
		return true
	}
	if c.Query("stream") == "true" {
		return true
	}

	type streamPayload struct {
		Stream bool `json:"stream"`
	}
	var p streamPayload
	if err := json.Unmarshal(bodyBytes, &p); err == nil {
		return p.Stream
	}

	return false
}

func (ch *GeminiChannel) ExtractModel(c *gin.Context, bodyBytes []byte) string {
	// gemini格式
	path := c.Request.URL.Path
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if part == "models" && i+1 < len(parts) {
			modelPart := parts[i+1]
			return strings.Split(modelPart, ":")[0]
		}
	}

	// openai格式
	type modelPayload struct {
		Model string `json:"model"`
	}
	var p modelPayload
	if err := json.Unmarshal(bodyBytes, &p); err == nil && p.Model != "" {
		return p.Model
	}

	return ""
}

// ValidateKey 通过发送generateContent请求检查给定API密钥是否有效
func (ch *GeminiChannel) ValidateKey(ctx context.Context, apiKey *models.APIKey, group *models.Group) (bool, error) {
	upstreamURL := ch.getUpstreamURL()
	if upstreamURL == nil {
		return false, fmt.Errorf("no upstream URL configured for channel %s", ch.Name)
	}

	// 安全拼接路径段
	reqURL, err := url.JoinPath(upstreamURL.String(), "v1beta", "models", ch.TestModel+":generateContent")
	if err != nil {
		return false, fmt.Errorf("failed to create gemini validation path: %w", err)
	}
	// 使用 url.Values 编码 key 参数，与 ModifyRequest 保持一致，避免含 &/=/# 的 key 截断 URL
	parsedReqURL, err := url.Parse(reqURL)
	if err != nil {
		return false, fmt.Errorf("failed to parse gemini validation URL: %w", err)
	}
	q := parsedReqURL.Query()
	q.Set("key", apiKey.KeyValue)
	parsedReqURL.RawQuery = q.Encode()
	reqURL = parsedReqURL.String()

	payload := gin.H{
		"contents": []gin.H{
			{
				"role": "user",
				"parts": []gin.H{
					{"text": "hi"},
				},
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Errorf("failed to marshal validation payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", reqURL, bytes.NewBuffer(body))
	if err != nil {
		return false, fmt.Errorf("failed to create validation request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// 应用自定义header规则（如果有）
	if len(group.HeaderRuleList) > 0 {
		headerCtx := utils.NewHeaderVariableContext(group, apiKey)
		utils.ApplyHeaderRules(req, group.HeaderRuleList, headerCtx)
	}

	resp, err := ch.GetHTTPClient().Do(req)
	if err != nil {
		return false, fmt.Errorf("failed to send validation request: %w", err)
	}
	defer resp.Body.Close()

	// 任何2xx状态码表示密钥有效
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, nil
	}

	// 对于非200响应，解析响应体以提供更具体的错误原因
	errorBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("key is invalid (status %d), but failed to read error body: %w", resp.StatusCode, err)
	}

	// 使用新解析器提取干净的错误信息
	parsedError := app_errors.ParseUpstreamError(errorBody)

	return false, fmt.Errorf("[status %d] %s", resp.StatusCode, parsedError)
}

// ApplyModelRedirect 覆盖Gemini通道的默认实现
func (ch *GeminiChannel) ApplyModelRedirect(req *http.Request, bodyBytes []byte, group *models.Group) ([]byte, error) {
	if len(group.ModelRedirectMap) == 0 {
		return bodyBytes, nil
	}

	if strings.Contains(req.URL.Path, "v1beta/openai") {
		return ch.BaseChannel.ApplyModelRedirect(req, bodyBytes, group)
	}

	return ch.applyNativeFormatRedirect(req, bodyBytes, group)
}

// applyNativeFormatRedirect 处理Gemini原生格式的模型重定向
func (ch *GeminiChannel) applyNativeFormatRedirect(req *http.Request, bodyBytes []byte, group *models.Group) ([]byte, error) {
	path := req.URL.Path
	parts := strings.Split(path, "/")

	for i, part := range parts {
		if part == "models" && i+1 < len(parts) {
			modelPart := parts[i+1]
			originalModel := strings.Split(modelPart, ":")[0]

			if targetModel, found := group.ModelRedirectMap[originalModel]; found {
				suffix := ""
				if colonIndex := strings.Index(modelPart, ":"); colonIndex != -1 {
					suffix = modelPart[colonIndex:]
				}
				parts[i+1] = targetModel + suffix
				req.URL.Path = strings.Join(parts, "/")

				logrus.WithFields(logrus.Fields{
					"group":          group.Name,
					"original_model": originalModel,
					"target_model":   targetModel,
					"channel":        "gemini_native",
					"original_path":  path,
					"new_path":       req.URL.Path,
				}).Debug("Model redirected")

				return bodyBytes, nil
			}

			if group.ModelRedirectStrict {
				return nil, fmt.Errorf("model '%s' is not configured in redirect rules", originalModel)
			}
			return bodyBytes, nil
		}
	}

	return bodyBytes, nil
}

// TransformModelList 根据重定向规则转换模型列表响应
func (ch *GeminiChannel) TransformModelList(req *http.Request, bodyBytes []byte, group *models.Group) (map[string]any, error) {
	var response map[string]any
	if err := json.Unmarshal(bodyBytes, &response); err != nil {
		logrus.WithError(err).Debug("Failed to parse model list response, returning empty")
		return nil, err
	}

	if modelsInterface, hasModels := response["models"]; hasModels {
		return ch.transformGeminiNativeFormat(req, response, modelsInterface, group), nil
	}

	if _, hasData := response["data"]; hasData {
		return ch.BaseChannel.TransformModelList(req, bodyBytes, group)
	}

	return response, nil
}

// transformGeminiNativeFormat 转换Gemini原生格式的模型列表
func (ch *GeminiChannel) transformGeminiNativeFormat(req *http.Request, response map[string]any, modelsInterface any, group *models.Group) map[string]any {
	upstreamModels, ok := modelsInterface.([]any)
	if !ok {
		return response
	}

	configuredModels := buildConfiguredGeminiModels(group.ModelRedirectMap)

	// 严格模式：仅返回已配置的模型（白名单）
	if group.ModelRedirectStrict {
		response["models"] = configuredModels
		delete(response, "nextPageToken")

		logrus.WithFields(logrus.Fields{
			"group":       group.Name,
			"model_count": len(configuredModels),
			"strict_mode": true,
			"format":      "gemini_native",
		}).Debug("Model list returned (strict mode - configured models only)")

		return response
	}

	// 非严格模式：合并上游+已配置的模型（上游优先）
	var merged []any
	if isFirstPage(req) {
		merged = mergeGeminiModelLists(upstreamModels, configuredModels)
		logrus.WithFields(logrus.Fields{
			"group":            group.Name,
			"upstream_count":   len(upstreamModels),
			"configured_count": len(configuredModels),
			"merged_count":     len(merged),
			"strict_mode":      false,
			"format":           "gemini_native",
			"page":             "first",
		}).Debug("Model list merged (non-strict mode - first page)")
	} else {
		merged = upstreamModels
		logrus.WithFields(logrus.Fields{
			"group":          group.Name,
			"upstream_count": len(upstreamModels),
			"strict_mode":    false,
			"format":         "gemini_native",
			"page":           "subsequent",
		}).Debug("Model list returned (non-strict mode - subsequent page)")
	}

	response["models"] = merged
	return response
}

// buildConfiguredGeminiModels 根据重定向规则构建Gemini格式的模型列表
func buildConfiguredGeminiModels(redirectMap map[string]string) []any {
	if len(redirectMap) == 0 {
		return []any{}
	}

	models := make([]any, 0, len(redirectMap))
	for sourceModel := range redirectMap {
		modelName := sourceModel
		if !strings.HasPrefix(sourceModel, "models/") {
			modelName = "models/" + sourceModel
		}

		models = append(models, map[string]any{
			"name":                       modelName,
			"displayName":                sourceModel,
			"supportedGenerationMethods": []string{"generateContent"},
		})
	}
	return models
}

// mergeGeminiModelLists 合并上游和已配置的模型列表（Gemini格式）
func mergeGeminiModelLists(upstream []any, configured []any) []any {
	upstreamNames := make(map[string]bool)
	for _, item := range upstream {
		if modelObj, ok := item.(map[string]any); ok {
			if modelName, ok := modelObj["name"].(string); ok {
				upstreamNames[modelName] = true
				cleanName := strings.TrimPrefix(modelName, "models/")
				upstreamNames[cleanName] = true
			}
		}
	}

	// 以上游所有模型开始
	result := make([]any, len(upstream))
	copy(result, upstream)

	// 添加上游中不存在的已配置模型
	for _, item := range configured {
		if modelObj, ok := item.(map[string]any); ok {
			if modelName, ok := modelObj["name"].(string); ok {
				cleanName := strings.TrimPrefix(modelName, "models/")
				if !upstreamNames[modelName] && !upstreamNames[cleanName] {
					result = append(result, item)
				}
			}
		}
	}

	return result
}

// isFirstPage 检查是否为Gemini分页请求的第一页
func isFirstPage(req *http.Request) bool {
	pageToken := req.URL.Query().Get("pageToken")
	return pageToken == ""
}
