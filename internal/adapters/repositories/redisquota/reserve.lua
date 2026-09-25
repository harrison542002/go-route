-- Check every window of one tenant and reserve against all of them, or
-- against none. Redis runs a script to completion before serving any
-- other command, so between the reads and the increments below no other
-- replica can slip a request in: that is the whole consistency argument.
--
-- KEYS[i]  counter HASH for window i (fields requests, tokens, cost_nanos)
-- ARGV[1..3]  the reservation: requests, tokens, cost_nanos
-- then three ARGV per window, in KEYS order:
--   expire_at (unix seconds), block (1|0), max_cost_nanos
--
-- All three fields are still counted: the durable counters carry them and
-- a future per-key rate limiter will want them live. Only cost is compared,
-- because a quota is a spend cap.
--
-- Replies (window indices are 1-based):
--   {'missing', i, ...}              uninitialised windows; nothing reserved
--   {'blocked', i, used, limit}      a block cap would be crossed; nothing reserved
--   {'ok', i, used, limit, ...}      reserved; any allow caps crossed follow

local amount = { tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3]) }
local dims = { 'requests', 'tokens', 'cost_nanos' }

-- A missing counter is not a zero counter: it may be a window Redis lost,
-- whose history is in Postgres. Reserving against it would hand the
-- tenant a fresh budget, so the caller seeds it first.
local missing = {}
for i = 1, #KEYS do
  if redis.call('EXISTS', KEYS[i]) == 0 then
    missing[#missing + 1] = i
  end
end
if #missing > 0 then
  return { 'missing', unpack(missing) }
end

local spent = {}
for i = 1, #KEYS do
  spent[i] = tonumber(redis.call('HGET', KEYS[i], 'cost_nanos')) or 0
end

-- All checks happen before any write, so a rejection leaves every
-- counter exactly as it was.
for i = 1, #KEYS do
  local base = 3 + (i - 1) * 3
  local limit = tonumber(ARGV[base + 3])
  if ARGV[base + 2] == '1' and spent[i] + amount[3] > limit then
    return { 'blocked', i, spent[i], limit }
  end
end

local soft = { 'ok' }
for i = 1, #KEYS do
  local base = 3 + (i - 1) * 3
  for d = 1, 3 do
    -- The original argument string, not the Lua number: Redis renders
    -- numbers with %.14g, and a large cost would reach HINCRBY in
    -- exponent notation and be refused.
    if amount[d] ~= 0 then
      redis.call('HINCRBY', KEYS[i], dims[d], ARGV[d])
    end
  end
  redis.call('EXPIREAT', KEYS[i], ARGV[base + 1])

  local limit = tonumber(ARGV[base + 3])
  if ARGV[base + 2] ~= '1' and spent[i] + amount[3] > limit then
    soft[#soft + 1] = i
    soft[#soft + 1] = spent[i]
    soft[#soft + 1] = limit
  end
end
return soft
