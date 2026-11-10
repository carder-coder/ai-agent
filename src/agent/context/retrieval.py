"""BM25 检索裁剪：按任务相关性给每轮打分，按分数选段直到字符预算。"""

from __future__ import annotations

import math
import re
from collections import Counter

from ..llm.types import Message
from ._layout import (
    anchor_end,
    describe_turn,
    drop_orphan_tools,
    estimate_context_chars,
    split_turns,
)
from .config import (
    BM25_B,
    BM25_K1,
    RETRIEVAL_BUDGET_CHARS,
    RETRIEVAL_KEEP_RECENT,
    RETRIEVAL_MAX_DROPPED_DETAIL,
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


def plan_retrieval_context(
    messages: list[Message],
    *,
    budget_chars: int = RETRIEVAL_BUDGET_CHARS,
    keep_recent: int = RETRIEVAL_KEEP_RECENT,
    k1: float = BM25_K1,
    b: float = BM25_B,
) -> ContextPlan:
    """锚点与最近轮固定保留，其余轮次按 BM25 分数选到字符预算内。"""
    if budget_chars < 1:
        raise ValueError(f"budget_chars 必须大于 0，收到 {budget_chars}")
    if keep_recent < 1:
        raise ValueError(f"keep_recent 必须大于 0，收到 {keep_recent}")

    total = len(messages)
    anchor = anchor_end(messages)
    turns = split_turns(messages, anchor)
    retrieval_trace: dict = {
        "budget": budget_chars,
        "keep_recent": keep_recent,
        "chars": estimate_context_chars(messages),
    }
    if len(turns) <= 1:
        retrieval_trace["reason"] = "no_old_turns"
        return ContextPlan(
            strategy="retrieval",
            messages=list(messages),
            total_messages=total,
            used_messages=total,
            dropped_messages=0,
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

    used = estimate_context_chars(messages[:anchor])
    used += sum(estimate_context_chars(turn) for turn in recent)
    selected: list[tuple[int, list[Message]]] = []
    for (index, turn), _score in ranked:
        cost = estimate_context_chars(turn)
        if used + cost <= budget_chars:
            selected.append((index, turn))
            used += cost
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

    retrieval_trace.update(
        {
            "enabled": True,
            "query_terms": len(dict.fromkeys(tokenize(query))),
            "scored_turns": len(candidates),
            "selected": len(selected),
            "dropped": len(dropped),
            "chars_after": estimate_context_chars(kept),
            "selected_indexes": [index for index, _ in selected],
            "scores": {str(index): round(score, 3) for index, score in score_by_index.items()},
            "over_budget": estimate_context_chars(kept) > budget_chars,
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
