"""上下文策略的集中参数：第 2 周所有裁剪参数只改这一个文件。"""

from __future__ import annotations

STRATEGIES: tuple[str, ...] = ("full", "window", "summary")

DEFAULT_WINDOW_TURNS = 4

# 摘要策略：超过字符阈值时压缩更早的轮次，保留最近若干轮。
DEFAULT_SUMMARY_VERSION = "v1"
SUMMARY_TRIGGER_CHARS = 12_000
SUMMARY_KEEP_TURNS = 2
SUMMARY_MAX_TOKENS = 800
SUMMARY_MAX_TOKENS_CAP = 2_000
SUMMARY_MAX_ATTEMPTS = 2
SUMMARY_MAX_INPUT_CHARS = 24_000
