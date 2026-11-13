# 第 5 周压测报告：200 并发 × 30 分钟

- 执行时间：2026-11-13 09:38:42 → 10:08:42（UTC+8）
- 被测对象：`gateway/cmd/gateway` 构建的 `gateway.exe`（仓库 HEAD `fa2bd98` 构建），mock Provider，端口 18080
- 压测工具：仓库内 `gateway/cmd/loadtest`（`--duration 30m --c 200 --method GET`）
- 结论：**0 错误完成 30 分钟 200 并发**；**主备切换实测 0 ms（接口口径）/ 43 ms（含 HTTP 往返），远低于 3 秒门槛**

## 1. 结果

```text
duration=30m0s requests=5470182 concurrency=200 success=5469986 errors=0 cancelled=196 error_rate=0.00% rps=3038.93 P50=53.90ms P95=142.10ms P99=224.10ms
```

| 指标 | 实测值 | 说明 |
| --- | --- | --- |
| 压测时长 | 30m0s | 由 `--duration 30m` 固定，非人工估算 |
| 总请求数 | 5,470,182 | 30 分钟内发起 |
| 成功 | 5,469,986 | HTTP 2xx 且响应体读取成功 |
| 错误 | 0 | `error_rate=0.00%` |
| 取消 | 196 | 截止时刻仍在途、被上下文取消，不计入成功/失败 |
| 平均吞吐 | 3,038.93 rps | 总请求数 ÷ 实际耗时 |
| P50 / P95 / P99 | 53.90 / 142.10 / 224.10 ms | 100 µs 分桶直方图的 nearest-rank 估算 |

## 2. 可复现步骤

```powershell
cd gateway
go build -o gateway.exe ./cmd/gateway
go build -o loadtest.exe ./cmd/loadtest

# 终端 1：mock Provider，端口 18080，并发上限 200
$env:GATEWAY_PROVIDER="mock"; $env:GATEWAY_PORT="18080"
$env:GATEWAY_MAX_CONCURRENCY="200"; $env:GATEWAY_TRACE_DIR=".tmp\traces"
.\gateway.exe

# 终端 2：30 分钟、200 并发、GET /healthz
.\loadtest.exe --duration 30m --c 200 --method GET --url http://127.0.0.1:18080/healthz
```

持续模式的硬限制由 `gateway/cmd/loadtest/main.go` 强制：目标必须是 localhost/loopback，
并发 ≤200，时长 ≤30 分钟；违反任一条直接退出码 2，因此这组参数不可被悄悄放大。

## 3. 环境

| 项目 | 值 |
| --- | --- |
| 机器 | 12th Gen Intel Core i3-12100，8 逻辑核 |
| 操作系统 | Windows 11 |
| Go | go1.25.3 windows/amd64 |
| 网关 | mock Provider，`GATEWAY_MAX_CONCURRENCY=200` |
| 压测目标 | `GET /healthz`（不进 Provider、工具、审批、trace 写入路径） |

## 4. 口径与限制（必须与本报告一起引用）

- 本轮压的是**网关 HTTP 入口健康检查**，度量的是路由、限流、中间件与连接处理的开销，
  **不代表**模型对话链路（`/v1/chat/completions`）的真实延迟与成本；生产容量评估需对
  Provider 桩或真实模型链路重测。
- `cancelled=196` 是 30 分钟截止时在途请求被取消，属于压测工具的正常收尾，不是错误。
- P50/P95/P99 来自 100 µs 分桶直方图，桶宽决定最大误差 0.1 ms；不是逐样本全量排序。
- 单机自压：压测进程与被测进程同机竞争 CPU，结果反映"同机可承受上限"，不能直接外推为
  多机集群容量。
- 与第 5 周旧数据（1550 万请求、P99 134.40 ms）不可比：旧数据来自已删除的临时 harness，
  无命令行、无可复现步骤，本报告起以本文件的命令为准，旧数据作废。

## 5. 附：Provider 主备切换实测（12-10 验收项）

设置 `GATEWAY_PROVIDER=doubao`（`ARK_BASE_URL=http://127.0.0.1:9` 不可达）、
`GATEWAY_PROVIDER_BACKUP=mock`、`GATEWAY_FAILOVER_THRESHOLD=3`、`GATEWAY_ADMIN_TOKEN=<32+ 字符>`，
对 `/v1/chat/completions` 连发 3 次：

| 请求 | 状态 | 说明 |
| --- | --- | --- |
| chat#1 | 502 `upstream_error` | 主 Provider 失败，连续失败计数 1 |
| chat#2 | 502 `upstream_error` | 连续失败计数 2 |
| chat#3 | 200（mock 响应） | 连续失败达阈值 3，**同一请求内切到备用并重试一次** |

自动切换后快照：

```json
{"active":"mock","primary":"doubao","backup":"mock","threshold":3,"switch_count":1,
 "last_reason":"auto: consecutive upstream failures","last_switch_ms":0,"consecutive_failures":0}
```

手动切回主 Provider：

```text
POST /v1/admin/provider {"provider":"doubao"} → 200
{"previous":"mock","snapshot":{"active":"doubao",...,"switch_count":2,"last_switch_ms":0}, "switch_ms":0}
```

- 接口内计时 `switch_ms = 0`，含 HTTP 往返的端到端耗时 43 ms，均远小于 3 秒门槛。
- 切换写入 trace，可用 `GET /v1/trace/{id}` 复核：

```json
{"kind":"provider_switch","previous":"mock","active":"doubao","reason":"manual: admin request","elapsed_ms":0,"switch_count":2}
```

- 切回不可达主 Provider 后的下一个请求返回 502 且 `consecutive_failures=1`，说明计数器在切换时
  已清零、切换后重新累计，不会因旧计数立刻再次弹回备用。

## 6. 核对命令

```powershell
cd gateway
go test ./internal/provider/failover ./internal/api -run 'Failover|AdminProvider|ChatAuto|ManualSwitch' -v
go test -race ./internal/api ./internal/provider/... 
```
