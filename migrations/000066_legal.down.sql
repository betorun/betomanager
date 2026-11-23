DROP TABLE IF EXISTS legal_acceptances;
DROP TRIGGER IF EXISTS trg_legal_documents_immutable ON legal_documents;
DROP TABLE IF EXISTS legal_documents;
DROP FUNCTION IF EXISTS legal_documents_immutable();
