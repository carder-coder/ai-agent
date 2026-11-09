# 第 1 周（2026-11-07 至 11-13）：Agent 最小内核

**学习内容**：消息结构与角色分工；function calling 的 schema 定义与参数回填；流式增量与 tool_calls 分片拼接；token 计数与计费口径；结构化输出；主流厂商 API 差异。

**本周交付**：A1 raw-agent，五个模块（llm_client、tool_registry、agent_loop、trace、cli），不超过 800 行，只接豆包，接口按适配器留位。

**本周预算**：名义 12 小时。实际 Day 1 用了约 12 小时，Day 2 计划 7 小时，前两天已超预算，因此 Day 3 至 Day 5 压到每天 1 至 2 小时，只做收口。

## Day 1（11-07，已完成）

建仓、搭依赖、写模型层与工具层、跑通端到端。结果见 [2026-11-07-day1.md](../experiments/2026-11-07-day1.md)：8 步 finished、30745 token（缓存 20096）、0.020272 元、单测 15 条全绿。

## Day 2（11-08，任务单）

七段任务写在 [2026-11-08-day2.md](../experiments/2026-11-08-day2.md)：统计任务收敛、边界与退避单测、trace 重放器、真实限流压测、提示 A/B、收尾、预习。**执行状态**：`scripts/replay.py`、`scripts/press_429.py` 尚未创建，单测仍为 15 条，压测与 A/B 未跑。

## Day 3（11-09，2 小时）：断点恢复

- 在 `src/agent/cli/main.py` 加 `--resume <trace 路径>`：读该 trace 的 llm 事件，把消息快照还原成初始 history，从最后一步之后继续跑。
- 在 `src/agent/trace/recorder.py` 确认每条 llm 事件都存了完整消息快照；缺就补，字段名与 `Message` 对齐。
- 完成标志：跑到第 4 步时手动 Ctrl+C，再用 `--resume` 续跑，能接上且不重复已完成的工具调用；trace 里出现两段 run，replay 能按时间串起来。

## Day 4（11-10，2 小时）：三种故障可读

- 断网：把 `ARK_BASE_URL` 指向不可达地址，确认抛 `APITimeoutError` 并写进 trace 的 `provider_error` 事件。
- 上游超时：把超时降到 1 秒，确认 429 之外的状态码也走同一条错误路径。
- 工具报错：让 `read_file` 读一个不存在的路径，确认 observation 里回填错误文本而不是抛堆栈。
- 补 `tests/test_loop.py`：三条故障各一条断言；需要覆盖率时先 `python -m uv add --dev pytest-cov`。
- 完成标志：三种故障各有一段可读的失败原因；循环分支覆盖率不低于 70%（`pytest --cov=agent.loop`）。

## Day 5（11-11，1 小时）：换机复现

- 按 README 走一遍三步：装依赖、填 `.env`、跑 `--provider fake` 与一次真实任务。
- 把 README 里缺的前置条件（Git 不在 PATH、代理、Python 版本）补上。
- 完成标志：在一个新目录里 clone 后十分钟内跑通一个真实任务，中途不需要翻代码找参数。

## Day 6（11-12，2 小时）：补第二天欠账

- 优先做第二天任务三里最有价值的部分：`scripts/replay.py`，因为后面每次调优都要用它核对成本。
- 若时间够，再补 `scripts/press_429.py`。
- 跑一次统计任务，把步数与成本记进本周实验记录。
- 完成标志：`git status` 干净；本周至少有两天各留下一条提交。

## Day 7（11-13，1 小时 + 1 小时缓冲）：复盘与预习

- 回填进度清单：周清单第 1 周三项（工具与单测、trace 可复盘、端到端跑通）按实际勾选；每日记录补 11-09 至 11-13 三行。
- 预习第 2 周材料：Lost in the Middle（长上下文位置效应）、提示缓存的命中条件。
- 完成标志：清单没有空项；写下一句"第 2 周第一件事"。

## 本周门槛自检

- 不看框架能跑通循环：已达成。
- 断点恢复与三种故障可读：Day 3、Day 4 完成后达成。
- 换机 10 分钟跑通：Day 5 完成后达成。
- 未达成项处置：写进清单的阶段门槛页，注明是第 2 周补做还是延后。
