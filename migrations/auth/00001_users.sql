-- +goose Up
-- Accounts of the auth service. The email is stored normalized (trimmed,
-- lowercased) by the service, so the unique index makes it unique in any
-- case. The ledger keeps user_id in its tables without a foreign key: the
-- services own their tables and do not join across them.
CREATE TABLE users (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email         TEXT        NOT NULL CHECK (length(email) BETWEEN 3 AND 254),
    password_hash TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX users_email_key ON users (email);

-- +goose Down
DROP TABLE users;
