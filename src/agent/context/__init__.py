"""上下文装配策略：全量拼接、滑窗与摘要压缩，共用同一提示模板。"""

from .config import (
    BM25_B,
    BM25_K1,
    DEFAULT_CONTEXT_STRATEGY,
    DEFAULT_SUMMARY_VERSION,
    DEFAULT_WINDOW_TURNS,
    RETRIEVAL_BUDGET_CHARS,
    RETRIEVAL_KEEP_RECENT,
    RETRIEVAL_MAX_DROPPED_DETAIL,
    STRATEGIES,
    SUMMARY_KEEP_TURNS,
    SUMMARY_MAX_ATTEMPTS,
    SUMMARY_MAX_INPUT_CHARS,
    SUMMARY_MAX_TOKENS,
    SUMMARY_MAX_TOKENS_CAP,
    SUMMARY_TRIGGER_CHARS,
)
from .retrieval import bm25_scores, plan_retrieval_context, tokenize
from .strategies import ContextPlan, plan_context
from .summary import (
    SUMMARY_PROMPTS,
    Summarizer,
    SummaryError,
    SummaryOutcome,
    estimate_context_chars,
    plan_summary_context,
)

__all__ = [
    "BM25_B",
    "BM25_K1",
    "DEFAULT_CONTEXT_STRATEGY",
    "DEFAULT_SUMMARY_VERSION",
    "DEFAULT_WINDOW_TURNS",
    "RETRIEVAL_BUDGET_CHARS",
    "RETRIEVAL_KEEP_RECENT",
    "RETRIEVAL_MAX_DROPPED_DETAIL",
    "STRATEGIES",
    "SUMMARY_KEEP_TURNS",
    "SUMMARY_MAX_ATTEMPTS",
    "SUMMARY_MAX_INPUT_CHARS",
    "SUMMARY_MAX_TOKENS",
    "SUMMARY_MAX_TOKENS_CAP",
    "SUMMARY_PROMPTS",
    "SUMMARY_TRIGGER_CHARS",
    "ContextPlan",
    "Summarizer",
    "SummaryError",
    "SummaryOutcome",
    "bm25_scores",
    "estimate_context_chars",
    "plan_context",
    "plan_retrieval_context",
    "plan_summary_context",
    "tokenize",
]
