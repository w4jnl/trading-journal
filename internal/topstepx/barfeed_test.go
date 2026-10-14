package topstepx

import (
	"context"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/marketdata"
	"github.com/w4jnl/trading-journal/internal/models"
)

// barsAPI is the sync test fake plus a bar history: it records the request
// and answers newest-first bars at the asked size, like the API.
type barsAPI struct {
	*fakeAPI
	contract string
	unit     AggregateBarUnit
	n        int
	calls    int
}

func (b *barsAPI) RetrieveBars(_ context.Context, token, contractID string, unit AggregateBarUnit, unitNumber int, start, end time.Time, limit int, includePartial, live bool) ([]AggregateBar, error) {
	b.calls++
	b.contract, b.unit, b.n = contractID, unit, unitNumber
	step := time.Duration(unitNumber) * time.Second
	if unit == UnitMinute {
		step = time.Duration(unitNumber) * time.Minute
	}
	var out []AggregateBar
	for t := end.Add(-step); !t.Before(start); t = t.Add(-step) {
		out = append(out, AggregateBar{Time: t, Open: 7000, High: 7001, Low: 6999, Close: 7000.5, Volume: 4})
	}
	return out, nil
}

// The feed maps the vendor symbol to the contract last traded on one of
// its roots, borrows a session of a user with credentials, asks the size
// requested, and returns bars inside the window in order; a root nobody
// traded is unsupported.
func TestBarFeed(t *testing.T) {
	svc, api, q, uid := setupSync(t)
	ctx := context.Background()
	bapi := &barsAPI{fakeAPI: api}
	svc.Client = bapi
	firm := "topstep"
	acct, err := q.CreateAccount(ctx, models.CreateAccountParams{UserID: uid, Name: "tsx", Firm: &firm, ImportSource: "topstepx_api", AccountRef: "TSX-1", Currency: "USD", CommissionSource: "inline"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertExecution(ctx, models.InsertExecutionParams{AccountID: acct.ID, BrokerExecID: "X1", ImportMethod: "topstepx_api",
		Symbol: "MESZ6", SymbolRoot: "MES", Side: "buy", Qty: 1, Price: 7000, ExecTime: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	feed := &BarFeed{Sync: svc, DB: svc.Importer.DB}
	from := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Minute)
	to := from.Add(5 * time.Minute)
	bars, err := feed.FetchExchangeBars(ctx, "ES=F", 60, from, to)
	if err != nil {
		t.Fatalf("minute bars: %v", err)
	}
	if bapi.unit != UnitMinute || bapi.n != 1 || bapi.contract != "CON.F.US.MES.Z26" {
		t.Errorf("request = %v x%d on %s", bapi.unit, bapi.n, bapi.contract)
	}
	if len(bars) != 5 || bars[0].Time != from.Unix() || bars[4].Time >= to.Unix() || bars[0].Volume != 4 {
		t.Errorf("bars = %d, first %v", len(bars), bars[0])
	}
	if _, err := feed.FetchExchangeBars(ctx, "ES=F", 1, from, from.Add(30*time.Second)); err != nil || bapi.unit != UnitSecond || bapi.n != 1 {
		t.Errorf("second bars: %v (unit %v x%d)", err, bapi.unit, bapi.n)
	}
	if _, err := feed.FetchExchangeBars(ctx, "CL=F", 60, from, to); err != marketdata.ErrUnsupported {
		t.Errorf("untraded root = %v, want ErrUnsupported", err)
	}
	if _, err := (&BarFeed{}).FetchExchangeBars(ctx, "ES=F", 60, from, to); err != marketdata.ErrUnsupported {
		t.Errorf("no sync = %v", err)
	}
}
