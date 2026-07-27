package mwmail

import (
	"context"
	"encoding/json"

	"github.com/w4jnl/trading-journal/internal/importer"
)

// ImporterIngest adapts importer.ImportService.IngestEmailFill to the
// IngestFunc hook. userID owns any account auto-created from an email (the
// same single-owner convention as flat watch-folder drops).
//
// The broker_exec_id is minted as "MWEMAIL-<order id>": it can never collide
// with a CSV ticket, and the real join key for supersede/dedup is the
// order_id column anyway. The Broker label ("Rithmic WS", CQG variants)
// doubles as the Source hint for firm guessing and EP->ES aliasing, exactly
// like the CSV Source column.
func ImporterIngest(svc *importer.ImportService, userID int64) IngestFunc {
	return func(ctx context.Context, res Result) (string, error) {
		root := importer.CanonicalizeSymbolRoot(importer.DeriveSymbolRoot(res.Symbol), res.Broker)
		raw, _ := json.Marshal(map[string]string{
			"Account ID":     res.Account,
			"Order ID":       res.OrderID,
			"Symbol":         res.Symbol,
			"Broker":         res.Broker,
			"Execution Time": res.FillTimeRaw,
			"Date":           res.Date,
			"Subject":        res.Subject,
		})
		return svc.IngestEmailFill(ctx, userID, importer.Fill{
			AccountRef:   res.Account,
			BrokerExecID: "MWEMAIL-" + res.OrderID,
			OrderID:      res.OrderID,
			Symbol:       res.Symbol,
			SymbolRoot:   root,
			Source:       res.Broker,
			Side:         res.Side,
			Qty:          res.Qty,
			Price:        res.Price,
			ExecTime:     res.ExecTime,
			RawData:      string(raw),
		})
	}
}
