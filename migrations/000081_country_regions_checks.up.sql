-- Contrôles des pays et des adresses de mesure (000080) : la base refuse ce
-- que le backend refuse déjà (pays ISO à deux lettres en majuscules, adresse
-- de mesure https), et reprise du pays des organisations créées avant 000080.

-- Valeurs existantes ramenées à la forme contrôlée : majuscules sans espaces,
-- sinon vides (organisations, adresses) ou retirées (table pays → région,
-- dont le pays est la clé). Rien de tel n'est censé exister.
UPDATE organizations SET country = CASE
        WHEN upper(btrim(country)) ~ '^[A-Z]{2}$' THEN upper(btrim(country))
        ELSE '' END
WHERE country !~ '^([A-Z]{2})?$';
DELETE FROM country_regions WHERE country !~ '^[A-Z]{2}$';
UPDATE region_settings SET probe_url = '' WHERE probe_url <> '' AND probe_url NOT LIKE 'https://%';

-- Reprise : pays inconnu d'une organisation personnelle → pays du profil de
-- son propriétaire (users.country est CHAR(2) : « » vide).
UPDATE organizations o SET country = upper(btrim(u.country))
FROM users u
WHERE o.kind = 'PERSONAL' AND o.owner_user_id = u.id
  AND o.country = '' AND upper(btrim(u.country)) ~ '^[A-Z]{2}$';

-- Puis pays du compte de facturation de l'organisation.
UPDATE organizations o SET country = upper(btrim(b.country))
FROM billing_accounts b
WHERE b.organization_id = o.id
  AND o.country = '' AND upper(btrim(b.country)) ~ '^[A-Z]{2}$';

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'organizations_country_check') THEN
        ALTER TABLE organizations ADD CONSTRAINT organizations_country_check
            CHECK (country ~ '^([A-Z]{2})?$');
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'country_regions_country_check') THEN
        ALTER TABLE country_regions ADD CONSTRAINT country_regions_country_check
            CHECK (country ~ '^[A-Z]{2}$');
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'region_settings_probe_url_check') THEN
        ALTER TABLE region_settings ADD CONSTRAINT region_settings_probe_url_check
            CHECK (probe_url = '' OR probe_url LIKE 'https://%');
    END IF;
END $$;
