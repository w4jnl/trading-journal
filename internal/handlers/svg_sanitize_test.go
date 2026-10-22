package handlers

import (
	"strings"
	"testing"
)

// The sanitizer keeps a drawing and drops what could run or fetch: the
// result is a document this code wrote, with the SVG namespace declared.
func TestSanitizeSVG(t *testing.T) {
	src := `<?xml version="1.0"?>
<!DOCTYPE svg [ <!ENTITY harmless "x"> ]>
<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" viewBox="0 0 200 100" width="200" height="100" onload="alert(1)">
  <!-- a comment -->
  <defs>
    <linearGradient id="g"><stop offset="0" stop-color="#0f0"/><stop offset="1" stop-color="red"/></linearGradient>
    <symbol id="arrow"><path d="M0 0 L10 5 L0 10z"/></symbol>
  </defs>
  <style>.lbl { font: 12px sans-serif; fill: url(#g); }</style>
  <style>@import url("https://evil.example/x.css");</style>
  <script>alert(document.cookie)</script>
  <rect x="10" y="10" width="80" height="40" fill="url(#g)" stroke="black" onclick="steal()"/>
  <rect x="10" y="60" width="80" height="20" fill="url(https://evil.example/a.svg#x)"/>
  <use xlink:href="#arrow" x="100" y="20"/>
  <use href="https://evil.example/a.svg#x" x="100" y="40"/>
  <image href="https://evil.example/track.png" width="10" height="10"/>
  <a href="javascript:alert(2)"><circle cx="150" cy="50" r="20" class="lbl"/></a>
  <foreignObject width="100" height="50"><body xmlns="http://www.w3.org/1999/xhtml"><iframe src="https://evil.example"></iframe></body></foreignObject>
  <text x="20" y="90" class="lbl" style="fill: red; background: url(https://evil.example/b.png)">Stop &amp; go &lt;here&gt;</text>
  <animate attributeName="href" to="https://evil.example" dur="1s"/>
</svg>
`
	out, err := sanitizeSVG([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, absent := range []string{"<script", "alert(", "onload", "onclick", "evil.example", "<image", "<foreignObject", "<iframe", "<a ", "<animate", "<!--", "DOCTYPE", "@import", "javascript:", "background"} {
		if strings.Contains(got, absent) {
			t.Errorf("sanitized SVG still contains %q\n---\n%s", absent, got)
		}
	}
	for _, present := range []string{
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 200 100" width="200" height="100">`,
		`<linearGradient id="g">`, `<stop offset="0" stop-color="#0f0"></stop>`,
		`<style>.lbl { font: 12px sans-serif; fill: url(#g); }</style>`,
		`<rect x="10" y="10" width="80" height="40" fill="url(#g)" stroke="black"></rect>`,
		`<rect x="10" y="60" width="80" height="20"></rect>`,
		`<use href="#arrow" x="100" y="20"></use>`, `<use x="100" y="40"></use>`,
		`<circle cx="150" cy="50" r="20" class="lbl"></circle>`,
		`<text x="20" y="90" class="lbl">Stop &amp; go &lt;here&gt;</text>`,
	} {
		if !strings.Contains(got, present) {
			t.Errorf("sanitized SVG lost %q\n---\n%s", present, got)
		}
	}
	if !strings.Contains(got, "<style></style>") {
		t.Errorf("the fetching style sheet should be emptied, got\n%s", got)
	}
	if strings.Contains(got, "&#x") {
		t.Errorf("whitespace escaped as character references\n%s", got)
	}
	// Sanitizing the output again changes nothing.
	again, err := sanitizeSVG(out)
	if err != nil || string(again) != got {
		t.Errorf("not idempotent: %v\n%s", err, again)
	}
}

// A document without the namespace gets it; what is not an SVG document is
// refused with a reason.
func TestSanitizeSVGRejects(t *testing.T) {
	out, err := sanitizeSVG([]byte(`<svg viewBox="0 0 10 10"><circle cx="5" cy="5" r="4"/></svg>`))
	if err != nil || !strings.Contains(string(out), `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10">`) {
		t.Errorf("bare svg: %v\n%s", err, out)
	}
	for name, src := range map[string]string{
		"html":          `<html><body><svg/></body></html>`,
		"truncated":     `<svg xmlns="http://www.w3.org/2000/svg"><rect width="1" height="1"/>`,
		"two roots":     `<svg xmlns="http://www.w3.org/2000/svg"/><svg xmlns="http://www.w3.org/2000/svg"/>`,
		"entity":        `<!DOCTYPE svg [<!ENTITY a "aaaa"><!ENTITY b "&a;&a;">]><svg xmlns="http://www.w3.org/2000/svg"><title>&b;</title></svg>`,
		"not xml":       `just text`,
		"png bytes":     "\x89PNG\r\n\x1a\n",
		"other charset": `<?xml version="1.0" encoding="UTF-16"?><svg xmlns="http://www.w3.org/2000/svg"/>`,
	} {
		if _, err := sanitizeSVG([]byte(src)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The sniff names the type from the bytes; the check decodes the whole
// image and refuses what breaks halfway.
func TestCheckAttachmentImage(t *testing.T) {
	svg := []byte("\xef\xbb\xbf  <svg xmlns=\"http://www.w3.org/2000/svg\"><rect width=\"1\" height=\"1\"/></svg>")
	if mt := sniffAttachmentMIME(svg); mt != "image/svg+xml" {
		t.Errorf("svg sniffed as %q", mt)
	}
	if mt := sniffAttachmentMIME([]byte("<html><svg/></html>")); mt != "image/svg+xml" {
		t.Errorf("html with svg sniffed as %q (the sanitizer refuses it)", mt)
	} else if _, err := checkAttachmentImage(mt, []byte("<html><svg/></html>")); err == nil {
		t.Error("html stored as svg")
	}
	if mt := sniffAttachmentMIME([]byte("plain words")); mt != "" {
		t.Errorf("text sniffed as %q", mt)
	}
	png := tinyPNGBytes(t)
	if mt := sniffAttachmentMIME(png); mt != "image/png" {
		t.Errorf("png sniffed as %q", mt)
	}
	if out, err := checkAttachmentImage("image/png", png); err != nil || len(out) != len(png) {
		t.Errorf("good png: %v", err)
	}
	broken := append([]byte{}, png[:len(png)-8]...)
	if _, err := checkAttachmentImage("image/png", broken); err == nil || !strings.Contains(err.Error(), "corrupt PNG") {
		t.Errorf("truncated png: %v", err)
	}
	flipped := append([]byte{}, png...)
	flipped[len(flipped)-20] ^= 0xff // inside the IDAT stream
	if _, err := checkAttachmentImage("image/png", flipped); err == nil {
		t.Error("damaged png accepted")
	}
	header := append([]byte{}, png[:33]...) // signature + IHDR only: the header decodes, the image does not
	if _, err := checkAttachmentImage("image/png", header); err == nil {
		t.Error("header-only png accepted")
	}
}
