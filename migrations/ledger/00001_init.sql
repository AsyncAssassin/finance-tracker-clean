-- +goose Up
-- One budget per user and category. Amounts are whole kopecks.
CREATE TABLE budgets (
    user_id       BIGINT      NOT NULL,
    category      TEXT        NOT NULL,
    limit_kopecks BIGINT      NOT NULL CHECK (limit_kopecks > 0),
    period        TEXT        NOT NULL CHECK (period IN ('month', 'year', 'none')),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, category)
);

CREATE TABLE transactions (
    id             BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id        BIGINT      NOT NULL,
    amount_kopecks BIGINT      NOT NULL CHECK (amount_kopecks > 0),
    category       TEXT        NOT NULL,
    description    TEXT        NOT NULL DEFAULT '',
    occurred_on    DATE        NOT NULL,
    external_id    TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The budget check sums one category of one user within a period.
CREATE INDEX transactions_user_category_date_idx ON transactions (user_id, category, occurred_on);
-- Listing shows the newest transactions first.
CREATE INDEX transactions_user_date_idx ON transactions (user_id, occurred_on DESC, id DESC);
-- A client-side ID may be used once per user; rows without one are free.
CREATE UNIQUE INDEX transactions_user_external_idx ON transactions (user_id, external_id) WHERE external_id IS NOT NULL;

-- +goose Down
DROP TABLE transactions;
DROP TABLE budgets;
