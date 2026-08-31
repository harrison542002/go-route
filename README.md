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
go run ./cmd/go-route serve --config configs/go-route.yaml
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

## Inspect what happened

Two commands read the decision log: `explain` for one request, `report`
for a period. Both need a decision store, so set `sink.type: postgres`
(see [Decision sinks](#decision-sinks)) or pass `--dsn` on the command
line.

### `explain` - why did *this* request go there?

Every response carries its decision ID in the `X-Go-Route-Decision-Id`
header, and every decision log line records it. Feed it back:

```bash
go run ./cmd/go-route explain dec_01a054ff-4eb0-75fd-b67b-44b75a4b88fe
```

```
  dec_01a054ff-4eb0-75fd-b67b-44b75a4b88fe
  2026-08-30 09:14:02 UTC · tenant default

  Request
    model     chat
    stream    yes
    metadata  feature=auto-tag team=platform

  Decision
    reason  model alias "chat"
    ladder  openai/gpt-5-mini-eu → openai/gpt-5-mini
    chose   openai/gpt-5-mini
    status  ok

  Attempts
    1  openai/gpt-5-mini-eu  connect  31ms  connection refused
    2  openai/gpt-5-mini     ok       1893ms

  Usage
    input   1840  (1024 cached)
    output  512   (128 reasoning)

  Cost
    actual           $0.001196  (price table 2026-08-01)
    vs openai/gpt-5  $0.005980  +$0.004784
                    estimated: actual token counts repriced

  Timing
    first token  412ms
    total        1893ms
```

The ladder line is the whole point: the first target refused the
connection, the second served, and the request still succeeded. The
`vs` line reprices the same token counts against an alternative - `+`
means that alternative would have cost that much *more*.

| Flag       | Default                 | What it does                              |
|------------|-------------------------|-------------------------------------------|
| `--json`   | off                     | print the raw record instead of the table |
| `--dsn`    | from the config's sink  | database to read from                     |
| `--config` | `configs/go-route.yaml` | config file to take the DSN from          |

An ID the store does not hold is reported as a missing decision - it may
simply have aged out of the table - rather than as an empty record.

### `report` - what did a period cost?

```bash
go run ./cmd/go-route report --since 30d --group-by feature
```

```
  default · 2026-08-01 00:00 to 2026-08-31 00:00

  feature      requests  cost      vs openai/gpt-5  p95 ttft  ok        fail
  auto-tag     18,402    $214.87   $1074.35         512ms     100%      0%
  chat-widget  6,120     $88.41    $442.05*         664ms     98%       2%
  summariser   1,204     $12.06†   $60.30           738ms     100%      0%
  ────────     ────────  ────────  ────────         ────────  ────────  ────────
  total        25,726    $315.34†  $1576.70

  Comparisons are estimates: actual token counts repriced against
  the alternative's rates. A different model produces different output.
  2% of requests could not be priced and are excluded from cost.
  * partial comparison: openai/gpt-5 covers 92% of requests.
```

| Flag         | Default                 | What it does                                                        |
|--------------|-------------------------|---------------------------------------------------------------------|
| `--since`    | `7d`                    | start of the period: a duration (`30d`, `24h`, `90m`) or a date      |
| `--until`    | now                     | end of the period, **exclusive**; same formats as `--since`          |
| `--group-by` | *(ungrouped)*           | `model`, `target`, `status`, `day`, or any metadata key              |
| `--tenant`   | `default`               | tenant to report on                                                  |
| `--limit`    | `500`                   | maximum groups shown; the rest are counted in a footnote             |
| `--format`   | `table`                 | `table`, `json`, or `csv`                                            |
| `--dsn`      | from the config's sink  | database to read from                                                |
| `--config`   | `configs/go-route.yaml` | config file to take the DSN from                                     |

Dates accept `2026-08-01`, `2026-08-01 09:30`, or full RFC 3339.
Durations accept `d`, `h`, `m`, `s` - `30d` included, which Go's own
duration parser does not take.

#### `--group-by`

| Value            | One row per                                            | Answers                              |
|------------------|--------------------------------------------------------|--------------------------------------|
| *(omitted)*      | the whole period                                       | what did this cost in total?         |
| `model`          | requested alias                                        | which alias is expensive?            |
| `target`         | target that served, `(unserved)` when all of them failed | which provider is actually carrying traffic? |
| `status`         | outcome                                                | how often does routing fail?         |
| `day`            | UTC calendar day                                       | is spend trending up?                |
| *anything else*  | value of that metadata key, `(unset)` when absent      | which feature/team/customer spends?  |

Metadata comes from `x-go-route-*` request headers, so a client that
sends `x-go-route-feature: auto-tag` can be reported on with
`--group-by feature`:

```bash
curl localhost:4000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'x-go-route-feature: auto-tag' \
  -H 'x-go-route-team: platform' \
  -d '{"model":"chat","messages":[{"role":"user","content":"say hi"}]}'
```

#### Reading the table

| Mark       | Means                                                             |
|------------|-------------------------------------------------------------------|
| `†`        | some requests in that row had no price and are excluded from cost |
| `*`        | the comparison covers only part of that row's traffic             |
| `-`        | no data for that cell                                             |

The total row deliberately leaves latency and outcome columns blank: a
p95 of p95s is not a p95. Run the report ungrouped for overall latency.

For anything beyond eyeballing, take the machine-readable formats -
`--format csv` for a spreadsheet, `--format json` for a pipeline:

```bash
go run ./cmd/go-route report --since 30d --group-by day --format csv > spend.csv
go run ./cmd/go-route report --since 7d --format json | jq '.Total'
```

CSV renders costs as dollars; JSON renders them as nanodollars (1e-9
USD), the unit they are summed in, so no rounding creeps into a figure
you go on to do arithmetic with.

## Configuration

Everything lives in one YAML file - start from
[configs/go-route.example.yaml](configs/go-route.example.yaml), which is
commented throughout:

- **providers** - where to dial (`base_url`, `api_key`). `${VAR}` is read
  from the environment; an unset variable is a startup error. Substitution
  runs over the whole file, comments included.
- **targets** - a provider plus a concrete upstream model name.
- **models** - the alias clients ask for, mapped to an ordered ladder of
  targets.
- **sink** - where routing decisions go. See [Decision sinks](#decision-sinks).

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
