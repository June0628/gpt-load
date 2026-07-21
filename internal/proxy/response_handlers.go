package proxy

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"gpt-load/internal/models"
	"gpt-load/internal/utils"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// handleStreamingResponse 转发流式响应，并返回捕获的完整响应体
// 流式客户端禁用了自动解压，因此需要手动解压捕获的响应体后再存储
// 使用磁盘临时文件缓冲流式数据，避免大响应体在高并发下导致 OOM
// 同时过滤上游追加的配额 JSON 事件，避免客户端解析报错
func (ps *ProxyServer) handleStreamingResponse(c *gin.Context, resp *http.Response, group *models.Group, apiKey *models.APIKey) string {
	defer resp.Body.Close()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		logrus.Error("Streaming unsupported by the writer, falling back to normal response")
		return ps.handleNormalResponse(c, resp, group, apiKey)
	}

	// 使用磁盘临时文件缓冲流式响应，避免大响应体占用内存
	tmpFile, err := os.CreateTemp("", "gpt-load-stream-*.bin")
	if err != nil {
		logrus.WithError(err).Warn("Failed to create temp file for streaming capture, falling back to in-memory buffer")
		return ps.handleStreamingResponseInMemory(c, resp, flusher, group, apiKey)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// 使用 bufio.Reader 按行读取 SSE 事件，以便检测和过滤配额事件
	reader := bufio.NewReaderSize(resp.Body, 64*1024)

	// skipUntilBlank 为 true 时跳过所有行直到遇到空行（SSE 事件分隔符）
	// 用于跳过以 event: 开头的自定义事件的所有字段
	skipUntilBlank := false

	for {
		line, readErr := reader.ReadString('\n')
		hasLine := len(line) > 0

		if hasLine {
			if skipUntilBlank {
				// 跳过模式中仍需检测配额信息（配额 data 行在 event 行之后）
				if q, isQuota := extractQuotaFromLine(line); isQuota && q != nil {
					notifyQuotaIfNeeded(ps.settingsManager, group, apiKey, q)
				}
				// 遇到空行（仅含 \n 或 \r\n）时退出跳过模式
				if strings.TrimSpace(line) == "" {
					skipUntilBlank = false
				}
				continue
			}

			// 检查是否为自定义事件类型行（如 event: catpaw.meta）
			// 这类事件通常携带配额信息，需要整事件跳过
			if strings.HasPrefix(line, "event:") {
				skipUntilBlank = true
				continue
			}

			// 检查当前行是否为配额事件
			if q, isQuota := extractQuotaFromLine(line); isQuota {
				// 提取配额信息，发送通知
				if q != nil {
					notifyQuotaIfNeeded(ps.settingsManager, group, apiKey, q)
				}
				// 跳过转发原始配额行，上游通常会在之后发送标准的 data: [DONE]
			} else {
				// 正常行：转发给客户端并写入临时文件
				if _, writeErr := c.Writer.Write([]byte(line)); writeErr != nil {
					logUpstreamError("writing stream to client", writeErr)
					// 客户端断开：将当前行补写入临时文件，确保日志完整
					tmpFile.WriteString(line)
					// 将剩余数据也写入临时文件
					io.Copy(tmpFile, reader)
					return ps.readAndProcessTempFile(tmpFile, resp.Header)
				}
				flusher.Flush()
				tmpFile.WriteString(line)
			}
		}

		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			logUpstreamError("reading from upstream", readErr)
			break
		}
	}

	return ps.readAndProcessTempFile(tmpFile, resp.Header)
}

// handleStreamingResponseInMemory 是 handleStreamingResponse 的内存降级版本
// 当无法创建临时文件时使用，保留原有行为
// 同样包含配额事件过滤逻辑
func (ps *ProxyServer) handleStreamingResponseInMemory(c *gin.Context, resp *http.Response, flusher http.Flusher, group *models.Group, apiKey *models.APIKey) string {
	var captured bytes.Buffer

	// 使用 bufio.Reader 按行读取 SSE 事件
	reader := bufio.NewReaderSize(resp.Body, 64*1024)

	// skipUntilBlank 为 true 时跳过所有行直到遇到空行（SSE 事件分隔符）
	skipUntilBlank := false

	for {
		line, readErr := reader.ReadString('\n')
		hasLine := len(line) > 0

		if hasLine {
			if skipUntilBlank {
				// 跳过模式中仍需检测配额信息（配额 data 行在 event 行之后）
				if q, isQuota := extractQuotaFromLine(line); isQuota && q != nil {
					notifyQuotaIfNeeded(ps.settingsManager, group, apiKey, q)
				}
				// 遇到空行（仅含 \n 或 \r\n）时退出跳过模式
				if strings.TrimSpace(line) == "" {
					skipUntilBlank = false
				}
				continue
			}

			// 检查是否为自定义事件类型行（如 event: catpaw.meta）
			if strings.HasPrefix(line, "event:") {
				skipUntilBlank = true
				continue
			}

			// 检查当前行是否为配额事件
			if q, isQuota := extractQuotaFromLine(line); isQuota {
				if q != nil {
					notifyQuotaIfNeeded(ps.settingsManager, group, apiKey, q)
				}
			} else {
				// 正常行：转发给客户端并写入捕获 buffer
				if _, writeErr := c.Writer.Write([]byte(line)); writeErr != nil {
					logUpstreamError("writing stream to client", writeErr)
					// 客户端断开：补写当前行到捕获 buffer
					captured.WriteString(line)
					// 将剩余数据也写入捕获 buffer
					io.Copy(&captured, reader)
					return ps.decompressAndEncode(captured.Bytes(), resp.Header)
				}
				flusher.Flush()
				captured.WriteString(line)
			}
		}

		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			logUpstreamError("reading from upstream", readErr)
			break
		}
	}

	return ps.decompressAndEncode(captured.Bytes(), resp.Header)
}

// readAndProcessTempFile 从临时文件读取已捕获的流式数据，解压并编码后返回
func (ps *ProxyServer) readAndProcessTempFile(tmpFile *os.File, headers http.Header) string {
	if _, err := tmpFile.Seek(0, 0); err != nil {
		logrus.WithError(err).Error("Failed to seek temp file for reading")
		return ""
	}
	data, err := io.ReadAll(tmpFile)
	if err != nil {
		logrus.WithError(err).Error("Failed to read temp file for log capture")
		return ""
	}
	return ps.decompressAndEncode(data, headers)
}

// handleNormalResponse 转发普通响应，自行关闭 resp.Body
// 返回捕获的完整响应体
// 普通客户端已启用自动解压，但为防止上游返回非标准响应，仍做防御性解压
// 同时过滤上游追加的配额 JSON，避免客户端解析报错
func (ps *ProxyServer) handleNormalResponse(c *gin.Context, resp *http.Response, group *models.Group, apiKey *models.APIKey) string {
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logUpstreamError("reading response body", err)
		return ""
	}

	// 过滤配额 JSON
	filteredBody, quota := filterQuotaFromBody(body)
	if quota != nil {
		notifyQuotaIfNeeded(ps.settingsManager, group, apiKey, quota)
		// 如果 body 被过滤修改，更新 Content-Length 头
		if len(filteredBody) != len(body) {
			c.Header("Content-Length", strconv.Itoa(len(filteredBody)))
		}
	}

	// 转发过滤后的响应体给客户端
	if len(filteredBody) > 0 {
		if _, writeErr := c.Writer.Write(filteredBody); writeErr != nil {
			logUpstreamError("writing response to client", writeErr)
		}
	}

	return ps.decompressAndEncode(filteredBody, resp.Header)
}

// decompressAndEncode 解压响应体（如果需要），然后转为字符串
// 流式客户端禁用了 Go 的自动解压，因此需要手动解压
// 普通客户端已启用自动解压，Content-Encoding 头已被移除，此函数作为安全兜底
func (ps *ProxyServer) decompressAndEncode(b []byte, headers http.Header) string {
	if len(b) == 0 {
		return ""
	}

	// 检查 Content-Encoding 头，必要时解压
	contentEncoding := headers.Get("Content-Encoding")
	if contentEncoding != "" {
		decompressed, err := utils.DecompressResponse(contentEncoding, b)
		if err != nil {
			logrus.WithError(err).Warnf("Failed to decompress response body with encoding '%s', storing as-is", contentEncoding)
		} else {
			b = decompressed
		}
	}

	// 如果数据包含二进制数据（非 UTF-8），进行 base64 编码以避免 MySQL 字符集错误
	if !isValidUTF8(b) {
		logrus.Warnf("Response body contains non-UTF-8 data after decompression, base64 encoding for safe storage")
		return base64.StdEncoding.EncodeToString(b)
	}

	return string(b)
}

// isValidUTF8 检查字节切片是否为有效的 UTF-8 文本
// 检查全部字节，确保不会遗漏尾部的二进制数据（如未解压的 gzip 残余）
func isValidUTF8(b []byte) bool {
	// 使用 bytes.ToValidUTF8 检测是否有无效字节
	valid := bytes.ToValidUTF8(b, nil)
	return bytes.Equal(valid, b)
}
