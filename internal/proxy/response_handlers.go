package proxy

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"

	"gpt-load/internal/utils"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// handleStreamingResponse 转发流式响应，并返回捕获的完整响应体
// 流式客户端禁用了自动解压，因此需要手动解压捕获的响应体后再存储
func (ps *ProxyServer) handleStreamingResponse(c *gin.Context, resp *http.Response) string {
	defer resp.Body.Close()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		logrus.Error("Streaming unsupported by the writer, falling back to normal response")
		return ps.handleNormalResponse(c, resp)
	}

	var captured bytes.Buffer
	buf := make([]byte, 4*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := c.Writer.Write(buf[:n]); writeErr != nil {
				logUpstreamError("writing stream to client", writeErr)
				return ps.decompressAndEncode(captured.Bytes(), resp.Header)
			}
			flusher.Flush()
			captured.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			logUpstreamError("reading from upstream", err)
			return ps.decompressAndEncode(captured.Bytes(), resp.Header)
		}
	}

	return ps.decompressAndEncode(captured.Bytes(), resp.Header)
}

// handleNormalResponse 转发普通响应，自行关闭 resp.Body
// 返回捕获的完整响应体
// 普通客户端已启用自动解压，但为防止上游返回非标准响应，仍做防御性解压
func (ps *ProxyServer) handleNormalResponse(c *gin.Context, resp *http.Response) string {
	defer resp.Body.Close()

	var captured bytes.Buffer
	// 同时写入客户端和捕获 buffer
	teeWriter := io.MultiWriter(c.Writer, &captured)
	if _, err := io.Copy(teeWriter, resp.Body); err != nil {
		logUpstreamError("copying response body", err)
	}
	return ps.decompressAndEncode(captured.Bytes(), resp.Header)
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
