CREATE TABLE platform_manager.widget_feedback (
    account_id UUID NOT NULL REFERENCES platform_lifecycle.accounts(id),
    product_id UUID NOT NULL REFERENCES platform_lifecycle.products(id),
    PRIMARY KEY (account_id,product_id)
);
GRANT SELECT,INSERT,DELETE ON platform_manager.widget_feedback TO platform_jobs;
