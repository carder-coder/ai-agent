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


TRUNCATION_MARKER = "…（上下文裁剪：已截断）"


def count_tokens(text: str | None) -> int:
    """廉价 token 估算：CJK 按 1 字 1 token，ASCII 按 4 字符 1 token。

    不引入 tokenizer 依赖；用于预算裁剪时与真实用量的偏差在 ±20% 内，
    比字符数更适合和"平均输入 token"比较。
    """
    if not text:
        return 0
    total = 0
    ascii_run = 0
    for char in text:
        if char.isascii():
            ascii_run += 1
            if ascii_run == 4:
                total += 1
                ascii_run = 0
        else:
            if ascii_run:
                total += 1
                ascii_run = 0
            total += 1
    if ascii_run:
        total += 1
    return total


def estimate_context_tokens(messages: list[Message]) -> int:
    """请求上下文的 token 估算：正文 + 工具名/参数 + 每条消息的固定开销。"""
    total = 0
    for message in messages:
        total += count_tokens(message.content) + 4
        for call in message.tool_calls:
            total += count_tokens(call.name) + count_tokens(call.arguments)
    return total


def _prefix_index_for_tokens(text: str, budget: int) -> int:
    """返回按估算口径累计到 budget 时的字符下标。"""
    total = 0
    ascii_run = 0
    for index, char in enumerate(text):
        if char.isascii():
            ascii_run += 1
            if ascii_run == 4:
                total += 1
                ascii_run = 0
        else:
            if ascii_run:
                total += 1
                ascii_run = 0
            total += 1
        if total >= budget:
            # 不把跨过预算的当前字符算进去，避免截断结果比 limit 多 1 token。
            return index
    return len(text)


def truncate_text_to_tokens(text: str, limit: int) -> str:
    """把正文截到 token 上限并加标记；未超限时原样返回，避免无意义改写。"""
    if limit <= 0:
        return ""
    if count_tokens(text) <= limit:
        return text
    marker_tokens = count_tokens(TRUNCATION_MARKER)
    if limit <= marker_tokens:
        return "…"
    budget = limit - marker_tokens
    return text[: _prefix_index_for_tokens(text, budget)] + TRUNCATION_MARKER


def truncate_turn(turn: list[Message], limit_tokens: int) -> list[Message] | None:
    """把一轮消息压到 token 上限：只截正文，保留 assistant/tool 配对与工具参数。

    工具参数不能截（会破坏 JSON），若参数本身就超预算则返回 None 由调用方整轮丢弃。
    """
    if limit_tokens <= 0:
        return None
    fixed = 4 * len(turn)
    for message in turn:
        for call in message.tool_calls:
            fixed += count_tokens(call.name) + count_tokens(call.arguments)
    available = limit_tokens - fixed
    if available < 1:
        return None
    content_messages = [message for message in turn if message.content]
    total_content = sum(count_tokens(message.content) for message in content_messages)
    if not content_messages or total_content <= available:
        return [
            Message(
                role=message.role,
                content=message.content,
                tool_calls=message.tool_calls,
                tool_call_id=message.tool_call_id,
                name=message.name,
            )
            for message in turn
        ]

    result: list[Message] = []
    spent = 0
    for message in turn:
        if not message.content:
            result.append(
                Message(
                    role=message.role,
                    content=message.content,
                    tool_calls=message.tool_calls,
                    tool_call_id=message.tool_call_id,
                    name=message.name,
                )
            )
            continue
        share = max(1, int(available * count_tokens(message.content) / total_content))
        if message is content_messages[-1]:
            share = max(1, available - spent)
        new_content = truncate_text_to_tokens(message.content, share)
        spent += count_tokens(new_content)
        result.append(
            Message(
                role=message.role,
                content=new_content,
                tool_calls=message.tool_calls,
                tool_call_id=message.tool_call_id,
                name=message.name,
            )
        )
    return result


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
        "tokens": estimate_context_tokens(turn),
        "preview": text[:120],
    }

