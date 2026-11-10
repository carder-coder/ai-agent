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
uv run python -m agent.cli --task "读 README 并总结" --context-strategy window --context-window 4
uv run python -m agent.cli --resume traces\<run_id>.jsonl --max-steps 12 # 从中断处继续
uv run pytest -q                                                # 单测
uv run pytest --cov=agent --cov-report=term-missing -q           # 单测与覆盖率
uv run ruff check .                                             # 静态检查
uv run python scripts/replay.py --trace traces\<run_id>.jsonl  # 核对单次 trace
uv run python scripts/check_cases.py                            # 校验 50 条评测集
uv run python scripts/run_cases.py --strategy full --dry-run    # 只看选中哪些用例
uv run python scripts/run_cases.py --strategy window --sample-per-category 1
uv run python scripts/press_429.py --model <mini-model-id> --concurrency 8 --rounds 20
```

trace 落在 `traces/<run_id>.jsonl`，每行一个事件（llm / tool / finish / run_end），
`run_end` 带本次总 token 与成本估算。每次模型响应和工具执行后会把消息、步数与累计用量
保存到同名 `.state.json`；Ctrl+C 后可传 trace 或 state 路径恢复，已完成的工具调用不会重放。
`--prompt-variant concise` 可切换精简系统提示，与默认 `detailed` 做同任务对照。
`press_429.py` 支持 `--model`、`--concurrency`、`--rounds`、`--endpoint`，默认 8 并发、每路 20 次；
它会真实发送最多 160 次模型请求并产生 API 费用。验证限流时优先指定低配额模型，仅在需要时运行。

## 上下文策略（第 2 周）

`--context-strategy` 一个参数切换两种装配方式，系统提示、工具 schema 与提示模板都不变：

| 策略 | 行为 | 适用 |
| --- | --- | --- |
| `full`（默认） | 每次请求携带全部 `messages` | 历史短，需要完整证据链 |
| `window` | 保留 system + 首条 user 作为锚点，再保留最近 `--context-window` 轮（默认 4）；更早的轮次整轮丢弃，工具调用与其结果不拆开 | 步数多，需要控制上下文预算 |

滑窗只裁剪"发给模型的消息"：`messages`、`.state.json` 断点与恢复逻辑仍保存全量；
`--resume` 未显式指定策略时沿用断点里的策略，避免恢复时悄悄改变实验条件。
每条 `llm` trace 事件带 `context` 字段 `{strategy,total,used,dropped,window_turns}`，
`scripts/replay.py` 会打印每步保留条数。

固定评测集 `cases/context_cases.jsonl` 共 50 条（`single_qa` 13 / `multi_tool` 13 /
`long_read` 12 / `refusal` 12），说明见 `cases/README.md`。`scripts/check_cases.py`
校验条数、字段、类别均衡与 ID 唯一；`scripts/run_cases.py` 用同一批任务跑任一策略，
结果逐条追加到 `out/context_results.jsonl`，含 token、成本、步数与 trace 路径。

## 当前阶段

阶段一第 2 周：A1 最小闭环完成；A2 context-lab 的 `full` / `window` 两种上下文策略已接入，
50 条固定评测集已冻结，后续补摘要与检索策略。

## 已知问题

- 工具只读，写文件类工具尚未实现
- 新任务默认不跨任务记忆；显式 `--resume` 可从本地断点恢复同一任务
- trace 未做脱敏与轮转，只适合本地调试
- 真实 429 退避由本地 HTTP 桩验证（两次等待合计约 3 秒）；真实压测此前 8 并发 × 20 请求未触发 429（退出码 3），没有配置 `ARK_MODEL_MINI` 时不对默认模型重复加压
- trace 重放可核对历史用量与成本；旧 trace 没有工具参数，新 trace 才记录参数
- run_shell 输出上限 4000 字符，超出会标注"已截断"，长输出场景需自己缩小范围
- 运行产物放 `out/`，trace 放 `traces/`，两者都已忽略且不被 search_files 扫描
- `window` 只裁剪请求，断点仍保存全量历史，长任务的内存与 `.state.json` 体积不会下降
- 滑窗按"轮"裁剪，不按 token 预算裁剪；预算控制留到摘要与检索策略

## 目录

```
src/agent/llm/       模型适配：base 协议、doubao、fake
src/agent/tools/     注册表与四个工具
src/agent/loop/      循环与状态机
src/agent/context/   上下文装配：full 全量、window 滑窗
src/agent/trace/     JSONL 落盘与成本计量
src/agent/cli/       命令行入口
cases/               50 条固定评测集与格式说明
tests/               单元测试
docs/adr/            关键决策记录
docs/experiments/    实验记录，与代码同次提交
scripts/             一次性脚本与跑批入口
```

## 远程仓库

origin 指向 https://github.com/carder-coder/ai-agent.git，主分支 main。

本机直连 github.com 的 443 会被重置，仓库级配置已写入 http.proxy=http://127.0.0.1:7897
（只对当前仓库生效，不动全局配置）。代理未启动时推送会报连接失败，先起代理再推。

凭据由 Git Credential Manager 保管，首次推送会弹 GitHub 登录，登录一次后不再询问；
不要用账号密码，GitHub 已不支持。换机器时重复一次登录即可。

推完自检三件事：git status -sb 显示 main...origin/main 且无 ahead；git ls-remote origin
的 HEAD 与本地 git log -1 的哈希一致；远端文件清单里没有 .env。
