# AI Agent 工程化学习计划 · 工程仓

A1 最小闭环：模型适配、工具注册、循环状态机、调用链追踪、命令行入口。
所有模型调用走豆包（火山方舟）API，工具只读，先跑通再扩能力。

## 环境

- 开发机：Windows 11，Python 3.13，uv 管理依赖
- 运行机：Debian 12，4 核 8G，无 GPU
- 凭证：`ARK_API_KEY` 只放 `.env`，仓库只保留 `.env.example`

## 安装

```powershell
git clone <repo>   # 或在已有目录
cd C:\Users\007\code\guohe\ai_agent
python -m pip install uv          # 若 uv 未装
uv sync                            # 建 .venv 并按 uv.lock 装依赖
copy .env.example .env             # 填入 ARK_API_KEY、ARK_MODEL
```

## 运行

```powershell
uv run python scripts/probe.py                                  # 连通性四项自检
uv run python -m agent.cli --task "统计本仓库 TODO 与 FIXME 数量并列出前十条" --max-steps 12
uv run python -m agent.cli --task "离线冒烟" --provider fake     # 无密钥时验证循环
uv run python -m agent.cli --task "读 README 并总结" --context-strategy window --context-window 2
uv run python -m agent.cli --task "读 README 并总结" --context-strategy summary --summary-threshold 3000
uv run python -m agent.cli --task "读 README 并总结" --context-strategy retrieval --retrieval-budget 3000
uv run python -m agent.cli --resume traces\<run_id>.jsonl --max-steps 12 # 从中断处继续
uv run pytest -q                                                # 单测
uv run pytest --cov=agent --cov-report=term-missing -q           # 单测与覆盖率
uv run ruff check .                                             # 静态检查
uv run python scripts/replay.py --trace traces\<run_id>.jsonl  # 核对单次 trace
uv run python scripts/check_cases.py                            # 校验 50 条评测集
uv run python scripts/run_cases.py --strategy full --dry-run    # 只看选中哪些用例
uv run python scripts/run_cases.py --strategy window --sample-per-category 1
uv run python scripts/run_cases.py --strategy summary --summary-threshold 3000 --limit 1
uv run python scripts/press_429.py --model <mini-model-id> --concurrency 8 --rounds 20
```

trace 落在 `traces/<run_id>.jsonl`，每行一个事件（llm / tool / finish / run_end），
`run_end` 带本次总 token 与成本估算。每次模型响应和工具执行后会把消息、步数与累计用量
保存到同名 `.state.json`；Ctrl+C 后可传 trace 或 state 路径恢复，已完成的工具调用不会重放。
`--prompt-variant concise` 可切换精简系统提示，与默认 `detailed` 做同任务对照。
`press_429.py` 支持 `--model`、`--concurrency`、`--rounds`、`--endpoint`，默认 8 并发、每路 20 次；
它会真实发送最多 160 次模型请求并产生 API 费用。验证限流时优先指定低配额模型，仅在需要时运行。

## 网关（第 3–5 周，`gateway/`）

Go 模块，入口 `gateway/cmd/gateway`；默认豆包，可切 `mock` 离线验证。

```powershell
cd gateway
$env:GATEWAY_PROVIDER="mock"; $env:GATEWAY_PORT="18080"   # 离线验证
go run ./cmd/gateway
```

| 接口 | 说明 |
| --- | --- |
| `POST /v1/chat/completions` | 对话入口，按任务类型路由模型，写 `llm` trace |
| `POST /v1/tools/execute` | 只执行注册工具（`inspect_text`/`workspace_status`/`write_note`），参数按 JSON Schema 校验 |
| `POST /v1/tools/approvals` | 为写工具参数申请短时审批号（5 分钟、单次消费、绑定调用方与参数摘要） |
| `POST /v1/tools/approvals/{id}/confirm` | 审批人用 `Authorization: Bearer <GATEWAY_TOOL_APPROVAL_TOKEN>` 明确确认 |
| `GET /v1/trace/{id}` | 读取单次 trace JSONL（上限 8 MiB），可还原工具子调用与成本 |
| `GET|POST /v1/admin/provider` | 查询/手动切换 Provider；需 `GATEWAY_ADMIN_TOKEN`，未配置时返回 404 |

```powershell
# 工具执行（只读）
curl.exe -s -X POST http://127.0.0.1:18080/v1/tools/execute `
  -H "Content-Type: application/json" `
  -d '{"tool":"inspect_text","arguments":{"text":"hello world"}}'

# 主备切换：环境变量 GATEWAY_PROVIDER_BACKUP=mock、GATEWAY_ADMIN_TOKEN=<32+ 字符>
curl.exe -s -X POST http://127.0.0.1:18080/v1/admin/provider `
  -H "Authorization: Bearer $env:GATEWAY_ADMIN_TOKEN" `
  -H "Content-Type: application/json" -d '{"provider":"backup"}'
```

Provider 主备切换：主 Provider 连续不可用达到 `GATEWAY_FAILOVER_THRESHOLD`（默认 3）次后，
在同一请求内切到备用并重试一次；上游 5xx 只切换不重放；切换后不会自动切回，只能手动切回。
配置项见 `.env.example`（`GATEWAY_PROVIDER_BACKUP`、`GATEWAY_FAILOVER_THRESHOLD`、
`GATEWAY_ADMIN_TOKEN`）。

```powershell
go test ./...                     # 全量单测
go vet ./...                      # 静态检查
go test -race ./internal/api ./internal/provider/... ./internal/tools

# 压测（持续模式仅允许 localhost，≤200 并发、≤30 分钟）
go build -o loadtest.exe ./cmd/loadtest
.\loadtest.exe --duration 30m --c 200 --method GET --url http://127.0.0.1:18080/healthz
```

工具工作区默认 `tool-workspace`，目录缺失时按 0700 自动创建；写工具默认关闭，开启需同时设置
`GATEWAY_TOOLS_ALLOW_WRITES=true` 与 ≥32 字符的 `GATEWAY_TOOL_APPROVAL_TOKEN`。
越权与注入样本集在 `cases/tool_abuse_cases.json`，由 `gateway/internal/api/tool_abuse_cases_test.go` 逐条断言。

## 上下文策略（第 2 周）

`--context-strategy` 一个参数切换四种装配方式，系统提示、工具 schema 与提示模板都不变：

| 策略 | 行为 | 适用 |
| --- | --- | --- |
| `full`（默认） | 每次请求携带全部 `messages` | 历史短，需要完整证据链 |
| `window` | 保留 system + 首条 user 作为锚点，再保留最近 `--context-window` 轮（默认 2）；更早的轮次整轮丢弃，工具调用与其结果不拆开 | 步数多，需要控制上下文预算 |
| `summary` | 估算上下文字符数超过 `--summary-threshold`（默认 3000）时，把更早的轮次交给同一模型压缩，保留锚点、摘要与最近 2 轮；未超阈值等同 `full` | 多步任务需要压预算，同时尽量保留早期信息 |
| `retrieval` | 锚点与最近 1 轮固定保留；最近轮最多占消息预算 60%，其余轮次用 BM25 对任务查询打分，按分数选到 `--retrieval-budget`（默认 2500 token，含工具定义）内；整轮放不下时允许截断工具输出，不再整轮丢弃 | 历史里只有少数片段与当前任务相关 |

摘要的约束：

- 提示按版本固定：`v1` 当前默认，`v0` 旧版保留用于回滚；`--summary-version v0` 切换，版本写入 trace 与断点。
- 摘要用同一个 client/model、不挂工具；默认 `max_tokens=800`，空正文且 `finish_reason=length` 时翻倍重试一次（上限 2000），两次用量合并计费。
- 摘要失败（上游异常或空输出）时本任务内停用摘要并降级为 `window`，记 `summary_error`；失败消耗的 token 也计入总成本。
- 每条 `llm` 事件带 `context.summary`；另有 `summary` / `summary_error` 事件，记录版本、覆盖条数、尝试次数、`finish_reason` 与成本。
- `retrieval` 用 BM25（英文词 + 中文二元组）按轮打分，`context.retrieval` 记录
  `budget_tokens` / `tools_tokens` / `message_budget` / 选中索引与分数 / `truncated_turns` /
  `request_tokens_est` / `over_budget`；`context.dropped_detail` 记录被整轮丢弃的索引、
  工具名、字符数与 120 字预览。
- 四种策略的参数集中在 `src/agent/context/config.py`，不在 CLI 或循环里散落默认值。
- A1 默认策略由 `DEFAULT_CONTEXT_STRATEGY` 统一控制，当前为 `full`；第 2 周 50×4 矩阵的结论见
  `docs/experiments/2026-11-10-week-02-rework.md`。

四种策略都只裁剪"发给模型的消息"：`messages`、`.state.json` 断点与恢复逻辑仍保存全量；
`--resume` 未显式指定策略/版本/阈值时沿用断点里的设置，避免恢复时悄悄改变实验条件。
每条 `llm` trace 事件带 `context` 字段 `{strategy,total,used,dropped,window_turns,summary,retrieval}`，
`scripts/replay.py` 会打印每步保留条数与摘要/失败信息。

固定评测集 `cases/context_cases.jsonl` 共 50 条（`single_qa` 13 / `multi_tool` 13 /
`long_read` 12 / `refusal` 12），说明见 `cases/README.md`。`scripts/check_cases.py`
校验条数、字段、类别均衡与 ID 唯一；`scripts/run_cases.py` 用同一批任务跑任一策略，
结果逐条追加到 `out/context_results.jsonl`，含 token、成本、步数与 trace 路径。

## 第 2 周结论（2026-11-10 返工后）

50 条任务 × 4 策略的 200 次矩阵；口径与改动见
[2026-11-10-week-02-rework.md](docs/experiments/2026-11-10-week-02-rework.md)。

| 策略 | 完成率 | 拒绝正确率 | 主请求输入 token | 附加调用输入 token | 平均成本（元） | 平均步数 | 单次请求 P95（s） |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `full` | 92.0% | 9/12 | 13,188 | 0 | 0.006808 | 4.08 | 15.82 |
| `window` | 64.0% | 4/12 | 14,126 | 0 | 0.009948 | 5.96 | 11.84 |
| `summary` | 74.0% | 6/12 | 13,161 | 8,118 | 0.016732 | 5.42 | 14.39 |
| `retrieval` | 60.0% | 8/12 | 17,118 | 0 | 0.010548 | 7.14 | 12.52 |

“完成率”只表示正常停止且有非空答案，不判断答对；拒绝类用例单列“拒绝正确率”。
`full` 的 `refusal-04` 直接贴出 `.env` 键与密钥前缀，已按规则判失败；旧表把它算成了成功。
分层看，`summary` 有 3/10 条降级样本、`retrieval` 有 28/50 条真正裁剪样本（完成 8 条），
降级与裁剪不再混进策略成绩。

本任务集的上下文多数不到 10k 字符，裁剪只丢掉后续仍需要的证据。A1 默认策略保持 `full`，
并由 `DEFAULT_CONTEXT_STRATEGY` 集中配置；四种策略保留为按需入口。结论、badcase 与复盘见
[2026-11-19-day13.md](docs/experiments/2026-11-19-day13.md)、
[2026-11-20-day14.md](docs/experiments/2026-11-20-day14.md)（后者为改造前旧口径）。

## 当前阶段

阶段一第 2 周完成：A2 context-lab 的 `full` / `window` / `summary` / `retrieval` 四策略、
50 条固定评测集、200 次矩阵与 badcase 台账均已落盘；A1 默认保持 `full`。
下一周进入第 3 周服务端工程化（网关与流式）。

## 已知问题

- 工具只读，写文件类工具尚未实现
- 新任务默认不跨任务记忆；显式 `--resume` 可从本地断点恢复同一任务
- trace 未做脱敏与轮转，只适合本地调试
- 真实 429 退避由本地 HTTP 桩验证（两次等待合计约 3 秒）；真实压测此前 8 并发 × 20 请求未触发 429（退出码 3），没有配置 `ARK_MODEL_MINI` 时不对默认模型重复加压
- trace 重放可核对历史用量与成本；旧 trace 没有工具参数，新 trace 才记录参数
- run_shell 输出上限 4000 字符，超出会标注"已截断"，长输出场景需自己缩小范围
- 运行产物放 `out/`，trace 放 `traces/`，两者都已忽略且不被 search_files 扫描
- `window` 只裁剪请求，断点仍保存全量历史，长任务的内存与 `.state.json` 体积不会下降
- `window` 按轮裁剪；`retrieval` 先按轮打分、再允许在轮内截断正文；预算按估算 token
  （不是真实 tokenizer），工具定义按固定 820 token 扣除；摘要阈值仍是字符估算
- `retrieval` 的分词只有英文词 + 中文二元组，没有停用词、同义词与 rerank；
  片段级裁剪只切正文，工具参数本身超预算时仍整轮丢弃
- `summary` 按请求重算，不是滚动摘要，长任务会多次调用摘要
- 摘要可能被 `max_tokens` 截断，trace 的 `finish_reason=length` 可筛出；`v0` 旧版提示在长上下文中更易返回空正文（实测触发重试与降级），回滚前需评估

## 目录

```
src/agent/llm/       模型适配：base 协议、doubao、fake
src/agent/tools/     注册表与四个工具
src/agent/loop/      循环与状态机
src/agent/context/   上下文装配：full 全量、window 滑窗、summary 摘要、retrieval BM25
src/agent/trace/     JSONL 落盘与成本计量
src/agent/cli/       命令行入口
cases/               50 条固定评测集与格式说明
tests/               单元测试
docs/adr/            关键决策记录
docs/experiments/    实验记录，与代码同次提交
scripts/             一次性脚本与跑批入口
gateway/             Go 服务侧：独立 module 的 A3 Agent Gateway
deploy/              网关容器构建与本地编排
```

Python 是 Agent 侧，Go 是服务侧；Go module 位于 `gateway/`，在该目录内运行
`go test ./...`、`go vet ./...` 或 `go run ./cmd/gateway`。
网关默认使用 `.env` 中的豆包配置；本地离线运行可在启动前设置
`$env:GATEWAY_PROVIDER = "mock"`，命令仍为 `go run ./cmd/gateway`。
网关仅对明确返回的 429 按 1/2/4 秒退避；超时不自动重放。`Idempotency-Key` 的响应缓存是进程内、
有界的 24 小时缓存，重启后清空；不要把它当跨实例分布式去重。
网关会在响应 `X-Trace-ID` 中返回请求标识，并以 `GATEWAY_MAX_CONCURRENCY` 为并发上限；超过上限时返回 503。
每次有效聊天请求会在 `GATEWAY_TRACE_DIR`（默认 `gateway/traces/`）写一份与 Python `scripts/replay.py` 对齐的 JSONL trace；
成本暂记 0 并标注“待计费”，`/metrics` 暴露请求数、错误数、延迟和首字节指标。

本地 mock 压测：先在一个终端设置 `GATEWAY_PROVIDER=mock` 并运行 `go run ./cmd/gateway`，
再从 `gateway/` 目录运行 `go run ./cmd/loadtest --n 200 --c 200`。Docker Compose 部署入口为
`docker compose -f deploy/docker-compose.yml up --build`；Compose 默认使用 mock，不会调用真实模型。

## 本机 Go 环境（CGO 与 race）

`go test -race` 在 Windows 上要求 CGO 启用并由 gcc 链接，本机用 MSYS2 的 UCRT64 GCC：

- 编译器：`C:\msys64\ucrt64\bin\gcc.exe`（`mingw-w64-ucrt-x86_64-gcc 16.2.0`，MSYS2 安装包 20260927）
- 持久配置：`go env -w CGO_ENABLED=1 CC=C:/msys64/ucrt64/bin/gcc.exe`，落在 `%APPDATA%\go\env`
- `C:\msys64\ucrt64\bin` 已追加到用户级 PATH。gcc.exe 自身要加载同目录的 DLL，缺这一步链接会报
  `running C:/msys64/ucrt64/bin/gcc.exe failed`；race 二进制运行同样依赖该目录，不能只在 `CC` 里写绝对路径
- PATH 变更只对新开的终端生效；当前会话临时用可先 `$env:PATH += ";C:\msys64\ucrt64\bin"`
- 2026-11-11 实测：`gateway/` 内 `go test -race ./...` 14 个包全部通过（无数据竞争告警），
  `go test ./...` 与 `go vet ./...` 同时通过；race 检测器有效性用一段故意竞争的样例程序验证过
- pacman 源已切到中科大（官方 `mirror.msys2.org` 返回 403、`repo.msys2.org` 超时），改动在
  `C:\msys64\etc\pacman.d\mirrorlist.msys` 与 `mirrorlist.mingw`，装包用
  `pacman -S mingw-w64-ucrt-x86_64-gcc`

## 远程仓库

origin 指向 https://github.com/carder-coder/ai-agent.git，主分支 main。

本机直连 github.com 的 443 会被重置，仓库级配置已写入 http.proxy=http://127.0.0.1:7897
（只对当前仓库生效，不动全局配置）。代理未启动时推送会报连接失败，先起代理再推。

凭据由 Git Credential Manager 保管，首次推送会弹 GitHub 登录，登录一次后不再询问；
不要用账号密码，GitHub 已不支持。换机器时重复一次登录即可。

推完自检三件事：git status -sb 显示 main...origin/main 且无 ahead；git ls-remote origin
的 HEAD 与本地 git log -1 的哈希一致；远端文件清单里没有 .env。
