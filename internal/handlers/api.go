package handlers

import (
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// APIMux builds the /api/v1 route table. It's mounted at "/api/" in main.go and
// wrapped by auth.APIKeyAuth.Middleware, which injects the key's user into the
// request context - so every handler below can use userID(ctx) and reuse the
// same user-scoped loaders the HTML app uses. Patterns are the FULL path
// because http.ServeMux subtree dispatch does not strip the prefix.
func (h *Handlers) APIMux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/v1", h.APIIndex)
	m.HandleFunc("GET /api/v1/openapi.json", h.APIOpenAPI)
	m.HandleFunc("GET /api/v1/strategies", h.APIStrategiesList)
	m.HandleFunc("GET /api/v1/strategies/{id}", h.APIStrategyDetail)
	m.HandleFunc("GET /api/v1/journal", h.APIJournalList)
	m.HandleFunc("POST /api/v1/journal", h.APIJournalCreate)
	m.HandleFunc("GET /api/v1/journal/{id}", h.APIJournalDetail)
	m.HandleFunc("POST /api/v1/journal/{id}/notes", h.APIJournalAppendNote)
	m.HandleFunc("GET /api/v1/journal/{id}/chart.png", h.APIJournalChartPNG)
	m.HandleFunc("GET /api/v1/trades", h.APITradesList)
	m.HandleFunc("GET /api/v1/trades/{id}", h.APITradeDetail)
	m.HandleFunc("GET /api/v1/trades/{id}/chart.png", h.APITradeChartPNG)
	m.HandleFunc("GET /api/v1/attachments/{id}", h.APIAttachmentServe)
	return m
}

// ---- API response projections (camelCase; we never serialize sqlc model
// structs directly - they're snake_case and models.User carries password_hash) ----

type apiAttachment struct {
	ID  int64  `json:"id"`
	URL string `json:"url"`
}

type apiStrategy struct {
	ID          int64           `json:"id"`
	Name        string          `json:"name"`
	Body        string          `json:"body"`
	Attachments []apiAttachment `json:"attachments"`
	CreatedAt   time.Time       `json:"createdAt"`
	UpdatedAt   time.Time       `json:"updatedAt"`
}

type apiStrategyRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type apiTrade struct {
	ID         int64      `json:"id"`
	Symbol     string     `json:"symbol"`
	SymbolRoot string     `json:"symbolRoot"`
	Direction  string     `json:"direction"`
	Qty        float64    `json:"qty"`
	Account    string     `json:"account"`
	EntryTime  time.Time  `json:"entryTime"`
	ExitTime   *time.Time `json:"exitTime"`
	GrossPnl   *float64   `json:"grossPnl"`
	Commission *float64   `json:"commission"`
	NetPnl     *float64   `json:"netPnl"`
}

type apiSummary struct {
	Trades     int64   `json:"trades"`
	Net        float64 `json:"net"`
	Gross      float64 `json:"gross"`
	Commission float64 `json:"commission"`
	Wins       int64   `json:"wins"`
	Losses     int64   `json:"losses"`
}

func apiTradesFrom(trades []linkedTrade) []apiTrade {
	out := make([]apiTrade, 0, len(trades))
	for _, t := range trades {
		out = append(out, apiTrade{
			ID: t.ID, Symbol: t.Symbol, SymbolRoot: t.SymbolRoot, Direction: t.Direction,
			Qty: t.Qty, Account: t.AccountName, EntryTime: t.EntryTime, ExitTime: t.ExitTime,
			GrossPnl: t.GrossPnl, Commission: t.Commission, NetPnl: t.NetPnl,
		})
	}
	return out
}

func apiSummaryFrom(s GroupSummary) apiSummary {
	return apiSummary{Trades: s.N, Net: s.Net, Gross: s.Gross, Commission: s.Commission, Wins: s.Wins, Losses: s.Losses}
}

// attachmentRefRe matches the /journal/attachments/{id} URLs embedded in journal
// and strategy markdown bodies.
var attachmentRefRe = regexp.MustCompile(`/journal/attachments/(\d+)`)

// apiRewriteAttachmentRefs rewrites in-body image refs to the API path
// (/api/v1/attachments/{id}) and returns the rewritten markdown plus the deduped
// referenced attachment ids in first-seen order. Used for both strategy bodies
// and journal notes. Body refs are the only reliable source of an item's images:
// journal_attachments.entry_id is left 0 by the save handlers and strategies have
// no attachment FK at all.
func apiRewriteAttachmentRefs(md string) (string, []int64) {
	var ids []int64
	seen := map[int64]bool{}
	out := attachmentRefRe.ReplaceAllStringFunc(md, func(m string) string {
		idStr := m[len("/journal/attachments/"):]
		id, _ := strconv.ParseInt(idStr, 10, 64)
		if id > 0 && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
		return "/api/v1/attachments/" + idStr
	})
	return out, ids
}

func apiAttachmentsFrom(ids []int64) []apiAttachment {
	out := make([]apiAttachment, 0, len(ids))
	for _, id := range ids {
		out = append(out, apiAttachment{ID: id, URL: "/api/v1/attachments/" + strconv.FormatInt(id, 10)})
	}
	return out
}

// apiPathID parses the {id} path value as an int64.
func apiPathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
