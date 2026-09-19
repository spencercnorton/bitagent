-- +goose Up
CREATE TABLE llm_request_budgets (
    scope text NOT NULL,
    month_start date NOT NULL,
    day_start date NOT NULL,
    daily_calls integer NOT NULL CHECK (daily_calls >= 0),
    monthly_calls integer NOT NULL CHECK (monthly_calls >= daily_calls),
    PRIMARY KEY (scope, month_start)
);

-- +goose Down
DROP TABLE llm_request_budgets;
