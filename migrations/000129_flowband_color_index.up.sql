-- Migrate flow-band color keys (e.g. "green-3") to positional palette indices ("p<n>").
-- The web client now stores flow-band colors as an index into a fixed 8x5 grid so the
-- colors recolor per theme (issue web#252). Families, in order:
--   red(0) orange(1) yellow(2) green(3) blue(4) purple(5) pink(6) neutral(7)
-- index = family*5 + (level-1)  where level is 1..5  ->  "p<index>"
-- Idempotent: rows already in "p<n>" form are skipped.

UPDATE user_reach_flow_ranges
SET color = 'p' || (
      (CASE split_part(color, '-', 1)
         WHEN 'red'     THEN 0
         WHEN 'orange'  THEN 1
         WHEN 'yellow'  THEN 2
         WHEN 'green'   THEN 3
         WHEN 'blue'    THEN 4
         WHEN 'purple'  THEN 5
         WHEN 'pink'    THEN 6
         WHEN 'neutral' THEN 7
         ELSE 7
       END) * 5
      + GREATEST(0, LEAST(4, COALESCE(NULLIF(split_part(color, '-', 2), '')::int, 3) - 1))
    )::text
WHERE color IS NOT NULL AND color !~ '^p[0-9]+$';

UPDATE user_reaches
SET base_color = 'p' || (
      (CASE split_part(base_color, '-', 1)
         WHEN 'red'     THEN 0
         WHEN 'orange'  THEN 1
         WHEN 'yellow'  THEN 2
         WHEN 'green'   THEN 3
         WHEN 'blue'    THEN 4
         WHEN 'purple'  THEN 5
         WHEN 'pink'    THEN 6
         WHEN 'neutral' THEN 7
         ELSE 7
       END) * 5
      + GREATEST(0, LEAST(4, COALESCE(NULLIF(split_part(base_color, '-', 2), '')::int, 3) - 1))
    )::text
WHERE base_color IS NOT NULL AND base_color !~ '^p[0-9]+$';
