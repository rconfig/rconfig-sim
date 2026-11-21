package inventory

import (
	"cmp"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Query is the Devices page's filters, sort and page, as URL parameters so
// a view can be bookmarked and an export uses the same rows.
type Query struct {
	Status   string // active, inactive or unknown
	Reason   string
	Instance string
	Family   string // catalogue family id
	Model    string
	Profile  string // a profile kind (hang, latency, refuse, disconnect), "none", or "" for any
	Outcome  string // last outcome (device state name)
	Q        string // free text on hostname or IP
	Sort     string // hostname (default), ip, port, model, status, outcome, last_seen
	Desc     bool
	Page     int // from 1
	PerPage  int
}

// PerPageDefault is the table's page size.
const PerPageDefault = 100

// ParseQuery reads a Query from URL parameters. Unknown values simply
// match nothing.
func ParseQuery(v url.Values) Query {
	q := Query{Status: v.Get("status"), Reason: v.Get("reason"), Instance: v.Get("instance"), Family: v.Get("family"), Model: v.Get("model"),
		Profile: v.Get("profile"), Outcome: v.Get("outcome"), Q: strings.TrimSpace(v.Get("q")),
		Sort: v.Get("sort"), Desc: v.Get("desc") == "1", PerPage: PerPageDefault}
	q.Page, _ = strconv.Atoi(v.Get("page"))
	if q.Page < 1 {
		q.Page = 1
	}
	return q
}

// Values is the query as URL parameters, leaving out defaults.
func (q Query) Values() url.Values {
	v := url.Values{}
	for k, s := range map[string]string{"status": q.Status, "reason": q.Reason, "instance": q.Instance, "family": q.Family,
		"model": q.Model, "profile": q.Profile, "outcome": q.Outcome, "q": q.Q, "sort": q.Sort} {
		if s != "" {
			v.Set(k, s)
		}
	}
	if q.Desc {
		v.Set("desc", "1")
	}
	if q.Page > 1 {
		v.Set("page", strconv.Itoa(q.Page))
	}
	return v
}

// Filters is the query without sort and page: what an export or a summary
// count needs.
func (q Query) Filters() Query {
	return Query{Status: q.Status, Reason: q.Reason, Instance: q.Instance, Family: q.Family, Model: q.Model, Profile: q.Profile,
		Outcome: q.Outcome, Q: q.Q}
}

func (q Query) match(r *Row) bool {
	switch {
	case q.Status != "" && r.Status != q.Status,
		q.Reason != "" && r.Reason != q.Reason,
		q.Instance != "" && r.Instance != q.Instance,
		q.Family != "" && r.Family != q.Family,
		q.Model != "" && r.Model != q.Model,
		q.Outcome != "" && r.LastOutcome != q.Outcome:
		return false
	}
	if q.Profile != "" {
		kind, _, _ := strings.Cut(r.Profile, ":")
		if q.Profile == "none" && r.Profile != "" || q.Profile != "none" && kind != q.Profile {
			return false
		}
	}
	if q.Q != "" {
		s := strings.ToLower(q.Q)
		if !strings.Contains(strings.ToLower(r.Hostname), s) && !strings.HasPrefix(r.IP, s) {
			return false
		}
	}
	return true
}

// Filter returns the rows q's filters match, sorted by q's sort, all pages.
func Filter(rows []Row, q Query) []Row {
	out := make([]Row, 0, len(rows))
	for i := range rows {
		if q.match(&rows[i]) {
			out = append(out, rows[i])
		}
	}
	sortRows(out, q.Sort, q.Desc)
	return out
}

func ipPort(a, b *Row) int {
	ia, ea := netip.ParseAddr(a.IP)
	ib, eb := netip.ParseAddr(b.IP)
	if ea == nil && eb == nil {
		if c := ia.Compare(ib); c != 0 {
			return c
		}
	} else if c := strings.Compare(a.IP, b.IP); c != 0 {
		return c
	}
	return cmp.Compare(a.Port, b.Port)
}

func sortRows(rows []Row, by string, desc bool) {
	var f func(a, b *Row) int
	switch by {
	case "ip", "port":
		f = ipPort
	case "model":
		f = func(a, b *Row) int { return cmp.Or(strings.Compare(a.Model, b.Model), ipPort(a, b)) }
	case "status":
		f = func(a, b *Row) int {
			return cmp.Or(strings.Compare(a.Status, b.Status), strings.Compare(a.Reason, b.Reason), ipPort(a, b))
		}
	case "outcome":
		f = func(a, b *Row) int { return cmp.Or(strings.Compare(a.LastOutcome, b.LastOutcome), ipPort(a, b)) }
	case "last_seen":
		f = func(a, b *Row) int { return cmp.Or(a.LastSeen.Compare(b.LastSeen), ipPort(a, b)) }
	default:
		f = func(a, b *Row) int { return strings.Compare(a.Hostname, b.Hostname) }
	}
	slices.SortStableFunc(rows, func(a, b Row) int {
		if desc {
			return f(&b, &a)
		}
		return f(&a, &b)
	})
}

// Page returns q's page of rows and the number of pages.
func Page(rows []Row, q Query) ([]Row, int) {
	per := q.PerPage
	if per <= 0 {
		per = PerPageDefault
	}
	pages := max(1, (len(rows)+per-1)/per)
	p := min(max(q.Page, 1), pages)
	lo := (p - 1) * per
	return rows[lo:min(lo+per, len(rows))], pages
}

// Summary is the counts above the table: each is a one-click filter.
type Summary struct {
	Total    int
	Active   int
	Unknown  int
	Inactive map[string]int // by reason
	Families map[string]int
	Models   map[string]int
}

// Summarise counts rows.
func Summarise(rows []Row) Summary {
	s := Summary{Total: len(rows), Inactive: map[string]int{}, Families: map[string]int{}, Models: map[string]int{}}
	for i := range rows {
		r := &rows[i]
		switch r.Status {
		case Active:
			s.Active++
		case Inactive:
			s.Inactive[r.Reason]++
		default:
			s.Unknown++
		}
		s.Models[r.Model]++
		if r.Family != "" {
			s.Families[r.Family]++
		}
	}
	return s
}
