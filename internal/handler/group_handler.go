// Package handler 提供应用程序的 HTTP 处理器
package handler

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"

	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/i18n"
	"gpt-load/internal/models"
	"gpt-load/internal/response"
	"gpt-load/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"gorm.io/datatypes"
)

func (s *Server) handleGroupError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}

	if svcErr, ok := err.(*services.I18nError); ok {
		if svcErr.Template != nil {
			response.ErrorI18nFromAPIError(c, svcErr.APIError, svcErr.MessageID, svcErr.Template)
		} else {
			response.ErrorI18nFromAPIError(c, svcErr.APIError, svcErr.MessageID)
		}
		return true
	}

	if apiErr, ok := err.(*app_errors.APIError); ok {
		response.Error(c, apiErr)
		return true
	}

	logrus.WithContext(c.Request.Context()).WithError(err).Error("unexpected group service error")
	response.Error(c, app_errors.ErrInternalServer)
	return true
}

// GroupCreateRequest 定义创建分组的请求参数。
type GroupCreateRequest struct {
	Name                string                     `json:"name"`
	DisplayName         string                     `json:"display_name"`
	Description         string                     `json:"description"`
	GroupType           string                     `json:"group_type"` // 'standard' or 'aggregate'
	Upstreams           json.RawMessage            `json:"upstreams"`
	ChannelType         string                     `json:"channel_type"`
	Sort                int                        `json:"sort"`
	TestModel           string                     `json:"test_model"`
	ValidationEndpoint  string                     `json:"validation_endpoint"`
	ParamOverrides      map[string]any             `json:"param_overrides"`
	ModelRedirectRules  map[string]string          `json:"model_redirect_rules"`
	ModelRedirectStrict bool                       `json:"model_redirect_strict"`
	Config              map[string]any             `json:"config"`
	HeaderRules         []models.HeaderRule        `json:"header_rules"`
	ProxyKeys           string                     `json:"proxy_keys"`
	BalanceQueryConfig  *BalanceQueryConfigRequest `json:"balance_query_config,omitempty"`
	KeyNeverExpires     bool                       `json:"key_never_expires"`
	DailyRequestLimit   int                        `json:"daily_request_limit"`
}

// CreateGroup 处理创建新分组。
func (s *Server) CreateGroup(c *gin.Context) {
	var req GroupCreateRequest
	if !bindJSON(c, &req) {
		return
	}

	params := services.GroupCreateParams{
		Name:                req.Name,
		DisplayName:         req.DisplayName,
		Description:         req.Description,
		GroupType:           req.GroupType,
		Upstreams:           req.Upstreams,
		ChannelType:         req.ChannelType,
		Sort:                req.Sort,
		TestModel:           req.TestModel,
		ValidationEndpoint:  req.ValidationEndpoint,
		ParamOverrides:      req.ParamOverrides,
		ModelRedirectRules:  req.ModelRedirectRules,
		ModelRedirectStrict: req.ModelRedirectStrict,
		Config:              req.Config,
		HeaderRules:         req.HeaderRules,
		ProxyKeys:           req.ProxyKeys,
		KeyNeverExpires:     req.KeyNeverExpires,
		DailyRequestLimit:   req.DailyRequestLimit,
	}

	if req.BalanceQueryConfig != nil {
		params.BalanceQueryConfig = &services.BalanceQueryConfigParams{
			Enabled:          req.BalanceQueryConfig.Enabled,
			AggregateBalance: req.BalanceQueryConfig.AggregateBalance,
		}
	}

	group, err := s.GroupService.CreateGroup(c.Request.Context(), params)
	if s.handleGroupError(c, err) {
		return
	}

	response.Success(c, s.newGroupResponse(c, group))
}

// ListGroups 处理获取所有分组列表。
func (s *Server) ListGroups(c *gin.Context) {
	groups, err := s.GroupService.ListGroups(c.Request.Context())
	if s.handleGroupError(c, err) {
		return
	}

	groupResponses := make([]GroupResponse, 0, len(groups))
	for i := range groups {
		groupResponses = append(groupResponses, *s.newGroupResponse(c, &groups[i]))
	}

	response.Success(c, groupResponses)
}

// BalanceQueryConfigRequest 定义分组的余额查询配置。
type BalanceQueryConfigRequest struct {
	Enabled          bool `json:"enabled"`
	AggregateBalance bool `json:"aggregate_balance"`
}

// GroupUpdateRequest 定义更新分组的请求参数，使用独立结构体避免零值被 GORM Update 忽略。
type GroupUpdateRequest struct {
	Name                *string                `json:"name,omitempty"`
	DisplayName         *string                `json:"display_name,omitempty"`
	Description         *string                `json:"description,omitempty"`
	GroupType           *string                `json:"group_type,omitempty"`
	Upstreams           json.RawMessage        `json:"upstreams"`
	ChannelType         *string                `json:"channel_type,omitempty"`
	Sort                *int                   `json:"sort"`
	TestModel           string                 `json:"test_model"`
	ValidationEndpoint  *string                `json:"validation_endpoint,omitempty"`
	ParamOverrides      map[string]any         `json:"param_overrides"`
	ModelRedirectRules  map[string]string      `json:"model_redirect_rules"`
	ModelRedirectStrict *bool                  `json:"model_redirect_strict"`
	Config              map[string]any         `json:"config"`
	HeaderRules         []models.HeaderRule    `json:"header_rules"`
	ProxyKeys           *string                `json:"proxy_keys,omitempty"`
	BalanceQueryConfig  *BalanceQueryConfigRequest `json:"balance_query_config,omitempty"`
	KeyNeverExpires     *bool                  `json:"key_never_expires,omitempty"`
	DailyRequestLimit   *int                   `json:"daily_request_limit,omitempty"`
}

type GroupReorderItemRequest struct {
	ID   uint `json:"id"`
	Sort int  `json:"sort"`
}

type GroupReorderRequest struct {
	Items []GroupReorderItemRequest `json:"items"`
}

func validateGroupReorderItems(items []GroupReorderItemRequest) error {
	if len(items) == 0 {
		return services.NewI18nError(app_errors.ErrValidation, "validation.reorder_items_required", nil)
	}

	seen := make(map[uint]struct{}, len(items))
	for _, item := range items {
		if item.ID == 0 {
			return services.NewI18nError(app_errors.ErrValidation, "validation.reorder_group_id", nil)
		}
		if item.Sort < 0 {
			return services.NewI18nError(app_errors.ErrValidation, "validation.reorder_sort_negative", nil)
		}
		if _, exists := seen[item.ID]; exists {
			return services.NewI18nError(app_errors.ErrValidation, "validation.reorder_duplicate_group", map[string]any{"id": item.ID})
		}
		seen[item.ID] = struct{}{}
	}

	return nil
}

// UpdateGroup 处理更新已有分组。
func (s *Server) UpdateGroup(c *gin.Context) {
	id, ok := parseGroupIDParam(c)
	if !ok {
		return
	}

	var req GroupUpdateRequest
	if !bindJSON(c, &req) {
		return
	}

	params := services.GroupUpdateParams{
		Name:                req.Name,
		DisplayName:         req.DisplayName,
		Description:         req.Description,
		GroupType:           req.GroupType,
		ChannelType:         req.ChannelType,
		Sort:                req.Sort,
		ValidationEndpoint:  req.ValidationEndpoint,
		ParamOverrides:      req.ParamOverrides,
		ModelRedirectRules:  req.ModelRedirectRules,
		ModelRedirectStrict: req.ModelRedirectStrict,
		Config:              req.Config,
		ProxyKeys:           req.ProxyKeys,
	}

	if req.Upstreams != nil {
		params.Upstreams = req.Upstreams
		params.HasUpstreams = true
	}

	if req.TestModel != "" {
		params.TestModel = req.TestModel
		params.HasTestModel = true
	}

	if req.HeaderRules != nil {
		rules := req.HeaderRules
		params.HeaderRules = &rules
	}

	if req.BalanceQueryConfig != nil {
		params.BalanceQueryConfig = &services.BalanceQueryConfigParams{
			Enabled:          req.BalanceQueryConfig.Enabled,
			AggregateBalance: req.BalanceQueryConfig.AggregateBalance,
		}
	}

	// 添加密钥失效配置参数
	params.KeyNeverExpires = req.KeyNeverExpires
	params.DailyRequestLimit = req.DailyRequestLimit

	group, err := s.GroupService.UpdateGroup(c.Request.Context(), id, params)
	if s.handleGroupError(c, err) {
		return
	}

	response.Success(c, s.newGroupResponse(c, group))
}

// ReorderGroups 处理分组的批量排序更新。
func (s *Server) ReorderGroups(c *gin.Context) {
	var req GroupReorderRequest
	if !bindJSON(c, &req) {
		return
	}

	if err := validateGroupReorderItems(req.Items); s.handleGroupError(c, err) {
		return
	}

	items := make([]services.GroupReorderItem, 0, len(req.Items))
	for _, item := range req.Items {
		items = append(items, services.GroupReorderItem{
			ID:   item.ID,
			Sort: item.Sort,
		})
	}

	if s.handleGroupError(c, s.GroupService.ReorderGroups(c.Request.Context(), items)) {
		return
	}

	response.SuccessI18n(c, "success.groups_reordered", nil)
}

// BalanceQueryConfigResponse 定义 API 响应中的余额查询配置。
type BalanceQueryConfigResponse struct {
	Enabled          bool `json:"enabled"`
	AggregateBalance bool `json:"aggregate_balance"`
}

// GroupBalanceInfoResponse 定义 API 响应中的分组余额信息结构。
type GroupBalanceInfoResponse struct {
	TotalKeys     int64  `json:"total_keys"`
	SuccessCount  int64  `json:"success_count"`
	FailCount     int64  `json:"fail_count"`
	TotalBalance  string `json:"total_balance"`
	TotalUsed     string `json:"total_used"`
	Currency      string `json:"currency"`
	LastUpdatedAt string `json:"last_updated_at,omitempty"`
}

// GroupResponse 定义分组响应的结构，排除敏感或大型字段。
type GroupResponse struct {
	ID                  uint                        `json:"id"`
	Name                string                      `json:"name"`
	Endpoint            string                      `json:"endpoint"`
	DisplayName         string                      `json:"display_name"`
	Description         string                      `json:"description"`
	GroupType           string                      `json:"group_type"`
	Upstreams           datatypes.JSON              `json:"upstreams"`
	ChannelType         string                      `json:"channel_type"`
	Sort                int                         `json:"sort"`
	TestModel           string                      `json:"test_model"`
	ValidationEndpoint  string                      `json:"validation_endpoint"`
	ParamOverrides      datatypes.JSONMap           `json:"param_overrides"`
	ModelRedirectRules  datatypes.JSONMap           `json:"model_redirect_rules"`
	ModelRedirectStrict bool                        `json:"model_redirect_strict"`
	Config              datatypes.JSONMap           `json:"config"`
	HeaderRules         []models.HeaderRule         `json:"header_rules"`
	ProxyKeys           string                      `json:"proxy_keys"`
	BalanceQueryConfig  *BalanceQueryConfigResponse `json:"balance_query_config"`
	BalanceInfo         *GroupBalanceInfoResponse   `json:"balance_info,omitempty"`
	KeyNeverExpires     bool                        `json:"key_never_expires"`
	DailyRequestLimit   int                         `json:"daily_request_limit"`
	LastValidatedAt     *time.Time                  `json:"last_validated_at"`
	CreatedAt           time.Time                   `json:"created_at"`
	UpdatedAt           time.Time                   `json:"updated_at"`
}

// aggregateGroupBalance 聚合分组下所有 API 密钥的余额信息
// 只聚合 active 状态的密钥
func (s *Server) aggregateGroupBalance(ctx *gin.Context, group *models.Group) *GroupBalanceInfoResponse {
	if group.GroupType == "aggregate" {
		return nil
	}

	var apiKeys []models.APIKey
	if err := s.DB.WithContext(ctx.Request.Context()).Where("group_id = ? AND status = ?", group.ID, models.KeyStatusActive).Find(&apiKeys).Error; err != nil {
		logrus.WithContext(ctx.Request.Context()).WithError(err).Error("Failed to fetch API keys for balance aggregation")
		return nil
	}

	if len(apiKeys) == 0 {
		return nil
	}

	var totalBalance float64
	var totalUsed float64
	var currency string
	var successCount int64
	var failCount int64
	var lastUpdatedAt string

	for _, key := range apiKeys {
		if key.BalanceTotal != "" && key.BalanceTotal != "N/A" {
			if balance, err := strconv.ParseFloat(key.BalanceTotal, 64); err == nil {
				if balance < 0 {
					balance = 0
				}
				totalBalance += balance
				successCount++
			} else {
				failCount++
			}
		} else {
			failCount++
		}
		if key.BalanceUsed != "" && key.BalanceUsed != "N/A" {
			if used, err := strconv.ParseFloat(key.BalanceUsed, 64); err == nil {
				if used < 0 {
					used = 0
				}
				totalUsed += used
			}
		}
	}

	if successCount == 0 && failCount == 0 {
		return nil
	}

	// 查找最近更新的密钥及其币种
	for _, key := range apiKeys {
		// 只考虑 UpdatedAt 非零（已查询过）的密钥
		if !key.UpdatedAt.IsZero() {
			if lastUpdatedAt == "" || key.UpdatedAt.Format(time.RFC3339) > lastUpdatedAt {
				lastUpdatedAt = key.UpdatedAt.Format(time.RFC3339)
			}
			// 从成功查询的密钥中获取币种
			if currency == "" && key.BalanceTotal != "" && key.BalanceTotal != "N/A" {
				// 根据上游 host 推断币种，国内平台返回 CNY
				currency = s.inferCurrencyFromGroup(group)
			}
		}
	}

	return &GroupBalanceInfoResponse{
		TotalKeys:     int64(len(apiKeys)),
		SuccessCount:  successCount,
		FailCount:     failCount,
		TotalBalance:  strconv.FormatFloat(totalBalance, 'f', 2, 64),
		TotalUsed:     strconv.FormatFloat(totalUsed, 'f', 2, 64),
		Currency:      currency,
		LastUpdatedAt: lastUpdatedAt,
	}
}

// newGroupResponse 从 models.Group 创建新的 GroupResponse。
func (s *Server) newGroupResponse(ctx *gin.Context, group *models.Group) *GroupResponse {
	appURL := s.SettingsManager.GetAppUrl()
	endpoint := ""
	if appURL != "" {
		u, err := url.Parse(appURL)
		if err == nil {
			u.Path = strings.TrimRight(u.Path, "/") + "/proxy/" + group.Name
			endpoint = u.String()
		}
	}

	// 从 JSON 解析 header 规则
	var headerRules []models.HeaderRule
	if len(group.HeaderRules) > 0 {
		if err := json.Unmarshal(group.HeaderRules, &headerRules); err != nil {
			logrus.WithError(err).Error("Failed to unmarshal header rules")
			headerRules = make([]models.HeaderRule, 0)
		}
	}

	// 如果启用了余额查询则聚合余额信息
	var balanceInfo *GroupBalanceInfoResponse
	if group.EnableBalanceQuery {
		balanceInfo = s.aggregateGroupBalance(ctx, group)
	}

	return &GroupResponse{
		ID:                  group.ID,
		Name:                group.Name,
		Endpoint:            endpoint,
		DisplayName:         group.DisplayName,
		Description:         group.Description,
		GroupType:           group.GroupType,
		Upstreams:           group.Upstreams,
		ChannelType:         group.ChannelType,
		Sort:                group.Sort,
		TestModel:           group.TestModel,
		ValidationEndpoint:  group.ValidationEndpoint,
		ParamOverrides:      group.ParamOverrides,
		ModelRedirectRules:  group.ModelRedirectRules,
		ModelRedirectStrict: group.ModelRedirectStrict,
		Config:              group.Config,
		HeaderRules:         headerRules,
		ProxyKeys:           group.ProxyKeys,
		BalanceQueryConfig: &BalanceQueryConfigResponse{
			Enabled:          group.EnableBalanceQuery,
			AggregateBalance: group.AggregateBalance,
		},
		BalanceInfo:       balanceInfo,
		KeyNeverExpires:   group.KeyNeverExpires,
		DailyRequestLimit: group.DailyRequestLimit,
		LastValidatedAt:   group.LastValidatedAt,
		CreatedAt:         group.CreatedAt,
		UpdatedAt:         group.UpdatedAt,
	}
}

// DeleteGroup 处理删除分组。
func (s *Server) DeleteGroup(c *gin.Context) {
	id, ok := parseGroupIDParam(c)
	if !ok {
		return
	}

	if s.handleGroupError(c, s.GroupService.DeleteGroup(c.Request.Context(), id)) {
		return
	}
	response.SuccessI18n(c, "success.group_deleted", nil)
}

// ConfigOption 表示分组的单个可配置选项。
type ConfigOption struct {
	Key          string `json:"key"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	DefaultValue any    `json:"default_value"`
}

// GetGroupConfigOptions 返回分组可用的配置选项列表。
func (s *Server) GetGroupConfigOptions(c *gin.Context) {
	options, err := s.GroupService.GetGroupConfigOptions()
	if s.handleGroupError(c, err) {
		return
	}

	translated := make([]ConfigOption, 0, len(options))
	for _, option := range options {
		name := option.Name
		if strings.HasPrefix(name, "config.") {
			name = i18n.Message(c, name)
		}
		description := option.Description
		if strings.HasPrefix(description, "config.") {
			description = i18n.Message(c, description)
		}

		translated = append(translated, ConfigOption{
			Key:          option.Key,
			Name:         name,
			Description:  description,
			DefaultValue: option.DefaultValue,
		})
	}

	response.Success(c, translated)
}

// inferCurrencyFromGroup 根据分组的上游 URL 推断币种
// 根据分组上游 URL 推断币种，国内平台返回 CNY
func (s *Server) inferCurrencyFromGroup(group *models.Group) string {
	if len(group.Upstreams) == 0 {
		return "USD"
	}
	var defs []struct {
		URL    string `json:"url"`
		Weight int    `json:"weight"`
	}
	if err := json.Unmarshal(group.Upstreams, &defs); err != nil {
		return "USD"
	}
	// 国内平台 host 集合，命中则返回 CNY
	domesticHosts := map[string]bool{
		"api.deepseek.com":       true,
		"api.moonshot.cn":        true,
		"api.baichuan-ai.com":    true,
		"api.zhipuai.cn":         true,
		"dashscope.aliyuncs.com": true,
		"api.siliconflow.cn":     true,
		"api.minimax.chat":       true,
		"api.sparkai.com":        true,
		"api.volcengine.com":     true,
		"api.chatanywhere.com.cn": true,
	}
	for _, def := range defs {
		if def.URL == "" {
			continue
		}
		parsedURL, err := url.Parse(def.URL)
		if err != nil {
			continue
		}
		if domesticHosts[parsedURL.Hostname()] {
			return "CNY"
		}
	}
	return "USD"
}

func (s *Server) GetGroupStats(c *gin.Context) {
	id, ok := parseGroupIDParam(c)
	if !ok {
		return
	}

	stats, err := s.GroupService.GetGroupStats(c.Request.Context(), id)
	if s.handleGroupError(c, err) {
		return
	}

	response.Success(c, stats)
}

// GroupCopyRequest 定义复制分组的请求参数。
type GroupCopyRequest struct {
	CopyKeys string `json:"copy_keys"` // "none"|"valid_only"|"all"
}

// GroupCopyResponse 定义分组复制操作的响应。
type GroupCopyResponse struct {
	Group *GroupResponse `json:"group"`
}

// CopyGroup 处理复制分组及可选内容。

func (s *Server) CopyGroup(c *gin.Context) {
	id, ok := parseGroupIDParam(c)
	if !ok {
		return
	}

	var req GroupCopyRequest
	if !bindJSON(c, &req) {
		return
	}

	newGroup, err := s.GroupService.CopyGroup(c.Request.Context(), id, req.CopyKeys)
	if s.handleGroupError(c, err) {
		return
	}

	groupResponse := s.newGroupResponse(c, newGroup)
	copyResponse := &GroupCopyResponse{
		Group: groupResponse,
	}

	response.Success(c, copyResponse)
}

// List 获取分组列表
func (s *Server) List(c *gin.Context) {
	var groups []models.Group
	if err := s.DB.Select("id, name,display_name").Find(&groups).Error; err != nil {
		response.ErrorI18nFromAPIError(c, app_errors.ErrDatabase, "database.cannot_get_groups")
		return
	}
	response.Success(c, groups)
}

// AddSubGroupsRequest 定义向聚合分组添加子分组的请求参数
type AddSubGroupsRequest struct {
	SubGroups []services.SubGroupInput `json:"sub_groups"`
}

// UpdateSubGroupWeightRequest 定义更新子分组权重的请求参数
type UpdateSubGroupWeightRequest struct {
	Weight int `json:"weight"`
}

// GetSubGroups 处理获取聚合分组的子分组
func (s *Server) GetSubGroups(c *gin.Context) {
	id, ok := parseGroupIDParam(c)
	if !ok {
		return
	}

	subGroups, err := s.AggregateGroupService.GetSubGroups(c.Request.Context(), id)
	if s.handleGroupError(c, err) {
		return
	}

	response.Success(c, subGroups)
}

// AddSubGroups 处理向聚合分组添加子分组
func (s *Server) AddSubGroups(c *gin.Context) {
	id, ok := parseGroupIDParam(c)
	if !ok {
		return
	}

	var req AddSubGroupsRequest
	if !bindJSON(c, &req) {
		return
	}

	if err := s.AggregateGroupService.AddSubGroups(c.Request.Context(), id, req.SubGroups); s.handleGroupError(c, err) {
		return
	}

	response.SuccessI18n(c, "success.sub_groups_added", nil)
}

// UpdateSubGroupWeight 处理更新子分组的权重
func (s *Server) UpdateSubGroupWeight(c *gin.Context) {
	id, ok := parseGroupIDParam(c)
	if !ok {
		return
	}

	subGroupID, ok := parseSubGroupIDParam(c)
	if !ok {
		return
	}

	var req UpdateSubGroupWeightRequest
	if !bindJSON(c, &req) {
		return
	}

	if err := s.AggregateGroupService.UpdateSubGroupWeight(c.Request.Context(), id, subGroupID, req.Weight); s.handleGroupError(c, err) {
		return
	}

	response.SuccessI18n(c, "success.sub_group_weight_updated", nil)
}

// DeleteSubGroup 处理从聚合分组中删除子分组
func (s *Server) DeleteSubGroup(c *gin.Context) {
	id, ok := parseGroupIDParam(c)
	if !ok {
		return
	}

	subGroupID, ok := parseSubGroupIDParam(c)
	if !ok {
		return
	}

	if err := s.AggregateGroupService.DeleteSubGroup(c.Request.Context(), id, subGroupID); s.handleGroupError(c, err) {
		return
	}

	response.SuccessI18n(c, "success.sub_group_deleted", nil)
}

// GetParentAggregateGroups 处理获取引用指定分组的父聚合分组
func (s *Server) GetParentAggregateGroups(c *gin.Context) {
	id, ok := parseGroupIDParam(c)
	if !ok {
		return
	}

	parentGroups, err := s.AggregateGroupService.GetParentAggregateGroups(c.Request.Context(), id)
	if s.handleGroupError(c, err) {
		return
	}

	response.Success(c, parentGroups)
}

// GetBalanceHistory 获取分组的余额查询历史记录
// 支持按 key_id 过滤，按时间倒序排列，分页返回
func (s *Server) GetBalanceHistory(c *gin.Context) {
	groupID, ok := parseGroupIDParam(c)
	if !ok {
		return
	}

	query := s.DB.WithContext(c.Request.Context()).
		Model(&models.BalanceHistory{}).
		Where("group_id = ?", groupID).
		Order("queried_at DESC")

	// 可选：按密钥 ID 过滤
	if keyIDStr := c.Query("key_id"); keyIDStr != "" {
		if keyID, err := strconv.Atoi(keyIDStr); err == nil && keyID > 0 {
			query = query.Where("key_id = ?", keyID)
		}
	}

	var histories []models.BalanceHistory
	result, err := response.Paginate(c, query, &histories)
	if err != nil {
		logrus.WithContext(c.Request.Context()).WithError(err).Error("Failed to fetch balance history")
		response.Error(c, app_errors.ErrInternalServer)
		return
	}

	response.Success(c, result)
}
