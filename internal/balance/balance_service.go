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
	"gorm.io/datatypes"
)

// BalanceInfo 是 models.BalanceInfo 的别名，便于本包使用
type BalanceInfo = models.BalanceInfo

// BalanceQueryResult 是 models.BalanceQueryResult 的别名，便于本包使用
type BalanceQueryResult = models.BalanceQueryResult

// BalanceService 处理不同平台的余额查询
type BalanceService struct{}

// 包级 httpClient，由 NewBalanceService 设置，供平台处理器使用
var serviceHTTPClient *http.Client

// NewBalanceService 创建新的余额查询服务
// 使用自定义 HTTP Client，支持连接池复用和超时设置
func NewBalanceService() *BalanceService {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}
	serviceHTTPClient = &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}
	return &BalanceService{}
}

// PlatformBalanceHandler 定义平台余额查询处理函数签名
type PlatformBalanceHandler func(ctx context.Context, baseURL string, apiKey string, customPath string) (*BalanceInfo, error)

// platformHandlers 注册各平台的余额查询处理器
var platformHandlers = map[string]PlatformBalanceHandler{
	"default":               handleDefaultBalance,        // 默认处理器（尝试标准 OpenAI 格式）
	"api.openai.com":        handleOpenAIBalance,         // OpenAI（使用真实 host 匹配）
	"api.siliconflow.cn":    handleSiliconFlowBalance,    // 硅基流动（国内站）
	"api.siliconflow.com":   handleSiliconFlowBalance,    // 硅基流动（国际站）
	"api.chatanywhere.org":   handleChatAnywhereBalance,   // ChatAnywhere（特殊处理）
	"api.chatanywhere.tech":  handleChatAnywhereBalance,   // ChatAnywhere（tech 域名）
	"api.chatanywhere.com.cn": handleChatAnywhereBalance,  // ChatAnywhere（国内站）
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

	// 获取所有上游 URL 并匹配正确的平台处理器
	upstreamURLs, err := getUpstreamURLsFromGroup(group.Upstreams)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to get upstream URL from group: %v", err),
		}, nil
	}

	// 遍历所有上游，优先选择有专用平台处理器的 URL；若均无专用处理器则使用第一个有效 URL 走默认处理器
	var (
		selectedURL  string
		selectedHost string
	)
	for _, u := range upstreamURLs {
		parsedURL, parseErr := url.Parse(u)
		if parseErr != nil {
			continue
		}
		host := parsedURL.Hostname()
		if _, ok := platformHandlers[host]; ok && host != "default" {
			selectedURL = u
			selectedHost = host
			break
		}
		if selectedURL == "" {
			selectedURL = u
			selectedHost = host
		}
	}

	if selectedURL == "" {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: "no valid upstream URL found",
		}, nil
	}

	// 查找对应的处理器，未匹配到专用处理器时使用默认处理器
	handler, ok := platformHandlers[selectedHost]
	if !ok {
		handler = platformHandlers["default"]
	}

	// 使用对应的处理器查询余额
	customPath := group.BalanceQueryPath
	return handler(ctx, selectedURL, apiKey.KeyValue, customPath)
}

// upstreamDef 表示分组上游配置项
type upstreamDef struct {
	URL    string `json:"url"`
	Weight int    `json:"weight"`
}

// getUpstreamURLsFromGroup 返回所有有效的上游 URL（用于多上游场景）
func getUpstreamURLsFromGroup(upstreams datatypes.JSON) ([]string, error) {
	if len(upstreams) == 0 {
		return nil, fmt.Errorf("no upstreams configured")
	}

	var defs []upstreamDef
	if err := json.Unmarshal(upstreams, &defs); err != nil {
		return nil, fmt.Errorf("failed to unmarshal upstreams: %w", err)
	}

	var urls []string
	for _, def := range defs {
		if def.URL != "" {
			urls = append(urls, def.URL)
		}
	}

	if len(urls) == 0 {
		return nil, fmt.Errorf("no valid upstream URL found")
	}
	return urls, nil
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

	resp, err := serviceHTTPClient.Do(req)
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
	// 当同时存在 total_grants 和 total_used 时，BalanceTotal = total - used（剩余可用）
	var totalGrants, totalUsed float64
	hasTotalGrants := false
	hasTotalUsed := false

	if total, ok := result["total_grants"].(float64); ok {
		totalGrants = total
		hasTotalGrants = true
	}
	if used, ok := result["total_used"].(float64); ok {
		totalUsed = used
		hasTotalUsed = true
	}

	balanceTotal := "N/A"
	balanceUsed := "N/A"

	if hasTotalGrants && hasTotalUsed {
		// 用总额减去已用，得到剩余可用余额
		balanceTotal = fmt.Sprintf("%.2f", totalGrants-totalUsed)
		balanceUsed = fmt.Sprintf("%.2f", totalUsed)
	} else if hasTotalGrants {
		balanceTotal = fmt.Sprintf("%.2f", totalGrants)
	} else if hasTotalUsed {
		balanceUsed = fmt.Sprintf("%.2f", totalUsed)
	}

	// 如果没有 total_grants，但有 remaining 字段，作为剩余值
	if remaining, ok := result["remaining"].(float64); ok && !hasTotalGrants {
		balanceTotal = fmt.Sprintf("%.2f", remaining)
	}

	// 检查是否有 balance 字段（一些平台使用此格式）
	if balance, ok := result["balance"].(float64); ok && !hasTotalGrants {
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

	resp, err := serviceHTTPClient.Do(req)
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

	resp, err := serviceHTTPClient.Do(req)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("request failed: %v", err),
		}, nil
	}
	defer resp.Body.Close()

	// 检查 HTTP 状态码
	if resp.StatusCode == http.StatusUnauthorized {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: "Key 无效 (401)",
		}, nil
	}

	if resp.StatusCode != http.StatusOK {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("HTTP %d", resp.StatusCode),
		}, nil
	}

	// 解析响应，字段为字符串类型（硅基流动 API 将数据嵌套在 data 字段中）
	// 注意：API 可能不返回根级别的 success 字段，使用指针以区分"缺失"和"false"
	var result struct {
		Success *bool `json:"success"`
		Data    struct {
			Name          string `json:"name"`
			Email         string `json:"email"`
			Balance       string `json:"balance"`
			ChargeBalance string `json:"chargeBalance"`
			TotalBalance  string `json:"totalBalance"`
			Status        string `json:"status"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to parse response: %v", err),
		}, nil
	}

	// 仅当 API 明确返回 success=false 时才报错，字段缺失时视为成功
	if result.Success != nil && !*result.Success {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: "API 返回 success=false",
		}, nil
	}

	// 处理余额值，确保显示正确的格式
	balanceTotal := result.Data.TotalBalance
	if balanceTotal == "" {
		balanceTotal = "0"
	}

	// 状态默认值
	status := result.Data.Status
	if status == "" {
		status = "unknown"
	}

	return &BalanceInfo{
		Success:      true,
		BalanceTotal: balanceTotal,
		BalanceUsed:  "N/A",
		Currency:     "CNY",
		Status:       status,
	}, nil
}

// handleChatAnywhereBalance ChatAnywhere 余额查询（特殊处理）
// baseURL 来自分组 Upstreams 配置，基于该 host 拼接余额查询地址。
func handleChatAnywhereBalance(ctx context.Context, baseURL string, apiKey string, customPath string) (*BalanceInfo, error) {
	// 余额查询统一使用 tech 域名（官方余额接口），与 Python 脚本保持一致
	balancePath := customPath
	if balancePath == "" {
		balancePath = "/v1/query/balance"
	}
	balanceURL := "https://api.chatanywhere.tech" + balancePath

	req, err := http.NewRequestWithContext(ctx, "POST", balanceURL, nil)
	if err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to create request: %v", err),
		}, nil
	}

	// 参照 ChatAnywhere 官方余额查询脚本的请求头配置
	req.Header.Set("Authorization", apiKey)
	req.Header.Set("accept", "application/json, text/plain, */*")
	req.Header.Set("accept-language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("access-control-allow-headers", "Authorization,Origin, X-Requested-With, Content-Type, Accept")
	req.Header.Set("access-control-allow-methods", "GET,POST")
	req.Header.Set("access-control-allow-origin", "*")
	req.Header.Set("cache-control", "no-cache")
	req.Header.Set("content-length", "0")
	req.Header.Set("origin", "https://api.chatanywhere.tech")
	req.Header.Set("pragma", "no-cache")
	req.Header.Set("referer", "https://api.chatanywhere.tech/")
	req.Header.Set("user-agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36")

	resp, err := serviceHTTPClient.Do(req)
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

	// 使用结构体解析，匹配 ChatAnywhere 余额查询 API 返回的字段
	var result struct {
		AdminKeyID   int     `json:"adminKeyId"`
		APIKey       string  `json:"apiKey"`
		BalanceTotal float64 `json:"balanceTotal"`
		BalanceUsed  float64 `json:"balanceUsed"`
		ID           int     `json:"id"`
		Status       int     `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return &BalanceInfo{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to parse response: %v", err),
		}, nil
	}

	// 格式化余额字段
	// balanceTotal 是总投放额度，balanceUsed 是已用额度
	// 可用余额 = 总额度 - 已用额度
	availableBalance := result.BalanceTotal - result.BalanceUsed
	if availableBalance < 0 {
		availableBalance = 0
	}
	balanceTotal := fmt.Sprintf("%.2f", availableBalance)
	balanceUsed := fmt.Sprintf("%.2f", result.BalanceUsed)
	status := fmt.Sprintf("%d", result.Status)
	adminKeyID := fmt.Sprintf("%d", result.AdminKeyID)
	id := fmt.Sprintf("%d", result.ID)

	return &BalanceInfo{
		Success:      true,
		BalanceTotal: balanceTotal,
		BalanceUsed:  balanceUsed,
		Status:       status,
		ID:           id,
		AdminKeyID:   adminKeyID,
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

	resp, err := serviceHTTPClient.Do(req)
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

	resp, err := serviceHTTPClient.Do(req)
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

	resp, err := serviceHTTPClient.Do(req)
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

	resp, err := serviceHTTPClient.Do(req)
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

	resp, err := serviceHTTPClient.Do(req)
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

	resp, err := serviceHTTPClient.Do(req)
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
			if total < 0 {
				total = 0
			}
			totalBalance += total
		}
		if used, err := parseBalance(binfo.BalanceUsed); err == nil {
			if used < 0 {
				used = 0
			}
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
		logrus.WithFields(fields).Warn("Balance query failed")
	}
}
