package handlers

import "strings"

// userWhere accumulates a dynamic WHERE clause whose FIRST predicate is the
// current user's ownership test. Every hand-built trade, execution and
// journal list in this package starts here, so a query cannot forget the
// predicate or append it after an optional clause (the review found both
// shapes across trades.go and analytics.go). sqlc cannot express optional
// filters, hence the hand-built SQL at all.
type userWhere struct {
	clauses []string
	args    []any
}

// Ownership predicates for the table aliases in play.
const (
	ownsTradesSQL   = "t.account_id IN (SELECT id FROM accounts WHERE user_id = ?)" // trades t, no accounts join
	ownsAccountsSQL = "a.user_id = ?"                                               // joined accounts a
	ownsJournalSQL  = "je.user_id = ?"                                              // journal_entries je
)

// whereUser starts the clause list with pred bound to uid.
func whereUser(pred string, uid int64) *userWhere {
	return &userWhere{clauses: []string{pred}, args: []any{uid}}
}

// and appends one clause with its arguments.
func (w *userWhere) and(clause string, args ...any) *userWhere {
	w.clauses = append(w.clauses, clause)
	w.args = append(w.args, args...)
	return w
}

// andIn appends `column IN (?, ...)`, or a clause that matches nothing when
// ids is empty - an active filter with no matching accounts shows none.
func (w *userWhere) andIn(column string, ids []int64) *userWhere {
	if len(ids) == 0 {
		return w.and("1 = 0")
	}
	ph := strings.Repeat("?,", len(ids))
	w.clauses = append(w.clauses, column+" IN ("+ph[:len(ph)-1]+")")
	for _, id := range ids {
		w.args = append(w.args, id)
	}
	return w
}

// sql joins the clauses with AND (no leading WHERE).
func (w *userWhere) sql() string { return strings.Join(w.clauses, " AND ") }
