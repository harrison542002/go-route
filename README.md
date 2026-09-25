# go-route

An OpenAI-compatible LLM proxy. Point any OpenAI client at it, request a
model *alias*, and go-route tries a ladder of real targets in order and
failing over to the next one when a target is unreachable, until the
response has started streaming.

## Requirements

- Go 1.26+
- Docker, for Postgres and Redis (and the integration tests, which start
  their own containers)
- [Atlas](https://atlasgo.io), to apply migrations
- An `OPENAI_API_KEY` (the sample config routes to OpenAI)

## Getting it running

go-route is two binaries — the proxy and the admin service — sharing a
database. `go build ./cmd/...` produces both, or `make build`.

Copy the example config and point the proxy at a database:

```bash
# this is just so someone can use example yaml file to run
cp configs/go-route.example.yaml configs/go-route.yaml

# please put your api key here
export OPENAI_API_KEY=sk-...
export DATABASE_URL=postgres://...
```

```bash
# there is no shared secret; mint yourself a credential once
go run ./cmd/go-route-admin create-token --name billing-sync

go run ./cmd/go-route-admin serve            # 127.0.0.1:4001 by default
```

In another shell, create a tenant and a key — the key is printed once:

```bash
curl -sX POST localhost:4001/admin/v1/tenants -H "Authorization: Bearer $GO_ROUTE_ADMIN_TOKEN" -H 'Content-Type: application/json' -d '{"external_id":"dev","name":"Local dev"}'
```

```bash
curl -sX POST localhost:4001/admin/v1/tenants/dev/keys -H "Authorization: Bearer $GO_ROUTE_ADMIN_TOKEN" -H 'Content-Type: application/json' -d '{}'
```

Then the proxy:

```bash
go run ./cmd/go-route serve --config configs/go-route.yaml
```

It listens on `:4000` by default and logs the models it loaded.

## Try it

Health check:

```bash
curl localhost:4000/healthz
```

A request with the key you just minted:

```bash
curl localhost:4000/v1/chat/completions \
  -H 'Authorization: Bearer gr_live_...' \
  -H 'Content-Type: application/json' \
  -d '{"model":"direct","messages":[{"role":"user","content":"say hi"}]}'
```

Streaming request:

```bash
curl -N localhost:4000/v1/chat/completions \
  -H 'Authorization: Bearer gr_live_...' \
  -H 'Content-Type: application/json' \
  -d '{"model":"chat","messages":[{"role":"user","content":"say hi"}],"stream":true}'
```

`chat` is deliberately configured with a dead target first, so this call
exercises failover but you should still get a normal response.

With the OpenAI SDK:

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:4000/v1", api_key="gr_live_...")
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

Every response carries its decision ID in the `X-Go-Route-Decision-Id`
header; feed it to `go run ./cmd/go-route explain <id>` to see which
targets were tried and what it cost.