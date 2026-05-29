package handlers

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
)

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
	Kind         string // "symbol_uncategorized" | "account_firm_missing" | "rate_missing"
	SymbolRoot   string // populated for symbol_uncategorized and rate_missing
	AccountID    int64  // populated for account_firm_missing
	AccountName  string // populated for account_firm_missing
	Firm         string // populated for rate_missing
	AssetClass   string // populated for rate_missing
	SizeClass    string // populated for rate_missing
	TradeCount   int64
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
	symAccum := map[string]int64{}              // symbol_root -> count (uncategorized symbol)
	firmAccum := map[int64]*orphanReason{}      // account_id -> entry (missing firm)
	rateAccum := map[string]*orphanReason{}     // "asset_class|firm|size_class" -> entry
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

	h.renderPage(w, r, "commissions.html", map[string]any{
		"NavActive":    "commissions",
		"BackHref":     validBackHref(r),
		"AssetClasses": assetClasses,
		"SizeClasses":  sizeClasses,
		"Rates":        rates,
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

// CommissionsBulkSave handles POST /commissions/save. One submission carries
// edits to both rates and instrument categorization. The handler:
//   1. Parses form fields keyed "rate.<asset_class>|<firm>|<size_class>.per_contract_per_side"
//      (legacy "per_contract_rt" still accepted for in-flight tabs) and
//      "instrument.<symbol_root>.<field>".
//   2. Diffs each against the current DB value.
//   3. For changed rates, upserts and queues a per-cell rebuild.
//   4. For changed instrument categorizations, upserts and queues a per-symbol
//      rebuild.
//   5. Dedups rebuild work (a single (account_id, symbol) may be hit by both
//      types of change).
func (h *Handlers) CommissionsBulkSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	currentRates, err := h.Queries.ListCommissionRates(ctx)
	if err != nil {
		h.serverError(w, fmt.Errorf("list rates: %w", err))
		return
	}
	rateBefore := make(map[string]float64, len(currentRates))
	for _, rt := range currentRates {
		rateBefore[rateKey(rt.AssetClass, rt.Firm, rt.SizeClass)] = rt.PerContractPerSide
	}

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
	rateInputs := map[string]string{}         // key -> rate string
	instInputs := map[string]*instInput{}     // symbol_root -> fields
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
		case strings.HasPrefix(key, "rate."):
			rest := strings.TrimPrefix(key, "rate.")
			dot := strings.LastIndexByte(rest, '.')
			if dot < 0 {
				continue
			}
			cell, field := rest[:dot], rest[dot+1:]
			// Accept the legacy "per_contract_rt" name too so a tab opened
			// before the rename still saves cleanly.
			if field != "per_contract_per_side" && field != "per_contract_rt" {
				continue
			}
			rateInputs[cell] = strings.TrimSpace(values[0])
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

	// Apply rate edits. Skip rows whose value is unchanged or unparseable.
	type cellKey struct{ AssetClass, Firm, SizeClass string }
	rebuildCells := map[cellKey]bool{}
	for cell, raw := range rateInputs {
		ac, firm, sc, ok := parseRateKey(cell)
		if !ok {
			continue
		}
		newVal, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			// blank rate row: leave as-is (don't insert 0.00 by accident)
			continue
		}
		prev, existed := rateBefore[cell]
		if existed && prev == newVal {
			continue
		}
		if err := h.Queries.UpsertCommissionRate(ctx, models.UpsertCommissionRateParams{
			AssetClass:         ac,
			Firm:               firm,
			SizeClass:          sc,
			PerContractPerSide: newVal,
		}); err != nil {
			h.serverError(w, fmt.Errorf("upsert rate %s: %w", cell, err))
			return
		}
		rebuildCells[cellKey{ac, firm, sc}] = true
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
	for cell := range rebuildCells {
		if err := h.ImportSvc.RebuildTradesForCommissionCell(ctx, cell.Firm, cell.AssetClass, cell.SizeClass); err != nil {
			h.logger().Warn("rebuild after rate change",
				"cell", fmt.Sprintf("%s|%s|%s", cell.AssetClass, cell.Firm, cell.SizeClass), "err", err)
		}
	}
	for sym := range rebuildSymbols {
		if err := h.ImportSvc.RebuildTradesForSymbol(ctx, sym); err != nil {
			h.logger().Warn("rebuild after instrument change", "symbol", sym, "err", err)
		}
	}

	http.Redirect(w, r, "/commissions?flash=saved", http.StatusSeeOther)
}

// CommissionsRateCreate handles POST /commissions/rates/new — insert a new
// (asset_class, firm, size_class) rate row and HTMX-swap the rendered row
// into the rates table. Upsert semantics: if the user picks a cell that
// already has a rate, the existing rate is updated.
func (h *Handlers) CommissionsRateCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	ac := strings.TrimSpace(r.FormValue("asset_class"))
	firm := strings.TrimSpace(r.FormValue("firm"))
	sc := strings.TrimSpace(r.FormValue("size_class"))
	// Accept both names so a tab opened before the rename still posts cleanly.
	rateStr := strings.TrimSpace(r.FormValue("per_contract_per_side"))
	if rateStr == "" {
		rateStr = strings.TrimSpace(r.FormValue("per_contract_rt"))
	}
	if ac == "" || firm == "" || sc == "" || rateStr == "" {
		http.Error(w, "asset_class, firm, size_class, per_contract_per_side are required", http.StatusBadRequest)
		return
	}
	rate, err := strconv.ParseFloat(rateStr, 64)
	if err != nil {
		http.Error(w, "per_contract_per_side must be a number", http.StatusBadRequest)
		return
	}
	if err := h.Queries.UpsertCommissionRate(ctx, models.UpsertCommissionRateParams{
		AssetClass: ac, Firm: firm, SizeClass: sc, PerContractPerSide: rate,
	}); err != nil {
		h.serverError(w, fmt.Errorf("upsert rate: %w", err))
		return
	}
	if err := h.ImportSvc.RebuildTradesForCommissionCell(ctx, firm, ac, sc); err != nil {
		h.logger().Warn("rebuild after rate create", "err", err)
	}
	h.renderPartial(w, "_commission_rate_row.html", "commission_rate_row", map[string]any{
		"Rate":    models.CommissionRate{AssetClass: ac, Firm: firm, SizeClass: sc, PerContractPerSide: rate},
		"Firms":   firmsForCommissions,
		"CanEdit": true, // route is admin-gated
	})
}

// CommissionsRateDelete handles POST /commissions/rates/delete with composite
// key in form fields. Returns 200 with empty body so an HTMX hx-swap="delete"
// removes the row from the table.
func (h *Handlers) CommissionsRateDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	ac := strings.TrimSpace(r.FormValue("asset_class"))
	firm := strings.TrimSpace(r.FormValue("firm"))
	sc := strings.TrimSpace(r.FormValue("size_class"))
	if ac == "" || firm == "" || sc == "" {
		http.Error(w, "asset_class, firm, size_class are required", http.StatusBadRequest)
		return
	}
	if err := h.Queries.DeleteCommissionRate(ctx, models.DeleteCommissionRateParams{
		AssetClass: ac, Firm: firm, SizeClass: sc,
	}); err != nil {
		h.serverError(w, fmt.Errorf("delete rate: %w", err))
		return
	}
	if err := h.ImportSvc.RebuildTradesForCommissionCell(ctx, firm, ac, sc); err != nil {
		h.logger().Warn("rebuild after rate delete", "err", err)
	}
	w.WriteHeader(http.StatusOK)
}

// rateKey joins (asset_class, firm, size_class) with '|' for use as a map
// key. The pipe is a safe separator because none of the codes contain it.
func rateKey(ac, firm, sc string) string { return ac + "|" + firm + "|" + sc }

func parseRateKey(s string) (ac, firm, sc string, ok bool) {
	parts := strings.Split(s, "|")
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

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
