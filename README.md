# go-route

An OpenAI-compatible LLM proxy. Point any OpenAI client at it, request a
model *alias*, and go-route tries a ladder of real targets in order and
failing over to the next one when a target is unreachable, until the
response has started streaming.

## Requirements

- Go 1.26+
- An `OPENAI_API_KEY` (the sample config routes to OpenAI)

## Run it

Copy the example config, then start the proxy:

```bash
# this is just so someone can use example yaml file to run
cp configs/go-route.example.yaml configs/go-route.yaml

# please put your api key here
export OPENAI_API_KEY=sk-...
go run ./cmd/go-route -config configs/go-route.yaml
```

`configs/` is gitignored apart from
[configs/go-route.example.yaml](configs/go-route.example.yaml), so your
own config stays local.

It listens on `:4000` by default and logs the models it loaded.

## Try it

Health check:

```bash
curl localhost:4000/healthz
```

Non-streaming request:

```bash
curl localhost:4000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"direct","messages":[{"role":"user","content":"say hi"}]}'
```

Streaming request:

```bash
curl -N localhost:4000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"chat","messages":[{"role":"user","content":"say hi"}],"stream":true}'
```

`chat` is deliberately configured with a dead target first, so this call
exercises failover but you should still get a normal response.

With the OpenAI SDK:

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:4000/v1", api_key="unused")
print(client.chat.completions.create(
    model="direct",
    messages=[{"role": "user", "content": "say hi"}],
).choices[0].message.content)
```

## Model aliases in the sample config

| Alias       | Ladder                              | What it shows            |
|-------------|-------------------------------------|--------------------------|
| `direct`    | one working target                  | the happy path           |
| `chat`      | dead target → working target        | failover                 |
| `auth-fail` | bad-credentials target → working    | a 401 stops the ladder   |

## Configuration

Everything lives in one YAML file — start from
[configs/go-route.example.yaml](configs/go-route.example.yaml), which is
commented throughout:

- **providers** — where to dial (`base_url`, `api_key`). `${VAR}` is read
  from the environment; an unset variable is a startup error. Substitution
  runs over the whole file, comments included.
- **targets** — a provider plus a concrete upstream model name.
- **models** — the alias clients ask for, mapped to an ordered ladder of
  targets.
- **sink** — where routing decisions go. See [Decision sinks](#decision-sinks).

## Decision sinks

Every routed request produces a decision record: which targets were
eligible, which one served it, why that one, what it cost, and how long
it took. The sink is where those records land.

| Type       | Where records go                | Survives restart | Use for                                    |
|------------|---------------------------------|------------------|--------------------------------------------|
| `log`      | stdout as structured JSON       | no               | the default; trying go-route out           |
| `postgres` | a `decisions` table             | yes              | anything you intend to report on           |
| `none`     | discarded                       | no               | running the proxy with no audit trail      |

```yaml
sink:
  type: postgres
  dsn: ${DATABASE_URL}

  # Records are buffered and written in batches so persistence never
  # blocks a request. When the buffer fills, records are DROPPED and
  # counted rather than slowing traffic down. A slow database must not
  # become a slow proxy.
  buffer_size: 4096      # records held in memory
  batch_size: 100        # rows per write
  flush_interval: 1s     # write a partial batch after this long
```

`postgres` creates its schema on first connect, so no migration step is
needed to get started. Drops are logged and counted; if you see them, the
database is not keeping up and `buffer_size` or `batch_size` needs
raising.

With `type: none` the proxy still routes and fails over normally. You
simply lose the ability to explain, report on, or attribute any of it
afterwards.

## Tests

```bash
# unit tests and E2E
make unit-test

# integration tests
make integration-test

# all tests
make test-all
```

There is also a live smoke suite that talks to real providers (it costs
money, so it is not part of CI). With go-route already running:

```bash
pip install -r scripts/requirements-dev.txt
pytest scripts/ --base-url http://localhost:4000
```
