-- Restore Kiro in platform constraints after later platform migrations.
--
-- Kiro's original constraint migrations (145/227/229) sort before
-- 245_openai_bps_platform.sql and 247_add_typesafe_platform.sql. Those later
-- migrations DROP and recreate the constraints with their own snapshots, which
-- would silently remove Kiro on both fresh installs and upgrades.
--
-- Keep this migration as the final constraint owner for the current platform set.
-- Channel monitor providers intentionally excludes typesafe (not a chat provider).

ALTER TABLE user_platform_quotas
    DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;

ALTER TABLE user_platform_quotas
    ADD CONSTRAINT user_platform_quotas_platform_check
    CHECK (platform IN ('anthropic', 'openai', 'openai_bps', 'gemini', 'antigravity',
                        'kiro', 'grok', 'kimi', 'zhipu', 'deepseek', 'minimax',
                        'opencode_go', 'typesafe'));

ALTER TABLE composite_model_routes
    DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;

ALTER TABLE composite_model_routes
    ADD CONSTRAINT composite_model_routes_target_platform_check
    CHECK (target_platform IN ('anthropic', 'openai', 'openai_bps', 'gemini', 'antigravity',
                               'kiro', 'grok', 'kimi', 'zhipu', 'deepseek', 'minimax',
                               'opencode_go', 'typesafe'));

ALTER TABLE channel_monitors
    DROP CONSTRAINT IF EXISTS channel_monitors_provider_check;

ALTER TABLE channel_monitors
    ADD CONSTRAINT channel_monitors_provider_check
    CHECK (provider IN ('openai', 'openai_bps', 'anthropic', 'gemini', 'grok', 'antigravity',
                        'kiro', 'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go'));

ALTER TABLE channel_monitor_request_templates
    DROP CONSTRAINT IF EXISTS channel_monitor_request_templates_provider_check;

ALTER TABLE channel_monitor_request_templates
    ADD CONSTRAINT channel_monitor_request_templates_provider_check
    CHECK (provider IN ('openai', 'openai_bps', 'anthropic', 'gemini', 'grok', 'antigravity',
                        'kiro', 'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go'));
