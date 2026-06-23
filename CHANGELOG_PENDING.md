# 更新日志（待提交）

本次更新涉及 **22 个文件**，主要包含日志增强、并发优化、密钥稳定性及国际化。

---

## 主要变更

### 1. 新增日志字段 🗃️

- 为所有日志分表补充 `tool_calls` 和 `response_body` 列
- 支持 MySQL (LONGTEXT) 和 SQLite

### 2. 通道并发优化 ⚡

- 重构 `GetChannel()`，采用双重检查锁定，缩小锁粒度
- 通道构造移至锁外，消除并发代理请求锁竞争

### 3. 密钥管理优化 🔐

- 引入信号量限制并发 goroutine，防止高峰期资源耗尽
- 事务冲突自动重试（带抖动延时）

### 4. 启动清理 🧹

- 启动时自动清理历史遗留的临时文件

### 5. 国际化扩展 🌐

- 补充中英日三语翻译

### 6. 代理与请求处理 🚀

- 优化请求辅助与响应处理逻辑
- 子分组管理与任务调度增强

### 7. 其他调整

- `models/types.go`: 请求日志模型新增 `ToolCalls` 和 `ResponseBody`
- `gemini_channel.go`: 通道配置过期判断逻辑调整

---

## 新增文件

- `internal/db/migrations/v1_8_0_AddToolCallsAndResponseBodyColumns.go`

---

## 提交命令

```bash
git add -A
git commit -m "feat: add tool_calls/response_body log fields, optimize concurrency and key management"
git push myfork
```
