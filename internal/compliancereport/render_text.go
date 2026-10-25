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
	"fmt"
	"strings"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	textWidth     = 78
	indent        = "  "
	statusWidth   = 11
	idWidth       = 21
	evidenceStart = len(indent) + statusWidth + 1 + idWidth + 2

	ansiRed    = "31"
	ansiYellow = "33"
	ansiBold   = "1"
)

// textWriter builds the text report.
type textWriter struct {
	b     strings.Builder
	color bool
}

func (t *textWriter) line(format string, args ...any) {
	fmt.Fprintf(&t.b, format, args...)
	t.b.WriteByte('\n')
}

func (t *textWriter) paragraph(prefix, text string) {
	for _, l := range wrap(text, textWidth-len(prefix)) {
		t.line("%s%s", prefix, l)
	}
}

func (t *textWriter) heading(title string) {
	t.line("")
	t.line("%s", t.paint(title, ansiBold))
}

func (t *textWriter) paint(s, code string) string {
	if !t.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func renderText(r Report, color bool) string {
	t := &textWriter{color: color}
	t.line("PostgreSQL compliance report")
	t.line("")
	t.line("%sCluster  %s/%s", indent, r.Cluster.Namespace, r.Cluster.Name)
	t.line("%sPreset   %s", indent, r.Standard)
	if r.CheckedAt != nil {
		t.line("%sChecked  %s, %s before it was read", indent, formatTime(*r.CheckedAt), age(*r.AgeSeconds))
	} else {
		t.line("%sChecked  never", indent)
	}
	t.line("%sRead     %s", indent, formatTime(r.ReadAt))
	t.line("%sSource   %s", indent, r.Source)
	if notice := freshnessNotice(r); notice != "" {
		t.line("")
		t.paragraph(indent, notice)
	}

	t.heading("READ THIS FIRST")
	for i, s := range r.ReadThisFirst {
		if i > 0 {
			t.line("")
		}
		t.paragraph(indent, s)
	}

	t.heading("THE OPERATOR'S OWN CHECKS")
	t.line("%s%s", indent, summaryLine(r.Summary))
	if len(r.Controls) == 0 {
		t.line("%sThe operator has not recorded any controls for this cluster.", indent)
	}
	t.line("")
	for _, c := range r.Controls {
		t.control(c)
	}

	t.heading("NOT CHECKED BY THE OPERATOR")
	t.paragraph(indent, r.NotChecked.Intro)
	for _, item := range r.NotChecked.Items {
		t.paragraph(indent+"- ", item)
	}
	t.paragraph(indent, "The full lists are in "+r.NotChecked.Pointer+".")

	t.emergency(r.EmergencyAccess)
	return t.b.String()
}

func (t *textWriter) control(c Control) {
	label := statusLabel(c.Status)
	padded := fmt.Sprintf("%-*s", statusWidth, label)
	switch c.Status {
	case dbv1alpha1.ControlNotMet:
		padded = t.paint(padded, ansiRed)
	case StatusOutOfDate:
		padded = t.paint(padded, ansiYellow)
	}
	lines := wrap(controlEvidence(c), textWidth-evidenceStart)
	first := ""
	if len(lines) > 0 {
		first, lines = lines[0], lines[1:]
	}
	t.line("%s%s %-*s  %s", indent, padded, idWidth, c.ID, first)
	for _, l := range lines {
		t.line("%s%s", strings.Repeat(" ", evidenceStart), l)
	}
}

func (t *textWriter) emergency(e EmergencyAccess) {
	t.heading("EMERGENCY ACCESS")
	if e.Warning == nil {
		t.paragraph(indent, "Emergency access is off for this cluster, and no requests exist.")
		return
	}
	t.paragraph(indent, t.paint(e.Warning.Title, ansiBold))
	for _, s := range e.Warning.Statements {
		t.line("")
		t.paragraph(indent, s)
	}
	t.line("")
	t.paragraph(indent, e.Warning.Record)
	t.line("")
	if !e.Enabled {
		t.paragraph(indent, "The cluster does not allow new requests now.")
		t.line("")
	}
	if len(e.Requests) == 0 {
		t.paragraph(indent, "No requests exist.")
		return
	}
	counts := make([]string, 0, len(e.Counts))
	for _, c := range e.Counts {
		counts = append(counts, fmt.Sprintf("%d %s", c.Count, c.State))
	}
	t.paragraph(indent, plural(len(e.Requests), "request")+": "+strings.Join(counts, ", "))
	for _, q := range e.Requests {
		t.line("")
		t.line("%s%s  %s  %s  %s", indent, q.Name, q.State, orNobody(q.RequestedBy), q.Role)
		if times := requestTimes(q); times != "" {
			t.line("%s%s", indent+indent, times)
		}
		t.paragraph(indent+indent, "\""+q.Reason+"\"")
		if q.Message != "" {
			t.paragraph(indent+indent, q.Message)
		}
	}
}

func orNobody(who string) string {
	if who == "" {
		return "(nobody recorded)"
	}
	return who
}
