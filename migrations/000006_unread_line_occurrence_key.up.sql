-- FIN-342: the owner approved replacing physical-line uniqueness with per-fact
-- identity. The replacement index is already valid; this file atomically swaps keys.
-- squawk-ignore ban-drop-constraint
ALTER TABLE unread_line DROP CONSTRAINT unread_line_pkey;
ALTER TABLE unread_line
    ADD CONSTRAINT unread_line_pkey PRIMARY KEY USING INDEX unread_line_occurrence_key;
