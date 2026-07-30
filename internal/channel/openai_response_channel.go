package channel

import (
	"context"
	"gpt-load/internal/models"
	"net/http"

	"github.com/gin-gonic/gin"
)

func init() {
	Register("openai-response", newOpenAIResponseChannel)
}

type OpenAIResponseChannel struct {
	*BaseChannel
}

func newOpenAIResponseChannel(f *Factory, group *models.Group) (ChannelProxy, error) {
	base, err := f.newBaseChannel("openai-response", group)
	if err != nil {
		return nil, err
	}

	return &OpenAIResponseChannel{
		BaseChannel: base,
	}, nil
}

func (ch *OpenAIResponseChannel) ModifyRequest(req *http.Request, apiKey *models.APIKey, group *models.Group) {
	req.Header.Set("Authorization", "Bearer "+apiKey.KeyValue)
}

func (ch *OpenAIResponseChannel) ValidateKey(ctx context.Context, apiKey *models.APIKey, group *models.Group) (bool, error) {
	payload := gin.H{
		"model": ch.TestModel,
		"input": "hi",
	}

	return ch.validateKeyWithPayload(ctx, apiKey, group, payload, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+apiKey.KeyValue)
	})
}
