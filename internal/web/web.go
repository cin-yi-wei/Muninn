// Package web 是 Muninn 的管理後台與對外 API（同一組 endpoint：網頁用、外部系統也用）。
//
// 認證：Authorization: Bearer <token>（token 從環境變數讀）。沒有 token 不啟動。
// 預設只聽本機（127.0.0.1）；要讓外部連，listen 改成其他位址並自行用反向代理或 tunnel 加 TLS。
//
//	GET    /                                   管理網頁
//	GET    /api/bindings                       所有頻道與即時狀態
//	POST   /api/bindings                       新增頻道（同 muninn bind）
//	DELETE /api/bindings/{name}                移除頻道（?delete_channel=1&keep_worktree=1）
//	POST   /api/bindings/{name}/pause|resume
//	POST   /api/bindings/{name}/set            {"key":"auto_approve","value":"true"}
//	POST   /api/bindings/{name}/message        {"text":"…","source":"ci"}  把訊息送進那個頻道的 claude（webhook 入口）
//	GET    /api/bindings/{name}/schedules
//	POST   /api/bindings/{name}/schedules      {"name","cron"|"at","message","timezone","catch_up"}
//	DELETE /api/bindings/{name}/schedules/{s}
//	POST   /api/bindings/{name}/schedules/{s}/pause|resume
//	GET    /api/tasks?binding=
//	POST   /api/tasks                          {"binding","task","role","level","title"}  代某個頻道委派，結果回那個頻道
//	POST   /api/tasks/{id}/cancel
//	GET    /api/audit?n=&binding=
package web

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"muninn/internal/admin"
	"muninn/internal/app"
	"muninn/internal/audit"
	"muninn/internal/registry"
	"muninn/internal/scheduler"
	"muninn/internal/tasks"
)

//go:embed index.html
var indexHTML []byte

// BindingInfo 是一個頻道的設定與即時狀態。
type BindingInfo struct {
	Name        string        `json:"name"`
	Platform    string        `json:"platform"`
	ChannelID   string        `json:"channel_id"`
	Repo        string        `json:"repo"`
	Branch      string        `json:"branch,omitempty"`
	Worktree    string        `json:"worktree,omitempty"`
	Paused      bool          `json:"paused"`
	Control     bool          `json:"control"`
	AutoApprove bool          `json:"auto_approve"`
	Model       string        `json:"model,omitempty"`
	Running     bool          `json:"running"`
	Status      *app.Snapshot `json:"status,omitempty"`
	Schedules   int           `json:"schedules"`
	ActiveTasks int           `json:"active_tasks"`
}

// Backend 是 daemon 提供給 web 的能力。
type Backend interface {
	Config() registry.Config
	App(name string) *app.App // 執行中的頻道；暫停或不存在回 nil
	Admin() *admin.Admin
	Tasks() *tasks.Manager
	Audit() *audit.Log
}

// Server 是管理後台。
type Server struct {
	Listen  string
	Token   string
	Backend Backend
}

// Run 啟動 HTTP server，直到 ctx 取消。
func (s *Server) Run(ctx context.Context) error {
	if s.Token == "" {
		return errors.New("web: 沒有 token，不啟動")
	}
	host, _, err := net.SplitHostPort(s.Listen)
	if err != nil {
		return fmt.Errorf("web: listen %q 格式錯誤：%w", s.Listen, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		log.Printf("web: ⚠️ 管理後台聽在 %s（不是本機），請確定前面有 TLS", s.Listen)
	}
	srv := &http.Server{Addr: s.Listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Printf("web: 管理後台 http://%s/", s.Listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return ctx.Err()
}

// Handler 回傳路由（測試也用）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'")
		_, _ = w.Write(indexHTML)
	})
	api := func(pattern string, h func(r *http.Request) (any, error)) {
		mux.HandleFunc(pattern, s.auth(func(w http.ResponseWriter, r *http.Request) {
			v, err := h(r)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			if err != nil {
				code := http.StatusBadRequest
				var he httpError
				if errors.As(err, &he) {
					code = he.code
				}
				w.WriteHeader(code)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			_ = json.NewEncoder(w).Encode(v)
		}))
	}
	api("GET /api/bindings", s.listBindings)
	api("POST /api/bindings", s.bind)
	api("DELETE /api/bindings/{name}", s.unbind)
	api("POST /api/bindings/{name}/pause", s.pause(true))
	api("POST /api/bindings/{name}/resume", s.pause(false))
	api("POST /api/bindings/{name}/set", s.set)
	api("POST /api/bindings/{name}/message", s.message)
	api("GET /api/bindings/{name}/schedules", s.listSchedules)
	api("POST /api/bindings/{name}/schedules", s.addSchedule)
	api("DELETE /api/bindings/{name}/schedules/{s}", s.removeSchedule)
	api("POST /api/bindings/{name}/schedules/{s}/pause", s.pauseSchedule(true))
	api("POST /api/bindings/{name}/schedules/{s}/resume", s.pauseSchedule(false))
	api("GET /api/tasks", s.listTasks)
	api("POST /api/tasks", s.delegate)
	api("POST /api/tasks/{id}/cancel", s.cancelTask)
	api("GET /api/audit", s.audit)
	return mux
}

type httpError struct {
	code int
	msg  string
}

func (e httpError) Error() string { return e.msg }

func notFound(format string, args ...any) error {
	return httpError{http.StatusNotFound, fmt.Sprintf(format, args...)}
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"token 不對"}`))
			return
		}
		h(w, r)
	}
}

func (s *Server) record(r *http.Request, action, binding, detail, result string) {
	s.Backend.Audit().Record(audit.Event{Kind: "api", Binding: binding, Actor: "web:" + clientIP(r), Action: action, Detail: detail, Result: result})
}

func clientIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("JSON 格式錯誤：%w", err)
	}
	return nil
}

func (s *Server) admin() *admin.Admin { return s.Backend.Admin().As("web") }

func (s *Server) binding(name string) (registry.Binding, error) {
	b, ok := s.Backend.Config().Find(name)
	if !ok {
		return b, notFound("沒有頻道 %q", name)
	}
	return b, nil
}

func (s *Server) listBindings(*http.Request) (any, error) {
	cfg := s.Backend.Config()
	out := []BindingInfo{}
	for _, b := range cfg.Bindings {
		info := BindingInfo{Name: b.Name, Platform: b.Platform, ChannelID: b.ChannelID, Repo: b.Repo, Branch: b.Branch,
			Worktree: b.Worktree, Paused: b.Paused, Control: b.Control, AutoApprove: b.AutoApprove, Model: b.Model}
		if a := s.Backend.App(b.Name); a != nil {
			snap := a.Snapshot()
			info.Running, info.Status = true, &snap
		}
		if list, err := scheduler.NewStore(cfg.StateDirFor(b.Name), cfg.Timezone).List(); err == nil {
			info.Schedules = len(list)
		}
		if tm := s.Backend.Tasks(); tm != nil {
			for _, t := range tm.List(b.Name) {
				if t.Active() {
					info.ActiveTasks++
				}
			}
		}
		out = append(out, info)
	}
	return out, nil
}

func (s *Server) bind(r *http.Request) (any, error) {
	var in struct {
		Name, Repo, Branch, Base, Platform, ChannelID, Model string
		AutoApprove, Control, CreateChannel                  bool
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	msg, err := s.admin().Bind(ctx, admin.BindOptions{Name: in.Name, Repo: in.Repo, Branch: in.Branch, Base: in.Base,
		Platform: in.Platform, ChannelID: in.ChannelID, CreateChannel: in.CreateChannel || in.ChannelID == "",
		AutoApprove: in.AutoApprove, Control: in.Control, Model: in.Model})
	if err != nil {
		return nil, err
	}
	return map[string]string{"message": msg}, nil
}

func (s *Server) unbind(r *http.Request) (any, error) {
	name := r.PathValue("name")
	if _, err := s.binding(name); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	msg, err := s.admin().Unbind(ctx, admin.UnbindOptions{Name: name,
		DeleteChannel: r.URL.Query().Get("delete_channel") == "1", KeepWorktree: r.URL.Query().Get("keep_worktree") == "1"})
	if err != nil {
		return nil, err
	}
	return map[string]string{"message": msg}, nil
}

func (s *Server) pause(p bool) func(*http.Request) (any, error) {
	return func(r *http.Request) (any, error) {
		name := r.PathValue("name")
		if _, err := s.binding(name); err != nil {
			return nil, err
		}
		msg, err := s.admin().SetPaused(name, p)
		if err != nil {
			return nil, err
		}
		return map[string]string{"message": msg}, nil
	}
}

func (s *Server) set(r *http.Request) (any, error) {
	name := r.PathValue("name")
	var in struct{ Key, Value string }
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	msg, err := s.admin().Set(name, in.Key, in.Value)
	if err != nil {
		return nil, err
	}
	return map[string]string{"message": msg}, nil
}

func (s *Server) message(r *http.Request) (any, error) {
	name := r.PathValue("name")
	var in struct{ Text, Source string }
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if in.Source == "" {
		in.Source = "web"
	}
	a := s.Backend.App(name)
	if a == nil {
		if _, err := s.binding(name); err != nil {
			return nil, err
		}
		return nil, httpError{http.StatusConflict, "頻道 " + name + " 暫停中，先恢復再送"}
	}
	id, err := a.InjectExternal(in.Source, in.Text)
	s.record(r, "message", name, in.Source+"："+in.Text, map[bool]string{true: "error", false: "ok"}[err != nil])
	if err != nil {
		return nil, err
	}
	return map[string]string{"id": id, "message": "已送進 " + name + "，claude 處理完會回在那個頻道"}, nil
}

func (s *Server) store(name string) (*scheduler.Store, error) {
	if _, err := s.binding(name); err != nil {
		return nil, err
	}
	cfg := s.Backend.Config()
	return scheduler.NewStore(cfg.StateDirFor(name), cfg.Timezone), nil
}

func (s *Server) listSchedules(r *http.Request) (any, error) {
	st, err := s.store(r.PathValue("name"))
	if err != nil {
		return nil, err
	}
	list, err := st.List()
	if err != nil {
		return nil, err
	}
	type item struct {
		scheduler.Schedule
		Next string `json:"next,omitempty"`
	}
	out := []item{}
	now := time.Now()
	for _, sc := range list {
		it := item{Schedule: sc}
		if n, ok := scheduler.Next(sc, st.DefaultTZ(), now); ok {
			it.Next = n.Format(time.RFC3339)
		}
		out = append(out, it)
	}
	return out, nil
}

func (s *Server) addSchedule(r *http.Request) (any, error) {
	name := r.PathValue("name")
	st, err := s.store(name)
	if err != nil {
		return nil, err
	}
	var in scheduler.Schedule
	var replace struct {
		Replace bool `json:"replace"`
	}
	body := http.MaxBytesReader(nil, r.Body, 1<<20)
	raw := json.RawMessage{}
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("JSON 格式錯誤：%w", err)
	}
	_ = json.Unmarshal(raw, &in)
	_ = json.Unmarshal(raw, &replace)
	in.Paused, in.LastFired = false, ""
	added, err := st.Add(in, replace.Replace, time.Now())
	s.record(r, "schedule_add", name, in.Name, map[bool]string{true: "error", false: "ok"}[err != nil])
	if err != nil {
		return nil, err
	}
	return added, nil
}

func (s *Server) removeSchedule(r *http.Request) (any, error) {
	name := r.PathValue("name")
	st, err := s.store(name)
	if err != nil {
		return nil, err
	}
	err = st.Remove(r.PathValue("s"))
	s.record(r, "schedule_remove", name, r.PathValue("s"), map[bool]string{true: "error", false: "ok"}[err != nil])
	if err != nil {
		return nil, notFound("%v", err)
	}
	return map[string]string{"message": "已刪除"}, nil
}

func (s *Server) pauseSchedule(p bool) func(*http.Request) (any, error) {
	return func(r *http.Request) (any, error) {
		st, err := s.store(r.PathValue("name"))
		if err != nil {
			return nil, err
		}
		if err := st.SetPaused(r.PathValue("s"), p, time.Now()); err != nil {
			return nil, notFound("%v", err)
		}
		return map[string]string{"message": "ok"}, nil
	}
}

func (s *Server) listTasks(r *http.Request) (any, error) {
	tm := s.Backend.Tasks()
	if tm == nil {
		return []tasks.Task{}, nil
	}
	list := tm.List(r.URL.Query().Get("binding"))
	if list == nil {
		list = []tasks.Task{}
	}
	return list, nil
}

func (s *Server) delegate(r *http.Request) (any, error) {
	var in struct{ Binding, Task, Role, Level, Title string }
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	b, err := s.binding(in.Binding)
	if err != nil {
		return nil, err
	}
	if s.Backend.App(b.Name) == nil {
		return nil, httpError{http.StatusConflict, "頻道 " + b.Name + " 暫停中，結果沒地方送"}
	}
	tm := s.Backend.Tasks()
	if tm == nil {
		return nil, errors.New("委派沒有啟用")
	}
	t, err := tm.Delegate(tasks.Source{Binding: b.Name, Repo: b.Repo, Workdir: b.Workdir()},
		tasks.DelegateInput{Task: in.Task, Title: in.Title, Role: in.Role, Level: in.Level})
	s.record(r, "delegate", b.Name, in.Task, map[bool]string{true: "error", false: "ok"}[err != nil])
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (s *Server) cancelTask(r *http.Request) (any, error) {
	tm := s.Backend.Tasks()
	if tm == nil {
		return nil, errors.New("委派沒有啟用")
	}
	t, ok := tm.Lookup(r.PathValue("id"))
	if !ok {
		return nil, notFound("沒有任務 %q", r.PathValue("id"))
	}
	t, err := tm.Cancel(t.Source, t.ID)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (s *Server) audit(r *http.Request) (any, error) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 || n > 1000 {
		n = 200
	}
	ev := s.Backend.Audit().Tail(n, r.URL.Query().Get("binding"))
	if ev == nil {
		ev = []audit.Event{}
	}
	return ev, nil
}
