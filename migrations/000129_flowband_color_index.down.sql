-- Reverse: positional palette index ("p<n>") -> legacy color key ("<family>-<level>").
-- family = n / 5 (integer), level = (n % 5) + 1. Postgres arrays are 1-indexed.
-- Note: indices that map to pink (30-34) reverse to "pink-N"; pre-#252 clients did not
-- offer pink, but down migrations are emergency-only.

UPDATE user_reach_flow_ranges
SET color = (ARRAY['red','orange','yellow','green','blue','purple','pink','neutral'])
              [ (substring(color from 2)::int / 5) + 1 ]
            || '-' || ((substring(color from 2)::int % 5) + 1)::text
WHERE color ~ '^p[0-9]+$';

UPDATE user_reaches
SET base_color = (ARRAY['red','orange','yellow','green','blue','purple','pink','neutral'])
                   [ (substring(base_color from 2)::int / 5) + 1 ]
                 || '-' || ((substring(base_color from 2)::int % 5) + 1)::text
WHERE base_color ~ '^p[0-9]+$';
