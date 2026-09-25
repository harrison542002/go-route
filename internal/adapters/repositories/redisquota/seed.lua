-- Raise counters to the durable snapshot, field by field: each field
-- becomes max(current, snapshot) and is never lowered. Because max is
-- idempotent and order-independent, replicas seeding the same window at
-- once converge on one value instead of counting history twice, and a key
-- that survived a failover holding a stale replica's smaller values is
-- corrected rather than left to run out the window.
--
-- KEYS[i]  counter HASH
-- four ARGV per key: requests, tokens, cost_nanos, expire_at

local dims = { 'requests', 'tokens', 'cost_nanos' }

for i = 1, #KEYS do
  local base = (i - 1) * 4
  local cur = redis.call('HMGET', KEYS[i], dims[1], dims[2], dims[3])
  for d = 1, 3 do
    -- The original argument string, not the Lua number: Redis renders
    -- numbers with %.14g, and a large cost would be written back in
    -- exponent notation, which HINCRBY then refuses.
    if cur[d] == false or tonumber(cur[d]) < tonumber(ARGV[base + d]) then
      redis.call('HSET', KEYS[i], dims[d], ARGV[base + d])
    end
  end
  redis.call('EXPIREAT', KEYS[i], ARGV[base + 4])
end
return #KEYS
