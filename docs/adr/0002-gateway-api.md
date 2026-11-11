# ADR 0002：A3 网关接口与兼容边界

- 状态：已采纳
- 日期：2026-11-11（按 Day 15 提前执行）
- 项目：`gateway/` 独立 Go module，module 为 `github.com/carder-coder/ai-agent/gateway`

## 决策

网关作为 Python Agent 的 Go 服务侧，监听 `GATEWAY_PORT`（默认 8080）。所有 JSON 错误统一为
`{"error":{"message":"...","type":"...","code":"..."}}`；不得透传上游响应头。未实现的接口返回
明确的 `501`，不伪装成成功。

| 方法与路径 | 请求 | 成功响应 | 本周范围 |
| --- | --- | --- | --- |
| `POST /v1/chat/completions` | OpenAI Chat Completions JSON：`model`、`messages` 必填；`stream` 可选 | 非流式为 `chat.completion`；流式为 `text/event-stream`，以 `data: [DONE]` 结束 | 本周实现 |
| `POST /v1/tools/execute` | `{"name":"工具名","arguments":{}}` | `{"trace_id":"...","ok":true,"result":{...}}` | 第 5 周实现 |
| `GET /v1/trace/{id}` | 路径携带 trace ID | `{"trace_id":"...","events":[...]}` | 后续实现 |
| `GET /metrics` | 无 | `text/plain; version=0.0.4` 指标文本 | 第 4 周扩展；本周末实现网关基础指标 |
| `POST /v1/cache/feedback` | `{"trace_id":"...","correct":true}` | `{"ok":true}` | 第 4 周用于缓存答案正误反馈 |

`GET /healthz` 是运行探针，不属于上述四个业务接口。

### 请求与错误

- 非法 JSON、缺字段或字段类型错误：`400`、`invalid_request_error`。
- 豆包鉴权失败：`401`、`authentication_error`；保留上游 `401`，但只转换错误体。
- 限流：`429`、`rate_limit_error`，按需返回规范化的 `Retry-After`。
- 上游超时：`504`、`timeout`。
- 上游其他非成功响应：`502`、`upstream_error`。
- 网关自身异常：`500`、`server_error`。
- 429 在没有返回任何客户端流数据前，按 1 / 2 / 4 秒退避重试；401 不重试。超时不自动重放，
  避免上游已受理但响应丢失时重复计费。
- 同一 `Idempotency-Key` 与同一请求体返回同一响应；同一 key 改变请求体返回 409。
  幂等结果保存在进程内 24 小时，重启后缓存不保留。
- 连续 5 次上游 5xx / 网络失败打开熔断 30 秒；半开时只放行一个探测请求。401、429 不触发熔断。

## 兼容约束

- 服务端使用 `net/http`；响应头由网关生成，禁止原样复制上游头。
- 流式响应必须逐块 flush；客户端取消通过 request context 传递到上游。
- 首字节延迟从收到请求开始计到首个内容 chunk；不把 usage-only 或终止块算作首字节。
- Provider 通过内部 `Adapter` 注册；配置决定使用哪个实现，接口和 API 不依赖具体供应商。
- Go 侧 JSONL trace 复用 Python `scripts/replay.py` 的事件字段；未计费前 `cost_yuan` 为 `0`。
- 价格从根 `.env` 的 `ARK_PRICE_IN`、`ARK_PRICE_OUT`、`ARK_PRICE_CACHED` 读取，不在代码中硬编码业务价格。

### 第 4 周指标与 usage 口径

| 指标 | Prometheus 名称 | 口径 |
| --- | --- | --- |
| QPS | `gateway_requests_per_second` | 最近一分钟完成的 HTTP 请求数 / 实际窗口秒数 |
| 分模型延迟 | `gateway_model_request_duration_seconds` | 从模型请求发出到响应处理完成的秒数直方图 |
| 输入 token | `gateway_model_input_tokens_total` | Provider 返回的累计输入 token |
| 输出 token | `gateway_model_output_tokens_total` | Provider 返回的累计输出 token |
| 缓存输入 token | `gateway_model_cached_input_tokens_total` | 输入 token 的缓存命中子集，不重复计入普通输入价格 |
| 错误率 | `gateway_errors_total / gateway_requests_total` | HTTP 4xx/5xx 请求占比；分模型错误数另见 `gateway_model_errors_total` |
| 累计成本 | `gateway_model_cost_yuan_total` | 按 usage 与模型单价计算的估算人民币成本 |

单次 usage 结构为 `{input_tokens, output_tokens, total_tokens, cached_tokens}`，其中
`cached_tokens <= input_tokens`；成本按非缓存输入、缓存输入、输出三档单价分别计算，再四舍五入到
0.0001 元。trace 的单次成本与累计成本均记录计费精度下的值。

路由通过 `GATEWAY_ROUTE_SIMPLE_MODEL`、`GATEWAY_ROUTE_COMPLEX_MODEL` 和
`GATEWAY_ROUTE_CONTEXT_THRESHOLD` 集中配置。聊天请求可附带 `task_type: "simple"` 或
`"complex"`；长上下文阈值优先，路由输入与选择原因记录在 `llm.route` trace 字段。

## 目录边界

Go 服务和 Python Agent 同仓，但各用独立工具链：Go 代码、命令和测试位于 `gateway/`，不在仓库根目录创建
`go.mod`。A1 的循环仍在 Python 侧；工具执行留到第 5 周；成本路由留到第 4 周。
后续 Go MCP Server 若落地，另用 `mcp/` module，不预建空目录。

## 后果

- 单仓共享 `.env` 价格口径、实验记录与 Python trace 重放工具。
- `go test`、`go vet` 在 `gateway/` 内运行，不会和根目录 Python 测试混扫。
- 本 ADR 只冻结 API 边界；具体超时、重试、幂等与指标实现按周计划逐步交付。
