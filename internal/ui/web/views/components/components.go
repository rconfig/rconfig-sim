// Package components is the rcfg-sim-ui component set (docs/UI-ARCHITECTURE.md
// section 9.6): buttons, cards, form fields, choice cards, segmented
// controls, the stepper, count rows, badges, summary sentences, inline
// help, Advanced disclosures, empty states and toasts. Every screen builds
// on these; no screen styles its own buttons, cards or inputs. The classes
// they use are defined once in static/src/app.css.
package components

import (
	"strconv"
	"strings"

	"github.com/a-h/templ"
)

// Button kinds (9.6).
const (
	Primary   = "primary"   // copper fill, one per view
	Secondary = "secondary" // outline
	Danger    = "danger"    // rose; always asks to confirm
	Ghost     = "ghost"     // icon or text only
	// Contrast is the dark button on the copper pending-changes bar, where
	// a copper primary would not stand out.
	Contrast = "contrast"
)

// Size is a button size: "" (normal) or "sm".
type Size string

const Small Size = "sm"

func buttonClass(kind string, size Size) string {
	c := "btn btn-" + kind
	if kind == "" {
		c = "btn btn-" + Secondary
	}
	if size == Small {
		c += " btn-sm"
	}
	return c
}

// Badge kinds.
const (
	BadgeFamily   = "family"   // IP, Optical, Topology
	BadgeProtocol = "protocol" // CLI, TL1
	BadgeWarn     = "warn"     // restart needed
	BadgeMuted    = "muted"    // read-only
	BadgeError    = "error"    // doesn't fit this host
	BadgeOK       = "ok"       // active, fits
	BadgeInfo     = "info"
)

// BadgeData is one badge: its kind and text.
type BadgeData struct {
	Kind string
	Text string
}

// Option is one choice in a segmented control or select.
type Option struct {
	Value string
	Label string
	Help  string // optional one-line description (segmented control only)
}

// Step is one step of a guided flow. A completed step is clickable: Href
// makes it a link; Goto makes it a submit button (name "goto", value
// Goto) for flows that keep their state in the form.
type Step struct {
	Label string
	Href  string
	Goto  string
}

// Choice is one choice card.
type Choice struct {
	Value       string
	Title       string
	Description string
	Icon        string // a short glyph shown in the card's corner
	Badges      []BadgeData
	Disabled    string // why it cannot be chosen; "" when it can
}

// Count is one count row: a model and how many devices of it.
type Count struct {
	Name        string // form field name
	Model       string
	Description string
	Protocol    string // badge: CLI or TL1
	AvgSize     string // e.g. "25 KB"
	Value       int
	Share       float64 // 0..100, for the share bar
}

// Toast kinds.
const (
	ToastOK    = "ok"
	ToastWarn  = "warn"
	ToastError = "error"
)

func itoa(n int) string { return strconv.Itoa(n) }

func pct(f float64) string {
	return strconv.FormatFloat(min(max(f, 0), 100), 'f', 1, 64) + "%"
}

func fmtFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func stepState(i, current int) string {
	switch {
	case i < current:
		return "done"
	case i == current:
		return "current"
	}
	return "todo"
}

func joinIDs(parts ...string) string { return strings.Join(parts, "-") }

func toastRole(kind string) string {
	if kind == ToastError {
		return "alert"
	}
	return "status"
}

// split takes "class" out of attrs, so a caller's extra classes join the
// component's own instead of making a second class attribute.
func split(attrs templ.Attributes) (string, templ.Attributes) {
	if attrs == nil {
		return "", nil
	}
	c, ok := attrs["class"].(string)
	if !ok {
		return "", attrs
	}
	rest := make(templ.Attributes, len(attrs)-1)
	for k, v := range attrs {
		if k != "class" {
			rest[k] = v
		}
	}
	return c, rest
}

func cls(base string, attrs templ.Attributes) string {
	extra, _ := split(attrs)
	if extra == "" {
		return base
	}
	return base + " " + extra
}

func rest(attrs templ.Attributes) templ.Attributes {
	_, r := split(attrs)
	return r
}
