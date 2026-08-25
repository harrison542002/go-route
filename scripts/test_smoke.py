"""End-to-end checks against a running go-route and live providers.

Requires configs/go-route.yaml to define these model aliases:
    direct     — a single working target
    chat       — a dead target first, a working one second
    auth-fail  — a target with bad credentials first, working one second
"""

import httpx
import pytest
from openai import APIStatusError

pytestmark = pytest.mark.smoke


class TestStreaming:
    def test_relays_content(self, collect_stream):
        text, chunks, ttft = collect_stream("direct")

        assert text.strip(), "no content relayed"
        assert chunks > 1, f"expected a multi-chunk stream, got {chunks}"
        assert ttft is not None, "no first token recorded"

    def test_sdk_accepts_every_chunk(self, collect_stream):
        """A leaked usage chunk raises IndexError inside collect_stream.

        Reaching this assertion at all is the real result; the SDK is
        stricter about chunk shape than any client we would write.
        """
        text, _, _ = collect_stream("direct", prompt="say hi")
        assert text

    def test_non_streaming_reports_usage(self, client):
        resp = client.chat.completions.create(
            model="direct",
            messages=[{"role": "user", "content": "say hi"}],
        )

        assert resp.usage.prompt_tokens > 0
        assert resp.usage.completion_tokens > 0
        assert resp.choices[0].message.content


class TestFailover:
    def test_dead_primary_is_invisible_to_the_client(self, collect_stream):
        """The 'chat' ladder starts with an unreachable port.

        A connection failure happens before the commit boundary, so the
        dispatcher can try the next target and the client sees a normal
        response. Attempt count is only visible in the decision record.
        """
        text, chunks, _ = collect_stream("chat")

        assert text.strip(), "failover did not produce content"
        assert chunks > 1

    def test_auth_failure_stops_the_ladder(self, client):
        """A 401 means our own credentials are wrong for that target.

        Every remaining target would fail identically, so the ladder must
        stop. The client gets 502, not 401: telling them 401 would send
        them checking a key that is not at fault.
        """
        with pytest.raises(APIStatusError) as exc:
            client.chat.completions.create(
                model="auth-fail",
                messages=[{"role": "user", "content": "hi"}],
                stream=True,
            )

        assert exc.value.status_code == 502, (
            f"got {exc.value.status_code}; if this is 200, classify() is "
            "marking 401 retryable and the ladder walked past it"
        )

    def test_upstream_error_text_does_not_leak(self, client):
        with pytest.raises(APIStatusError) as exc:
            client.chat.completions.create(
                model="auth-fail",
                messages=[{"role": "user", "content": "hi"}],
                stream=True,
            )

        assert "api key" not in exc.value.message.lower()
        assert "incorrect" not in exc.value.message.lower()


class TestRejection:
    @pytest.mark.parametrize(
        "model,reason",
        [
            ("definitely-not-configured", "unknown model"),
            ("", "empty model"),
        ],
    )
    def test_unroutable_models_are_rejected(self, client, model, reason):
        with pytest.raises(APIStatusError) as exc:
            client.chat.completions.create(
                model=model,
                messages=[{"role": "user", "content": "hi"}],
                stream=True,
            )

        assert exc.value.status_code == 400, reason

    def test_error_lists_available_models(self, client):
        with pytest.raises(APIStatusError) as exc:
            client.chat.completions.create(
                model="definitely-not-configured",
                messages=[{"role": "user", "content": "hi"}],
            )

        assert "available" in exc.value.message.lower()


class TestHeaders:
    def test_decision_id_is_present_and_prefixed(self, base_url):
        """This header is written only by ClientStream.Commit, so its
        presence is independent proof the commit boundary was crossed."""
        with httpx.stream(
            "POST",
            f"{base_url}/v1/chat/completions",
            json={
                "model": "direct",
                "messages": [{"role": "user", "content": "hi"}],
                "stream": True,
            },
            timeout=30,
        ) as r:
            decision_id = r.headers.get("x-go-route-decision-id", "")
            content_type = r.headers.get("content-type", "")
            r.close()

        assert decision_id.startswith("dec_"), f"got {decision_id!r}"
        assert "text/event-stream" in content_type

    def test_no_decision_id_when_nothing_commits(self, base_url):
        """An exhausted ladder never commits, so the header must be
        absent. This is a second, independent check on the boundary."""
        r = httpx.post(
            f"{base_url}/v1/chat/completions",
            json={
                "model": "auth-fail",
                "messages": [{"role": "user", "content": "hi"}],
                "stream": True,
            },
            timeout=30,
        )

        assert r.status_code == 502
        assert not r.headers.get("x-go-route-decision-id")
        assert "application/json" in r.headers.get("content-type", "")


class TestDisconnect:
    def test_server_survives_a_client_hangup(self, base_url, client):
        """Abandoning a stream mid-flight must not break the server, and
        must still produce a decision record with status
        client_disconnect. Check the server's stdout to confirm.
        """
        with httpx.stream(
            "POST",
            f"{base_url}/v1/chat/completions",
            json={
                "model": "direct",
                "messages": [{"role": "user", "content": "write a long story"}],
                "stream": True,
            },
            timeout=30,
        ) as r:
            next(r.iter_bytes())  # take one chunk, then abandon

        # The next request proves the server is still healthy.
        resp = client.chat.completions.create(
            model="direct",
            messages=[{"role": "user", "content": "hi"}],
        )
        assert resp.choices[0].message.content