# 第 3 周（2026-11-21 至 11-27）：服务端工程化一：网关与流式

**学习内容**：OpenAI 兼容协议；SSE 流式转发；多 provider 适配层；超时、重试、熔断；幂等键；上下文取消与背压。

**本周交付**：A3 agent-gateway 迭代 1：四个接口的骨架 + 流式转发 + provider 插件化。

**本周预算**：12 小时，按 1 + 2 + 2 + 1 + 3 + 1 + 1 摊，最后 1 小时只补欠账。

| 日期 | 时长 | 当天做什么 | 完成标志 |
| --- | --- | --- | --- |
| 11-21 | 1 小时 | 定接口与目录：`cmd/gateway`、`internal/provider`、`internal/sse`，把四个接口的请求与响应写在 `docs/adr/0002-gateway-api.md` | 接口清单可评审 |
| 11-22 | 2 小时 | 实现 `POST /v1/chat/completions` 兼容层：请求体校验、错误码对齐 OpenAI | curl 打通本地 mock |
| 11-23 | 2 小时 | provider 插件化：定义 Adapter 接口，先写 doubao 一个实现，新增 provider 只改注册表 | 换 provider 只改配置 |
| 11-24 | 1 小时 | SSE 逐块 flush 与首字节计时，禁止把上游响应头原样透传 | mock 下首字节不超过 800 毫秒 |
| 11-25 | 3 小时 | 超时、重试、熔断、幂等键：同一 Idempotency-Key 不重复计费 | 401、429、超时三条路径各有单测 |
| 11-26 | 1 小时 | 写本地 mock 上游与压测脚本（自写或 hey），先跑 50 并发摸上限 | 压测能出 P95、错误率 |
| 11-27 | 1 小时 | 记迭代 1 结果：并发上限、首字节分布、已知缺陷 | 实验记录一段 |

**本周产出**：可演示的流式网关 v1、接口文档、压测初版与首字节数据。

**门槛自检**：mock 上游下 200 并发稳定，首字节达标，客户端无感切换尚未做时可留到迭代 2。

## 现状：本周要在一个纯 Python 仓里开 Go 线

核对时间 2026-11-11，仓库实况如下：

- 代码只有 Python 一条线：`src/agent/{llm,tools,loop,trace,cli,context}`、`tests/`（12 个测试文件）、`scripts/`（6 个脚本），根目录只有 `pyproject.toml` 与 `uv.lock`，没有 `cmd/` 与 `internal/`。
- 计划正文 3.2 写明 A3 用 Go（材料是 openai-go、anthropic-sdk-go 源码、eino 的 ChatModel 抽象、`net/http` 与 `httputil`），所以本周必须引入 Go 工具链，不是继续在 Python 里加服务。
- 表6 的目录约定只覆盖 Python 骨架，并注明 `deploy/` 在阶段二建。Go 的目录规范需要本周自己定下来，写进 ADR。
- 本机 `go version` 为 go1.25.3，`GOPATH` 在 `C:\Users\007\go`，可以直接开工。

## 目录决策五条

0. **不另起仓库，Go 仍放在 `ai_agent` 仓内。** 理由有三条：计划 1.7 已定"后面的项目都在骨架里扩目录，不另起新仓"，分仓等于自己推翻约定；跨语言共用一个仓，`.env` 的价格、trace 的 JSONL 字段、`scripts/replay.py` 这套核对工具才能只维护一份，分仓后第 4 周计费与第 5 周审计都要跨仓抄口径；实验记录与 ADR 按阶段连续编号，分仓会把"第 3 周为什么这么定"从时间线上切断。只有当网关要独立发版、被别的项目当依赖引用，或 CI 单次构建超过十分钟时，才值得拆出去单独建仓。
1. **Go 用独立 module 放在 `gateway/`，不在根目录建 `go.mod`。** 根目录已有 `pyproject.toml`、`uv.lock`，再放一份 `go.mod` 与 `cmd/`、`internal/`，两个工具链的边界会混在一起（`go vet ./...` 与 `uv run pytest` 各自扫的范围重叠），排查"哪条命令扫到了什么"要多花时间。`gateway/go.mod` 的 module 路径写 `github.com/carder-coder/ai-agent/gateway`，导入路径自解释。
2. **只建网关这条线的最小目录。** `/v1/tools/execute` 留到第 5 周、计费与路由留到第 4 周，本周不预建 `internal/tools`、`internal/cost`、`internal/cache`。空目录 git 也不跟踪，先建等于制造以后要删的空壳。
3. **`deploy/` 按表6 在阶段二建，但本周只放网关一份。** 一份 `Dockerfile` 加一份 `docker-compose.yml`，等第 4 周有真实依赖（Redis、PostgreSQL）再往里加服务，不提前为 A4、A5 铺位。
4. **以后 A5 的 Go MCP Server 同样走独立 module（`mcp/`）。** 本周只在 ADR 里写一句约定，不建目录，避免三个"以后再说"的空壳同时出现。

## 新增目录树

树形（`+` 表示本周新建）：

```
+ gateway/                       独立 Go module，A3 全部代码在这里
+   go.mod  go.sum
+   cmd/gateway/main.go          装配：读配置、挂路由、优雅关停
+   cmd/loadtest/main.go         并发压测，复用 sse.Reader，输出 P50/P95/P99
+   internal/config/config.go    读根 .env 的 ARK_* 与端口、超时、并发上限
+   internal/api/router.go       路由注册，本周只有 chat 与 metrics 两条
+   internal/api/chat.go         /v1/chat/completions：解析、校验、分流 stream
+   internal/api/errors.go       OpenAI 风格错误体与状态码映射
+   internal/api/middleware.go   trace_id 注入、panic 恢复、请求日志、并发限流
+   internal/provider/provider.go   Adapter 接口（Chat 返回 chunk 流 + usage）
+   internal/provider/registry.go   名字到 Adapter 的注册表，主备切换的钩子位
+   internal/provider/doubao/doubao.go  豆包实现：base_url、鉴权、错误码映射
+   internal/provider/mock/mock.go      本地 mock 上游：可控延迟、可控 429、可控中断
+   internal/sse/reader.go       解析上游 SSE（data:、[DONE]、多行 data）
+   internal/sse/writer.go       逐块 flush、心跳、按 ctx 检测客户端断开
+   internal/retry/retry.go      1/2/4 秒退避，只对未开始输出流的失败重试
+   internal/metrics/metrics.go  QPS、首字节、延迟直方图、错误率（手写，无外部依赖）
+   internal/trace/trace.go      trace_id 生成与 JSONL 落盘，字段对齐 Python 侧
+ deploy/
+   Dockerfile                   多阶段构建，产物小于 30MB
+   docker-compose.yml           先只放 gateway 一个服务
  docs/adr/0002-gateway-api.md   四接口契约、错误码、兼容边界
 docs/adr/0003-provider-adapter.md  Adapter 接口与插件化规则（第 17 天写）
```

测试按 Go 惯例与被测代码同目录，不另建 `gateway/tests`：`api/chat_test.go`、`api/errors_test.go`、`sse/reader_test.go`、`provider/registry_test.go`、`retry/retry_test.go`。逐日实验记录仍放 `docs/experiments/`，编号接续到 `2026-11-21-day15.md` 至 `2026-11-27-day21.md`。

## 逐日落地

| 日 | 新增文件 | 当天结束时应能跑的命令 |
| --- | --- | --- |
| Day 15（11-21） | `gateway/go.mod`、`cmd/gateway/main.go`、`internal/config`、`internal/api/{router,errors}.go`、`docs/adr/0002-gateway-api.md` | `go run ./cmd/gateway` 起得来，`curl -s localhost:8080/healthz` 返回 200 |
| Day 16（11-22） | `internal/api/chat.go`、`internal/provider/{provider,doubao}.go`、`api/errors_test.go` | 非流式一次真调豆包返回 OpenAI 结构；故意发非法请求体得到 400 与字段级原因 |
| Day 17（11-23） | `internal/provider/registry.go`、`internal/provider/mock/mock.go`、`provider/registry_test.go`、`docs/adr/0003-provider-adapter.md` | 改一行配置把 provider 从 doubao 换成 mock，命令不变 |
| Day 18（11-24） | `internal/sse/{reader,writer}.go`、`internal/metrics/metrics.go`、`sse/reader_test.go` | 流式请求每块即时可读（`curl -N` 肉眼可见逐行到达），mock 下首字节小于 800 毫秒 |
| Day 19（11-25） | `internal/retry/retry.go`、`internal/api/middleware.go`、`api/chat_test.go`、`retry/retry_test.go` | 401、429、超时三条路径各有断言；同一 `Idempotency-Key` 第二次请求不再打上游 |
| Day 20（11-26） | `cmd/loadtest/main.go`、`deploy/{Dockerfile,docker-compose.yml}` | `go run ./cmd/loadtest --n 200 --c 200` 出 P95 与错误率；`docker compose up` 起得来 |
| Day 21（11-27） | `internal/trace/trace.go`、`/metrics` 暴露、`docs/experiments/2026-11-27-day21.md` | `python -m uv run python scripts/replay.py --trace gateway/traces/<id>.jsonl` 能读通 Go 写的 trace |

## 与 Python 侧的三个对齐点

1. **trace 字段对齐，让 `scripts/replay.py` 直接能读 Go 写的 trace。** 事件名沿用 `run_start`、`llm`、`tool`、`run_end`；`llm` 里保留 `first_byte_ms`、`usage{in,out,total,cached}`、`attempts`；`cost_yuan` 第 4 周才填，本周先写 0 并注明"待计费"。这条不是洁癖：跨语言共用同一个重放器，等于用一份工具核对两条线的数字。
2. **错误体对齐 OpenAI**：`{"error": {"message", "type", "code"}}`。映射固定为上游 401 原样 401（`authentication_error`）、429 原样 429 并带 `Retry-After`、超时 504（`timeout`）、上游 5xx 转 502（`upstream_error`）、请求体非法 400（`invalid_request_error`）。客户端 SDK 只认这套，自创格式等于以后每接一个客户端改一次。
3. **价格不在 Go 代码里写常量。** 从根 `.env` 的 `ARK_PRICE_IN`、`ARK_PRICE_OUT`、`ARK_PRICE_CACHED` 读进 config 结构体，第 4 周计费直接用。Python 侧已经踩过"模块级常量早于 .env 求值"的坑，Go 这边一开始就把读取动作放在启动装配里。

## 本周不建的东西

`internal/loop`（Agent 循环仍在 Python 侧）、`internal/tools`（第 5 周）、`internal/cost` 与 `internal/cache`（第 4 周）、`mcp/`（第 9 周）、`eval/`（第 12 周）、`gateway/internal/metrics` 之外的任何观测后端（Prometheus 服务端不上机，`/metrics` 出文本即可）。

## 开工前三件事

1. `go version` 确认版本（本机 1.25.3），`go mod init github.com/carder-coder/ai-agent/gateway`。
2. `.gitignore` 补三条：`/gateway/bin/`、`*.test`、`gateway/traces/`；`go.sum` 必须入库，不能忽略。
3. README 的目录一节补 `gateway/` 与 `deploy/` 两行，写清"Python 是 Agent 侧，Go 是服务侧"，避免以后自己都记混哪条线在哪。

## 本周目录验收

```powershell
cd gateway
go vet ./...
go test ./...
go run ./cmd/gateway                                   # 另开一个窗口
curl -N localhost:8080/v1/chat/completions -d "{...}"   # 逐块到达
go run ./cmd/loadtest --n 200 --c 200                   # P95 与错误率
cd ..
python -m uv run python scripts/replay.py --trace gateway/traces/<id>.jsonl
```

四条命令全过，才算"目录结构可用"，而不是"文件创建成功"。

