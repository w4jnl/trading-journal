package main

import (
	"fmt"
	"os"

	"github.com/w4jnl/trading-journal/internal/mwmail"
)

// runMWMailCommand implements `trading-journal mwmail <subcommand>` - dev
// tooling for the MotiveWave email capture module. `parse` is the parser
// tuning loop: run it on an archived .eml from mw_email.archive_dir, inspect
// the extraction, adjust the provisional patterns, repeat.
func runMWMailCommand(args []string) error {
	if len(args) < 1 || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(`Usage: trading-journal mwmail parse <file.eml>

Runs the provisional MotiveWave fill parser on a captured message and prints
the extracted fields. Captured messages live in mw_email.archive_dir
(default data/mw-raw).
`)
		return nil
	}
	switch args[0] {
	case "parse":
		if len(args) != 2 {
			return fmt.Errorf("usage: trading-journal mwmail parse <file.eml>")
		}
		raw, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		res := mwmail.Parse(raw)
		fmt.Printf("from:      %s\n", res.From)
		fmt.Printf("to:        %s\n", res.To)
		fmt.Printf("date:      %s\n", res.Date)
		fmt.Printf("subject:   %s\n", res.Subject)
		fmt.Printf("parsed:    %v\n", res.Parsed)
		fmt.Printf("side:      %s\n", res.Side)
		fmt.Printf("qty:       %g\n", res.Qty)
		fmt.Printf("symbol:    %s\n", res.Symbol)
		fmt.Printf("price:     %g\n", res.Price)
		fmt.Printf("account:   %s\n", res.Account)
		fmt.Printf("order_id:  %s\n", res.OrderID)
		fmt.Printf("fill_time: %s\n", res.FillTimeRaw)
		return nil
	default:
		return fmt.Errorf("unknown mwmail subcommand %q (try: parse)", args[0])
	}
}
