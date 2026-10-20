-- Rollback of the initial schema: only for a failed first deployment, the tables are empty
-- (docs/migrations.md, «Откат»). Reverse order of dependencies.
DROP TABLE user_metric_version;
DROP TABLE metric_definition;
DROP TABLE monthly_agg;
DROP TABLE month_state;
DROP TABLE recalc_queue;
DROP TABLE merchant;
DROP TABLE regular_payment;
DROP TABLE user_bank_category;
DROP TABLE unread_line;
DROP TABLE transfer_pair, transfer_pair_member;
DROP TABLE upload_record;
DROP TABLE source_record;
DROP TABLE operation;
DROP TABLE upload_account;
DROP TABLE upload;
DROP TABLE account;
DROP TABLE category;
DROP TABLE user_state;
