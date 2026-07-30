package channel

import (
	"context"
	"gpt-load/internal/models"
	"net/http"

	"github.com/gin-gonic/gin"
)

func init() {
	Register("openai", newOpenAIChannel)
}

type OpenAIChannel struct {
	*BaseChannel
}

func newOpenAIChannel(f *Factory, group *models.Group) (ChannelProxy, error) {
	base, err := f.newBaseChannel("openai", group)
	if err != nil {
		return nil, err
	}

	return &OpenAIChannel{
		BaseChannel: base,
	}, nil
}

// ModifyRequest 为OpenAI服务设置Authorization头部
func (ch *OpenAIChannel) ModifyRequest(req *http.Request, apiKey *models.APIKey, group *models.Group) {
	req.Header.Set("Authorization", "Bearer "+apiKey.KeyValue)
}

// ValidateKey 通过发送聊天补全请求检查给定API密钥是否有效
func (ch *OpenAIChannel) ValidateKey(ctx context.Context, apiKey *models.APIKey, group *models.Group) (bool, error) {
	// 使用最小、低成本的载荷进行验证
	payload := gin.H{
		"model": ch.TestModel,
		"messages": []gin.H{
			{"role": "user", "content": "hi"},
		},
	}

	return ch.validateKeyWithPayload(ctx, apiKey, group, payload, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+apiKey.KeyValue)
	})
}
