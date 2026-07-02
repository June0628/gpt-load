// Package proxy 提供高性能 OpenAI 多密钥代理服务器
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gpt-load/internal/channel"
	"gpt-load/internal/config"
	"gpt-load/internal/encryption"
	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/keypool"
	"gpt-load/internal/models"
	"gpt-load/internal/response"
	"gpt-load/internal/services"
	"gpt-load/internal/utils"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// ProxyServer 代理服务器
type ProxyServer struct {
	keyProvider       *keypool.KeyProvider
	groupManager      *services.GroupManager
	subGroupManager   *services.SubGroupManager
	settingsManager   *config.SystemSettingsManager
	channelFactory    *channel.Factory
	requestLogService *services.RequestLogService
	encryptionSvc     encryption.Service
}

// NewProxyServer 创建新的代理服务器
func NewProxyServer(
	keyProvider *keypool.KeyProvider,
	groupManager *services.GroupManager,
	subGroupManager *services.SubGroupManager,
	settingsManager *config.SystemSettingsManager,
	channelFactory *channel.Factory,
	requestLogService *services.RequestLogService,
	encryptionSvc encryption.Service,
) (*ProxyServer, error) {
	return &ProxyServer{
		keyProvider:       keyProvider,
		groupManager:      groupManager,
		subGroupManager:   subGroupManager,
		settingsManager:   settingsManager,
		channelFactory:    channelFactory,
		requestLogService: requestLogService,
		encryptionSvc:     encryptionSvc,
	}, nil
}

// HandleProxy 代理请求入口
func (ps *ProxyServer) HandleProxy(c *gin.Context) {
	startTime := time.Now()
	groupName := c.Param("group_name")

	originalGroup, err := ps.groupManager.GetGroupByName(groupName)
	if err != nil {
		response.Error(c, app_errors.ParseDBError(err))
		return
	}

	// 如果是聚合组则选择子组
	subGroupName, err := ps.subGroupManager.SelectSubGroup(originalGroup)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"aggregate_group": originalGroup.Name,
			"error":           err,
		}).Error("Failed to select sub-group from aggregate")
		response.Error(c, app_errors.NewAPIError(app_errors.ErrNoKeysAvailable, "No available sub-groups"))
		return
	}

	group := originalGroup
	if subGroupName != "" {
		group, err = ps.groupManager.GetGroupByName(subGroupName)
		if err != nil {
			response.Error(c, app_errors.ParseDBError(err))
			return
		}
	}

	channelHandler, err := ps.channelFactory.GetChannel(group)
	if err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInternalServer, fmt.Sprintf("Failed to get channel for group '%s': %v", groupName, err)))
		return
	}

	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		logrus.Errorf("Failed to read request body: %v", err)
		response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, "Failed to read request body"))
		return
	}
	c.Request.Body.Close()

	finalBodyBytes, err := ps.applyParamOverrides(bodyBytes, group)
	if err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInternalServer, fmt.Sprintf("Failed to apply parameter overrides: %v", err)))
		return
	}

	isStream := channelHandler.IsStreamRequest(c, finalBodyBytes)

	ps.executeRequestWithRetry(c, channelHandler, originalGroup, group, finalBodyBytes, isStream, startTime, 0)
}

// executeRequestWithRetry 用 for 循环处理请求和重试，避免递归栈溢出
func (ps *ProxyServer) executeRequestWithRetry(
	c *gin.Context,
	channelHandler channel.ChannelProxy,
	originalGroup *models.Group,
	group *models.Group,
	bodyBytes []byte,
	isStream bool,
	startTime time.Time,
	_ int,
) {
	cfg := group.EffectiveConfig
	maxRetries := cfg.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}

	// 获取模型名称（用于魔塔平台模型维度限流）
	modelName := ""
	if channelHandler != nil {
		modelName = channelHandler.ExtractModel(c, bodyBytes)
	}

	// dailyLimitSkips 记录因每日限制跳过的次数，不消耗主重试次数
	dailyLimitSkips := 0

	for retryCount := 0; retryCount <= maxRetries; retryCount++ {
		// 先构建上游 URL，用于判断是否魔塔平台
		upstreamURL, err := channelHandler.BuildUpstreamURL(c.Request.URL, originalGroup.Name)
		if err != nil {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrInternalServer, fmt.Sprintf("Failed to build upstream URL: %v", err)))
			return
		}

		// 选择密钥：魔塔平台需要检查模型维度限流
		var apiKey *models.APIKey
		if keypool.IsModelScopeUpstream(upstreamURL) && modelName != "" {
			apiKey, err = ps.keyProvider.SelectKeyWithModelCheck(group.ID, upstreamURL, modelName, maxRetries)
		} else {
			apiKey, err = ps.keyProvider.SelectKey(group.ID)
		}

		if err != nil {
			logrus.Errorf("Failed to select a key for group %s on attempt %d: %v", group.Name, retryCount+1, err)
			response.Error(c, app_errors.NewAPIError(app_errors.ErrNoKeysAvailable, err.Error()))
			ps.logRequest(c, originalGroup, group, nil, startTime, http.StatusServiceUnavailable, err, isStream, "", channelHandler, bodyBytes, models.RequestTypeFinal, "")
			return
		}

		// 检查密钥是否已达到每日请求限制
		if ps.keyProvider.CheckDailyRequestLimit(apiKey, group) {
			// 密钥已达每日限制，循环重试选择下一个密钥
			logrus.WithFields(logrus.Fields{
				"group": group.Name,
				"keyID": apiKey.ID,
			}).Debug("Key reached daily request limit, trying next key")
			dailyLimitSkips++
			if dailyLimitSkips <= maxRetries {
				retryCount-- // 不消耗主重试次数
				continue
			}
			logrus.Errorf("All keys in group %s have reached daily request limit", group.Name)
			response.Error(c, app_errors.NewAPIError(app_errors.ErrNoKeysAvailable, "所有密钥已达到每日请求限制"))
			ps.logRequest(c, originalGroup, group, nil, startTime, http.StatusServiceUnavailable, errors.New("daily request limit reached"), isStream, "", channelHandler, bodyBytes, models.RequestTypeFinal, "")
			return
		}

		var ctx context.Context
		var cancel context.CancelFunc
		if isStream {
			ctx, cancel = context.WithCancel(c.Request.Context())
		} else {
			timeout := time.Duration(cfg.RequestTimeout) * time.Second
			ctx, cancel = context.WithTimeout(c.Request.Context(), timeout)
		}

		req, err := http.NewRequestWithContext(ctx, c.Request.Method, upstreamURL, bytes.NewReader(bodyBytes))
		if err != nil {
			cancel()
			logrus.Errorf("Failed to create upstream request: %v", err)
			response.Error(c, app_errors.ErrInternalServer)
			return
		}
		req.ContentLength = int64(len(bodyBytes))

		req.Header = c.Request.Header.Clone()

		// 删除 hop-by-hop headers，这些不应该转发给上游
		// 先读取 Connection header 中列出的自定义 headers，再删除 Connection
		if connHeaders := req.Header.Get("Connection"); connHeaders != "" {
			for _, h := range strings.Split(connHeaders, ",") {
				req.Header.Del(strings.TrimSpace(h))
			}
		}
		hopByHopHeaders := []string{
			"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
			"Te", "Trailers", "Transfer-Encoding", "Upgrade",
		}
		for _, h := range hopByHopHeaders {
			req.Header.Del(h)
		}

		// 清除客户端认证密钥
		req.Header.Del("Authorization")
		req.Header.Del("X-Api-Key")
		req.Header.Del("X-Goog-Api-Key")

		// 应用模型重定向
		finalBodyBytes, err := channelHandler.ApplyModelRedirect(req, bodyBytes, group)
		if err != nil {
			cancel()
			response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, err.Error()))
			ps.logRequest(c, originalGroup, group, apiKey, startTime, http.StatusBadRequest, err, isStream, upstreamURL, channelHandler, bodyBytes, models.RequestTypeFinal, "")
			return
		}

		// 如果请求体被重定向修改则更新
		if !bytes.Equal(finalBodyBytes, bodyBytes) {
			req.Body = io.NopCloser(bytes.NewReader(finalBodyBytes))
			req.ContentLength = int64(len(finalBodyBytes))
		}

		channelHandler.ModifyRequest(req, apiKey, group)

		// 应用自定义头部规则
		if len(group.HeaderRuleList) > 0 {
			headerCtx := utils.NewHeaderVariableContextFromGin(c, group, apiKey)
			utils.ApplyHeaderRules(req, group.HeaderRuleList, headerCtx)
		}

		var client *http.Client
		if isStream {
			client = channelHandler.GetStreamClient()
			req.Header.Set("X-Accel-Buffering", "no")
		} else {
			client = channelHandler.GetHTTPClient()
		}

		resp, err := client.Do(req)

		// 统一的重试错误处理，重试策略由 group.FailoverStatusCodeMatcher 定义
		shouldRetryByStatus := resp != nil && shouldFailoverOnStatusCode(resp.StatusCode, group)
		if err != nil || shouldRetryByStatus {
			if err != nil && app_errors.IsIgnorableError(err) {
				cancel()
				if resp != nil {
					resp.Body.Close()
				}
				logrus.Debugf("Client-side ignorable error for key %s, aborting retries: %v", utils.MaskAPIKey(apiKey.KeyValue), err)
				ps.logRequest(c, originalGroup, group, apiKey, startTime, 499, err, isStream, upstreamURL, channelHandler, bodyBytes, models.RequestTypeFinal, "")
				return
			}

			var statusCode int
			var errorMessage string
			var parsedError string

		if err != nil {
			// client.Do 返回非 nil err 时仍可能返回非 nil resp（如重定向错误），需关闭 Body 避免 TCP 连接无法复用
			if resp != nil {
				resp.Body.Close()
			}
			statusCode = 500
			errorMessage = err.Error()
			parsedError = errorMessage
			logrus.Debugf("Request failed (attempt %d/%d) for key %s: %v", retryCount+1, maxRetries+1, utils.MaskAPIKey(apiKey.KeyValue), err)
		} else {
				// 可重试的上游响应（HTTP状态码匹配故障转移策略）
				statusCode = resp.StatusCode
				errorBody, readErr := io.ReadAll(resp.Body)
				resp.Body.Close()
				if readErr != nil {
					logrus.Errorf("Failed to read error body: %v", readErr)
					errorBody = []byte("Failed to read error body")
				}

				errorBody = handleGzipCompression(resp, errorBody)
				errorMessage = string(errorBody)
				parsedError = app_errors.ParseUpstreamError(errorBody)
				logrus.Debugf("Request failed with status %d (attempt %d/%d) for key %s. Parsed Error: %s", statusCode, retryCount+1, maxRetries+1, utils.MaskAPIKey(apiKey.KeyValue), parsedError)
			}

			// 上游 key 可能出现在错误文本中（如 Gemini 通道将 key 放入 URL query，
			// 传输层错误会把完整 URL 带入 err.Error()），返回客户端和落库前先脱敏
			errorMessage = utils.RedactSecret(errorMessage, apiKey.KeyValue)
			parsedError = utils.RedactSecret(parsedError, apiKey.KeyValue)

			// 魔塔平台 429 需要区分两种错误：
			// 1. "exceeded your current quota, please check your plan and billing details" -> 永久禁用密钥
			// 2. "exceeded today's quota for model X" -> 今日禁用该模型，不触发密钥故障计数
			if keypool.IsModelScopeUpstream(upstreamURL) && statusCode == http.StatusTooManyRequests {
				parsedErrorLower := strings.ToLower(parsedError)
				if strings.Contains(parsedErrorLower, "exceeded your current quota, please check your plan and billing details") {
					// 账户整体配额耗尽，永久禁用密钥
					logrus.WithFields(logrus.Fields{
						"keyID": apiKey.ID,
						"model": modelName,
					}).Warn("ModelScope key quota exhausted, disabling key permanently")
					ps.keyProvider.UpdateStatus(apiKey, group, false, parsedError)
				} else if strings.Contains(parsedErrorLower, "exceeded today's quota for model") {
					// 单个模型当日配额耗尽，将该模型剩余次数设为 0，不触发密钥故障计数
					logrus.WithFields(logrus.Fields{
						"keyID": apiKey.ID,
						"model": modelName,
					}).Debug("ModelScope model daily quota exhausted, setting remaining to 0")
					ps.keyProvider.UpdateModelScopeRemaining(apiKey, modelName, "0")
				} else {
					// 其他 429，跳过故障计数
					logrus.WithFields(logrus.Fields{
						"keyID": apiKey.ID,
						"model": modelName,
					}).Debug("ModelScope 429 rate limit, skipping key failure count")
				}
			} else {
				ps.keyProvider.UpdateStatus(apiKey, group, false, parsedError)
			}

			// 判断是否为最后一次尝试
			isLastAttempt := retryCount >= maxRetries
			requestType := models.RequestTypeRetry
			if isLastAttempt {
				requestType = models.RequestTypeFinal
			}

			ps.logRequest(c, originalGroup, group, apiKey, startTime, statusCode, errors.New(parsedError), isStream, upstreamURL, channelHandler, finalBodyBytes, requestType, "")

			// 如果是最后一次尝试，直接返回错误
			if isLastAttempt {
				cancel()
				var errorJSON map[string]any
				if err := json.Unmarshal([]byte(errorMessage), &errorJSON); err == nil {
					c.JSON(statusCode, errorJSON)
				} else {
					response.Error(c, app_errors.NewAPIErrorWithUpstream(statusCode, "UPSTREAM_ERROR", errorMessage))
				}
				return
			}

			cancel()
			continue
		}

		// 成功：此处 resp 保证非空（shouldRetryByStatus 已检查 resp != nil）
		// 注意：resp.Body 由各 handler 自行关闭，避免双重关闭

		logrus.Debugf("Request for group %s succeeded on attempt %d with key %s", group.Name, retryCount+1, utils.MaskAPIKey(apiKey.KeyValue))

		// 检查 aihubmix 渠道的滥用响应（HTTP 200 但内容为免费资源滥用提示）
		// 未充值账户超过免费试用限制时，会返回 200 但内容为提示文本，需屏蔽并当作错误处理
		if isAihubmixAbuseResponse(upstreamURL, resp) {
			resp.Body.Close()
			abuseError := "aihubmix: free resource abuse detected, please recharge your account"

			logrus.WithFields(logrus.Fields{
				"group": group.Name,
				"keyID": apiKey.ID,
			}).Warn("Aihubmix abuse response detected, retrying without failure count")

			// 更新密钥状态，让 keyProvider 后续轮换掉这个 key
			ps.keyProvider.UpdateStatus(apiKey, group, false, abuseError)

			isLastAttempt := retryCount >= maxRetries
			requestType := models.RequestTypeRetry
			if isLastAttempt {
				requestType = models.RequestTypeFinal
			}

			ps.logRequest(c, originalGroup, group, apiKey, startTime, http.StatusForbidden, errors.New(abuseError), isStream, upstreamURL, channelHandler, bodyBytes, requestType, "")

			if isLastAttempt {
				cancel()
				response.Error(c, app_errors.NewAPIErrorWithUpstream(http.StatusForbidden, "UPSTREAM_ERROR", abuseError))
				return
			}

			cancel()
			continue
		}

		// 增加每日请求计数
		ps.keyProvider.IncrementDailyRequestCount(apiKey, group)

		// 魔塔平台：从响应头更新模型维度剩余次数
		if keypool.IsModelScopeUpstream(upstreamURL) && modelName != "" {
			if remaining := resp.Header.Get(keypool.ModelScopeHeaderRemaining); remaining != "" {
				ps.keyProvider.UpdateModelScopeRemaining(apiKey, modelName, remaining)
			}
		}

		var responseBody string
		// 检查是否为模型列表请求（需要特殊处理）
		if shouldInterceptModelList(c.Request.URL.Path, c.Request.Method) {
			defer cancel()
			ps.handleModelListResponse(c, resp, group, channelHandler)
		} else {
			for key, values := range resp.Header {
				for _, value := range values {
					c.Header(key, value)
				}
			}
			c.Status(resp.StatusCode)

			if isStream {
				// 流式请求：不提前 cancel，让 context 随客户端连接生命周期自然结束
				// 否则 HTTP/2 下会截断流
				// 注意：c.Request.Context() 会在客户端断开时自动取消，不会泄露
				responseBody = ps.handleStreamingResponse(c, resp)
				cancel() // 流结束后显式 cancel，消除 go vet 警告
			} else {
				defer cancel()
				responseBody = ps.handleNormalResponse(c, resp)
			}
		}

		ps.logRequest(c, originalGroup, group, apiKey, startTime, resp.StatusCode, nil, isStream, upstreamURL, channelHandler, finalBodyBytes, models.RequestTypeFinal, responseBody)
		return
	}
}

func shouldFailoverOnStatusCode(statusCode int, group *models.Group) bool {
	if group == nil {
		return false
	}
	return group.FailoverStatusCodeMatcher.Match(statusCode)
}

// logRequest 记录请求日志
func (ps *ProxyServer) logRequest(
	c *gin.Context,
	originalGroup *models.Group,
	group *models.Group,
	apiKey *models.APIKey,
	startTime time.Time,
	statusCode int,
	finalError error,
	isStream bool,
	upstreamAddr string,
	channelHandler channel.ChannelProxy,
	bodyBytes []byte,
	requestType string,
	responseBody string,
) {
	if ps.requestLogService == nil {
		return
	}

	var requestBodyToLog, userAgent, agentFilesToLog, toolCallsToLog string

	if group.EffectiveConfig.EnableRequestBodyLogging {
		requestBodyToLog = string(bodyBytes)
		userAgent = c.Request.UserAgent()
		// 提取agent上传的文件内容（如Cline插件上传的文件）
		agentFiles := utils.ExtractAgentFiles(bodyBytes)
		agentFilesToLog = utils.AgentFilesToJSON(agentFiles)
		// 提取历史消息中的工具调用信息（如Cline的工具调用）
		toolCalls := utils.ExtractAgentToolCalls(bodyBytes)
		toolCallsToLog = utils.AgentToolCallsToJSON(toolCalls)
	}

	duration := time.Since(startTime).Milliseconds()

	logEntry := &models.RequestLog{
		GroupID:      group.ID,
		GroupName:    group.Name,
		IsSuccess:    finalError == nil && statusCode < 400,
		SourceIP:     c.ClientIP(),
		StatusCode:   statusCode,
		RequestPath:  utils.TruncateString(c.Request.URL.String(), 500),
		Duration:     duration,
		UserAgent:    userAgent,
		RequestType:  requestType,
		IsStream:     isStream,
		UpstreamAddr: utils.TruncateString(upstreamAddr, 500),
		RequestBody:  requestBodyToLog,
		AgentFiles:   agentFilesToLog,
		ToolCalls:     toolCallsToLog,
		ResponseBody:  responseBody,
	}

	// 设置父组
	if originalGroup != nil && originalGroup.GroupType == "aggregate" && originalGroup.ID != group.ID {
		logEntry.ParentGroupID = originalGroup.ID
		logEntry.ParentGroupName = originalGroup.Name
	}

	if channelHandler != nil && bodyBytes != nil {
		logEntry.Model = channelHandler.ExtractModel(c, bodyBytes)
	}

	if apiKey != nil {
		if ps.encryptionSvc != nil {
			// 加密密钥值用于日志存储
			encryptedKeyValue, err := ps.encryptionSvc.Encrypt(apiKey.KeyValue)
			if err != nil {
				logrus.WithError(err).Error("Failed to encrypt key value for logging")
				logEntry.KeyValue = "failed-to-encryption"
			} else {
				logEntry.KeyValue = encryptedKeyValue
			}
			// 添加 KeyHash 用于反查
			logEntry.KeyHash = ps.encryptionSvc.Hash(apiKey.KeyValue)
		} else {
			logEntry.KeyValue = "encryption-unavailable"
			logEntry.KeyHash = ""
		}
	}

	if finalError != nil {
		logEntry.ErrorMessage = finalError.Error()
	}

	if err := ps.requestLogService.Record(logEntry); err != nil {
		logrus.Errorf("Failed to record request log: %v", err)
	}
}
