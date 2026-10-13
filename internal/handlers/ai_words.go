package handlers

import (
	"net/url"
	"strings"
)

// aiWords is the fixed text the app writes around an in-app review, in the
// review language chosen in Settings > AI, so a Czech review is not framed in
// English. Tag values and label names stay as the app shows them on the
// pills. Every value offered in aiLanguageOptions needs an entry (a test
// checks); English is the fallback.
type aiWords struct {
	Review     string // the heading: "AI review — 2026-10-13"
	NoteAdded  string // the stamp when a review is appended to a written note
	Via        string // "claude-sonnet-5-5 via Anthropic (Claude)"
	ThisServer string // names the server-wide default as the provider
	Filled     string // "Filled in by this review: tag bad · ..."
	Tag        string
	Strategy   string
	Mistakes   string
	Emotions   string
}

var aiWordsByLanguage = map[string]aiWords{
	"":           {"AI review", "Note added", "via", "this server", "Filled in by this review", "tag", "strategy", "mistakes", "emotions"},
	"Czech":      {"AI hodnocení", "Poznámka přidána", "přes", "tento server", "Doplněno tímto hodnocením", "štítek", "strategie", "chyby", "emoce"},
	"Slovak":     {"AI hodnotenie", "Poznámka pridaná", "cez", "tento server", "Doplnené týmto hodnotením", "štítok", "stratégia", "chyby", "emócie"},
	"Dutch":      {"AI-review", "Notitie toegevoegd", "via", "deze server", "Ingevuld door deze review", "tag", "strategie", "fouten", "emoties"},
	"German":     {"KI-Review", "Notiz hinzugefügt", "über", "diesen Server", "Von diesem Review ergänzt", "Tag", "Strategie", "Fehler", "Emotionen"},
	"French":     {"Analyse IA", "Note ajoutée", "via", "ce serveur", "Complété par cette analyse", "tag", "stratégie", "erreurs", "émotions"},
	"Spanish":    {"Análisis de IA", "Nota añadida", "vía", "este servidor", "Completado por este análisis", "etiqueta", "estrategia", "errores", "emociones"},
	"Italian":    {"Analisi IA", "Nota aggiunta", "tramite", "questo server", "Compilato da questa analisi", "tag", "strategia", "errori", "emozioni"},
	"Polish":     {"Analiza AI", "Notatka dodana", "przez", "ten serwer", "Uzupełnione przez tę analizę", "tag", "strategia", "błędy", "emocje"},
	"Portuguese": {"Análise de IA", "Nota adicionada", "via", "este servidor", "Preenchido por esta análise", "etiqueta", "estratégia", "erros", "emoções"},
}

// aiWordsFor returns the review framing for a language, English when unknown.
func aiWordsFor(language string) aiWords {
	if w, ok := aiWordsByLanguage[language]; ok {
		return w
	}
	return aiWordsByLanguage[""]
}

// aiModelRef names the model a review ran on: its id and where it ran. It is
// rendered per language, so the line under a Czech review reads
// "claude-sonnet-5-5 přes Anthropic (Claude)".
type aiModelRef struct {
	Model    string // the model id; "" for an unnamed server default
	Provider string // the hosted provider's label or a custom endpoint's host; "" = the server default
}

// Label renders the reference in w's words.
func (m aiModelRef) Label(w aiWords) string {
	where := m.Provider
	if where == "" {
		where = w.ThisServer
	}
	if m.Model == "" {
		return where
	}
	return m.Model + " " + w.Via + " " + where
}

// customHost is what a custom endpoint is called in the model line: its
// host, not a label in the app's language.
func customHost(baseURL string) string {
	if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return strings.TrimSpace(baseURL)
}
