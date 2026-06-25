### 1. AIHubMix 平台支持 🆕

- 新增 AIHubMix 余额查询处理器（`/api/user/self` 接口）
- 新增 AIHubMix 免费资源滥用响应检测：HTTP 200 但内容为滥用提示时自动识别并屏蔽
- 滥用检测仅读取前 4KB 检查关键词，使用 `MultiReader` + `peekReadCloser` 重建 body，不破坏流式 SSE 传输
- 滥用检测命中时自动更新密钥状态，防止同一坏 key 无限重试
- `cron_checker` 启动时立即触发余额查询，无需等待首次 ticker

### 2. WebDAV 上传重试机制 🔄

- 重构 `uploadFileToWebDAV`，支持 5xx 服务端错误自动重试（最多 3 次，指数退避 1s/2s/4s）
- 409/404 处理提取为独立 `doMKCOLRetry` 方法，修复 `defer` 在循环内导致的文件句柄泄漏
- 每次重试前重新打开文件，确保 reader 不被重复消费

### 3. 密钥与请求日志修复 🐛

- 修复 `logRequest` 中 `bodyBytes` 使用错误：重试路径和最终路径均改为 `finalBodyBytes`（重定向后的请求体）
- `request_log_service.go`：仅 `RequestBody` 仍截断至 16MB（MEDIUMTEXT 限制），其余字段不再截断

### 4. 前端优化 ✨

- **余额轮询**：选中分组启用余额查询时，每 60 秒自动刷新数据（静默模式，不触发 loading 闪烁）
- **selectedGroup 同步**：轮询回调正确更新当前选中分组的引用，确保 UI 数据最新
- **分组端点**：前端基于当前域名拼接 `/proxy/{groupName}` URL，替代后端返回的 endpoint
- **余额面板**：未启用余额查询时隐藏余额统计面板（`v-if` 条件渲染）
- 资源清理：组件卸载时清除轮询定时器

### 5. CORS 安全加固 🔒

- 当 `AllowCredentials` 启用且 Origin 为通配符 `*` 时，不再发送 `Access-Control-Allow-Credentials: true` 头
- 符合 RFC 6454 规范，防止浏览器拒绝不安全的 CORS 组合

### 6. 认证中间件 🛡️

- `ProxyAuth` 中间件拆分 `authorized` 赋值，避免短路求值导致的时序差异（理论上防止时序攻击）
