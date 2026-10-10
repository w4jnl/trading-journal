package handlers

import (
	"fmt"
	"regexp"
	"strings"
)

// importNote is the import log's Message column: Summary is the one-line
// reading shown in the table, Raw the importer's exact text (tooltip). Both
// empty when the log row carries no message.
type importNote struct {
	Summary string
	Raw     string
}

var (
	reImportRow  = regexp.MustCompile(`^row (\d+): (.*)$`)
	reMissingCol = regexp.MustCompile(`missing column "([^"]+)"`)
)

// humanizeImportMessage turns the importer's error chain ("parse: row 12:
// parse price: strconv.ParseFloat: ...") into a sentence a trader can act
// on. Messages it does not recognise pass through unchanged, so a new
// importer error is never hidden, only unpolished.
func humanizeImportMessage(msg *string) importNote {
	if msg == nil {
		return importNote{}
	}
	raw := strings.TrimSpace(*msg)
	if raw == "" {
		return importNote{}
	}
	n := importNote{Summary: raw, Raw: raw}
	body, phase := raw, ""
	for _, p := range []string{"parse: ", "ingest: "} {
		if strings.HasPrefix(body, p) {
			phase, body = strings.TrimSuffix(p, ": "), strings.TrimPrefix(body, p)
			break
		}
	}
	switch {
	case strings.Contains(body, "unrecognized CSV header"):
		n.Summary = "Not a MotiveWave or TopstepX export: the header row was not recognised."
	case reMissingCol.MatchString(body):
		col := reMissingCol.FindStringSubmatch(body)[1]
		n.Summary = fmt.Sprintf("The export is missing the %q column. Re-export with the default columns.", col)
	case strings.HasPrefix(body, "read header"):
		n.Summary = "The file is empty, or cut off before the header row."
	case reImportRow.MatchString(body):
		m := reImportRow.FindStringSubmatch(body)
		n.Summary = fmt.Sprintf("Row %s could not be read: %s. Nothing from this file was imported.", m[1], importRowProblem(m[2]))
	case strings.Contains(body, "already orders-imported"), strings.Contains(body, "already trades-imported"):
		n.Summary = body // written for people already
	case phase == "ingest":
		n.Summary = "The file parsed, but saving its fills failed: " + body + "."
	}
	return n
}

// importRowProblem names the field a per-row parser error is about.
func importRowProblem(detail string) string {
	d := strings.ToLower(detail)
	switch {
	case strings.Contains(d, "must be positive"):
		return "the quantity is zero or negative"
	case strings.HasPrefix(d, "parse quantity"), strings.HasPrefix(d, "parse size"):
		return "the quantity is not a number"
	case strings.HasPrefix(d, "parse price"), strings.HasPrefix(d, "parse execute price"):
		return "the price is not a number"
	case strings.HasPrefix(d, "parse time"), strings.HasPrefix(d, "parse filled_at"):
		return "the timestamp is not in the expected format"
	case strings.HasPrefix(d, "unknown action"), strings.HasPrefix(d, "unknown side"):
		return "the side is neither buy nor sell (" + detail + ")"
	case strings.Contains(d, "wrong number of fields"):
		return "it has a different number of columns than the header"
	}
	if i := strings.Index(detail, ":"); i > 0 {
		return detail[:i]
	}
	return detail
}
