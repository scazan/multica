-- Extend issue.origin_type for issues mirrored from Asana tasks. origin_id
-- stores the asana_task_link id. Mirrors 366 (telegram_chat).
--
-- This only widens the allowed set. Recreate the CHECK as NOT VALID so the
-- ACCESS EXCLUSIVE lock is held briefly; migration 588 performs the table scan
-- under SHARE UPDATE EXCLUSIVE without blocking normal reads and writes.
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
    CHECK (origin_type IN ('autopilot', 'quick_create', 'lark_chat', 'slack_chat', 'agent_create', 'dingtalk_chat', 'wecom_chat', 'telegram_chat', 'asana'))
    NOT VALID;
