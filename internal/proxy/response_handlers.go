package proxy

import (
	"bytes"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// maxResponseBodyCapture 限制单条日志捕获的响应体大小，避免内存/数据库膨胀
const maxResponseBodyCapture = 16 * 1024 * 1024 // 16MB

// handleStreamingResponse 转发流式响应，并返回捕获的响应体（截断到 maxResponseBodyCapture）
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
				return truncateResponseBody(captured.Bytes())
			}
			flusher.Flush()
			// 同步写入捕获 buffer（不超过上限）
			if captured.Len() < maxResponseBodyCapture {
				remaining := maxResponseBodyCapture - captured.Len()
				if n > remaining {
					captured.Write(buf[:remaining])
				} else {
					captured.Write(buf[:n])
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			logUpstreamError("reading from upstream", err)
			return truncateResponseBody(captured.Bytes())
		}
	}

	return truncateResponseBody(captured.Bytes())
}

// handleNormalResponse 转发普通响应，自行关闭 resp.Body
// 返回捕获的响应体（截断到 maxResponseBodyCapture）
func (ps *ProxyServer) handleNormalResponse(c *gin.Context, resp *http.Response) string {
	defer resp.Body.Close()

	var captured bytes.Buffer
	// 限制捕获大小，避免大响应体占用过多内存
	limitWriter := &limitedBufferWriter{dst: c.Writer, buf: &captured, limit: maxResponseBodyCapture}
	if _, err := io.Copy(limitWriter, resp.Body); err != nil {
		logUpstreamError("copying response body", err)
	}
	return truncateResponseBody(captured.Bytes())
}

// truncateResponseBody 将字节数组转为字符串，超限时截断
func truncateResponseBody(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if len(b) > maxResponseBodyCapture {
		b = b[:maxResponseBodyCapture]
	}
	return string(b)
}

// limitedBufferWriter 同时写入 HTTP 响应与捕获 buffer，捕获 buffer 有大小上限
type limitedBufferWriter struct {
	dst   http.ResponseWriter
	buf   *bytes.Buffer
	limit int
}

func (w *limitedBufferWriter) Write(p []byte) (int, error) {
	// 始终写回客户端
	n, err := w.dst.Write(p)
	if err != nil {
		return n, err
	}
	// 写入捕获 buffer，但不超过上限（bytes.Buffer.Write 永不返回 error）
	if w.buf.Len() < w.limit {
		remaining := w.limit - w.buf.Len()
		if len(p) > remaining {
			w.buf.Write(p[:remaining])
		} else {
			w.buf.Write(p)
		}
	}
	return n, nil
}
