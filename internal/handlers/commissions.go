package handlers

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
)

// commissionBaseline is the sentinel effective_from the migration backfills onto
// pre-existing rates (and the create handler uses when the user leaves the
// "effective from" field blank). A version at this instant covers all history.
var commissionBaseline = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

func errToQuery(err error) string {
	return url.QueryEscape(err.Error())
}

// firmsForCommissions are the firms that look up rates through the table.
// Topstep is included for accounts imported from the orders CSV (no inline
// commission column); accounts imported from the trades CSV are flagged
// commission_source = 'inline' and bypass the rate lookup entirely.
var firmsForCommissions = []string{"apex", "lucid", "topstep", "amp", "ibkr"}

// orphanReason groups orphaned trades by why they're missing a commission,
// so the diagnostics banner can show one row per fixable problem.
type orphanReason struct {
	Kind        string // "symbol_uncategorized" | "account_firm_missing" | "rate_missing"
	SymbolRoot  string // populated for symbol_uncategorized and rate_missing
	AccountID   int64  // populated for account_firm_missing
	AccountName string // populated for account_firm_missing
	Firm        string // populated for rate_missing
	AssetClass  string // populated for rate_missing
	SizeClass   string // populated for rate_missing
	TradeCount  int64
}

// commissionVersion is one effective-dated rate row, prepared for the template.
type commissionVersion struct {
	ID            int64
	EffectiveFrom time.Time
	PerSide       float64
	IsBaseline    bool // effective_from is the 1970 sentinel: label it "initial"
}

// commissionCell is one (asset_class, firm, size_class) commission cell with its
// current rate (the version in effect now) and full version history.
type commissionCell struct {
	Key        string // "asset|firm|size", used for row ids and anchors
	AssetClass string
	Firm       string
	SizeClass  string
	Current    float64
	HasCurrent bool // false only if every version is future-dated
	Versions   []commissionVersion
}

// buildCommissionCells groups the flat, cell-then-effective_from-ordered rate
// list into one cell per (asset, firm, size). "Current" is the version with the
// greatest effective_from <= now (matches the builder's rateAt as-of pick).
func buildCommissionCells(rates []models.CommissionRate, now time.Time) []commissionCell {
	byKey := map[string]*commissionCell{}
	var order []string
	for _, rt := range rates {
		key := rateKey(rt.AssetClass, rt.Firm, rt.SizeClass)
		cell, ok := byKey[key]
		if !ok {
			cell = &commissionCell{
				Key: key, AssetClass: rt.AssetClass, Firm: rt.Firm, SizeClass: rt.SizeClass,
			}
			byKey[key] = cell
			order = append(order, key)
		}
		cell.Versions = append(cell.Versions, commissionVersion{
			ID:            rt.ID,
			EffectiveFrom: rt.EffectiveFrom,
			PerSide:       rt.PerContractPerSide,
			IsBaseline:    !rt.EffectiveFrom.After(commissionBaseline),
		})
		// rates arrive effective_from ASC, so the last version <= now is current.
		if !rt.EffectiveFrom.After(now) {
			cell.Current = rt.PerContractPerSide
			cell.HasCurrent = true
		}
	}
	cells := make([]commissionCell, 0, len(order))
	for _, key := range order {
		cells = append(cells, *byKey[key])
	}
	return cells
}

// CommissionsPage renders the /commissions admin page: diagnostics banner at
// the top, then the rate matrix, instrument categorization, and a collapsible
// class-definition section.
func (h *Handlers) CommissionsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	assetClasses, err := h.Queries.ListAssetClasses(ctx)
	if err != nil {
		h.serverError(w, fmt.Errorf("list asset classes: %w", err))
		return
	}
	sizeClasses, err := h.Queries.ListSizeClasses(ctx)
	if err != nil {
		h.serverError(w, fmt.Errorf("list size classes: %w", err))
		return
	}
	rates, err := h.Queries.ListCommissionRates(ctx)
	if err != nil {
		h.serverError(w, fmt.Errorf("list rates: %w", err))
		return
	}
	instruments, err := h.Queries.ListInstruments(ctx)
	if err != nil {
		h.serverError(w, fmt.Errorf("list instruments: %w", err))
		return
	}
	orphanRows, err := h.Queries.ListOrphanedCommissionTrades(ctx, userID(ctx))
	if err != nil {
		h.serverError(w, fmt.Errorf("list orphans: %w", err))
		return
	}

	// Bucket orphans by reason. A single (account, symbol) row can only fall
	// into one bucket — the LEFT JOIN with `cr.per_contract_per_side IS NULL` filter
	// already excludes trades that have a matching rate row.
	symAccum := map[string]int64{}          // symbol_root -> count (uncategorized symbol)
	firmAccum := map[int64]*orphanReason{}  // account_id -> entry (missing firm)
	rateAccum := map[string]*orphanReason{} // "asset_class|firm|size_class" -> entry
	for _, o := range orphanRows {
		switch {
		case o.AccountFirm == nil:
			ent, ok := firmAccum[o.AccountID]
			if !ok {
				ent = &orphanReason{
					Kind: "account_firm_missing", AccountID: o.AccountID, AccountName: o.AccountName,
				}
				firmAccum[o.AccountID] = ent
			}
			ent.TradeCount += o.TradeCount
		case o.AssetClass == nil || o.SizeClass == nil:
			symAccum[o.SymbolRoot] += o.TradeCount
		default:
			key := *o.AssetClass + "|" + *o.AccountFirm + "|" + *o.SizeClass
			ent, ok := rateAccum[key]
			if !ok {
				ent = &orphanReason{
					Kind: "rate_missing",
					Firm: *o.AccountFirm, AssetClass: *o.AssetClass, SizeClass: *o.SizeClass,
				}
				rateAccum[key] = ent
			}
			ent.TradeCount += o.TradeCount
		}
	}
	var reasons []orphanReason
	for sym, n := range symAccum {
		reasons = append(reasons, orphanReason{Kind: "symbol_uncategorized", SymbolRoot: sym, TradeCount: n})
	}
	for _, r := range firmAccum {
		reasons = append(reasons, *r)
	}
	for _, r := range rateAccum {
		reasons = append(reasons, *r)
	}
	sortOrphansByCount(reasons)

	tz := h.localTZ()
	h.renderPage(w, r, "commissions.html", map[string]any{
		"NavActive":    "commissions",
		"BackHref":     validBackHref(r),
		"AssetClasses": assetClasses,
		"SizeClasses":  sizeClasses,
		"Cells":        buildCommissionCells(rates, time.Now()),
		"NowLocal":     time.Now().In(tz).Format("2006-01-02T15:04:05"),
		"Instruments":  instruments,
		"Firms":        firmsForCommissions,
		"Orphans":      reasons,
		"CanEdit":      auth.MustUser(ctx).IsAdmin,
		"Flash":        r.URL.Query().Get("flash"),
		"FlashErr":     r.URL.Query().Get("err"),
	})
}

// sortOrphansByCount sorts orphan reasons descending by trade_count so the
// most impactful problem appears first in the banner.
func sortOrphansByCount(rs []orphanReason) {
	sort.Slice(rs, func(i, j int) bool { return rs[i].TradeCount > rs[j].TradeCount })
}

// CommissionsClassCreate handles POST /commissions/classes/new. Supports both
// asset_classes and size_classes via a `kind` form field. Codes must be
// unique (DB enforces PK); labels are free text. After insertion the page
// reloads so the new option appears in every select.
func (h *Handlers) CommissionsClassCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	kind := strings.TrimSpace(r.FormValue("kind"))
	code := strings.TrimSpace(r.FormValue("code"))
	label := strings.TrimSpace(r.FormValue("label"))
	sortStr := strings.TrimSpace(r.FormValue("sort_order"))
	if code == "" || label == "" || (kind != "asset" && kind != "size") {
		http.Redirect(w, r, "/commissions?flash=createfail&err=missing+fields", http.StatusSeeOther)
		return
	}
	sortOrder := int64(100)
	if v, err := strconv.ParseInt(sortStr, 10, 64); err == nil {
		sortOrder = v
	}
	var err error
	if kind == "asset" {
		err = h.Queries.InsertAssetClass(ctx, models.InsertAssetClassParams{
			Code: code, Label: label, SortOrder: sortOrder,
		})
	} else {
		err = h.Queries.InsertSizeClass(ctx, models.InsertSizeClassParams{
			Code: code, Label: label, SortOrder: sortOrder,
		})
	}
	if err != nil {
		http.Redirect(w, r, "/commissions?flash=createfail&err="+errToQuery(err), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/commissions?flash=saved#classes", http.StatusSeeOther)
}

// CommissionsBulkSave handles POST /commissions/save. It owns only the
// instrument categorization table (commission rates are edited through each
// cell's version UI - see CommissionsRateCreate/Delete). The handler:
//  1. Parses form fields keyed "instrument.<symbol_root>.<field>".
//  2. Diffs each against the current DB value.
//  3. For changed instrument categorizations, upserts and queues a per-symbol
//     rebuild (deduped: a symbol edited twice rebuilds once).
func (h *Handlers) CommissionsBulkSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	// Rates are edited through the per-cell version UI (add/delete a version),
	// not this bulk save - it owns only the instrument categorization table.
	currentInstruments, err := h.Queries.ListInstruments(ctx)
	if err != nil {
		h.serverError(w, fmt.Errorf("list instruments: %w", err))
		return
	}
	instBefore := make(map[string]models.Instrument, len(currentInstruments))
	for _, inst := range currentInstruments {
		instBefore[inst.SymbolRoot] = inst
	}

	// Bucket form values.
	type instInput struct {
		AssetClass string
		SizeClass  string
		TickSize   string
		TickValue  string
		Multiplier string
		seen       map[string]bool
	}
	instInputs := map[string]*instInput{} // symbol_root -> fields
	getInst := func(sym string) *instInput {
		if v, ok := instInputs[sym]; ok {
			return v
		}
		v := &instInput{seen: map[string]bool{}}
		instInputs[sym] = v
		return v
	}

	for key, values := range r.PostForm {
		if len(values) == 0 {
			continue
		}
		switch {
		case strings.HasPrefix(key, "instrument."):
			rest := strings.TrimPrefix(key, "instrument.")
			dot := strings.IndexByte(rest, '.')
			if dot < 0 {
				continue
			}
			sym, field := rest[:dot], rest[dot+1:]
			ii := getInst(sym)
			ii.seen[field] = true
			v := strings.TrimSpace(values[0])
			switch field {
			case "asset_class":
				ii.AssetClass = v
			case "size_class":
				ii.SizeClass = v
			case "tick_size":
				ii.TickSize = v
			case "tick_value":
				ii.TickValue = v
			case "multiplier":
				ii.Multiplier = v
			}
		}
	}

	// Apply instrument edits. Each instrument's needs_setup is recomputed from
	// the (post-update) tick_size/tick_value/multiplier/asset_class/size_class.
	rebuildSymbols := map[string]bool{}
	for sym, in := range instInputs {
		before, ok := instBefore[sym]
		if !ok {
			continue
		}
		ts := parseFloatPtr(in.TickSize, before.TickSize, in.seen["tick_size"])
		tv := parseFloatPtr(in.TickValue, before.TickValue, in.seen["tick_value"])
		mu := parseFloatPtr(in.Multiplier, before.Multiplier, in.seen["multiplier"])
		ac := stringPtrOrNil(pickField(in.AssetClass, before.AssetClass, in.seen["asset_class"]))
		sc := stringPtrOrNil(pickField(in.SizeClass, before.SizeClass, in.seen["size_class"]))

		// needs_setup is "missing any of the five required fields".
		needsSetup := int64(0)
		if ts == nil || tv == nil || mu == nil || ac == nil || sc == nil {
			needsSetup = 1
		}

		changed := !floatPtrEq(ts, before.TickSize) ||
			!floatPtrEq(tv, before.TickValue) ||
			!floatPtrEq(mu, before.Multiplier) ||
			!stringPtrEq(ac, before.AssetClass) ||
			!stringPtrEq(sc, before.SizeClass) ||
			needsSetup != before.NeedsSetup
		if !changed {
			continue
		}

		if err := h.Queries.UpdateInstrumentSetup(ctx, models.UpdateInstrumentSetupParams{
			TickSize:   ts,
			TickValue:  tv,
			Multiplier: mu,
			AssetClass: ac,
			SizeClass:  sc,
			NeedsSetup: needsSetup,
			SymbolRoot: sym,
		}); err != nil {
			h.serverError(w, fmt.Errorf("update instrument %s: %w", sym, err))
			return
		}
		rebuildSymbols[sym] = true
	}

	// Run rebuilds. Errors here are logged but don't fail the save — the data
	// is correct, just the cached trades may need a manual rebuild.
	for sym := range rebuildSymbols {
		if err := h.ImportSvc.RebuildTradesForSymbol(ctx, sym); err != nil {
			h.logger().Warn("rebuild after instrument change", "symbol", sym, "err", err)
		}
	}
	// Commission/instrument specs are global reference data - every user's
	// derived trades may have changed.
	if len(rebuildSymbols) > 0 {
		h.LiveData.BumpAll()
	}

	http.Redirect(w, r, "/commissions?flash=saved", http.StatusSeeOther)
}

// CommissionsRateCreate handles POST /commissions/rates/new — add (or overwrite)
// one effective-dated rate version for a cell. The `effective_from` field is a
// datetime-local in the trading tz; blank means "baseline" (covers all history,
// the same 1970 sentinel the migration backfills). Re-posting an existing
// (cell, effective_from) updates that version's rate. Redirects back to the page
// so the regrouped, re-sorted history renders; the #rate-<key> anchor re-opens
// the affected cell.
func (h *Handlers) CommissionsRateCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	ac := strings.TrimSpace(r.FormValue("asset_class"))
	firm := strings.TrimSpace(r.FormValue("firm"))
	sc := strings.TrimSpace(r.FormValue("size_class"))
	rateStr := strings.TrimSpace(r.FormValue("per_contract_per_side"))
	if ac == "" || firm == "" || sc == "" || rateStr == "" {
		h.redirectCommissions(w, r, "", fmt.Errorf("asset class, firm, size and rate are required"))
		return
	}
	rate, err := strconv.ParseFloat(rateStr, 64)
	if err != nil || rate < 0 {
		h.redirectCommissions(w, r, "", fmt.Errorf("rate must be a non-negative number"))
		return
	}
	effective := commissionBaseline
	if raw := strings.TrimSpace(r.FormValue("effective_from")); raw != "" {
		effective, err = parseExecTime(raw, h.localTZ())
		if err != nil {
			h.redirectCommissions(w, r, "", fmt.Errorf("effective from: %w", err))
			return
		}
	}
	if err := h.Queries.UpsertCommissionRate(ctx, models.UpsertCommissionRateParams{
		AssetClass: ac, Firm: firm, SizeClass: sc, EffectiveFrom: effective, PerContractPerSide: rate,
	}); err != nil {
		h.serverError(w, fmt.Errorf("upsert rate: %w", err))
		return
	}
	if err := h.ImportSvc.RebuildTradesForCommissionCell(ctx, firm, ac, sc); err != nil {
		h.logger().Warn("rebuild after rate create", "err", err)
	}
	h.LiveData.BumpAll()
	h.redirectCommissions(w, r, rateKey(ac, firm, sc), nil)
}

// CommissionsRateDelete handles POST /commissions/rates/delete. It deletes one
// version by its stable id (a DATETIME text match on the 1970 baseline is
// fragile), then recomputes the affected cell's trades. firm/asset/size come
// along only to target that recompute.
func (h *Handlers) CommissionsRateDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("id")), 10, 64)
	if err != nil {
		h.redirectCommissions(w, r, "", fmt.Errorf("bad version id"))
		return
	}
	ac := strings.TrimSpace(r.FormValue("asset_class"))
	firm := strings.TrimSpace(r.FormValue("firm"))
	sc := strings.TrimSpace(r.FormValue("size_class"))
	if err := h.Queries.DeleteCommissionRate(ctx, id); err != nil {
		h.serverError(w, fmt.Errorf("delete rate: %w", err))
		return
	}
	if ac != "" && firm != "" && sc != "" {
		if err := h.ImportSvc.RebuildTradesForCommissionCell(ctx, firm, ac, sc); err != nil {
			h.logger().Warn("rebuild after rate delete", "err", err)
		}
		h.LiveData.BumpAll()
	}
	h.redirectCommissions(w, r, rateKey(ac, firm, sc), nil)
}

// redirectCommissions sends the user back to /commissions after a version CRUD.
// A non-empty cellKey adds a #rate-<key> anchor so the browser scrolls to (and
// a small script re-opens) the cell that was just edited.
func (h *Handlers) redirectCommissions(w http.ResponseWriter, r *http.Request, cellKey string, e error) {
	dest := "/commissions"
	if e != nil {
		dest += "?err=" + errToQuery(e)
	} else {
		dest += "?flash=saved"
	}
	if cellKey != "" {
		dest += "#rate-" + cellKey
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// rateKey joins (asset_class, firm, size_class) with '|' for use as a map
// key. The pipe is a safe separator because none of the codes contain it.
func rateKey(ac, firm, sc string) string { return ac + "|" + firm + "|" + sc }

// parseFloatPtr parses a form-text value into *float64. If the field wasn't
// submitted, it returns the existing value. Empty string parses to nil.
func parseFloatPtr(raw string, prev *float64, submitted bool) *float64 {
	if !submitted {
		return prev
	}
	if raw == "" {
		return nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return prev
	}
	return &v
}

// pickField returns submitted value when submitted, else the prev string.
// Empty string after submission means "clear to NULL".
func pickField(raw string, prev *string, submitted bool) string {
	if !submitted {
		if prev == nil {
			return ""
		}
		return *prev
	}
	return raw
}

func stringPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func floatPtrEq(a, b *float64) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

func stringPtrEq(a, b *string) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}
