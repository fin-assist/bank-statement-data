ALTER TABLE regular_payment ADD COLUMN dismissal_calc_version integer;
COMMENT ON COLUMN regular_payment.dismissal_calc_version IS 'Исходная целевая версия текущего исключения; повтор PUT возвращает её без нового изменения. Прежние отметки инициализируются при первом повторе.';
