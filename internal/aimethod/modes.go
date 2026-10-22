package aimethod

import (
	"fmt"
	"sort"
	"strings"
)

// ---- The method as an external agent reads it ---------------------------
//
// An MCP client (Claude Desktop, Claude Code, ChatGPT) gets the method from
// the server: the instructions at initialize, the document as a resource and
// a tool, the modes as prompts. The client skill only points here, so the
// three copies that used to drift are one text that ships with the binary.

// Mode is one way of working with the journal from outside: the steps and
// the tools in order.
type Mode struct {
	Name    string // the MCP prompt's name
	Title   string
	Purpose string
	Args    []ModeArg
	Steps   []string
}

// ModeArg is a prompt argument.
type ModeArg struct {
	Name, Description string
	Required          bool
}

// Modes lists what an agent can be asked to do, in menu order.
var Modes = []Mode{
	{
		Name: "review_day", Title: "Review a trading day", Purpose: "Write the once-only review of every decision of a day and the day's synthesis.",
		Args: []ModeArg{{Name: "date", Description: "YYYY-MM-DD in the trading timezone; the last trading day before today when absent."}, {Name: "force", Description: "true to review entries that already carry the marker."}},
		Steps: []string{
			"Call `server_info` once and work against that instance for the whole run; take the language and the marker from it.",
			"Call `prepare_day` (with `date` when given). It clusters copytrader fan-out into group entries and lone trades into trade entries, creates the missing ones and returns `entries[]` with `entryId`, `scope`, `created`, `alreadyAnalyzed`, `label`, `netPnl` and `checks`. Work only from these entry ids; never pass raw trade ids.",
			"Call `list_strategies` once (id, name, playbook) and read `trading-journal://lessons` (or `list_lessons`) once: the open lessons are what the trader is working on.",
			"For every entry with `alreadyAnalyzed: false` (or every entry when forced): `get_chart_image(entryId)` and look at it; `get_journal_entry(entryId)` for the notes, labels, checklist and the assigned strategy; `get_levels(market, day)` once per market for the day map; judge the decision by the trade/group brief of the method, citing the entry's `checks` as evidence.",
			"Classify only what is unset and only when it clearly applies: `assign_strategy` only when the trade meets the playbook's hard conditions (its market, its session window against the entry time, its entry trigger: the strategy's checklist lists them), never by elimination; `set_tag` (good/neutral/bad on the process); `add_labels` (mistakes at most 3, emotions at most 2, each only when its description's evidence is in the context). The calls refuse to overwrite.",
			"`append_note(entryId, note)`: the review under a heading `### AI review (<model>) — <date>`, then the lesson line, then the marker line.",
			"Synthesise the day: `create_journal_entry(scope=\"day\", date, body)` with the day brief's sections built on the day's lessons; on `alreadyExists` append to that entry when it lacks the marker, else leave it.",
			"Print a short overview in chat: net, trades, win rate, entries reviewed and skipped, two to four recurring observations, anything needing attention.",
		},
	},
	{
		Name: "review_entry", Title: "Review one entry", Purpose: "Review a single trade, group, day or week entry.",
		Args: []ModeArg{{Name: "entryId", Description: "The journal entry id.", Required: true}, {Name: "force", Description: "true to review an entry that already carries the marker."}},
		Steps: []string{
			"`get_journal_entry(entryId)`; stop when the notes carry the marker unless forced.",
			"For a trade or group: `get_chart_image(entryId)`, `get_strategy` when one is assigned, `get_levels` for the day map, `get_coach_checks(day)` for the day's checks. For a day: `get_coach_checks` and `get_day_plan`. For a week: `get_weekly_digest`.",
			"Write the review by the entry's brief; classify what is unset when it clearly applies; `append_note` with the lesson line and the marker. Print the review in chat too.",
		},
	},
	{
		Name: "today", Title: "How am I doing today", Purpose: "The day so far, read-only: nothing is written.",
		Steps: []string{
			"`get_daily_stats(day=\"today\", refresh=true)` pulls the latest fills first; `list_trades(day=\"today\", refresh=true)` for the list (page through when `page.pages` > 1). Use `day=\"yesterday\"` or `\"last\"` when asked; never compute the date.",
			"Report net (gross and commission), the trades with wins, losses and win rate, the best and worst trade, the shape of the day in a few words. An empty list means no closed trade yet; open positions are not P&L. Do not start a review unless asked.",
		},
	},
	{
		Name: "review_week", Title: "Review a trading week", Purpose: "The week's review on the weekly digest.",
		Args: []ModeArg{{Name: "week", Description: "ISO week YYYY-Www; the last complete week when absent."}},
		Steps: []string{
			"`get_weekly_digest(week)`: its numbers are per decision and set against the week before.",
			"Write the review by the week brief (Result, Keep, Fix, Recurring, Focus for next week) and `create_journal_entry(scope=\"week\", ...)` with the marker, or `append_note` when the week entry exists without it.",
		},
	},
	{
		Name: "plan_day", Title: "Plan the session", Purpose: "Draft the day plan from the trader's norms, lessons and the day map.",
		Args: []ModeArg{{Name: "day", Description: "YYYY-MM-DD; today when absent."}},
		Steps: []string{
			"`get_day_plan(day)` to see what is written; `get_levels(market)` for each market the trader works; `list_lessons(open=true)`; `get_coach_checks` on the last trading days for the norms.",
			"Propose the decision cap, the stop time, the max loss and the levels (from the map's zones and the gamma levels, nearest first) with one reason each; never set a bias. `set_day_plan` only when the trader confirms.",
		},
	},
}

// WriteRules are the two write semantics, side by side.
const WriteRules = `Writes from outside the app are additive and only-if-unset: append_note adds to a note, never replaces; assign_strategy, set_tag and add_labels fill a blank and refuse to overwrite; set_day_plan and set_position_plan record with source "api" until the trader saves them. The library is the exception: create_strategy and update_strategy write the trader's strategies at the trader's request, and update_strategy replaces the name, the playbook or the checklist it is given (append and images add), so read the strategy first and change it only when the trader asks. Every review you append ends with the lesson line and then the marker line, so a re-run skips it and the lessons ledger keeps the lesson. Ids are different namespaces (trade, entry, account): when a tool says "not found", re-fetch and use the exact value, never invent one. The in-app reviewer works the same method but records tags, strategies and labels as suggestions the trader accepts.`

// MarkerLine is the line an external agent writes at the end of a review.
func MarkerLine() string { return "<!-- " + Markers.MCP + " method=" + Version + " -->" }

// PrepareNote is what prepare_day says to the agent.
func PrepareNote() string {
	return "For each entry with alreadyAnalyzed=false: get_chart_image(entryId=...) and get_journal_entry(entryId) (+ get_strategy if one is assigned), judge it by the method (resource trading-journal://method, tool get_review_method), then append_note(entryId, analysis ending in the lesson line and " + MarkerLine() + "). Then synthesize the day. Only ever pass the entryId values above - never raw trade ids."
}

// Instructions is what the MCP server says at initialize: who the agent is,
// where the method is, the marker, the write rules.
func Instructions() string {
	return strings.Join([]string{
		"You are working with a futures day trader's journal. " + Role,
		"The method (how to review a trade, a group, a day or a week; how to plan a session) lives on this server, versioned: read the resource trading-journal://method or call get_review_method at the start of a session and follow it; the prompts review_day, review_entry, today, review_week and plan_day carry the same steps. Call server_info first: it names the instance, the trader's review language and profile (their style, the evidence they weigh most, their markets, their standing instructions; also the resource trading-journal://profile), the marker and the method version.",
		"Every review you write ends with the lesson line <!-- tj-ai:lesson key=<mistake|none> | <one sentence> --> and then the marker line " + MarkerLine() + "; a note that already carries the marker is done. Write the prose in the trader's language from server_info; the machine-readable lines stay English.",
		WriteRules,
		"Dates: day, from and to on list_trades and get_daily_stats accept today, yesterday and last, resolved on the server in the trading timezone; never compute a date yourself.",
	}, "\n\n")
}

// DocOptions shape the method document.
type DocOptions struct {
	Tools    []string // the MCP tool names, listed when given
	Language string
	Profile  string // the trader's profile and house rules, when set
}

// Document is the whole method as markdown: for the resource, the tool and
// the REST endpoint, and what the client skills point at.
func Document(o DocOptions) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# The review method, version %s\n\n", Version)
	b.WriteString("## The analyst\n\n" + Role + "\n\n" + Grounding + "\n\n" + DayMapRule + "\n\n" + Format + "\n\n")
	if o.Language != "" {
		b.WriteString("## Language\n\nWrite the prose in " + o.Language + "; the machine-readable lines, the label keys and the JSON keys stay English.\n\n")
	}
	if o.Profile != "" {
		b.WriteString("## The trader\n\n" + o.Profile + "\n\n")
	}
	b.WriteString("## The briefs\n\n")
	keys := make([]Scope, 0, len(Scopes))
	for k := range Scopes {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return scopeOrder[keys[i]] < scopeOrder[keys[j]] })
	for _, k := range keys {
		fmt.Fprintf(&b, "### %s (skill version %s)\n\n%s\n\n", strings.ToUpper(string(k)[:1])+string(k)[1:], Scopes[k].Version, Scopes[k].Brief)
	}
	b.WriteString("## The lines every review ends with\n\n" + LessonLine() + "\n\nFor a trade or a group: " + VerdictDecision + "\n\nFor a day or a week: " + VerdictTagOnly + "\n\nAn external agent writes the marker line last: `" + MarkerLine() + "`; the in-app reviewer writes `<!-- " + Markers.Local + " method=" + Version + " -->`.\n\n")
	b.WriteString("## Writing to the journal\n\n" + WriteRules + "\n\n")
	b.WriteString("## Modes\n\n")
	for _, m := range Modes {
		fmt.Fprintf(&b, "### %s (`%s`)\n\n%s\n\n", m.Title, m.Name, m.Purpose)
		for _, a := range m.Args {
			req := ""
			if a.Required {
				req = " (required)"
			}
			fmt.Fprintf(&b, "- `%s`%s: %s\n", a.Name, req, a.Description)
		}
		if len(m.Args) > 0 {
			b.WriteString("\n")
		}
		for i, s := range m.Steps {
			fmt.Fprintf(&b, "%d. %s\n", i+1, s)
		}
		b.WriteString("\n")
	}
	b.WriteString("## The evidence\n\n")
	gk := make([]string, 0, len(Glossary))
	for k := range Glossary {
		gk = append(gk, k)
	}
	sort.Strings(gk)
	for _, k := range gk {
		fmt.Fprintf(&b, "- `%s`: %s\n", k, Glossary[k])
	}
	if len(o.Tools) > 0 {
		b.WriteString("\n## Tools\n\n")
		for _, t := range o.Tools {
			fmt.Fprintf(&b, "- `%s`\n", t)
		}
	}
	return b.String()
}

// PromptText is a mode as one user message for an MCP prompt: the steps with
// the arguments filled in, the trader's profile block (Profile.Block, empty
// for the default), the language and the marker.
func PromptText(m Mode, args map[string]string, language, profile string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n\n", m.Title, m.Purpose)
	for _, a := range m.Args {
		if v := strings.TrimSpace(args[a.Name]); v != "" {
			fmt.Fprintf(&b, "- %s = %s\n", a.Name, v)
		}
	}
	if len(m.Args) > 0 {
		b.WriteString("\n")
	}
	for i, s := range m.Steps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, s)
	}
	if profile != "" {
		b.WriteString("\n" + profile + "\n")
	}
	b.WriteString("\nFollow the method on this server (resource trading-journal://method, tool get_review_method) for what to write.")
	if language != "" {
		b.WriteString(" Write the prose in " + language + "; the machine-readable lines stay English.")
	}
	b.WriteString(" End every review with the lesson line and then " + MarkerLine() + ".")
	return b.String()
}

// ModeByName finds a mode.
func ModeByName(name string) (Mode, bool) {
	for _, m := range Modes {
		if m.Name == name {
			return m, true
		}
	}
	return Mode{}, false
}
