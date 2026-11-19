DROP TABLE IF EXISTS domain_orders;
DELETE FROM billing_catalogs WHERE data ? 'domain_margin_bp' AND created_by = 'migration';
