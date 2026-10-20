-- Rollback of the initial schema: only for a failed first deployment, the tables are empty
-- (docs/migrations.md, «Откат»). Reverse order of dependencies.
DROP TABLE recalc_queue;
DROP TABLE merchant;
DROP TABLE regular_payment;
DROP TABLE unread_line;
DROP TABLE source_record;
DROP TABLE operation;
DROP TABLE upload;
DROP TABLE account;
DROP TABLE category;
DROP TABLE user_state;
