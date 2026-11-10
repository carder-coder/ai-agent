"""消息布局工具：固定锚点、按 assistant 切轮、清理孤立工具消息。"""

from __future__ import annotations

from ..llm.types import Message


def system_prefix_end(messages: list[Message]) -> int:
    """开头连续 system 消息的结束位置。"""
    end = 0
    while end < len(messages) and messages[end].role == "system":
        end += 1
    return end


def anchor_end(messages: list[Message]) -> int:
    """固定前缀：开头的 system 消息 + 紧随其后的第一条 user 任务。"""
    end = system_prefix_end(messages)
    if end < len(messages) and messages[end].role == "user":
        end += 1
    return end


def split_turns(messages: list[Message], start: int) -> list[list[Message]]:
    """把前缀之后的消息切成轮次，每轮从 assistant 响应开始并含其工具结果。

    无法归属到某一轮的孤立消息（例如没有 assistant 父消息的 tool 结果）直接丢弃，
    避免拼出模型侧无法接受的 tool 消息。
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


def drop_orphan_tools(messages: list[Message]) -> list[Message]:
    """兜底：tool 消息的 tool_call_id 必须能在同批保留的 assistant 调用里找到。"""
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


def estimate_context_chars(messages: list[Message]) -> int:
    """估算请求上下文大小：正文加工具名/参数，作为触发裁剪的廉价代理。"""
    total = 0
    for message in messages:
        total += len(message.content or "")
        for call in message.tool_calls:
            total += len(call.name) + len(call.arguments)
    return total


def describe_turn(index: int, turn: list[Message]) -> dict:
    """为 trace 生成被裁轮次的紧凑描述，用于 badcase 定位。"""
    tools = [
        call.name
        for message in turn
        if message.role == "assistant"
        for call in message.tool_calls
    ]
    text = " ".join(message.content or "" for message in turn).replace("\n", " ")
    return {
        "index": index,
        "tool": "、".join(tools) if tools else turn[0].role,
        "chars": estimate_context_chars(turn),
        "preview": text[:120],
    }
