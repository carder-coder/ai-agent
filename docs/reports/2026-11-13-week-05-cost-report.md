# 第 5 周成本报告

- 报告时间：2026-11-13（UTC+8）
- 数据来源：网关 `GET /cost-report`（进程内快照）+ `traces/*.jsonl` 的 `run_end` 用量字段
- 结论：**本周开发与压测全部走 mock Provider / 健康检查，真实模型对话 0 次，实际成本 ¥0**；
  成本核算链路（每千次对话成本、分模型占比、缓存命中率）已可跑通并有单测覆盖

## 1. 成本口径

`GET /cost-report` 返回当前进程启动以来的累计快照：

| 字段 | 计算方式 | 空窗口约定 |
| --- | --- | --- |
| `conversation_count` | 本进程完成的对话次数 | 0 |
| `total_cost_yuan` | 各次对话按 `usage` 与价格表累加 | 0 |
| `cost_per_1000_conversations_yuan` | `total_cost_yuan / conversation_count × 1000` | 0（不做除零） |
| `model_cost_shares` | 单模型成本 ÷ 总成本 | `[]` |
| `semantic_cache_hit_ratio` | 命中数 ÷ 可缓存查询数 | 0 |

预算护栏独立于成本报告：`GATEWAY_DAILY_BUDGET_YUAN`、`GATEWAY_REQUEST_MAX_COST_YUAN`、
`GATEWAY_BUDGET_MAX_OUTPUT_TOKENS` 在请求前估算并拒绝超预算请求，状态落
`GATEWAY_BUDGET_STATE_FILE`。

## 2. 本次实测

mock Provider（端口 18085，`GATEWAY_PROVIDER=mock`）启动后直接查询：

```powershell
curl.exe -s http://127.0.0.1:18085/cost-report
```

```json
{"period_start":"2026-11-13T02:11:02.2196223Z","generated_at":"2026-11-13T02:11:04.2730103Z",
 "conversation_count":0,"total_cost_yuan":0,"cost_per_1000_conversations_yuan":0,
 "model_cost_shares":[],"semantic_cache_hit_ratio":0}
```

| 项目 | 实测值 | 说明 |
| --- | --- | --- |
| 真实模型对话数 | 0 | 全部请求由 mock Provider 应答 |
| Provider 成本 | ¥0 | 未产生计费 token |
| 每千次对话成本 | 不适用（接口按约定返回 0） | 无对话，不能外推生产单价 |
| 分模型成本占比 | 不适用（空数组） | 无模型调用 |
| 语义缓存命中率 | 不适用（接口按约定返回 0） | 无缓存查询 |
| 30 分钟压测请求 | 5,470,182 次 `GET /healthz` | 不计作模型对话，成本 ¥0 |

## 3. 成本链路可核算的验证

```powershell
cd gateway
go test ./internal/metrics ./internal/api -run 'Cost|Budget' -v
```

- `gateway/internal/metrics` 的成本报告测试用两次模拟对话、总成本 ¥0.10 验证：每千次成本
  ¥50、模型占比 30%/70%、缓存命中率 1/4；空窗口下各字段按上表约定返回 0 / 空数组。
- `gateway/internal/api/cost_report_test.go` 覆盖 `GET /cost-report` 的 HTTP 输出与方法限制。
- 每次对话的 `run_end` 写入 `total_cost_yuan` 与 `usage{in,out,total,cached}`，可按 trace 逐个
  复核，不依赖进程内计数。

## 4. 边界与后续

- 成本报告是**进程内累计快照**，服务重启即清零，不能替代跨重启的持久化账单；跨周期核算需
  汇总 trace 的 `run_end` 或接入真实账单。
- 本周没有任何真实模型调用，因此本报告**不提供**生产环境的"单次对话成本"结论；如需该结论，
  应用真实 Provider 跑一批标注样本后重算，并在报告中注明模型、价格表与样本量。
- 语义缓存命中率只覆盖可缓存查询，压缩/流式等不走缓存的请求不计入分母。

## 5. 与旧记录的差异

第 5 周此前只有一篇"压测与成本"合成记录（`docs/experiments/2026-12-11-day7.md`）。本报告把它
拆成独立的成本报告，压测部分见
[2026-11-13-week-05-loadtest-report.md](2026-11-13-week-05-loadtest-report.md)，两份文档各自
带可复现命令与边界声明。
