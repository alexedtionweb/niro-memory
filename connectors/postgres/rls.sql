-- Row-level security (Options.RLS): every tenant table only exposes rows
-- of the tenant set with set_config('niromem.tenant', ..., true) in the
-- current transaction. Jobs are not covered: workers claim across tenants.
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['memories','history','entities','entity_aliases','memory_entities','runs','episodes','embeddings','settings','relationship_sets','knowledge_relations','operation_receipts'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t); -- owners too
        IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = current_schema() AND tablename = t AND policyname = 'niromem_tenant') THEN
            EXECUTE format('CREATE POLICY niromem_tenant ON %I USING (tenant = current_setting(''niromem.tenant'', true)) WITH CHECK (tenant = current_setting(''niromem.tenant'', true))', t);
        END IF;
    END LOOP;
END $$;
