/*
Copyright 2026.

Licensed under the Business Source License 1.1 (the "License"). You may not
use this file except in compliance with the License, a copy of which is in the
LICENSE file at the root of this repository. Production use requires a
commercial license from the Licensor. On the Change Date (2030-10-17) the
work becomes available under the Change License (Apache-2.0).
*/

package compliancereport

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Format is how a report is written.
type Format string

// The formats a report can be written in.
const (
	FormatText     Format = "text"
	FormatMarkdown Format = "markdown"
	FormatJSON     Format = "json"
)

// Options say how to write a report.
type Options struct {
	Format Format
	// Color paints the results that need attention, in the text format only.
	Color bool
}

// ParseFormat reads a format name.
func ParseFormat(s string) (Format, error) {
	switch f := Format(s); f {
	case FormatText, FormatMarkdown, FormatJSON:
		return f, nil
	}
	return "", fmt.Errorf("unknown format %q: use text, markdown or json", s)
}

// Render writes the report.
func Render(w io.Writer, r Report, opts Options) error {
	switch opts.Format {
	case FormatText, "":
		_, err := io.WriteString(w, renderText(r, opts.Color))
		return err
	case FormatMarkdown:
		_, err := io.WriteString(w, renderMarkdown(r))
		return err
	case FormatJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	return fmt.Errorf("unknown format %q", opts.Format)
}

const timeLayout = "2006-01-02 15:04:05 UTC"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func formatTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return formatTime(*t)
}

// age says how long, to the minute above a minute and to the second below.
func age(seconds int64) string {
	d := time.Duration(seconds) * time.Second
	switch {
	case d < time.Minute:
		return plural(int(d/time.Second), "second")
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute")
	case d < 48*time.Hour:
		h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
		if m == 0 {
			return plural(h, "hour")
		}
		return plural(h, "hour") + " " + plural(m, "minute")
	}
	return plural(int(d/(24*time.Hour)), "day")
}

func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// wrap breaks text into lines of at most width characters. A word longer than
// the width gets a line to itself.
func wrap(text string, width int) []string {
	var lines []string
	line := ""
	for word := range strings.FieldsSeq(text) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) <= width:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// freshnessNotice is the words under the header when the report is not current.
// It returns nothing for a current report.
func freshnessNotice(r Report) string {
	switch r.Freshness {
	case OutOfDate:
		return "OUT OF DATE. The operator's checks run every ten minutes, and nothing newer than this has been recorded. " +
			"The operator may not be running, or its checks may have stopped. Every result below is the last one recorded, " +
			"and is not current. " + ChecksStop
	case NeverChecked:
		return "NOT CHECKED YET. The operator's jobs have not reported for this cluster, so every result that rests on them is Unknown. " +
			"A result the operator reads from its own status may already be shown."
	}
	return ""
}

func summaryLine(s Summary) string {
	return fmt.Sprintf("%d met, %d not met, %d unknown, %d out of date", s.Met, s.NotMet, s.Unknown, s.OutOfDate)
}

func statusLabel(status string) string {
	switch status {
	case StatusOutOfDate:
		return "Out of date"
	}
	return status
}

// controlEvidence is the evidence column. For a result that is out of date it
// begins with the result that was recorded.
func controlEvidence(c Control) string {
	if c.Status == StatusOutOfDate {
		return "Recorded as " + c.Recorded + ". " + c.Evidence
	}
	return c.Evidence
}

// requestTimes says when the access began and ends or ended.
func requestTimes(q Request) string {
	var parts []string
	if q.StartedAt != nil {
		parts = append(parts, "started "+formatTimePtr(q.StartedAt))
	}
	switch {
	case q.EndedAt != nil:
		parts = append(parts, "ended "+formatTimePtr(q.EndedAt))
	case q.ExpiresAt != nil:
		parts = append(parts, "ends "+formatTimePtr(q.ExpiresAt))
	}
	return strings.Join(parts, ", ")
}
