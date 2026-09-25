-- Apply one request's correction (actual minus reserved) to the windows
-- it was reserved in. A counter that has expired is left alone: HINCRBY
-- on it would recreate it with no expiry and a nonsense value, and its
-- window is over anyway.
--
-- KEYS[i]  counter HASH
-- ARGV[1..3]  deltas for requests, tokens, cost_nanos (may be negative)

local dims = { 'requests', 'tokens', 'cost_nanos' }
for i = 1, #KEYS do
  if redis.call('EXISTS', KEYS[i]) == 1 then
    for d = 1, 3 do
      if ARGV[d] ~= '0' then
        redis.call('HINCRBY', KEYS[i], dims[d], ARGV[d])
      end
    end
  end
end
return #KEYS
