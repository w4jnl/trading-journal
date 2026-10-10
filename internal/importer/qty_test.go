package importer

import (
	"strings"
	"testing"
)

// A zero quantity is a broken row, not a fill: the parsers refuse it rather
// than feeding the FIFO builder a divide-by-zero.
func TestParsersRejectNonPositiveQty(t *testing.T) {
	mw := "Account,Ticket,Underlying,Symbol,Exchange,Time,Action,Quantity,Price,Source\n" +
		"APEX-1,T1,ES,ESM6,CME,05/10/2026 09:00:00.000-0500,BOT,0,7250.00,Rithmic\n"
	if _, err := ParseMotiveWave(strings.NewReader(mw)); err == nil || !strings.Contains(err.Error(), "must be positive") {
		t.Errorf("MotiveWave zero quantity: err = %v, want a positive-quantity error", err)
	}
	ts := "Id,AccountName,ContractName,Status,Size,Side,FilledAt,ExecutePrice,PositionDisposition\n" +
		"1,EXPRESS-1,ESM6,Filled,0,Bid,2026-05-10T14:00:00Z,7250.00,Opening\n"
	if _, err := ParseTopstepX(strings.NewReader(ts)); err == nil || !strings.Contains(err.Error(), "must be positive") {
		t.Errorf("TopstepX zero size: err = %v, want a positive-size error", err)
	}
}
