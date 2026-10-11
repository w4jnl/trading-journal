package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/copyset"
	"github.com/w4jnl/trading-journal/internal/importer"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/mwmail"
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

// emailFillAlert is one admin warning line on /accounts: MW email fills were
// captured for this account ref but never booked. Status tells the fix:
// "unknown" - no account matches the ref (set "acct id in emails" on the
// owning account, or import that account's CSV); "ambiguous" - several users
// own the ref; "unimported" - the ref resolves NOW but some captured orders
// never made it into executions (the alias was set after the fills, and no
// CSV import covered them yet).
type emailFillAlert struct {
	Ref       string
	Status    string // unknown | ambiguous | unimported
	OwnerName string // set for unimported
	Count     int
	LastSeen  time.Time
}

// retryEmailFillsFor re-ingests archived MotiveWave email captures whose
// account ref has just become resolvable - a manually created account, or an
// email alias pointed at an existing one. Those captures were skipped when
// they arrived because nothing owned the ref, and the normal replay never
// revisits a terminal outcome, so without this the operator would have to
// export and import a CSV covering the period purely to recover fills the
// app already holds.
//
// Best-effort by design: no MW archive, no importer, or a failing pass
// returns 0 and leaves the alert in place rather than failing the account
// save the user actually asked for.
func (h *Handlers) retryEmailFillsFor(ctx context.Context, refs ...string) int {
	if h.MWEmailArchiveDir == "" || h.ImportSvc == nil {
		return 0
	}
	set := map[string]bool{}
	for _, r := range refs {
		if r = strings.TrimSpace(r); r != "" {
			set[r] = true
		}
	}
	if len(set) == 0 {
		return 0
	}
	res, err := mwmail.RetryUnattributed(ctx, h.MWEmailArchiveDir, mwmail.ImporterIngest(h.ImportSvc), set)
	if err != nil {
		h.logger().Warn("mw email retry after account change failed", "err", err)
	}
	if res.Attempted > 0 {
		h.logger().Info("mw email fills retried after account change",
			"refs", refs, "attempted", res.Attempted, "ingested", res.Ingested, "skipped", res.Skipped)
	}
	return res.Ingested
}

// AccountsRetryEmailFills handles POST /accounts/email-fills/retry - the
// "Import them now" action on the admin alert card. It re-offers every
// captured-but-unattributed fill whose ref resolves TODAY, which covers the
// accounts that already existed when the emails were skipped (a ref created
// by a CSV import, or an alias set before this feature shipped). Creating an
// account or setting an alias retries on its own; this is the catch-up for
// everything already in that state.
func (h *Handlers) AccountsRetryEmailFills(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var refs []string
	for _, a := range h.emailFillAlerts(ctx) {
		// Only refs that resolve now can be booked; unknown/ambiguous ones
		// need the operator to act first.
		if a.Status == "unimported" {
			refs = append(refs, a.Ref)
		}
	}
	dest := "/accounts?flash=saved"
	if n := h.retryEmailFillsFor(ctx, refs...); n > 0 {
		dest = "/accounts?mwfills=" + strconv.Itoa(n)
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// emailFillAlerts recomputes the alert list from the capture archive's replay
// journal, filtered against CURRENT data - entries that resolve and whose
// orders are imported disappear on their own. Empty when the email listener
// is disabled. Best-effort: errors log and hide the card rather than break
// the page.
func (h *Handlers) emailFillAlerts(ctx context.Context) []emailFillAlert {
	if h.MWEmailArchiveDir == "" {
		return nil
	}
	fills, err := mwmail.UnprocessedFills(h.MWEmailArchiveDir)
	if err != nil {
		h.logger().Warn("unprocessed email fills scan failed", "err", err)
		return nil
	}
	byRef := map[string][]mwmail.UnprocessedFill{}
	var order []string
	for _, f := range fills {
		if f.Account == "" {
			continue
		}
		if _, seen := byRef[f.Account]; !seen {
			order = append(order, f.Account)
		}
		byRef[f.Account] = append(byRef[f.Account], f)
	}
	var out []emailFillAlert
	for _, ref := range order {
		group := byRef[ref]
		a := emailFillAlert{Ref: ref}
		owners, err := h.Queries.ListAccountsByRefAnyUser(ctx, models.ListAccountsByRefAnyUserParams{
			ImportSource: "motivewave_csv",
			Ref:          ref,
		})
		if err != nil {
			continue
		}
		switch {
		case len(owners) == 0:
			a.Status = "unknown"
			for _, f := range group {
				a.Count++
				if f.ExecTime.After(a.LastSeen) {
					a.LastSeen = f.ExecTime
				}
			}
		case len(owners) > 1:
			a.Status = "ambiguous"
			a.Count = len(group)
		default:
			// Resolves today - only orders still absent from executions count.
			acct := owners[0]
			for _, f := range group {
				if f.OrderID == "" {
					continue
				}
				oid := f.OrderID
				if _, err := h.Queries.GetExecutionByOrderID(ctx, models.GetExecutionByOrderIDParams{
					AccountID: acct.ID, OrderID: &oid,
				}); errors.Is(err, sql.ErrNoRows) {
					a.Count++
					if f.ExecTime.After(a.LastSeen) {
						a.LastSeen = f.ExecTime
					}
				}
			}
			if a.Count == 0 {
				continue // fully covered by later imports - stale journal history
			}
			a.Status = "unimported"
			a.OwnerName = acct.Name
		}
		out = append(out, a)
	}
	return out
}

// AccountsPage renders the /accounts list. Each row is a small form that POSTs
// back to /accounts/{id} to update the row in place via HTMX.
func (h *Handlers) AccountsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accounts, err := h.Queries.ListAccounts(ctx, auth.MustUser(ctx).ID)
	if err != nil {
		h.serverError(w, fmt.Errorf("list accounts: %w", err))
		return
	}
	// Active accounts first, retired (active=0) at the bottom. The base
	// query orders by name; this is a stable secondary sort, so within
	// each group names stay alphabetical.
	sort.SliceStable(accounts, func(i, j int) bool {
		return accounts[i].Active > accounts[j].Active
	})
	balances, err := h.loadBalances(ctx, time.Now())
	if err != nil {
		h.serverError(w, fmt.Errorf("load balances: %w", err))
		return
	}
	flashRaw := r.URL.Query().Get("flash")
	flashErr := r.URL.Query().Get("err")
	clonedID := r.URL.Query().Get("id") // only meaningful when flash=cloned
	movedCount := int64(0)
	deletedName := ""
	recomputedOK := int64(0)
	recomputedFail := int64(0)
	// "moved=N" / "deleted=NAME" / "recomputed=N[.F]" flashes encode their
	// payload after '=' so the template can render a specific message. Other
	// flashes pass through.
	flash := flashRaw
	switch {
	case strings.HasPrefix(flashRaw, "moved="):
		if n, err := strconv.ParseInt(flashRaw[len("moved="):], 10, 64); err == nil {
			movedCount = n
		}
		flash = ""
	case strings.HasPrefix(flashRaw, "deleted="):
		deletedName = flashRaw[len("deleted="):]
		flash = ""
	case strings.HasPrefix(flashRaw, "recomputed="):
		parts := strings.SplitN(flashRaw[len("recomputed="):], ".", 2)
		if n, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
			recomputedOK = n
		}
		if len(parts) == 2 {
			if n, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
				recomputedFail = n
			}
		}
		flash = ""
	}
	// Count of captured email fills booked by the save/create that redirected
	// here (see retryEmailFillsFor) - 0 means there were none to recover.
	mwFills, _ := strconv.Atoi(r.URL.Query().Get("mwfills"))

	// Unattributable email-fill warnings are operator material: the email
	// names no journal user, so only admins see the card.
	var emailAlerts []emailFillAlert
	canRetry := false
	if auth.MustUser(ctx).IsAdmin {
		emailAlerts = h.emailFillAlerts(ctx)
		for _, a := range emailAlerts {
			// Only a ref that resolves today can be booked from the archive;
			// unknown/ambiguous ones need the operator to act first.
			if a.Status == "unimported" {
				canRetry = true
				break
			}
		}
	}
	h.renderPage(w, r, "accounts.html", map[string]any{
		"NavActive":          "accounts",
		"BackHref":           validBackHref(r),
		"Accounts":           accounts,
		"EmailAlerts":        emailAlerts,
		"CanRetryEmailFills": canRetry && h.MWEmailArchiveDir != "" && h.ImportSvc != nil,
		"TopstepxSync":       h.TopstepxSync != nil,
		"FirmOptions":        []string{"", "apex", "lucid", "topstep", "amp", "ibkr"},
		"TypeOptions":        accountTypeOptions(),
		"Flash":              flash,
		"FlashErr":           flashErr,
		"MWFills":            mwFills,
		"MovedCount":         movedCount,
		"DeletedName":        deletedName,
		"RecomputedOK":       recomputedOK,
		"RecomputedFail":     recomputedFail,
		"Balances":           balances,
		"Today":              time.Now().In(h.localTZ()).Format("2006-01-02"),
		"ClonedID":           clonedID,
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
		EmailRef string
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
		case "email_ref":
			ri.EmailRef = v
		case "active":
			ri.Active = v == "on" || v == "1"
		}
	}

	// Accounts whose `active` checkbox wasn't submitted (unchecked) are
	// implicitly inactive — make sure we materialize their row too so we don't
	// silently drop the toggle. Pull every account once, walk via the form map.
	allAccounts, err := h.Queries.ListAccounts(ctx, auth.MustUser(ctx).ID)
	if err != nil {
		h.serverError(w, fmt.Errorf("list accounts: %w", err))
		return
	}

	firmChanged := map[int64]bool{}
	var aliasRefs []string
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
		prevAlias := ""
		if acc.EmailRef != nil {
			prevAlias = *acc.EmailRef
		}

		if _, err := h.Queries.UpdateAccount(ctx, models.UpdateAccountParams{
			Name:       ri.Name,
			Firm:       strPtrOrNil(ri.Firm),
			Type:       strPtrOrNil(ri.Type),
			Currency:   ri.Currency,
			Active:     boolToInt(ri.Active),
			NeedsSetup: needsSetup,
			EmailRef:   strPtrOrNil(ri.EmailRef),
			ID:         acc.ID,
			UserID:     auth.MustUser(ctx).ID,
		}); err != nil {
			h.serverError(w, fmt.Errorf("update account %d: %w", acc.ID, err))
			return
		}
		if prevFirm != ri.Firm {
			firmChanged[acc.ID] = true
		}
		// A newly pointed email alias is the other way a stranded capture
		// becomes attributable (the AMP/CQG case: emails carry a different id
		// than the CSV). Retry that ref, and the account's own, once saved.
		if ri.EmailRef != "" && ri.EmailRef != prevAlias {
			aliasRefs = append(aliasRefs, ri.EmailRef, acc.AccountRef)
		}
	}

	for id := range firmChanged {
		if err := h.ImportSvc.RebuildTradesForAccount(ctx, id); err != nil {
			h.logger().Warn("recompute trades after bulk save firm change",
				"account_id", id, "err", err)
		}
	}
	if len(firmChanged) > 0 {
		h.LiveData.Bump(userID(ctx))
	}

	dest := "/accounts?flash=saved"
	if n := h.retryEmailFillsFor(ctx, aliasRefs...); n > 0 {
		dest += "&mwfills=" + strconv.Itoa(n)
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// AccountsCreate handles POST /accounts/new — a small manual-create form
// from the /accounts page. Fields: name (required), firm, type, currency,
// account_ref (defaults to name). import_source is derived from firm so a
// manually-created account converges with a future import for the same
// (import_source, account_ref) pair.
func (h *Handlers) AccountsCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	firm := strings.TrimSpace(r.FormValue("firm"))
	typ := strings.TrimSpace(r.FormValue("type"))
	currency := strings.TrimSpace(r.FormValue("currency"))
	ref := strings.TrimSpace(r.FormValue("account_ref"))

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
	if ref == "" {
		ref = name
	}
	// import_source: pick the CSV channel a future automated import would
	// arrive on, so manual + import accounts merge naturally.
	importSrc := "manual"
	switch firm {
	case "topstep":
		importSrc = "topstepx_csv"
	case "apex", "lucid", "amp", "ibkr":
		importSrc = "motivewave_csv"
	}
	needsSetup := int64(0)
	if firm == "" || typ == "" {
		needsSetup = 1
	}
	if _, err := h.Queries.CreateAccount(ctx, models.CreateAccountParams{
		UserID:           auth.MustUser(ctx).ID,
		Name:             name,
		Firm:             strPtrOrNil(firm),
		ImportSource:     importSrc,
		AccountRef:       ref,
		Type:             strPtrOrNil(typ),
		Currency:         currency,
		NeedsSetup:       needsSetup,
		CommissionSource: "rate",
	}); err != nil {
		// Most likely a unique-key collision on (import_source, account_ref).
		// Surface it inline so the user sees the message.
		http.Redirect(w, r, "/accounts?flash=createfail&err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	// The new account may be exactly what a pile of captured-but-unattributed
	// email fills were waiting for; book them now instead of asking for a CSV.
	dest := "/accounts?flash=created"
	if n := h.retryEmailFillsFor(ctx, ref); n > 0 {
		dest += "&mwfills=" + strconv.Itoa(n)
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// AccountsClone handles POST /accounts/{id}/clone — creates a new account
// that inherits firm/type/currency/import_source from the source account
// and optionally upserts a balance anchor for today. The only required
// field is the new account_ref; the user types that and (optionally) an
// opening balance in the inline form on /accounts.
//
// On success: redirect to /accounts#row-{newID} so the browser scrolls
// to the freshly created row. For HTMX submits we emit HX-Redirect with
// the same URL — htmx performs a real navigation so the fragment scroll
// fires naturally (a partial swap would require factoring the whole row
// template into a partial, which is out of scope).
func (h *Handlers) AccountsClone(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	idStr := r.PathValue("id")
	srcID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad source id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	ref := strings.TrimSpace(r.FormValue("account_ref"))
	if ref == "" {
		respondClone(w, r, 0, "/accounts?flash=createfail&err="+url.QueryEscape("account ref is required"))
		return
	}
	balStr := strings.TrimSpace(r.FormValue("balance"))
	balStr = strings.ReplaceAll(balStr, ",", "")
	var balance float64
	hasBalance := false
	if balStr != "" {
		v, err := strconv.ParseFloat(balStr, 64)
		if err != nil {
			respondClone(w, r, 0, "/accounts?flash=createfail&err="+url.QueryEscape("opening balance must be a number"))
			return
		}
		balance = v
		hasBalance = true
	}

	src, err := h.Queries.GetAccountByID(ctx, models.GetAccountByIDParams{ID: srcID, UserID: auth.MustUser(ctx).ID})
	if err != nil {
		http.Error(w, "source account not found", http.StatusNotFound)
		return
	}

	// Inherit everything except the ref (and name, which gets " (copy)"
	// appended). CommissionSource starts as "rate" — an inline-trades-CSV
	// claim only makes sense once a trades CSV actually lands for this
	// account, which is its own import flow.
	needsSetup := int64(0)
	if src.Firm == nil || src.Type == nil {
		needsSetup = 1
	}
	created, err := h.Queries.CreateAccount(ctx, models.CreateAccountParams{
		UserID:           auth.MustUser(ctx).ID,
		Name:             src.Name + " (copy)",
		Firm:             src.Firm,
		ImportSource:     src.ImportSource,
		AccountRef:       ref,
		Type:             src.Type,
		Currency:         src.Currency,
		NeedsSetup:       needsSetup,
		CommissionSource: "rate",
	})
	if err != nil {
		// Most likely a unique-key collision on (import_source, account_ref).
		respondClone(w, r, 0, "/accounts?flash=createfail&err="+url.QueryEscape(err.Error()))
		return
	}

	if hasBalance {
		tz := h.localTZ()
		today := time.Now().In(tz)
		anchorDate := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, tz)
		if err := h.Queries.UpsertBalanceAnchor(ctx, models.UpsertBalanceAnchorParams{
			AccountID:  created.ID,
			AnchorDate: anchorDate,
			Balance:    balance,
			Note:       nil,
		}); err != nil {
			h.serverError(w, fmt.Errorf("upsert anchor on clone: %w", err))
			return
		}
	}

	dest := fmt.Sprintf("/accounts?flash=cloned&id=%d#row-%d", created.ID, created.ID)
	respondClone(w, r, created.ID, dest)
}

// respondClone unifies the HTMX vs plain-form response path for the
// clone endpoint. HTMX gets HX-Redirect (htmx does a real browser
// navigation, so URL fragments scroll-to-row). Non-HTMX gets the
// classic 303.
func respondClone(w http.ResponseWriter, r *http.Request, _ int64, dest string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", dest)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// AccountsReassign handles POST /accounts/{id}/reassign with form field `to`
// = target account id. Moves trades, executions, and journal_entries from
// the source account to the target, then refreshes daily_stats on both.
//
// The source account is left in place (empty); user can delete it manually
// once they confirm the move worked. The transaction guarantees that either
// the entire move lands or nothing does.
func (h *Handlers) AccountsReassign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	srcStr := r.PathValue("id")
	srcID, err := strconv.ParseInt(srcStr, 10, 64)
	if err != nil {
		http.Error(w, "bad source id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	dstStr := strings.TrimSpace(r.FormValue("to"))
	dstID, err := strconv.ParseInt(dstStr, 10, 64)
	if err != nil {
		http.Error(w, "bad target id", http.StatusBadRequest)
		return
	}
	if dstID == srcID {
		http.Redirect(w, r, "/accounts?flash=reassignsame", http.StatusSeeOther)
		return
	}
	// Ownership: both accounts must be the caller's. A foreign source would
	// let a user pull another tenant's whole history into their own account;
	// a foreign destination would scatter their data into someone else's.
	// The reassign queries themselves are keyed on account_id only.
	uid := userID(ctx)
	if _, err := h.Queries.GetAccountByID(ctx, models.GetAccountByIDParams{ID: srcID, UserID: uid}); err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := h.Queries.GetAccountByID(ctx, models.GetAccountByIDParams{ID: dstID, UserID: uid}); err != nil {
		http.Error(w, "target account not found", http.StatusNotFound)
		return
	}

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		h.serverError(w, fmt.Errorf("begin: %w", err))
		return
	}
	defer tx.Rollback()
	q := h.Queries.WithTx(tx)

	tradesMoved, err := q.ReassignTradesAccount(ctx, models.ReassignTradesAccountParams{
		AccountID:   dstID, // new value
		AccountID_2: srcID, // WHERE account_id = ?
	})
	if err != nil {
		h.serverError(w, fmt.Errorf("reassign trades: %w", err))
		return
	}
	if _, err := q.ReassignExecutionsAccount(ctx, models.ReassignExecutionsAccountParams{
		AccountID: dstID, AccountID_2: srcID,
	}); err != nil {
		h.serverError(w, fmt.Errorf("reassign executions: %w", err))
		return
	}
	if _, err := q.ReassignJournalEntriesAccount(ctx, models.ReassignJournalEntriesAccountParams{
		AccountID: &dstID, AccountID_2: &srcID,
	}); err != nil {
		h.serverError(w, fmt.Errorf("reassign journal: %w", err))
		return
	}
	tz := h.Timezone
	if err := importer.RefreshDailyStatsForAccount(ctx, q, srcID, tz); err != nil {
		h.serverError(w, fmt.Errorf("refresh src stats: %w", err))
		return
	}
	if _, err := copyset.RebuildForAccount(ctx, q, dstID); err != nil {
		h.serverError(w, fmt.Errorf("rebuild copy sets: %w", err))
		return
	}
	if err := importer.RefreshDailyStatsForAccount(ctx, q, dstID, tz); err != nil {
		h.serverError(w, fmt.Errorf("refresh dst stats: %w", err))
		return
	}
	if err := tx.Commit(); err != nil {
		h.serverError(w, fmt.Errorf("commit: %w", err))
		return
	}

	// The destination now holds two accounts' fills, possibly for the same
	// contract: replay its FIFO queues so trades reflect the merged stream
	// and carry the destination's commission source, instead of waiting for
	// the next import to happen to rebuild that symbol.
	if h.ImportSvc != nil {
		if err := h.ImportSvc.RebuildTradesForAccount(ctx, dstID); err != nil {
			h.logger().Warn("reassign: rebuild destination failed", "account", dstID, "err", err)
		}
	}
	h.LiveData.Bump(uid)
	flash := fmt.Sprintf("moved%%3D%d", tradesMoved) // moved=<n>
	http.Redirect(w, r, "/accounts?flash="+flash, http.StatusSeeOther)
}

// AccountsDelete handles POST /accounts/{id}/delete — permanently removes
// the account AND every row that references it: journal entries, trades
// (which cascade-delete their trade_executions), executions, daily_stats.
// sync_log rows are kept but their account_id is nulled so the historical
// import log stays intact.
//
// The whole thing runs in a single transaction; SQLite's FK enforcement
// will trip immediately if anything is mis-ordered.
func (h *Handlers) AccountsDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}

	acct, err := h.Queries.GetAccountByID(ctx, models.GetAccountByIDParams{ID: id, UserID: auth.MustUser(ctx).ID})
	if err != nil {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		h.serverError(w, fmt.Errorf("begin: %w", err))
		return
	}
	defer tx.Rollback()

	// Order matters. journal_entries.{account_id, trade_id} and
	// trades.account_id are NO ACTION FKs — delete children first.
	// trade_executions has ON DELETE CASCADE on trades.id, so it drops
	// automatically when we delete trades.
	statements := []struct {
		label string
		sql   string
	}{
		{"journal entries", `
			DELETE FROM journal_entries
			WHERE account_id = ?1
			   OR trade_id IN (SELECT id FROM trades WHERE account_id = ?1)
		`},
		{"trades", `DELETE FROM trades WHERE account_id = ?1`},
		{"executions", `DELETE FROM executions WHERE account_id = ?1`},
		{"daily stats", `DELETE FROM daily_stats WHERE account_id = ?1`},
		{"sync log", `UPDATE sync_log SET account_id = NULL WHERE account_id = ?1`},
		{"account", `DELETE FROM accounts WHERE id = ?1`},
	}
	for _, st := range statements {
		if _, err := tx.ExecContext(ctx, st.sql, id); err != nil {
			h.serverError(w, fmt.Errorf("delete %s: %w", st.label, err))
			return
		}
	}

	if err := tx.Commit(); err != nil {
		h.serverError(w, fmt.Errorf("commit: %w", err))
		return
	}

	h.LiveData.Bump(userID(ctx))
	flash := "deleted=" + url.QueryEscape(acct.Name)
	http.Redirect(w, r, "/accounts?flash="+url.QueryEscape(flash), http.StatusSeeOther)
}

// AccountsRecomputeAll handles POST /accounts/recompute — re-runs the position
// builder for every account, rebuilding trades and daily_stats from scratch.
// Used after a config change (e.g., trading timezone) when the calendar /
// analytics views need to re-bucket existing data.
func (h *Handlers) AccountsRecomputeAll(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accounts, err := h.Queries.ListAccounts(ctx, auth.MustUser(ctx).ID)
	if err != nil {
		h.serverError(w, fmt.Errorf("list accounts: %w", err))
		return
	}
	var rebuilt, failed int
	for _, a := range accounts {
		if err := h.ImportSvc.RebuildTradesForAccount(ctx, a.ID); err != nil {
			h.logger().Warn("recompute trades failed", "account_id", a.ID, "err", err)
			failed++
			continue
		}
		rebuilt++
	}
	if rebuilt > 0 {
		h.LiveData.Bump(userID(ctx))
	}
	flash := fmt.Sprintf("recomputed=%d", rebuilt)
	if failed > 0 {
		flash = fmt.Sprintf("recomputed=%d.%d", rebuilt, failed)
	}
	http.Redirect(w, r, "/accounts?flash="+flash, http.StatusSeeOther)
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
		UserID:     auth.MustUser(ctx).ID,
	})
	if err != nil {
		h.serverError(w, fmt.Errorf("update account: %w", err))
		return
	}

	if err := h.recomputeAffectedTradesAfterFirmChange(ctx, id); err != nil {
		h.logger().Warn("recompute trades after account edit failed", "account_id", id, "err", err)
	} else {
		h.LiveData.Bump(userID(ctx))
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

// AccountsRefetch: POST /accounts/{id}/refetch - queue a full TopstepX API
// history re-fetch for one account (ignores the incremental window and the
// api_backfill_done flag; dedup makes re-pulling safe). Only meaningful for
// TopstepX fill-based accounts; inline (trades-CSV) accounts are refused, the
// same rule the sync service applies.
func (h *Handlers) AccountsRefetch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.TopstepxSync == nil {
		http.Redirect(w, r, "/accounts?err="+errToQuery(fmt.Errorf("TopstepX sync is not configured on this server")), http.StatusSeeOther)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	uid := auth.MustUser(ctx).ID
	acct, err := h.Queries.GetAccountByID(ctx, models.GetAccountByIDParams{ID: id, UserID: uid})
	if err != nil {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	if acct.ImportSource != string(importer.FormatTopstepX) {
		http.Redirect(w, r, "/accounts?err=only+TopstepX+accounts+can+be+re-fetched", http.StatusSeeOther)
		return
	}
	claim, err := h.Queries.GetAccountImportClaim(ctx, models.GetAccountImportClaimParams{ID: acct.ID, UserID: uid})
	if err != nil {
		h.serverError(w, fmt.Errorf("read import claim: %w", err))
		return
	}
	if claim == "trades" {
		http.Redirect(w, r, "/accounts?err=account+is+trades-CSV+(inline)+imported;+API+sync+is+disabled+for+it", http.StatusSeeOther)
		return
	}
	h.TopstepxSync.EnqueueAccountRefetch(uid, acct.ID)
	http.Redirect(w, r, "/accounts?flash=refetch-queued", http.StatusSeeOther)
}
