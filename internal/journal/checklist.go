package journal

import (
	"context"
	"fmt"
	"strings"

	"github.com/w4jnl/trading-journal/internal/models"
)

// Playbook checklists: a strategy's rules as tick items, ticked per entry.

// ChecklistItem is one rule of a strategy's checklist.
type ChecklistItem struct {
	ID       int64  `json:"id"`
	Text     string `json:"text"`
	Checked  bool   `json:"checked"`
	Position int64  `json:"-"`
}

// Checklist is an entry's view of its strategy's checklist.
type Checklist struct {
	StrategyID int64           `json:"strategyId"`
	Items      []ChecklistItem `json:"items"`
	Done       int             `json:"done"`
	Total      int             `json:"total"`
}

// Missed lists the unticked items' texts.
func (c *Checklist) Missed() []string {
	var out []string
	for _, it := range c.Items {
		if !it.Checked {
			out = append(out, it.Text)
		}
	}
	return out
}

// Complete reports every item ticked (false on an empty checklist).
func (c *Checklist) Complete() bool { return c.Total > 0 && c.Done == c.Total }

// ParseChecklistText turns the Library's one-item-per-line text into items;
// blank lines and leading list markers are dropped.
func ParseChecklistText(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "-*•0123456789.) ")
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// SetChecklistItems replaces a strategy's checklist with the given texts,
// in order. Items whose text is unchanged keep their id (and so every
// entry's ticks on them); removed items drop with their ticks.
func (s *Service) SetChecklistItems(ctx context.Context, userID, strategyID int64, texts []string) error {
	if _, err := s.Q.GetStrategy(ctx, models.GetStrategyParams{ID: strategyID, UserID: userID}); err != nil {
		return ErrNotFound
	}
	existing, err := s.Q.ListChecklistItems(ctx, strategyID)
	if err != nil {
		return fmt.Errorf("list checklist: %w", err)
	}
	byText := map[string]models.StrategyChecklistItem{}
	for _, it := range existing {
		if _, dup := byText[it.Text]; !dup {
			byText[it.Text] = it
		}
	}
	keep := map[int64]bool{}
	for pos, text := range texts {
		if it, ok := byText[text]; ok && !keep[it.ID] {
			keep[it.ID] = true
			if it.Position != int64(pos) {
				if err := s.Q.UpdateChecklistItemPosition(ctx, models.UpdateChecklistItemPositionParams{Position: int64(pos), ID: it.ID}); err != nil {
					return fmt.Errorf("move checklist item: %w", err)
				}
			}
			continue
		}
		id, err := s.Q.InsertChecklistItem(ctx, models.InsertChecklistItemParams{StrategyID: strategyID, Position: int64(pos), Text: text})
		if err != nil {
			return fmt.Errorf("insert checklist item: %w", err)
		}
		keep[id] = true
	}
	for _, it := range existing {
		if !keep[it.ID] {
			if err := s.Q.DeleteChecklistItem(ctx, it.ID); err != nil {
				return fmt.Errorf("delete checklist item: %w", err)
			}
		}
	}
	return nil
}

// ChecklistsForUser groups every item of the user's strategies by strategy.
func (s *Service) ChecklistsForUser(ctx context.Context, userID int64) (map[int64][]ChecklistItem, error) {
	rows, err := s.Q.ListChecklistItemsForUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list checklists: %w", err)
	}
	out := map[int64][]ChecklistItem{}
	for _, r := range rows {
		out[r.StrategyID] = append(out[r.StrategyID], ChecklistItem{ID: r.ID, Text: r.Text, Position: r.Position})
	}
	return out, nil
}

// EntryChecklist is the entry's strategy checklist with its ticks; nil when
// the entry has no strategy or the strategy has no items.
func (s *Service) EntryChecklist(ctx context.Context, userID, entryID int64) (*Checklist, error) {
	e, err := s.Q.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: entryID, UserID: &userID})
	if err != nil || e.StrategyID == nil {
		return nil, nil
	}
	items, err := s.Q.ListChecklistItems(ctx, *e.StrategyID)
	if err != nil {
		return nil, fmt.Errorf("list checklist: %w", err)
	}
	if len(items) == 0 {
		return nil, nil
	}
	ticked, err := s.Q.ListEntryChecks(ctx, entryID)
	if err != nil {
		return nil, fmt.Errorf("list checks: %w", err)
	}
	on := map[int64]bool{}
	for _, id := range ticked {
		on[id] = true
	}
	c := &Checklist{StrategyID: *e.StrategyID, Total: len(items)}
	for _, it := range items {
		ci := ChecklistItem{ID: it.ID, Text: it.Text, Position: it.Position, Checked: on[it.ID]}
		if ci.Checked {
			c.Done++
		}
		c.Items = append(c.Items, ci)
	}
	return c, nil
}

// applyFieldChecks stores the ticks of a save: only items of the entry's
// strategy count, so a stale tick from another strategy is dropped.
func (s *Service) applyFieldChecks(ctx context.Context, entryID int64, f Fields) error {
	if err := s.Q.DeleteEntryChecks(ctx, entryID); err != nil {
		return fmt.Errorf("clear checks: %w", err)
	}
	if f.StrategyID == nil || len(f.Checks) == 0 {
		return nil
	}
	items, err := s.Q.ListChecklistItems(ctx, *f.StrategyID)
	if err != nil {
		return fmt.Errorf("list checklist: %w", err)
	}
	valid := map[int64]bool{}
	for _, it := range items {
		valid[it.ID] = true
	}
	for _, id := range f.Checks {
		if !valid[id] {
			continue
		}
		if err := s.Q.InsertEntryCheck(ctx, models.InsertEntryCheckParams{JournalEntryID: entryID, ItemID: id}); err != nil {
			return fmt.Errorf("insert check: %w", err)
		}
	}
	return nil
}
