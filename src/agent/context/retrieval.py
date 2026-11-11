"""BM25 检索裁剪：按任务相关性给每轮打分，按 token 预算选段，必要时截断片段。

预算按 token 估算；锚点固定，最近轮最多占 60%，至少留 40% 给按分数选出的旧轮次。
候选轮次整轮放不下时，允许把工具输出截到剩余预算，而不是直接丢掉整轮证据。
"""

from __future__ import annotations

import math
import re
from collections import Counter

from ..llm.types import Message
from ._layout import (
    anchor_end,
    describe_turn,
    drop_orphan_tools,
    estimate_context_tokens,
    split_turns,
    truncate_turn,
)
from .config import (
    BM25_B,
    BM25_K1,
    RETRIEVAL_BUDGET_TOKENS,
    RETRIEVAL_KEEP_RECENT,
    RETRIEVAL_MAX_DROPPED_DETAIL,
    RETRIEVAL_MIN_SEGMENT_TOKENS,
    RETRIEVAL_RECENT_RATIO,
)
from .strategies import ContextPlan

_ASCII_TOKEN = re.compile(r"[a-z0-9_]+")
_CJK_RUN = re.compile(r"[\u4e00-\u9fff]+")


def tokenize(text: str) -> list[str]:
    """英文/数字按词，中文按二元组切分；不引入外部分词依赖。"""
    lowered = text.lower()
    tokens = _ASCII_TOKEN.findall(lowered)
    for run in _CJK_RUN.findall(lowered):
        if len(run) == 1:
            tokens.append(run)
        else:
            tokens.extend(run[index : index + 2] for index in range(len(run) - 1))
    return tokens


def _turn_text(turn: list[Message]) -> str:
    parts: list[str] = []
    for message in turn:
        if message.content:
            parts.append(message.content)
        for call in message.tool_calls:
            parts.append(f"{call.name} {call.arguments}")
    return " ".join(parts)


def bm25_scores(
    documents: list[str],
    query: str,
    *,
    k1: float = BM25_K1,
    b: float = BM25_B,
) -> list[float]:
    """标准 BM25；文档数很少时也能稳定工作，空查询返回全零。"""
    if not documents:
        return []
    tokenized = [tokenize(document) for document in documents]
    lengths = [len(tokens) for tokens in tokenized]
    average = sum(lengths) / len(lengths) if lengths else 0.0
    doc_freq: Counter[str] = Counter()
    for tokens in tokenized:
        for term in set(tokens):
            doc_freq[term] += 1

    query_terms = list(dict.fromkeys(tokenize(query)))
    total_docs = len(documents)
    scores: list[float] = []
    for tokens, length in zip(tokenized, lengths, strict=True):
        term_freq = Counter(tokens)
        score = 0.0
        for term in query_terms:
            frequency = term_freq.get(term, 0)
            if frequency == 0:
                continue
            idf = math.log(1 + (total_docs - doc_freq[term] + 0.5) / (doc_freq[term] + 0.5))
            denominator = frequency + k1 * (1 - b + b * length / (average or 1.0))
            score += idf * frequency * (k1 + 1) / denominator
        scores.append(score)
    return scores


def _query_text(messages: list[Message], anchor: int) -> str:
    return "\n".join(
        message.content or "" for message in messages[:anchor] if message.role == "user"
    )


def _allocate(sizes: list[int], total: int, floor: int) -> list[int]:
    """在总量 total 内按大小比例分配，每项不低于 floor。"""
    if not sizes:
        return []
    base = [min(size, floor) for size in sizes]
    if sum(base) >= total:
        return base
    extra_total = total - sum(base)
    remaining = [max(size - floor, 0) for size in sizes]
    weight = sum(remaining)
    if weight <= 0:
        return base
    allocation = list(base)
    for index, weight_item in enumerate(remaining):
        allocation[index] += int(extra_total * weight_item / weight)
    return allocation


def _count_truncated(original: list[Message], fitted: list[Message]) -> int:
    return sum(
        1
        for before, after in zip(original, fitted, strict=True)
        if before.content and after.content and before.content != after.content
    )


def _fit_recent(
    recent: list[list[Message]],
    cap_tokens: int,
    min_segment_tokens: int,
) -> tuple[list[list[Message]], int, int, int]:
    """把最近若干轮压到 cap 内；返回（拟合后的轮次，截断轮数，截断消息数，无法压缩轮数）。"""
    sizes = [estimate_context_tokens(turn) for turn in recent]
    if sum(sizes) <= cap_tokens:
        return recent, 0, 0, 0
    allocation = _allocate(sizes, cap_tokens, min_segment_tokens)
    fitted: list[list[Message]] = []
    truncated_turns = 0
    truncated_messages = 0
    untruncatable = 0
    for turn, size, limit in zip(recent, sizes, allocation, strict=True):
        if size <= limit:
            fitted.append(turn)
            continue
        shrunk = truncate_turn(turn, limit)
        if shrunk is None:
            fitted.append(turn)
            untruncatable += 1
            continue
        truncated_turns += 1
        truncated_messages += _count_truncated(turn, shrunk)
        fitted.append(shrunk)
    return fitted, truncated_turns, truncated_messages, untruncatable


def plan_retrieval_context(
    messages: list[Message],
    *,
    budget_tokens: int = RETRIEVAL_BUDGET_TOKENS,
    tools_tokens: int = 0,
    keep_recent: int = RETRIEVAL_KEEP_RECENT,
    min_segment_tokens: int = RETRIEVAL_MIN_SEGMENT_TOKENS,
    recent_ratio: float = RETRIEVAL_RECENT_RATIO,
    k1: float = BM25_K1,
    b: float = BM25_B,
) -> ContextPlan:
    """锚点固定；最近轮按配额保留并允许截断；旧轮按 BM25 分数选到 token 预算内。"""
    if budget_tokens < 1:
        raise ValueError(f"budget_tokens 必须大于 0，收到 {budget_tokens}")
    if tools_tokens < 0:
        raise ValueError(f"tools_tokens 不能为负，收到 {tools_tokens}")
    message_budget = max(1, budget_tokens - tools_tokens)
    if keep_recent < 1:
        raise ValueError(f"keep_recent 必须大于 0，收到 {keep_recent}")
    if min_segment_tokens < 1:
        raise ValueError(f"min_segment_tokens 必须大于 0，收到 {min_segment_tokens}")
    if not 0 < recent_ratio < 1:
        raise ValueError(f"recent_ratio 必须在 (0,1) 内，收到 {recent_ratio}")

    total = len(messages)
    anchor = anchor_end(messages)
    turns = split_turns(messages, anchor)
    before_tokens = estimate_context_tokens(messages)
    retrieval_trace: dict = {
        "budget_tokens": budget_tokens,
        "tools_tokens": tools_tokens,
        "message_budget": message_budget,
        "keep_recent": keep_recent,
        "min_segment_tokens": min_segment_tokens,
        "recent_ratio": recent_ratio,
        "tokens_before": before_tokens,
    }
    if len(turns) <= 1:
        # 没有可打分的旧轮时仍要守住预算：最近一轮自己太大就截断它的正文，
        # 否则首步/第二步的大工具输出会直接冲爆预算（旧实现只标记 over_budget）。
        anchor_tokens = estimate_context_tokens(messages[:anchor])
        recent = turns[-1:] if turns else []
        truncated_turns = truncated_messages = untruncatable = 0
        tokens_after = before_tokens
        kept = list(messages)
        if recent and before_tokens > message_budget:
            cap = max(1, message_budget - anchor_tokens)
            fitted, truncated_turns, truncated_messages, untruncatable = _fit_recent(
                recent, cap, min_segment_tokens
            )
            kept = list(messages[:anchor])
            for turn in fitted:
                kept.extend(turn)
            kept = drop_orphan_tools(kept)
            tokens_after = estimate_context_tokens(kept)
        retrieval_trace.update(
            {
                "reason": "no_old_turns",
                "tokens_after": tokens_after,
                "request_tokens_est": tokens_after + tools_tokens,
                "used_tokens": tokens_after,
                "truncated_turns": truncated_turns,
                "truncated_messages": truncated_messages,
                "untruncatable_turns": untruncatable,
                "over_budget": tokens_after + tools_tokens > budget_tokens,
            }
        )
        return ContextPlan(
            strategy="retrieval",
            messages=kept,
            total_messages=total,
            used_messages=len(kept),
            dropped_messages=total - len(kept),
            retrieval=retrieval_trace,
        )

    keep = max(1, min(keep_recent, len(turns) - 1))
    recent = turns[-keep:]
    candidates = list(enumerate(turns[:-keep]))
    query = _query_text(messages, anchor)
    scores = bm25_scores([_turn_text(turn) for _, turn in candidates], query, k1=k1, b=b)
    ranked = sorted(
        zip(candidates, scores, strict=True),
        key=lambda item: (item[1], item[0][0]),
        reverse=True,
    )
    score_by_index = {index: score for (index, _), score in ranked}

    anchor_tokens = estimate_context_tokens(messages[:anchor])
    # 至少留 40% 消息预算给按分数选出的旧轮，避免最近轮把预算吃光后退化成滑窗。
    reserve_for_old = int(message_budget * (1 - recent_ratio)) if candidates else 0
    recent_cap = max(
        RETRIEVAL_MIN_SEGMENT_TOKENS * len(recent),
        message_budget - anchor_tokens - reserve_for_old,
    )
    recent, recent_trunc_turns, recent_trunc_messages, untruncatable = _fit_recent(
        recent, recent_cap, min_segment_tokens
    )
    used = anchor_tokens + sum(estimate_context_tokens(turn) for turn in recent)

    selected: list[tuple[int, list[Message]]] = []
    truncated_turns = recent_trunc_turns
    truncated_messages = recent_trunc_messages
    for (index, turn), _score in ranked:
        remaining = message_budget - used
        cost = estimate_context_tokens(turn)
        if cost <= remaining:
            selected.append((index, turn))
            used += cost
        elif remaining >= min_segment_tokens:
            shrunk = truncate_turn(turn, remaining)
            if shrunk is None:
                continue
            selected.append((index, shrunk))
            truncated_turns += 1
            truncated_messages += _count_truncated(turn, shrunk)
            used += estimate_context_tokens(shrunk)
    selected.sort(key=lambda item: item[0])
    selected_indexes = {index for index, _ in selected}

    kept = list(messages[:anchor])
    for _, turn in selected:
        kept.extend(turn)
    for turn in recent:
        kept.extend(turn)
    kept = drop_orphan_tools(kept)

    dropped = [
        (index, turn)
        for index, turn in enumerate(turns)
        if index not in selected_indexes and index < len(turns) - keep
    ]
    detail = []
    for index, turn in dropped[:RETRIEVAL_MAX_DROPPED_DETAIL]:
        item = describe_turn(index, turn)
        item["score"] = round(score_by_index.get(index, 0.0), 3)
        detail.append(item)

    after_tokens = estimate_context_tokens(kept)
    retrieval_trace.update(
        {
            "enabled": True,
            "query_terms": len(dict.fromkeys(tokenize(query))),
            "scored_turns": len(candidates),
            "selected": len(selected),
            "dropped": len(dropped),
            "used_tokens": after_tokens,
            "tokens_after": after_tokens,
            "request_tokens_est": after_tokens + tools_tokens,
            "selected_indexes": [index for index, _ in selected],
            "scores": {str(index): round(score, 3) for index, score in score_by_index.items()},
            "truncated_turns": truncated_turns,
            "truncated_messages": truncated_messages,
            "untruncatable_turns": untruncatable,
            "over_budget": after_tokens + tools_tokens > budget_tokens,
            "reason": "anchor_over_budget" if anchor_tokens > message_budget else None,
        }
    )
    return ContextPlan(
        strategy="retrieval",
        messages=kept,
        total_messages=total,
        used_messages=len(kept),
        dropped_messages=total - len(kept),
        retrieval=retrieval_trace,
        dropped_detail=detail,
    )
