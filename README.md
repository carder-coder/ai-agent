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
uv run pytest -q                                                # 单测
uv run ruff check .                                             # 静态检查
```

trace 落在 `traces/<run_id>.jsonl`，每行一个事件（llm / tool / finish / run_end），
`run_end` 带本次总 token 与成本估算。

## 当前阶段

阶段一第 1 周：A1 最小闭环骨架完成，四个只读工具可用，端到端任务已跑通。

## 已知问题

- 工具只读，写文件类工具留到第 2 周
- 单次任务不做多轮记忆，上下文每次重开
- trace 未做脱敏与轮转，只适合本地调试
- 真实 429 退避只在本地桩上验证过，未经真实限流压测
- 统计类任务当前 7 步收尾，高于表23 的 3 到 6 步目标
- run_shell 输出上限 4000 字符，超出会标注"已截断"，长输出场景需自己缩小范围
- 运行产物放 `out/`，trace 放 `traces/`，两者都已忽略且不被 search_files 扫描

## 目录

```
src/agent/llm/     模型适配：base 协议、doubao、fake
src/agent/tools/   注册表与四个工具
src/agent/loop/    循环与状态机
src/agent/trace/   JSONL 落盘与成本计量
src/agent/cli/     命令行入口
tests/             单元测试
docs/adr/          关键决策记录
docs/experiments/  实验记录，与代码同次提交
scripts/           一次性脚本
```
