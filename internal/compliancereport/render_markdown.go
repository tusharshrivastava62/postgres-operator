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
)

func renderMarkdown(r Report) string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	p("# PostgreSQL compliance report")
	p("")
	p("| | |")
	p("|---|---|")
	p("| Cluster | `%s/%s` |", r.Cluster.Namespace, r.Cluster.Name)
	p("| Preset | `%s` |", r.Standard)
	if r.CheckedAt != nil {
		p("| Checked | %s, %s before it was read |", formatTime(*r.CheckedAt), age(*r.AgeSeconds))
	} else {
		p("| Checked | never |")
	}
	p("| Read | %s |", formatTime(r.ReadAt))
	p("| Source | %s |", cell(r.Source))
	if notice := freshnessNotice(r); notice != "" {
		p("")
		p("> **%s", strings.Replace(notice, ".", ".**", 1))
	}

	p("")
	p("## Read this first")
	for _, s := range r.ReadThisFirst {
		p("")
		p("> %s", s)
	}

	p("")
	p("## The operator's own checks")
	p("")
	p("%s", summaryLine(r.Summary))
	if len(r.Controls) == 0 {
		p("")
		p("The operator has not recorded any controls for this cluster.")
	} else {
		p("")
		p("| Result | Control | Evidence |")
		p("|---|---|---|")
		for _, c := range r.Controls {
			p("| %s | `%s` | %s |", statusLabel(c.Status), c.ID, cell(controlEvidence(c)))
		}
	}

	p("")
	p("## Not checked by the operator")
	p("")
	p("%s", r.NotChecked.Intro)
	p("")
	for _, item := range r.NotChecked.Items {
		p("- %s", item)
	}
	p("")
	p("The full lists are in %s.", r.NotChecked.Pointer)

	p("")
	p("## Emergency access")
	e := r.EmergencyAccess
	if e.Warning == nil {
		p("")
		p("Emergency access is off for this cluster, and no requests exist.")
		return b.String()
	}
	p("")
	p("> **%s**", e.Warning.Title)
	for _, s := range e.Warning.Statements {
		p(">")
		p("> %s", s)
	}
	p("")
	p("%s", e.Warning.Record)
	if !e.Enabled {
		p("")
		p("The cluster does not allow new requests now.")
	}
	if len(e.Requests) == 0 {
		p("")
		p("No requests exist.")
		return b.String()
	}
	counts := make([]string, 0, len(e.Counts))
	for _, c := range e.Counts {
		counts = append(counts, fmt.Sprintf("%d %s", c.Count, c.State))
	}
	p("")
	p("%s: %s", plural(len(e.Requests), "request"), strings.Join(counts, ", "))
	p("")
	p("| Request | State | Requested by | Role | When | Reason |")
	p("|---|---|---|---|---|---|")
	for _, q := range e.Requests {
		reason := q.Reason
		if q.Message != "" {
			reason += " (" + q.Message + ")"
		}
		p("| `%s` | %s | %s | `%s` | %s | %s |", q.Name, q.State, cell(orNobody(q.RequestedBy)), q.Role, requestTimes(q), cell(reason))
	}
	return b.String()
}

// cell makes text safe to put in a table cell.
func cell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.Join(strings.Fields(s), " ")
}
