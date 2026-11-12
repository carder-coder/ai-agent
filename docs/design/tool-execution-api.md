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
  `properties`、`required`、`enum`、`maxLength`、`maxItems` 与
  `additionalProperties: false`；不支持的 schema 关键字不得用于放宽校验。
- 校验错误返回 `400 invalid_tool_arguments`，`error.details` 为字段路径和原因数组。
- 未注册工具返回 `404 tool_not_found`；超时返回 `504 tool_timeout`；其他执行错误返回
  `502 tool_execution_failed`。所有响应均使用统一 `error` envelope。
- 成功响应包含 `trace_id`、`tool`、`result` 和 `duration_ms`。结果输出限制为 16 KiB，
  单次执行超时 3 秒。
- 每次执行都会写 trace；响应头 `X-Trace-ID` 与响应中的 `trace_id` 相同。

## 写操作审批

写操作默认关闭。启用写能力需同时设置 `GATEWAY_TOOLS_ALLOW_WRITES=true` 和独立审批密钥
`GATEWAY_TOOL_APPROVAL_TOKEN`。写工具只允许在配置的工具工作目录内创建受限文件，不接受
绝对路径、路径分隔符或覆盖已有文件。

1. 调用 `POST /v1/tools/approvals` 提交工具名和参数，取得短时有效的 `approval_id`。
2. 审批人调用 `POST /v1/tools/approvals/{id}/confirm`，携带审批密钥并明确确认。
3. 执行人将同一工具名、参数和 `approval_id` 传给 `/v1/tools/execute`。审批只绑定该组参数，
   过期后失效且成功执行后不可重放。

任一开关未配置、审批未确认、参数不匹配或审批失效时，写操作均不得执行。

## `GET /v1/trace/{id}`

- 只接受 32 位小写十六进制 trace ID，禁止路径片段和其他文件名。
- 成功返回 JSONL 原文（`application/x-ndjson`）；trace 不存在返回 `404 trace_not_found`。
- trace 只记录工具参数、调用方标识、结果摘要、耗时和状态，不记录认证凭证。
- trace 文件仅服务进程用户可读写；调用方身份由可信入口写入 `X-Caller-ID`。

## 审计与安全边界

- 工具使用服务端固定注册表和 JSON Schema；命令以独立可执行文件及固定参数调用，禁止拼接
  shell 字符串或经 shell 解释。
- 命令工具只开放只读白名单；写操作走独立审批流程，并在实现侧再次检查工作目录边界。
- 审计事件至少包含调用方、工具名、校验后的参数、结果摘要、耗时和完成状态；错误响应不泄漏
  命令行、环境变量、凭证或完整工具输出。
