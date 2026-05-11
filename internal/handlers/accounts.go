package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jaro/trading-journal/internal/models"
)

// validFirms are the firms recognized by the commission_rates seed and the
// importer's GuessFirm rules. The empty string represents "not yet decided"
// and maps to NULL in the DB (firm is nullable).
var validFirms = map[string]bool{
	"":        true,
	"apex":    true,
	"lucid":   true,
	"topstep": true,
	"amp":     true,
	"ibkr":    true,
}

// validTypes accepts every type the system has ever issued. The dropdown
// uses accountTypeOptions() below to control what's offered to the user; any
// legacy value already saved in the DB still passes validation on subsequent
// edits so we don't trap users into re-typing their entire account list.
var validTypes = map[string]bool{
	"":            true,
	"prop_eval":   true,
	"prop_funded": true,
	"prop_live":   true,
	"personal":    true,
	"paper":       true,
	// Legacy values from before the prop_eval/funded/live split.
	"prop": true,
	"live": true,
}

// accountTypeOption pairs a slug with a human-readable label. Templates use
// .Value as the form value and .Label for display.
type accountTypeOption struct {
	Value string
	Label string
}

// accountTypeOptions returns the dropdown entries in order. Legacy values are
// included but suffixed "(legacy)" so the user can see and migrate away from
// them.
func accountTypeOptions() []accountTypeOption {
	return []accountTypeOption{
		{"", "—"},
		{"prop_eval", "Prop · Eval"},
		{"prop_funded", "Prop · Funded"},
		{"prop_live", "Prop · Live"},
		{"personal", "Personal"},
		{"paper", "Paper"},
		{"prop", "prop (legacy)"},
		{"live", "live (legacy)"},
	}
}

// AccountsPage renders the /accounts list. Each row is a small form that POSTs
// back to /accounts/{id} to update the row in place via HTMX.
func (h *Handlers) AccountsPage(w http.ResponseWriter, r *http.Request) {
	accounts, err := h.Queries.ListAccounts(r.Context())
	if err != nil {
		h.serverError(w, fmt.Errorf("list accounts: %w", err))
		return
	}
	h.renderPage(w, "accounts.html", map[string]any{
		"NavActive":   "accounts",
		"BackHref":    validBackHref(r),
		"Accounts":    accounts,
		"FirmOptions": []string{"", "apex", "lucid", "topstep", "amp", "ibkr"},
		"TypeOptions": accountTypeOptions(),
	})
}

// AccountUpdate handles POST /accounts/{id}: validates input, applies the
// update (auto-clearing needs_setup when both firm and type are set), and
// returns the updated row partial for HTMX to swap in.
func (h *Handlers) AccountUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	firm := strings.TrimSpace(r.FormValue("firm"))
	typ := strings.TrimSpace(r.FormValue("type"))
	currency := strings.TrimSpace(r.FormValue("currency"))
	active := r.FormValue("active") == "on" || r.FormValue("active") == "1"

	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if !validFirms[firm] {
		http.Error(w, "invalid firm: "+firm, http.StatusBadRequest)
		return
	}
	if !validTypes[typ] {
		http.Error(w, "invalid type: "+typ, http.StatusBadRequest)
		return
	}
	if currency == "" {
		currency = "USD"
	}

	// needs_setup auto-clears once firm and type are both filled in.
	needsSetup := int64(0)
	if firm == "" || typ == "" {
		needsSetup = 1
	}

	updated, err := h.Queries.UpdateAccount(ctx, models.UpdateAccountParams{
		Name:       name,
		Firm:       strPtrOrNil(firm),
		Type:       strPtrOrNil(typ),
		Currency:   currency,
		Active:     boolToInt(active),
		NeedsSetup: needsSetup,
		ID:         id,
	})
	if err != nil {
		h.serverError(w, fmt.Errorf("update account: %w", err))
		return
	}

	if err := h.recomputeAffectedTradesAfterFirmChange(ctx, id); err != nil {
		h.logger().Warn("recompute trades after account edit failed", "account_id", id, "err", err)
	}

	h.renderPartial(w, "_account_row.html", "account_row", map[string]any{
		"Account":     updated,
		"FirmOptions": []string{"", "apex", "lucid", "topstep", "amp", "ibkr"},
		"TypeOptions": []string{"", "prop", "live", "paper"},
	})
}

// recomputeAffectedTradesAfterFirmChange re-runs the position builder for
// every (account_id, symbol) pair the account already has trades for, so that
// commission and net P&L pick up a newly-set or changed firm.
func (h *Handlers) recomputeAffectedTradesAfterFirmChange(ctx context.Context, accountID int64) error {
	trades, err := h.Queries.ListTradesForAccount(ctx, accountID)
	if err != nil {
		return fmt.Errorf("list trades: %w", err)
	}
	if len(trades) == 0 {
		return nil
	}
	// Defer to ImportSvc, whose ingest path already wires the position builder.
	// We don't need a new entrypoint — call the rebuild directly through the
	// builder type. Wrap in a transaction so commission lookups + trade rewrites
	// stay consistent.
	return h.ImportSvc.RebuildTradesForAccount(ctx, accountID)
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

