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

// validTypes accepts every type slug the system honors. `live` is the
// canonical real-money type (it absorbed the older `personal` slug — see
// migration 20260511130000). `personal` is still accepted on input so an
// in-flight tab pasting that value doesn't error; the value flows through
// validation but new rows should be saved as `live`.
var validTypes = map[string]bool{
	"":            true,
	"prop_eval":   true,
	"prop_funded": true,
	"prop_live":   true,
	"live":        true,
	"paper":       true,
	// Accepted on input for tolerance; not offered in the dropdown.
	"personal": true,
	"prop":     true,
}

// accountTypeOption pairs a slug with a human-readable label. Templates use
// .Value as the form value and .Label for display.
type accountTypeOption struct {
	Value string
	Label string
}

// accountTypeOptions returns the dropdown entries in order. Legacy `prop`
// rows were migrated to `prop_eval` (migration 20260511140000) so the slug
// is no longer offered — still accepted on input via validTypes for tolerance.
func accountTypeOptions() []accountTypeOption {
	return []accountTypeOption{
		{"prop_eval", "Prop · Eval"},
		{"prop_funded", "Prop · Funded"},
		{"prop_live", "Prop · Live"},
		{"live", "Live"},
		{"paper", "Paper"},
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
		"Flash":       r.URL.Query().Get("flash"),
	})
}

// AccountsBulkSave handles POST /accounts/save — one form submission for
// every row on the page. Each row's inputs use the name "accounts.<id>.<field>"
// so the handler can iterate r.PostForm and update each account in turn.
// Returns 303 back to /accounts so a refresh doesn't resubmit.
func (h *Handlers) AccountsBulkSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	// Group form values by account id. Field names look like
	// "accounts.42.name", "accounts.42.firm", … plus a checkbox
	// "accounts.42.active" that's only present when checked.
	type rowInput struct {
		Name     string
		Firm     string
		Type     string
		Currency string
		Active   bool
	}
	byID := map[int64]*rowInput{}
	get := func(id int64) *rowInput {
		if r, ok := byID[id]; ok {
			return r
		}
		r := &rowInput{}
		byID[id] = r
		return r
	}
	for key, values := range r.PostForm {
		if !strings.HasPrefix(key, "accounts.") || len(values) == 0 {
			continue
		}
		rest := key[len("accounts."):]
		dot := strings.IndexByte(rest, '.')
		if dot < 0 {
			continue
		}
		idStr := rest[:dot]
		field := rest[dot+1:]
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			continue
		}
		ri := get(id)
		v := strings.TrimSpace(values[0])
		switch field {
		case "name":
			ri.Name = v
		case "firm":
			ri.Firm = v
		case "type":
			ri.Type = v
		case "currency":
			ri.Currency = v
		case "active":
			ri.Active = v == "on" || v == "1"
		}
	}

	// Accounts whose `active` checkbox wasn't submitted (unchecked) are
	// implicitly inactive — make sure we materialize their row too so we don't
	// silently drop the toggle. Pull every account once, walk via the form map.
	allAccounts, err := h.Queries.ListAccounts(ctx)
	if err != nil {
		h.serverError(w, fmt.Errorf("list accounts: %w", err))
		return
	}

	firmChanged := map[int64]bool{}
	for _, acc := range allAccounts {
		ri, present := byID[acc.ID]
		if !present {
			// No fields submitted for this account — leave it alone.
			continue
		}
		if ri.Name == "" {
			continue // require a name; skip silently rather than 400 the whole batch
		}
		if !validFirms[ri.Firm] || !validTypes[ri.Type] {
			continue // ignore rows with invalid enums
		}
		if ri.Currency == "" {
			ri.Currency = "USD"
		}
		needsSetup := int64(0)
		if ri.Firm == "" || ri.Type == "" {
			needsSetup = 1
		}

		prevFirm := ""
		if acc.Firm != nil {
			prevFirm = *acc.Firm
		}

		if _, err := h.Queries.UpdateAccount(ctx, models.UpdateAccountParams{
			Name:       ri.Name,
			Firm:       strPtrOrNil(ri.Firm),
			Type:       strPtrOrNil(ri.Type),
			Currency:   ri.Currency,
			Active:     boolToInt(ri.Active),
			NeedsSetup: needsSetup,
			ID:         acc.ID,
		}); err != nil {
			h.serverError(w, fmt.Errorf("update account %d: %w", acc.ID, err))
			return
		}
		if prevFirm != ri.Firm {
			firmChanged[acc.ID] = true
		}
	}

	for id := range firmChanged {
		if err := h.ImportSvc.RebuildTradesForAccount(ctx, id); err != nil {
			h.logger().Warn("recompute trades after bulk save firm change",
				"account_id", id, "err", err)
		}
	}

	http.Redirect(w, r, "/accounts?flash=saved", http.StatusSeeOther)
}

// AccountUpdate handles POST /accounts/{id}: validates input, applies the
// update (auto-clearing needs_setup when both firm and type are set), and
// returns the updated row partial for HTMX to swap in.
//
// Kept for the (now hidden) per-row save path so older HTMX-driven UIs keep
// working; the canonical save path is AccountsBulkSave.
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
		"TypeOptions": accountTypeOptions(),
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

