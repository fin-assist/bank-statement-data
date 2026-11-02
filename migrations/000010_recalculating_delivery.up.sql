ALTER TABLE regular_payment ADD COLUMN dismissal_calc_version integer;
COMMENT ON COLUMN regular_payment.dismissal_calc_version IS 'Исходная целевая версия текущего исключения; повтор PUT возвращает её без нового изменения. Прежние отметки инициализируются при первом повторе.';

CREATE TABLE recalculating_delivery (
    user_id uuid NOT NULL REFERENCES user_state(user_id) ON DELETE CASCADE,
    calc_version integer NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    next_attempt_at timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, calc_version)
);
COMMENT ON TABLE recalculating_delivery IS 'Надёжная доставка только true-маркера пересчёта; удаляется после ACK независимо от готовности метрик.';
COMMENT ON COLUMN recalculating_delivery.user_id IS 'Пользователь; удаление аккаунта каскадно удаляет задание.';
COMMENT ON COLUMN recalculating_delivery.calc_version IS 'Не зарезервированная в счётчике целевая версия current + 1, положительная.';
COMMENT ON COLUMN recalculating_delivery.created_at IS 'Время постановки задания в транзакции изменения.';
COMMENT ON COLUMN recalculating_delivery.next_attempt_at IS 'Ближайшее время повторной попытки доставки.';
COMMENT ON COLUMN recalculating_delivery.attempts IS 'Число неуспешных попыток, ограниченное кодом сервиса.';
CREATE INDEX recalculating_delivery_due ON recalculating_delivery(next_attempt_at);
