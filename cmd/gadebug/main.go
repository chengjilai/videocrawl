package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"videocrawl/internal/enum"
	"videocrawl/internal/sites"
)

func main() {
	cfg := sites.Site{Proxy: "http://127.0.0.1:8888", Dial: "proxy"}
	cl := enum.NewGallica(cfg)
	// wrap transport with status logging
	orig := cl.HTTP.Transport
	cl.HTTP.Transport = &loggingRT{next: orig}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	err := cl.Download(ctx, "bpt6k392975d", "/tmp/gadebug/out.pdf")
	fmt.Println("download err:", err)
}

type loggingRT struct{ next http.RoundTripper }

func (l *loggingRT) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := l.next.RoundTrip(r)
	if err != nil {
		fmt.Printf("REQ %s %s -> ERR %v\n", r.Method, r.URL, err)
		return resp, err
	}
	fmt.Printf("REQ %s %s -> %d | Cookie: %q\n", r.Method, r.URL, resp.StatusCode, r.Header.Get("Cookie"))
	if sc := resp.Header.Values("Set-Cookie"); len(sc) > 0 {
		for _, c := range sc {
			fmt.Printf("    Set-Cookie: %s\n", c)
		}
	}
	return resp, nil
}
