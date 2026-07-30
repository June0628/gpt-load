package channel

import (
	"context"
	"gpt-load/internal/models"
	"net/http"

	"github.com/gin-gonic/gin"
)

func init() {
	Register("anthropic", newAnthropicChannel)
}

type AnthropicChannel struct {
	*BaseChannel
}

func newAnthropicChannel(f *Factory, group *models.Group) (ChannelProxy, error) {
	base, err := f.newBaseChannel("anthropic", group)
	if err != nil {
		return nil, err
	}

	return &AnthropicChannel{
		BaseChannel: base,
	}, nil
}

// ModifyRequest 为Anthropic API设置必需的头部
func (ch *AnthropicChannel) ModifyRequest(req *http.Request, apiKey *models.APIKey, group *models.Group) {
	req.Header.Set("x-api-key", apiKey.KeyValue)
	req.Header.Set("anthropic-version", "2023-06-01")
}

// ValidateKey 通过发送消息请求检查给定API密钥是否有效
func (ch *AnthropicChannel) ValidateKey(ctx context.Context, apiKey *models.APIKey, group *models.Group) (bool, error) {
	// 使用最小、低成本的载荷进行验证
	payload := gin.H{
		"model":      ch.TestModel,
		"max_tokens": 100,
		"messages": []gin.H{
			{"role": "user", "content": "hi"},
		},
	}

	return ch.validateKeyWithPayload(ctx, apiKey, group, payload, func(req *http.Request) {
		req.Header.Set("x-api-key", apiKey.KeyValue)
		req.Header.Set("anthropic-version", "2023-06-01")
	})
}
