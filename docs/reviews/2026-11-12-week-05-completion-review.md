# 第 5 周（A3 迭代 3：工具执行与审批）完成情况评审



* 评审对象：`docs/plan/week-05.md` 的 7 天任务与当前仓库实现

* 评审日期：2026-11-12

* 评审方式：读代码 + `go vet ./...`、`go test ./...`、`go test -race ./internal/api ./internal/tools ./internal/trace` + 用 mock Provider 实启动网关做端到端请求 + 5 秒短时压测；不改动任何代码与数据

* 提交范围：`17be1d8`（第 1 天）至 `fa2bd98`（第 7 天），7 次提交全部落在 2026-11-12，工作区干净

## 一句结论

工具执行这条主链路是**真的通了**：注册表 + JSON Schema 字段级校验、只读命令白名单、写操作默认关 + 双密钥审批、脱敏审计、`GET /v1/trace/{id}` 合并子调用，五项都有代码、有测试、有实跑证据，`go test ./...` 与三个新包的 `-race` 全过。

但本周交付有三处**没兑现**：**Provider 主备切换（3 秒指标）完全没实现**；**30 分钟压测的原始结果无法用仓库复现**（临时 harness 已删，只剩一张表）；**"两份报告" 只落了一份实验记录**，压测报告没有独立成文。三处都不影响主链路可用，但按计划自己的验收口径（"文件、命令或 trace 可核对"）判定，第七天与第六天各缺一项。

## 一、按天逐项核对



| 计划项                                   | 完成标志             | 实现核对                                                                                                                                                                                 | 判定                                 |
| ------------------------------------- | ---------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ---------------------------------- |
| 12-05 定 `/v1/tools/execute` 契约与错误码    | 契约可评审            | `docs/design/tool-execution-api.md` 54 行；`errors.go` 统一 envelope；实测 400/404/200 与文档一致（`invalid_tool_arguments`、`tool_not_found`）                                                     | 通过（审批系列错误码未逐条写入契约，见 P2-1）          |
| 12-06 JSON Schema 校验 + 长度 / 类型 / 枚举上限 | 非法参数一律拒绝并返回字段级原因 | `tools/schema.go` 支持 `type/properties/required/items/enum/maxLength/maxItems/pattern/additionalProperties`，拒绝未支持关键字；`maxLength` 按 `utf8.RuneCountInString` 计；实测一条请求返回 3 条字段原因且按字段名排序 | 通过                                 |
| 12-07 沙箱：命令白名单、超时、输出上限、禁止拼 shell      | 越权命令被拒且有审计       | `tools/registry.go` 为唯一工具清单；`executor.go` 用 `exec.CommandContext` + 固定 argv，`Env` 只留 `PATH/SYSTEMROOT/WINDIR` 并关 Git 全局配置；3s 超时、16 KiB 截断；实测 `format="short; whoami"` 被 400 拒        | 通过（属应用级白名单，无 OS 级隔离，见 P1-2）        |
| 12-08 危险操作分级与人工审批                     | 审批链路可演示          | 写工具默认关，需 `GATEWAY_TOOLS_ALLOW_WRITES=true` + ≥32 字符密钥；审批绑定 caller/tool/ 参数摘要，TTL 5 分钟、单次消费；实测未审批 404、无密钥确认 401、确认后 200、重放 404、`../evil.txt` 400                                      | 通过（分级仅 "读 / 写" 两级，审批动作未落审计，见 P2-2） |
| 12-09 审计日志 + `GET /v1/trace/{id}` 串联  | 任一次对话可还原全部子调用    | 工具侧写 `tool_start`/`tool` 事件（caller、脱敏参数、结果摘要、耗时、status）；实测对话 trace 查询返回 `run_start → cache → tool(workspace_status) → run_end`，工具事件确在 `run_end` 之前                                   | 通过                                 |
| 12-10 200 并发 30 分钟压测 + 摘主接入点          | 3 秒内完成切换         | 持续压测 CLI 已落地（限 [localhost](https://localhost)、≤200 并发、≤30 分钟、有界直方图）；30 分钟数据见记录；**Provider 主备切换未实现**，`router.go` 启动时绑定单个 Adapter，无备用配置与热切换                                            | **部分不通过**（切换项未做，记录已如实标注）           |
| 12-11 两份报告定稿                          | 压测报告、成本报告        | `/cost-report` 可用且口径写清（空窗口返回 0、进程内快照不跨重启）；压测数据只在第六天记录里一张表，没有独立报告文件                                                                                                                   | **部分不通过**（两份文档只落一份）                |

## 二、实跑证据（本次核对的接口行为）

用 `GATEWAY_PROVIDER=mock`、独立 trace 目录与临时 git 工作区启动网关，真实 HTTP 请求结果：



| 用例                                | 结果                                                                                                  |
| --------------------------------- | --------------------------------------------------------------------------------------------------- |
| `inspect_text` 合法调用               | 200，`{"characters":8,"words":2}`，响应头 `X-Trace-ID` 与 body `trace_id` 一致                              |
| 参数类型 / 枚举 / 多余字段错误                | 400 `invalid_tool_arguments`，details 为 `arguments.extra`/`arguments.mode`/`arguments.text` 三条       |
| `format="short; whoami"`          | 400，`arguments.format: must be one of the allowed values`（未进 shell）                                 |
| 未注册工具 `powershell`                | 404 `tool_not_found`                                                                                |
| `GET /v1/trace/{id}`              | 200 `application/x-ndjson`，含 `run_start`/`tool_start`/`tool`/`run_end`；`text` 参数只留 `bytes`+`sha256` |
| 非法 / 不存在 trace ID                 | 404 `trace_not_found`（`..` 路径被路由层拒绝）                                                                |
| `write_note` 未提交审批                | 404 `approval_not_found`，未创建文件                                                                      |
| 审批无密钥确认 / 有密钥确认                   | 401 `invalid_approval_token` / 200 `confirmed`                                                      |
| 已审批执行 / 同一 `approval_id` 重放       | 200 `bytes_written:5` / 404 `approval_not_found`                                                    |
| 文件名 `../evil.txt`                 | 400，schema `pattern` 拒绝                                                                             |
| 对话 + 子工具合并                        | `GET /v1/trace/{对话 ID}` 输出内含 `tool` 事件且位于 `run_end` 前                                               |
| `GET /cost-report`                | 200，`conversation_count:2`、`total_cost_yuan:0`、`semantic_cache_hit_ratio:0.5`                       |
| 短时压测（5s / 20 并发 / GET `/healthz`） | `requests=46502 errors=0 rps=9299.77 P50=2.00ms P95=3.30ms P99=4.70ms`                              |
| 压测非 loopback 目标                   | 拒绝，`duration mode only accepts localhost or loopback IP targets`，退出码 2                              |

## 三、问题清单

### P1-1 30 分钟压测结果不可复现，"能跑压测" 只能按小规模核对

`docs/experiments/2026-12-10-day6.md` 的 15,519,281 请求、P99 134.40 ms 来自一个 "执行后已移除" 的临时 `go test` harness，仓库里没有脚本、没有原始输出文件，只有表格。计划约定 "所有验收都要求能用文件、命令或 trace 核对"，这一项目前只能靠 CLI 跑短时压测自证（本次 5 秒跑通），30 分钟那组数字谁都无法重放。

另外这组数字压的是 `/healthz`，不是 `/v1/chat/completions`：它衡量的是 Router + 中间件的 HTTP 开销，**不含模型上游延迟、SSE 流式、语义缓存与预算检查**。记录里已声明边界，但如果把它当作 "网关容量已达标" 的证据，结论会偏乐观。

建议：把压测 harness 以 `gateway/cmd/loadtest` 的 `-duration` 模式固化（已具备），补一次可保存原始输出的运行（`--duration 30m --c 200`，输出重定向到文件），并在报告里明确 "这是健康检查路径基线"。

### P1-2 "沙箱" 目前只是应用级白名单，工作区默认值偏大

`Executor` 侧做得对：固定 argv、无 shell 解释、环境变量白名单、3 秒超时、16 KiB 截断。但没有 OS 级资源限制（CPU / 内存 / 网络 / 文件系统读写范围），`workspace_status` 之外的隔离全靠 "注册表里只有这一个命令"。

更实际的风险是 `GATEWAY_TOOL_WORKSPACE` 默认 `..`：从 `gateway/` 启动时落在**仓库根目录**，开启写工具后 `write_note` 的落点就是仓库根（与 `.env`、`README.md` 同级）。写工具虽然默认关且要审批，但默认值本身不该指向敏感目录。

建议：默认改为显式的 `tool-workspace/` 子目录，或不设默认、缺失即启动失败；文档里补一句 "沙箱边界 = 注册表 + argv + 环境变量 + 超时 + 输出上限，不含 OS 级隔离"。

### P2-1 契约文档没有覆盖审批相关错误码

`tool-execution-api.md` 列了 `invalid_tool_arguments`/`tool_not_found`/`tool_timeout`/`tool_execution_failed`/`trace_not_found`/`trace_too_large`，但 `approval_not_found`、`approval_expired`、`approval_not_confirmed`、`approval_mismatch`、`invalid_approval_token`、`caller_id_required` 六个码在实现里存在、文档里没有。调用方只能读代码猜状态码（409 与 404 混用）。

建议：契约里补一张错误码表（码、HTTP 状态、触发条件），与 `writeApprovalError` 一一对应。

### P2-2 审批动作本身没有审计，确认人身份不落库

`approvals.go` 的 `Create`/`Confirm` 只操作内存表，不写任何 trace 事件；`ToolEvent.Caller` 记录的是**执行方**，审批密钥持有者是谁、何时确认的，事后查不到。`trace` 里能看到 "谁调用了写工具"，看不到 "谁批的"。

同时审批状态在进程内：网关重启后未消费的审批全部丢失，多副本部署下也不能跨实例确认。这与 "人工审批流" 的目标有差距。

建议：至少给 `prepare`/`confirm` 写一条轻量审计事件（approval\_id、工具、参数摘要、确认结果），并在文档里声明 "审批状态不跨进程 / 不跨实例"。

### P2-3 README 与 `.env.example` 没跟上本周接口

README 的网关段落仍只讲 `/v1/chat/completions`、`/metrics`、`X-Trace-ID` 与压测，没有 `/v1/tools/execute`、`/v1/tools/approvals`、`/v1/trace/{id}`、`/cost-report`；`.env.example` 末尾 `ARK_PRICE_IN/OUT/CACHED` 重复了一遍，`GATEWAY_TOOL_WORKSPACE=..` 的解释也只写在注释里。契约文档写了，入口文档漏了。

### P3-1 "越权与注入样本全部拦截" 缺少注入侧样本集

越权侧覆盖是够的：未审批、错 caller、改参数、重放、路径穿越、未知工具、命令拼接都有测试与实测。但计划里点名的 **OWASP LLM Top 10 提示注入与过度授权**，没有独立的注入样本集与结论记录 —— 本周只做到了 "命令层不可注入"，"模型被提示注入后诱导调用越权工具" 这一层没有用例。周产出的这句话目前证据不足。

## 四、本周产出与门槛自检



| 本周产出要求               | 实际                                                                                                                                | 判定              |
| -------------------- | --------------------------------------------------------------------------------------------------------------------------------- | --------------- |
| 四个接口可用               | 实跑可用：`/v1/chat/completions`、`/v1/tools/execute`、`/v1/tools/approvals`（含 `/confirm`）、`/v1/trace/{id}`，另有 `/cost-report`、`/metrics` | 通过              |
| 压测与成本报告齐全            | 成本口径 + 空窗口说明在 day7 记录；压测只有 day6 记录一张表                                                                                             | **部分**（见 P1-1）  |
| 越权与注入样本全部拦截          | 越权拦截有测试与实测；注入样本缺失                                                                                                                 | **部分**（见 P3-1）  |
| 能跑压测                 | CLI 有 `-duration` 模式，5 秒跑通；30 分钟数据不可复现                                                                                            | 部分              |
| 成本可核算                | 按 usage 计费、精度 0.0001 元、`/cost-report` 可用；本周真实调用 0 次，成本恒为 0                                                                        | 通过（口径可用，数据无代表性） |
| trace 可还原            | 对话 trace 合并子工具调用，实测通过                                                                                                             | 通过              |
| 审批完成（未完成可只留只读工具进阶段三） | 审批已完成并演示，超出计划下限                                                                                                                   | 通过              |

## 五、记录与流程缺口



* 12-05 至 12-09 五天**没有&#x20;**`docs/experiments`**&#x20;日记录**，`17be1d8`～`1b66c43` 五个提交集中在 11-12 的 10:14–10:36 完成，只有第六、七天补了记录。计划要求 "每天收工补步数与成本两个数字"，这五天补不出来。

* 第六天记录没有写工时与步数，第七天写了 "真实模型对话 0 / Provider 成本 ¥0"，属于如实记录，但周预算 12 小时无法核对。

* 提交拆分是干净的：代码与记录分开提交，符合仓库约定；`traces/`、`out/`、`.env` 未入库。

## 六、核对命令



```
cd gateway
go vet ./...
go test ./...
go test -race ./internal/api ./internal/tools ./internal/trace
go run ./cmd/loadtest --duration 5s --c 20 --method GET --url http://127.0.0.1:8080/healthz
```

端到端复核要点：`/v1/tools/execute` 非法参数应返回 400 且 details 到字段；`format="short; whoami"` 必须 400；`write_note` 未审批不得创建文件；同一 `approval_id` 第二次必须失败；`GET /v1/trace/{对话 ID}` 应能看到 `tool` 事件在 `run_end` 之前。