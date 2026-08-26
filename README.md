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
- **sink** — where routing decisions go: `log` (default), `memory`, or `none`.

## Tests

```bash
go test ./...
```

There is also a live smoke suite that talks to real providers (it costs
money, so it is not part of CI). With go-route already running:

```bash
pip install -r scripts/requirements-dev.txt
pytest scripts/ --base-url http://localhost:4000
```
