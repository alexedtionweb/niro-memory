-- niro-agent-memory SQLite schema, version 1.
-- Times are unix milliseconds (INTEGER). JSON columns hold arrays/objects.

CREATE TABLE IF NOT EXISTS tenant_purges (
    tenant TEXT PRIMARY KEY, id TEXT NOT NULL, state TEXT NOT NULL,
    phase INTEGER NOT NULL, deleted INTEGER NOT NULL, ready_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tenant_purge_requests (
    tenant TEXT NOT NULL, request_id TEXT NOT NULL, purge_id TEXT NOT NULL,
    PRIMARY KEY (tenant, request_id)
);

CREATE TABLE IF NOT EXISTS memories (
    tenant         TEXT    NOT NULL,
    id             TEXT    NOT NULL,
    user_id        TEXT    NOT NULL DEFAULT '',
    agent_id       TEXT    NOT NULL DEFAULT '',
    run_id         TEXT    NOT NULL DEFAULT '',
    kind           TEXT    NOT NULL,
    layer          INTEGER NOT NULL,
    title          TEXT    NOT NULL DEFAULT '',
    text           TEXT    NOT NULL,
    subject        TEXT    NOT NULL DEFAULT '',
    predicate      TEXT    NOT NULL DEFAULT '',
    object         TEXT    NOT NULL DEFAULT '',
    slot           TEXT    NOT NULL DEFAULT '',
    hash           TEXT    NOT NULL DEFAULT '',
    tags           TEXT    NOT NULL DEFAULT '[]',
    metadata       TEXT    NOT NULL DEFAULT '{}',
    confidence     REAL    NOT NULL DEFAULT 0,
    importance     REAL    NOT NULL DEFAULT 0,
    heat           INTEGER NOT NULL DEFAULT 0,
    visibility     TEXT    NOT NULL,
    grants         TEXT    NOT NULL DEFAULT '[]',
    valid_from     INTEGER NOT NULL,
    valid_to       INTEGER,
    recorded_at    INTEGER NOT NULL,
    invalidated_at INTEGER,
    expires_at     INTEGER,
    version        INTEGER NOT NULL DEFAULT 1,
    supersedes     TEXT    NOT NULL DEFAULT '',
    src_kind       TEXT    NOT NULL DEFAULT '',
    actor          TEXT    NOT NULL DEFAULT '',
    role           TEXT    NOT NULL DEFAULT '',
    trust          TEXT    NOT NULL DEFAULT '',
    derived_by     TEXT    NOT NULL DEFAULT '',
    source         TEXT    NOT NULL DEFAULT '{}',
    fts_scope      TEXT    NOT NULL DEFAULT '', -- scope tokens, see sqlgen.MemoryTokens
    UNIQUE (tenant, id)
);
-- Newest-first listing inside a tenant narrowed by owner (UNIQUE covers tenant,id).
CREATE INDEX IF NOT EXISTS memories_owner ON memories (tenant, user_id, agent_id, id);
CREATE INDEX IF NOT EXISTS memories_hash ON memories (tenant, hash);
CREATE INDEX IF NOT EXISTS memories_enrichment_identity ON memories
    (tenant, json_extract(metadata, '$."enrichment_identity"'));
-- At most one open value per slot and owner.
CREATE UNIQUE INDEX IF NOT EXISTS memories_slot ON memories (tenant, user_id, agent_id, slot)
    WHERE slot <> '' AND valid_to IS NULL AND invalidated_at IS NULL;

CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5 (
    title, text, fts_scope, content = 'memories', content_rowid = 'rowid',
    tokenize = 'unicode61 remove_diacritics 0'
);
-- Term dictionary with document counts: prefix expansion and IDF for the
-- BM25 computed in Go (see sqlite.go, rankFTS).
CREATE VIRTUAL TABLE IF NOT EXISTS memories_vocab USING fts5vocab (memories_fts, 'row');
CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
    INSERT INTO memories_fts (rowid, title, text, fts_scope) VALUES (new.rowid, new.title, new.text, new.fts_scope);
END;
CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
    INSERT INTO memories_fts (memories_fts, rowid, title, text, fts_scope) VALUES ('delete', old.rowid, old.title, old.text, old.fts_scope);
END;

CREATE TABLE IF NOT EXISTS history (
    tenant    TEXT    NOT NULL,
    memory_id TEXT    NOT NULL,
    action    TEXT    NOT NULL,
    old       TEXT    NOT NULL DEFAULT '',
    new       TEXT    NOT NULL DEFAULT '',
    related   TEXT    NOT NULL DEFAULT '',
    actor     TEXT    NOT NULL DEFAULT '',
    reason    TEXT    NOT NULL DEFAULT '',
    at        INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS history_memory ON history (tenant, memory_id);

CREATE TABLE IF NOT EXISTS entities (
    tenant   TEXT    NOT NULL,
    id       TEXT    NOT NULL,
    key      TEXT    NOT NULL,
    name     TEXT    NOT NULL,
    type     TEXT    NOT NULL DEFAULT '',
    mentions INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant, key)
) WITHOUT ROWID;
CREATE UNIQUE INDEX IF NOT EXISTS entities_id ON entities (tenant, id);
CREATE INDEX IF NOT EXISTS entities_mentions ON entities (tenant, mentions DESC);

CREATE TABLE IF NOT EXISTS entity_aliases (
    tenant     TEXT NOT NULL,
    alias      TEXT NOT NULL,
    entity_key TEXT NOT NULL,
    name       TEXT NOT NULL,
    PRIMARY KEY (tenant, alias)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS entity_aliases_key ON entity_aliases (tenant, entity_key);

CREATE TABLE IF NOT EXISTS memory_entities (
    tenant    TEXT NOT NULL,
    entity_id TEXT NOT NULL,
    memory_id TEXT NOT NULL,
    PRIMARY KEY (tenant, entity_id, memory_id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS memory_entities_memory ON memory_entities (tenant, memory_id);

CREATE TABLE IF NOT EXISTS runs (
    tenant        TEXT    NOT NULL,
    run_id        TEXT    NOT NULL,
    user_id       TEXT    NOT NULL DEFAULT '',
    agent_id      TEXT    NOT NULL DEFAULT '',
    last_seq      INTEGER NOT NULL DEFAULT 0,
    processed_seq INTEGER NOT NULL DEFAULT 0,
    summary       TEXT    NOT NULL DEFAULT '',
    summary_upto  INTEGER NOT NULL DEFAULT 0,
    updated_at    INTEGER NOT NULL,
    PRIMARY KEY (tenant, run_id)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS episodes (
    tenant        TEXT    NOT NULL,
    id            TEXT    NOT NULL,
    run_id        TEXT    NOT NULL,
    seq           INTEGER NOT NULL,
    client_msg_id TEXT    NOT NULL DEFAULT '',
    user_id       TEXT    NOT NULL DEFAULT '',
    agent_id      TEXT    NOT NULL DEFAULT '',
    role          TEXT    NOT NULL,
    actor         TEXT    NOT NULL DEFAULT '',
    occurred_at   INTEGER NOT NULL,
    recorded_at   INTEGER NOT NULL,
    status        TEXT    NOT NULL,
    parts         TEXT    NOT NULL,
    metadata      TEXT    NOT NULL DEFAULT '{}',
    text          TEXT    NOT NULL DEFAULT '',
    blobs         TEXT    NOT NULL DEFAULT '',
    fts_scope     TEXT    NOT NULL DEFAULT '', -- scope tokens, see sqlgen.EpisodeTokens
    UNIQUE (tenant, id),
    UNIQUE (tenant, run_id, seq)
);
CREATE UNIQUE INDEX IF NOT EXISTS episodes_client ON episodes (tenant, run_id, client_msg_id) WHERE client_msg_id <> '';
CREATE INDEX IF NOT EXISTS episodes_occurred ON episodes (tenant, occurred_at);
CREATE VIRTUAL TABLE IF NOT EXISTS episodes_fts USING fts5 (
    text, fts_scope, content = 'episodes', content_rowid = 'rowid',
    tokenize = 'unicode61 remove_diacritics 0'
);
CREATE VIRTUAL TABLE IF NOT EXISTS episodes_vocab USING fts5vocab (episodes_fts, 'row');
CREATE TRIGGER IF NOT EXISTS episodes_ai AFTER INSERT ON episodes BEGIN
    INSERT INTO episodes_fts (rowid, text, fts_scope) VALUES (new.rowid, new.text, new.fts_scope);
END;
CREATE TRIGGER IF NOT EXISTS episodes_ad AFTER DELETE ON episodes BEGIN
    INSERT INTO episodes_fts (episodes_fts, rowid, text, fts_scope) VALUES ('delete', old.rowid, old.text, old.fts_scope);
END;
CREATE TRIGGER IF NOT EXISTS episodes_au AFTER UPDATE OF text ON episodes BEGIN
    INSERT INTO episodes_fts (episodes_fts, rowid, text, fts_scope) VALUES ('delete', old.rowid, old.text, old.fts_scope);
    INSERT INTO episodes_fts (rowid, text, fts_scope) VALUES (new.rowid, new.text, new.fts_scope);
END;

CREATE TABLE IF NOT EXISTS embeddings (
    tenant      TEXT    NOT NULL,
    model       TEXT    NOT NULL,
    target_key  TEXT    NOT NULL,
    target_kind TEXT    NOT NULL,
    target_id   TEXT    NOT NULL,
    part        INTEGER NOT NULL DEFAULT 0,
    modality    TEXT    NOT NULL DEFAULT '',
    user_id     TEXT    NOT NULL DEFAULT '',
    agent_id    TEXT    NOT NULL DEFAULT '',
    run_id      TEXT    NOT NULL DEFAULT '',
    vec         BLOB    NOT NULL,
    PRIMARY KEY (tenant, model, target_key)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS embeddings_target ON embeddings (tenant, target_id);
CREATE INDEX IF NOT EXISTS embeddings_scope ON embeddings (tenant, model, user_id);

CREATE TABLE IF NOT EXISTS jobs (
    id          TEXT    NOT NULL PRIMARY KEY,
    tenant      TEXT    NOT NULL,
    kind        TEXT    NOT NULL,
    key         TEXT    NOT NULL DEFAULT '',
    payload     BLOB,
    state       TEXT    NOT NULL,
    attempts    INTEGER NOT NULL DEFAULT 0,
    priority    INTEGER NOT NULL DEFAULT 0 CHECK (priority BETWEEN -100 AND 100),
    not_before  INTEGER NOT NULL,
    lease_until INTEGER NOT NULL DEFAULT 0,
    worker      TEXT    NOT NULL DEFAULT '',
    checkpoint  BLOB,
    last_error  TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
) WITHOUT ROWID;
CREATE UNIQUE INDEX IF NOT EXISTS jobs_pending_key ON jobs (tenant, kind, key) WHERE state = 'pending' AND key <> '';
CREATE INDEX IF NOT EXISTS jobs_due ON jobs (state, not_before);
CREATE INDEX IF NOT EXISTS jobs_key ON jobs (tenant, kind, key);
CREATE INDEX IF NOT EXISTS jobs_tenant_state_id ON jobs (tenant, state, id);
CREATE INDEX IF NOT EXISTS jobs_tenant_id ON jobs (tenant, id);

CREATE TABLE IF NOT EXISTS settings (
    tenant TEXT NOT NULL PRIMARY KEY,
    doc    BLOB NOT NULL
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS relationship_sets (
    tenant TEXT NOT NULL,
    anchor TEXT NOT NULL,
    data TEXT NOT NULL,
    PRIMARY KEY (tenant, anchor)
);
CREATE TABLE IF NOT EXISTS operation_receipts (
    tenant TEXT NOT NULL, key TEXT NOT NULL, hash TEXT NOT NULL, receipt TEXT NOT NULL,
    PRIMARY KEY (tenant, key)
);
CREATE TABLE IF NOT EXISTS knowledge_relations (
    tenant TEXT NOT NULL, id TEXT NOT NULL, source TEXT NOT NULL, target TEXT NOT NULL, data TEXT NOT NULL,
    PRIMARY KEY (tenant, id)
);
CREATE INDEX IF NOT EXISTS knowledge_relations_source ON knowledge_relations(tenant, source);
CREATE INDEX IF NOT EXISTS knowledge_relations_target ON knowledge_relations(tenant, target);
