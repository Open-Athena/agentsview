# `export-snapshot`: include the `message_search_content` view in `schema.sql`

Found by the transcripts hub (`~/c/oa/transcripts`) loading a disky snapshot exported at `3f4753055`.

## Problem

Upstream's FTS schema (`internal/db/db.go` `schemaFTS`) now builds `messages_fts` as an external-content table over a view:

```sql
CREATE VIEW IF NOT EXISTS message_search_content AS SELECT id, session_id, content AS rendered, COALESCE(dialogue_text, content) AS content FROM messages;
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(content, rendered, content='message_search_content', content_rowid='id', tokenize='porter unicode61');
```

`internal/snapshot/export.go` `writeSchema` copies `baseTables` + `ftsObjects` DDL from `sqlite_master`. `ftsObjects` is `messages_fts`, `messages_ai`, `messages_ad`, `messages_au`, so it lacks the view. The exported `schema.sql` creates `messages_fts` pointing at a table that doesn't exist. Inserts still work, because the triggers write the FTS index directly. Any read that needs the content table fails with `no such table: main.message_search_content`, including `snippet()`, `SELECT … FROM messages_fts` and `count(*)`. That breaks snapshot search in the SPA and the hub Worker (`worker/snapshot.ts` uses `snippet(messages_fts, …)`).

Older snapshots, such as trainium's, used `content='messages'` and are unaffected.

## Fix

Add `"message_search_content"` to `ftsObjects`, ahead of `messages_fts`. `writeSchema` already skips absent objects, so sources with older schemas still export.

## Test

An export-snapshot test that loads the produced `schema.sql` plus data into a fresh SQLite DB, then runs the exact query the snapshot server uses (`snippet(messages_fts, …)` with `MATCH`). Assert the exact result rows.

## Hub workaround (until this lands)

`~/c/oa/transcripts/scripts/load.py` (`FTS_VIEW`) creates `CREATE VIEW IF NOT EXISTS message_search_content …` after loading. Drop it there once exports include the view.

## Status

Done. `ftsObjects` now leads with `message_search_content`, and `TestExportedSchemaSupportsSnippetSearch` (`internal/snapshot/fts_test.go`) loads an export into a fresh DB and runs the snapshot server's `snippet(messages_fts, …)` `MATCH` query. Before the fix it failed with `SQL logic error`. `TestExportProjectScope` now expects the view in `schema.sql`. The hub's `FTS_VIEW` workaround can go once its snapshots are re-exported.
