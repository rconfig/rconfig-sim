package components

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func render(t *testing.T, c templ.Component) string {
	t.Helper()
	var b strings.Builder
	if err := c.Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func with(c templ.Component, child string) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		return c.Render(templ.WithChildren(ctx, templ.Raw(child)), w)
	})
}

func has(t *testing.T, name, html string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(html, w) {
			t.Errorf("%s: missing %q in\n%s", name, w, html)
		}
	}
}

func TestComponents(t *testing.T) {
	for _, c := range []struct {
		name  string
		c     templ.Component
		wants []string
	}{
		{"primary button", with(Button(Primary, "", templ.Attributes{"hx-post": "/x"}), "Go"),
			[]string{`class="btn btn-primary"`, `hx-post="/x"`, `hx-disabled-elt="this"`, "Go", "btn-spinner htmx-indicator"}},
		{"small ghost", with(Button(Ghost, Small, nil), "x"), []string{`class="btn btn-ghost btn-sm"`}},
		{"submit", with(Submit(Primary, nil), "Save"), []string{`type="submit"`, "btn-primary"}},
		{"danger asks", with(ConfirmButton("Stop everything?", "", templ.Attributes{"hx-post": "/stop"}), "Stop"),
			[]string{"btn-danger", `hx-confirm="Stop everything?"`}},
		{"disabled explains", with(DisabledButton(Primary, "", "a job is running"), "Build"),
			[]string{"disabled", `title="a job is running"`}},
		{"link button", with(LinkButton(Secondary, Small, "/a", nil), "A"), []string{`href="/a"`, "btn-secondary", "btn-sm"}},
		{"card", with(Card("Title", "Help line."), "<p>body</p>"), []string{`class="card"`, "card-title", "Title", "Help line.", "<p>body</p>"}},
		{"field with help", with(Field("Delay", "In milliseconds."), "<input>"), []string{`class="field"`, "Delay", `<details class="help">`, "In milliseconds."}},
		{"input", Input(templ.Attributes{"name": "q"}), []string{`class="input"`, `name="q"`}},
		{"number", NumberInput(templ.Attributes{"name": "n"}), []string{`type="number"`, "input num"}},
		{"select", Select("s", "b", []Option{{"a", "A", ""}, {"b", "B", ""}}, nil), []string{`name="s"`, `value="b" selected`}},
		{"checkbox", Checkbox("Tick", true, templ.Attributes{"name": "c"}), []string{`type="checkbox"`, "checked", "Tick"}},
		{"slider", Slider("d-min", "delay_min", "ms", "0", "100", "1", "5", nil, templ.Attributes{"data-default-max": "10"}),
			[]string{`type="range"`, `data-sync="d-min"`, `id="d-min"`, `name="delay_min"`, "ms", `data-default-max="10"`}},
		{"choice cards", ChoiceCards("kind", "ip", "What", []Choice{
			{Value: "ip", Title: "Enterprise IP", Description: "Cisco", Badges: []BadgeData{{BadgeFamily, "IP"}}},
			{Value: "topo", Title: "Topology", Disabled: "coming in Phase 9"}}, nil),
			[]string{`type="radio"`, `value="ip" checked`, "Enterprise IP", "badge-family", "choice-disabled", "disabled", "coming in Phase 9"}},
		{"segmented", Segmented("ssh_auth", "driver", "SSH login", []Option{{"driver", "Inside TL1", "Like a 6500."}, {"password", "Password first", ""}}, nil),
			[]string{"segmented", `value="driver" checked`, "Like a 6500.", "SSH login"}},
		{"stepper", Stepper([]Step{{"Kind", "/k", ""}, {"Devices", "/d", ""}, {"Review", "", ""}}, 1),
			[]string{"step-done", `href="/k"`, "step-current", `aria-current`, "step-todo"}},
		{"stepper submit", Stepper([]Step{{"Kind", "", "0"}, {"Devices", "", "1"}}, 1),
			[]string{`<button type="submit" name="goto" value="0" class="step-link">Kind</button>`}},
		{"count row", CountRow(Count{Name: "count_sm", Model: "sm", Protocol: "CLI", AvgSize: "25 KB", Value: 6000, Share: 60}),
			[]string{"data-count-row", `name="count_sm"`, `value="6000"`, `data-step="1"`, `data-step="-1"`, "width:60.0%", "25 KB", "CLI"}},
		{"badge", Badge(BadgeWarn, "restart needed"), []string{"badge badge-warn", "restart needed"}},
		{"actions", with(Actions("Builds 9 devices."), "<button>"), []string{`class="summary"`, "Builds 9 devices.", "actions-buttons"}},
		{"advanced", with(Advanced(2), "<input>"), []string{`<details class="advanced">`, "Advanced", "(2 changed)"}},
		{"advanced none changed", with(Advanced(0), ""), []string{"Advanced"}},
		{"empty", with(Empty("No runs yet", "Start a testbench."), "<a>go</a>"), []string{"empty-title", "No runs yet", "Start a testbench.", "<a>go</a>"}},
		{"toast ok", Toast(ToastOK, "Saved.", nil), []string{"toast-ok", `role="status"`, `data-toast="ok"`, "data-dismiss"}},
		{"toast error", Toast(ToastError, "Failed.", []string{"why"}), []string{"toast-error", `role="alert"`, "why"}},
		{"table", with(Table(nil), "<tr></tr>"), []string{"table-wrap", `<table class="table">`}},
		{"pill link", with(PillLink("ok", "/x", nil), "3"), []string{"pill pill-ok", `href="/x"`}},
		{"code", with(Code(nil), "log"), []string{`class="code num"`, "log"}},
	} {
		has(t, c.name, render(t, c.c), c.wants...)
	}
	if strings.Contains(render(t, with(Advanced(0), "")), "changed") {
		t.Error("Advanced with nothing changed shows a count")
	}
}
