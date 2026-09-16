-- v2mem 记忆库 schema
-- 关键：content_idx 是「可检索形态」。SQLite FTS5 自带分词器均不适用于中文——
--   unicode61 把整串汉字当成一个 token；trigram 有 3 字符下限，二字词（目录/路径）查不到。
-- 因此由 Go 侧把连续汉字切成一元组+二元组写入 content_idx，FTS5 只对它建索引。

CREATE TABLE IF NOT EXISTS memories (
  id            TEXT PRIMARY KEY,
  content       TEXT NOT NULL,
  content_idx   TEXT NOT NULL DEFAULT '',
  kind          TEXT NOT NULL DEFAULT 'fact',
  content_hash  TEXT NOT NULL,
  project       TEXT NOT NULL DEFAULT '',
  salience      REAL NOT NULL DEFAULT 0.5,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  last_seen_at  INTEGER NOT NULL,
  expires_at    INTEGER,
  superseded_by TEXT,
  origin_device TEXT NOT NULL,
  origin_tool   TEXT NOT NULL DEFAULT '',
  access_count  INTEGER NOT NULL DEFAULT 0
);

CREATE UNIQUE INDEX IF NOT EXISTS ux_mem_hash ON memories(content_hash, project);
CREATE INDEX IF NOT EXISTS ix_mem_project ON memories(project);
CREATE INDEX IF NOT EXISTS ix_mem_seen   ON memories(last_seen_at);

CREATE TABLE IF NOT EXISTS tags (
  memory_id TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
  key       TEXT NOT NULL,
  value     TEXT NOT NULL,
  PRIMARY KEY (memory_id, key, value)
);
CREATE INDEX IF NOT EXISTS ix_tags_kv ON tags(key, value);

CREATE TABLE IF NOT EXISTS edges (
  src        TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
  dst        TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
  rel        TEXT NOT NULL,
  valid_from INTEGER,
  valid_to   INTEGER,
  PRIMARY KEY (src, dst, rel)
);

CREATE VIRTUAL TABLE IF NOT EXISTS fts_mem USING fts5(
  content_idx,
  content = 'memories',
  content_rowid = 'rowid'
);

CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
  INSERT INTO fts_mem(rowid, content_idx) VALUES (new.rowid, new.content_idx);
END;

CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
  INSERT INTO fts_mem(fts_mem, rowid, content_idx) VALUES ('delete', old.rowid, old.content_idx);
END;

CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN
  INSERT INTO fts_mem(fts_mem, rowid, content_idx) VALUES ('delete', old.rowid, old.content_idx);
  INSERT INTO fts_mem(rowid, content_idx) VALUES (new.rowid, new.content_idx);
END;
