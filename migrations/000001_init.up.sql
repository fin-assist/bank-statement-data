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
--   first); the service deletes operations physically when the last upload containing them
--   goes (their source records follow; the code clears transfer_pair_id of the partner).
--   Only upload itself is deleted logically (status deleted).
-- * source_record.upload_ids is the one array of IDs in the schema (owner's decision in the
--   review of FIN-111): no foreign key, the code keeps it to the user's own uploads.
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
-- * Every table and every column has a COMMENT ON in Russian (owner's request in the review
--   of FIN-111): for a table what it holds, its key and its role, for a column its meaning.
--   Partitions of operation have none; the parent describes them.

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
COMMENT ON COLUMN user_state.user_id IS 'Идентификатор пользователя из сервиса user (sub токена).';
COMMENT ON COLUMN user_state.timezone IS
    'Часовой пояс профиля (IANA); Europe/Moscow, пока не пришёл первый SetUserTimezone.';
COMMENT ON COLUMN user_state.settings_version IS
    'Версия применённого SetUserTimezone; событие с меньшей или равной версией подтверждается и '
    'игнорируется.';
COMMENT ON COLUMN user_state.calc_version IS
    'Монотонна у пользователя: увеличивается в транзакции, которая пишет строку outbox UpsertUserMetrics.';
COMMENT ON COLUMN user_state.categories_changed_at IS
    'Последняя ручная смена категории, перекатегоризация или отметка «не подписка» '
    '(DataVersion.categories_changed_at).';
COMMENT ON COLUMN user_state.uploads_changed_at IS 'Последнее завершение или удаление загрузки (DataVersion.uploads_changed_at).';
COMMENT ON COLUMN user_state.daily_recalc_due_at IS
    'Ближайшие 00:05 в поясе пользователя, когда пересчитываются текущие периоды; NULL — данных ещё нет.';
COMMENT ON COLUMN user_state.created_at IS 'Когда строка создана; техническая отметка.';

CREATE INDEX user_state_daily_recalc_due_at_idx ON user_state (daily_recalc_due_at)
    WHERE daily_recalc_due_at IS NOT NULL;

-- ---------------------------------------------------------------- reference data

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
COMMENT ON COLUMN category.id IS 'Код категории (cat_…) — category_id в API.';
COMMENT ON COLUMN category.name IS 'Название категории для показа — category_name в API.';
COMMENT ON COLUMN category.bank_name IS
    'Категория банка, которую представляет строка: MEMO после обрезки краёв, обрезанный до 64 символов; у '
    'системных без соответствия — NULL.';
COMMENT ON COLUMN category.icon IS 'Ключ иконки — category_icon в API.';
COMMENT ON COLUMN category.is_system IS
    'Системная категория («Между своими счетами», «Возврат», «Без категории»); создаётся только '
    'миграцией.';
COMMENT ON COLUMN category.is_known IS
    'Категория из реестра известных категорий банка (FIN-6) или системная; false — категория банка, '
    'встреченная впервые.';
COMMENT ON COLUMN category.for_expense IS 'Категорию можно дать расходу (kinds в 5.1).';
COMMENT ON COLUMN category.for_income IS 'Категорию можно дать доходу (kinds в 5.1).';
COMMENT ON COLUMN category.assignable IS 'Пользователь может выбрать категорию вручную (4.3); у «Без категории» — нет.';
COMMENT ON COLUMN category.sort_order IS
    'Порядок показа системных категорий и категорий реестра; NULL — после них, по первому появлению у '
    'пользователя (user_bank_category).';
COMMENT ON COLUMN category.note IS 'Пояснение к системной категории («не в тратах»); у остальных NULL.';
COMMENT ON COLUMN category.created_at IS 'Когда строка создана.';
COMMENT ON COLUMN category.updated_at IS 'Когда строка последний раз изменена (синхронизация с реестром FIN-6).';

INSERT INTO category (id, name, bank_name, icon, is_system, is_known, for_expense, for_income, assignable, sort_order, note) VALUES
    ('cat_own_transfer',  'Между своими счетами', NULL,            'transfer', true, true, true,  true, true,  10, 'не в тратах'),
    ('cat_refund',        'Возврат',              NULL,            'repeat',   true, true, false, true, true,  20, 'уменьшает расходы на покупку'),
    ('cat_uncategorized', 'Без категории',        'Без категории', 'tag',      true, true, true,  true, false, 30, 'в выписке нет категории');

-- ---------------------------------------------------------------- accounts

CREATE TABLE account (
    user_id            uuid        NOT NULL REFERENCES user_state ON DELETE CASCADE,
    id                 bigint      GENERATED ALWAYS AS IDENTITY,
    source             text        NOT NULL,
    ext_id_hmac        bytea       NOT NULL,
    ext_id_key_version bigint      NOT NULL,
    type               text        NOT NULL,
    mask               text,
    is_other_bank      boolean     NOT NULL,
    currency           text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, id),
    UNIQUE (user_id, source, ext_id_hmac)
);

COMMENT ON TABLE account IS
    'Счета пользователя, встреченные в выписках; ключ (user_id, id), счёт источника однозначен по '
    '(user_id, source, ext_id_hmac). Полный номер (ACCTID) не хранится: только его HMAC с ключом сервиса, '
    'тип и последние 4 символа.';
COMMENT ON COLUMN account.user_id IS 'Пользователь (user_state).';
COMMENT ON COLUMN account.id IS 'Внутренний ID счёта; наружу не уходит.';
COMMENT ON COLUMN account.source IS
    'Источник, из которого пришёл счёт: код из констант сервиса (tbank_ofx), там же его название (source_name).';
COMMENT ON COLUMN account.ext_id_hmac IS
    'Хеш номера счёта — HMAC-SHA-256 с ключом сервиса по user_id и ACCTID (или ID счёта в источнике): '
    'один и тот же счёт двух пользователей даёт разные значения, строки разных пользователей не связать.';
COMMENT ON COLUMN account.ext_id_key_version IS
    'Версия ключа HMAC, которым посчитано значение; ротация переписывает строки на новый ключ.';
COMMENT ON COLUMN account.type IS 'Код типа счёта из источника (checking, savings, …); type_name строит сервис.';
COMMENT ON COLUMN account.mask IS 'Последние 4 символа номера; NULL у счёта другого банка (OB$…).';
COMMENT ON COLUMN account.is_other_bank IS
    'Счёт другого банка, подключённый в Т-Банке (ACCTID с префиксом OB$); маски у него нет.';
COMMENT ON COLUMN account.currency IS 'Валюта счёта (ISO 4217), если источник её сообщает; иначе NULL.';
COMMENT ON COLUMN account.created_at IS 'Когда счёт впервые встретился в выписке.';

-- ---------------------------------------------------------------- uploads

CREATE TABLE upload (
    user_id            uuid        NOT NULL REFERENCES user_state ON DELETE CASCADE,
    id                 uuid        NOT NULL DEFAULT uuidv7(),
    source             text        NOT NULL,
    file_name          text        NOT NULL,
    size_bytes         bigint      NOT NULL,
    content_sha256     bytea       NOT NULL,
    object_key         text        NOT NULL,
    file_deleted_at    timestamptz,
    status             text        NOT NULL DEFAULT 'processing',
    error_code         text,
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
COMMENT ON COLUMN upload.user_id IS 'Пользователь (user_state).';
COMMENT ON COLUMN upload.id IS 'Идентификатор загрузки (UUIDv7) — upload_id в API.';
COMMENT ON COLUMN upload.source IS
    'Источник файла (формат выписки): код из констант сервиса (tbank_ofx), там же его название (source_name).';
COMMENT ON COLUMN upload.file_name IS 'Имя файла, как его прислал клиент, до 255 символов (3.4).';
COMMENT ON COLUMN upload.size_bytes IS 'Размер файла в байтах.';
COMMENT ON COLUMN upload.content_sha256 IS
    'Хеш SHA-256 файла: хеш запроса для ключа идемпотентности (вместе с file_name) и подсказка поддержке. '
    'Не уникален: тот же файл можно загрузить снова (FIN-79).';
COMMENT ON COLUMN upload.object_key IS
    'Ключ файла в Object Storage, <user_id>/<id загрузки>: уникален у загрузки, а purge удаляет файлы '
    'пользователя по префиксу <user_id>/ (ключ строит сервис; docs/schema.md).';
COMMENT ON COLUMN upload.file_deleted_at IS
    'Когда файл удалён из Object Storage. failed/deleted с NULL здесь — файл ещё должно удалить задание.';
COMMENT ON COLUMN upload.status IS
    'Статус: processing -> done | failed (error_code); done -> deleted. 3.4 показывает processing и done.';
COMMENT ON COLUMN upload.error_code IS 'Только у failed: processing_failed | cancelled.';
COMMENT ON COLUMN upload.percent IS
    'Общий прогресс разбора 0–100 для события progress WatchUpload; обновляется по ходу разбора. Этап '
    '(stage) не хранится: код выводит его из percent (docs/schema.md).';
COMMENT ON COLUMN upload.uploaded_at IS
    'Создание загрузки; упорядочивает загрузки при закрытии непрочитанных строк (при равенстве — по id) и '
    'в 3.4.';
COMMENT ON COLUMN upload.finished_at IS 'Когда загрузка вышла из processing (done или failed); NULL — ещё разбирается.';
COMMENT ON COLUMN upload.deleted_at IS 'Когда загрузка удалена (3.5); задан только у deleted.';
COMMENT ON COLUMN upload.operation_count IS 'Операций прочитано из файла, включая уже загруженные ранее.';
COMMENT ON COLUMN upload.first_operation_at IS
    'Первая операция файла (период в 3.4); задана у done (и остаётся у deleted), NULL — в файле нет операций '
    'или разбор не завершён.';
COMMENT ON COLUMN upload.last_operation_at IS
    'Последняя операция файла (период в 3.4); задана у done (и остаётся у deleted), NULL — в файле нет '
    'операций или разбор не завершён.';
COMMENT ON COLUMN upload.result IS
    'Итог импорта (ImportResult) события complete (protojson), который повторяет WatchUpload; только у '
    'done.';

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
COMMENT ON COLUMN upload_account.user_id IS 'Пользователь (user_state).';
COMMENT ON COLUMN upload_account.upload_id IS 'Загрузка.';
COMMENT ON COLUMN upload_account.account_id IS 'Счёт блока STMTTRNRS в файле загрузки.';
COMMENT ON COLUMN upload_account.statement_start IS 'Начало периода выписки — DTSTART блока как в файле; NULL — тега нет.';
COMMENT ON COLUMN upload_account.statement_end IS 'Конец периода выписки — DTEND блока как в файле; NULL — тега нет.';

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
    transfer_pair_id          uuid,
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, occurred_at, id),
    FOREIGN KEY (user_id, account_id) REFERENCES account ON DELETE CASCADE
) PARTITION BY RANGE (occurred_at);

COMMENT ON TABLE operation IS
    'Канонические операции пользователя, ключ (user_id, occurred_at, id); секционирована по годам occurred_at '
    '(границы в UTC), у партиций operation_yYYYY своих описаний нет. Столбцы правил (category_id, '
    'refund_target_category_id, is_split, transfer_pair_id) пересчитываются после каждого импорта, удаления и '
    'ручного изменения.';
COMMENT ON COLUMN operation.user_id IS 'Пользователь (user_state).';
COMMENT ON COLUMN operation.occurred_at IS 'Время операции (OFX DTPOSTED); ключ партиции, сортировки и месяца.';
COMMENT ON COLUMN operation.id IS 'Идентификатор операции (UUIDv7) — id в API.';
COMMENT ON COLUMN operation.account_id IS 'Счёт операции.';
COMMENT ON COLUMN operation.direction IS 'Направление: expense | income.';
COMMENT ON COLUMN operation.amount_minor IS 'Сумма без знака в минорных единицах; направление — в direction.';
COMMENT ON COLUMN operation.currency IS 'Валюта операции (ISO 4217).';
COMMENT ON COLUMN operation.status IS 'Статус: posted | pending.';
COMMENT ON COLUMN operation.description IS 'Описание из источника как есть; может быть пустым (FIN-67).';
COMMENT ON COLUMN operation.bank_category_id IS
    'Категория из источника (MEMO) как ID справочника; пустой MEMO — cat_uncategorized.';
COMMENT ON COLUMN operation.posted_at IS 'Время проведения банком; NULL — источник не сообщает.';
COMMENT ON COLUMN operation.mcc IS 'Код MCC торговой точки, если источник его сообщает (в OFX Т-Банка нет).';
COMMENT ON COLUMN operation.extensions IS
    'Поля, которые есть только у одного источника (extensions.<source>), схема — по версии адаптера.';
COMMENT ON COLUMN operation.category_id IS 'Итоговая категория (docs/calc.md 3.5).';
COMMENT ON COLUMN operation.manual_category_id IS
    'Категория, выбранная пользователем (4.3); переживает повторный импорт и пересчёт.';
COMMENT ON COLUMN operation.refund_target_category_id IS 'У возврата (категория cat_refund): категория, расходы которой он уменьшает.';
COMMENT ON COLUMN operation.is_split IS
    'Пользователь разделил пару перевода, в которой была операция (docs/calc.md 3.2, 3.4).';
COMMENT ON COLUMN operation.transfer_pair_id IS
    'Пара перевода между своими счетами (docs/calc.md 3.2): одно значение (UUIDv7) у обеих сторон, оно же ID '
    'записи own_transfer в API; время и сумма записи — у стороны списания. NULL — операция не в паре. Что '
    'сторон ровно две, это расход и доход и при потере одной у другой значение очищается, держит код '
    '(docs/schema.md).';
COMMENT ON COLUMN operation.created_at IS 'Когда операция создана импортом.';
COMMENT ON COLUMN operation.updated_at IS 'Когда строка последний раз изменена (правила, ручная категория).';

-- GetOperation and ChangeCategory by ID: one probe per yearly partition.
CREATE INDEX operation_user_id_id_idx ON operation (user_id, id);
-- GetOperation and ChangeCategory by the ID of an own_transfer record: both sides of the pair.
CREATE INDEX operation_transfer_pair_idx ON operation (user_id, transfer_pair_id)
    WHERE transfer_pair_id IS NOT NULL;

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
    upload_ids            uuid[]      NOT NULL DEFAULT '{}',
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
    'ссылаться несколько записей (несколько источников). upload_ids — загрузки, в файлах которых есть '
    'запись: операция живёт, пока хотя бы у одной её записи массив не пуст (docs/calc.md 2, api.md 3.5).';
COMMENT ON COLUMN source_record.user_id IS 'Пользователь (user_state).';
COMMENT ON COLUMN source_record.id IS 'Внутренний ID записи; наружу не уходит.';
COMMENT ON COLUMN source_record.account_id IS 'Счёт, в рамках которого уникален external_id.';
COMMENT ON COLUMN source_record.external_id IS 'Идентификатор операции в источнике (OFX FITID), непрозрачный.';
COMMENT ON COLUMN source_record.operation_occurred_at IS
    'Время канонической операции (её occurred_at) — часть её ключа (партиция); следует за операцией через '
    'ON UPDATE CASCADE.';
COMMENT ON COLUMN source_record.operation_id IS 'Каноническая операция, которую представляет запись.';
COMMENT ON COLUMN source_record.upload_ids IS
    'Загрузки этого пользователя, в файлах которых есть запись, без повторов. Удаление загрузки убирает её '
    'из массива; запись с пустым массивом удаляется вместе с операцией, если у той не осталось других '
    'записей. По FIN-79 строку по FITID закрывает только загрузка из этого массива. FK нет: '
    'принадлежность загрузок пользователю проверяет код (docs/schema.md).';
COMMENT ON COLUMN source_record.created_at IS 'Когда запись впервые встретилась в загрузке.';

-- Cascade from operation and the lookup of an operation's records.
CREATE INDEX source_record_operation_idx ON source_record (user_id, operation_id);
-- No index on upload_ids: deleting an upload filters the user's records (the PK prefix
-- user_id, thousands of rows) by upload_ids @> ARRAY[$upload]; FIN-79 reads one record by
-- its unique key and checks $upload = ANY(upload_ids). docs/schema.md, «Ожидаемые запросы».

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

COMMENT ON TABLE unread_line IS
    'Строки файла, которые не удалось учесть (3.6), ключ (user_id, upload_id, line_number). Содержимое не '
    'хранится: только номер строки, дата, причина и, если прочитаны, счёт и FITID, по которым строку '
    'закрывает более поздняя загрузка.';
COMMENT ON COLUMN unread_line.user_id IS 'Пользователь (user_state).';
COMMENT ON COLUMN unread_line.upload_id IS 'Загрузка, в файле которой строка.';
COMMENT ON COLUMN unread_line.line_number IS 'Номер строки тега <STMTTRN> в файле, с 1.';
COMMENT ON COLUMN unread_line.reason IS 'Причина: bad_amount | bad_date | no_date | truncated.';
COMMENT ON COLUMN unread_line.occurred_at IS 'Дата операции, если прочиталась.';
COMMENT ON COLUMN unread_line.account_id IS 'Счёт блока, если строка в блоке со счётом.';
COMMENT ON COLUMN unread_line.external_id IS
    'Идентификатор строки (FITID), если есть: по (счёт, FITID) строку закрывает более поздняя загрузка.';
COMMENT ON COLUMN unread_line.resolved_by_upload_id IS
    'Более поздняя загрузка, закрывшая строку (FIN-79); NULL — строка открыта. При удалении той загрузки '
    'очищается и проверяется заново.';
COMMENT ON COLUMN unread_line.resolution IS
    'Как закрыта: read — более поздняя загрузка её прочитала; replaced — там она снова не прочиталась; '
    'задаётся вместе с resolved_by_upload_id.';

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
COMMENT ON COLUMN user_bank_category.user_id IS 'Пользователь (user_state).';
COMMENT ON COLUMN user_bank_category.category_id IS 'Категория банка (строка справочника category).';
COMMENT ON COLUMN user_bank_category.first_uploaded_at IS 'Время загрузки (uploaded_at), в которой категория встретилась впервые.';
COMMENT ON COLUMN user_bank_category.first_line_number IS
    'Номер строки первого появления в том файле; вместе с first_uploaded_at задаёт порядок в 5.1.';

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
COMMENT ON COLUMN regular_payment.user_id IS 'Пользователь (user_state).';
COMMENT ON COLUMN regular_payment.id IS 'Стабильный ID платежа (6.3); из описания не выводится.';
COMMENT ON COLUMN regular_payment.periodicity IS 'Периодичность: monthly (другие позже, FIN-127).';
COMMENT ON COLUMN regular_payment.description IS 'Описание операций платежа (точное совпадение) — часть ключа платежа.';
COMMENT ON COLUMN regular_payment.dismissed_at IS 'Отмечен «не подписка» (DismissRegularPayment); NULL — показывается.';
COMMENT ON COLUMN regular_payment.created_at IS 'Когда платёж впервые распознан.';

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
COMMENT ON COLUMN merchant.user_id IS 'Пользователь (user_state).';
COMMENT ON COLUMN merchant.id IS 'Идентификатор продавца в токене выборки s_merchant_<id>.';
COMMENT ON COLUMN merchant.key IS 'Нормализованное описание операций — продавец; уникально у пользователя.';
COMMENT ON COLUMN merchant.created_at IS 'Когда продавец впервые понадобился для токена.';

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
    'как аренду), считает месяц из операций и периодов загрузок вне этой транзакции, затем одной '
    'транзакцией увеличивает calc_version, добавляет строку outbox UpsertUserMetrics и удаляет строку '
    'очереди, только если generation не изменился; изменившаяся строка остаётся и будет взята снова. '
    'Посчитанные суммы здесь не хранятся: готовые метрики хранит analytics.';
COMMENT ON COLUMN recalc_queue.user_id IS 'Пользователь (user_state).';
COMMENT ON COLUMN recalc_queue.month IS 'Первое число месяца в поясе пользователя.';
COMMENT ON COLUMN recalc_queue.generation IS
    'Счётчик пометок месяца; строка удаляется после пересчёта, только если он не изменился.';
COMMENT ON COLUMN recalc_queue.enqueued_at IS 'Когда месяц помечен впервые.';
COMMENT ON COLUMN recalc_queue.next_attempt_at IS
    'С какого момента строку можно взять: аренда обработчика и отсрочка повтора после ошибки.';
COMMENT ON COLUMN recalc_queue.attempts IS 'Неудачных попыток пересчёта.';
COMMENT ON COLUMN recalc_queue.last_error IS 'Последняя ошибка пересчёта для диагностики; NULL — ошибок не было.';

CREATE INDEX recalc_queue_next_attempt_at_idx ON recalc_queue (next_attempt_at);

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
COMMENT ON COLUMN metric_definition.metric_id IS 'Идентификатор метрики или правила из реестра (FIN-6).';
COMMENT ON COLUMN metric_definition.version IS 'Версия определения, с 1.';
COMMENT ON COLUMN metric_definition.params IS 'Пороги и параметры версии (JSON-объект).';
COMMENT ON COLUMN metric_definition.is_current IS 'Текущая версия метрики; у метрики она одна (metric_definition_current_idx).';
COMMENT ON COLUMN metric_definition.created_at IS 'Когда версия добавлена.';

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
COMMENT ON COLUMN user_metric_version.user_id IS 'Пользователь (user_state).';
COMMENT ON COLUMN user_metric_version.metric_id IS 'Метрика.';
COMMENT ON COLUMN user_metric_version.version IS 'Версия определения, которой метрика посчитана последней.';
COMMENT ON COLUMN user_metric_version.computed_at IS 'Когда метрика посчитана.';

CREATE INDEX user_metric_version_metric_idx ON user_metric_version (metric_id, version);
