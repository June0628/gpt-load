package balance

import (
	"context"
	"encoding/json"
	"fmt"
	"gpt-load/internal/models"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// BalanceInfo 是 models.BalanceInfo 的别名，便于本包使用
type BalanceInfo = models.BalanceInfo

// BalanceQueryResult 是 models.BalanceQueryResult 的别名，便于本包使用
type BalanceQueryResult = models.BalanceQueryResult

// BalanceService 处理不同平台的余额查询
type BalanceService struct {
	HTTPClient *http.Client
}

// NewBalanceService 创建新的余额查询服务
func NewBalanceService() *BalanceService {
	return &BalanceService{
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// PlatformBalanceHandler 定义平台余额查询处理函数签名
type PlatformBalanceHandler func(ctx context.Context, baseURL string, apiKey string, customPath string) (*BalanceInfo, error)

// platformHandlers 注册各平台的余额查询处理器
var platformHandlers = map[string]PlatformBalanceHandler{
	"default":               handleDefaultBalance,        // 默认处理器（尝试标准 OpenAI 格式）
	"openai":                handleOpenAIBalance,         // OpenAI
	"api.siliconflow.cn":    handleSiliconFlowBalance,    // 硅基流动
	"api.chatanywhere.org":  handleChatAnywhereBalance,   // ChatAnywhere（特殊处理）
	"api.deepseek.com":      handleDeepSeekBalance,       // DeepSeek
	"api.moonshot.cn":       handleMoonshotBalance,       // 月之暗面
	"api.baichuan-ai.com":   handleBaichuanBalance,       // 百川智能
	"api.minimax.chat":      handleMiniMaxBalance,        // MiniMax
	"api.sparkai.com":       handleSparkBalance,          // 讯飞星火
	"api.zhipuai.cn":        handleZhipuBalance,          // 智谱 AI
	"dashscope.aliyuncs.com":handleDashScopeBalance,      // 通义千问
	"api.volcengine.com":    handleVolcEngineBalance,     // 火山引擎
}

// QueryBalance 查询单个密钥的余额
func (s *BalanceService) QueryBalance(ctx context.Context, group *models.Group, apiKey *models.APIKey) (*BalanceInfo, error) {
	if apiKey == nil {
		return nil, fmt.Errorf("api key is nil")
	}

	// 解析上游 URL 获取域名
	upstreamURL := group.EffectiveConfig.AppUrl
	parsedURL, err := url.Parse(upstreamURL)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to parse upstream URL: %v", err),
		}, nil
	}

	host := parsedURL.Host

	// 查找对应的处理器
	handler, ok := platformHandlers[host]
	if !ok {
		// 如果没有找到特定处理器，使用默认处理器
		handler = platformHandlers["default"]
	}

	// 使用对应的处理器查询余额
	customPath := group.BalanceQueryPath
	return handler(ctx, upstreamURL, apiKey.KeyValue, customPath)
}

// handleDefaultBalance 默认余额查询处理器（尝试标准 OpenAI 格式）
func handleDefaultBalance(ctx context.Context, baseURL string, apiKey string, customPath string) (*BalanceInfo, error) {
	// 如果指定了自定义路径，使用自定义路径
	balancePath := customPath
	if balancePath == "" {
		balancePath = "/v1/dashboard/billing/credit_grants"
	}

	reqURL := strings.TrimRight(baseURL, "/") + balancePath

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to create request: %v", err),
		}, nil
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("request failed: %v", err),
		}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("HTTP %d", resp.StatusCode),
		}, nil
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to parse response: %v", err),
		}, nil
	}

	// 尝试解析标准 OpenAI 格式
	balanceTotal := "N/A"
	balanceUsed := "N/A"

	if total, ok := result["total_grants"].(float64); ok {
		balanceTotal = fmt.Sprintf("%.2f", total)
	}
	if used, ok := result["total_used"].(float64); ok {
		balanceUsed = fmt.Sprintf("%.2f", used)
	}
	if remaining, ok := result["remaining"].(float64); ok {
		balanceTotal = fmt.Sprintf("%.2f", remaining)
	}

	// 检查是否有 balance 字段（一些平台使用此格式）
	if balance, ok := result["balance"].(float64); ok {
		balanceTotal = fmt.Sprintf("%.2f", balance)
	}

	return &BalanceInfo{
		Success:      true,
		BalanceTotal: balanceTotal,
		BalanceUsed:  balanceUsed,
		Currency:     "USD",
	}, nil
}

// handleOpenAIBalance OpenAI 余额查询
func handleOpenAIBalance(ctx context.Context, baseURL string, apiKey string, customPath string) (*BalanceInfo, error) {
	balancePath := customPath
	if balancePath == "" {
		balancePath = "/v1/dashboard/billing/credit_grants"
	}

	reqURL := strings.TrimRight(baseURL, "/") + balancePath

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to create request: %v", err),
		}, nil
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("request failed: %v", err),
		}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("HTTP %d", resp.StatusCode),
		}, nil
	}

	var result struct {
		TotalGranted float64 `json:"total_granted"`
		TotalUsed    float64 `json:"total_used"`
		TotalAvailable float64 `json:"total_available"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to parse response: %v", err),
		}, nil
	}

	balanceTotal := fmt.Sprintf("%.2f", result.TotalGranted)
	balanceUsed := fmt.Sprintf("%.2f", result.TotalUsed)
	if result.TotalAvailable > 0 {
		balanceTotal = fmt.Sprintf("%.2f", result.TotalAvailable)
	}

	return &BalanceInfo{
		Success:      true,
		BalanceTotal: balanceTotal,
		BalanceUsed:  balanceUsed,
		Currency:     "USD",
	}, nil
}

// handleSiliconFlowBalance 硅基流动余额查询
func handleSiliconFlowBalance(ctx context.Context, baseURL string, apiKey string, customPath string) (*BalanceInfo, error) {
	balancePath := customPath
	if balancePath == "" {
		balancePath = "/v1/user/info"
	}

	reqURL := strings.TrimRight(baseURL, "/") + balancePath

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to create request: %v", err),
		}, nil
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("request failed: %v", err),
		}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("HTTP %d", resp.StatusCode),
		}, nil
	}

	var result struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			ID             string  `json:"id"`
			Balance        float64 `json:"balance"`
			Status         string  `json:"status"`
			CreditBalance  float64 `json:"creditBalance"`
			CashBalance    float64 `json:"cashBalance"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to parse response: %v", err),
		}, nil
	}

	if result.Code != 0 {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: result.Message,
		}, nil
	}

	balanceTotal := fmt.Sprintf("%.2f", result.Data.Balance)
	if result.Data.CreditBalance > 0 || result.Data.CashBalance > 0 {
		balanceTotal = fmt.Sprintf("%.2f", result.Data.CreditBalance+result.Data.CashBalance)
	}

	return &BalanceInfo{
		Success:      true,
		BalanceTotal: balanceTotal,
		BalanceUsed:  "N/A",
		Status:       result.Data.Status,
		ID:           result.Data.ID,
		Currency:     "CNY",
	}, nil
}

// handleChatAnywhereBalance ChatAnywhere 余额查询（特殊处理）
func handleChatAnywhereBalance(ctx context.Context, baseURL string, apiKey string, customPath string) (*BalanceInfo, error) {
	// ChatAnywhere 使用不同的域名进行余额查询
	balanceURL := "https://api.chatanywhere.tech/v1/query/balance"

	req, err := http.NewRequestWithContext(ctx, "GET", balanceURL, nil)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to create request: %v", err),
		}, nil
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("request failed: %v", err),
		}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("HTTP %d", resp.StatusCode),
		}, nil
	}

	var result struct {
		AdminKeyId   string      `json:"adminKeyId"`
		ApiKey       string      `json:"apiKey"`
		BalanceTotal interface{} `json:"balanceTotal"`
		BalanceUsed  interface{} `json:"balanceUsed"`
		ID           string      `json:"id"`
		Status       string      `json:"status"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to parse response: %v", err),
		}, nil
	}

	balanceTotal := fmt.Sprintf("%v", result.BalanceTotal)
	balanceUsed := fmt.Sprintf("%v", result.BalanceUsed)
	if result.BalanceTotal == nil {
		balanceTotal = "N/A"
	}
	if result.BalanceUsed == nil {
		balanceUsed = "N/A"
	}

	return &BalanceInfo{
		Success:      true,
		BalanceTotal: balanceTotal,
		BalanceUsed:  balanceUsed,
		Status:       result.Status,
		ID:           result.ID,
		AdminKeyID:   result.AdminKeyId,
		Currency:     "USD",
	}, nil
}

// handleDeepSeekBalance DeepSeek 余额查询
func handleDeepSeekBalance(ctx context.Context, baseURL string, apiKey string, customPath string) (*BalanceInfo, error) {
	balancePath := customPath
	if balancePath == "" {
		balancePath = "/user/balance"
	}

	reqURL := strings.TrimRight(baseURL, "/") + balancePath

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to create request: %v", err),
		}, nil
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("request failed: %v", err),
		}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("HTTP %d", resp.StatusCode),
		}, nil
	}

	var result struct {
		Success bool `json:"success"`
		Data    struct {
			Balance     float64 `json:"balance"`
			Currency    string  `json:"currency"`
			GrantBalance float64 `json:"grant_balance"`
			CashBalance float64 `json:"cash_balance"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to parse response: %v", err),
		}, nil
	}

	balanceTotal := fmt.Sprintf("%.2f", result.Data.Balance)
	if result.Data.GrantBalance > 0 || result.Data.CashBalance > 0 {
		balanceTotal = fmt.Sprintf("%.2f", result.Data.GrantBalance+result.Data.CashBalance)
	}

	return &BalanceInfo{
		Success:      true,
		BalanceTotal: balanceTotal,
		BalanceUsed:  "N/A",
		Currency:     result.Data.Currency,
	}, nil
}

// handleMoonshotBalance 月之暗面余额查询
func handleMoonshotBalance(ctx context.Context, baseURL string, apiKey string, customPath string) (*BalanceInfo, error) {
	balancePath := customPath
	if balancePath == "" {
		balancePath = "/v1/users/me"
	}

	reqURL := strings.TrimRight(baseURL, "/") + balancePath

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to create request: %v", err),
		}, nil
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("request failed: %v", err),
		}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("HTTP %d", resp.StatusCode),
		}, nil
	}

	var result struct {
		Data struct {
			ID             string  `json:"id"`
			TotalBalance   float64 `json:"total_balance"`
			GrantedBalance float64 `json:"granted_balance"`
			CashBalance    float64 `json:"cash_balance"`
			Status         string  `json:"status"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to parse response: %v", err),
		}, nil
	}

	balanceTotal := fmt.Sprintf("%.2f", result.Data.TotalBalance)

	return &BalanceInfo{
		Success:      true,
		BalanceTotal: balanceTotal,
		BalanceUsed:  "N/A",
		Status:       result.Data.Status,
		ID:           result.Data.ID,
		Currency:     "CNY",
	}, nil
}

// handleBaichuanBalance 百川智能余额查询
func handleBaichuanBalance(ctx context.Context, baseURL string, apiKey string, customPath string) (*BalanceInfo, error) {
	balancePath := customPath
	if balancePath == "" {
		balancePath = "/v1/account/balance"
	}

	reqURL := strings.TrimRight(baseURL, "/") + balancePath

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to create request: %v", err),
		}, nil
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("request failed: %v", err),
		}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("HTTP %d", resp.StatusCode),
		}, nil
	}

	var result struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			TotalBalance float64 `json:"total_balance"`
			CashBalance  float64 `json:"cash_balance"`
			Currency     string  `json:"currency"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to parse response: %v", err),
		}, nil
	}

	if result.Code != 0 {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: result.Message,
		}, nil
	}

	balanceTotal := fmt.Sprintf("%.2f", result.Data.TotalBalance)

	return &BalanceInfo{
		Success:      true,
		BalanceTotal: balanceTotal,
		BalanceUsed:  "N/A",
		Currency:     result.Data.Currency,
	}, nil
}

// handleMiniMaxBalance MiniMax 余额查询
func handleMiniMaxBalance(ctx context.Context, baseURL string, apiKey string, customPath string) (*BalanceInfo, error) {
	balancePath := customPath
	if balancePath == "" {
		balancePath = "/v1/account/get_balance"
	}

	reqURL := strings.TrimRight(baseURL, "/") + balancePath

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to create request: %v", err),
		}, nil
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("request failed: %v", err),
		}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("HTTP %d", resp.StatusCode),
		}, nil
	}

	var result struct {
		Balance float64 `json:"balance"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to parse response: %v", err),
		}, nil
	}

	return &BalanceInfo{
		Success:      true,
		BalanceTotal: fmt.Sprintf("%.2f", result.Balance),
		BalanceUsed:  "N/A",
		Currency:     "CNY",
	}, nil
}

// handleSparkBalance 讯飞星火余额查询
func handleSparkBalance(ctx context.Context, baseURL string, apiKey string, customPath string) (*BalanceInfo, error) {
	// 讯飞使用不同的认证方式，这里使用默认处理器
	return handleDefaultBalance(ctx, baseURL, apiKey, customPath)
}

// handleZhipuBalance 智谱 AI 余额查询
func handleZhipuBalance(ctx context.Context, baseURL string, apiKey string, customPath string) (*BalanceInfo, error) {
	balancePath := customPath
	if balancePath == "" {
		balancePath = "/api/paas/v4/balance"
	}

	reqURL := strings.TrimRight(baseURL, "/") + balancePath

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to create request: %v", err),
		}, nil
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("request failed: %v", err),
		}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("HTTP %d", resp.StatusCode),
		}, nil
	}

	var result struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			TotalBalance float64 `json:"totalBalance"`
			CashBalance  float64 `json:"cashBalance"`
			GrantedBalance float64 `json:"grantedBalance"`
			Currency     string  `json:"currency"`
			Status       int     `json:"status"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to parse response: %v", err),
		}, nil
	}

	if result.Code != 200 {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: result.Message,
		}, nil
	}

	balanceTotal := fmt.Sprintf("%.2f", result.Data.TotalBalance)
	status := "active"
	if result.Data.Status != 1 {
		status = "inactive"
	}

	return &BalanceInfo{
		Success:      true,
		BalanceTotal: balanceTotal,
		BalanceUsed:  "N/A",
		Status:       status,
		Currency:     result.Data.Currency,
	}, nil
}

// handleDashScopeBalance 通义千问余额查询
func handleDashScopeBalance(ctx context.Context, baseURL string, apiKey string, customPath string) (*BalanceInfo, error) {
	balancePath := customPath
	if balancePath == "" {
		balancePath = "/api/v1/account/balance"
	}

	reqURL := strings.TrimRight(baseURL, "/") + balancePath

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to create request: %v", err),
		}, nil
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-DashScope-SPL", "enable")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("request failed: %v", err),
		}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("HTTP %d", resp.StatusCode),
		}, nil
	}

	var result struct {
		Code string `json:"code"`
		Data struct {
			AvailableCredit string `json:"available_credit"`
			Currency        string `json:"currency"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to parse response: %v", err),
		}, nil
	}

	if result.Code != "Success" {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: result.Code,
		}, nil
	}

	return &BalanceInfo{
		Success:      true,
		BalanceTotal: result.Data.AvailableCredit,
		BalanceUsed:  "N/A",
		Currency:     result.Data.Currency,
	}, nil
}

// handleVolcEngineBalance 火山引擎余额查询
func handleVolcEngineBalance(ctx context.Context, baseURL string, apiKey string, customPath string) (*BalanceInfo, error) {
	// 火山引擎使用复杂的签名认证，使用默认处理器
	return handleDefaultBalance(ctx, baseURL, apiKey, customPath)
}

// FormatBalanceInfo 将余额信息格式化为单行字符串，便于写入文件和日志
func FormatBalanceInfo(binfo *BalanceInfo) string {
	if binfo == nil || !binfo.Success {
		if binfo != nil {
			return fmt.Sprintf("余额查询失败：%s", binfo.ErrorMessage)
		}
		return "余额查询失败：unknown error"
	}

	parts := []string{
		fmt.Sprintf("余额总量：%s", binfo.BalanceTotal),
		fmt.Sprintf("已用：%s", binfo.BalanceUsed),
	}

	if binfo.Currency != "" {
		parts = append(parts, fmt.Sprintf("货币：%s", binfo.Currency))
	}
	if binfo.Status != "" {
		parts = append(parts, fmt.Sprintf("状态：%s", binfo.Status))
	}
	if binfo.ID != "" {
		parts = append(parts, fmt.Sprintf("ID: %s", binfo.ID))
	}
	if binfo.AdminKeyID != "" {
		parts = append(parts, fmt.Sprintf("AdminKeyID: %s", binfo.AdminKeyID))
	}

	return strings.Join(parts, " | ")
}

// AggregateBalanceInfo 聚合多个余额信息，计算总额
func AggregateBalanceInfo(balanceInfos []*BalanceInfo) *BalanceInfo {
	if len(balanceInfos) == 0 {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: "no balance info to aggregate",
		}
	}

	var totalBalance float64
	var totalUsed float64
	var currency string
	successCount := 0
	failCount := 0
	var errors []string

	for _, binfo := range balanceInfos {
		if binfo == nil || !binfo.Success {
			failCount++
			if binfo != nil && binfo.ErrorMessage != "" {
				errors = append(errors, binfo.ErrorMessage)
			}
			continue
		}

		successCount++
		if currency == "" {
			currency = binfo.Currency
		}

		// 解析余额数值
		if total, err := parseBalance(binfo.BalanceTotal); err == nil {
			totalBalance += total
		}
		if used, err := parseBalance(binfo.BalanceUsed); err == nil {
			totalUsed += used
		}
	}

	if successCount == 0 {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("all queries failed: %s", strings.Join(errors, "; ")),
		}
	}

	return &BalanceInfo{
		Success:      true,
		BalanceTotal: fmt.Sprintf("%.2f", totalBalance),
		BalanceUsed:  fmt.Sprintf("%.2f", totalUsed),
		Currency:     currency,
	}
}

// parseBalance 解析余额字符串为浮点数
func parseBalance(s string) (float64, error) {
	if s == "" || s == "N/A" {
		return 0, fmt.Errorf("invalid balance string")
	}
	var result float64
	_, err := fmt.Sscanf(s, "%f", &result)
	return result, err
}

// LogBalanceQueryResult 记录余额查询结果
func LogBalanceQueryResult(apiKey *models.APIKey, balanceInfo *BalanceInfo, groupName string) {
	if balanceInfo == nil {
		return
	}

	fields := logrus.Fields{
		"group":       groupName,
		"key_id":      apiKey.ID,
		"success":     balanceInfo.Success,
	}

	if balanceInfo.Success {
		fields["balance_total"] = balanceInfo.BalanceTotal
		fields["balance_used"] = balanceInfo.BalanceUsed
		fields["currency"] = balanceInfo.Currency
		logrus.WithFields(fields).Info("Balance query successful")
	} else {
		fields["error"] = balanceInfo.ErrorMessage
		logrus.WithFields(fields).Debug("Balance query failed")
	}
}
