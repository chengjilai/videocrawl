package dom

import (
	"bytes"
	"io"
	"io/ioutil"
	"regexp"
	"unicode/utf8"

	"github.com/gogs/chardet"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
	"golang.org/x/text/encoding"
	xunicode "golang.org/x/text/encoding/unicode"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// FastParse parses html.Node from the specified reader without caring about
// text encoding. It always assume that the input uses UTF-8 encoding.
func FastParse(r io.Reader) (*html.Node, error) {
	return html.Parse(r)
}

// Parse parses html.Node from the specified reader while converting the character
// encoding into UTF-8. This function is useful to correctly parse web pages that
// uses custom text encoding, e.g. web pages from Asian websites. However, since it
// has to detect charset before parsing, this function is quite slow and expensive
// so if you sure the reader uses valid UTF-8, just use FastParse.
func Parse(r io.Reader) (*html.Node, error) {
	// Split the reader using tee
	content, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}

	// Detect page encoding. chardet sniffing only runs when the page declares
	// no charset AND its bytes are not valid UTF-8 (see detectEncoding) — the
	// common case (UTF-8 declared or default) never pays for it.
	pageEncoding, _ := detectEncoding(content)

	// Parse HTML using the page encoding
	r = bytes.NewReader(content)
	r = transform.NewReader(r, pageEncoding.NewDecoder())
	r = normalizeTextEncoding(r)
	return html.Parse(r)
}

// detectEncoding decides how to convert the page's raw bytes into UTF-8
// without paying for chardet charset sniffing when it isn't needed:
//
//  1. A charset declared in a <meta> tag (either the HTML5
//     <meta charset="utf-8"> form or the HTML4
//     <meta http-equiv="Content-Type" content="text/html; charset=utf-8">
//     form) is honored directly. A non-UTF-8 declaration is only trusted
//     when the bytes are not already valid UTF-8: a valid UTF-8 byte stream
//     is authoritative, which also matches what chardet would have concluded
//     and keeps mis-declared pages readable.
//  2. Otherwise, a byte stream that is already valid UTF-8 is used as-is.
//     This is the overwhelmingly common case for crawled pages (UTF-8
//     declared or default) and used to cost a full chardet scan.
//  3. Only when the page has no charset info AND the bytes are not valid
//     UTF-8 do we fall back to the original chardet sniffing.
func detectEncoding(content []byte) (encoding.Encoding, string) {
	if declared := declaredCharset(content); declared != "" {
		if e, name := charset.Lookup(declared); e != nil && (name == "utf-8" || !utf8.Valid(content)) {
			return e, name
		}
	}

	if utf8.Valid(content) {
		return xunicode.UTF8, "utf-8"
	}

	res, err := chardet.NewHtmlDetector().DetectBest(content)
	if err != nil {
		return xunicode.UTF8, ""
	}

	pageEncoding, name := charset.Lookup(res.Charset)
	if pageEncoding == nil {
		pageEncoding, name = xunicode.UTF8, "utf-8"
	}
	return pageEncoding, name
}

var (
	// rxMetaTag matches an opening <meta ...> tag. Assuming attribute values
	// do not contain '>' is a safe simplification for encoding sniffing.
	rxMetaTag = regexp.MustCompile(`(?i)<meta[^>]*>`)

	// rxMetaHTTPEquivCT matches http-equiv="content-type".
	rxMetaHTTPEquivCT = regexp.MustCompile(`(?i)\bhttp-equiv\s*=\s*["']?\s*content-type\b`)

	// rxCharsetParam matches charset=... both as a bare attribute (HTML5
	// <meta charset="utf-8">) and as a parameter of the content value
	// (HTML4 content="text/html; charset=utf-8").
	rxCharsetParam = regexp.MustCompile(`(?i)\bcharset\s*=\s*["']?\s*([a-z0-9._:+-]+)`)
)

// declaredCharset returns the charset declared in the document's <meta> tags,
// or "" when the document declares none. The HTML spec requires charset
// declarations to appear within the first 1024 bytes of the document, so only
// that prefix is scanned.
func declaredCharset(content []byte) string {
	if len(content) > 1024 {
		content = content[:1024]
	}

	for _, tag := range rxMetaTag.FindAll(content, -1) {
		if rxMetaHTTPEquivCT.Match(tag) {
			// HTML4 form: charset is a parameter of the Content-Type value,
			// e.g. <meta http-equiv="Content-Type"
			//           content="text/html; charset=utf-8">
			if m := rxCharsetParam.FindSubmatch(tag); m != nil {
				return string(m[1])
			}
			continue
		}
		// HTML5 form: <meta charset="utf-8">
		if m := rxCharsetParam.FindSubmatch(tag); m != nil {
			return string(m[1])
		}
	}
	return ""
}

// normalizeTextEncoding convert text encoding from NFD to NFC.
// It also remove soft hyphen since apparently it's useless in web.
// See: https://web.archive.org/web/19990117011731/http://www.hut.fi/~jkorpela/shy.html
func normalizeTextEncoding(r io.Reader) io.Reader {
	fnSoftHyphen := func(r rune) bool { return r == '\u00AD' }
	softHyphenSet := runes.Predicate(fnSoftHyphen)
	transformer := transform.Chain(norm.NFD, runes.Remove(softHyphenSet), norm.NFC)
	return transform.NewReader(r, transformer)
}
