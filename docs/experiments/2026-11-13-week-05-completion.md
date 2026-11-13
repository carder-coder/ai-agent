# 2026-11-13 第 5 周补齐：主备切换、审批审计、注入样本与压测复现

> 背景：第 5 周完成情况评审（`docs/reviews/2026-11-12-week-05-completion-review.md`）判定三项交付
> 未兑现（Provider 主备切换未实现、30 分钟压测不可复现、两份报告只落一份）并有 P1/P2/P3 问题项。
> 本文记录 2026-11-13 的补齐内容与实测证据，**不补写 12-05～12-09 的历史日记录**。

## 1. 补齐项与证据

| 评审项 | 补齐内容 | 证据 |
| --- | --- | --- |
| P1-1 30 分钟压测不可复现、压 `/healthz` | 用仓库内 `cmd/loadtest` 持续模式重跑 200 并发 × 30 分钟，命令与限制写入报告 | 5,470,182 请求、0 错误、rps 3038.93、P99 224.10 ms；[压测报告](../reports/2026-11-13-week-05-loadtest-report.md) |
| P1-2 无 OS 级隔离 | 未做 OS 隔离的边界在报告中明确保留；工作区默认改为 `tool-workspace` 并在缺失时按 0700 自动创建，避免默认指向仓库根 | 实跑 `workspace_exists=True`；`internal/tools/executor.go` |
| P2-1 审批错误码未进契约 | 契约补"错误码总表"（26 个码 + HTTP 状态 + 触发条件 + 接口） | `docs/design/tool-execution-api.md` |
| P2-2 审批动作未落审计 | prepare/confirm 各自写 `approval` 事件（`approval_id`/`action`/`caller`/`approver=token-sha256:前12位`/脱敏参数/`status`/`duration_ms`），审计不可写时失败关闭 | `internal/api/tools.go`、`internal/trace/trace.go`、`internal/api/approval_audit_test.go` |
| P2-3 README/.env.example 未同步 | README 新增网关章节（接口表、启动、切换、压测、测试命令）；`.env.example` 删除重复的 `ARK_PRICE_*`，补 `GATEWAY_PROVIDER_BACKUP`/`GATEWAY_FAILOVER_THRESHOLD`/`GATEWAY_ADMIN_TOKEN`，工作区默认改 `tool-workspace` | `README.md`、`.env.example` |
| P3-1 缺注入样本集 | 新增 `cases/tool_abuse_cases.json`（23 条）并由 Go 测试逐条断言状态码/错误码；另程序化补 >1 MiB 与超长文本两条 | `gateway/internal/api/tool_abuse_cases_test.go` |
| 12-10 主备切换未实现 | 新增 `internal/provider/failover`（自动/手动切换 + 耗时统计）、`/v1/admin/provider`（GET 快照 / POST 切换、Bearer 鉴权、写 `provider_switch` 审计） | 自动切换：第 3 次请求 502→200；手动切换 `switch_ms=0`（端到端 43 ms） |
| 12-11 两份报告只落一份 | 拆成压测报告与成本报告两份定稿 | `docs/reports/` 下两篇 |

## 2. 代码改动

- `gateway/internal/provider/failover/failover.go`、`failover_test.go`（新增）：`Manager` 实现
  `provider.Adapter`，主 Provider 连续不可用达到阈值后切到备用并在同一请求内重试一次；
  上游 5xx 只切换不重放；`context.Canceled` 不计失败；切换后不自动切回。
- `gateway/internal/api/admin.go`、`admin_test.go`（新增）：管理接口与切换审计。
- `gateway/internal/api/tools.go`：审批 prepare/confirm 增加审计与失败关闭；
  `validApprovalToken` 收敛为通用 `validBearerToken`。
- `gateway/internal/api/router.go`：按 `[GATEWAY_PROVIDER, GATEWAY_PROVIDER_BACKUP]` 组装适配器，
  经 `failover.New` 注入 `chatHandler`，挂载 `/v1/admin/provider`。
- `gateway/internal/config/config.go`、`config_test.go`：新增三个 failover 配置项与校验；
  工具工作区默认 `tool-workspace`。
- `gateway/internal/tools/executor.go`：工作区缺失时 `os.MkdirAll(0700)`，是文件则报错。
- `gateway/internal/trace/trace.go`：`ApprovalEvent`/`RecordApproval`、`RecordProviderSwitch`。
- `cases/tool_abuse_cases.json`（新增）、`gateway/internal/api/tool_abuse_cases_test.go`、
  `approval_audit_test.go`（新增）。

## 3. 实测记录

```text
# 自动切换（主 Provider 指向 127.0.0.1:9 不可达，阈值 3）
chat#1 status=502
chat#2 status=502
chat#3 status=200   ← 同一请求内切换 backup 并重试
snapshot: active=mock switch_count=1 last_switch_ms=0 consecutive_failures=0

# 手动切回主 Provider
POST /v1/admin/provider {"provider":"doubao"} → 200, switch_ms=0, 端到端 43 ms
GET  /v1/admin/provider → active=doubao switch_count=2
下一个请求 → 502, consecutive_failures=1（计数已重置）

# 切换审计 trace
{"kind":"provider_switch","previous":"mock","active":"doubao","reason":"manual: admin request","elapsed_ms":0,"switch_count":2}
```

```text
# 全量检查
go vet ./...        → 通过
go test ./...       → 全部 ok（含 failover 8 例、admin 4 例、审批审计、23 条注入样本）
go test -race ./internal/api ./internal/provider/... ./internal/tools ./internal/trace → 通过
```

## 4. 仍未解决（诚实边界）

- 压测只覆盖 `GET /healthz`，不进入 Provider/工具/审批/trace 写入路径，不能代表模型对话链路的
  容量与延迟；生产评估需对 Provider 桩或真实模型链路重测。
- 自动切换本身没有写 `provider_switch` 审计事件，只能通过快照与运行日志核对；手动切换有 trace。
- 工具沙箱仍是应用级白名单（固定可执行文件 + 固定参数 + 超时 + 输出上限），没有 OS 级隔离
  （容器/低权限账号）；该边界在契约与评审文档中保留，不因本次补齐而消失。
- 成本报告是进程内快照，本周无真实模型调用，因此没有生产"单次对话成本"结论。
- 注入样本集覆盖工具接口的参数与审批绕过，不覆盖模型层提示注入（后者属于 A2 的评测集范围）。
