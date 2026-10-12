// Package dayplan stores the pre-market plan: one per user and trading
// day, written before the session (bias, levels, a cap on decisions, a
// stop time, a max loss per account, notes) and read against the day's
// trades afterwards by internal/coach.
package dayplan

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/journal"
	"github.com/w4jnl/trading-journal/internal/models"
)

// Plan is one day's plan.
type Plan struct {
	Day       string    `json:"day"`       // YYYY-MM-DD, trading timezone
	Bias      string    `json:"bias"`      // long | short | neutral | flat | "" (none)
	Levels    string    `json:"levels"`    // free text, one level per line
	MaxTrades *int64    `json:"maxTrades"` // decisions; a copy set counts once
	StopTime  string    `json:"stopTime"`  // HH:MM, trading timezone; "" none
	MaxLoss   *float64  `json:"maxLoss"`   // per account, positive money
	Notes     string    `json:"notes"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Bias is one bias choice.
type Bias struct {
	Key  string
	Name string
}

// Biases lists the choices in display order.
var Biases = []Bias{{"long", "Long"}, {"short", "Short"}, {"neutral", "Both ways"}, {"flat", "Stay out"}}

// BiasName is the display name of a bias key ("" for none or unknown).
func BiasName(key string) string {
	for _, b := range Biases {
		if b.Key == key {
			return b.Name
		}
	}
	return ""
}

const maxText = 4000

// IsEmpty reports a plan with nothing in it (saving one deletes the plan).
func (p Plan) IsEmpty() bool {
	return p.Bias == "" && strings.TrimSpace(p.Levels) == "" && p.MaxTrades == nil && p.StopTime == "" && p.MaxLoss == nil && strings.TrimSpace(p.Notes) == ""
}

// StopMinutes is the stop time as minutes after midnight; ok false without one.
func (p Plan) StopMinutes() (int, bool) {
	if p.StopTime == "" {
		return 0, false
	}
	t, err := time.Parse("15:04", p.StopTime)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

// Normalize validates the plan in place: bias from the list, the day and
// stop time well-formed (9:30 becomes 09:30), a positive cap and loss
// (zero clears them), text trimmed and bounded. Bad input is a
// journal.ValidationError so handlers answer 400.
func Normalize(p *Plan) error {
	if _, err := time.Parse("2006-01-02", p.Day); err != nil {
		return journal.BadInput("bad day; expected YYYY-MM-DD")
	}
	p.Bias = strings.ToLower(strings.TrimSpace(p.Bias))
	if p.Bias != "" && BiasName(p.Bias) == "" {
		return journal.BadInput("bias must be long, short, neutral or flat")
	}
	p.StopTime = strings.TrimSpace(p.StopTime)
	if p.StopTime != "" {
		t, err := time.Parse("15:04", p.StopTime)
		if err != nil {
			return journal.BadInput("stop time must be HH:MM")
		}
		p.StopTime = t.Format("15:04")
	}
	if p.MaxTrades != nil && *p.MaxTrades <= 0 {
		if *p.MaxTrades < 0 {
			return journal.BadInput("max trades must be positive")
		}
		p.MaxTrades = nil
	}
	if p.MaxLoss != nil && *p.MaxLoss <= 0 {
		if *p.MaxLoss < 0 {
			return journal.BadInput("max loss is a positive amount")
		}
		p.MaxLoss = nil
	}
	p.Levels = strings.TrimSpace(p.Levels)
	p.Notes = strings.TrimSpace(p.Notes)
	if len(p.Levels) > maxText || len(p.Notes) > maxText {
		return journal.BadInput("levels and notes are limited to %d characters", maxText)
	}
	return nil
}

// Get loads the user's plan for a day; nil without one.
func Get(ctx context.Context, q *models.Queries, userID int64, day string) (*Plan, error) {
	row, err := q.GetDayPlan(ctx, models.GetDayPlanParams{UserID: userID, Day: day})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load day plan: %w", err)
	}
	p := fromRow(row)
	return &p, nil
}

// List loads the user's plans in an inclusive day range.
func List(ctx context.Context, q *models.Queries, userID int64, from, to string) ([]Plan, error) {
	rows, err := q.ListDayPlansBetween(ctx, models.ListDayPlansBetweenParams{UserID: userID, Day: from, Day_2: to})
	if err != nil {
		return nil, fmt.Errorf("list day plans: %w", err)
	}
	out := make([]Plan, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromRow(r))
	}
	return out, nil
}

// Save validates and stores the plan; an empty plan deletes it. Returns
// the stored plan (nil after a delete).
func Save(ctx context.Context, q *models.Queries, userID int64, p Plan, now time.Time) (*Plan, error) {
	if err := Normalize(&p); err != nil {
		return nil, err
	}
	if p.IsEmpty() {
		if err := q.DeleteDayPlan(ctx, models.DeleteDayPlanParams{UserID: userID, Day: p.Day}); err != nil {
			return nil, fmt.Errorf("delete day plan: %w", err)
		}
		return nil, nil
	}
	now = now.UTC()
	if err := q.UpsertDayPlan(ctx, models.UpsertDayPlanParams{
		UserID: userID, Day: p.Day, Bias: p.Bias, Levels: p.Levels, MaxTrades: p.MaxTrades,
		StopTime: p.StopTime, MaxLoss: p.MaxLoss, Notes: p.Notes, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		return nil, fmt.Errorf("save day plan: %w", err)
	}
	return Get(ctx, q, userID, p.Day)
}

// ParseInt / ParseFloat read optional form numbers ("" is nil).
func ParseInt(s string) (*int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil, journal.BadInput("max trades must be a whole number")
	}
	return &v, nil
}

func ParseFloat(s string) (*float64, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	if s == "" {
		return nil, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, journal.BadInput("max loss must be a number")
	}
	return &v, nil
}

func fromRow(r models.DayPlan) Plan {
	return Plan{
		Day: r.Day, Bias: r.Bias, Levels: r.Levels, MaxTrades: r.MaxTrades, StopTime: r.StopTime,
		MaxLoss: r.MaxLoss, Notes: r.Notes, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}
