package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/models"
	"io"
	"net/http"
	"strings"

	"github.com/sirupsen/logrus"
)

func (ps *ProxyServer) applyParamOverrides(bodyBytes []byte, group *models.Group) ([]byte, error) {
	if len(group.ParamOverrides) == 0 || len(bodyBytes) == 0 {
		return bodyBytes, nil
	}

	var requestData map[string]any
	if err := json.Unmarshal(bodyBytes, &requestData); err != nil {
		logrus.Warnf("failed to unmarshal request body for param override, passing through: %v", err)
		return bodyBytes, nil
	}

	for key, value := range group.ParamOverrides {
		requestData[key] = value
	}

	return json.Marshal(requestData)
}

// logUpstreamError 提供集中式的上游交互错误日志记录
func logUpstreamError(context string, err error) {
	if err == nil {
		return
	}
	if app_errors.IsIgnorableError(err) {
		logrus.Debugf("Ignorable upstream error in %s: %v", context, err)
	} else {
		logrus.Errorf("Upstream error in %s: %v", context, err)
	}
}

// aihubmixAbuseKeywords 是 aihubmix 渠道返回的免费资源滥用提示中的关键文本
// 使用关键子串匹配，确保即使响应格式有细微差异也能正确识别
const aihubmixAbuseKeywords = "to prevent abuse of free resources, accounts that have not been recharged"

// isAihubmixAbuseResponse 检查上游响应是否为 aihubmix 的免费资源滥用提示
// 当 aihubmix 渠道返回 HTTP 200 但响应体包含滥用提示文本时，应视为错误而非成功
// 注意：仅读取前 4KB 检查关键词，使用 MultiReader 重建 body 以保留流式 SSE 行为
func isAihubmixAbuseResponse(upstreamURL string, resp *http.Response) bool {
	if resp == nil || resp.StatusCode != http.StatusOK {
		return false
	}

	// 仅检查包含 aihubmix 域名的上游 URL
	if !strings.Contains(upstreamURL, "aihubmix") {
		return false
	}

	// 只读取前 4KB 检查关键词，避免读取整个响应体破坏流式传输
	const peekSize = 4096
	peek := make([]byte, peekSize)
	n, _ := io.ReadAtLeast(resp.Body, peek, 1)
	if n == 0 {
		return false
	}

	head := string(peek[:n])

	// 检查是否包含滥用提示关键词（直接在前缀中匹配）
	if strings.Contains(head, aihubmixAbuseKeywords) {
		return true
	}

	// 尝试解压 gzip 后再检查（上游可能返回压缩内容）
	if strings.Contains(resp.Header.Get("Content-Encoding"), "gzip") {
		if reader, gzipErr := gzip.NewReader(bytes.NewReader(peek[:n])); gzipErr == nil {
			defer reader.Close()
			if decompressed, readErr := io.ReadAll(reader); readErr == nil {
				if strings.Contains(string(decompressed), aihubmixAbuseKeywords) {
					return true
				}
			}
		}
	}

	// 未匹配滥用消息，使用 MultiReader 将 peek 过的数据 + 剩余 body 拼回，保留流式传输
	resp.Body = &peekReadCloser{
		Reader: io.MultiReader(bytes.NewReader(peek[:n]), resp.Body),
		closer: resp.Body,
	}
	return false
}

// peekReadCloser 包装 io.Reader，将 Close() 委托给原始 body，确保底层连接正确关闭
type peekReadCloser struct {
	io.Reader
	closer io.ReadCloser
}

func (p *peekReadCloser) Close() error {
	return p.closer.Close()
}

// handleGzipCompression 检查gzip编码并在必要时解压响应体
func handleGzipCompression(resp *http.Response, bodyBytes []byte) []byte {
	if strings.Contains(resp.Header.Get("Content-Encoding"), "gzip") {
		reader, gzipErr := gzip.NewReader(bytes.NewReader(bodyBytes))
		if gzipErr != nil {
			logrus.Warnf("Failed to create gzip reader for error body: %v", gzipErr)
			return bodyBytes
		}
		defer reader.Close()

		decompressedBody, readAllErr := io.ReadAll(reader)
		if readAllErr != nil {
			logrus.Warnf("Failed to decompress gzip error body: %v", readAllErr)
			return bodyBytes
		}
		return decompressedBody
	}
	return bodyBytes
}
