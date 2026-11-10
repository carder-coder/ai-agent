"""两种上下文装配策略：full 全量拼接，window 只保留最近若干轮工具交互。

这里只决定"本次请求发给模型哪些消息"，不修改 run() 保存的完整对话。
因此断点恢复仍基于全量 messages，滑窗只影响提示内容。摘要策略见 summary.py。
"""

from __future__ import annotations

from dataclasses import dataclass

from ..llm.types import Message
from ._layout import (
    anchor_end,
    describe_turn,
    drop_orphan_tools,
    split_turns,
)
from .config import DEFAULT_WINDOW_TURNS, STRATEGIES


@dataclass
class ContextPlan:
    """一次模型请求实际使用的消息，以及可用于 trace 的裁剪统计。"""

    strategy: str
    messages: list[Message]
    total_messages: int
    used_messages: int
    dropped_messages: int
    window_turns: int | None = None
    summary: dict | None = None
    retrieval: dict | None = None
    dropped_detail: list[dict] | None = None

    def to_trace(self) -> dict:
        """序列化成 trace 字段；full 策略也会记录，便于同一批任务对照。"""
        payload = {
            "strategy": self.strategy,
            "total": self.total_messages,
            "used": self.used_messages,
            "dropped": self.dropped_messages,
            "window_turns": self.window_turns,
        }
        if self.summary is not None:
            payload["summary"] = self.summary
        if self.retrieval is not None:
            payload["retrieval"] = self.retrieval
        if self.dropped_detail:
            payload["dropped_detail"] = self.dropped_detail
        return payload


def plan_context(
    messages: list[Message],
    strategy: str = "full",
    window_turns: int = DEFAULT_WINDOW_TURNS,
) -> ContextPlan:
    """按策略生成本次请求的消息列表。

    - full：原样使用全部消息，保持第 1 周的行为。
    - window：保留 system + 首条 user 作为锚点，再保留最近 window_turns 轮
      （一轮 = 一条 assistant 响应及其 tool 结果），更早的轮次整轮丢弃。
    """
    total = len(messages)
    if strategy == "full":
        return ContextPlan(
            strategy="full",
            messages=list(messages),
            total_messages=total,
            used_messages=total,
            dropped_messages=0,
        )
    if strategy not in STRATEGIES:
        raise ValueError(f"未知上下文策略：{strategy}（可选：{'、'.join(STRATEGIES)}）")
    if window_turns < 1:
        raise ValueError(f"window_turns 必须大于 0，收到 {window_turns}")

    anchor = anchor_end(messages)
    turns = split_turns(messages, anchor)
    kept = list(messages[:anchor])
    for turn in turns[-window_turns:]:
        kept.extend(turn)
    kept = drop_orphan_tools(kept)

    dropped = turns[: max(0, len(turns) - window_turns)]
    return ContextPlan(
        strategy="window",
        messages=kept,
        total_messages=total,
        used_messages=len(kept),
        dropped_messages=total - len(kept),
        window_turns=window_turns,
        dropped_detail=[describe_turn(index, turn) for index, turn in enumerate(dropped[:5])],
    )
