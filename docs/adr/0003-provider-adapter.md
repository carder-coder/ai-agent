# ADR 0003：网关 Provider Adapter 与注册规则

- 状态：已采纳
- 日期：2026-11-11（按 Day 17 提前执行）

## 决策

HTTP API 只依赖 `internal/provider.Adapter`，不直接依赖供应商 SDK 或协议类型：

```go
type Adapter interface {
    Chat(ctx context.Context, body []byte, stream bool) (Response, error)
}
```

`Response` 表示一次结果：非流式使用 `Body` 与 `Usage`，流式使用 `Stream`（可逐块读取并可关闭），
错误响应使用 `StatusCode`、`Body` 与可选 `RetryAfter`。API 负责把 provider 错误映射为稳定的 OpenAI
错误体，绝不透传 provider 响应头。

注册表以稳定的小写名称映射 Adapter；启动时注册 `doubao` 与 `mock`，由 `GATEWAY_PROVIDER`
选择，缺省为 `doubao`。未注册的名称使启动明确失败，不静默回退。新增 Provider 应实现 Adapter，
再加入同一注册位置，不改 API handler 与命令行。

`mock` 不访问外网；支持配置延迟、HTTP 状态和中断，用于离线验证取消、错误映射与流式行为。
真实豆包 Adapter 通过根 `.env` 配置凭证、模型、地址和超时。

## 约束与后果

- Adapter 的 `Chat` 同时承载非流式与流式结果，避免 API 暴露具体供应商类型。
- `ChunkStream` 的 `Next` 在结束时返回 `io.EOF`，调用方必须关闭流；请求 context 负责取消上游 I/O。
- 用量以 provider 返回的 usage 为准，缺失时置零；不从文本长度推算。
- 当前启动装配由 API router 集中注册；未来依赖增多时，可将装配提取到 `cmd/gateway`，但注册名仍以配置为准。
