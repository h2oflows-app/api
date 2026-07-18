-- #155: index api_key_hash for inbound API-key auth lookups (PK is owner_id).
CREATE INDEX IF NOT EXISTS idx_user_api_access_api_key_hash ON user_api_access(api_key_hash);
