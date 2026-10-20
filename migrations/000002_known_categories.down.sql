-- Rollback of 000002: removes exactly the known categories it inserted. Fails on a foreign key
-- if operations already refer to them; then roll back the application, not the data
-- (docs/migrations.md, «Откат»).
DELETE FROM category WHERE id IN (
    'cat_supermarkets', 'cat_transfers', 'cat_fastfood', 'cat_restaurants', 'cat_taxi',
    'cat_digital', 'cat_finance', 'cat_transport', 'cat_mobile', 'cat_pharmacy', 'cat_fuel',
    'cat_charity', 'cat_dividends', 'cat_marketplaces', 'cat_bonuses', 'cat_cash', 'cat_salary',
    'cat_services', 'cat_rail', 'cat_service', 'cat_telecom', 'cat_nko', 'cat_bank_fees',
    'cat_utilities', 'cat_interest', 'cat_pets', 'cat_medicine', 'cat_flights', 'cat_streaming',
    'cat_toll_roads', 'cat_art', 'cat_gifts', 'cat_top_ups', 'cat_beauty', 'cat_flowers',
    'cat_gov', 'cat_deposits', 'cat_travel', 'cat_repair', 'cat_other', 'cat_goods',
    'cat_auto_services', 'cat_yandex', 'cat_fines', 'cat_clothes', 'cat_entertainment',
    'cat_photo', 'cat_car_dealers', 'cat_hotels', 'cat_fitness', 'cat_internet'
);
