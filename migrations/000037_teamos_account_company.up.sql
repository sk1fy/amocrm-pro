CREATE TABLE teamos_account_companies (
    account_id bigint PRIMARY KEY CHECK (account_id > 0),
    company_id uuid NOT NULL UNIQUE,
    observed_at timestamptz NOT NULL DEFAULT now()
);
