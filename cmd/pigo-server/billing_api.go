// HTTP endpoints for billing: the price table, usage reports, and the per-turn
// usage attached to a session's history.
//
// Reports are computed from the ledger on each request. The ledger is one line
// per model call split by month, so a report reads only the months in its
// period; that stays fast long past the scale this server is meant for, and it
// means there is no second copy of the numbers to drift out of step.
package main

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// --- prices ------------------------------------------------------------------

type priceListResponse struct {
	Currency string       `json:"currency"`
	Unit     string       `json:"unit"`
	Prices   []modelPrice `json:"prices"`
}

func (s *apiServer) priceList() priceListResponse {
	prices := append([]modelPrice(nil), s.settings.prices()...)
	sort.Slice(prices, func(i, j int) bool {
		if prices[i].Provider != prices[j].Provider {
			return prices[i].Provider < prices[j].Provider
		}
		return prices[i].Model < prices[j].Model
	})
	if prices == nil {
		prices = []modelPrice{}
	}
	return priceListResponse{Currency: "CNY", Unit: "元/百万 token", Prices: prices}
}

// handleListPrices is readable by every user: what a model costs is not a
// secret, and it lets a user choose a model knowing its price.
func (s *apiServer) handleListPrices(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.priceList())
}

func (s *apiServer) handlePutPrice(w http.ResponseWriter, r *http.Request) {
	var request modelPrice
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	next, err := request.validate()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	next.UpdatedBy = principalForRequest(r).Username
	next.UpdatedAt = time.Now().UTC()
	if err := s.settings.putPrice(next); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logAdminAction(r, "put model price", fmt.Sprintf("%s/%s in=%g cacheRead=%s cacheWrite=%s out=%g",
		next.Provider, next.Model, next.Input, optionalPrice(next.CacheRead), optionalPrice(next.CacheWrite), next.Output))
	writeJSON(w, http.StatusOK, s.priceList())
}

// handleDeletePrice takes the row in the query string: model ids contain
// slashes ("openrouter/auto"), which do not survive as a path segment.
func (s *apiServer) handleDeletePrice(w http.ResponseWriter, r *http.Request) {
	providerName := strings.TrimSpace(r.URL.Query().Get("provider"))
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if providerName == "" || model == "" {
		writeError(w, http.StatusBadRequest, "provider 和 model 都不能为空")
		return
	}
	removed, err := s.settings.removePrice(providerName, model)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "该模型没有配置价格")
		return
	}
	s.logAdminAction(r, "delete model price", providerName+"/"+model)
	writeJSON(w, http.StatusOK, s.priceList())
}

func optionalPrice(v *float64) string {
	if v == nil {
		return "input"
	}
	return strconv.FormatFloat(*v, 'g', -1, 64)
}

// --- aggregation -------------------------------------------------------------

// usageTotals sums a set of calls. Cost is in yuan; the sums are taken over the
// integer nano-yuan amounts first, so they add up exactly.
type usageTotals struct {
	Calls      int     `json:"calls"`
	Input      int     `json:"input"`
	CacheRead  int     `json:"cacheRead"`
	CacheWrite int     `json:"cacheWrite"`
	Output     int     `json:"output"`
	Reasoning  int     `json:"reasoning"`
	Cost       float64 `json:"cost"`
	// PlatformCost is what the deployment's keys paid; SelfCost what users'
	// own keys paid.
	PlatformCost float64 `json:"platformCost"`
	SelfCost     float64 `json:"selfCost"`
	// Unpriced counts calls of models with no price (recorded at zero);
	// Incomplete counts calls whose usage is unknown (aborted or unreported).
	Unpriced   int `json:"unpriced"`
	Incomplete int `json:"incomplete"`

	cost, platform, self int64
}

func (t *usageTotals) add(e ledgerEntry) {
	t.Calls++
	t.Input += e.Input
	t.CacheRead += e.CacheRead
	t.CacheWrite += e.CacheWrite
	t.Output += e.Output
	t.Reasoning += e.Reasoning
	t.cost += e.Cost
	if e.BilledTo == billedSelf {
		t.self += e.Cost
	} else {
		t.platform += e.Cost
	}
	if !e.Priced {
		t.Unpriced++
	}
	if e.Status == usageAborted || e.Status == usageUnreported {
		t.Incomplete++
	}
	t.Cost, t.PlatformCost, t.SelfCost = yuan(t.cost), yuan(t.platform), yuan(t.self)
}

// usageGroup is one row of a breakdown.
type usageGroup struct {
	Key   string `json:"key"`
	Label string `json:"label,omitempty"`
	// Deleted marks a user row whose account no longer exists: the ledger keeps
	// their calls, and the report says whose they were.
	Deleted bool `json:"deleted,omitempty"`
	usageTotals
}

type grouper struct {
	rows  map[string]*usageGroup
	order []string
}

func (g *grouper) add(key, label string, e ledgerEntry) {
	if g.rows == nil {
		g.rows = map[string]*usageGroup{}
	}
	row := g.rows[key]
	if row == nil {
		row = &usageGroup{Key: key, Label: label}
		g.rows[key] = row
		g.order = append(g.order, key)
	}
	if label != "" {
		row.Label = label
	}
	row.add(e)
}

// byCost returns the rows, most expensive first.
func (g *grouper) byCost() []usageGroup {
	out := make([]usageGroup, 0, len(g.order))
	for _, key := range g.order {
		out = append(out, *g.rows[key])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].cost != out[j].cost {
			return out[i].cost > out[j].cost
		}
		return out[i].Calls > out[j].Calls
	})
	return out
}

// byKey returns the rows in key order (dates).
func (g *grouper) byKey() []usageGroup {
	out := g.byCost()
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// entryModel is the model a call is reported under: the one that answered.
func entryModel(e ledgerEntry) string {
	if e.ResponseModel != "" {
		return e.ResponseModel
	}
	return e.Model
}

type unpricedModel struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Calls    int    `json:"calls"`
}

type usageReportResponse struct {
	From     string          `json:"from"`
	To       string          `json:"to"`
	Currency string          `json:"currency"`
	Totals   usageTotals     `json:"totals"`
	ByDay    []usageGroup    `json:"byDay"`
	ByModel  []usageGroup    `json:"byModel"`
	ByUser   []usageGroup    `json:"byUser,omitempty"`
	Unpriced []unpricedModel `json:"unpriced"`
}

// usageEntryView is a ledger entry as a report shows it: cost in yuan.
type usageEntryView struct {
	ledgerEntry
	CostYuan float64 `json:"costYuan"`
}

func (s *apiServer) buildReport(entries []ledgerEntry, from, to time.Time, perUser bool) usageReportResponse {
	var (
		totals              usageTotals
		days, models, users grouper
		unpriced            = map[string]*unpricedModel{}
		unpricedOrder       []string
		knownUsers          = map[string]bool{}
	)
	for _, e := range entries {
		totals.add(e)
		days.add(e.At.In(time.Local).Format("2006-01-02"), "", e)
		models.add(e.Provider+"/"+entryModel(e), "", e)
		if perUser {
			users.add(e.UserID, e.Username, e)
		}
		if !e.Priced {
			key := e.Provider + "\x00" + entryModel(e)
			if unpriced[key] == nil {
				unpriced[key] = &unpricedModel{Provider: e.Provider, Model: entryModel(e)}
				unpricedOrder = append(unpricedOrder, key)
			}
			unpriced[key].Calls++
		}
	}
	report := usageReportResponse{
		From:     from.Format("2006-01-02"),
		To:       to.AddDate(0, 0, -1).Format("2006-01-02"),
		Currency: "CNY",
		Totals:   totals,
		ByDay:    days.byKey(),
		ByModel:  models.byCost(),
		Unpriced: []unpricedModel{},
	}
	if perUser {
		report.ByUser = users.byCost()
		for i := range report.ByUser {
			row := &report.ByUser[i]
			if row.Key == "" {
				row.Label = "（服务令牌）"
				continue
			}
			known, seen := knownUsers[row.Key]
			if !seen {
				_, known = s.auth.findUser(row.Key)
				knownUsers[row.Key] = known
			}
			row.Deleted = !known
		}
	}
	for _, key := range unpricedOrder {
		report.Unpriced = append(report.Unpriced, *unpriced[key])
	}
	return report
}

// reportQuery reads the shared filters. Dates are whole local days, both ends
// inclusive; the default period is the current month to date.
func reportQuery(r *http.Request) (ledgerQuery, time.Time, time.Time, error) {
	values := r.URL.Query()
	now := time.Now().In(time.Local)
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
	to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, 1)
	if raw := strings.TrimSpace(values.Get("from")); raw != "" {
		parsed, err := time.ParseInLocation("2006-01-02", raw, time.Local)
		if err != nil {
			return ledgerQuery{}, from, to, fmt.Errorf("from 须为 YYYY-MM-DD")
		}
		from = parsed
	}
	if raw := strings.TrimSpace(values.Get("to")); raw != "" {
		parsed, err := time.ParseInLocation("2006-01-02", raw, time.Local)
		if err != nil {
			return ledgerQuery{}, from, to, fmt.Errorf("to 须为 YYYY-MM-DD")
		}
		to = parsed.AddDate(0, 0, 1)
	}
	if !to.After(from) {
		return ledgerQuery{}, from, to, fmt.Errorf("结束日期不能早于开始日期")
	}
	return ledgerQuery{
		From:      from,
		To:        to,
		SessionID: strings.TrimSpace(values.Get("session")),
		Provider:  strings.TrimSpace(values.Get("provider")),
		Model:     strings.TrimSpace(values.Get("model")),
	}, from, to, nil
}

// --- reports -----------------------------------------------------------------

// handleMyUsage reports the caller's own calls. A session filter is honoured
// only for the caller's own session — the user filter below guarantees that.
// callsPage is one page of a period's calls, newest first.
type callsPage struct {
	Total int              `json:"total"`
	Page  int              `json:"page"`
	Size  int              `json:"size"`
	Items []usageEntryView `json:"items"`
}

const (
	defaultCallsPageSize = 50
	maxCallsPageSize     = 200
)

// pageCalls filters a period's entries (modelKey is a report's by-model key,
// provider/model; kind is "chat" or "compaction") and returns one page, newest
// first. "model" is not used here: reportQuery already reads it, as an exact
// ledger model.
func pageCalls(entries []ledgerEntry, values url.Values) callsPage {
	model := strings.TrimSpace(values.Get("modelKey"))
	kind := strings.TrimSpace(values.Get("kind"))
	size, _ := strconv.Atoi(values.Get("size"))
	if size <= 0 {
		size = defaultCallsPageSize
	}
	if size > maxCallsPageSize {
		size = maxCallsPageSize
	}
	page, _ := strconv.Atoi(values.Get("page"))
	if page < 1 {
		page = 1
	}
	var matched []ledgerEntry
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if model != "" && e.Provider+"/"+entryModel(e) != model {
			continue
		}
		if kind != "" && e.Kind != kind {
			continue
		}
		matched = append(matched, e)
	}
	out := callsPage{Total: len(matched), Page: page, Size: size, Items: []usageEntryView{}}
	for i := (page - 1) * size; i < len(matched) && i < page*size; i++ {
		out.Items = append(out.Items, usageEntryView{ledgerEntry: matched[i], CostYuan: yuan(matched[i].Cost)})
	}
	return out
}

// handleMyCalls pages through the caller's own calls.
func (s *apiServer) handleMyCalls(w http.ResponseWriter, r *http.Request) {
	principal := principalForRequest(r)
	query, _, _, err := reportQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !principal.Service {
		query.UserID = principal.UserID
	}
	entries, err := s.ledger.scan(query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pageCalls(entries, r.URL.Query()))
}

// handleAdminCalls pages through everyone's calls, optionally one user's.
func (s *apiServer) handleAdminCalls(w http.ResponseWriter, r *http.Request) {
	query, _, _, err := reportQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	query.UserID = strings.TrimSpace(r.URL.Query().Get("user"))
	entries, err := s.ledger.scan(query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pageCalls(entries, r.URL.Query()))
}

func (s *apiServer) handleMyUsage(w http.ResponseWriter, r *http.Request) {
	principal := principalForRequest(r)
	query, from, to, err := reportQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !principal.Service {
		query.UserID = principal.UserID
	}
	entries, err := s.ledger.scan(query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if r.URL.Query().Get("format") == "csv" {
		writeUsageCSV(w, entries, "my-usage")
		return
	}
	writeJSON(w, http.StatusOK, s.buildReport(entries, from, to, false))
}

// handleAdminUsage reports everyone's calls, optionally for one user.
func (s *apiServer) handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	query, from, to, err := reportQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	query.UserID = strings.TrimSpace(r.URL.Query().Get("user"))
	entries, err := s.ledger.scan(query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if r.URL.Query().Get("format") == "csv" {
		s.logAdminAction(r, "export usage", from.Format("2006-01-02")+".."+to.AddDate(0, 0, -1).Format("2006-01-02"))
		writeUsageCSV(w, entries, "usage")
		return
	}
	writeJSON(w, http.StatusOK, s.buildReport(entries, from, to, true))
}

// handleSessionUsage totals one session, for the running figure beside the
// model picker.
func (s *apiServer) handleSessionUsage(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.sessionForRequest(r, r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	managed.mu.Lock()
	meta := managed.meta
	managed.mu.Unlock()
	entries, err := s.ledger.scan(ledgerQuery{SessionID: meta.ID, From: meta.CreatedAt.Add(-time.Minute)})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var totals usageTotals
	for _, e := range entries {
		totals.add(e)
	}
	writeJSON(w, http.StatusOK, totals)
}

// writeUsageCSV exports calls one per row, oldest first. The UTF-8 byte order
// mark makes spreadsheet programs read the Chinese headers correctly.
func writeUsageCSV(w http.ResponseWriter, entries []ledgerEntry, name string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.csv"`, name, time.Now().Format("20060102")))
	_, _ = w.Write([]byte("\ufeff"))
	out := csv.NewWriter(w)
	_ = out.Write([]string{
		"时间", "用户", "用户ID", "会话", "轮次", "类型", "状态", "provider", "请求模型", "应答模型",
		"输入", "缓存命中", "缓存写入", "输出", "推理", "输入单价", "缓存命中单价", "缓存写入单价", "输出单价",
		"已定价", "费用(元)", "付费方", "密钥来源", "上游费用(USD)", "用量异常", "responseId",
	})
	for _, e := range entries {
		price := priceSnapshot{}
		if e.Price != nil {
			price = *e.Price
		}
		upstream := ""
		if e.UpstreamCostUSD != nil {
			upstream = strconv.FormatFloat(*e.UpstreamCostUSD, 'f', -1, 64)
		}
		_ = out.Write([]string{
			e.At.In(time.Local).Format("2006-01-02 15:04:05"), e.Username, e.UserID, e.SessionID, e.TurnID,
			e.Kind, e.Status, e.Provider, e.Model, e.ResponseModel,
			strconv.Itoa(e.Input), strconv.Itoa(e.CacheRead), strconv.Itoa(e.CacheWrite), strconv.Itoa(e.Output), strconv.Itoa(e.Reasoning),
			formatPrice(price.Input), formatPrice(price.CacheRead), formatPrice(price.CacheWrite), formatPrice(price.Output),
			strconv.FormatBool(e.Priced), strconv.FormatFloat(yuan(e.Cost), 'f', 9, 64), e.BilledTo, e.KeySource,
			upstream, strconv.FormatBool(e.UsageAnomaly), e.ResponseID,
		})
	}
	out.Flush()
}

func formatPrice(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// --- usage in history ---------------------------------------------------------

// turnUsage is one user turn's usage as shown under its reply. A turn's calls
// are found through the responses in its transcript, then widened to every call
// of the same turn — which brings in the compaction and aborted calls, whose
// responses never reach the transcript.
type turnUsage struct {
	usageTotals
	Details []*usageReport `json:"details"`
}

// attachTurnUsage puts each turn's usage on the turn's last assistant message.
func attachTurnUsage(messages []historyMessage, responseTurns map[string]int, entries []ledgerEntry) {
	if len(entries) == 0 {
		return
	}
	// Transcript turn → ledger turn ids, via the responses.
	turnIDs := map[int]map[string]bool{}
	for _, e := range entries {
		if e.ResponseID == "" {
			continue
		}
		if turn, ok := responseTurns[e.ResponseID]; ok {
			if turnIDs[turn] == nil {
				turnIDs[turn] = map[string]bool{}
			}
			turnIDs[turn][e.TurnID] = true
		}
	}
	byTurnID := map[string][]ledgerEntry{}
	for _, e := range entries {
		byTurnID[e.TurnID] = append(byTurnID[e.TurnID], e)
	}
	last := map[int]int{}
	for i, message := range messages {
		if message.Role == "assistant" {
			last[message.turn] = i
		}
	}
	for turn, ids := range turnIDs {
		index, ok := last[turn]
		if !ok {
			continue
		}
		sorted := make([]string, 0, len(ids))
		for id := range ids {
			sorted = append(sorted, id)
		}
		sort.Strings(sorted)
		usage := &turnUsage{}
		for _, id := range sorted {
			for _, e := range byTurnID[id] {
				usage.add(e)
				usage.Details = append(usage.Details, reportOf(e))
			}
		}
		messages[index].Usage = usage
	}
}

// sessionLedger reads a session's calls for its history.
func (s *apiServer) sessionLedger(meta sessionMeta) []ledgerEntry {
	entries, err := s.ledger.scan(ledgerQuery{SessionID: meta.ID, From: meta.CreatedAt.Add(-time.Minute)})
	if err != nil {
		return nil
	}
	return entries
}
