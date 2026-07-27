package mwmail

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/w4jnl/trading-journal/internal/importer"
)

// replayArchive runs archived messages through Parse + Ingest, oldest first,
// skipping everything the replay journal already records as terminally
// processed (by a previous replay or the live path) - each capture is
// attempted once, ever. Recovery counts (inserted/updated) are what the live
// path missed; read and ingest errors are NOT journaled, so they retry on
// the next boot.
func (s *Server) replayArchive(ctx context.Context) {
	paths, err := s.archive.list()
	if err != nil {
		s.logger().Error("mwmail: archive replay listing failed", "err", err)
		return
	}
	counts := map[string]int{}
	mark := func(hash, action string) {
		if hash == "" { // unrecognizable filename - nothing stable to key on
			return
		}
		if err := s.archive.markDone(hash, action); err != nil {
			s.logger().Warn("mwmail: replay journal write failed", "hash", hash, "err", err)
		}
	}
	for _, p := range paths {
		if ctx.Err() != nil {
			return
		}
		hash := hashFromName(filepath.Base(p))
		if hash != "" && s.archive.isDone(hash) {
			counts["already_done"]++
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			s.logger().Warn("mwmail: archive replay read failed", "file", p, "err", err)
			counts["read_error"]++
			continue
		}
		res := Parse(raw)
		if res.IsTest || !res.IngestReady() {
			counts["not_ingestable"]++
			mark(hash, "not_ingestable")
			continue
		}
		action, err := s.Ingest(ctx, res)
		if err != nil {
			s.logger().Warn("mwmail: archive replay ingest failed",
				"file", p, "order_id", res.OrderID, "err", err)
			counts["error"]++
			continue
		}
		counts[action]++
		mark(hash, action)
		s.logger().Debug("mwmail: archive replayed",
			"file", p, "order_id", res.OrderID, "action", action)
	}
	attrs := []any{"files", len(paths)}
	for k, v := range counts {
		attrs = append(attrs, k, v)
	}
	s.logger().Info("mwmail: archive replay done", attrs...)
}

// ImporterIngest adapts importer.ImportService.IngestEmailFill to the
// IngestFunc hook. User attribution happens inside IngestEmailFill: the fill
// is booked under whichever user already owns the account ref; unknown refs
// are never auto-created from email (no owner signal) and stay archive-only
// until their first CSV import.
//
// The broker_exec_id is minted as "MWEMAIL-<order id>": it can never collide
// with a CSV ticket, and the real join key for supersede/dedup is the
// order_id column anyway. The Broker label ("Rithmic WS", CQG variants)
// doubles as the Source hint for EP->ES aliasing, exactly like the CSV
// Source column.
func ImporterIngest(svc *importer.ImportService) IngestFunc {
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
		return svc.IngestEmailFill(ctx, importer.Fill{
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
