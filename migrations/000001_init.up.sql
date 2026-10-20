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
-- * Arrays of IDs instead of link tables (owner's decisions in the review of FIN-111):
--   source_record.upload_ids and upload.account_ids. No foreign keys on their elements: the
--   code keeps them to the user's own uploads and accounts (docs/schema.md).
-- * IDs that leave the database (API, other services, support) are UUIDv7 (uuidv7() of
--   PostgreSQL 18 as the default; the service may pass its own). Internal IDs are bigint
--   identities.
-- * Only keys, NOT NULL and DEFAULT live here (owner's decision of 19.10.2026): no CHECK,
--   EXCLUDE, triggers or functions. Value sets, formats, status transitions and the other
--   rules are checked by the service code (docs/schema.md, «Инварианты в коде сервиса»).
--   Foreign key cascades and partitioning stay.
-- * Codes are plain text (readable in psql); the allowed values are listed in the comments.
-- * Money is bigint minor units; operation amounts are unsigned, the direction is a column.
-- * Integers are sized by their real bounds (owner's decision in the review of FIN-111): IDs,
--   money and the versions the contracts carry as int64 (settings_version, calc_version) are
--   bigint; counters and small quantities are integer (a file is at most 10 MB, a user never
--   has a million operations in one file) and the percent of parsing is smallint.
--   squawk-ignore-file prefer-bigint-over-int, prefer-bigint-over-smallint
-- * Instants are timestamptz. Calendar months are date (the 1st) in the user's time zone at
--   calculation time (user_state.timezone); a zone change recalculates every month.
-- * Every table and every column has a COMMENT ON in Russian (owner's request in the review
--   of FIN-111): for a table what it holds, its key and its role, for a column its meaning.
--   Partitions of operation have none; the parent describes them.

-- ---------------------------------------------------------------- user

CREATE TABLE user_state (
    user_id               uuid        PRIMARY KEY,
    timezone              text        NOT NULL DEFAULT 'Europe/Moscow',
    settings_version      bigint      NOT NULL DEFAULT 0,
    calc_version          bigint      NOT NULL DEFAULT 0,
    registry_version      integer     NOT NULL DEFAULT 0,
    categories_changed_at timestamptz,
    uploads_changed_at    timestamptz,
    daily_recalc_due_at   timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE user_state IS
    'Одна строка на каждого пользователя сервиса. В ней хранятся часовой пояс из профиля, версии настроек '
    'и расчёта и время последних изменений данных. Все остальные таблицы пользователя ссылаются на эту '
    'строку, поэтому удаление пользователя — это удаление одной строки, остальное удаляется каскадом. '
    'Шаги одного пользователя, которые нельзя выполнять параллельно, сначала блокируют его строку через '
    'SELECT … FOR NO KEY UPDATE. Обычный FOR UPDATE здесь не подходит: он ждал бы каждую транзакцию, '
    'которая вставила строки этого пользователя, и два импорта заблокировали бы друг друга.';
COMMENT ON COLUMN user_state.user_id IS 'Идентификатор пользователя из сервиса user, тот же, что в токене.';
COMMENT ON COLUMN user_state.timezone IS
    'Часовой пояс из профиля пользователя. Пока сервис user его не прислал, считается, что это '
    'Europe/Moscow.';
COMMENT ON COLUMN user_state.settings_version IS
    'Версия последнего применённого события смены пояса. Событие с такой же или меньшей версией пришло с '
    'опозданием, и сервис его пропускает.';
COMMENT ON COLUMN user_state.calc_version IS
    'Номер последнего расчёта метрик пользователя. Он растёт на единицу в той же транзакции, которая '
    'ставит метрики в очередь на отправку в analytics.';
COMMENT ON COLUMN user_state.registry_version IS
    'Версия реестра метрик, по которой пользователь был посчитан в последний раз. Сам реестр с версиями '
    'хранится в коде. Фоновое задание пересчитывает пользователей с устаревшей версией, а до пересчёта '
    'изменённые метрики считаются недоступными, а не нулевыми. Ноль значит, что пользователя ещё не '
    'считали.';
COMMENT ON COLUMN user_state.categories_changed_at IS
    'Когда пользователь последний раз менял категорию операции или отмечал платёж как «не подписка».';
COMMENT ON COLUMN user_state.uploads_changed_at IS 'Когда последний раз завершилась или была удалена загрузка выписки.';
COMMENT ON COLUMN user_state.daily_recalc_due_at IS
    'Когда пересчитать текущие периоды в следующий раз: ближайшие 00:05 по поясу пользователя. Пусто, '
    'пока у пользователя нет данных.';
COMMENT ON COLUMN user_state.created_at IS 'Когда строка создана.';

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
    sort_order  integer     UNIQUE,
    note        text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE category IS
    'Справочник категорий, общий для всех пользователей. В нём три системные категории, известные '
    'категории Т-Банка и категории банка, которые впервые встретились в чьей-то выписке. Персональных '
    'данных здесь нет: название категории приходит из выписки и одинаково у всех клиентов банка.';
COMMENT ON COLUMN category.id IS
    'Код категории, например cat_supermarkets. Под этим кодом категория уходит в API.';
COMMENT ON COLUMN category.name IS 'Название категории, которое видит пользователь.';
COMMENT ON COLUMN category.bank_name IS
    'Название категории так, как оно записано в выписке банка (поле MEMO), без пробелов по краям и не '
    'длиннее 64 символов. По нему импорт находит категорию операции. У системных категорий, которых нет в '
    'выписках, поле пустое.';
COMMENT ON COLUMN category.icon IS 'Ключ иконки категории.';
COMMENT ON COLUMN category.is_system IS
    'Признак системной категории: «Между своими счетами», «Возврат» или «Без категории». Такие категории '
    'создаёт только миграция.';
COMMENT ON COLUMN category.is_known IS
    'Признак категории из списка известных категорий Т-Банка или системной категории. Категория, которая '
    'впервые встретилась в выписке, этого признака не имеет.';
COMMENT ON COLUMN category.for_expense IS 'Можно ли отнести к этой категории расход.';
COMMENT ON COLUMN category.for_income IS 'Можно ли отнести к этой категории доход.';
COMMENT ON COLUMN category.assignable IS
    'Может ли пользователь сам выбрать эту категорию для операции. Категорию «Без категории» выбрать '
    'нельзя.';
COMMENT ON COLUMN category.sort_order IS
    'Место категории в общем списке. Его задают только системным и известным категориям. Остальные '
    'категории каждый пользователь видит после них в том порядке, в котором они впервые встретились в его '
    'выписках. Этот порядок код вычисляет по операциям пользователя (docs/schema.md).';
COMMENT ON COLUMN category.note IS
    'Короткое пояснение к системной категории, например «не в тратах». У остальных категорий поле пустое.';
COMMENT ON COLUMN category.created_at IS 'Когда категория добавлена.';
COMMENT ON COLUMN category.updated_at IS 'Когда категория последний раз менялась.';

INSERT INTO category (id, name, bank_name, icon, is_system, is_known, for_expense, for_income, assignable, sort_order, note) VALUES
    ('cat_own_transfer',  'Между своими счетами', NULL,            'transfer', true, true, true,  true, true,  10, 'не в тратах'),
    ('cat_refund',        'Возврат',              NULL,            'repeat',   true, true, false, true, true,  20, 'уменьшает расходы на покупку'),
    ('cat_uncategorized', 'Без категории',        'Без категории', 'tag',      true, true, true,  true, false, 30, 'в выписке нет категории');

-- ---------------------------------------------------------------- accounts

CREATE TABLE account (
    user_id            uuid        NOT NULL REFERENCES user_state ON DELETE CASCADE,
    id                 bigint      GENERATED ALWAYS AS IDENTITY,
    source             text        NOT NULL,
    ext_id             text        NOT NULL,
    type               text        NOT NULL,
    mask               text,
    is_other_bank      boolean     NOT NULL,
    currency           text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, id),
    UNIQUE (user_id, source, ext_id)
);

COMMENT ON TABLE account IS
    'Счета пользователя, которые встретились в его выписках. Номер счёта хранится так, как он записан в '
    'выписке. Один и тот же номер из одного источника у одного пользователя — это один счёт.';
COMMENT ON COLUMN account.user_id IS 'Пользователь, которому принадлежит строка.';
COMMENT ON COLUMN account.id IS 'Внутренний номер счёта в сервисе. Наружу он не отдаётся.';
COMMENT ON COLUMN account.source IS
    'Откуда пришёл счёт, например tbank_ofx. Коды источников и их названия заданы в коде сервиса.';
COMMENT ON COLUMN account.ext_id IS
    'Номер счёта так, как он записан в выписке (ACCTID). У счетов Т-Банка это 20 цифр, у подключённого '
    'счёта другого банка — строка, которая начинается с OB$. Это персональные данные: наружу отдаются '
    'только последние цифры, в логи номер не пишется.';
COMMENT ON COLUMN account.type IS
    'Тип счёта из выписки, например checking или savings. Название типа для пользователя составляет '
    'сервис.';
COMMENT ON COLUMN account.mask IS
    'Последние четыре цифры номера. У подключённого счёта другого банка поле пустое.';
COMMENT ON COLUMN account.is_other_bank IS
    'Признак счёта другого банка, подключённого в Т-Банке. Номер такого счёта начинается с OB$.';
COMMENT ON COLUMN account.currency IS 'Валюта счёта, если выписка её указывает.';
COMMENT ON COLUMN account.created_at IS 'Когда счёт впервые встретился в выписке.';

-- ---------------------------------------------------------------- uploads

CREATE TABLE upload (
    user_id            uuid        NOT NULL REFERENCES user_state ON DELETE CASCADE,
    id                 uuid        NOT NULL DEFAULT uuidv7(),
    source             text        NOT NULL,
    file_name          text        NOT NULL,
    size_bytes         integer     NOT NULL,
    content_sha256     bytea       NOT NULL,
    object_key         text        NOT NULL,
    file_deleted_at    timestamptz,
    status             text        NOT NULL DEFAULT 'processing',
    error_code         text,
    percent            smallint    NOT NULL DEFAULT 0,
    uploaded_at        timestamptz NOT NULL DEFAULT now(),
    finished_at        timestamptz,
    deleted_at         timestamptz,
    operation_count    integer,
    first_operation_at timestamptz,
    last_operation_at  timestamptz,
    statement_from     date,
    statement_to       date,
    account_ids        bigint[]    NOT NULL DEFAULT '{}',
    result             jsonb,
    PRIMARY KEY (user_id, id),
    -- One file per upload: deleting the file of one upload never removes another's, even when
    -- the same content is uploaded again.
    UNIQUE (user_id, object_key)
);

COMMENT ON TABLE upload IS
    'Загруженные пользователем файлы выписок. Удалённая или неудачная загрузка остаётся в таблице. '
    'Поэтому повторное удаление отвечает успехом, а «не найдено» получает только идентификатор, которого '
    'у пользователя никогда не было. По этой же строке пользователь при переподключении получает итог '
    'неудачной загрузки (api.md 3.2–3.5).';
COMMENT ON COLUMN upload.user_id IS 'Пользователь, которому принадлежит строка.';
COMMENT ON COLUMN upload.id IS 'Идентификатор загрузки, под ним она уходит в API.';
COMMENT ON COLUMN upload.source IS
    'Формат выписки, например tbank_ofx. Коды форматов и их названия заданы в коде сервиса.';
COMMENT ON COLUMN upload.file_name IS 'Имя файла, как его прислал клиент, не длиннее 255 символов.';
COMMENT ON COLUMN upload.size_bytes IS 'Размер файла в байтах.';
COMMENT ON COLUMN upload.content_sha256 IS
    'Хеш содержимого файла (SHA-256). По нему вместе с именем файла узнаётся повтор того же запроса, и он '
    'помогает поддержке. Он не уникален: тот же файл можно загрузить ещё раз.';
COMMENT ON COLUMN upload.object_key IS
    'Путь к файлу в хранилище: идентификатор пользователя, косая черта и идентификатор загрузки. У каждой '
    'загрузки свой файл. При удалении пользователя все его файлы находятся по общему началу пути.';
COMMENT ON COLUMN upload.file_deleted_at IS
    'Когда файл удалён из хранилища. Если загрузка неудачна или удалена, а поле пустое, файл ещё '
    'предстоит удалить фоновому заданию.';
COMMENT ON COLUMN upload.status IS
    'Состояние загрузки. Новая загрузка разбирается (processing), потом становится успешной (done) или '
    'неудачной (failed). Успешную загрузку пользователь может удалить (deleted).';
COMMENT ON COLUMN upload.error_code IS
    'Почему загрузка неудачна: не удалось разобрать файл (processing_failed) или пользователь прервал '
    'разбор (cancelled). Заполнено только у неудачной загрузки.';
COMMENT ON COLUMN upload.percent IS
    'Сколько процентов разбора выполнено, от 0 до 100. Это значение получает клиент, который следит за '
    'разбором. Название этапа не хранится: код выводит его из процента.';
COMMENT ON COLUMN upload.uploaded_at IS
    'Когда загрузка создана. По этому времени загрузки упорядочиваются в истории и при закрытии '
    'непрочитанных строк.';
COMMENT ON COLUMN upload.finished_at IS
    'Когда разбор закончился, успешно или нет. Пока файл разбирается, поле пустое.';
COMMENT ON COLUMN upload.deleted_at IS
    'Когда пользователь удалил загрузку. Заполнено только у удалённой загрузки.';
COMMENT ON COLUMN upload.operation_count IS 'Сколько операций прочитано из файла, включая уже загруженные раньше.';
COMMENT ON COLUMN upload.first_operation_at IS
    'Время первой операции в файле. Заполняется в конце успешного разбора и остаётся после удаления '
    'загрузки. Поле пустое, если в файле нет операций или разбор не закончился.';
COMMENT ON COLUMN upload.last_operation_at IS
    'Время последней операции в файле. Заполняется так же, как время первой операции.';
COMMENT ON COLUMN upload.statement_from IS
    'Первый день периода выписки — дата из тега DTSTART, как она записана в файле. У всех счетов одного '
    'файла период один. Поле пустое, если в файле нет периода или разбор не закончился (docs/schema.md).';
COMMENT ON COLUMN upload.statement_to IS
    'Последний день периода выписки — дата из тега DTEND, как она записана в файле. Этот день входит в '
    'период целиком.';
COMMENT ON COLUMN upload.account_ids IS
    'Счета, которые есть в файле, в том числе счета без операций. По этому списку код проверяет, содержит '
    'ли более поздняя загрузка счёт непрочитанной строки. Внешнего ключа у списка нет, поэтому то, что в '
    'нём только счета этого пользователя, проверяет код (docs/schema.md).';
COMMENT ON COLUMN upload.result IS
    'Итог импорта, который пользователь видит после успешного разбора. Хранится, чтобы показать его снова '
    'при переподключении. Заполнено только у успешной загрузки.';

-- Timeout sweep: uploads processing for more than 3 minutes, across users.
CREATE INDEX upload_processing_idx ON upload (uploaded_at) WHERE status = 'processing';
-- Files still to remove from Object Storage.
CREATE INDEX upload_file_pending_idx ON upload (finished_at)
    WHERE status IN ('failed', 'deleted') AND file_deleted_at IS NULL;

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
    'Операции пользователя, по одной на каждую настоящую операцию, даже если она пришла из нескольких '
    'выписок. Таблица разбита на части по годам времени операции, границы годов — по UTC. У частей '
    'таблицы своих описаний нет. Категорию, возврат и пару перевода код пересчитывает после каждого '
    'импорта, удаления загрузки и ручной правки.';
COMMENT ON COLUMN operation.user_id IS 'Пользователь, которому принадлежит строка.';
COMMENT ON COLUMN operation.occurred_at IS
    'Время операции из выписки (DTPOSTED). По нему операция попадает в год, сортируется и относится к '
    'месяцу.';
COMMENT ON COLUMN operation.id IS 'Идентификатор операции, под ним она уходит в API.';
COMMENT ON COLUMN operation.account_id IS 'Счёт, по которому прошла операция.';
COMMENT ON COLUMN operation.direction IS 'Расход (expense) или доход (income).';
COMMENT ON COLUMN operation.amount_minor IS
    'Сумма без знака в минимальных единицах валюты операции (для рублей — в копейках). Расход это или доход, '
    'показывает направление.';
COMMENT ON COLUMN operation.currency IS 'Валюта операции.';
COMMENT ON COLUMN operation.status IS
    'Проведена операция банком (posted) или ещё ожидает проведения (pending).';
COMMENT ON COLUMN operation.description IS 'Описание операции из выписки как есть. Оно может быть пустым.';
COMMENT ON COLUMN operation.bank_category_id IS
    'Категория, которую дал операции банк. Если банк категорию не указал, это «Без категории».';
COMMENT ON COLUMN operation.posted_at IS 'Когда банк провёл операцию, если выписка это сообщает.';
COMMENT ON COLUMN operation.mcc IS
    'Код торговой точки (MCC), если выписка его сообщает. В выписке Т-Банка его нет.';
COMMENT ON COLUMN operation.extensions IS
    'Поля, которые есть только у одного источника. Их состав задаёт код разбора этого источника.';
COMMENT ON COLUMN operation.category_id IS
    'Итоговая категория операции, которую видит пользователь (docs/calc.md 3.5).';
COMMENT ON COLUMN operation.manual_category_id IS
    'Категория, которую выбрал сам пользователь. Она сохраняется при повторном импорте и пересчёте.';
COMMENT ON COLUMN operation.refund_target_category_id IS 'Для возврата — категория, расходы которой он уменьшает.';
COMMENT ON COLUMN operation.is_split IS
    'Пользователь разделил пару перевода, в которую входила эта операция (docs/calc.md 3.2).';
COMMENT ON COLUMN operation.transfer_pair_id IS
    'Общий идентификатор у двух операций, которые вместе составляют перевод между своими счетами. В API '
    'такая пара показывается одной записью с этим идентификатором, а время и сумма берутся у списания. '
    'Если операция не в паре, поле пустое. Что в паре ровно две операции, расход и доход, проверяет код '
    '(docs/schema.md).';
COMMENT ON COLUMN operation.created_at IS 'Когда операция появилась при импорте.';
COMMENT ON COLUMN operation.updated_at IS 'Когда операцию последний раз меняли правила или пользователь.';

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
    'Записи об операциях так, как они пришли из источника: счёт и идентификатор операции в выписке. По '
    'ним импорт узнаёт операцию, которая уже была загружена раньше. Одна операция может иметь несколько '
    'записей, если она пришла из разных источников. Таблица не разбита по годам: так один и тот же '
    'идентификатор из выписки остаётся одной записью, даже если у операции изменилась дата.';
COMMENT ON COLUMN source_record.user_id IS 'Пользователь, которому принадлежит строка.';
COMMENT ON COLUMN source_record.id IS
    'Внутренний номер записи. Наружу он не отдаётся. В порядке этих номеров импорт добавляет записи, то '
    'есть в порядке строк файла.';
COMMENT ON COLUMN source_record.account_id IS 'Счёт, к которому относится идентификатор операции.';
COMMENT ON COLUMN source_record.external_id IS
    'Идентификатор операции в выписке (FITID). Сервис не разбирает его содержимое.';
COMMENT ON COLUMN source_record.operation_occurred_at IS
    'Время операции, к которой относится запись. Оно нужно для ссылки на таблицу операций, разбитую по '
    'годам, и меняется вместе со временем операции.';
COMMENT ON COLUMN source_record.operation_id IS 'Операция, к которой относится запись.';
COMMENT ON COLUMN source_record.upload_ids IS
    'Загрузки этого пользователя, в файлах которых есть запись. Когда загрузку удаляют, она исчезает из '
    'списка. Операция удаляется, когда ни у одной её записи не осталось загрузок. Внешнего ключа у списка '
    'нет, поэтому то, что в нём только загрузки этого пользователя, проверяет код (docs/schema.md).';
COMMENT ON COLUMN source_record.created_at IS 'Когда запись впервые встретилась в загрузке.';

-- Cascade from operation and the lookup of an operation's records.
CREATE INDEX source_record_operation_idx ON source_record (user_id, operation_id);
-- No index on upload_ids: deleting an upload filters the user's records (the PK prefix
-- user_id, thousands of rows) by upload_ids @> ARRAY[$upload]; FIN-79 reads one record by
-- its unique key and checks $upload = ANY(upload_ids). docs/schema.md, «Ожидаемые запросы».

CREATE TABLE unread_line (
    user_id               uuid        NOT NULL,
    upload_id             uuid        NOT NULL,
    line_number           integer     NOT NULL,
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
    'Строки файла выписки, которые не удалось прочитать. Содержимое строки не хранится, только её номер, '
    'дата, причина, а если удалось прочитать, то счёт и идентификатор операции. Более поздняя загрузка, '
    'которая прочитала эти данные, закрывает строку (api.md 3.6).';
COMMENT ON COLUMN unread_line.user_id IS 'Пользователь, которому принадлежит строка.';
COMMENT ON COLUMN unread_line.upload_id IS 'Загрузка, в файле которой находится строка.';
COMMENT ON COLUMN unread_line.line_number IS
    'Номер строки в файле, с которой начинается операция. Строки нумеруются с единицы.';
COMMENT ON COLUMN unread_line.reason IS
    'Почему строку не удалось прочитать: неверная сумма (bad_amount), неверная дата (bad_date), нет даты '
    '(no_date) или нет идентификатора операции (truncated).';
COMMENT ON COLUMN unread_line.occurred_at IS 'Дата операции, если её удалось прочитать.';
COMMENT ON COLUMN unread_line.account_id IS 'Счёт, к которому относится строка, если он известен.';
COMMENT ON COLUMN unread_line.external_id IS
    'Идентификатор операции (FITID), если его удалось прочитать. По счёту и этому идентификатору строку '
    'закрывает более поздняя загрузка.';
COMMENT ON COLUMN unread_line.resolved_by_upload_id IS
    'Более поздняя загрузка, которая закрыла строку. Пока строка открыта, поле пустое. Если ту загрузку '
    'удалят, код очищает поле и проверяет строку заново.';
COMMENT ON COLUMN unread_line.resolution IS
    'Как закрыта строка: более поздняя загрузка её прочитала (read) или в ней эта строка снова не '
    'прочиталась и заменила старую (replaced). Заполняется вместе с закрывшей загрузкой.';

-- 3.6 by period and unread_lines_count of a month: open lines with a date.
CREATE INDEX unread_line_open_date_idx ON unread_line (user_id, occurred_at)
    WHERE resolved_by_upload_id IS NULL AND occurred_at IS NOT NULL;
-- Closing by (account, FITID) on import.
CREATE INDEX unread_line_external_id_idx ON unread_line (user_id, account_id, external_id)
    WHERE external_id IS NOT NULL;
-- Lines closed by an upload: recheck when it is deleted, resolved_unread_count of 3.4.
CREATE INDEX unread_line_resolved_by_idx ON unread_line (user_id, resolved_by_upload_id)
    WHERE resolved_by_upload_id IS NOT NULL;

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
    'Регулярные платежи, которые сервис распознал у пользователя. Платёж определяется описанием операций '
    'и периодичностью. Строка создаётся при первом распознавании и больше не удаляется, поэтому '
    'идентификатор платежа и отметка «не подписка» сохраняются при пересчётах и новых загрузках.';
COMMENT ON COLUMN regular_payment.user_id IS 'Пользователь, которому принадлежит строка.';
COMMENT ON COLUMN regular_payment.id IS 'Постоянный идентификатор платежа. Из описания он не выводится.';
COMMENT ON COLUMN regular_payment.periodicity IS
    'Как часто повторяется платёж. Сейчас бывает только ежемесячный (monthly).';
COMMENT ON COLUMN regular_payment.description IS 'Описание операций этого платежа, точно как в выписке.';
COMMENT ON COLUMN regular_payment.dismissed_at IS
    'Когда пользователь отметил платёж как «не подписка». Пока отметки нет, платёж показывается.';
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
    'Получатели платежей, на которые клиент может сослаться в фильтре операций. Получатель — это описание '
    'операции, приведённое к одному виду. Клиент передаёт в фильтре только идентификатор получателя, '
    'чтобы описание не попало в адрес запроса. Ссылка действует, пока строка существует.';
COMMENT ON COLUMN merchant.user_id IS 'Пользователь, которому принадлежит строка.';
COMMENT ON COLUMN merchant.id IS 'Идентификатор получателя, который клиент передаёт в фильтре.';
COMMENT ON COLUMN merchant.key IS
    'Описание операций, приведённое к одному виду. У одного пользователя такое описание встречается один '
    'раз.';
COMMENT ON COLUMN merchant.created_at IS 'Когда получатель впервые понадобился для фильтра.';

-- ---------------------------------------------------------------- calculation

CREATE TABLE recalc_queue (
    user_id         uuid        NOT NULL REFERENCES user_state ON DELETE CASCADE,
    month           date        NOT NULL,
    generation      integer     NOT NULL DEFAULT 1,
    enqueued_at     timestamptz NOT NULL DEFAULT now(),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    attempts        integer     NOT NULL DEFAULT 0,
    last_error      text,
    PRIMARY KEY (user_id, month)
);

COMMENT ON TABLE recalc_queue IS
    'Месяцы пользователей, которые нужно пересчитать. Когда месяц помечают снова, счётчик пометок растёт '
    'на единицу. Обработчик берёт строку, считает месяц по операциям и периодам загрузок, отправляет '
    'метрики в analytics и удаляет строку. Если за это время месяц пометили снова, строка остаётся и '
    'будет пересчитана ещё раз. Готовые суммы в этой базе не хранятся.';
COMMENT ON COLUMN recalc_queue.user_id IS 'Пользователь, которому принадлежит строка.';
COMMENT ON COLUMN recalc_queue.month IS
    'Месяц, который нужно пересчитать: его первое число по поясу пользователя.';
COMMENT ON COLUMN recalc_queue.generation IS
    'Сколько раз месяц помечали. Обработчик удаляет строку, только если это число не изменилось, пока он '
    'считал.';
COMMENT ON COLUMN recalc_queue.enqueued_at IS 'Когда месяц пометили впервые.';
COMMENT ON COLUMN recalc_queue.next_attempt_at IS
    'Когда строку можно взять в работу. Обработчик сдвигает это время вперёд, пока считает, и после '
    'ошибки.';
COMMENT ON COLUMN recalc_queue.attempts IS 'Сколько раз пересчёт не удался.';
COMMENT ON COLUMN recalc_queue.last_error IS 'Текст последней ошибки пересчёта для разбора проблем.';

CREATE INDEX recalc_queue_next_attempt_at_idx ON recalc_queue (next_attempt_at);
