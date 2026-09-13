// Package api 实现 ghydra daemon 的本机控制 API（M3-W1，方案 D3 修订版）。
//
// 安全模型（W1-Design §1）：
//   - Host 校验：仅 127.0.0.1 / localhost / ::1（防 DNS rebinding）
//   - token：/api/* 读写全要（X-GHydra-Token 头 | Bearer | ?token=）
//   - CORS：ACAO:* + 预检 204（wails 壳 origin 不定；token 是写保护核心）
//   - SSE：连接上限 8、慢消费者踢线、15s 心跳
//
// 依赖方向：本包只认函数签名与自身 DTO，不 import channel/probe/get/
// store——cmd 装配闭包（单一实现、CLI/API 双入口）。
package api

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrBusy 依赖侧并发冲突（→409）：doctor 已在跑 / 下载任务已存在。
var ErrBusy = errors.New("busy")

// UserError 请求语义错误（→400）。依赖侧返回。
type UserError string

func (e UserError) Error() string { return string(e) }

// Deps 装配面：全部可选字段在 New 时校验必填项。
type Deps struct {
	// 必填
	Status    func() ApiStatus                           // GET /api/status + SSE status 帧
	ConfigGet func() ApiConfig                           // GET /api/config
	ConfigSet func(patch ConfigPatch) (ApiConfig, error) // POST（仅 cdn 热更）
	DoctorRun func(repo string) (string, error)          // 异步触发；返回 runID；忙 = ErrBusy

	// 可选（nil → 端点 503 "not assembled"）
	SystemOn      func(mode string) (OnResp, error)
	SystemOff     func(shutdown bool) (OffResp, error)
	DoctorSummary func(hours int) (DoctorSummaryResp, error)
	GitOp         func(action string, body []byte) (any, error) // enable|disable|status
	SSHOp         func(action string, body []byte) (any, error)
	GetStart      func(req GetStartReq) (GetStartResp, error)
	GetProgress   func() GetProgressResp

	// 观测：额外放行的 Host（测试/特殊部署）。127.0.0.1/localhost/::1 恒放行。
	ExtraHosts []string
}

// Server /api/* 子树服务。
type Server struct {
	deps  Deps
	token string
	hub   *hub

	mu      sync.Mutex
	pending []ConnEvent // conn 帧合批缓冲

	maxBody   int64
	heartbeat time.Duration
	statusEv  time.Duration // status diff 节拍
	connEv    time.Duration // conn 合批窗口
}

// New 构造。token 空 = 拒绝一切（防装配遗漏裸奔）。
func New(deps Deps, token string) *Server {
	if deps.Status == nil || deps.ConfigGet == nil || deps.ConfigSet == nil || deps.DoctorRun == nil {
		panic("api: Deps.Status/ConfigGet/ConfigSet/DoctorRun 必填")
	}
	return &Server{
		deps:      deps,
		token:     token,
		hub:       newHub(0),
		maxBody:   1 << 20,
		heartbeat: 15 * time.Second,
		statusEv:  time.Second,
		connEv:    250 * time.Millisecond,
	}
}

// Handler 返回 /api/ 子树处理器（挂 mux.Handle("/api/", …)）。
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.serve)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	setCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !s.hostOK(r.Host) {
		writeErr(w, http.StatusForbidden, "host not allowed")
		return
	}
	if !s.tokenOK(r) {
		writeErr(w, http.StatusUnauthorized, "missing or invalid token")
		return
	}

	sub := strings.TrimPrefix(r.URL.Path, "/api/")
	sub = strings.Trim(sub, "/")

	switch {
	case sub == "status" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.deps.Status())

	case sub == "config" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.deps.ConfigGet())

	case sub == "config" && r.Method == http.MethodPost:
		var p ConfigPatch
		if !readBody(w, r, s.maxBody, &p) {
			return
		}
		cfg, err := s.deps.ConfigSet(p)
		writeDep(w, cfg, err)

	case sub == "on" && r.Method == http.MethodPost:
		if s.deps.SystemOn == nil {
			writeErr(w, http.StatusServiceUnavailable, "on/off not assembled")
			return
		}
		var req OnReq
		if !readBody(w, r, s.maxBody, &req) {
			return
		}
		if req.Mode == "" {
			req.Mode = "pac"
		}
		if req.Mode != "pac" && req.Mode != "proxy" {
			writeErr(w, http.StatusBadRequest, `mode must be "pac" or "proxy"`)
			return
		}
		resp, err := s.deps.SystemOn(req.Mode)
		writeDep(w, resp, err)

	case sub == "off" && r.Method == http.MethodPost:
		if s.deps.SystemOff == nil {
			writeErr(w, http.StatusServiceUnavailable, "on/off not assembled")
			return
		}
		var req OffReq
		if !readBody(w, r, s.maxBody, &req) {
			return
		}
		resp, err := s.deps.SystemOff(req.Shutdown)
		writeDep(w, resp, err)

	case sub == "doctor/run" && r.Method == http.MethodPost:
		var body struct {
			Repo string `json:"repo"`
		}
		if !readBody(w, r, s.maxBody, &body) {
			return
		}
		runID, err := s.deps.DoctorRun(body.Repo)
		if err != nil {
			writeDep(w, nil, err)
			return
		}
		writeJSON(w, http.StatusOK, DoctorRunResp{RunID: runID, Started: true})

	case sub == "doctor/summary" && r.Method == http.MethodGet:
		if s.deps.DoctorSummary == nil {
			writeErr(w, http.StatusServiceUnavailable, "doctor summary not assembled")
			return
		}
		resp, err := s.deps.DoctorSummary(intHours(r.URL.Query().Get("hours"), 24))
		writeDep(w, resp, err)

	case sub == "git/enable" && r.Method == http.MethodPost,
		sub == "git/disable" && r.Method == http.MethodPost,
		sub == "git/status" && r.Method == http.MethodGet:
		if s.deps.GitOp == nil {
			writeErr(w, http.StatusServiceUnavailable, "git op not assembled")
			return
		}
		body, ok := drainBody(w, r, s.maxBody)
		if !ok {
			return
		}
		resp, err := s.deps.GitOp(strings.TrimPrefix(sub, "git/"), body)
		writeDep(w, resp, err)

	case sub == "ssh/enable" && r.Method == http.MethodPost,
		sub == "ssh/disable" && r.Method == http.MethodPost,
		sub == "ssh/status" && r.Method == http.MethodGet:
		if s.deps.SSHOp == nil {
			writeErr(w, http.StatusServiceUnavailable, "ssh op not assembled")
			return
		}
		body, ok := drainBody(w, r, s.maxBody)
		if !ok {
			return
		}
		resp, err := s.deps.SSHOp(strings.TrimPrefix(sub, "ssh/"), body)
		writeDep(w, resp, err)

	case sub == "get/start" && r.Method == http.MethodPost:
		if s.deps.GetStart == nil {
			writeErr(w, http.StatusServiceUnavailable, "get not assembled")
			return
		}
		var req GetStartReq
		if !readBody(w, r, s.maxBody, &req) {
			return
		}
		if req.URL == "" {
			writeErr(w, http.StatusBadRequest, "url required")
			return
		}
		resp, err := s.deps.GetStart(req)
		writeDep(w, resp, err)

	case sub == "get/progress" && r.Method == http.MethodGet:
		if s.deps.GetProgress == nil {
			writeErr(w, http.StatusServiceUnavailable, "get not assembled")
			return
		}
		writeJSON(w, http.StatusOK, s.deps.GetProgress())

	case sub == "mitm/status" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, MitmStatus{Available: false, Enabled: false,
			Reason: "mitm engine lands in W2"})

	case sub == "mitm/enable" || sub == "mitm/disable" || sub == "mitm/uninstall-cert":
		writeErr(w, http.StatusNotImplemented, "mitm engine lands in W2")

	case sub == "events" && r.Method == http.MethodGet:
		s.handleEvents(w, r)

	default:
		writeErr(w, http.StatusNotFound, "no such endpoint: /api/"+sub)
	}
}

// ---- SSE ----

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	ch, err := s.hub.subscribe()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer s.hub.unsubscribe(ch)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	writeFrame := func(b []byte) bool {
		if _, err := w.Write(b); err != nil {
			return false
		}
		fl.Flush()
		return true
	}

	// hello + 首帧 status（前端秒出画面）
	hb, _ := json.Marshal(HelloFrame{Proto: 1, HeartbeatS: int(s.heartbeat / time.Second), APIVersion: APIVersion})
	if !writeFrame([]byte("event: hello\ndata: " + string(hb) + "\n\n")) {
		return
	}
	if st := s.deps.Status(); !writeFrame([]byte(fmt.Sprintf("event: status\ndata: %s\n\n", mustJSON(st)))) {
		return
	}

	hbT := time.NewTicker(s.heartbeat)
	defer hbT.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-hbT.C:
			if !writeFrame([]byte(": ping\n\n")) {
				return
			}
		case frame, ok := <-ch:
			if !ok {
				return // 被踢（慢消费/超限）
			}
			if !writeFrame(frame) {
				return
			}
		}
	}
}

// PushConn 连接事件入批（serve 的 OnEvent 调用；非阻塞）。
func (s *Server) PushConn(ev ConnEvent) {
	s.mu.Lock()
	s.pending = append(s.pending, ev)
	s.mu.Unlock()
}

// PushDoctor doctor 完成帧（立即广播）。
func (s *Server) PushDoctor(f DoctorFrame) { s.hub.broadcast("doctor", f) }

// PushGet 下载任务帧（调用方负责节流，registry 250ms）。
func (s *Server) PushGet(t GetTask) { s.hub.broadcast("get", t) }

// Start 启动内部节拍：status diff（1s）+ conn 合批 flush（250ms）。
// ctx 取消即停。
func (s *Server) Start(ctx interface{ Done() <-chan struct{} }) {
	go func() {
		var last string
		t := time.NewTicker(s.statusEv)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.tickStatus(&last)
			}
		}
	}()
	go func() {
		t := time.NewTicker(s.connEv)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.flushConns()
			}
		}
	}()
}

// tickStatus 快照 diff；变化才广播（测试直接调用）。
func (s *Server) tickStatus(last *string) {
	st := s.deps.Status()
	cur := string(mustJSON(st))
	if cur == *last {
		return
	}
	*last = cur
	s.hub.broadcast("status", st)
}

// flushConns conn 合批（测试直接调用）。
func (s *Server) flushConns() {
	s.mu.Lock()
	evs := s.pending
	s.pending = nil
	s.mu.Unlock()
	if len(evs) > 0 {
		s.hub.broadcast("conn", ConnFrame{Events: evs})
	}
}

// ---- guards ----

func (s *Server) hostOK(raw string) bool {
	host := raw
	if h, _, err := net.SplitHostPort(raw); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	for _, extra := range s.deps.ExtraHosts {
		if strings.EqualFold(extra, host) {
			return true
		}
	}
	return false
}

func (s *Server) tokenOK(r *http.Request) bool {
	if s.token == "" {
		return false // 未装配 token = 拒绝一切
	}
	got := r.Header.Get("X-GHydra-Token")
	if got == "" {
		if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
			got = strings.TrimPrefix(a, "Bearer ")
		}
	}
	if got == "" {
		got = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

func setCORS(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "x-ghydra-token, authorization, content-type")
}

// ---- 小工具 ----

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// writeDep 依赖返回值 → HTTP：nil resp + err 仍走错误路径。
func writeDep(w http.ResponseWriter, resp any, err error) {
	if err != nil {
		switch {
		case errors.Is(err, ErrBusy):
			writeErr(w, http.StatusConflict, err.Error())
		case errors.As(err, new(UserError)):
			writeErr(w, http.StatusBadRequest, err.Error())
		default:
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func readBody(w http.ResponseWriter, r *http.Request, max int64, v any) bool {
	body, ok := drainBody(w, r, max)
	if !ok {
		return false
	}
	if len(body) == 0 {
		return true // 空 body = 零值（如 {"cdn":...} 全缺省）
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields() // 契约严格：未知字段 400（拼写错误早暴露）
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return false
	}
	return true
}

func drainBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, bool) {
	if r.Body == nil {
		return nil, true
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return nil, false
	}
	if int64(len(b)) > max {
		writeErr(w, http.StatusRequestEntityTooLarge, "body too large")
		return nil, false
	}
	return b, true
}

func intHours(raw string, def int) int {
	if raw == "" {
		return def
	}
	n := 0
	for _, c := range raw {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
		if n > 24*30 {
			return 24 * 30
		}
	}
	if n == 0 {
		return def
	}
	return n
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}
