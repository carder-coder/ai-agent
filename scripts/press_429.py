"""Concurrent, rate-limit retry probe against the configured Ark endpoint."""

from __future__ import annotations

import argparse
import json
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path

from agent.cli.main import load_dotenv
from agent.llm import DoubaoClient, Message, ProviderError, ProviderRateLimited
from agent.llm.doubao import RETRY_DELAYS

PROMPT = "Reply with the single word: ok"
ALLOWED_DELAY_TOLERANCE_S = 0.2


def _worker(worker_id: int, request_count: int) -> list[dict]:
    records = []
    client = DoubaoClient()
    try:
        for request_no in range(1, request_count + 1):
            started = time.perf_counter()
            error = None
            try:
                client.complete(
                    [Message(role="user", content=PROMPT)],
                    max_tokens=16,
                )
            except ProviderError as exc:
                error = exc
            except Exception as exc:  # Keep a bad request visible without killing the pool.
                error = exc
            elapsed_ms = round((time.perf_counter() - started) * 1000)
            is_rate_limited = isinstance(error, ProviderRateLimited) or bool(
                client.last_retry_delays
            )
            status_code = 429 if is_rate_limited else getattr(error, "status_code", None)
            records.append(
                {
                    "worker": worker_id,
                    "request": request_no,
                    "ok": error is None,
                    "error_code": status_code,
                    "error": None if error is None else f"{type(error).__name__}: {error}",
                    "elapsed_ms": elapsed_ms,
                    "attempts": client.last_attempts,
                    "retry_delays": list(client.last_retry_delays),
                }
            )
    finally:
        client._client.close()
    return records


def main() -> int:
    parser = argparse.ArgumentParser(description="压测真实模型端点的 429 退避行为")
    parser.add_argument("--workers", type=int, default=8)
    parser.add_argument("--requests-per-worker", type=int, default=20)
    args = parser.parse_args()
    if args.workers < 1 or args.requests_per_worker < 1:
        parser.error("并发数和每路请求数必须为正整数")

    load_dotenv(Path.cwd())
    try:
        config_check = DoubaoClient()
        config_check._client.close()
    except ProviderError as exc:
        parser.exit(1, f"无法初始化模型：{exc}\n")

    records = []
    with ThreadPoolExecutor(max_workers=args.workers) as pool:
        futures = [
            pool.submit(_worker, worker_id, args.requests_per_worker)
            for worker_id in range(1, args.workers + 1)
        ]
        for future in as_completed(futures):
            try:
                records.extend(future.result())
            except Exception as exc:
                print(json.dumps({"worker_error": f"{type(exc).__name__}: {exc}"}))
                return 1

    saw_429 = False
    invalid_backoff = False
    for record in sorted(records, key=lambda item: (item["worker"], item["request"])):
        print(json.dumps(record, ensure_ascii=False))
        if record["error_code"] == 429:
            saw_429 = True
            delays = record["retry_delays"]
            if record["attempts"] <= 1 or len(delays) != record["attempts"] - 1:
                invalid_backoff = True
            expected_delays = RETRY_DELAYS[: len(delays)]
            if len(expected_delays) != len(delays) or any(
                abs(actual - expected) > ALLOWED_DELAY_TOLERANCE_S
                for actual, expected in zip(delays, expected_delays, strict=True)
            ):
                invalid_backoff = True

    print(
        json.dumps(
            {
                "requests": len(records),
                "saw_429": saw_429,
                "invalid_backoff": invalid_backoff,
            }
        )
    )
    if invalid_backoff:
        return 2
    return 0 if saw_429 else 3


if __name__ == "__main__":
    raise SystemExit(main())
