package handlers

import (
	"strings"
	"testing"
)

func TestUserWhereStartsWithOwner(t *testing.T) {
	w := whereUser(ownsTradesSQL, 7).and("t.status = ?", "closed").andIn("t.account_id", []int64{3, 4})
	if got := w.sql(); !strings.HasPrefix(got, ownsTradesSQL+" AND ") || !strings.HasSuffix(got, "t.account_id IN (?,?)") {
		t.Fatalf("sql = %q", got)
	}
	if len(w.args) != 4 || w.args[0] != int64(7) || w.args[1] != "closed" || w.args[2] != int64(3) {
		t.Fatalf("args = %v", w.args)
	}
	empty := whereUser(ownsJournalSQL, 1).andIn("je.id", nil)
	if got := empty.sql(); got != ownsJournalSQL+" AND 1 = 0" {
		t.Fatalf("empty IN = %q", got)
	}
}
