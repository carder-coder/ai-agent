# 工具执行与 trace 查询接口

本文定义 A3 迭代 3 的工具执行接口、参数校验边界、写操作审批流程和 trace 查询约定。

## `POST /v1/tools/execute`

请求体为 JSON，最大 1 MiB：

```json
{
  "tool": "workspace_status",
  "arguments": {},
  "approval_id": ""
}
```

- `tool` 必须是服务端注册的工具名；调用方不能提交可执行文件名或 shell 命令。
- `arguments` 必须是对象，并按注册工具的 JSON Schema 校验。当前实现支持 `type`、
  `properties`、`required`、`items`、`enum`、`maxLength`、`maxItems` 与
  `pattern`、`additionalProperties: false`；不支持的 schema 关键字不得用于放宽校验。
- `inspect_text` 统计文本字符数/词数；`workspace_status` 只接受 `short` 或 `branch` 输出格式，
  并映射至固定的 `git status` 参数。
- 校验错误返回 `400 invalid_tool_arguments`，`error.details` 为字段路径和原因数组。
- 未注册工具返回 `404 tool_not_found`；超时返回 `504 tool_timeout`；其他执行错误返回
  `502 tool_execution_failed`。所有响应均使用统一 `error` envelope。
- 成功响应包含 `trace_id`、`tool`、`result` 和 `duration_ms`。结果输出限制为 16 KiB，
  单次执行超时 3 秒。
- 每次执行都会写 trace；响应头 `X-Trace-ID` 与响应中的 `trace_id` 相同。
- trace 中敏感文本/凭证参数仅保留 UTF-8 字节数和 SHA-256 摘要；完整结果正文不写入审计事件。
- 如果工具属于已有对话，可在 `X-Conversation-ID` 中传入该对话的 trace ID；查询父 ID 时会把关联的
  工具事件合并到对话 JSONL 的 `run_end` 之前。省略该头时，工具调用保留为独立 trace。

## 写操作审批

写操作默认关闭。启用写能力需同时设置 `GATEWAY_TOOLS_ALLOW_WRITES=true` 和独立审批密钥
`GATEWAY_TOOL_APPROVAL_TOKEN`（至少 32 个字符）。写工具只允许在配置的工具工作目录内创建受限
文件，不接受绝对路径、路径分隔符或覆盖已有文件。

1. 调用 `POST /v1/tools/approvals` 提交工具名和参数，取得短时有效的 `approval_id`。
2. 审批人调用 `POST /v1/tools/approvals/{id}/confirm`，携带审批密钥并明确确认。
3. 执行人将同一工具名、参数和 `approval_id` 传给 `/v1/tools/execute`。审批只绑定该组参数，
   过期后失效且成功执行后不可重放。

任一开关未配置、审批未确认、参数不匹配或审批失效时，写操作均不得执行。

## `GET /v1/trace/{id}`

- 只接受 32 位小写十六进制 trace ID，禁止路径片段和其他文件名。
- 成功返回 JSONL 原文（`application/x-ndjson`）；trace 不存在返回 `404 trace_not_found`。
- 单次查询最多返回 8 MiB；超过上限返回 `413 trace_too_large`。
- trace 只记录工具参数、调用方标识、结果摘要、耗时和状态，不记录认证凭证。
- trace 文件仅服务进程用户可读写；调用方身份由可信入口写入 `X-Caller-ID`。
- 网关本身不认证普通调用方；部署时必须由可信认证代理删除外部传入的 `X-Caller-ID` 并写入
  已认证身份，不得将写工具接口直接暴露到不可信网络。

## 审计与安全边界

- 工具使用服务端固定注册表和 JSON Schema；命令以独立可执行文件及固定参数调用，禁止拼接
  shell 字符串或经 shell 解释。
- 命令只继承 `PATH` 与操作系统必要变量，并强制关闭 Git 全局/系统配置及 fsmonitor。
- 命令工具只开放只读白名单；写操作走独立审批流程，并在实现侧再次检查工作目录边界。
- 审计事件至少包含调用方、工具名、校验后的参数、结果摘要、耗时和完成状态；错误响应不泄漏
  命令行、环境变量、凭证或完整工具输出。

### 审批审计事件

审批请求（`/v1/tools/approvals` 与 `/v1/tools/approvals/{id}/confirm`）各自生成独立 trace，
并写入一条 `approval` 事件：

| 字段 | 说明 |
| --- | --- |
| `approval_id` | 审批号；prepare 成功时写入，confirm 时回填 |
| `action` | `prepare` 或 `confirm` |
| `tool` | 申请审批的工具名（prepare） |
| `caller` | 请求头 `X-Caller-ID` 中的调用方 |
| `approver` | 审批密钥摘要标识 `token-sha256:<前 12 位>`，不落密钥原文 |
| `parameters` | 脱敏后的参数：文本/凭证类字段只保留 `redacted` 与长度 |
| `status` | `prepared`/`confirmed` 或失败码（见错误码总表） |
| `duration_ms` | 审批请求处理耗时 |

审批请求与执行请求通过 `approval_id` 关联：`/v1/tools/execute` 的 `tool` 事件参数中包含
`approval_id`，可据此还原"谁申请、谁批准、谁执行"。审计 trace 不可写时审批直接失败关闭
（`503 trace_unavailable`），不会在无审计的情况下发放审批号。

## `GET|POST /v1/admin/provider`

Provider 主备切换管理接口，默认关闭；显式设置 `GATEWAY_ADMIN_TOKEN`（≥32 字符）后启用，
请求需携带 `Authorization: Bearer <token>`。

- `GET` 返回当前快照：`active`、`primary`、`backup`、`threshold`、`switch_count`、
  `last_switch_at`、`last_switch_ms`、`last_reason`、`consecutive_failures`。
- `POST` 请求体 `{"provider":"backup"}`，返回 `previous`、`snapshot` 与实际耗时 `switch_ms`。
- 未配置令牌返回 `404 provider_admin_disabled`；令牌错误返回 `401 invalid_admin_token`；
  未注册 Provider 返回 `404 provider_not_found`；已是当前 Provider 返回
  `409 provider_already_active`；切换失败返回 `503 provider_switch_failed`。
- 切换成功后写 `provider_switch` 审计事件（`previous`/`active`/`reason`/`elapsed_ms`/
  `switch_count`），并按 `X-Trace-ID` 可通过 `/v1/trace/{id}` 复核。

### 主备切换配置

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `GATEWAY_PROVIDER` | `doubao` | 主 Provider 名称 |
| `GATEWAY_PROVIDER_BACKUP` | 空 | 备用 Provider 名称；为空表示不启用自动切换 |
| `GATEWAY_FAILOVER_THRESHOLD` | `3` | 连续不可用次数达到该值后自动切换，范围 1–10 |
| `GATEWAY_ADMIN_TOKEN` | 空 | 管理接口令牌；非空时至少 32 字符，为空则管理接口返回 404 |
| `GATEWAY_TOOL_WORKSPACE` | `tool-workspace` | 工具工作区；目录缺失时按 0700 自动创建 |

切换语义：主 Provider 连续失败达到阈值后在同一请求内切到备用并重试一次；切换后不再自动切回，
只能通过管理接口手动切回。上游返回 5xx 时只切换不重放，避免重复计费。

## 错误码总表

所有错误响应统一为 `{"error":{"message","type","code","details?"}}`。

| HTTP | `error.code` | 触发条件 | 接口 |
| --- | --- | --- | --- |
| 400 | `invalid_request` | 请求体不是合法 JSON 对象、包含未知字段或缺少必填字段 | 全部 |
| 400 | `invalid_conversation_id` | `X-Conversation-ID` 不是 32 位 trace ID | 执行 |
| 400 | `caller_id_required` | 写工具请求缺少 `X-Caller-ID` | 执行/审批 |
| 400 | `invalid_tool_arguments` | 参数不符合注册 schema，`details` 给出字段级原因 | 执行/审批 |
| 400 | `approval_not_required` | 对只读工具申请审批 | 审批 |
| 400 | `approval_confirmation_required` | 确认体 `confirmed` 不为 `true` | 审批确认 |
| 401 | `invalid_approval_token` | 审批密钥缺失或不匹配 | 审批确认 |
| 401 | `invalid_admin_token` | 管理令牌缺失或不匹配 | 管理接口 |
| 403 | `tool_writes_disabled` | 未启用 `GATEWAY_TOOLS_ALLOW_WRITES` | 执行/审批 |
| 404 | `tool_not_found` | 工具未注册（含大小写变体） | 执行/审批 |
| 404 | `approval_not_found` | 审批号不存在、已被消费或从未签发 | 执行/审批确认 |
| 404 | `conversation_not_found` | `X-Conversation-ID` 对应 trace 不存在 | 执行 |
| 404 | `trace_not_found` | trace ID 未命中 | trace 查询 |
| 404 | `provider_admin_disabled` | 未配置 `GATEWAY_ADMIN_TOKEN` | 管理接口 |
| 404 | `provider_not_found` | 目标 Provider 未注册 | 管理接口 |
| 405 | `method_not_allowed` | 方法不匹配 | 执行/审批/管理 |
| 409 | `provider_already_active` | 目标 Provider 已是当前生效 Provider | 管理接口 |
| 409 | `approval_expired` | 审批超过 5 分钟有效期 | 执行 |
| 413 | `request_too_large` | 请求体超过 1 MiB | 执行/审批 |
| 413 | `trace_too_large` | 单次 trace 查询超过 8 MiB | trace 查询 |
| 409 | `approval_not_confirmed` | 审批尚未确认 | 执行 |
| 409 | `approval_mismatch` | 调用方、工具名或参数摘要与审批不一致 | 执行 |
| 503 | `approval_unavailable` | 审批存储读写失败 | 审批 |
| 503 | `trace_unavailable` | 审计 trace 不可写，请求失败关闭 | 执行/审批 |
| 503 | `concurrency_limit` | 超过并发上限（默认 200） | 全部 |
| 504 | `tool_timeout` | 工具执行超过 3 秒 | 执行 |
| 502 | `tool_execution_failed` | 工具执行失败（非超时） | 执行 |

`approval_not_found` 之外，`approval_expired`、`approval_not_confirmed`、`approval_mismatch`
均为 `409` 且属于"不得执行"的拒绝分支。
