-- 0014_admin_auth.down.sql — drop the admin-auth schema. admin_sessions.user_id -> admin_users is
-- ON DELETE CASCADE, and api_tokens.created_by -> operator_keys is RESTRICT but api_tokens is
-- itself dropped, so ordering only needs sessions before users. IF EXISTS keeps down re-runnable.
DROP TABLE IF EXISTS admin_audit;
DROP TABLE IF EXISTS admin_sessions;
DROP TABLE IF EXISTS admin_users;
DROP TABLE IF EXISTS api_tokens;
