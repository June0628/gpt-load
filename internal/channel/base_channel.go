package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/httpclient"
	"gpt-load/internal/models"
	"gpt-load/internal/types"
	"gpt-load/internal/utils"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"gorm.io/datatypes"
)

// UpstreamInfo 保存单个上游服务器的信息，包括权重
type UpstreamInfo struct {
	URL           *url.URL
	Weight        int
	CurrentWeight int
}

// BaseChannel 提供通道代理的通用功能
type BaseChannel struct {
	Name               string
	Upstreams          []UpstreamInfo
	HTTPClient         *httpclient.ProxyClientPool
	StreamClient       *httpclient.ProxyClientPool
	TestModel          string
	ValidationEndpoint string
	upstreamLock       sync.Mutex

	// 从分组缓存的字段用于过期检查
	channelType         string
	groupUpstreams      datatypes.JSON
	effectiveConfig     *types.SystemSettings
	modelRedirectRules  datatypes.JSONMap
	modelRedirectStrict bool
}

// getUpstreamURL 使用平滑加权轮询算法选择上游URL
func (b *BaseChannel) getUpstreamURL() *url.URL {
	b.upstreamLock.Lock()
	defer b.upstreamLock.Unlock()

	if len(b.Upstreams) == 0 {
		return nil
	}
	if len(b.Upstreams) == 1 {
		return b.Upstreams[0].URL
	}

	totalWeight := 0
	var best *UpstreamInfo

	for i := range b.Upstreams {
		up := &b.Upstreams[i]
		totalWeight += up.Weight
		up.CurrentWeight += up.Weight

		if best == nil || up.CurrentWeight > best.CurrentWeight {
			best = up
		}
	}

	if best == nil {
		return b.Upstreams[0].URL // 降级到第一个可用的
	}

	best.CurrentWeight -= totalWeight
	return best.URL
}

// BuildUpstreamURL 构建上游服务的目标URL
func (b *BaseChannel) BuildUpstreamURL(originalURL *url.URL, groupName string) (string, error) {
	base := b.getUpstreamURL()
	if base == nil {
		return "", fmt.Errorf("no upstream URL configured for channel %s", b.Name)
	}

	finalURL := *base
	proxyPrefix := "/proxy/" + groupName
	requestPath := originalURL.Path
	requestPath = strings.TrimPrefix(requestPath, proxyPrefix)

	finalURL.Path = strings.TrimRight(finalURL.Path, "/") + requestPath

	finalURL.RawQuery = originalURL.RawQuery

	return finalURL.String(), nil
}

// IsStreamRequest 通过请求头、查询参数和请求体中的 stream 字段判断是否为流式请求
func (b *BaseChannel) IsStreamRequest(c *gin.Context, bodyBytes []byte) bool {
	if strings.Contains(c.GetHeader("Accept"), "text/event-stream") {
		return true
	}

	if c.Query("stream") == "true" {
		return true
	}

	var p struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(bodyBytes, &p); err == nil {
		return p.Stream
	}

	return false
}

// ExtractModel 从请求体的 model 字段中提取模型名称
func (b *BaseChannel) ExtractModel(c *gin.Context, bodyBytes []byte) string {
	var p struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(bodyBytes, &p); err == nil {
		return p.Model
	}
	return ""
}

// buildValidationURL 将验证端点的路径和查询参数拼接到上游 URL 上
func (b *BaseChannel) buildValidationURL() (string, error) {
	upstreamURL := b.getUpstreamURL()
	if upstreamURL == nil {
		return "", fmt.Errorf("no upstream URL configured for channel %s", b.Name)
	}

	endpointURL, err := url.Parse(b.ValidationEndpoint)
	if err != nil {
		return "", fmt.Errorf("failed to parse validation endpoint: %w", err)
	}

	finalURL := *upstreamURL
	finalURL.Path = strings.TrimRight(finalURL.Path, "/") + endpointURL.Path
	finalURL.RawQuery = endpointURL.RawQuery
	return finalURL.String(), nil
}

// newValidationRequest 构建带 JSON 载荷的验证请求，并应用分组的自定义 header 规则
func (b *BaseChannel) newValidationRequest(ctx context.Context, reqURL string, payload any, apiKey *models.APIKey, group *models.Group) (*http.Request, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal validation payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create validation request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if len(group.HeaderRuleList) > 0 {
		headerCtx := utils.NewHeaderVariableContext(group, apiKey)
		utils.ApplyHeaderRules(req, group.HeaderRuleList, headerCtx)
	}

	return req, nil
}

// doValidationRequest 发送验证请求，2xx 表示密钥有效，否则解析上游错误信息
func (b *BaseChannel) doValidationRequest(req *http.Request) (bool, error) {
	resp, err := b.GetHTTPClient().Do(req)
	if err != nil {
		return false, fmt.Errorf("failed to send validation request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, nil
	}

	errorBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("key is invalid (status %d), but failed to read error body: %w", resp.StatusCode, err)
	}

	return false, fmt.Errorf("[status %d] %s", resp.StatusCode, app_errors.ParseUpstreamError(errorBody))
}

// validateKeyWithPayload 使用验证端点和给定载荷校验密钥有效性
func (b *BaseChannel) validateKeyWithPayload(ctx context.Context, apiKey *models.APIKey, group *models.Group, payload any, setAuth func(*http.Request)) (bool, error) {
	reqURL, err := b.buildValidationURL()
	if err != nil {
		return false, err
	}

	req, err := b.newValidationRequest(ctx, reqURL, payload, apiKey, group)
	if err != nil {
		return false, err
	}
	if setAuth != nil {
		setAuth(req)
	}

	return b.doValidationRequest(req)
}

// IsConfigStale 检查通道配置是否与提供的分组相比已过期
func (b *BaseChannel) IsConfigStale(group *models.Group) bool {
	if b.channelType != group.ChannelType {
		return true
	}
	if b.TestModel != group.TestModel {
		return true
	}
	if b.ValidationEndpoint != utils.GetValidationEndpoint(group) {
		return true
	}
	if !bytes.Equal(b.groupUpstreams, group.Upstreams) {
		return true
	}
	if !reflect.DeepEqual(b.effectiveConfig, &group.EffectiveConfig) {
		return true
	}
	// 检查模型重定向规则变更
	if !reflect.DeepEqual(b.modelRedirectRules, group.ModelRedirectRules) {
		return true
	}
	if b.modelRedirectStrict != group.ModelRedirectStrict {
		return true
	}
	return false
}

// GetHTTPClient 返回标准请求的客户端（从代理池中轮询选择）
func (b *BaseChannel) GetHTTPClient() *http.Client {
	return b.HTTPClient.GetClient()
}

// GetStreamClient 返回流式请求的客户端（从代理池中轮询选择）
func (b *BaseChannel) GetStreamClient() *http.Client {
	return b.StreamClient.GetClient()
}

// ApplyModelRedirect 根据分组的重定向规则应用模型重定向
func (b *BaseChannel) ApplyModelRedirect(req *http.Request, bodyBytes []byte, group *models.Group) ([]byte, error) {
	if len(group.ModelRedirectMap) == 0 || len(bodyBytes) == 0 {
		return bodyBytes, nil
	}

	var requestData map[string]any
	if err := json.Unmarshal(bodyBytes, &requestData); err != nil {
		return bodyBytes, nil
	}

	modelValue, exists := requestData["model"]
	if !exists {
		return bodyBytes, nil
	}

	model, ok := modelValue.(string)
	if !ok {
		return bodyBytes, nil
	}

	// 直接匹配，无需前缀处理
	if targetModel, found := group.ModelRedirectMap[model]; found {
		requestData["model"] = targetModel

		// 记录重定向用于审计
		logrus.WithFields(logrus.Fields{
			"group":          group.Name,
			"original_model": model,
			"target_model":   targetModel,
			"channel":        "json_body",
		}).Debug("Model redirected")

		return json.Marshal(requestData)
	}

	if group.ModelRedirectStrict {
		return nil, fmt.Errorf("model '%s' is not configured in redirect rules", model)
	}

	return bodyBytes, nil
}

// TransformModelList 根据重定向规则转换模型列表响应
func (b *BaseChannel) TransformModelList(req *http.Request, bodyBytes []byte, group *models.Group) (map[string]any, error) {
	var response map[string]any
	if err := json.Unmarshal(bodyBytes, &response); err != nil {
		logrus.WithError(err).Debug("Failed to parse model list response, returning empty")
		return nil, err
	}

	dataInterface, exists := response["data"]
	if !exists {
		return response, nil
	}

	upstreamModels, ok := dataInterface.([]any)
	if !ok {
		return response, nil
	}

	// 构建已配置的源模型列表（两种模式的通用逻辑）
	configuredModels := buildConfiguredModels(group.ModelRedirectMap)

	// 严格模式：仅返回已配置的模型（白名单）
	if group.ModelRedirectStrict {
		response["data"] = configuredModels

		logrus.WithFields(logrus.Fields{
			"group":       group.Name,
			"model_count": len(configuredModels),
			"strict_mode": true,
		}).Debug("Model list returned (strict mode - configured models only)")

		return response, nil
	}

	// 非严格模式：合并上游+已配置的模型（上游优先）
	merged := mergeModelLists(upstreamModels, configuredModels)
	response["data"] = merged

	logrus.WithFields(logrus.Fields{
		"group":            group.Name,
		"upstream_count":   len(upstreamModels),
		"configured_count": len(configuredModels),
		"merged_count":     len(merged),
		"strict_mode":      false,
	}).Debug("Model list merged (non-strict mode)")

	return response, nil
}

// GetBalanceQueryPath 返回通道的余额查询路径
func (b *BaseChannel) GetBalanceQueryPath(group *models.Group) string {
	if group.BalanceQueryPath != "" {
		return group.BalanceQueryPath
	}
	// 默认返回空字符串，由余额服务根据平台决定使用哪个路径
	return ""
}

// buildConfiguredModels 根据重定向规则构建模型列表
func buildConfiguredModels(redirectMap map[string]string) []any {
	if len(redirectMap) == 0 {
		return []any{}
	}

	models := make([]any, 0, len(redirectMap))
	for sourceModel := range redirectMap {
		models = append(models, map[string]any{
			"id":       sourceModel,
			"object":   "model",
			"created":  0,
			"owned_by": "system",
		})
	}
	return models
}

// mergeModelLists 合并上游和已配置的模型列表
func mergeModelLists(upstream []any, configured []any) []any {
	// 创建上游模型ID集合
	upstreamIDs := make(map[string]bool)
	for _, item := range upstream {
		if modelObj, ok := item.(map[string]any); ok {
			if modelID, ok := modelObj["id"].(string); ok {
				upstreamIDs[modelID] = true
			}
		}
	}

	// 以上游所有模型开始
	result := make([]any, len(upstream))
	copy(result, upstream)

	// 添加上游中不存在的已配置模型
	for _, item := range configured {
		if modelObj, ok := item.(map[string]any); ok {
			if modelID, ok := modelObj["id"].(string); ok {
				if !upstreamIDs[modelID] {
					result = append(result, item)
				}
			}
		}
	}

	return result
}
