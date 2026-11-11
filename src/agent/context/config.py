"""
上下文策略的集中参数：第 2 周所有裁剪参数只改这一个文件。

检索预算按 token 估算，不再用字符：字符数对中英文混排的折算不稳定，
和"平均输入 token"也不可比。默认 2,500 token 指整个请求（含工具定义），
略低于 full 的单请求均值（约 3,200），既能触发裁剪，又不会把最近轮挤成唯一上下文。
"""

from __future__ import annotations

STRATEGIES: tuple[str, ...] = ("full", "window", "summary", "retrieval")

DEFAULT_CONTEXT_STRATEGY = "full"

DEFAULT_WINDOW_TURNS = 2

# 摘要策略：超过字符阈值时压缩更早的轮次，保留最近若干轮。
# 本轮只把检索预算改成 token；摘要阈值保持字符口径，避免改变已有 summary 臂的实验条件。
DEFAULT_SUMMARY_VERSION = "v1"
SUMMARY_TRIGGER_CHARS = 3_000
SUMMARY_KEEP_TURNS = 2
SUMMARY_MAX_TOKENS = 800
SUMMARY_MAX_TOKENS_CAP = 2_000
SUMMARY_MAX_ATTEMPTS = 2
SUMMARY_MAX_INPUT_CHARS = 24_000

# 检索策略：BM25 给每轮打分，按 token 预算选段；预算不足时允许截断工具输出。
RETRIEVAL_BUDGET_TOKENS = 2_500
RETRIEVAL_KEEP_RECENT = 1
# 最近轮最多占预算的 60%，至少留 40% 给按分数选出的旧轮次。
RETRIEVAL_RECENT_RATIO = 0.6
# 片段级裁剪的下限：低于这个量保留下来也只是噪声。
RETRIEVAL_MIN_SEGMENT_TOKENS = 128
RETRIEVAL_MAX_DROPPED_DETAIL = 5
# 四个只读工具的函数定义在真实请求里约占 820 token；预算按请求总量算，
# 先扣掉它，否则"2500 token"实际会发出 3300+ token 的提示。
TOOL_SCHEMA_TOKENS = 820
BM25_K1 = 1.5
BM25_B = 0.75
