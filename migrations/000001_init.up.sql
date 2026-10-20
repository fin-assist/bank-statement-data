-- Schema of bank-statement-data (FIN-111). Rationale, alternatives and open questions are in
-- the PR; the meaning of every field is in fin-assist/contracts (docs/api.md 3-6,
-- docs/grpc-api.md 7.2-7.3, docs/calc.md).
--
-- Conventions:
-- * user_id is the first column of every primary and unique key of user data (sharding by
--   user later, Citus or hash sub-partitions). Every user table references user_state with
--   ON DELETE CASCADE, directly or through its parent: PurgeUserData is one
--   DELETE FROM user_state (plus the files in Object Storage).
-- * Foreign keys between user tables include user_id, so a row can never point at another
--   user's account, upload or operation.
-- * Foreign keys between user tables cascade. The purge deletes everything through them
--   (with NO ACTION the cascade from user_state would fail on whichever table it reaches
--   first); the service deletes operations physically when an upload goes (their source
--   records, upload membership and pairs follow) and month_state when a month is rebuilt
--   (its monthly_agg follows). Only upload itself is deleted logically (status deleted).
-- * IDs that leave the database (API, other services, support) are UUIDv7 (uuidv7() of
--   PostgreSQL 18 as the default; the service may pass its own). Internal IDs are bigint
--   identities.
-- * Only keys, NOT NULL and DEFAULT live here (owner's decision of 19.10.2026): no CHECK,
--   EXCLUDE, triggers or functions. Value sets, formats, status transitions and the other
--   rules are checked by the service code (docs/schema.md, «Инварианты в коде сервиса»).
--   Foreign key cascades and partitioning stay.
-- * Codes are plain text (readable in psql); the allowed values are listed in the comments.
-- * Money is bigint minor units; operation amounts are unsigned, the direction is a column.
-- * Instants are timestamptz. Calendar months are date (the 1st) in the user's time zone at
--   calculation time; the zone they were computed in is stored next to them.
-- * Every table has a COMMENT ON TABLE in Russian (owner's request in the review of FIN-111):
--   what it holds, its key and its role. Partitions of operation have none; the parent
--   describes them. Column comments stay in English.

-- ---------------------------------------------------------------- user

CREATE TABLE user_state (
    user_id               uuid        PRIMARY KEY,
    timezone              text        NOT NULL DEFAULT 'Europe/Moscow',
    settings_version      bigint      NOT NULL DEFAULT 0,
    calc_version          bigint      NOT NULL DEFAULT 0,
    categories_changed_at timestamptz,
    uploads_changed_at    timestamptz,
    daily_recalc_due_at   timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE user_state IS
    'Пользователь сервиса, одна строка на user_id (ключ): настройки из user (SetUserTimezone), версии и '
    'отметки изменений. На неё ссылается каждая пользовательская таблица (ON DELETE CASCADE), PurgeUserData — '
    'один DELETE этой строки. Шаги одного пользователя (закрытие непрочитанных строк, calc_version) '
    'сериализуются блокировкой SELECT ... FOR NO KEY UPDATE: FOR UPDATE ждал бы каждую открытую транзакцию, '
    'вставившую ссылающиеся строки (их FK берёт FOR KEY SHARE), и два импорта взаимно блокировались бы.';
COMMENT ON COLUMN user_state.timezone IS
    'IANA zone of the profile; Europe/Moscow until the first SetUserTimezone arrives.';
COMMENT ON COLUMN user_state.settings_version IS
    'Version of the applied SetUserTimezone; a smaller or equal one is acknowledged and ignored.';
COMMENT ON COLUMN user_state.calc_version IS
    'Monotonic per user: incremented in the transaction that writes an UpsertUserMetrics outbox row.';
COMMENT ON COLUMN user_state.categories_changed_at IS
    'DataVersion.categories_changed_at: last manual category change, re-categorisation or dismissal.';
COMMENT ON COLUMN user_state.uploads_changed_at IS
    'DataVersion.uploads_changed_at: last upload finished or deleted.';
COMMENT ON COLUMN user_state.daily_recalc_due_at IS
    'Next 00:05 in the user''s zone when the current periods are recalculated; NULL - no data yet.';

CREATE INDEX user_state_daily_recalc_due_at_idx ON user_state (daily_recalc_due_at)
    WHERE daily_recalc_due_at IS NOT NULL;

-- ---------------------------------------------------------------- reference data

CREATE TABLE source (
    id   text PRIMARY KEY,
    name text NOT NULL
);

COMMENT ON TABLE source IS
    'Справочник источников данных (форматы выписок, API банков), ключ — код источника; name — source_name в API.';

INSERT INTO source (id, name) VALUES ('tbank_ofx', 'Выписка Т-Банка');

CREATE TABLE category (
    id          text        PRIMARY KEY,
    name        text        NOT NULL,
    bank_name   text        UNIQUE,
    icon        text        NOT NULL,
    is_system   boolean     NOT NULL DEFAULT false,
    is_known    boolean     NOT NULL DEFAULT false,
    for_expense boolean     NOT NULL,
    for_income  boolean     NOT NULL,
    assignable  boolean     NOT NULL,
    sort_order  bigint      UNIQUE,
    note        text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE category IS
    'Общий для всех пользователей справочник категорий (5.1), ключ — код категории: системные категории, '
    'реестр известных категорий банка (FIN-6) и впервые встреченные категории банка (is_known = false). '
    'Не персональные данные: названия берутся из MEMO — это категория банка.';
COMMENT ON COLUMN category.bank_name IS
    'MEMO after trimming, cut to 64 characters: the bank category this row stands for.';
COMMENT ON COLUMN category.sort_order IS
    'Display order of system and registry categories; NULL - after them, by first appearance for '
    'the user (user_bank_category).';

INSERT INTO category (id, name, bank_name, icon, is_system, is_known, for_expense, for_income, assignable, sort_order, note) VALUES
    ('cat_own_transfer',  'Между своими счетами', NULL,            'transfer', true, true, true,  true, true,  10, 'не в тратах'),
    ('cat_refund',        'Возврат',              NULL,            'repeat',   true, true, false, true, true,  20, 'уменьшает расходы на покупку'),
    ('cat_uncategorized', 'Без категории',        'Без категории', 'tag',      true, true, true,  true, false, 30, 'в выписке нет категории');

-- ---------------------------------------------------------------- accounts

CREATE TABLE account (
    user_id            uuid        NOT NULL REFERENCES user_state ON DELETE CASCADE,
    id                 bigint      GENERATED ALWAYS AS IDENTITY,
    source_id          text        NOT NULL REFERENCES source,
    ext_id_hmac        bytea       NOT NULL,
    ext_id_key_version bigint      NOT NULL,
    type               text        NOT NULL,
    mask               text,
    is_other_bank      boolean     NOT NULL,
    currency           text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, id),
    UNIQUE (user_id, source_id, ext_id_hmac)
);

COMMENT ON TABLE account IS
    'Счета пользователя, встреченные в выписках; ключ (user_id, id), счёт источника однозначен по '
    '(user_id, source_id, ext_id_hmac). Полный номер (ACCTID) не хранится: только его HMAC с ключом сервиса, '
    'тип и последние 4 символа.';
COMMENT ON COLUMN account.ext_id_hmac IS
    'HMAC-SHA-256 with the service key over user_id and ACCTID (or the source''s account ID): the '
    'same account of two users gives different values, so rows cannot be linked across users.';
COMMENT ON COLUMN account.ext_id_key_version IS
    'Version of the HMAC key the value was computed with; rotation rewrites the rows to the new key.';
COMMENT ON COLUMN account.type IS 'Account type code from the source (checking, savings, ...); type_name is built by the service.';
COMMENT ON COLUMN account.mask IS 'Last 4 characters of the number; NULL for an account of another bank (OB$...).';

-- ---------------------------------------------------------------- uploads

CREATE TABLE upload (
    user_id            uuid        NOT NULL REFERENCES user_state ON DELETE CASCADE,
    id                 uuid        NOT NULL DEFAULT uuidv7(),
    source_id          text        NOT NULL REFERENCES source,
    file_name          text        NOT NULL,
    size_bytes         bigint      NOT NULL,
    content_sha256     bytea       NOT NULL,
    object_key         text        NOT NULL,
    file_deleted_at    timestamptz,
    status             text        NOT NULL DEFAULT 'processing',
    error_code         text,
    stage              text,
    percent            bigint      NOT NULL DEFAULT 0,
    uploaded_at        timestamptz NOT NULL DEFAULT now(),
    finished_at        timestamptz,
    deleted_at         timestamptz,
    operation_count    bigint,
    first_operation_at timestamptz,
    last_operation_at  timestamptz,
    result             jsonb,
    PRIMARY KEY (user_id, id),
    -- One file per upload: deleting the file of one upload never removes another's, even when
    -- the same content is uploaded again.
    UNIQUE (user_id, object_key)
);

COMMENT ON TABLE upload IS
    'Загрузки выписок (3.2-3.5), ключ (user_id, id). Удалённая или неудачная загрузка сохраняет строку: '
    'DELETE существовавшей загрузки отвечает OK, NOT_FOUND — только для ID, которого у пользователя не было; '
    'WatchUpload повторяет итог неудачной загрузки.';
COMMENT ON COLUMN upload.content_sha256 IS
    'SHA-256 of the file: the request hash of the idempotency key (with file_name) and a hint for '
    'support. Not unique: the same file may be uploaded again (FIN-79).';
COMMENT ON COLUMN upload.object_key IS
    'Object Storage key of the file, <user_id>/<upload id>: unique per upload, and the purge deletes '
    'the user''s files by the <user_id>/ prefix (the service builds the key; docs/schema.md).';
COMMENT ON COLUMN upload.file_deleted_at IS
    'When the file was removed from Object Storage. failed/deleted with NULL here - a job still has to remove it.';
COMMENT ON COLUMN upload.status IS
    'processing -> done | failed (error_code); done -> deleted. 3.4 shows processing and done.';
COMMENT ON COLUMN upload.error_code IS 'Only for failed: processing_failed | cancelled.';
COMMENT ON COLUMN upload.stage IS
    'Current stage while processing (parsing | dedup | rules), for the first WatchUpload event.';
COMMENT ON COLUMN upload.uploaded_at IS
    'Creation of the upload; orders uploads for closing unread lines (ties - by id) and for 3.4.';
COMMENT ON COLUMN upload.operation_count IS 'Operations read from the file, including ones already loaded.';
COMMENT ON COLUMN upload.result IS
    'ImportResult of the complete event (protojson), replayed by WatchUpload; only while done.';

-- Timeout sweep: uploads processing for more than 3 minutes, across users.
CREATE INDEX upload_processing_idx ON upload (uploaded_at) WHERE status = 'processing';
-- Files still to remove from Object Storage.
CREATE INDEX upload_file_pending_idx ON upload (finished_at)
    WHERE status IN ('failed', 'deleted') AND file_deleted_at IS NULL;

CREATE TABLE upload_account (
    user_id         uuid        NOT NULL,
    upload_id       uuid        NOT NULL,
    account_id      bigint      NOT NULL,
    statement_start timestamptz,
    statement_end   timestamptz,
    PRIMARY KEY (user_id, upload_id, account_id),
    FOREIGN KEY (user_id, upload_id) REFERENCES upload ON DELETE CASCADE,
    FOREIGN KEY (user_id, account_id) REFERENCES account ON DELETE CASCADE
);

COMMENT ON TABLE upload_account IS
    'Счета загрузки с периодом выписки DTSTART..DTEND как в файле, ключ (user_id, upload_id, account_id). '
    'По нему в зоне профиля на момент проверки считаются покрытые дни, пропуски и закрытие непрочитанных строк.';

-- ---------------------------------------------------------------- operations

CREATE TABLE operation (
    user_id                   uuid        NOT NULL REFERENCES user_state ON DELETE CASCADE,
    occurred_at               timestamptz NOT NULL,
    id                        uuid        NOT NULL DEFAULT uuidv7(),
    account_id                bigint      NOT NULL,
    direction                 text        NOT NULL,
    amount_minor              bigint      NOT NULL,
    currency                  text        NOT NULL,
    status                    text        NOT NULL,
    description               text        NOT NULL DEFAULT '',
    bank_category_id          text        NOT NULL REFERENCES category,
    posted_at                 timestamptz,
    mcc                       text,
    extensions                jsonb,
    category_id               text        NOT NULL REFERENCES category,
    manual_category_id        text        REFERENCES category,
    refund_target_category_id text        REFERENCES category,
    is_split                  boolean     NOT NULL DEFAULT false,
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, occurred_at, id),
    FOREIGN KEY (user_id, account_id) REFERENCES account ON DELETE CASCADE
) PARTITION BY RANGE (occurred_at);

COMMENT ON TABLE operation IS
    'Канонические операции пользователя, ключ (user_id, occurred_at, id); секционирована по годам occurred_at '
    '(границы в UTC), у партиций operation_yYYYY своих описаний нет. Столбцы правил (category_id, '
    'refund_target_category_id, is_split) пересчитываются после каждого импорта, удаления и ручного изменения.';
COMMENT ON COLUMN operation.direction IS 'expense | income.';
COMMENT ON COLUMN operation.status IS 'posted | pending.';
COMMENT ON COLUMN operation.occurred_at IS 'Time of the operation (OFX DTPOSTED); partition, sort and month key.';
COMMENT ON COLUMN operation.description IS 'Description from the source as is; may be empty (FIN-67).';
COMMENT ON COLUMN operation.bank_category_id IS 'Category from the source (MEMO) as a dictionary ID; empty MEMO - cat_uncategorized.';
COMMENT ON COLUMN operation.extensions IS 'Fields only one source has (extensions.<source>), schema per adapter version.';
COMMENT ON COLUMN operation.category_id IS 'Final category (docs/calc.md 3.5).';
COMMENT ON COLUMN operation.manual_category_id IS 'Category chosen by the user (4.3); survives re-import and recalculation.';
COMMENT ON COLUMN operation.refund_target_category_id IS 'For a refund (category cat_refund): the category whose expenses it reduces.';
COMMENT ON COLUMN operation.is_split IS 'The user split the transfer pair this operation was in (docs/calc.md 3.2, 3.4).';

-- GetOperation and ChangeCategory by ID: one probe per yearly partition.
CREATE INDEX operation_user_id_id_idx ON operation (user_id, id);

-- Yearly partitions with UTC bounds, 2010-2027: statements reach years back, and a partition
-- must exist before an import writes into it. Later years: the partition job
-- (docs/migrations.md, «Партиции»). An operation outside every partition fails the import.
DO $$
BEGIN
    FOR y IN 2010..2027 LOOP
        EXECUTE format(
            'CREATE TABLE operation_y%s PARTITION OF operation FOR VALUES FROM (%L) TO (%L)',
            y, make_timestamptz(y, 1, 1, 0, 0, 0, 'UTC'), make_timestamptz(y + 1, 1, 1, 0, 0, 0, 'UTC'));
    END LOOP;
END $$;

CREATE TABLE source_record (
    user_id               uuid        NOT NULL REFERENCES user_state ON DELETE CASCADE,
    id                    bigint      GENERATED ALWAYS AS IDENTITY,
    account_id            bigint      NOT NULL,
    external_id           text        NOT NULL,
    operation_occurred_at timestamptz NOT NULL,
    operation_id          uuid        NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, id),
    UNIQUE (user_id, account_id, external_id),
    FOREIGN KEY (user_id, account_id) REFERENCES account ON DELETE CASCADE,
    FOREIGN KEY (user_id, operation_occurred_at, operation_id) REFERENCES operation
        ON DELETE CASCADE ON UPDATE CASCADE
);

COMMENT ON TABLE source_record IS
    'Реестр дедупликации, без секционирования: одна строка на (счёт, внешний ID) источника, уникальна по '
    '(user_id, account_id, external_id), и ссылка на её каноническую операцию. На одну операцию могут '
    'ссылаться несколько записей (несколько источников).';
COMMENT ON COLUMN source_record.external_id IS 'ID of the operation in the source (OFX FITID), opaque.';

-- Cascade from operation and the lookup of an operation's records.
CREATE INDEX source_record_operation_idx ON source_record (user_id, operation_id);

CREATE TABLE upload_record (
    user_id          uuid   NOT NULL,
    upload_id        uuid   NOT NULL,
    source_record_id bigint NOT NULL,
    PRIMARY KEY (user_id, upload_id, source_record_id),
    FOREIGN KEY (user_id, upload_id) REFERENCES upload ON DELETE CASCADE,
    FOREIGN KEY (user_id, source_record_id) REFERENCES source_record ON DELETE CASCADE
);

COMMENT ON TABLE upload_record IS
    'Какие загрузки содержат запись источника, ключ (user_id, upload_id, source_record_id). Операция живёт, '
    'пока её запись есть хотя бы в одной загрузке (docs/calc.md 2); FIN-79 закрывает строку по FITID только '
    'через загрузку, в которой она есть.';

-- "Is the record in another upload?" when an upload is deleted; cascade from source_record.
CREATE INDEX upload_record_source_record_idx ON upload_record (user_id, source_record_id);

CREATE TABLE transfer_pair_member (
    user_id               uuid        NOT NULL REFERENCES user_state ON DELETE CASCADE,
    operation_id          uuid        NOT NULL,
    operation_occurred_at timestamptz NOT NULL,
    pair_id               uuid        NOT NULL,
    -- An operation is in at most one pair, whichever side it takes (docs/calc.md 3.2).
    PRIMARY KEY (user_id, operation_id),
    -- Target of the side keys of transfer_pair.
    UNIQUE (user_id, pair_id, operation_id),
    FOREIGN KEY (user_id, operation_occurred_at, operation_id) REFERENCES operation
        ON DELETE CASCADE ON UPDATE CASCADE
);

COMMENT ON TABLE transfer_pair_member IS
    'Операции — стороны пары перевода между своими счетами. Ключ (user_id, operation_id) не даёт операции '
    'попасть больше чем в одну пару; удаление операции удаляет её строку здесь, а через transfer_pair — пару '
    'и вторую сторону.';

CREATE TABLE transfer_pair (
    user_id             uuid        NOT NULL REFERENCES user_state ON DELETE CASCADE,
    id                  uuid        NOT NULL DEFAULT uuidv7(),
    debit_operation_id  uuid        NOT NULL,
    credit_operation_id uuid        NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, id),
    -- Both sides must be members of this very pair. The members are inserted in the same
    -- transaction after the pair, hence DEFERRABLE INITIALLY DEFERRED; the cascade still fires
    -- at once: losing a member deletes the pair (and with it the other member).
    FOREIGN KEY (user_id, id, debit_operation_id) REFERENCES transfer_pair_member (user_id, pair_id, operation_id)
        ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (user_id, id, credit_operation_id) REFERENCES transfer_pair_member (user_id, pair_id, operation_id)
        ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED
);

ALTER TABLE transfer_pair_member ADD FOREIGN KEY (user_id, pair_id) REFERENCES transfer_pair ON DELETE CASCADE;

COMMENT ON TABLE transfer_pair IS
    'Пары переводов между своими счетами пользователя, ключ (user_id, id); пара показывается одной записью '
    'own_transfer с ID = id. Пара переживает следующие импорты, пока существуют обе стороны (docs/calc.md 3.2), '
    'потеря стороны удаляет пару. Что дебет — расход, кредит — доход и это разные операции, проверяет код '
    '(docs/schema.md).';

CREATE TABLE unread_line (
    user_id               uuid        NOT NULL,
    upload_id             uuid        NOT NULL,
    line_number           bigint      NOT NULL,
    reason                text        NOT NULL,
    occurred_at           timestamptz,
    account_id            bigint,
    external_id           text,
    resolved_by_upload_id uuid,
    resolution            text,
    PRIMARY KEY (user_id, upload_id, line_number),
    FOREIGN KEY (user_id, upload_id) REFERENCES upload ON DELETE CASCADE,
    FOREIGN KEY (user_id, resolved_by_upload_id) REFERENCES upload ON DELETE CASCADE,
    FOREIGN KEY (user_id, account_id) REFERENCES account ON DELETE CASCADE
);

COMMENT ON COLUMN unread_line.reason IS 'bad_amount | bad_date | no_date | truncated.';
COMMENT ON TABLE unread_line IS
    'Строки файла, которые не удалось учесть (3.6), ключ (user_id, upload_id, line_number). Содержимое не '
    'хранится: только номер строки, дата, причина и, если прочитаны, счёт и FITID, по которым строку '
    'закрывает более поздняя загрузка.';
COMMENT ON COLUMN unread_line.occurred_at IS 'Date of the operation, when it was read.';
COMMENT ON COLUMN unread_line.resolved_by_upload_id IS
    'The later upload that closed the line (FIN-79); NULL - open. Cleared and rechecked when that upload is deleted.';
COMMENT ON COLUMN unread_line.resolution IS 'read - the later upload read it; replaced - it failed there again; set together with resolved_by_upload_id.';

-- 3.6 by period and unread_lines_count of a month: open lines with a date.
CREATE INDEX unread_line_open_date_idx ON unread_line (user_id, occurred_at)
    WHERE resolved_by_upload_id IS NULL AND occurred_at IS NOT NULL;
-- Closing by (account, FITID) on import.
CREATE INDEX unread_line_external_id_idx ON unread_line (user_id, account_id, external_id)
    WHERE external_id IS NOT NULL;
-- Lines closed by an upload: recheck when it is deleted, resolved_unread_count of 3.4.
CREATE INDEX unread_line_resolved_by_idx ON unread_line (user_id, resolved_by_upload_id)
    WHERE resolved_by_upload_id IS NOT NULL;

-- ---------------------------------------------------------------- categories per user

CREATE TABLE user_bank_category (
    user_id             uuid        NOT NULL REFERENCES user_state ON DELETE CASCADE,
    category_id         text        NOT NULL REFERENCES category,
    first_uploaded_at   timestamptz NOT NULL,
    first_line_number   bigint      NOT NULL,
    PRIMARY KEY (user_id, category_id)
);

COMMENT ON TABLE user_bank_category IS
    'Категории банка, встреченные пользователем, с первым появлением; ключ (user_id, category_id). 5.1 выводит '
    'категории вне реестра в этом порядке (docs/calc.md 10). Строка остаётся после удаления загрузки.';

-- ---------------------------------------------------------------- regular payments, selections

CREATE TABLE regular_payment (
    user_id      uuid        NOT NULL REFERENCES user_state ON DELETE CASCADE,
    id           uuid        NOT NULL DEFAULT uuidv7(),
    periodicity  text        NOT NULL,
    description  text        NOT NULL,
    dismissed_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, id),
    UNIQUE (user_id, periodicity, description)
);

COMMENT ON TABLE regular_payment IS
    'Распознанные регулярные платежи: ключ платежа (описание + периодичность, уникален у пользователя) и его '
    'стабильный ID (6.3), ключ (user_id, id). Строка создаётся при первом распознавании и не удаляется, '
    'поэтому ID и отметка «не подписка» переживают пересчёты и повторные загрузки.';
COMMENT ON COLUMN regular_payment.periodicity IS 'monthly (others later, FIN-127).';
COMMENT ON COLUMN regular_payment.dismissed_at IS 'Marked «не подписка» (DismissRegularPayment); NULL - shown.';

CREATE TABLE merchant (
    user_id    uuid        NOT NULL REFERENCES user_state ON DELETE CASCADE,
    id         uuid        NOT NULL DEFAULT uuidv7(),
    key        text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, id),
    UNIQUE (user_id, key)
);

COMMENT ON TABLE merchant IS
    'Получатели платежей (нормализованные описания) за токенами выборки s_merchant_<id>; ключ (user_id, id), '
    'key уникален у пользователя. Токен не несёт описания (персональные данные в строке запроса) и действует, '
    'пока строка существует.';

-- ---------------------------------------------------------------- calculation

CREATE TABLE recalc_queue (
    user_id         uuid        NOT NULL REFERENCES user_state ON DELETE CASCADE,
    month           date        NOT NULL,
    generation      bigint      NOT NULL DEFAULT 1,
    enqueued_at     timestamptz NOT NULL DEFAULT now(),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    attempts        bigint      NOT NULL DEFAULT 0,
    last_error      text,
    PRIMARY KEY (user_id, month)
);

COMMENT ON TABLE recalc_queue IS
    'Очередь месяцев на пересчёт, ключ (user_id, month). Отметка: INSERT ... ON CONFLICT DO UPDATE SET '
    'generation = generation + 1. Обработчик забирает строки (FOR UPDATE SKIP LOCKED, сдвигая next_attempt_at '
    'как аренду), считает вне этой транзакции, затем одной транзакцией пишет month_state/monthly_agg, '
    'увеличивает calc_version, добавляет строку outbox и удаляет строку очереди, только если generation не '
    'изменился; изменившаяся строка остаётся и будет взята снова.';
COMMENT ON COLUMN recalc_queue.month IS 'First day of the month in the user''s zone.';

CREATE INDEX recalc_queue_next_attempt_at_idx ON recalc_queue (next_attempt_at);

CREATE TABLE month_state (
    user_id       uuid        NOT NULL REFERENCES user_state ON DELETE CASCADE,
    month         date        NOT NULL,
    time_zone     text        NOT NULL,
    covered_days  bigint      NOT NULL,
    days_in_month bigint      NOT NULL,
    data_from     timestamptz NOT NULL,
    data_to       timestamptz NOT NULL,
    calc_version  bigint      NOT NULL,
    calculated_at timestamptz NOT NULL,
    PRIMARY KEY (user_id, month)
);

COMMENT ON TABLE month_state IS
    'Месяцы пользователя хотя бы с одним покрытым днём, ключ (user_id, month). Нет строки — нет данных за '
    'месяц; строка без строк monthly_agg — месяц покрыт и суммы нулевые. Здесь «нет данных» отличается от 0.';
COMMENT ON COLUMN month_state.data_to IS 'DataBounds.data_to of the month (docs/api.md, «Формат»).';

CREATE TABLE monthly_agg (
    user_id      uuid   NOT NULL,
    month        date   NOT NULL,
    account_id   bigint NOT NULL,
    category_id  text   NOT NULL REFERENCES category,
    kind         text   NOT NULL,
    currency     text   NOT NULL,
    amount_minor bigint NOT NULL,
    op_count     bigint NOT NULL,
    PRIMARY KEY (user_id, month, account_id, category_id, kind, currency),
    FOREIGN KEY (user_id, month) REFERENCES month_state ON DELETE CASCADE,
    FOREIGN KEY (user_id, account_id) REFERENCES account ON DELETE CASCADE
);

COMMENT ON COLUMN monthly_agg.kind IS 'expense | income | refund.';
COMMENT ON TABLE monthly_agg IS
    'Фоновый кэш сумм по месяцу, счёту, итоговой категории и виду (ключ — эти столбцы с user_id и валютой), '
    'перестраивается целиком вместе с месяцем. Возврат учитывается видом refund под целевой категорией и '
    'уменьшает её расходы.';

CREATE TABLE metric_definition (
    metric_id  text        NOT NULL,
    version    bigint      NOT NULL,
    params     jsonb       NOT NULL DEFAULT '{}',
    is_current boolean     NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (metric_id, version)
);

COMMENT ON TABLE metric_definition IS
    'Реестр определений метрик и правил с версиями (FIN-6), ключ (metric_id, version): пороги и параметры '
    'docs/calc.md 7. Текущая версия у метрики одна.';

CREATE UNIQUE INDEX metric_definition_current_idx ON metric_definition (metric_id) WHERE is_current;

CREATE TABLE user_metric_version (
    user_id     uuid        NOT NULL REFERENCES user_state ON DELETE CASCADE,
    metric_id   text        NOT NULL,
    version     bigint      NOT NULL,
    computed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, metric_id),
    FOREIGN KEY (metric_id, version) REFERENCES metric_definition
);

COMMENT ON TABLE user_metric_version IS
    'Версия определения, по которой метрика пользователя посчитана последней; ключ (user_id, metric_id). '
    'Задание дозаполнения выбирает пользователей с версией ниже текущей; до пересчёта метрика недоступна, а не '
    'равна нулю.';

CREATE INDEX user_metric_version_metric_idx ON user_metric_version (metric_id, version);
