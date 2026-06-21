-- Move long_name into note (prepend) for all runs that have one.
-- The long_name field is being retired; its content is preserved in note.
UPDATE user_reaches
SET note = CASE
        WHEN long_name IS NOT NULL AND long_name != '' AND note IS NOT NULL AND note != ''
            THEN long_name || E'\n\n' || note
        WHEN long_name IS NOT NULL AND long_name != ''
            THEN long_name
        ELSE note
    END
WHERE long_name IS NOT NULL AND long_name != '';
