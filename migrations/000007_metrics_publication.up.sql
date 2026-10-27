ALTER TABLE user_state ADD COLUMN daily_recalc_attempts smallint NOT NULL DEFAULT 0;
ALTER TABLE user_state ADD COLUMN daily_recalc_failed_since timestamptz;
COMMENT ON COLUMN user_state.daily_recalc_attempts IS 'Число последовательных сбоев ежедневного пересчёта без пользовательской очереди, ограниченное 32767; успех или новое пользовательское изменение сбрасывает счётчик.';
COMMENT ON COLUMN user_state.daily_recalc_failed_since IS 'Первый пропущенный ежедневный срок при повторных ошибках; сохраняет честный возраст ожидающего пересчёта, пока next_attempt переносится по backoff.';

ALTER TABLE upload ADD COLUMN statement_from_instant timestamptz;
ALTER TABLE upload ADD COLUMN statement_to_instant timestamptz;
ALTER TABLE upload ADD COLUMN statement_range_source text;
COMMENT ON COLUMN upload.statement_from_instant IS 'Исходный минимальный DTSTART либо момент первой операции до календарной проекции в пояс профиля; NULL у старой загрузки без точной provenance.';
COMMENT ON COLUMN upload.statement_to_instant IS 'Исходный максимальный DTEND либо момент последней операции до календарной проекции; включающая граница по исходному OFX.';
COMMENT ON COLUMN upload.statement_range_source IS 'Источник исходных границ: tags, operations или none для нового пустого файла; NULL у прежней загрузки, где остались только календарные даты.';

CREATE TABLE metrics_publication (
    user_id uuid PRIMARY KEY REFERENCES user_state(user_id) ON DELETE CASCADE,
    state jsonb NOT NULL DEFAULT '{}',
    period_keys jsonb NOT NULL DEFAULT '[]',
    recalculating boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE metrics_publication IS 'Контрольная точка последнего вычисленного состояния, полного перечня календарных периодов и начала пересчёта: позволяет возобновить работу и удалить периоды через removed_periods после удаления исходных фактов или смены пояса.';
COMMENT ON COLUMN metrics_publication.user_id IS 'Владелец состояния; удаление пользователя удаляет служебные данные каскадом.';
COMMENT ON COLUMN metrics_publication.state IS 'Полное состояние последнего вычисленного набора; не означает подтверждённую доставку в analytics.';
COMMENT ON COLUMN metrics_publication.period_keys IS 'Идентификаторы календарных периодов последнего вычисленного набора: пары Kind (month, quarter, year) и Key (YYYY-MM, YYYY-Qn, YYYY). Используются для removed_periods обоих режимов переводов.';
COMMENT ON COLUMN metrics_publication.recalculating IS 'Признак зафиксированного начала незавершённого пересчёта; позволяет возобновить его без повторного маркера начала.';

CREATE TABLE metrics_delivery (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES user_state(user_id) ON DELETE CASCADE,
    calc_version integer NOT NULL,
    payload bytea NOT NULL,
    final boolean NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX metrics_delivery_version_idx ON metrics_delivery(user_id,calc_version) WHERE final=false;
COMMENT ON TABLE metrics_delivery IS 'Неизменяемые protobuf payload под FK пользователя; наличие неподтверждённых частей задаёт ACK-барьер для финального состояния. Общий outbox хранит только непрозрачную ссылку; удаление пользователя атомарно стирает финансовые payload.';
COMMENT ON COLUMN metrics_delivery.id IS 'Непрозрачный идентификатор доставки без данных пользователя.';
COMMENT ON COLUMN metrics_delivery.user_id IS 'Владелец снимка; удаление пользователя каскадно удаляет все ожидающие payload.';
COMMENT ON COLUMN metrics_delivery.calc_version IS 'Версия неизменяемого снимка для идемпотентности analytics.';
COMMENT ON COLUMN metrics_delivery.payload IS 'Точный сериализованный UpsertUserMetricsRequest; не изменяется после записи.';
COMMENT ON COLUMN metrics_delivery.final IS 'Финальное состояние recalculating=false ждёт подтверждения частей предыдущей версии.';

COMMENT ON COLUMN metrics_publication.created_at IS 'Когда впервые записано служебное состояние публикации.';
COMMENT ON COLUMN metrics_delivery.created_at IS 'Когда неизменяемый payload поставлен на доставку.';
