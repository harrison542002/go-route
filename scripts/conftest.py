"""Fixtures for the go-route smoke suite.

These tests run against a live proxy and real upstream providers. They
cost money and are excluded from CI — run them by hand before a release,
or after changing anything in the provider adapter or dispatcher.
"""

import httpx
import pytest
from openai import OpenAI

DEFAULT_BASE_URL = "http://localhost:4000"


def pytest_addoption(parser):
    parser.addoption(
        "--base-url",
        default=DEFAULT_BASE_URL,
        help="go-route base URL (default: http://localhost:4000)",
    )


@pytest.fixture(scope="session")
def base_url(pytestconfig):
    return pytestconfig.getoption("--base-url").rstrip("/")


@pytest.fixture(scope="session", autouse=True)
def proxy_is_running(base_url):
    """Fail the whole session fast rather than once per test."""
    try:
        httpx.get(f"{base_url}/healthz", timeout=2)
    except httpx.HTTPError as e:
        pytest.exit(f"go-route is not reachable at {base_url}: {e}", returncode=1)


@pytest.fixture(scope="session")
def client(base_url):
    """The real OpenAI SDK — the point of these tests.

    max_retries=0 because the SDK retries 429 and 5xx by default, which
    would mask exactly the failures we are asserting on.
    """
    return OpenAI(base_url=f"{base_url}/v1", api_key="unused", max_retries=0)


@pytest.fixture
def collect_stream(client):
    """Consume a stream and return (text, chunk_count, ttft_seconds).

    Indexing choices[0] is deliberate: if the injected usage chunk ever
    leaks to the client, this raises IndexError rather than passing
    quietly. That is the bug the UsageOnly flag exists to prevent.
    """
    import time

    def _collect(model, prompt="count from 1 to 10, one per line", **kwargs):
        started = time.monotonic()
        ttft = None
        chunks = 0
        pieces = []

        stream = client.chat.completions.create(
            model=model,
            messages=[{"role": "user", "content": prompt}],
            stream=True,
            extra_headers={"x-go-route-feature": "smoke-test"},
            **kwargs,
        )

        for chunk in stream:
            chunks += 1
            if chunk.choices and chunk.choices[0].delta.content:
                if ttft is None:
                    ttft = time.monotonic() - started
                pieces.append(chunk.choices[0].delta.content)

        return "".join(pieces), chunks, ttft

    return _collect