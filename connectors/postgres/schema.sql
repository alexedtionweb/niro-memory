-- niro-agent-memory PostgreSQL schema, version 1. Idempotent; applied under
-- an advisory lock so many nodes can start at once.

CREATE TABLE IF NOT EXISTS tenant_purges (
    tenant TEXT PRIMARY KEY, id TEXT NOT NULL, state TEXT NOT NULL,
    phase INTEGER NOT NULL, deleted BIGINT NOT NULL, ready_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS tenant_purge_requests (
    tenant TEXT NOT NULL, request_id TEXT NOT NULL, purge_id TEXT NOT NULL,
    PRIMARY KEY (tenant, request_id)
);

CREATE TABLE IF NOT EXISTS memories (
    tenant         text        NOT NULL,
    id             text        NOT NULL,
    user_id        text        NOT NULL DEFAULT '',
    agent_id       text        NOT NULL DEFAULT '',
    run_id         text        NOT NULL DEFAULT '',
    kind           text        NOT NULL,
    layer          int         NOT NULL,
    title          text        NOT NULL DEFAULT '',
    text           text        NOT NULL,
    subject        text        NOT NULL DEFAULT '',
    predicate      text        NOT NULL DEFAULT '',
    object         text        NOT NULL DEFAULT '',
    slot           text        NOT NULL DEFAULT '',
    hash           text        NOT NULL DEFAULT '',
    tags           jsonb       NOT NULL DEFAULT '[]',
    metadata       jsonb       NOT NULL DEFAULT '{}',
    confidence     float8      NOT NULL DEFAULT 0,
    importance     float8      NOT NULL DEFAULT 0,
    heat           bigint      NOT NULL DEFAULT 0,
    visibility     text        NOT NULL,
    grants         jsonb       NOT NULL DEFAULT '[]',
    valid_from     timestamptz NOT NULL,
    valid_to       timestamptz,
    recorded_at    timestamptz NOT NULL,
    invalidated_at timestamptz,
    expires_at     timestamptz,
    version        int         NOT NULL DEFAULT 1,
    supersedes     text        NOT NULL DEFAULT '',
    src_kind       text        NOT NULL DEFAULT '',
    actor          text        NOT NULL DEFAULT '',
    role           text        NOT NULL DEFAULT '',
    trust          text        NOT NULL DEFAULT '',
    derived_by     text        NOT NULL DEFAULT '',
    source         jsonb       NOT NULL DEFAULT '{}',
    fts_scope      text        NOT NULL DEFAULT '', -- scope tokens, see sqlgen.MemoryTokens
    tsv            tsvector GENERATED ALWAYS AS (to_tsvector('simple', title || ' ' || text || ' ' || fts_scope)) STORED,
    PRIMARY KEY (tenant, id)
);
CREATE INDEX IF NOT EXISTS memories_owner ON memories (tenant, user_id, agent_id, id);
CREATE INDEX IF NOT EXISTS memories_hash ON memories (tenant, hash);
CREATE INDEX IF NOT EXISTS memories_enrichment_identity ON memories
    (tenant, (CASE WHEN jsonb_typeof(metadata->'enrichment_identity') = 'string'
        THEN metadata->>'enrichment_identity' END));
CREATE INDEX IF NOT EXISTS memories_supersedes ON memories (tenant, supersedes) WHERE supersedes <> '';
CREATE INDEX IF NOT EXISTS memories_tsv ON memories USING gin (tsv);
CREATE UNIQUE INDEX IF NOT EXISTS memories_slot ON memories (tenant, user_id, agent_id, slot)
    WHERE slot <> '' AND valid_to IS NULL AND invalidated_at IS NULL;

CREATE TABLE IF NOT EXISTS history (
    seq       bigserial PRIMARY KEY,
    tenant    text        NOT NULL,
    memory_id text        NOT NULL,
    action    text        NOT NULL,
    old       text        NOT NULL DEFAULT '',
    new       text        NOT NULL DEFAULT '',
    related   text        NOT NULL DEFAULT '',
    actor     text        NOT NULL DEFAULT '',
    reason    text        NOT NULL DEFAULT '',
    at        timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS history_memory ON history (tenant, memory_id, seq);

CREATE TABLE IF NOT EXISTS entities (
    tenant   text   NOT NULL,
    id       text   NOT NULL,
    key      text   NOT NULL,
    name     text   NOT NULL,
    type     text   NOT NULL DEFAULT '',
    mentions bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant, key)
);
CREATE UNIQUE INDEX IF NOT EXISTS entities_id ON entities (tenant, id);
CREATE INDEX IF NOT EXISTS entities_mentions ON entities (tenant, mentions DESC);
CREATE INDEX IF NOT EXISTS entities_prefix ON entities (tenant, key text_pattern_ops);

CREATE TABLE IF NOT EXISTS entity_aliases (
    tenant     text NOT NULL,
    alias      text NOT NULL,
    entity_key text NOT NULL,
    name       text NOT NULL,
    PRIMARY KEY (tenant, alias)
);

CREATE TABLE IF NOT EXISTS memory_entities (
    tenant    text NOT NULL,
    entity_id text NOT NULL,
    memory_id text NOT NULL,
    PRIMARY KEY (tenant, entity_id, memory_id)
);
CREATE INDEX IF NOT EXISTS memory_entities_memory ON memory_entities (tenant, memory_id);

CREATE TABLE IF NOT EXISTS runs (
    tenant        text        NOT NULL,
    run_id        text        NOT NULL,
    user_id       text        NOT NULL DEFAULT '',
    agent_id      text        NOT NULL DEFAULT '',
    last_seq      bigint      NOT NULL DEFAULT 0,
    processed_seq bigint      NOT NULL DEFAULT 0,
    summary       text        NOT NULL DEFAULT '',
    summary_upto  bigint      NOT NULL DEFAULT 0,
    updated_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant, run_id)
);

CREATE TABLE IF NOT EXISTS episodes (
    tenant        text        NOT NULL,
    id            text        NOT NULL,
    run_id        text        NOT NULL,
    seq           bigint      NOT NULL,
    client_msg_id text        NOT NULL DEFAULT '',
    user_id       text        NOT NULL DEFAULT '',
    agent_id      text        NOT NULL DEFAULT '',
    role          text        NOT NULL,
    actor         text        NOT NULL DEFAULT '',
    occurred_at   timestamptz NOT NULL,
    recorded_at   timestamptz NOT NULL,
    status        text        NOT NULL,
    parts         json        NOT NULL, -- json (not jsonb) keeps tool payloads byte-exact
    metadata      jsonb       NOT NULL DEFAULT '{}',
    text          text        NOT NULL DEFAULT '',
    blobs         text[]      NOT NULL DEFAULT '{}',
    fts_scope     text        NOT NULL DEFAULT '', -- scope tokens, see sqlgen.EpisodeTokens
    tsv           tsvector GENERATED ALWAYS AS (to_tsvector('simple', text || ' ' || fts_scope)) STORED,
    PRIMARY KEY (tenant, id),
    UNIQUE (tenant, run_id, seq)
);
CREATE UNIQUE INDEX IF NOT EXISTS episodes_client ON episodes (tenant, run_id, client_msg_id) WHERE client_msg_id <> '';
CREATE INDEX IF NOT EXISTS episodes_occurred ON episodes (tenant, occurred_at);
CREATE INDEX IF NOT EXISTS episodes_tsv ON episodes USING gin (tsv);
CREATE INDEX IF NOT EXISTS episodes_blobs ON episodes USING gin (blobs);

CREATE TABLE IF NOT EXISTS embeddings (
    tenant      text   NOT NULL,
    model       text   NOT NULL,
    target_key  text   NOT NULL,
    target_kind text   NOT NULL,
    target_id   text   NOT NULL,
    part        int    NOT NULL DEFAULT 0,
    modality    text   NOT NULL DEFAULT '',
    user_id     text   NOT NULL DEFAULT '',
    agent_id    text   NOT NULL DEFAULT '',
    run_id      text   NOT NULL DEFAULT '',
    dims        int    NOT NULL,
    vec         float4[] NOT NULL,
    PRIMARY KEY (tenant, model, target_key)
);
CREATE INDEX IF NOT EXISTS embeddings_target ON embeddings (tenant, target_id);
CREATE INDEX IF NOT EXISTS embeddings_scope ON embeddings (tenant, model, user_id);

CREATE TABLE IF NOT EXISTS jobs (
    id          text        PRIMARY KEY,
    tenant      text        NOT NULL,
    kind        text        NOT NULL,
    key         text        NOT NULL DEFAULT '',
    payload     bytea,
    state       text        NOT NULL,
    attempts    int         NOT NULL DEFAULT 0,
    priority    int         NOT NULL DEFAULT 0 CHECK (priority BETWEEN -100 AND 100),
    not_before  timestamptz NOT NULL,
    lease_until timestamptz,
    worker      text        NOT NULL DEFAULT '',
    checkpoint  bytea,
    last_error  text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS jobs_pending_key ON jobs (tenant, kind, key) WHERE state = 'pending' AND key <> '';
CREATE INDEX IF NOT EXISTS jobs_due ON jobs (state, not_before);
CREATE INDEX IF NOT EXISTS jobs_key ON jobs (tenant, kind, key);
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS priority int NOT NULL DEFAULT 0 CHECK (priority BETWEEN -100 AND 100);
CREATE INDEX IF NOT EXISTS jobs_priority_due ON jobs (state, priority DESC, not_before, tenant);
CREATE INDEX IF NOT EXISTS jobs_tenant_state_id ON jobs (tenant, state, id);
CREATE INDEX IF NOT EXISTS jobs_tenant_id ON jobs (tenant, id);

CREATE TABLE IF NOT EXISTS settings (
    tenant text  PRIMARY KEY,
    doc    bytea NOT NULL
);
CREATE TABLE IF NOT EXISTS relationship_sets (
    tenant TEXT NOT NULL,
    anchor TEXT NOT NULL,
    data JSONB NOT NULL,
    PRIMARY KEY (tenant, anchor)
);
ALTER TABLE memories ADD COLUMN IF NOT EXISTS closed_at timestamptz;
CREATE TABLE IF NOT EXISTS operation_receipts (
    tenant TEXT NOT NULL, key TEXT NOT NULL, hash TEXT NOT NULL, receipt JSONB NOT NULL,
    PRIMARY KEY (tenant, key)
);
CREATE TABLE IF NOT EXISTS knowledge_relations (
    tenant TEXT NOT NULL, id TEXT NOT NULL, source TEXT NOT NULL, target TEXT NOT NULL, data JSONB NOT NULL,
    PRIMARY KEY (tenant, id)
);
CREATE INDEX IF NOT EXISTS knowledge_relations_source ON knowledge_relations(tenant, source);
CREATE INDEX IF NOT EXISTS knowledge_relations_target ON knowledge_relations(tenant, target);
