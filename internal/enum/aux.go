package enum

import (
	"encoding/xml"
	"io"
	"time"
)

// xmlUnmarshal: xml.Unmarshal wrapper (keeps the rss parser here).
func xmlUnmarshal(r io.Reader, v any) error {
	return xml.NewDecoder(r).Decode(v)
}

// parseRSSDate: handles the RFC1123/RFC822 forms feeds actually use.
func parseRSSDate(s string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC1123, time.RFC1123Z, time.RFC822, time.RFC822Z,
		time.RFC3339, "Mon, 02 Jan 2006 15:04:05 MST",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, &time.ParseError{}
}
