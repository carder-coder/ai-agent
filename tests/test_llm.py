"""模型适配层：流式分片必须按 index 拼接，名字和参数都不能被后一片覆盖。"""

from __future__ import annotations

import json
from types import SimpleNamespace

from agent.llm import DoubaoClient, Message


def _delta(content=None, tool_calls=None, finish=None):
    return SimpleNamespace(
        choices=[SimpleNamespace(delta=SimpleNamespace(content=content, tool_calls=tool_calls),
                                 finish_reason=finish)]
    )


def _frag(index, name=None, arguments=None, call_id=None):
    return SimpleNamespace(
        index=index,
        id=call_id,
        function=SimpleNamespace(name=name, arguments=arguments),
    )


def test_tool_call_fragments_are_assembled(monkeypatch) -> None:
    client = DoubaoClient(model="stub", api_key="stub", base_url="http://127.0.0.1:9/v1")
    chunks = [
        _delta(tool_calls=[_frag(0, name="ad", arguments='{"a": 1', call_id="call_1")]),
        _delta(tool_calls=[_frag(0, name="d", arguments='7, "b": 25}')]),
        _delta(finish="tool_calls"),
    ]
    monkeypatch.setattr(
        client._client.chat.completions, "create", lambda **kwargs: iter(chunks)
    )
    resp = client.complete([Message(role="user", content="加一下")])

    assert len(resp.message.tool_calls) == 1
    call = resp.message.tool_calls[0]
    assert call.name == "add"
    assert json.loads(call.arguments) == {"a": 17, "b": 25}
    assert resp.attempts == 1
