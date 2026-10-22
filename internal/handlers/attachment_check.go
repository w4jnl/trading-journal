package handlers

import (
	"bytes"
	"fmt"
	"image"
	"image/gif"
	_ "image/jpeg" // registers the decoder image.Decode needs
	_ "image/png"  // registers the decoder image.Decode needs
	"net/http"
	"strings"

	_ "golang.org/x/image/webp" // registers the decoder image.Decode needs
)

// maxAttachmentPixels bounds the decoded size of a raster attachment: the
// bytes are small, the pixels are what decoding costs. 50 megapixels is
// more than an 8K screenshot.
const maxAttachmentPixels = 50_000_000

// attachmentTypeNames are the words the errors use for the stored types.
var attachmentTypeNames = map[string]string{
	"image/png":     "PNG",
	"image/jpeg":    "JPEG",
	"image/gif":     "GIF",
	"image/webp":    "WebP",
	"image/svg+xml": "SVG",
}

// attachmentTypesSentence lists the stored types for an error message.
const attachmentTypesSentence = "PNG, JPEG, GIF, WebP or SVG"

// sniffAttachmentMIME names the image type the bytes are, from the bytes
// alone, or "" when they are not an image this app stores. A client's
// declared type is not consulted: a clipboard paste declares nothing and a
// mislabelled file would be served with the wrong type.
func sniffAttachmentMIME(data []byte) string {
	mt := http.DetectContentType(data)
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	if _, ok := attachmentTypeNames[mt]; ok && mt != "image/svg+xml" {
		return mt
	}
	if looksLikeSVG(data) {
		return "image/svg+xml"
	}
	return ""
}

// looksLikeSVG reports whether the bytes start like an SVG document: markup
// with an <svg element near the top. sanitizeSVG does the real check.
func looksLikeSVG(data []byte) bool {
	head := data
	if len(head) > 4096 {
		head = head[:4096]
	}
	head = bytes.TrimLeft(head, "\xef\xbb\xbf \t\r\n")
	return bytes.HasPrefix(head, []byte("<")) && bytes.Contains(head, []byte("<svg"))
}

// checkAttachmentImage verifies that data is a complete image of type mt
// (one of allowedAttachmentMIMEs) and returns the bytes to store: the input
// for a raster image, the sanitized document for an SVG. A raster image is
// decoded whole, not just its header, so a stream that breaks halfway (the
// usual damage when an agent copies base64 by hand) is refused with a
// message the client can act on instead of being stored as a broken image.
func checkAttachmentImage(mt string, data []byte) ([]byte, error) {
	name := attachmentTypeNames[mt]
	switch mt {
	case "image/svg+xml":
		out, err := sanitizeSVG(data)
		if err != nil {
			return nil, fmt.Errorf("not a well-formed SVG document (%v)", err)
		}
		return out, nil
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return nil, fmt.Errorf("unsupported type %q", mt)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("corrupt %s (%v)", name, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxAttachmentPixels {
		return nil, fmt.Errorf("%s is %dx%d pixels, at most %d megapixels are stored", name, cfg.Width, cfg.Height, maxAttachmentPixels/1_000_000)
	}
	if mt == "image/gif" {
		_, err = gif.DecodeAll(bytes.NewReader(data))
	} else {
		_, _, err = image.Decode(bytes.NewReader(data))
	}
	if err != nil {
		return nil, fmt.Errorf("corrupt %s (%v)", name, err)
	}
	return data, nil
}
