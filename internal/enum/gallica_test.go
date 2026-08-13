package enum

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func respWith(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestCapturePDF(t *testing.T) {
	dir := t.TempDir()

	t.Run("pdf body captured", func(t *testing.T) {
		dest := filepath.Join(dir, "a.pdf")
		got, err := capturePDF(respWith("%PDF-1.4\n%binary-data-here"), dest)
		if err != nil {
			t.Fatalf("capturePDF: %v", err)
		}
		if !got {
			t.Fatal("expected got=true for a PDF verify body")
		}
		b, err := os.ReadFile(dest)
		if err != nil {
			t.Fatalf("dest missing: %v", err)
		}
		if string(b) != "%PDF-1.4\n%binary-data-here" {
			t.Fatalf("dest content = %q", b)
		}
		if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
			t.Fatal(".part should be renamed away")
		}
	})

	t.Run("html body not captured", func(t *testing.T) {
		dest := filepath.Join(dir, "b.pdf")
		got, err := capturePDF(respWith("<!DOCTYPE html><html>challenge</html>"), dest)
		if err != nil {
			t.Fatalf("capturePDF: %v", err)
		}
		if got {
			t.Fatal("expected got=false for an HTML verify body")
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Fatal("dest must not exist for a non-PDF body")
		}
	})

	t.Run("empty dest consumes body", func(t *testing.T) {
		got, err := capturePDF(respWith("%PDF-1.4 whatever"), "")
		if err != nil {
			t.Fatalf("capturePDF: %v", err)
		}
		if got {
			t.Fatal("expected got=false with empty dest")
		}
	})

	t.Run("short body not captured", func(t *testing.T) {
		dest := filepath.Join(dir, "c.pdf")
		got, err := capturePDF(respWith("%PD"), dest)
		if err != nil {
			t.Fatalf("capturePDF: %v", err)
		}
		if got {
			t.Fatal("expected got=false for a truncated body")
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Fatal("dest must not exist for a truncated body")
		}
	})
}
