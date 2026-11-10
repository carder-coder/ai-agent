"""两种上下文装配策略：full 全量拼接，window 只保留最近若干轮工具交互。

这里只决定"本次请求发给模型哪些消息"，不修改 run() 保存的完整对话。
因此断点恢复仍基于全量 messages，滑窗只影响提示内容。
"""

from __future__ import annotations

from dataclasses import dataclass

from ..llm.types import Message

STRATEGIES: tuple[str, ...] = ("full", "window")
DEFAULT_WINDOW_TURNS = 4


@dataclass
class ContextPlan:
    """一次模型请求实际使用的消息，以及可用于 trace 的裁剪统计。"""

    strategy: str
    messages: list[Message]
    total_messages: int
    used_messages: int
    dropped_messages: int
    window_turns: int | None = None

    def to_trace(self) -> dict:
        """序列化成 trace 字段；full 策略也会记录，便于同一批任务对照。"""
        return {
            "strategy": self.strategy,
            "total": self.total_messages,
            "used": self.used_messages,
            "dropped": self.dropped_messages,
            "window_turns": self.window_turns,
        }


def _anchor_end(messages: list[Message]) -> int:
    """定位固定前缀：开头的 system 消息 + 紧随其后的第一条 user 任务。"""
    end = 0
    while end < len(messages) and messages[end].role == "system":
        end += 1
    if end < len(messages) and messages[end].role == "user":
        end += 1
    return end


def _split_turns(messages: list[Message], start: int) -> list[list[Message]]:
    """把前缀之后的消息切成轮次，每轮从 assistant 响应开始，含它的工具结果。

    无法归属到某一轮的孤立消息（例如没有 assistant 父消息的 tool 结果）
    直接丢弃，避免拼出模型侧无法接受的 tool 消息。
    """
    turns: list[list[Message]] = []
    current: list[Message] | None = None
    for message in messages[start:]:
        if message.role == "assistant":
            if current is not None:
                turns.append(current)
            current = [message]
        elif current is None:
            continue
        else:
            current.append(message)
    if current is not None:
        turns.append(current)
    return turns


def _drop_orphan_tools(messages: list[Message]) -> list[Message]:
    """兜底：tool 消息的 tool_call_id 必须能在本轮已保留的 assistant 调用里找到。"""
    known_ids = {
        call.id
        for message in messages
        if message.role == "assistant"
        for call in message.tool_calls
    }
    return [
        message
        for message in messages
        if message.role != "tool" or message.tool_call_id in known_ids
    ]


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

    anchor_end = _anchor_end(messages)
    turns = _split_turns(messages, anchor_end)
    kept = list(messages[:anchor_end])
    for turn in turns[-window_turns:]:
        kept.extend(turn)
    kept = _drop_orphan_tools(kept)

    return ContextPlan(
        strategy="window",
        messages=kept,
        total_messages=total,
        used_messages=len(kept),
        dropped_messages=total - len(kept),
        window_turns=window_turns,
    )
