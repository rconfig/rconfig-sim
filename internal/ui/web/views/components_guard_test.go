package views

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestScreensUseComponents enforces section 9.6: no screen styles its own
// buttons, cards or inputs. Every .templ outside components/ must use the
// component set for them. Hidden inputs carry form state and are allowed.
func TestScreensUseComponents(t *testing.T) {
	files, err := filepath.Glob("*.templ")
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates: %v", err)
	}
	own := []*regexp.Regexp{
		regexp.MustCompile(`<button\b`),
		regexp.MustCompile(`<select\b`),
		regexp.MustCompile(`<textarea\b`),
		regexp.MustCompile(`<input\b[^>]*>`),
		regexp.MustCompile(`class="[^"]*\brounded-card\b`),
		regexp.MustCompile(`class="[^"]*\bbg-accent-600\b`),
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			for _, re := range own {
				m := re.FindString(line)
				if m == "" || strings.Contains(m, `type="hidden"`) {
					continue
				}
				t.Errorf("%s:%d styles its own element (%s); use internal/ui/web/views/components", f, i+1, strings.TrimSpace(m))
			}
		}
	}
}
