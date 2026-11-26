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
  channel       TEXT NOT NULL DEFAULT '', -- user|agent|tool|import；与 source 并存。空=迁移前旧行
  provenance    TEXT,
  access_count  INTEGER NOT NULL DEFAULT 0,
  scope         TEXT NOT NULL DEFAULT '', -- project|global|cross_project；空=legacy 未定
  adopted_count INTEGER NOT NULL DEFAULT 0,
  last_adopted_at INTEGER,
  -- 人工后审核（docs/design-data-lifecycle.md §6.4）：'' 未审核 | confirmed | corrected | withdrawn。
  -- withdrawn 必须退出检索（visibilityWhere），但行与证据保留：撤回不是删除。
  -- 直写来源（mem add 等）写入即 confirmed；distill-accept 一律 '' 等人工看。
  review_state  TEXT NOT NULL DEFAULT '',
  reviewed_at   INTEGER NOT NULL DEFAULT 0,
  reviewed_by   TEXT NOT NULL DEFAULT ''
);

-- 既有库（在此列加入前创建）没有 source 列：Open 时用 ensureColumn 幂等补上；
-- 对应索引 ix_mem_source 在 ensureColumn 里补列成功后一并创建。
-- provenance 为后加列：ensureColumn 幂等 ALTER，存量行保持 NULL（无溯源）。
-- review_state/reviewed_* 是 S10a 后加列（schema v3）：ensureReviewSchema 补列并**只回填一次**；
-- ix_mem_review_state 也由它建（旧库补列前建索引会先于 ALTER 失败）。

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

-- dst 不再外键到 memories：derived_from 的对端是 candidate id（或 obs id），
-- 不是记忆。src 仍级联删除。旧库由 Open 里的 relaxEdgeDstFK 拆掉 dst 外键。
CREATE TABLE IF NOT EXISTS edges (
  src        TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
  dst        TEXT NOT NULL,
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

-- 观测缓冲与蒸馏隔离区（同库另表，不进 fts_mem）。
-- hooks 只许写 observations。candidates 由 distill / import 写入，accept 才进 memories。
-- 写失败不在这里处理：调用方把行落到 spool，下次 SessionStart 再灌回来。
-- summary / summary_idx 在 P1 全外置后恒为空，正文走 summary_ref；两列保留只为兼容读取。
-- fts_obs 在 Open 里补齐：未迁移库仍是 external-content 表；迁移后改为独立表。
-- 默认 mem search 仍只查 memories；--layer observation|both 才查这张表。
CREATE TABLE IF NOT EXISTS observations (
  id            TEXT PRIMARY KEY,
  session_id    TEXT NOT NULL,
  project       TEXT NOT NULL DEFAULT '',
  harness       TEXT NOT NULL DEFAULT '',
  event         TEXT NOT NULL,            -- post-tool-use|stop|session-end|…
  tool_name     TEXT NOT NULL DEFAULT '',
  summary       TEXT NOT NULL DEFAULT '', -- P1 后为空；legacy 行仍可读
  summary_idx   TEXT NOT NULL DEFAULT '', -- P1 后为空；FTS 自带索引副本
  summary_ref   TEXT NOT NULL DEFAULT '', -- content/YYYY/MM/DD/observations.jsonl.gz#<seq>
  summary_hash  TEXT NOT NULL DEFAULT '', -- 规范 JSON 行的 sha256
  payload_bytes INTEGER NOT NULL DEFAULT 0,
  raw_ref       TEXT NOT NULL DEFAULT '', -- 可选外部路径，勿存密钥
  content_hash  TEXT NOT NULL,
  created_at    INTEGER NOT NULL,
  meta_json     TEXT NOT NULL DEFAULT '{}',
  channel       TEXT NOT NULL DEFAULT '', -- user|agent|tool|import
  event_id      TEXT NOT NULL DEFAULT '', -- 宿主事件号；空不参与幂等。唯一索引在 Open 里建
  event_uid     TEXT NOT NULL DEFAULT '', -- canonical 全局事件身份；空=legacy/无身份
  source_id     TEXT NOT NULL DEFAULT '', -- 证据源；空=legacy/无身份
  source_generation INTEGER NOT NULL DEFAULT 0,
  source_seq    INTEGER NOT NULL DEFAULT 0,
  occurred_at_ns INTEGER NOT NULL DEFAULT 0,
  archive_hash  TEXT NOT NULL DEFAULT '',
  raw_record_hash TEXT NOT NULL DEFAULT '',
  parser_version TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ix_obs_session ON observations(session_id, created_at);
CREATE INDEX IF NOT EXISTS ix_obs_project ON observations(project, created_at);
-- 外置分片的当日序号；不放 db_meta，避免破坏「db_meta 恒 3 行」的既有断言。
CREATE TABLE IF NOT EXISTS summary_seq (
  day TEXT PRIMARY KEY,
  seq INTEGER NOT NULL
);
-- contentless FTS 不存列值，用固定 rowid 映射 obs_id；VACUUM 重排 observations.rowid 也不影响。
CREATE TABLE IF NOT EXISTS obs_fts_map (
  rowid  INTEGER PRIMARY KEY,
  obs_id TEXT NOT NULL UNIQUE
);
-- ix_obs_archive（证据归档准出按 archive_hash 反查）不能写在这里：存量库还没有
-- archive_hash 列，CREATE INDEX 会先于 ensureColumn 执行而失败。它由
-- ensureEvidenceSchema 在补列之后建。

CREATE TABLE IF NOT EXISTS candidates (
  id            TEXT PRIMARY KEY,
  session_id    TEXT NOT NULL DEFAULT '',
  project       TEXT NOT NULL DEFAULT '',
  kind          TEXT NOT NULL,
  content       TEXT NOT NULL,
  content_hash  TEXT NOT NULL,
  status        TEXT NOT NULL,            -- pending|accepted|rejected|expired|failed|quarantined
  source        TEXT NOT NULL,            -- distill|dream|import-proposal
  channel       TEXT NOT NULL DEFAULT '', -- user|agent|tool|import；与 source 并存
  salience      REAL NOT NULL DEFAULT 0.5,
  obs_ids_json  TEXT NOT NULL DEFAULT '[]',
  created_at    INTEGER NOT NULL,
  decided_at    INTEGER,
  memory_id     TEXT,                      -- accept 后回填
  run_id        TEXT NOT NULL DEFAULT '',
  bundle_id     TEXT NOT NULL DEFAULT '',
  proposal_index INTEGER NOT NULL DEFAULT 0,
  scope         TEXT NOT NULL DEFAULT '',
  subject_key   TEXT NOT NULL DEFAULT '',
  task_state    TEXT NOT NULL DEFAULT '',
  rationale     TEXT NOT NULL DEFAULT '',
  review_class  TEXT NOT NULL DEFAULT '',
  revision      INTEGER NOT NULL DEFAULT 0,
  schema_version INTEGER NOT NULL DEFAULT 0,
  -- 事实的原始来源（非写候选的人）：user|assistant|空串（旧行）
  attributed_to TEXT NOT NULL DEFAULT '',
  -- 准入判定的痕迹（docs/design-admission.md）：verdict 只记判定，改 status 只能走人工裁决。
  -- judge_model 带 prompt 版本（ollama:qwen2.5@judge-v1），判定标准一改，历史才能分段看。
  judge_verdict TEXT NOT NULL DEFAULT '',
  judge_model   TEXT NOT NULL DEFAULT '',
  -- S10a：判定理由必须独立成列，覆盖 rationale 会把「提案为什么这么提」抹掉（§7 第 5 条）。
  judge_reason  TEXT NOT NULL DEFAULT '',
  -- 失败/隔离是候选级终态的两档（§2.1）：failed 可重试、quarantined 等人工显式重放。
  -- status_reason 记最后一次失败原因，attempt 记重试次数（两者都由 MarkCandidate* 维护）。
  status_reason TEXT NOT NULL DEFAULT '',
  attempt       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS ix_cand_status ON candidates(status, created_at);
-- 同一会话里相同内容的 pending/accepted 不双插。rejected 不占这个键，允许改写后再来。
CREATE UNIQUE INDEX IF NOT EXISTS ux_cand_hash
  ON candidates(session_id, content_hash)
  WHERE status IN ('pending', 'accepted');

CREATE TABLE IF NOT EXISTS distill_runs (
  id            TEXT PRIMARY KEY,
  session_id    TEXT NOT NULL,
  provider      TEXT NOT NULL,
  status        TEXT NOT NULL,            -- ok|empty|partial|fail|skipped（empty=合法 0 条）
  error         TEXT NOT NULL DEFAULT '',
  cand_count    INTEGER NOT NULL DEFAULT 0,
  started_at    INTEGER NOT NULL,
  finished_at   INTEGER NOT NULL,
  window_key    TEXT NOT NULL DEFAULT '', -- 旧蒸馏窗口键；新 Proposal 不再用它做游标
  bundle_id     TEXT NOT NULL DEFAULT '',
  executor      TEXT NOT NULL DEFAULT '', -- host|local|byok|none
  model         TEXT NOT NULL DEFAULT '',
  model_version TEXT NOT NULL DEFAULT '',
  prompt_version TEXT NOT NULL DEFAULT '',
  schema_version INTEGER NOT NULL DEFAULT 0,
  generated_count INTEGER NOT NULL DEFAULT 0,
  valid_count   INTEGER NOT NULL DEFAULT 0,
  invalid_count INTEGER NOT NULL DEFAULT 0,
  duplicate_count INTEGER NOT NULL DEFAULT 0,
  attempt       INTEGER NOT NULL DEFAULT 0,
  source_fingerprint TEXT NOT NULL DEFAULT ''
);
-- 不再建 UNIQUE(session_id, started_at)：同 session 同一秒多条运行账是合法形态
-- （maintain 一次追账处理多个窗口）。窗口幂等由部分索引 ux_distill_window 保护，
-- 该索引在 Open 里等 ensureColumn 补完 window_key 之后再建。
-- ux_distill_window 不放在这里：旧库的 distill_runs 还没有 window_key，
-- 索引要等 Open 里 ensureColumn 之后再建。

-- T1.0 Evidence 账本。只登记来源、归档段、连续 Bundle 与条目；
-- 真正的原始字节由 internal/evidence 写到本地 gzip segment。
CREATE TABLE IF NOT EXISTS evidence_sources (
  id           TEXT PRIMARY KEY,
  kind         TEXT NOT NULL DEFAULT 'hook', -- hook|transcript|import|replay
  locator      TEXT NOT NULL DEFAULT '',
  generation   INTEGER NOT NULL DEFAULT 1,
  status       TEXT NOT NULL DEFAULT 'active', -- active|rotated|deleted
  last_seq     INTEGER NOT NULL DEFAULT 0,
  cursor_seq   INTEGER NOT NULL DEFAULT 0,
  source_tip   TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS evidence_archives (
  archive_hash    TEXT PRIMARY KEY,
  source_id       TEXT NOT NULL REFERENCES evidence_sources(id),
  generation      INTEGER NOT NULL,
  from_seq        INTEGER NOT NULL,
  to_seq          INTEGER NOT NULL,
  raw_count       INTEGER NOT NULL,
  byte_size       INTEGER NOT NULL,
  path            TEXT NOT NULL,
  raw_hash_concat TEXT NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL,
  UNIQUE(source_id, generation, from_seq)
);
CREATE INDEX IF NOT EXISTS ix_evarch_source ON evidence_archives(source_id, generation, to_seq);

CREATE TABLE IF NOT EXISTS evidence_bundles (
  id                 TEXT PRIMARY KEY,
  source_id          TEXT NOT NULL REFERENCES evidence_sources(id),
  generation         INTEGER NOT NULL,
  from_seq_exclusive INTEGER NOT NULL,
  to_seq_inclusive   INTEGER NOT NULL,
  evidence_hash      TEXT NOT NULL,
  source_tip         TEXT NOT NULL DEFAULT '',
  status             TEXT NOT NULL DEFAULT 'pending', -- pending|leased|ok|noop|fail|backlog|quarantined
  attempt            INTEGER NOT NULL DEFAULT 0,
  not_before         INTEGER NOT NULL DEFAULT 0,
  lease_until        INTEGER NOT NULL DEFAULT 0,
  lease_owner        TEXT NOT NULL DEFAULT '',
  last_run_id        TEXT NOT NULL DEFAULT '',
  created_at         INTEGER NOT NULL,
  updated_at         INTEGER NOT NULL,
  UNIQUE(source_id, generation, from_seq_exclusive)
);
CREATE INDEX IF NOT EXISTS ix_evbundle_status ON evidence_bundles(status, not_before);
CREATE INDEX IF NOT EXISTS ix_evbundle_lease ON evidence_bundles(lease_until);

CREATE TABLE IF NOT EXISTS evidence_bundle_items (
  bundle_id   TEXT NOT NULL REFERENCES evidence_bundles(id) ON DELETE CASCADE,
  ordinal     INTEGER NOT NULL,
  evidence_id TEXT NOT NULL,
  event_uid   TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (bundle_id, ordinal),
  UNIQUE (bundle_id, evidence_id)
);
CREATE INDEX IF NOT EXISTS ix_evbi_evidence ON evidence_bundle_items(evidence_id);

-- S6c 序号洞台账：cursor 无法再等到的缺失区间。
-- 洞不是 Bundle（不会被 Claim/重试），只用于「缺失已跳过」的可审计记录与 --gaps 出口。
-- id 用 source+generation+from_seq 的稳定 hash，使同一洞在重复检测时天然幂等
-- （与 evidence_bundles 的 id 同策略），不依赖自增/时间。
CREATE TABLE IF NOT EXISTS evidence_gaps (
  id          TEXT PRIMARY KEY,
  source_id   TEXT NOT NULL REFERENCES evidence_sources(id),
  generation  INTEGER NOT NULL,
  from_seq    INTEGER NOT NULL,
  to_seq      INTEGER NOT NULL,
  detected_at INTEGER NOT NULL,
  UNIQUE(source_id, generation, from_seq)
);
CREATE INDEX IF NOT EXISTS ix_evgap_detected ON evidence_gaps(detected_at);

-- legacy 标记与证据物理隔离，避免被误读成伪造的 evidence 字段。
CREATE TABLE IF NOT EXISTS migration_marks (
  entity_type TEXT NOT NULL,
  entity_id   TEXT NOT NULL,
  mark        TEXT NOT NULL,
  marked_at   INTEGER NOT NULL,
  PRIMARY KEY (entity_type, entity_id)
);

-- 第二波共享表：T2/T3/T4 只写逻辑，不再各自 append schema。
CREATE TABLE IF NOT EXISTS candidate_evidence (
  candidate_id TEXT NOT NULL REFERENCES candidates(id) ON DELETE CASCADE,
  evidence_id  TEXT NOT NULL,
  event_uid    TEXT NOT NULL DEFAULT '',
  source_id    TEXT NOT NULL DEFAULT '',
  source_seq   INTEGER NOT NULL DEFAULT 0,
  archive_hash TEXT NOT NULL DEFAULT '',
  raw_record_hash TEXT NOT NULL DEFAULT '',
  evidence_role TEXT NOT NULL DEFAULT 'direct', -- direct|error|resolution|completion|context
  quote_hash   TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  PRIMARY KEY (candidate_id, evidence_id, evidence_role)
);
CREATE INDEX IF NOT EXISTS ix_cand_evidence_event ON candidate_evidence(event_uid);
-- S14：完成 Bundle 回收（mem gc --retention）的引用保护按 evidence_id 反查候选，
-- 没有它每个受检 item 都要全表扫 candidate_evidence。event_uid 索引不覆盖这一列。
CREATE INDEX IF NOT EXISTS ix_cand_evidence_evidence ON candidate_evidence(evidence_id);

CREATE TABLE IF NOT EXISTS commit_decisions (
  id             TEXT PRIMARY KEY,
  candidate_id   TEXT NOT NULL,
  revision       INTEGER NOT NULL DEFAULT 0,
  evidence_hash  TEXT NOT NULL DEFAULT '',
  action         TEXT NOT NULL, -- auto_commit|human_accept|reject|duplicate|conflict|backlog|defer|reopen
  actor          TEXT NOT NULL DEFAULT 'auto', -- auto|human|human-reviewed-llm|judge|model-auto
  reason_codes_json TEXT NOT NULL DEFAULT '[]',
  memory_id      TEXT NOT NULL DEFAULT '',
  -- model_tag = 做出这次裁决的 model@promptver（空串=非模型裁决）。
  -- 按版本回滚（§6.4）要能只撤某一套提示词的产物，缺了它就只能全撤。
  model_tag      TEXT NOT NULL DEFAULT '',
  -- created_memory = 这次裁决是否**新建**了记忆行（命中相同知识时为 0，删除就是误伤）。
  created_memory INTEGER NOT NULL DEFAULT 0,
  idempotency_key TEXT NOT NULL,
  created_at     INTEGER NOT NULL,
  UNIQUE(idempotency_key)
);
CREATE INDEX IF NOT EXISTS ix_commit_candidate ON commit_decisions(candidate_id, created_at);
-- S14（docs/design-storage-layering.md §9）：复核摘要按「每条记忆取最近一条
-- model-auto 裁决」，子查询以 (memory_id, actor) 定位、按 created_at 排序；
-- 没有它在数万条裁决账上逐行全表扫（实测 3.4s/次）。ix_commit_model 由
-- ensureReviewSchema 建，不要在这里重复。
CREATE INDEX IF NOT EXISTS ix_commit_memory_actor ON commit_decisions(memory_id, actor, created_at);
-- ix_commit_model 不写在这里：旧库还没有 model_tag 列，建在建表脚本里会先于
-- ensureReviewSchema 的 ALTER 失败。它由 ensureReviewSchema 在补列之后创建。

CREATE TABLE IF NOT EXISTS memory_evidence (
  memory_id      TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
  evidence_id    TEXT NOT NULL,
  event_uid      TEXT NOT NULL DEFAULT '',
  archive_hash   TEXT NOT NULL DEFAULT '',
  raw_locator    TEXT NOT NULL DEFAULT '',
  grade          TEXT NOT NULL DEFAULT 'C', -- A|B|C
  seq            INTEGER NOT NULL DEFAULT 0,
  occurred_at_ns INTEGER NOT NULL DEFAULT 0,
  created_at     INTEGER NOT NULL,
  PRIMARY KEY (memory_id, evidence_id)
);
CREATE INDEX IF NOT EXISTS ix_mem_evidence_event ON memory_evidence(event_uid);
-- S14：同 ix_cand_evidence_evidence，引用保护的另一半。
CREATE INDEX IF NOT EXISTS ix_mem_evidence_evidence ON memory_evidence(evidence_id);

CREATE TABLE IF NOT EXISTS read_traces (
  id            TEXT PRIMARY KEY,
  session_id    TEXT NOT NULL DEFAULT '',
  project       TEXT NOT NULL DEFAULT '',
  harness       TEXT NOT NULL DEFAULT '',
  query_gate    TEXT NOT NULL DEFAULT 'run', -- run|skip
  gate_reason   TEXT NOT NULL DEFAULT '',
  raw_query     TEXT NOT NULL DEFAULT '',
  focus_query   TEXT NOT NULL DEFAULT '',
  scope         TEXT NOT NULL DEFAULT '',
  target_limit  INTEGER NOT NULL DEFAULT 0,
  budget_chars  INTEGER NOT NULL DEFAULT 0,
  overfetch     INTEGER NOT NULL DEFAULT 0,
  raw_hits      INTEGER NOT NULL DEFAULT 0,
  qualified     INTEGER NOT NULL DEFAULT 0,
  injected      INTEGER NOT NULL DEFAULT 0,
  outcome       TEXT NOT NULL DEFAULT '', -- injected|abstained_no_hit|abstained_low_quality|skipped_gate|error
  elapsed_ms    INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_read_traces_created ON read_traces(created_at);
-- 每轮 prompt-submit 都要按会话取「开场注入过哪些规则」（SessionInjectedRuleIDs）。
-- 这条查询在钩子的关键路径上，没有索引就得全表扫 read_traces。
CREATE INDEX IF NOT EXISTS ix_read_traces_session ON read_traces(session_id);

CREATE TABLE IF NOT EXISTS read_trace_items (
  trace_id      TEXT NOT NULL REFERENCES read_traces(id) ON DELETE CASCADE,
  ordinal       INTEGER NOT NULL,
  memory_id     TEXT NOT NULL DEFAULT '',
  content_hash  TEXT NOT NULL DEFAULT '',
  signals_json  TEXT NOT NULL DEFAULT '{}',
  match_class   TEXT NOT NULL DEFAULT '',
  eligible      INTEGER NOT NULL DEFAULT 1,
  drop_reason   TEXT NOT NULL DEFAULT '',
  injection_rank INTEGER NOT NULL DEFAULT 0,
  provenance    TEXT NOT NULL DEFAULT '',
  feedback      TEXT NOT NULL DEFAULT '', -- adopted|negative|''
  feedback_reason TEXT NOT NULL DEFAULT '',
  feedback_at   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (trace_id, ordinal)
);
CREATE INDEX IF NOT EXISTS ix_read_trace_feedback ON read_trace_items(feedback, feedback_at);

-- M5b-0 模糊检索：每条记忆的 MinHash 签名（512B 小端 uint64 序列）。
-- 与记忆同写同删（外键级联）；旧库缺签名的行由 Open 时一次性补齐（backfillSigs），
-- 覆盖 import 直插等绕开 Add 的写入路径。
CREATE TABLE IF NOT EXISTS mem_sigs (
  memory_id TEXT PRIMARY KEY REFERENCES memories(id) ON DELETE CASCADE,
  sig       BLOB NOT NULL
);
