CREATE TABLE preview_tasks (
 id TEXT PRIMARY KEY,
 provider TEXT NOT NULL,
 account_id INTEGER NOT NULL DEFAULT 0,
 kind TEXT NOT NULL CHECK (kind IN ('image','video')),
 model TEXT NOT NULL,
 prompt TEXT NOT NULL DEFAULT '',
 vendor_id TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL,
 media_url TEXT NOT NULL DEFAULT '',
 media_type TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL
);
CREATE INDEX preview_tasks_created ON preview_tasks(created_at DESC);
