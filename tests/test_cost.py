"""成本折算：价格必须每次从环境变量读取，不能停在导入时的值。"""

from __future__ import annotations

from agent.llm.types import Usage
from agent.trace.recorder import cost_yuan, prices


def test_default_price_is_economy_tier() -> None:
    """确认未配置价格环境变量时使用经济档默认单价。"""
    price_in, price_out, price_cached = prices()
    assert (price_in, price_out, price_cached) == (0.8, 3.2, 0.016)


def test_cost_uses_three_tiers(monkeypatch) -> None:
    """确认普通输入、缓存输入和输出 token 分别按对应单价计费。"""
    monkeypatch.setenv("ARK_PRICE_IN", "0.8")
    monkeypatch.setenv("ARK_PRICE_OUT", "3.2")
    monkeypatch.setenv("ARK_PRICE_CACHED", "0.016")
    usage = Usage(input_tokens=10000, output_tokens=5000, total_tokens=15000, cached_tokens=8000)
    # (2000*0.8 + 8000*0.016 + 5000*3.2) / 1e6
    assert abs(cost_yuan(usage) - (1600 + 128 + 16000) / 1_000_000) < 1e-9


def test_env_change_takes_effect_after_import(monkeypatch) -> None:
    """确认模块导入后修改环境变量仍会影响后续成本计算。"""
    monkeypatch.setenv("ARK_PRICE_OUT", "10")
    usage = Usage(input_tokens=0, output_tokens=1_000_000, total_tokens=1_000_000)
    assert abs(cost_yuan(usage) - 10.0) < 1e-9


def test_cached_tokens_not_billed_at_full_price(monkeypatch) -> None:
    """确认缓存输入 token 不会按普通输入的完整价格重复计费。"""
    monkeypatch.setenv("ARK_PRICE_IN", "0.8")
    monkeypatch.setenv("ARK_PRICE_CACHED", "0.016")
    usage = Usage(
        input_tokens=1_000_000,
        output_tokens=0,
        total_tokens=1_000_000,
        cached_tokens=1_000_000,
    )
    assert abs(cost_yuan(usage) - 0.016) < 1e-9
