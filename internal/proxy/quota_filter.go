package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"gpt-load/internal/config"
	"gpt-load/internal/models"
	"gpt-load/internal/utils"

	"github.com/sirupsen/logrus"
)

// quotaPayload 表示上游响应中嵌入的配额信息
type quotaPayload struct {
	LastOne    bool   `json:"lastOne"`
	Created    int64  `json:"created"`
	Content    string `json:"content"`
	Quota      struct {
		ModelRequestTotalCount int `json:"modelRequestTotalCount"`
		ModelRequestLimitCount int `json:"modelRequestLimitCount"`
	} `json:"quota"`
	StatusCode int `json:"statusCode"`
}

// quotaNotifier 使用内存 map 管理配额通知的冷却
type quotaNotifier struct {
	cooldowns sync.Map // key: groupID:keyHash -> time.Time
}

// globalQuotaNotifier 是全局单例，用于配额通知冷却
var globalQuotaNotifier = &quotaNotifier{}

const (
	// quotaNotificationThreshold 剩余次数低于此值时触发通知
	quotaNotificationThreshold = 500
	// quotaNotificationCooldown 通知冷却时间，避免重复发送
	quotaNotificationCooldown = 10 * time.Minute
)

// extractQuotaFromJSON 检查 JSON 字节切片是否包含配额信息
// 返回解析后的配额信息和 true（如果是配额事件）
func extractQuotaFromJSON(data []byte) (*quotaPayload, bool) {
	// 快速检查：必须同时包含 "quota" 和 "modelRequestTotalCount" 才进行 JSON 解析
	if !bytes.Contains(data, []byte(`"quota"`)) || !bytes.Contains(data, []byte(`"modelRequestTotalCount"`)) {
		return nil, false
	}

	var payload quotaPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, false
	}

	// 验证是否有有效的配额信息
	if payload.Quota.ModelRequestLimitCount == 0 {
		return nil, false
	}

	return &payload, true
}

// extractQuotaFromLine 检查 SSE 行是否包含配额信息
// 行可能是 "data: {...}" 格式或原始 JSON "{...}"
func extractQuotaFromLine(line string) (*quotaPayload, bool) {
	// 快速检查以避免解析每一行
	if !strings.Contains(line, `"quota"`) || !strings.Contains(line, `"modelRequestTotalCount"`) {
		return nil, false
	}

	// 从行中提取 JSON — 处理 "data: " 前缀
	jsonStr := line
	if strings.HasPrefix(jsonStr, "data: ") {
		jsonStr = strings.TrimPrefix(jsonStr, "data: ")
	} else if strings.HasPrefix(jsonStr, "data:") {
		jsonStr = strings.TrimPrefix(jsonStr, "data:")
	}
	jsonStr = strings.TrimSpace(jsonStr)

	if jsonStr == "" {
		return nil, false
	}

	return extractQuotaFromJSON([]byte(jsonStr))
}

// filterQuotaFromBody 从非流式响应体中移除配额 JSON 对象
// 用于配额 JSON 可能被追加到主响应之后的情况
// 返回过滤后的响应体和提取的配额信息（如果有）
func filterQuotaFromBody(body []byte) ([]byte, *quotaPayload) {
	if !bytes.Contains(body, []byte(`"quota"`)) {
		return body, nil
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	var cleanParts [][]byte
	var quota *quotaPayload

	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			break
		}

		if q, ok := extractQuotaFromJSON(raw); ok {
			quota = q
		} else {
			cleanParts = append(cleanParts, raw)
		}
	}

	// 如果没有找到有效内容
	if len(cleanParts) == 0 {
		if quota != nil {
			// body 中只有配额 JSON，过滤后无有效内容，返回空 body
			return nil, quota
		}
		// 无法解析任何有效 JSON，返回原始 body 作为兜底
		return body, nil
	}

	// 只有一个有效部分时直接返回（保留原始格式）
	if len(cleanParts) == 1 {
		return cleanParts[0], quota
	}

	// 多个非配额对象（不常见的情况），用换行符连接
	var result bytes.Buffer
	for i, part := range cleanParts {
		if i > 0 {
			result.WriteByte('\n')
		}
		result.Write(part)
	}
	return result.Bytes(), quota
}

// notifyQuotaIfNeeded 在剩余配额低于阈值且冷却期已过时发送飞书 Webhook 通知
func notifyQuotaIfNeeded(settingsManager *config.SystemSettingsManager, group *models.Group, apiKey *models.APIKey, quota *quotaPayload) {
	if quota == nil {
		return
	}

	remaining := quota.Quota.ModelRequestLimitCount - quota.Quota.ModelRequestTotalCount
	if remaining >= quotaNotificationThreshold {
		return
	}

	// 使用 group ID + key hash 构建冷却 key
	keyHash := ""
	if apiKey != nil {
		keyHash = apiKey.KeyHash
	}
	cooldownKey := fmt.Sprintf("quota:%d:%s", group.ID, keyHash)

	// 检查冷却期
	if last, ok := globalQuotaNotifier.cooldowns.Load(cooldownKey); ok {
		if time.Since(last.(time.Time)) < quotaNotificationCooldown {
			return
		}
	}
	globalQuotaNotifier.cooldowns.Store(cooldownKey, time.Now())

	// 发送通知
	webhookURL := settingsManager.GetSettings().FeishuWebhookURL
	if webhookURL == "" {
		logrus.WithFields(logrus.Fields{
			"group":     group.Name,
			"remaining": remaining,
		}).Debug("Quota below threshold but Feishu webhook not configured")
		return
	}

	groupName := group.Name
	if group.DisplayName != "" {
		groupName = group.DisplayName
	}

	keyDisplay := "unknown"
	if apiKey != nil {
		keyDisplay = utils.MaskAPIKey(apiKey.KeyValue)
	}

	title := fmt.Sprintf("⚠️ [%s] 渠道剩余次数不足告警", groupName)
	content := fmt.Sprintf("**分组**: %s\n**密钥**: %s\n**已用次数**: %d\n**总次数**: %d\n**剩余次数**: %d\n\n请及时关注渠道配额，避免服务中断。",
		groupName, keyDisplay, quota.Quota.ModelRequestTotalCount, quota.Quota.ModelRequestLimitCount, remaining)

	if err := utils.SendFeishuWebhook(webhookURL, title, content); err != nil {
		logrus.WithError(err).WithFields(logrus.Fields{
			"group":     group.Name,
			"remaining": remaining,
		}).Error("Failed to send quota notification via Feishu webhook")
	} else {
		logrus.WithFields(logrus.Fields{
			"group":     group.Name,
			"remaining": remaining,
			"total":     quota.Quota.ModelRequestLimitCount,
			"used":      quota.Quota.ModelRequestTotalCount,
		}).Info("Sent quota low notification via Feishu webhook")
	}
}
