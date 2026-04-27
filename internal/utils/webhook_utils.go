package utils

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"
)

// feishuWebhookMessage 飞书 webhook 消息结构
type feishuWebhookMessage struct {
	MsgType string                 `json:"msg_type"`
	Content map[string]interface{} `json:"content"`
}

// SendFeishuWebhook 发送飞书 Webhook 通知
// webhookURL: 飞书群机器人 Webhook 地址
// title: 消息标题
// content: 消息正文内容
func SendFeishuWebhook(webhookURL, title, content string) error {
	if webhookURL == "" {
		return fmt.Errorf("feishu webhook URL is empty")
	}

	msg := feishuWebhookMessage{
		MsgType: "interactive",
		Content: map[string]interface{}{
			"config": map[string]interface{}{
				"wide_screen_mode": true,
			},
			"header": map[string]interface{}{
				"title": map[string]interface{}{
					"tag":     "plain_text",
					"content": title,
				},
				"template": "red",
			},
			"elements": []map[string]interface{}{
				{
					"tag": "div",
					"text": map[string]interface{}{
						"tag":     "lark_md",
						"content": content,
					},
				},
			},
		},
	}

	// 将消息体包装为飞书卡片消息格式
	payload := map[string]interface{}{
		"msg_type": "interactive",
		"card":     msg.Content,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal webhook message: %w", err)
	}

	req, err := http.NewRequest("POST", webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send webhook request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("webhook request returned status %d", resp.StatusCode)
	}

	logrus.WithField("title", title).Debug("Successfully sent Feishu webhook notification")
	return nil
}

// SendFeishuWebhookText 发送飞书 Webhook 纯文本通知
func SendFeishuWebhookText(webhookURL, text string) error {
	if webhookURL == "" {
		return fmt.Errorf("feishu webhook URL is empty")
	}

	payload := map[string]interface{}{
		"msg_type": "text",
		"content": map[string]interface{}{
			"text": text,
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal webhook message: %w", err)
	}

	req, err := http.NewRequest("POST", webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send webhook request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("webhook request returned status %d", resp.StatusCode)
	}

	logrus.Debug("Successfully sent Feishu webhook text notification")
	return nil
}
