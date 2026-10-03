// Package provider 定义上游模块的抽象与公共转发骨架。
//
// 每个模块（zen / cline）实现 Provider 接口，差异集中在：
//   - 上游地址与路径（cline 需要额外的 /v1 前缀）
//   - 请求头构造（zen 白名单+改写；cline 完全忽略下游头，只发固定头）
//   - 请求体改写（zen 强制流式并补齐工具；cline 保持原样）
//   - 响应判定（zen 仅 429 冷却；cline 有 403 重试与 429 时长解析）
//
// 公共骨架 Provider.serve 负责：选 key → 转发 → 按 Decision 驱动重试/冷却 → 统计，
// 使模块差异不影响主流程。
package provider

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"zengateway/internal/stats"
	"zengateway/internal/store"
)

// Kind 是请求类型。
type Kind string

const (
	KindChat      Kind = "chat"
	KindResponses Kind = "responses"
	KindModels    Kind = "models"
)

// Action 是上游响应的处置动作。
type Action int

const (
	// ActionPass 直接透传给下游（含 2xx 与不需要特殊处理的错误）。
	ActionPass Action = iota
	// ActionRetrySame 用同一 key 重试（cline 的 403 卡片校验场景）。
	ActionRetrySame
	// ActionCooldownAndNext 冷却该 key 的该模型，并换下一个 key 重试。
	ActionCooldownAndNext
)

// Decision 描述一次上游响应应如何处置。
type Decision struct {
	Action   Action
	Cooldown time.Duration // ActionCooldownAndNext 时有效
	Reason   string        // 冷却原因，写入冷却池与统计
	Detail   string        // 原始错误摘要（截断后落库）
	MaxRetry int           // ActionRetrySame 的最大重试次数（不含首次请求）
}

// Versions 是构造请求头所需的版本号集合。
type Versions struct {
	Zen      string
	ClineCLI string
	ClineSDK string
}

// Provider 是上游模块的行为抽象。
type Provider interface {
	// Name 返回模块名（zen / cline），同时是 store 中的模块标识。
	Name() string
	// UpstreamBase 返回上游基址。
	UpstreamBase() string
	// PathFor 返回某类请求对应的上游路径（已含模块前缀，如 cline 的 /v1）。
	PathFor(kind Kind) string
	// RequireKey 表示是否必须有可用 key（cline 为 true）。
	RequireKey() bool
	// PrepareBody 改写下游请求体（就地修改），返回是否有改动。
	PrepareBody(kind Kind, body map[string]any) bool
	// BuildHeaders 用池中 key 与版本号构造发往上游的请求头。
	BuildHeaders(downstream http.Header, key store.APIKey, v Versions) http.Header
	// ModelOf 从请求体提取模型名（冷却维度）。
	ModelOf(body map[string]any) string
	// Classify 依据状态码与响应体决定处置动作。
	Classify(status int, body []byte) Decision
}

// Runtime 提供骨架所需的运行时依赖（由 main 装配）。
type Runtime struct {
	Store    *store.Store
	Log      *slog.Logger
	Versions func() Versions
	// MaxKeySwitches 限制单次请求最多切换多少个 key。
	MaxKeySwitches int
	// RetryBackoff 是 ActionRetrySame 的退避序列。
	RetryBackoff []time.Duration
}

// DefaultRetryBackoff 是 403 重试的固定退避。
func DefaultRetryBackoff() []time.Duration {
	return []time.Duration{300 * time.Millisecond, 800 * time.Millisecond, 1500 * time.Millisecond}
}

// BodyPeekLimit 限制读取错误响应体的最大字节数（判定用）。
const BodyPeekLimit = 64 << 10

// MaxBodyBytes 限制下游请求体的最大字节数。
const MaxBodyBytes = 32 << 20

// Handler 把 Provider 与运行时装配为一个 HTTP 处理器。
type Handler struct {
	P  Provider
	RT Runtime
}

// NewHandler 创建处理器。
func NewHandler(p Provider, rt Runtime) *Handler {
	if rt.Log == nil {
		rt.Log = slog.New(slog.DiscardHandler)
	}
	if rt.MaxKeySwitches <= 0 {
		rt.MaxKeySwitches = 10
	}
	if len(rt.RetryBackoff) == 0 {
		rt.RetryBackoff = DefaultRetryBackoff()
	}
	return &Handler{P: p, RT: rt}
}

// Serve 是模块的统一入口。
//
// accessToken 为空表示该模块允许匿名访问（仅 zen 默认如此）。
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, kind Kind, accessToken string) {
	// 1) 下游鉴权
	if !h.authorize(w, r, accessToken) {
		return
	}

	// 2) models 端点：无需请求体
	if kind == KindModels {
		h.serveModels(w, r)
		return
	}
	h.serveCompletion(w, r, kind)
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, accessToken string) bool {
	if strings.TrimSpace(accessToken) == "" {
		return true // 未配置 token：允许匿名
	}
	got := extractBearer(r.Header.Get("Authorization"))
	if got == accessToken {
		return true
	}
	writeJSONError(w, http.StatusUnauthorized,
		"invalid API key for this gateway module", "authentication_error")
	return false
}

func (h *Handler) serveModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	key, err := h.pickKey("")
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable,
			"no upstream key available for module "+h.P.Name(), "server_error")
		return
	}
	start := time.Now()
	client, err := clientFor(key)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "proxy client: "+err.Error(), "server_error")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
		h.P.UpstreamBase()+h.P.PathFor(KindModels), nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	req.Header = h.P.BuildHeaders(r.Header, key, h.RT.Versions())

	resp, err := client.Do(req)
	if err != nil {
		h.recordRequest(h.P.Name(), "", key.ID, http.StatusBadGateway, start, stats.Result{}, false)
		writeJSONError(w, http.StatusBadGateway, "upstream request: "+err.Error(), "server_error")
		return
	}
	defer resp.Body.Close()
	// models 列表不含 token 用量，仅记录请求数与耗时
	h.recordRequest(h.P.Name(), "", key.ID, resp.StatusCode, start, stats.Result{}, false)
	copyResponse(w, resp.Body, resp.StatusCode, resp.Header)
}

func (h *Handler) serveCompletion(w http.ResponseWriter, r *http.Request, kind Kind) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read body: "+err.Error(), "invalid_request_error")
		return
	}
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // 保留数字精度
	if err := dec.Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body: "+err.Error(), "invalid_request_error")
		return
	}
	// JSON 字面量 null / [] / 123 等能解码成功但得到 nil map，
	// 后续写入（如注入 stream/tools）会 panic，因此统一回退为空对象。
	if body == nil {
		body = map[string]any{}
	}
	// 模块可对请求体做改写（zen 强制流式并补齐工具）
	h.P.PrepareBody(kind, body)

	newBody, err := json.Marshal(body)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "re-encode body: "+err.Error(), "server_error")
		return
	}
	model := h.P.ModelOf(body)
	module := h.P.Name()

	maxSwitch := h.RT.MaxKeySwitches
	if n, err := h.RT.Store.CountKeys(module); err == nil && n < maxSwitch && n > 0 {
		maxSwitch = n
	}

	start := time.Now()
	attempted := map[int64]bool{}

	for switchIdx := 0; switchIdx < maxSwitch; switchIdx++ {
		key, err := h.pickKey(model)
		if err != nil {
			writeJSONError(w, statusAllCooling, "all upstream keys are cooling down for model "+model,
				"rate_limit_error")
			return
		}
		if attempted[key.ID] {
			// 该 key 已在本次请求中失败过且仍被选中，说明无其他可用 key
			break
		}
		attempted[key.ID] = true

		outcome := h.attempt(w, r, kind, newBody, key, model, start)
		switch outcome.action {
		case ActionPass:
			return // 响应已写出
		case ActionCooldownAndNext:
			h.setCooldown(key, model, outcome.decision)
			continue
		case ActionRetrySame:
			// 同 key 重试已在 attempt 内部完成；若仍返回该动作说明已耗尽重试
			return
		}
	}

	// 全部 key 均不可用
	h.recordError(module, model, 0, start, "all keys exhausted")
	writeJSONError(w, StatusAllCooling, "all upstream keys exhausted for model "+model,
		"rate_limit_error")
}

// StatusAllCooling 在全部 key 冷却/耗尽时返回给下游。
const StatusAllCooling = http.StatusTooManyRequests

// outcome 是 attempt 的结果。
type outcome struct {
	action   Action
	decision Decision
}

// attempt 用指定 key 发起请求，并按 Provider 的判定执行同 key 重试（如 cline 403）。
func (h *Handler) attempt(w http.ResponseWriter, r *http.Request, kind Kind,
	body []byte, key store.APIKey, model string, start time.Time) outcome {

	client, err := clientFor(key)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "proxy client: "+err.Error(), "server_error")
		return outcome{action: ActionPass}
	}

	module := h.P.Name()
	isStream := requestIsStream(body)

	for retry := 0; ; retry++ {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
			h.P.UpstreamBase()+h.P.PathFor(kind), bytes.NewReader(body))
		if err != nil {
			h.recordError(module, model, key.ID, start, "build request: "+err.Error())
			writeJSONError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return outcome{action: ActionPass}
		}
		req.Header = h.P.BuildHeaders(r.Header, key, h.RT.Versions())

		resp, err := client.Do(req)
		if err != nil {
			h.recordError(module, model, key.ID, start, "upstream request: "+err.Error())
			writeJSONError(w, http.StatusBadGateway, "upstream request: "+err.Error(), "server_error")
			return outcome{action: ActionPass}
		}

		h.RT.Log.Debug("forwarding", "module", module, "kind", kind, "model", model,
			"keyId", key.ID, "status", resp.StatusCode, "retry", retry)

		// 非 2xx 且需要判定时读取（受限）响应体
		needClassify := resp.StatusCode != http.StatusOK
		var payload []byte
		if needClassify {
			payload, _ = io.ReadAll(io.LimitReader(resp.Body, BodyPeekLimit))
		}
		decision := Decision{Action: ActionPass}
		if needClassify {
			decision = h.P.Classify(resp.StatusCode, payload)
		}

		if decision.Action == ActionRetrySame && retry < decision.MaxRetry {
			resp.Body.Close()
			wait := h.backoffFor(retry)
			h.RT.Log.Info("retrying upstream request on same key",
				"module", module, "status", resp.StatusCode, "attempt", retry+1, "wait", wait)
			h.recordRetry(module, model, key.ID, start)
			if !sleepCtx(r, wait) {
				writeJSONError(w, http.StatusRequestTimeout, "client gone", "server_error")
				return outcome{action: ActionPass}
			}
			continue
		}

		if decision.Action == ActionCooldownAndNext {
			resp.Body.Close()
			return outcome{action: ActionCooldownAndNext, decision: decision}
		}

		// 透传：状态码 + 安全响应头 + body；同时采集 token 用量与 TTFT
		collector := stats.NewCollector(isStream, start)
		if len(payload) > 0 {
			collector.Write(payload) // 非 2xx 但需透传的响应体也要计入解析
		}
		written := copyResponseWithPrefix(w, io.TeeReader(resp.Body, collector),
			payload, resp.StatusCode, resp.Header)
		resp.Body.Close()
		res := collector.Finish()
		h.recordRequest(module, model, key.ID, resp.StatusCode, start, res, isStream && written == 0)
		return outcome{action: ActionPass}
	}
}

func (h *Handler) backoffFor(retry int) time.Duration {
	if retry < len(h.RT.RetryBackoff) {
		return h.RT.RetryBackoff[retry]
	}
	return h.RT.RetryBackoff[len(h.RT.RetryBackoff)-1]
}

func (h *Handler) setCooldown(key store.APIKey, model string, d Decision) {
	until := time.Now().Add(d.Cooldown)
	if err := h.RT.Store.SetCooldown(store.Cooldown{
		Module: h.P.Name(), KeyID: key.ID, Model: model,
		Until: until, Reason: d.Reason, Detail: d.Detail, CreatedAt: time.Now(),
	}); err != nil {
		h.RT.Log.Warn("set cooldown failed", "err", err, "keyId", key.ID)
		return
	}
	h.RT.Log.Info("key cooled down", "module", h.P.Name(), "keyId", key.ID,
		"model", model, "until", until, "reason", d.Reason)
}

// statusAllCooling 用于全部 key 冷却的场景。
const statusAllCooling = http.StatusTooManyRequests

// ---- 统计记录 -------------------------------------------------------------

// recordRequest 把一次请求的统计（含 token 用量与 TTFT）写入缓冲，
// 并追加一条请求明细供后续排查。
func (h *Handler) recordRequest(module, model string, keyID int64, status int, start time.Time,
	res stats.Result, clientGone bool) {

	delta := store.StatDelta{
		Day: time.Now().Format(store.DayLayout), Module: module, KeyID: keyID, Model: model,
		Requests: 1, Errors: errCount(status),
		PromptTokens:     res.Usage.PromptTokens,
		CompletionTokens: res.Usage.CompletionTokens,
		CachedTokens:     res.Usage.CachedTokens,
		TotalTokens:      res.Usage.TotalTokens,
	}
	if res.HasTTFT {
		delta.TTFTSumMs = res.TTFTMs
		delta.TTFTCount = 1
	}
	h.RT.Store.AddStat(delta)

	var ttft *int64
	if res.HasTTFT {
		v := res.TTFTMs
		ttft = &v
	}
	errMsg := ""
	if clientGone {
		errMsg = "downstream disconnected before response completed"
	}
	now := time.Now()
	h.RT.Store.AppendRequestLog(store.RequestLog{
		TS: now, Day: now.Format(store.DayLayout), Module: module, KeyID: keyID, Model: model,
		Status: status, LatencyMs: time.Since(start).Milliseconds(), TTFTMs: ttft,
		PromptTokens: res.Usage.PromptTokens, CompletionTokens: res.Usage.CompletionTokens,
		CachedTokens: res.Usage.CachedTokens, TotalTokens: res.Usage.TotalTokens,
		Error: errMsg,
	})
}

// requestIsStream 判断请求体是否要求流式响应，
// 以便用 SSE 还是整体 JSON 的方式解析用量。
func requestIsStream(body []byte) bool {
	var probe struct {
		Stream *bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.Stream != nil && *probe.Stream
}

func (h *Handler) recordError(module, model string, keyID int64, start time.Time, msg string) {
	h.RT.Store.AddStat(store.StatDelta{
		Day: time.Now().Format(store.DayLayout), Module: module, KeyID: keyID, Model: model,
		Requests: 1, Errors: 1,
	})
}

func (h *Handler) recordRetry(module, model string, keyID int64, start time.Time) {
	h.RT.Store.AddStat(store.StatDelta{
		Day: time.Now().Format(store.DayLayout), Module: module, KeyID: keyID, Model: model,
		Retries: 1,
	})
}

func errCount(status int) int64 {
	if status >= 400 {
		return 1
	}
	return 0
}

// ---- 通用工具 -------------------------------------------------------------

// extractBearer 从 Authorization 头提取 token。
func extractBearer(authz string) string {
	authz = strings.TrimSpace(authz)
	if authz == "" {
		return ""
	}
	parts := strings.SplitN(authz, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
		return strings.TrimSpace(parts[1])
	}
	if strings.EqualFold(authz, "bearer") {
		return "" // 只有 scheme 没有凭据
	}
	return authz
}

// pickKey 返回下一个可用 key。
//
// 当模块允许匿名（zen）且 key 池为空时，回落到内置的 public 匿名条目，
// 使未配置任何 key 的部署仍可直接使用免费层。该条目不在 upstream_keys 表中，
// 因此需要单独检查它的冷却状态，否则 429 冷却后会绕过限制持续请求上游。
func (h *Handler) pickKey(model string) (store.APIKey, error) {
	module := h.P.Name()
	key, err := h.RT.Store.PickKey(module, model, time.Now())
	if err == store.ErrNotFound && !h.P.RequireKey() {
		anon := store.APIKey{
			ID:          0,
			Module:      module,
			Label:       "anonymous",
			APIKey:      "public",
			IsAnonymous: true,
			Enabled:     true,
		}
		cooling, cerr := h.RT.Store.IsCoolingDown(module, anon.ID, model, time.Now())
		if cerr != nil {
			return store.APIKey{}, cerr
		}
		if cooling {
			return store.APIKey{}, store.ErrNotFound
		}
		return anon, nil
	}
	return key, err
}

// writeJSONError 输出 OpenAI 风格的错误体。
func writeJSONError(w http.ResponseWriter, status int, message, typ string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": message, "type": typ},
	})
}

// copyResponse 透传响应：过滤长度/连接相关头，逐块写出并 flush（支持 SSE）。
func copyResponse(w http.ResponseWriter, body io.Reader, status int, header http.Header) {
	copyResponseWithPrefix(w, body, nil, status, header)
}

// copyResponseWithPrefix 透传响应：过滤长度/连接相关头，逐块写出并 flush（支持 SSE）。
// 返回已写给下游的字节数。
func copyResponseWithPrefix(w http.ResponseWriter, body io.Reader, prefix []byte, status int, header http.Header) int64 {
	for k, vs := range header {
		switch strings.ToLower(k) {
		case "content-length", "connection", "transfer-encoding":
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(status)
	flush(w)

	var total int64
	if len(prefix) > 0 {
		n, err := w.Write(prefix)
		total += int64(n)
		flush(w)
		if err != nil {
			return total
		}
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			wn, werr := w.Write(buf[:n])
			total += int64(wn)
			flush(w)
			if werr != nil {
				return total // 下游断开
			}
		}
		if err != nil {
			return total
		}
	}
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// sleepCtx 等待指定时长；下游取消则返回 false。
func sleepCtx(r *http.Request, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-r.Context().Done():
		return false
	case <-t.C:
		return true
	}
}
