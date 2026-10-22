package handlers

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// An SVG attachment is text an agent can author exactly, so the strategy
// writes and the uploads accept it; but an SVG is also a document that can
// carry scripts, event handlers and references to other origins. sanitizeSVG
// rebuilds the drawing from an allowlist: it parses the XML, keeps the
// elements and attributes a static drawing needs, drops everything else
// (scripts, foreignObject, external images, animation, event handlers,
// references that leave the file) and serialises the result itself, so what
// is stored is a document this code wrote. The attachment route adds a
// Content-Security-Policy that forbids scripts and loads on top, for the
// case of a browser opening the file directly rather than in an <img>.

const (
	svgNS   = "http://www.w3.org/2000/svg"
	xlinkNS = "http://www.w3.org/1999/xlink"

	// svgMaxDepth and svgMaxElements bound the work a pathological document
	// can cause; a hand-drawn chart annotation is a few hundred elements.
	svgMaxDepth    = 128
	svgMaxElements = 50000
)

// svgElements are the elements kept. Everything else is dropped with its
// subtree, except svgUnwrapped elements, whose children are kept in place.
var svgElements = setOf(
	"svg", "g", "defs", "title", "desc", "symbol", "use", "switch", "style",
	"path", "rect", "circle", "ellipse", "line", "polyline", "polygon",
	"text", "tspan", "textPath",
	"marker", "pattern", "clipPath", "mask",
	"linearGradient", "radialGradient", "stop",
	"filter", "feBlend", "feColorMatrix", "feComponentTransfer", "feComposite",
	"feConvolveMatrix", "feDiffuseLighting", "feDisplacementMap", "feDropShadow",
	"feFlood", "feFuncA", "feFuncB", "feFuncG", "feFuncR", "feGaussianBlur",
	"feMerge", "feMergeNode", "feMorphology", "feOffset", "feSpecularLighting",
	"feTile", "feTurbulence", "feDistantLight", "fePointLight", "feSpotLight",
)

// svgUnwrapped are dropped but keep their children: a link around a shape
// loses the link, the shape stays.
var svgUnwrapped = setOf("a")

// svgAttributes are the attributes kept on any kept element; href is handled
// apart (local references only) and namespace declarations are regenerated.
var svgAttributes = setOf(
	// identity and layout
	"id", "class", "style", "transform", "viewBox", "preserveAspectRatio",
	"width", "height", "x", "y", "x1", "y1", "x2", "y2", "cx", "cy", "r", "rx", "ry",
	"d", "points", "dx", "dy", "rotate", "textLength", "lengthAdjust", "pathLength",
	"startOffset", "method", "spacing", "side", "version", "requiredFeatures", "systemLanguage",
	// paint servers, clips, masks, markers, patterns
	"offset", "gradientUnits", "gradientTransform", "spreadMethod", "fx", "fy", "fr",
	"patternUnits", "patternContentUnits", "patternTransform",
	"clipPathUnits", "maskUnits", "maskContentUnits",
	"markerWidth", "markerHeight", "markerUnits", "refX", "refY", "orient",
	// filters
	"filterUnits", "primitiveUnits", "in", "in2", "result", "mode", "type", "values",
	"stdDeviation", "operator", "k1", "k2", "k3", "k4", "radius", "scale",
	"xChannelSelector", "yChannelSelector", "baseFrequency", "numOctaves", "seed",
	"stitchTiles", "surfaceScale", "diffuseConstant", "specularConstant", "specularExponent",
	"kernelMatrix", "order", "divisor", "bias", "targetX", "targetY", "edgeMode",
	"kernelUnitLength", "preserveAlpha", "azimuth", "elevation", "z", "pointsAtX",
	"pointsAtY", "pointsAtZ", "limitingConeAngle", "tableValues", "slope", "intercept",
	"amplitude", "exponent", "flood-color", "flood-opacity", "lighting-color",
	// presentation
	"fill", "fill-opacity", "fill-rule", "stroke", "stroke-width", "stroke-dasharray",
	"stroke-dashoffset", "stroke-linecap", "stroke-linejoin", "stroke-miterlimit",
	"stroke-opacity", "opacity", "color", "display", "visibility", "overflow",
	"clip-path", "clip-rule", "mask", "marker-start", "marker-mid", "marker-end",
	"stop-color", "stop-opacity", "paint-order", "vector-effect", "shape-rendering",
	"text-rendering", "image-rendering", "color-interpolation", "color-interpolation-filters",
	"mix-blend-mode", "isolation", "filter",
	"font-family", "font-size", "font-size-adjust", "font-weight", "font-style",
	"font-variant", "font-stretch", "letter-spacing", "word-spacing", "text-anchor",
	"text-decoration", "dominant-baseline", "alignment-baseline", "baseline-shift",
	"direction", "writing-mode", "unicode-bidi", "white-space",
)

func setOf(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// svgRemoteRe finds a url() whose target is not a fragment of this document,
// and the at-rules that fetch (@import) in a style sheet or style attribute.
var svgRemoteRe = regexp.MustCompile(`(?i)url\(\s*["']?\s*[^#"'\s)]|@import|expression\(`)

// svgSchemeRe finds a URL scheme anywhere in an attribute value, ignoring the
// whitespace and control characters a browser would skip.
var svgSchemeRe = regexp.MustCompile(`(?i)(javascript|data|vbscript|file):`)

// svgValueSafe reports whether an attribute value or style text may be kept.
func svgValueSafe(v string) bool {
	compact := strings.Map(func(r rune) rune {
		if r <= ' ' || r == 0x7f {
			return -1
		}
		return r
	}, v)
	if svgSchemeRe.MatchString(compact) {
		return false
	}
	return !svgRemoteRe.MatchString(v)
}

// sanitizeSVG returns the drawing rebuilt from the allowlist, or an error
// when the bytes are not a well-formed SVG document.
func sanitizeSVG(data []byte) ([]byte, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = true
	dec.Entity = xml.HTMLEntity
	// Only UTF-8 (and ASCII) documents: no charset conversion.
	dec.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		switch strings.ToLower(charset) {
		case "utf-8", "utf8", "us-ascii", "ascii":
			return input, nil
		}
		return nil, fmt.Errorf("svg: unsupported encoding %q", charset)
	}

	var out bytes.Buffer
	out.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")

	// skip > 0 means we are inside a dropped subtree, skip deep. stack holds
	// the open elements that were not dropped and whether each is emitted
	// (an unwrapped element is on the stack, with no tags written).
	type openEl struct {
		name string
		emit bool
	}
	var (
		skip     int
		stack    []openEl
		rootSeen bool
		rootDone bool
		elements int
	)
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("svg: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if skip > 0 {
				skip++
				continue
			}
			if rootDone {
				return nil, errors.New("svg: content after the root element")
			}
			elements++
			if elements > svgMaxElements {
				return nil, errors.New("svg: too many elements")
			}
			if len(stack) >= svgMaxDepth {
				return nil, errors.New("svg: nested too deep")
			}
			name := t.Name.Local
			nsOK := t.Name.Space == "" || t.Name.Space == svgNS
			if !rootSeen {
				if name != "svg" || !nsOK {
					return nil, errors.New("svg: the root element is not <svg>")
				}
				rootSeen = true
			}
			if !nsOK || !svgElements[name] {
				if svgUnwrapped[name] && nsOK {
					stack = append(stack, openEl{name: name})
					continue
				}
				skip = 1
				continue
			}
			stack = append(stack, openEl{name: name, emit: true})
			out.WriteByte('<')
			out.WriteString(name)
			if len(stack) == 1 {
				out.WriteString(` xmlns="` + svgNS + `"`)
			}
			for _, a := range t.Attr {
				writeSVGAttr(&out, a)
			}
			out.WriteByte('>')
		case xml.EndElement:
			if skip > 0 {
				skip--
				continue
			}
			if len(stack) == 0 {
				return nil, errors.New("svg: unbalanced element")
			}
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if top.emit {
				out.WriteString("</")
				out.WriteString(t.Name.Local)
				out.WriteByte('>')
			}
			if len(stack) == 0 {
				rootDone = true
			}
		case xml.CharData:
			if skip > 0 || !rootSeen {
				continue
			}
			if rootDone {
				if strings.TrimSpace(string(t)) != "" {
					return nil, errors.New("svg: content after the root element")
				}
				continue
			}
			if len(stack) > 0 && stack[len(stack)-1].name == "style" && !svgValueSafe(string(t)) {
				// A style sheet that fetches or runs: dropped whole.
				continue
			}
			svgEscape(&out, string(t))
		case xml.Comment, xml.ProcInst, xml.Directive:
			// Comments and processing instructions carry nothing a drawing
			// needs; a DOCTYPE could declare entities. All dropped.
		}
	}
	if !rootSeen || !rootDone || len(stack) != 0 {
		return nil, errors.New("svg: document is incomplete")
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// writeSVGAttr writes one attribute when the allowlist keeps it. Namespace
// declarations are dropped (the root gets its own xmlns), event handlers
// (on*) and unknown attributes are dropped, href and xlink:href are kept
// only as local references (#id) and written as href, and a value that
// names a URL scheme or fetches through url() is dropped.
func writeSVGAttr(out *bytes.Buffer, a xml.Attr) {
	name := a.Name.Local
	switch a.Name.Space {
	case "", svgNS:
	case xlinkNS:
		if name != "href" {
			return
		}
	default:
		return // xmlns:*, xml:*, foreign namespaces
	}
	if name == "xmlns" || strings.HasPrefix(strings.ToLower(name), "on") {
		return
	}
	val := a.Value
	if name == "href" {
		v := strings.TrimSpace(val)
		if !strings.HasPrefix(v, "#") || len(v) < 2 {
			return
		}
		val = v
	} else if !svgAttributes[name] {
		return
	}
	if !svgValueSafe(val) {
		return
	}
	out.WriteByte(' ')
	out.WriteString(name)
	out.WriteString(`="`)
	svgEscape(out, val)
	out.WriteByte('"')
}

// svgEscape writes s as XML text or attribute content, escaping only the
// markup characters (xml.EscapeText would also turn newlines and tabs into
// character references, which makes a style sheet or a text run unreadable).
func svgEscape(out *bytes.Buffer, s string) {
	for _, r := range s {
		switch r {
		case '&':
			out.WriteString("&amp;")
		case '<':
			out.WriteString("&lt;")
		case '>':
			out.WriteString("&gt;")
		case '"':
			out.WriteString("&quot;")
		default:
			out.WriteRune(r)
		}
	}
}
