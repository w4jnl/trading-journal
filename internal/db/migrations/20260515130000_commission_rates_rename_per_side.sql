-- migrate:up
-- The column was misnamed: every seeded value (and every value the user
-- entered through /commissions) is the broker's per-side rate, not the
-- round-turn. The position builder applied it as if it were RT, so
-- commissions came out at half the actual amount on every rate-based
-- account. Rename to per_contract_per_side; a follow-up code change
-- makes the builder charge both legs (rate * qty * 2). modernc.org/sqlite
-- supports RENAME COLUMN (SQLite 3.25+), so no table rebuild is needed.
ALTER TABLE commission_rates RENAME COLUMN per_contract_rt TO per_contract_per_side;

-- migrate:down
ALTER TABLE commission_rates RENAME COLUMN per_contract_per_side TO per_contract_rt;
