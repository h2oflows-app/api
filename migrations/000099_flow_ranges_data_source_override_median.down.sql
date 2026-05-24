ALTER TABLE flow_ranges
    DROP CONSTRAINT IF EXISTS flow_ranges_data_source_check,
    ADD CONSTRAINT flow_ranges_data_source_check
        CHECK (data_source IN ('manual','ai_seed','ai_web'));
