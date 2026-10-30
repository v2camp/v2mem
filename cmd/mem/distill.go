package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/audit"
	"github.com/v2camp/v2mem/internal/distill"
	"github.com/v2camp/v2mem/internal/scrub"
	"github.com/v2camp/v2mem/internal/store"
)

// 蒸馏只写 candidates 与 distill_runs。本文件没有 store.Add。
// 采纳在 cmdAccept，那是唯一把候选升格进 memories 的入口。

func cmdDistill(args []string) error {
	var c common
	fs := flag.NewFlagSet("distill", flag.ContinueOnError)
	c.register(fs)
	session := fs.String("session", "", "只蒸馏这个会话")
	since := fs.Duration("since", 0, "只看这段时间之内的观测，如 24h")
	provider := fs.String("provider", "auto", "auto|none|openai|ollama")
	dry := fs.Bool("dry-run", false, "只打印候选，不写库")
	strict := fs.Bool("strict", false, "失败时非零退出（默认 fail-open，退出码 0）")
	pending := fs.Bool("pending", false, "蒸馏所有尚未处理的会话")
	auditFile := fs.String("audit-file", "", "审计日志路径（默认 ~/.v2mem/audit.jsonl；- 关闭）")
	noScrub := fs.Bool("no-scrub", false, "关闭密钥打码（仅本地调试）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*pending && strings.TrimSpace(*session) == "" {
		return errors.New("需要 --session 或 --pending")
	}
	model, timeout := distill.LoadConfig(distill.ConfigPath())
	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	sessions := []string{strings.TrimSpace(*session)}
	if *pending {
		sessions, err = st.SessionsNeedingDistill(20)
		if err != nil {
			return err
		}
	}
	if len(sessions) == 0 || (len(sessions) == 1 && sessions[0] == "") {
		if c.json {
			return printJSON(map[string]any{"status": "skipped", "candidates": 0})
		}
		fmt.Println("没有需要蒸馏的会话")
		return nil
	}

	var failed error
	total := 0
	for _, sid := range sessions {
		out, n, err := distillOne(st, distillJob{
			Session: sid, Provider: *provider, Model: model, Timeout: timeout,
			Since: *since, Dry: *dry, Audit: *auditFile, DB: c.db, NoScrub: *noScrub,
		})
		total += n
		if err != nil && failed == nil {
			failed = err
		}
		if c.json {
			continue
		}
		fmt.Printf("会话 %s  provider=%s status=%s 候选 %d\n", sid, out.Provider, out.Status, n)
		if out.Error != "" {
			fmt.Fprintf(os.Stderr, "v2mem distill: %s\n", out.Error)
		}
		if *dry {
			for _, f := range out.Facts {
				fmt.Printf("  [%s] %s\n", f.Kind, f.Content)
			}
		}
	}
	if c.json {
		_ = printJSON(map[string]any{"candidates": total, "sessions": len(sessions)})
	}
	if failed != nil && *strict {
		return failed
	}
	return nil
}

type distillJob struct {
	Session  string
	Provider string
	Model    string
	Timeout  time.Duration
	Since    time.Duration
	Dry      bool
	Audit    string
	DB       string
	NoScrub  bool
}

func distillOne(st *store.Store, job distillJob) (distill.Outcome, int, error) {
	started := time.Now()
	var sinceUnix int64
	if job.Since > 0 {
		sinceUnix = time.Now().Add(-job.Since).Unix()
	}
	obs, err := st.ObservationsForDistill(job.Session, sinceUnix, 200)
	if err != nil {
		return distill.Outcome{}, 0, err
	}
	provider := job.Provider
	if provider == "" {
		provider = "auto"
	}
	if len(obs) == 0 {
		out := distill.Outcome{Provider: provider, Status: "skipped"}
		writeDistillAudit(job, out, 0, time.Since(started))
		return out, 0, nil
	}
	ids := make([]string, len(obs))
	project := ""
	in := make([]distill.Observation, len(obs))
	for i, o := range obs {
		ids[i] = o.ID
		if project == "" {
			project = o.Project
		}
		in[i] = distill.Observation{
			ID: o.ID, SessionID: o.SessionID, Project: o.Project,
			Summary: scrub.Apply(o.Summary, job.NoScrub),
		}
	}
	window := distill.WindowKey(ids)
	done, err := st.DistillWindowDone(job.Session, window)
	if err != nil {
		return distill.Outcome{}, 0, err
	}
	if done {
		out := distill.Outcome{Provider: provider, Status: "skipped"}
		return out, 0, nil
	}
	out := distill.Run(in, distill.Options{Provider: provider, Model: job.Model, Timeout: job.Timeout})
	if job.Dry {
		n := 0
		if out.Status == "ok" {
			n = len(out.Facts)
		}
		return out, n, nil
	}
	if out.Status != "ok" {
		_ = st.SaveDistillRun(store.DistillRun{
			SessionID: job.Session, Provider: out.Provider, Status: "fail",
			Error: out.Error, StartedAt: started.Unix(), FinishedAt: time.Now().Unix(),
		})
		writeDistillAudit(job, out, 0, time.Since(started))
		if out.Error == "" {
			out.Error = "蒸馏失败"
		}
		return out, 0, errors.New(out.Error)
	}
	n := 0
	for _, f := range out.Facts {
		res, err := st.InsertCandidate(store.CandidateInput{
			SessionID: job.Session, Project: project, Kind: f.Kind,
			Content: scrub.Apply(f.Content, job.NoScrub), Source: "distill", Channel: store.ChannelAgent,
			Salience: f.Salience, ObsIDs: f.ObsIDs,
		})
		if err != nil {
			out.Status = "fail"
			out.Error = err.Error()
			_ = st.SaveDistillRun(store.DistillRun{
				SessionID: job.Session, Provider: out.Provider, Status: "fail", Error: err.Error(),
			})
			writeDistillAudit(job, out, n, time.Since(started))
			return out, n, err
		}
		if res.Created {
			n++
		}
	}
	if err := st.SaveDistillRun(store.DistillRun{
		SessionID: job.Session, Provider: out.Provider, Status: "ok",
		CandCount: n, WindowKey: window,
		StartedAt: started.Unix(), FinishedAt: time.Now().Unix(),
	}); err != nil && !errors.Is(err, store.ErrAlreadyDistilled) {
		return out, n, err
	}
	writeDistillAudit(job, out, n, time.Since(started))
	return out, n, nil
}

func writeDistillAudit(job distillJob, out distill.Outcome, n int, d time.Duration) {
	if job.Audit == "-" {
		return
	}
	_ = audit.Append(auditPath(job.Audit), audit.Record{
		TS: time.Now().Unix(), Event: "distill", SessionID: job.Session,
		Provider: out.Provider, Mode: out.Status, Count: n, Source: "distill",
		MS: d.Milliseconds(), Empty: n == 0,
	})
}

func cmdAccept(args []string) error {
	var c common
	fs := flag.NewFlagSet("accept", flag.ContinueOnError)
	c.register(fs)
	all := fs.Bool("all-pending", false, "采纳当前筛选下的全部 pending")
	session := fs.String("session", "", "配合 --all-pending，只采纳这个会话")
	auditFile := fs.String("audit-file", "", "审计日志路径（默认 ~/.v2mem/audit.jsonl；- 关闭）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	var ids []string
	if *all {
		list, err := st.ListCandidates("pending", *session, 1000)
		if err != nil {
			return err
		}
		for _, cand := range list {
			ids = append(ids, cand.ID)
		}
		if len(ids) == 0 {
			fmt.Println("没有 pending 候选")
			return nil
		}
	} else {
		id := strings.TrimSpace(strings.Join(fs.Args(), " "))
		if id == "" {
			return errors.New("缺少 ID，用法: mem accept <id|前缀> 或 mem accept --all-pending [--session S]")
		}
		ids = []string{id}
	}

	var accepted []store.AcceptResult
	for _, id := range ids {
		res, err := st.AcceptCandidate(id)
		if err != nil {
			return err
		}
		accepted = append(accepted, *res)
		if *auditFile != "-" {
			mode := "create"
			if res.Already {
				mode = "overwrite"
			}
			_ = audit.Append(auditPath(*auditFile), audit.Record{
				TS: time.Now().Unix(), Event: "accept", SessionID: res.Candidate.SessionID,
				Project: res.Candidate.Project, Source: "distill-accept", Mode: mode,
				Count: 1, Hashes: []string{res.MemoryID}, Kinds: []string{res.Candidate.Kind},
			})
		}
	}
	if c.json {
		return printJSON(accepted)
	}
	for _, res := range accepted {
		fmt.Printf("已采纳 %s → 记忆 %s source=distill-accept\n", shortID(res.Candidate.ID), shortID(res.MemoryID))
	}
	return nil
}

func cmdReject(args []string) error {
	var c common
	fs := flag.NewFlagSet("reject", flag.ContinueOnError)
	c.register(fs)
	auditFile := fs.String("audit-file", "", "审计日志路径（默认 ~/.v2mem/audit.jsonl；- 关闭）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if id == "" {
		return errors.New("缺少 ID，用法: mem reject <id|前缀>")
	}
	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()
	cand, err := st.RejectCandidate(id)
	if err != nil {
		return err
	}
	if *auditFile != "-" {
		_ = audit.Append(auditPath(*auditFile), audit.Record{
			TS: time.Now().Unix(), Event: "reject", SessionID: cand.SessionID,
			Project: cand.Project, Source: "distill", Count: 1, Mode: "rejected",
		})
	}
	if c.json {
		return printJSON(cand)
	}
	fmt.Printf("已拒绝 %s\n", shortID(cand.ID))
	return nil
}

func cmdCandidates(args []string) error {
	if len(args) == 0 || args[0] != "ls" {
		return errors.New("用法: mem candidates ls [--status pending] [--session S]")
	}
	var c common
	fs := flag.NewFlagSet("candidates ls", flag.ContinueOnError)
	c.register(fs)
	status := fs.String("status", "pending", "pending|accepted|rejected|expired|空为全部")
	session := fs.String("session", "", "限定会话")
	limit := fs.Int("limit", 50, "条数上限")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()
	list, err := st.ListCandidates(*status, *session, *limit)
	if err != nil {
		return err
	}
	if list == nil {
		list = []store.Candidate{}
	}
	if c.json {
		return printJSON(list)
	}
	if len(list) == 0 {
		fmt.Println("（没有候选）")
		return nil
	}
	for _, cand := range list {
		fmt.Printf("%s  [%s] %s  %s\n", shortID(cand.ID), cand.Kind, cand.Status, oneLine(cand.Content))
	}
	return nil
}
