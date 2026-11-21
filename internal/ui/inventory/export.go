package inventory

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"
)

// Columns are the export columns, in order. They are public API: tool
// agnostic, and how another tool imports them is up to that tool.
var Columns = []string{"hostname", "ip", "port", "username", "password", "enable_password", "model", "status", "status_reason"}

// ExportRow is one exported device, the JSON object form of Columns.
type ExportRow struct {
	Hostname       string `json:"hostname"`
	IP             string `json:"ip"`
	Port           int    `json:"port"`
	Username       string `json:"username"`
	Password       string `json:"password"`
	EnablePassword string `json:"enable_password"`
	Model          string `json:"model"`
	Status         string `json:"status"`
	StatusReason   string `json:"status_reason"`
}

func exportRow(r *Row) ExportRow {
	return ExportRow{Hostname: r.Hostname, IP: r.IP, Port: r.Port, Username: r.Username, Password: r.Password,
		EnablePassword: r.EnablePassword, Model: r.Model, Status: r.Status, StatusReason: r.Reason}
}

// flushEvery is how many rows go out between flushes, so a large export
// streams instead of building up.
const flushEvery = 500

// WriteCSV streams rows as CSV: a header row of Columns, then one row per
// device, RFC 4180 quoting, UTF-8. flush, when not nil, is called every few
// hundred rows.
func WriteCSV(w io.Writer, rows []Row, flush func()) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(Columns); err != nil {
		return err
	}
	rec := make([]string, len(Columns))
	for i := range rows {
		e := exportRow(&rows[i])
		rec[0], rec[1], rec[2], rec[3], rec[4] = e.Hostname, e.IP, strconv.Itoa(e.Port), e.Username, e.Password
		rec[5], rec[6], rec[7], rec[8] = e.EnablePassword, e.Model, e.Status, e.StatusReason
		if err := cw.Write(rec); err != nil {
			return err
		}
		if (i+1)%flushEvery == 0 {
			cw.Flush()
			if flush != nil {
				flush()
			}
		}
	}
	cw.Flush()
	return cw.Error()
}

// WriteJSON streams rows as a JSON array of objects with the Columns as
// keys, one object at a time.
func WriteJSON(w io.Writer, rows []Row, flush func()) error {
	bw := bufio.NewWriter(w)
	if _, err := bw.WriteString("["); err != nil {
		return err
	}
	for i := range rows {
		b, err := json.Marshal(exportRow(&rows[i]))
		if err != nil {
			return err
		}
		if i > 0 {
			bw.WriteString(",")
		}
		bw.WriteString("\n  ")
		if _, err := bw.Write(b); err != nil {
			return err
		}
		if (i+1)%flushEvery == 0 {
			if err := bw.Flush(); err != nil {
				return err
			}
			if flush != nil {
				flush()
			}
		}
	}
	if len(rows) > 0 {
		bw.WriteString("\n")
	}
	bw.WriteString("]\n")
	return bw.Flush()
}

// FileName is rcfg-sim-<build or testbench>-<status filter>-<date>.<ext>.
func FileName(scope, status string, at time.Time, ext string) string {
	if status == "" {
		status = "all"
	}
	clean := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
				return r
			}
			return '-'
		}, s)
	}
	return "rcfg-sim-" + clean(scope) + "-" + clean(status) + "-" + at.Format("20060102") + "." + ext
}
