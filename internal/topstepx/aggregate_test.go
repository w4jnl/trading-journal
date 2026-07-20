package topstepx

import (
	"testing"
	"time"
)

func ht(id, orderID int64, side int, size int32, price, fees float64, comm *float64, at time.Time) HalfTrade {
	return HalfTrade{
		ID: id, OrderID: orderID, Side: side, Size: size, Price: price,
		Fees: fees, Commissions: comm, CreationTimestamp: at,
		ContractID: "CON.F.US.MES.M26",
	}
}

func TestAggregateOrders_MultiFillWeightedAverage(t *testing.T) {
	t0 := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	c := 0.35
	fills := AggregateOrders([]HalfTrade{
		// One order (id 100) filled in two parts at different prices; the
		// second part reported first-in-slice to prove ordering independence.
		ht(2, 100, 0, 3, 7302, 1.11, &c, t0.Add(2*time.Second)),
		ht(1, 100, 0, 2, 7300, 0.74, &c, t0),
	}, nil)
	if len(fills) != 1 {
		t.Fatalf("len = %d, want 1", len(fills))
	}
	f := fills[0]
	if f.BrokerExecID != "100" || f.Side != "buy" {
		t.Errorf("id/side = %q/%q, want 100/buy", f.BrokerExecID, f.Side)
	}
	if f.Qty != 5 {
		t.Errorf("qty = %v, want 5", f.Qty)
	}
	wantPx := (7302*3 + 7300*2) / 5.0
	if f.Price != wantPx {
		t.Errorf("price = %v, want %v (size-weighted)", f.Price, wantPx)
	}
	wantComm := 1.11 + 0.35 + 0.74 + 0.35
	if diff := f.Commission - wantComm; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("commission = %v, want %v", f.Commission, wantComm)
	}
	if !f.ExecTime.Equal(t0) {
		t.Errorf("time = %v, want earliest %v", f.ExecTime, t0)
	}
}

func TestAggregateOrders_VoidedExcluded(t *testing.T) {
	t0 := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	voided := ht(1, 100, 1, 2, 7300, 1.0, nil, t0)
	voided.Voided = true
	fills := AggregateOrders([]HalfTrade{
		voided,
		ht(2, 100, 1, 3, 7301, 1.5, nil, t0.Add(time.Second)),
	}, nil)
	if len(fills) != 1 {
		t.Fatalf("len = %d, want 1", len(fills))
	}
	if fills[0].Qty != 3 || fills[0].Commission != 1.5 {
		t.Errorf("qty/comm = %v/%v, want 3/1.5 (voided half dropped)", fills[0].Qty, fills[0].Commission)
	}
}

func TestAggregateOrders_ZeroOrderIDFallback(t *testing.T) {
	t0 := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	fills := AggregateOrders([]HalfTrade{
		ht(41, 0, 0, 1, 7300, 1.0, nil, t0),
		ht(42, 0, 0, 1, 7301, 1.0, nil, t0.Add(time.Second)),
	}, nil)
	// Two zero-orderId halves must NOT merge; each keys off its own trade id.
	if len(fills) != 2 {
		t.Fatalf("len = %d, want 2 (no merging under orderId 0)", len(fills))
	}
	if fills[0].BrokerExecID != "T41" || fills[1].BrokerExecID != "T42" {
		t.Errorf("ids = %q, %q, want T41, T42", fills[0].BrokerExecID, fills[1].BrokerExecID)
	}
}

func TestAggregateOrders_NilCommissionsFeesOnly(t *testing.T) {
	t0 := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	fills := AggregateOrders([]HalfTrade{ht(1, 100, 1, 2, 7300, 1.48, nil, t0)}, nil)
	if len(fills) != 1 || fills[0].Commission != 1.48 {
		t.Fatalf("commission = %v, want 1.48 (fees only, commissions null)", fills[0].Commission)
	}
	if fills[0].Side != "sell" {
		t.Errorf("side = %q, want sell (Ask=1)", fills[0].Side)
	}
}

func TestAggregateOrders_SortedByTime(t *testing.T) {
	t0 := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	fills := AggregateOrders([]HalfTrade{
		ht(2, 200, 1, 1, 7305, 1.0, nil, t0.Add(time.Minute)),
		ht(1, 100, 0, 1, 7300, 1.0, nil, t0),
	}, nil)
	if len(fills) != 2 || fills[0].BrokerExecID != "100" || fills[1].BrokerExecID != "200" {
		t.Fatalf("fills out of time order: %+v", fills)
	}
}
