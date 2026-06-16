package channel

import (
	"context"
	"gpt-load/internal/models"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
)

// ChannelProxy 定义不同API通道代理的接口
type ChannelProxy interface {
	// BuildUpstreamURL 构建上游服务的目标URL
	BuildUpstreamURL(originalURL *url.URL, groupName string) (string, error)

	// IsConfigStale 检查通道配置是否与提供的分组相比已过期
	IsConfigStale(group *models.Group) bool

	// GetHTTPClient 返回标准请求的客户端
	GetHTTPClient() *http.Client

	// GetStreamClient 返回流式请求的客户端
	GetStreamClient() *http.Client

	// ModifyRequest 允许通道添加特定头部或修改请求
	ModifyRequest(req *http.Request, apiKey *models.APIKey, group *models.Group)

	// IsStreamRequest 检查请求是否为流式响应
	IsStreamRequest(c *gin.Context, bodyBytes []byte) bool

	// ExtractModel 从请求中提取模型名称
	ExtractModel(c *gin.Context, bodyBytes []byte) string

	// ValidateKey 检查给定的API密钥是否有效
	ValidateKey(ctx context.Context, apiKey *models.APIKey, group *models.Group) (bool, error)

	// ApplyModelRedirect 根据分组的重定向规则应用模型重定向
	ApplyModelRedirect(req *http.Request, bodyBytes []byte, group *models.Group) ([]byte, error)

	// TransformModelList 根据重定向规则转换模型列表响应
	TransformModelList(req *http.Request, bodyBytes []byte, group *models.Group) (map[string]any, error)

	// GetBalanceQueryPath 返回通道的余额查询路径
	GetBalanceQueryPath(group *models.Group) string
}
