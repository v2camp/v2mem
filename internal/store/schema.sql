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
  source        TEXT NOT NULL DEFAULT '',
  provenance    TEXT,
  access_count  INTEGER NOT NULL DEFAULT 0
);

-- 既有库（在此列加入前创建）没有 source 列：Open 时用 ensureColumn 幂等补上；
-- 对应索引 ix_mem_source 在 ensureColumn 里补列成功后一并创建。
-- provenance 为后加列：ensureColumn 幂等 ALTER，存量行保持 NULL（无溯源）。

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

-- 钩子事件的幂等去重。
--
-- 要解决什么：同一个事件可能被**多处配置**触发（如 TraeWork 的项目级 .trae/hooks.json
-- 与全局 ~/.trae-cn/hooks.json 都定义了 SessionStart / UserPromptSubmit），
-- 按宿主文档的合并语义「同类事件所有匹配 hooks 并行执行」⇒ mem hook 会跑两次、
-- 同样的内容注入两遍。
--
-- 做法：用一个原子声明把「同一事件只让一个调用者注入」变成数据库级保证。
-- 并发下两个进程同时插入时，只有先到的那次能改变行数，另一次被 WHERE 挡掉。
CREATE TABLE IF NOT EXISTS hook_dedup (
  key TEXT PRIMARY KEY,
  ts  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_hook_dedup_ts ON hook_dedup(ts);

-- 观测缓冲与蒸馏隔离区（同库另表，不进 fts_mem / mem search）。
-- hooks 只许写 observations；candidates / distill_runs 本波只建表，不接 distill/accept。
-- 写失败不在这里处理：调用方把行落到 spool，下次 SessionStart 再灌回来。
CREATE TABLE IF NOT EXISTS observations (
  id            TEXT PRIMARY KEY,
  session_id    TEXT NOT NULL,
  project       TEXT NOT NULL DEFAULT '',
  harness       TEXT NOT NULL DEFAULT '',
  event         TEXT NOT NULL,            -- post-tool-use|stop|session-end|…
  tool_name     TEXT NOT NULL DEFAULT '',
  summary       TEXT NOT NULL DEFAULT '', -- 有界摘要，非全量 stdout
  raw_ref       TEXT NOT NULL DEFAULT '', -- 可选外部路径，勿存密钥
  content_hash  TEXT NOT NULL,
  created_at    INTEGER NOT NULL,
  meta_json     TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS ix_obs_session ON observations(session_id, created_at);
CREATE INDEX IF NOT EXISTS ix_obs_project ON observations(project, created_at);
CREATE UNIQUE INDEX IF NOT EXISTS ux_obs_dedup ON observations(session_id, content_hash);

CREATE TABLE IF NOT EXISTS candidates (
  id            TEXT PRIMARY KEY,
  session_id    TEXT NOT NULL DEFAULT '',
  project       TEXT NOT NULL DEFAULT '',
  kind          TEXT NOT NULL,
  content       TEXT NOT NULL,
  content_hash  TEXT NOT NULL,
  status        TEXT NOT NULL,            -- pending|accepted|rejected|expired
  source        TEXT NOT NULL,            -- distill|dream|import-proposal
  salience      REAL NOT NULL DEFAULT 0.5,
  obs_ids_json  TEXT NOT NULL DEFAULT '[]',
  created_at    INTEGER NOT NULL,
  decided_at    INTEGER,
  memory_id     TEXT                       -- accept 后回填；本波无写入路径
);
CREATE INDEX IF NOT EXISTS ix_cand_status ON candidates(status, created_at);

CREATE TABLE IF NOT EXISTS distill_runs (
  id            TEXT PRIMARY KEY,
  session_id    TEXT NOT NULL,
  provider      TEXT NOT NULL,
  status        TEXT NOT NULL,            -- ok|fail|skipped
  error         TEXT NOT NULL DEFAULT '',
  cand_count    INTEGER NOT NULL DEFAULT 0,
  started_at    INTEGER NOT NULL,
  finished_at   INTEGER NOT NULL,
  UNIQUE(session_id, started_at)
);

-- M5b-0 模糊检索：每条记忆的 MinHash 签名（512B 小端 uint64 序列）。
-- 与记忆同写同删（外键级联）；旧库缺签名的行由 Open 时一次性补齐（backfillSigs），
-- 覆盖 import 直插等绕开 Add 的写入路径。
CREATE TABLE IF NOT EXISTS mem_sigs (
  memory_id TEXT PRIMARY KEY REFERENCES memories(id) ON DELETE CASCADE,
  sig       BLOB NOT NULL
);
