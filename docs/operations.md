# Jobs and destructive cleanup

## Job controls

Open **Operations -> Jobs**. The list refreshes every five seconds and loads
100 jobs per page using an exclusive ID cursor, not an increasing SQL offset.
Document/media import and source-chunk jobs on that page appear in one
expandable card per original file. Expand **Processing stages** for per-job
controls, errors and checkpoints. Cards/counts reflect the current queue
page/filter, not an exhaustive pipeline history; completed jobs leave the queue.
Shared model-wide embedding/indexing work stays separate rather than being
incorrectly assigned to one file.

**Preview file** loads the authenticated original on demand, in both Jobs and
Remember import cards. Images, audio, video, PDF and plain-text/Markdown previews
are supported where the browser supports their codecs/PDF viewer. Markdown is
displayed as plain text, never executed HTML. Text previews are capped at
128 KiB. DOCX and unsupported formats retain **Download original**. Preview URLs
are released on close/navigation; no model inference is triggered by previewing.

| State | Meaning / controls |
|---|---|
| pending | Waiting for due time and worker capacity. Pause, cancel, delete, set priority. |
| leased | A worker holds the job. Pause/cancel revokes its lease. |
| paused | Not scheduled. Resume preserves checkpoint/attempt history. |
| cancelled | Not scheduled. Retry starts another attempt, or delete the record. |
| dead | Failed permanently or exhausted retries. Inspect error, then retry or delete. |

Retry updates the **same ID**, resets attempts/checkpoint/error, and schedules it
now. Resume/retry reports a conflict if another pending job already owns the same
tenant/kind/key; it never silently duplicates or discards work. Priority is an
integer from -100 to 100. Higher values run first within a tenant; tenant fairness
still applies. Priority does not preempt running work. Deleting a job does not
delete memories it already created, and an external producer may enqueue new work.

Pause/cancel interrupts a running handler on its next heartbeat (normally within
ten seconds). It cannot undo committed writes or stop custom code that ignores
its context. A revoked worker cannot finish or overwrite the administrative
state. Paused jobs never resume automatically.
Expected pause/cancel/deletion revocations are logged as informational events,
not failed completions. A worker stops without finishing after heartbeat failure;
real lease/storage failures still produce warnings. Dead/retry logs are emitted
only after that outcome has actually been persisted.

An inline worker is enabled by `worker.inline` in the server configuration.
Otherwise run `niromem worker` with the same configuration, or **Run worker once**.
Presence in a list is not proof of progress: inspect due time, lease, last update,
attempts and last error. CPU inference can spend minutes loading/waiting for a
model. The Qwen/LFM examples allow `knowledge.timeout_ms: 600000`; restart server
and workers to apply the new deadline, then retry failed jobs. More workers can
make contention worse on small machines.

Admin-only development endpoints:

- `GET /_dev/api/jobs?state=pending&after_id=ID`
- `POST /_dev/api/jobs/control` with `{"id":"ID","action":"pause"}`
- Actions: `pause`, `resume`, `retry`, `cancel`, `delete`, `priority`
- For priority, include `"priority": 50`.

Controls always apply to the authenticated tenant. Switch tenants before
controlling an item displayed by **all tenants**.

## Delete everything

In **Memories -> Browse -> Danger zone: delete everything**, type the exact tenant
name and confirm the final warning. This is an irreversible **entire-tenant**
operation, not deletion of the current slice, project or filtered results.

It deletes memories (including historical/expired versions), history, episodes,
runs, original files (including orphan files), entities, relationships, embeddings,
external vector points, jobs, operation receipts and tenant settings. Other
tenants, authentication/server configuration and backups are not affected.
S3 version history, object-lock retention and database free pages/backups are not
securely erased. Use the storage administrator's lifecycle/secure-erasure policies
for those. A small durable purge-control receipt is retained for retry/progress.

Stop external producers and let their in-flight requests finish before starting.
The store fences tenant writes,
ordinary jobs stop being claimed and running leases are revoked. Cleanup waits
for revoked leases to expire, then runs bounded transactions (at most 500 rows
per table batch and 256 files per blob batch), yielding worker capacity between
bounded passes without exhausting retries. Progress survives process restarts.
Do not terminate the worker expecting that to undo deletion.

Closing the browser does not cancel cleanup. **Refresh cleanup status** shows its
progress; inspect **Jobs** for errors. Failed cleanup keeps writes locked; fix the
storage error and retry its `tenant.purge` job. That irreversible job cannot be
paused/cancelled/deleted. After completion the empty tenant accepts writes again.

Admin API:

```http
POST /v1/maintenance/purge
Content-Type: application/json

{"confirmation":"my-tenant","request_id":"a-unique-operation-id"}
```

Returns HTTP 202. Retry an uncertain request with the same request ID.
`GET /v1/maintenance/purge` returns durable progress for the authenticated tenant.
Unsupported custom blob/vector/store connectors reject cleanup before it starts.

## Large datasets

The browser never downloads the entire dataset to delete it. Memory browsing
uses `before_id` keyset pagination (newest-first); jobs use `after_id`. Legacy
offset APIs remain compatible, but the development browser does not use deep
offsets. Graphs remain bounded visualizations of the current slice.

These are scalable access patterns, **not a certification of billion-item
performance** or Redis-style instantaneous `FLUSHDB`. Use PostgreSQL plus an
appropriately sized/indexed vector backend for large installations. SQLite and
memstore are local/single-node options. Benchmark your cardinality, tenant
distribution, cleanup throughput and storage reclamation before production use.
