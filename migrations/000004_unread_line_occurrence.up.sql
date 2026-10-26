-- Preserve physical XML line numbers while identifying each unread fact.
ALTER TABLE unread_line
    ADD COLUMN line_occurrence integer NOT NULL DEFAULT 1;
COMMENT ON COLUMN unread_line.line_occurrence IS
    'Порядковый номер непрочитанной операции на физической строке файла, начиная с единицы.';
