# go-route

An OpenAI-compatible LLM proxy. Point any OpenAI client at it, request a
model *alias*, and go-route tries a ladder of real targets in order and
failing over to the next one when a target is unreachable, until the
response has started streaming.

## Requirements

- Go 1.26+
- An `OPENAI_API_KEY` (the sample config routes to OpenAI)

## Two binaries

`go build ./cmd/...` produces both, or `make build`:

| Binary           | Serves                                        | Configured by                     |
|------------------|-----------------------------------------------|-----------------------------------|
| `go-route`       | the proxy, plus `explain` and `report`        | a YAML config file                |
| `go-route-admin` | the [admin API](#admin-api) and its credentials | flags and the environment       |

They share a database and nothing else. The admin API touches only
Postgres — no routing table, no provider clients, no in-memory state the
proxy owns — so there was nothing for them to gain from one process, and
plenty to lose: admin traffic is a handful of calls a minute against a
service that has to stay up while the proxy scales out with tenant
traffic. Separate binaries mean separate pools, so a slow admin query
cannot take connections from a request being served; separate release
paths, so a change to one does not restart the other; and separate
sockets, so the admin service can be bound to a private interface, run at
one small replica, and never be reachable from the internet at all.

## Run it

Copy the example config, then start the proxy:

```bash
# this is just so someone can use example yaml file to run
cp configs/go-route.example.yaml configs/go-route.yaml

# please put your api key here
export OPENAI_API_KEY=sk-...
export DATABASE_URL=postgres://...
go run ./cmd/go-route serve --config configs/go-route.yaml
```

`configs/` is gitignored apart from
[configs/go-route.example.yaml](configs/go-route.example.yaml), so your
own config stays local.

It listens on `:4000` by default and logs the models it loaded.

The tenants and keys it authenticates are created over the
[admin API](#admin-api), which is a second binary and a second process:

```bash
export DATABASE_URL=postgres://...

# there is no shared secret; mint yourself a credential once
go run ./cmd/go-route-admin create-token --name billing-sync

go run ./cmd/go-route-admin serve            # 127.0.0.1:4001 by default
```

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

There is no **admin** key. The admin API is its own binary, configured by
flags and the environment; see [Admin API](#admin-api). A config file
carried over from before the split still has an `admin:` block, and
because unknown keys are a load error rather than a silent no-op, it will
now refuse to start until the block is deleted.

## Decision sinks

Every routed request produces a decision record: which targets were
eligible, which one served it, why that one, what it cost, and how long
it took. The sink is where those records land.

Postgres is required, and there is no other option. go-route cannot
authenticate a request without `api_keys`, so a deployment without a
database cannot serve traffic at all — which made a choice of sink a
choice between recording a request and serving one it could not
attribute.

```yaml
sink:
  dsn: ${DATABASE_URL}

  # Records are buffered and written in batches so persistence never
  # blocks a request. When the buffer fills, records are DROPPED and
  # counted rather than slowing traffic down. A slow database must not
  # become a slow proxy.
  buffer_size: 4096      # records held in memory
  batch_size: 100        # rows per write
  flush_interval: 1s     # write a partial batch after this long
```

`postgres` expects an already-migrated database. Schema is owned by
[Atlas](https://atlasgo.io):

```bash
export DATABASE_URL=postgres://...
atlas migrate apply --env local    # or: make migrate
```

Clients authenticate with an API key in the standard place, so pointing
an existing OpenAI SDK at go-route needs a base URL change and nothing
else:

```
Authorization: Bearer gr_live_...
```

The key decides the tenant. It is SHA-256'd and looked up in `api_keys`,
which yields the tenant, the key id recorded against the spend, and the
model allowlist that key may request. A client never names the tenant it
wants billed. Unknown and revoked keys are both 401 with the same body;
a disabled tenant or a model outside the key's allowlist is 403; a
database that cannot be reached is 503, never a rejection.

With the `log` and `none` sinks there is no `api_keys` table to check
against, so every request is admitted as the default tenant. That is the
"trying go-route out" mode and it authenticates nothing.

Each record is split in two: the money goes to `usage_ledger` and the
routing story to `audit_log`, both keyed by the same decision ID and
written in one transaction. Drops are logged and counted; if you see
them, the database is not keeping up and `buffer_size` or `batch_size`
needs raising.

Monthly partitions for those two tables are created by the gateway, not
by a migration: which months a deployment needs depends on how long it
has been running, not on its schema version.

See [db/migrations/](db/migrations/) for the schema, [db/queries/](db/queries/)
for the queries sqlc compiles, and [db/gen/](db/gen/) for what it generates.

Both long-lived processes top those monthly partitions up on a daily
timer, not only at startup: coverage pinned to the last restart would run
out on a long-lived process, and every write after that would fail. The
admin service needs it as much as the proxy does — its audit rows land in
the same monthly partitions. The credential commands only ensure
coverage once, since they are over in milliseconds and cannot outrun the
calendar.

## Admin API

Tenants, their API keys and their quotas are managed over an internal
HTTP API. It is the only way they are created: go-route provisions
nothing implicitly, and the customer's billing system - not go-route -
decides what each tenant is allowed.

It is served by its own binary, `go-route-admin`:

```bash
go build ./cmd/...              # or: make build
./bin/go-route-admin serve --listen 127.0.0.1:4001 --dsn "$DATABASE_URL"
```

There is no config file. The proxy's YAML exists to describe providers,
targets, aliases and pricing, none of which this service reads, so
pointing it at that file would only be a way to share a blast radius for
no benefit. Three settings is not a file's worth of settings:

| Flag                 | Default                   | What it does                                                                         |
|----------------------|---------------------------|--------------------------------------------------------------------------------------|
| `--dsn`              | `$DATABASE_URL`           | the database; the flag wins, `DATABASE_URL` is the fallback, neither is a startup error |
| `--listen`           | `127.0.0.1:4001`          | address of the admin listener; loopback by default, because this is not for tenants  |
| `--request-timeout`  | `10s`                     | deadline for each admin request, database work included; must be above `0s`, at most `2m` |
| `--jwt-secret`       | `$GO_ROUTE_JWT_SECRET`    | HMAC secret signing the access tokens people get when they log in; required, at least 32 bytes |
| `--access-token-ttl` | `15m`                     | how long an access token verifies; between `1m` and `24h`                            |

`--dsn` is persistent, so it applies to `serve` and to the credential and
user commands alike; the rest belong to `serve`.

`--jwt-secret` has no default and nothing generates one. A secret this
process invented would be a different secret after every restart, so
every session would end with a deployment — and, worse, two replicas
would refuse each other's tokens while appearing to work. Set it from
wherever the deployment keeps secrets:

```bash
GO_ROUTE_JWT_SECRET=$(openssl rand -base64 48) go-route-admin serve
```

Everything wrong is reported in one error, the way an invalid config file
is, so a deployment is fixed in one pass rather than one restart per
mistake:

```
Error: admin: invalid:
  - --request-timeout must be above 0s and at most 2m0s, got 10m0s
  - no database configured: pass --dsn, or set DATABASE_URL
  - no signing secret configured: pass --jwt-secret, or set GO_ROUTE_JWT_SECRET
```

There is no token here either. Callers authenticate against the database:
a machine with its own named credential, a person with an email and a
password they exchange for a short-lived token; see
[Two kinds of caller](#two-kinds-of-caller).

Every request runs under `request_timeout`. The deadline travels in the
request's context down through the use case into every database call,
so a statement stuck behind a lock is cancelled and its transaction
rolled back, rather than holding a connection and the tenant's lock for
as long as the database cares to take. The caller gets a `504`; nothing
was committed, so retrying is safe. The listener's write timeout is
derived from it, so the `504` always has time to be sent.

The contract is [schemas/admin/openapi.yaml](schemas/admin/openapi.yaml),
an OpenAPI 3.0 document: every path, body, status code and error, with
the reasoning behind each. It is not documentation written after the
fact. The request and response types, the routing and the decoding in
[schemas/admin/gen/](schemas/admin/gen/)
are generated from it with [oapi-codegen](https://github.com/oapi-codegen/oapi-codegen),
configured by [schemas/admin/oapi-codegen.yaml](schemas/admin/oapi-codegen.yaml)
and pinned as a `tool` in `go.mod`, and every request is validated against it before a handler runs. To
change the API, change the spec and regenerate:

```bash
make openapi    # or: go generate ./schemas/admin/gen
```

CI runs `go generate ./...` and fails on a diff, so the generated code
cannot drift from the spec.

It is a separate process, not a path on the proxy, so that it can be
bound to loopback or a private interface, kept off the internet entirely,
and run at one replica while the proxy scales with tenant traffic. See
[Two binaries](#two-binaries). A deployment that does not want an admin
API simply does not run it.

### Two kinds of caller

Two things reach this API and they need different credentials.

A **machine** — a billing sync job, CI, a support console — holds a
named, long-lived token from `create-token`, read out of its own secret
store. A **person** opening the admin dashboard cannot do that: nobody
should be pasting a token that never expires into a browser, and if they
did it would end up in local storage, in a screenshot and in a bug
report. So a person has an account with an email and a password, logs in,
and gets a short-lived token instead.

| | Machine | Person |
|---|---|---|
| Credential | a named token, `gr_admin_…` | an email and a password |
| Made by | `go-route-admin create-token` | `go-route-admin create-user` |
| Presented as | `Authorization: Bearer gr_admin_…` | `Authorization: Bearer <jwt>` |
| Audited as | `admin:<name>` | `user:<email>` |
| Taken away by | `revoke-token` | `disable-user` |
| Lifetime | until revoked, or `--expires-in` | the access token's TTL; the client refreshes |

Past authentication there is one model, not two. Both kinds resolve to an
actor string and a role, and the permission check, the handlers and the
audit rows never ask which kind they are looking at. That is the whole
point of doing it this way: the second identity kind added one middleware
branch and no new concept anywhere else, and a third — an OIDC subject,
say — would add one more.

### Credentials

Every machine caller holds its own credential: a row in `admin_credentials` with
a name, the SHA-256 of a token, a role and an optional expiry. The name
is the identity, so a change made with the `billing-sync` credential is
audited as `admin:billing-sync` whatever the caller claims to be.

They are minted from the CLI, never over the API. That is deliberate: an
endpoint that mints a credential from a credential is a
privilege-escalation surface, and one worth designing on purpose later
rather than acquiring by accident now. It is also how a fresh deployment
gets its first credential, with no shared secret sitting in a config
file waiting to be rotated everywhere at once.

```bash
# the first credential on a fresh database; needs the DSN, not a
# running service - not even this one
go-route-admin create-token --name billing-sync --dsn "$DATABASE_URL"

  billing-sync
  id 0199c4f2-6c1a-7a41-9f02-2b9d4b0f5a10

  gr_admin_4k2xq7ftm9vbz3n6hs8r5wjc2ydpl4ae7gu9knxm3btq6vhs2z

  This token is shown once and cannot be shown again: only its
  SHA-256 is stored. Lose it and you revoke this credential and
  mint another.

  Use it as:  Authorization: Bearer <token>

# a credential for the dashboard's read-only users, good for a quarter
go-route-admin create-token --name dashboard-viewer --role readonly --expires-in 90d

  dashboard-viewer
  id 0199c4f2-6c1a-7a41-9f02-2b9d4b0f5a11
  role readonly (reads only; any change is refused with 403)
  expires 2026-12-21 09:14 UTC

  gr_admin_zk4m8qvhtn2bs6rc9wjd3yxpl7ae5gu4knxm3btq6vhs2z

  This token is shown once and cannot be shown again: only its
  SHA-256 is stored. Lose it and you revoke this credential and
  mint another.

  Use it as:  Authorization: Bearer <token>

go-route-admin list-tokens
NAME              TOKEN             ROLE      CREATED     LAST USED         EXPIRES                     REVOKED
billing-sync      gr_admin_4k2xq7…  admin     2026-09-22  2026-09-22 14:07  —                           —
dashboard-viewer  gr_admin_zk4m8q…  readonly  2026-09-22  —                 2026-12-21 09:14            —
quarterly-audit   gr_admin_aaaaaa…  readonly  2026-06-01  2026-08-30 11:02  2026-08-31 00:00 (expired)  —

go-route-admin revoke-token billing-sync
revoked billing-sync (gr_admin_4k2xq7…), last used 2026-09-22 14:07
```

| Command                                    | Does                                                  |
|--------------------------------------------|-------------------------------------------------------|
| `go-route-admin create-token --name <name> [--role admin\|readonly] [--expires-in 90d]` | mints one and prints the token, once |
| `go-route-admin list-tokens`                | names, token prefixes, roles, created, last used, expiry, revoked (`--json` for raw rows) |
| `go-route-admin revoke-token <name-or-id>`  | kills it; the row stays                               |
| `go-route-admin serve`                      | runs the API itself                                   |

They are the admin binary's own commands, not a subcommand group under
it: an `admin` level inside `go-route-admin` would only repeat the
binary's name.

Every call sends its token as `Authorization: Bearer <token>`. The token
is hashed and looked up on every request: admin traffic is a handful of
calls a minute, so the round trip costs nothing that matters, and in
exchange a revocation takes effect immediately in every process, with no
cache to invalidate. A token matching no credential, a token whose
credential is revoked and a token whose credential has expired get the
same 401, word for word, as does every path under `/admin/`, known or
not — so the API cannot be mapped and a guessed token cannot be
confirmed to have once existed or to have worked yesterday.

#### Roles

| Role       | May                                                                  |
|------------|----------------------------------------------------------------------|
| `admin`    | everything the API offers; the default, and what every credential could do before roles existed |
| `readonly` | read only: `GET` and `HEAD`. Any other method is a `403` with type `permission_error` |

An admin dashboard is what asks for the split. It shows spend, usage and
audit history, and the people who need to look at those — finance,
support — are not the people who should be able to lift a quota, mint a
key or disable a tenant. Until roles existed the only safe answer to "can
they see it" was no.

The check runs as its own middleware step, immediately after
authentication and wrapping the whole `/admin/` tree, exactly as the
token check does. It decides from the method alone: HTTP already says
which requests are safe, the spec puts every read behind `GET` and every
change behind `POST`, `PATCH`, `PUT` or `DELETE`, and anything else is
treated as a change. So a path added later is covered by existing rather
than by someone remembering it, and there is no per-endpoint list to
keep in step with the spec.

The refusal is a `403` and says plainly that the credential is read-only.
Unlike the `401` it may be specific: the caller is authenticated, so
naming its own role tells it nothing it could not read off its own
credential — and a dashboard can grey a control out instead of guessing.

There are still no per-resource scopes. Two roles are the line anyone has
actually asked for; a permission model would be a configuration to get
wrong in exchange for a distinction nobody has needed drawn.

#### Expiry

`--expires-in` mints a credential that stops working after a while, in
the same duration forms `go-route report --since` takes — `90d`, `24h`,
`90m`. Without the flag the credential does not expire, which the output
of `create-token` says in as many words rather than leaving implied.

Expiry is checked when the token is presented, not swept from the table
by a job. A sweep would have to delete or blank the row, and the audit
rows the credential wrote name it: the history would stop being readable.
So an expired credential stays exactly as attributable as a revoked one,
and `list-tokens` marks it `(expired)` in its own column, because running
out and being taken away are different facts even though the API refuses
both the same way.

Credentials are never deleted, only revoked, for the reason keys and
tenants are not: the audit rows they wrote name them, and history has to
stay readable. `last_used_at` is written best-effort and out of band, so
it answers "is anyone still using this before I revoke it" without ever
failing or slowing a request.

Creating and revoking a credential are themselves audited, as
`cli:<os user>` — not `admin:<name>`, because nobody presented a
credential to run a CLI command, and the one table whose value is that
it does not lie should not start there.

### People

A person is a row in `admin_users`: an email, an argon2id hash of a
password, a role and an optional disabled stamp. The email is the
identity, so a quota lifted by `ops@example.com` is audited as
`user:ops@example.com`. Emails are stored and compared lower-cased —
normalised in Go on the way in, with a `CHECK` on the column to catch a
path that forgets — because an audit row naming `Ops@example.com` when
the account is `ops@example.com` is an audit row that names nobody.

Accounts are made from the CLI, never over the API, for the reason
credentials are: an endpoint that creates an account from an account is a
privilege-escalation surface. It is also how the first person exists on a
fresh deployment.

```bash
# the first person on a fresh database; needs the DSN, not a running
# service - not even this one
go-route-admin create-user --email ops@example.com --role admin

  ops@example.com
  id 0199c4f2-6c1a-7a41-9f02-2b9d4b0f5a20
  role admin (the whole API)

  qv7m2kx9dphs4btn6rwz3yjc8aefl5gu2knxm3btq6vhs2z

  This password is shown once and cannot be shown again: only an
  argon2id hash of it is stored. Lose it and set another with
  set-password.

  Log in at:  POST /admin/v1/auth/login

go-route-admin list-users
EMAIL               ROLE      CREATED     LAST LOGIN        PASSWORD SET  DISABLED
ops@example.com     admin     2026-09-23  2026-09-23 10:41  2026-09-23    —
viewer@example.com  readonly  2026-09-23  —                 2026-09-23    —
gone@example.com    admin     2026-06-01  2026-08-30 11:02  2026-06-01    2026-09-01 08:15

go-route-admin set-password --email ops@example.com
go-route-admin disable-user gone@example.com
disabled gone@example.com (admin), last login 2026-08-30 11:02
```

| Command | Does |
|---------|------|
| `go-route-admin create-user --email <e> [--role admin\|readonly] [--password <p>]` | creates one and prints the generated password, once |
| `go-route-admin list-users` | emails, roles, created, last login, password set, disabled (`--json` for raw rows) |
| `go-route-admin disable-user <email>` | locks them out and revokes every session they hold; the row stays |
| `go-route-admin set-password --email <e> [--password <p>]` | replaces the password and ends every session opened under the old one |

Without `--password` a strong one is generated, which is the way to use
these. There is no interactive prompt: a command that blocks on a
terminal is a command no provisioning script can run. Passwords are
hashed with argon2id — 64 MiB, three passes, two lanes — and stored as a
PHC string, so the parameters travel with each hash and raising them
later leaves the rows written under the old ones verifiable.

A password given with `--password` has to satisfy the policy: between 12
and 128 characters, counted as characters rather than bytes so a
passphrase in any script gets the same allowance, not entirely
whitespace, and not containing the local part of the address it belongs
to or the name of this product. Nothing is trimmed, so a leading or
trailing space is a character like any other. The refusal names the rule
that failed and never repeats the password back. There are deliberately
no character-class requirements — demanding a digit, a capital and a
symbol moves people onto a handful of shapes a cracker tries first, for
almost no entropy, and [NIST SP
800-63B](https://pages.nist.gov/800-63-3/sp800-63b.html) tells verifiers
not to impose them.

The 128-character ceiling is a limit on work, not on taste: argon2id
reads the whole password, so `login` refuses anything longer before it
looks the account up, with the same 401 every other failure gets. No
stored password can be that long, so the refusal says nothing.

Nobody is ever deleted, only disabled, for the reason tenants, keys and
credentials are not: the audit rows they wrote name them.

#### Logging in

```bash
curl -s localhost:4001/admin/v1/auth/login \
  -d '{"email":"ops@example.com","password":"qv7m2kx9dphs4btn6rwz3yjc8aefl5gu2knxm3btq6vhs2z"}'

{
  "access_token": "eyJhbGciOiJIUzI1NiIs…",
  "token_type": "Bearer",
  "expires_at": "2026-09-23T10:56:41Z",
  "refresh_token": "gr_refresh_4k2xq7ftm9vbz3n6hs8r5wjc2ydpl4ae7gu9knxm3btq",
  "user": {
    "id": "0199c4f2-6c1a-7a41-9f02-2b9d4b0f5a20",
    "email": "ops@example.com",
    "role": "admin",
    "created_at": "2026-09-23T10:40:02Z",
    "disabled_at": null,
    "last_login_at": "2026-09-23T10:41:41Z"
  }
}

# then, exactly as a machine token is used
curl localhost:4001/admin/v1/tenants -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIs…"

# who am I? answers for either kind of credential
curl localhost:4001/admin/v1/auth/me -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIs…"
{"actor":"user:ops@example.com","kind":"user","role":"admin","user":{…}}
```

The access token is a JWT signed with `--jwt-secret` (HS256), carrying
the person's id as its subject, their email, their role, an issue time,
an expiry and a `jti`. Verification pins the algorithm to HS256, so a
token whose header says `none` — or names an asymmetric algorithm in the
hope the verifier will take its own key as an HMAC secret — is refused
rather than trusted.

**The role in the token is for the dashboard, not for the server.** What
a request may do is read from the `admin_users` row on every request,
exactly as a machine credential is looked up on every request. So
disabling a person, or demoting one to `readonly`, takes effect on their
very next call — there is no window in which a token signed a minute ago
still carries the power it was signed with. The access token's TTL is
about something else: how long a token that leaks is usable at all.

#### The client drives the refresh

The server never renews a session on its own. When the access token
expires the client exchanges its refresh token for a new pair:

```bash
curl -s localhost:4001/admin/v1/auth/refresh \
  -d '{"refresh_token":"gr_refresh_4k2xq7…"}'
```

Refresh tokens are stored — hashed, as everything else is — because that
is what makes logging out and disabling a person actually cut access.
They rotate: each refresh revokes the token presented and issues a new
one, so a token that travels further than the client it was issued to
stops working the moment the real client next refreshes. Presenting a
token that has already been rotated away means two clients hold it, so
the person's whole set is revoked, `user.refresh_reuse` is written to the
audit log, and everybody logs in again. That is the cheapest theft
detection rotation gives away for free.

A refresh token lives 30 days, is swept from the table once it is dead,
and is opaque — keep it where a page's scripts cannot read it. Logging
out revokes the one presented:

```bash
curl -s -X POST localhost:4001/admin/v1/auth/logout \
  -H "Authorization: Bearer $ACCESS_TOKEN" \
  -d '{"refresh_token":"gr_refresh_4k2xq7…"}'
```

It is the one change a `readonly` caller may make, because the role check
is about what the API holds and a session is the caller's own.

#### What a 401 means here

`login` refuses an unknown email, a wrong password, a disabled account
and too many recent failures with one answer, and takes comparable time
over each: an unknown email is verified against a hash of nothing, so
there is no early return for a stopwatch to read the user list out of.
`refresh` refuses an unknown, expired, revoked or replayed token, and one
belonging to a disabled person, with one answer too — a client that
cannot refresh has one thing to do either way, which is to send its user
back to the login form.

A failed login is audited as `user.login_failed` with the email attempted
and the address it came from, and never the password. Repeated failures
for one email from one address are throttled after five in fifteen
minutes. The counter is in-process, which is the right size for the one
small replica this service is meant to run at and is **not** a rate
limiter for a horizontally scaled deployment: a second replica counts its
own failures and the effective limit doubles. It buys time against
someone working through a password list; the argon2id cost is what makes
that expensive.

#### Which paths are open

`POST /admin/v1/auth/login` and `POST /admin/v1/auth/refresh` are the
only two endpoints reachable without a token — a person cannot present a
credential they have not been given yet. Everything else under `/admin/`,
including `logout` and `me`, is guarded.

The allowlist is exact and matched on method and path together, and
anything that does not match falls through to the guarded handler. So a
path added later is covered by existing, a near miss (`/auth/login/`,
`/auth/LOGIN`, `GET` instead of `POST`) is guarded rather than open, and
a test walks every operation in the spec to prove it.

```bash
AUTH="Authorization: Bearer $GO_ROUTE_ADMIN_TOKEN"   # the token you minted

# a tenant, keyed by the id your own system gives it
curl localhost:4001/admin/v1/tenants -H "$AUTH" \
  -d '{"external_id":"cus_123","name":"Acme","metadata":{"plan":"pro"}}'

# a key for it - the plaintext is in this response and nowhere else
curl localhost:4001/admin/v1/tenants/cus_123/keys -H "$AUTH" \
  -d '{"model_allowlist":["chat","direct"]}'

# its limits: 60 requests a minute, $50 a billing period
curl -X PUT localhost:4001/admin/v1/tenants/cus_123/quotas -H "$AUTH" -d '{"quotas":[
  {"window_kind":"minute","max_requests":60},
  {"window_kind":"period","period_start":"2026-09-14T00:00:00Z",
   "period_end":"2026-10-14T00:00:00Z","max_cost_nanos":50000000000}
]}'
```

### Endpoints

`{tenant}` accepts either go-route's UUID for the tenant or its
`external_id`, so a caller can address tenants by its own ids. A value
that parses as a UUID is tried as go-route's id first, then as an
external id.

| Method   | Path                                         | Does                                            |
|----------|----------------------------------------------|-------------------------------------------------|
| `POST`   | `/admin/v1/auth/login`                       | email and password in, a session out (no token) |
| `POST`   | `/admin/v1/auth/refresh`                     | rotate a refresh token into a new one (no token) |
| `POST`   | `/admin/v1/auth/logout`                      | revoke the presented refresh token              |
| `GET`    | `/admin/v1/auth/me`                          | who this token authenticates as, either kind    |
| `POST`   | `/admin/v1/tenants`                          | create a tenant                                 |
| `GET`    | `/admin/v1/tenants`                          | list tenants (`?include_disabled=true&limit=N`) |
| `GET`    | `/admin/v1/tenants/{tenant}`                 | read one                                        |
| `PATCH`  | `/admin/v1/tenants/{tenant}`                 | change `name` and/or `metadata`                 |
| `DELETE` | `/admin/v1/tenants/{tenant}`                 | **disable** it                                  |
| `POST`   | `/admin/v1/tenants/{tenant}/enable`          | re-enable it                                    |
| `POST`   | `/admin/v1/tenants/{tenant}/keys`            | mint a key                                      |
| `GET`    | `/admin/v1/tenants/{tenant}/keys`            | list its keys                                   |
| `GET`    | `/admin/v1/keys/{id}`                        | read one key                                    |
| `PATCH`  | `/admin/v1/keys/{id}`                        | replace its `model_allowlist`                   |
| `DELETE` | `/admin/v1/keys/{id}`                        | **revoke** it                                   |
| `GET`    | `/admin/v1/tenants/{tenant}/quotas`          | the whole quota set                             |
| `PUT`    | `/admin/v1/tenants/{tenant}/quotas`          | replace the whole set                           |
| `GET`    | `/admin/v1/tenants/{tenant}/quotas/{window}` | one window's quota                              |
| `PUT`    | `/admin/v1/tenants/{tenant}/quotas/{window}` | set one window, leave the others                |
| `DELETE` | `/admin/v1/tenants/{tenant}/quotas/{window}` | remove one window's quota                       |
| `GET`    | `/admin/v1/tenants/{tenant}/usage`           | what a period cost, total or grouped            |
| `GET`    | `/admin/v1/tenants/{tenant}/audit`           | audit events, newest first, paged               |
| `GET`    | `/admin/v1/requests/{decision_id}`           | one request's ladder and attempts               |

Nothing a ledger row refers to is ever deleted. `DELETE` on a tenant
sets `disabled_at` - its keys stop authenticating (403), its history
stays readable - and `DELETE` on a key sets `revoked_at` (401 from then
on, with the key still attributable in old ledger rows). Quotas carry
no history, so deleting one really deletes it. `external_id` cannot be
changed: it is the join key into the customer's own records.

Errors use the proxy's envelope, `{"error":{"message":...,"type":...}}`:

| Status | `type`                  | When                                                          |
|--------|-------------------------|---------------------------------------------------------------|
| 400    | `invalid_request_error` | malformed JSON, an unknown field, a value the spec does not allow, input failing validation |
| 401    | `authentication_error`  | no token, or one belonging to no credential or to a revoked one |
| 403    | `permission_error`      | a `readonly` credential attempting a write                    |
| 404    | `not_found_error`       | no such tenant, key or quota; no such endpoint                |
| 405    | `invalid_request_error` | a known path with a method it does not have                   |
| 409    | `conflict_error`        | `external_id` taken by a different tenant; a key for a disabled tenant; changing a revoked key |
| 413    | `invalid_request_error` | a body over 1 MiB                                             |
| 504    | `timeout_error`         | the request outlived `request_timeout`; nothing was committed |

Requests are checked against the spec before anything else runs, so
unknown fields are rejected rather than ignored: `max_request` for
`max_requests` would otherwise silently leave a limit unset. The message
names the field, as in `request body field quotas.0.on_exceed: value is
not one of the allowed values ["block","allow"]`. Bodies are read as
JSON whatever their `Content-Type`, so `curl -d` needs no extra header.

### Retrying a write

A billing system's signup flow times out and retries; a sync job runs
again after a crash. There is no `Idempotency-Key` header to reach for,
because the schema already makes the writes that matter safe to repeat,
and a generic replay layer on top of that bought little: it cost a
table, an hourly sweep to keep it from growing, and a contract that the
retry present the same bytes as the first attempt - which any client
that re-serialises its request breaks by reordering two keys, turning
the retry it asked for into a `422`.

What each repeat actually does:

| Operation               | A repeat...                                                                |
|-------------------------|----------------------------------------------------------------------------|
| create tenant           | with the same body returns the existing tenant, `200` instead of `201`; a different name or metadata for a taken `external_id` is a `409` |
| patch tenant or key     | sets the same values again, which changes nothing                          |
| disable, enable, revoke | of something already in that state is a no-op returning it                 |
| replace quotas          | with the same set changes nothing                                          |
| put quota               | with the same limits changes nothing                                       |
| delete quota            | of one that is not there is still `204`: the limit is gone either way      |
| **mint a key**          | **mints a second key** - see below                                         |

No-ops write nothing - not the row, not an audit entry - so a sync job
reasserting its desired state every minute costs a read.

A write that fails or times out is rolled back whole, audit row
included, so it changed nothing and can simply be sent again.

**Minting a key is the exception.** Every call makes a new secret, so a
retry after a lost response mints a second key rather than returning the
first. That is the shape the data already has - keys are one-to-many
against a tenant, and several live keys is the normal case, not an
error - so a spare is listed by `GET .../keys` alongside the rest and
revoked like any other. A caller that cannot tell whether its first
attempt landed lists the tenant's keys and revokes what it did not mean
to make. The alternative, refusing to mint without a token the caller
has to invent, made the common case harder to serve the uncommon one.

### Keys

A key is `gr_live_` plus 52 characters of base32 from 32 random bytes.
Only its SHA-256 is stored; `key_prefix` (`gr_live_` and six more
characters) is kept so a UI can tell a tenant's keys apart. The
plaintext is in the `secret` field of the response that created it and
nowhere else - nothing writes it down, which is the point of storing
only the hash. A caller that lost that response revokes the key and
mints another.

Admin credentials are minted the same way and by the same code, under
`gr_admin_` instead, so a token pasted in the wrong place is
recognisable as the operator's rather than a tenant's.

`model_allowlist` is the set of aliases the key may request. `null`
allows every alias and `[]` allows none; the distinction is the point,
so `PATCH` requires the field to be present. Aliases are not checked
against the loaded config: a tier can be set up before the alias it
names is deployed.

### Quotas

One quota per window: `minute`, `hour`, `day`, `month`, or `period`,
which carries explicit `period_start` and `period_end` for billing
cycles that run from a signup date rather than the first of the month.

| Field                        | Meaning                                                              |
|------------------------------|----------------------------------------------------------------------|
| `window_kind`                | which window this limits                                             |
| `period_start`, `period_end` | RFC 3339; required for `period`, rejected for anything else          |
| `max_requests`               | requests in the window                                               |
| `max_tokens`                 | tokens in the window                                                 |
| `max_cost_nanos`             | spend in the window, in nanodollars (1e-9 USD)                       |
| `on_exceed`                  | `block` (429, the default) or `allow` (record the overage and keep serving) |

At least one limit must be set, none may be negative, and `null` means
that dimension is unlimited - which is different from `0`. These mirror
the table's constraints, and are checked first so a bad request gets a
sentence rather than a constraint name.

`PUT .../quotas` replaces the tenant's whole set in one transaction:
windows left out are removed, and a failure part way leaves the old set
exactly as it was, rather than the tenant with no limits at all. That
is what lets a sync job simply reassert the state it wants. An empty
set removes every limit, and has to be asked for as `{"quotas": []}` -
a body without the field is a 400.

Quotas are stored and served here; enforcing them at ingress is not
part of this API.

### Audit

Every change writes an `audit_log` row in the transaction that made it,
so a change that rolls back leaves no row claiming it happened. The
actor is `admin:<name>` for a machine credential and `user:<email>` for
a person - the audit log records who, not how they authenticated - the
action one of `tenant.create`, `tenant.update`,
`tenant.disable`, `tenant.enable`, `key.create`, `key.update`,
`key.revoke`, `quota.replace`, `quota.upsert` or `quota.delete`, and
`reason_detail` holds the state the change left behind as JSON - never a
key's secret.

Credential and account changes are audited the same way, as
`admin_credential.create`, `admin_credential.revoke`, `user.create`,
`user.disable` and `user.password_change`, with the actor `cli:<os user>`
because they are made from the CLI and no credential was presented to
make them.

Sessions are audited too: `user.login` and `user.logout` under the
person's own actor, `user.login_failed` with the email attempted and the
address it came from and never the password, and `user.refresh_reuse`
when a refresh token is presented after it was rotated away - the row
that says why everyone was suddenly logged out.

Those detail shapes are in the spec too, as `TenantAuditDetail`,
`APIKeyAuditDetail` and `QuotaAuditDetail`, and generated with
everything else. No path returns them, so generation is kept from
pruning them (`skip-prune`). They are there because a row written today
is read months from now, during an incident: what history is read back
as is a contract like any other, and versioning it with the spec is what
keeps it from drifting every time a response changes.

### Reads for a dashboard

Three endpoints answer over HTTP what `explain` and `report` answer at a
terminal, because a dashboard cannot shell into a box to find out what a
tenant spent:

| Path                                 | The question                                     |
|--------------------------------------|--------------------------------------------------|
| `GET /admin/v1/tenants/{t}/usage`    | what did this period cost, total or grouped?     |
| `GET /admin/v1/tenants/{t}/audit`    | what happened to this tenant, newest first?      |
| `GET /admin/v1/requests/{id}`        | why did *this* request go where it did?          |

They are reads, so a `readonly` credential reaches all three and nothing
else - which is exactly what a dashboard's viewer should hold.

`usage` takes the same period and groupings `report` does: `since`
(required - an unbounded report would scan every partition still on
disk), `until` (exclusive, defaulting to now), `group_by`, `meta_key`
and `limit`. What did not fit the limit is counted in
`truncated_groups` rather than dropped, so a short list is
distinguishable from a complete one.

`audit` pages by cursor rather than page number. `audit_log` is
partitioned and grows without bound: an offset makes the database walk
and discard every row it skips, and it shifts under rows arriving while
a caller pages, repeating one event and missing another. `next_cursor`
is opaque - pass back exactly the string you were given, with the same
`since` and `until` - and it is absent on the last page. A cursor that
does not decode is a 400, not a silent restart from the top, which would
loop a paging client forever.

Every amount is in nanodollars (`cost_nanos`), the unit spend is summed
in, for the same reason `report --format json` uses it: no rounding
creeps into a figure a dashboard goes on to do arithmetic with.

There is deliberately no quota-status endpoint yet - limits against live
counters. Working out where each window starts is the window arithmetic
that belongs with quota enforcement itself, and a second copy of it here
would be one copy too many.

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
