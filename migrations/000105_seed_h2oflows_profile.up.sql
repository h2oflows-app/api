-- Seed curator handle so GET /users/h2oflows resolves for explore browse-user default.
-- Uses a fixed sentinel UUID; owner_id does not need to match a real auth user.
INSERT INTO user_profiles (owner_id, handle)
VALUES ('00000000-0000-0000-0000-000000000001', 'h2oflows')
ON CONFLICT DO NOTHING;
