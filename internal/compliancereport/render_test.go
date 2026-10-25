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
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// The fixtures are real status, captured from a three-instance hipaa cluster on
// Kind in four states. The time each is read at is fixed here.
var fixtures = []struct {
	name string
	at   time.Time
}{
	{"fresh", time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC)},
	{"all-met", time.Date(2026, 10, 25, 3, 45, 0, 0, time.UTC)},
	{"not-met", time.Date(2026, 10, 25, 3, 42, 40, 0, time.UTC)},
	{"operator-down", time.Date(2026, 10, 25, 7, 0, 0, 0, time.UTC)},
}

var formats = map[Format]string{FormatText: "txt", FormatMarkdown: "md", FormatJSON: "json"}

func fixtureReport(t *testing.T, name string, at time.Time) Report {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	objs, err := ParseObjects(data)
	if err != nil {
		t.Fatal(err)
	}
	in, err := objs.Select("", "", "file "+name+".json")
	if err != nil {
		t.Fatal(err)
	}
	return mustBuild(t, in, at)
}

func render(t *testing.T, r Report, f Format) string {
	t.Helper()
	var b bytes.Buffer
	if err := Render(&b, r, Options{Format: f}); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestGoldenReports(t *testing.T) {
	for _, fx := range fixtures {
		r := fixtureReport(t, fx.name, fx.at)
		for f, ext := range formats {
			t.Run(fx.name+"/"+string(f), func(t *testing.T) {
				got := render(t, r, f)
				path := filepath.Join("testdata", "golden", fx.name+"."+ext)
				if *update {
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("%v (run go test ./internal/compliancereport -update to write it)", err)
				}
				if got != string(want) {
					t.Errorf("%s differs from %s; run with -update if the change is meant", fx.name, path)
				}
			})
		}
	}
}

// oneLine puts text on one line, so a sentence the text format wrapped can be
// found.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// Whatever the cluster's state and the format, the report carries the three
// statements, and the emergency access wording whenever it applies.
func TestEveryReportInEveryFormatCarriesTheStatements(t *testing.T) {
	always := []string{PresetIsNotCompliance, MappingUnreviewed, ControlsMetMeaning, NotAProofForAuditors, NotExhaustive}
	emergency := []string{EmergencyWarningTitle, EmergencyNotInPgaudit, EmergencyClearText, EmergencyWhateverTheSettings, EmergencyAuditors}
	for _, fx := range fixtures {
		r := fixtureReport(t, fx.name, fx.at)
		for f := range formats {
			t.Run(fx.name+"/"+string(f), func(t *testing.T) {
				out := oneLine(render(t, r, f))
				if f == FormatJSON {
					// JSON escapes nothing in these sentences, apart from quotes.
					out = strings.ReplaceAll(out, `\"`, `"`)
				}
				want := always
				if r.EmergencyAccess.Warning != nil {
					want = append(slices.Clone(always), emergency...)
				}
				for _, s := range want {
					if !strings.Contains(out, s) {
						t.Errorf("the report lacks: %s", s)
					}
				}
				for _, item := range NotChecked {
					if !strings.Contains(out, item) {
						t.Errorf("the report lacks the not-checked item: %s", item)
					}
				}
			})
		}
	}
}

// The report never says the cluster is compliant. Apart from the fixed
// sentences, which say it is not, and the text the operator and the requesters
// wrote, none of these words appears.
func TestTheReportNeverClaimsCompliance(t *testing.T) {
	forbidden := []string{"compliant", "certified", "certification", "audit-ready", "approved", "pass", "fail"}
	for _, fx := range fixtures {
		r := fixtureReport(t, fx.name, fx.at)
		for f := range formats {
			t.Run(fx.name+"/"+string(f), func(t *testing.T) {
				out := oneLine(render(t, r, f))
				if f == FormatJSON {
					out = strings.ReplaceAll(out, `\"`, `"`)
				}
				fixed := []string{
					PresetIsNotCompliance, ControlsMetMeaning, NotAProofForAuditors, MappingUnreviewed,
					EmergencyWarningTitle, EmergencyClearText, EmergencyWhateverTheSettings, EmergencyNotInPgaudit,
				}
				quoted := []string{}
				for _, c := range r.Controls {
					quoted = append(quoted, c.Evidence)
				}
				for _, q := range r.EmergencyAccess.Requests {
					quoted = append(quoted, q.Reason, q.Message)
				}
				for _, s := range append(fixed, quoted...) {
					if s != "" {
						out = strings.ReplaceAll(out, oneLine(s), "")
					}
				}
				for _, word := range forbidden {
					if strings.Contains(strings.ToLower(out), word) {
						t.Errorf("the report uses %q outside the fixed wording and the quoted text:\n%s", word, out)
					}
				}
			})
		}
	}
}

func TestJSONHasASchemaVersionAndTheWording(t *testing.T) {
	r := fixtureReport(t, "not-met", fixtures[2].at)
	var back map[string]any
	if err := json.Unmarshal([]byte(render(t, r, FormatJSON)), &back); err != nil {
		t.Fatal(err)
	}
	if back["schemaVersion"] != float64(SchemaVersion) {
		t.Errorf("schemaVersion %v", back["schemaVersion"])
	}
	for _, key := range []string{"readThisFirst", "notChecked", "emergencyAccess", "freshness", "controls"} {
		if _, ok := back[key]; !ok {
			t.Errorf("the JSON has no %q", key)
		}
	}
}

func TestAgeIsSaidInTheLargestUnitThatFits(t *testing.T) {
	cases := map[int64]string{
		0: "0 seconds", 1: "1 second", 59: "59 seconds", 60: "1 minute", 119: "1 minute", 120: "2 minutes",
		3600: "1 hour", 3660: "1 hour 1 minute", 7200: "2 hours", 47 * 3600: "47 hours", 48 * 3600: "2 days",
	}
	for secs, want := range cases {
		if got := age(secs); got != want {
			t.Errorf("age(%d) = %q, want %q", secs, got, want)
		}
	}
}

func TestTextFitsTheLine(t *testing.T) {
	for _, fx := range fixtures {
		out := render(t, fixtureReport(t, fx.name, fx.at), FormatText)
		for line := range strings.SplitSeq(out, "\n") {
			// The source line holds a path, which does not wrap.
			if len(line) > textWidth && !strings.Contains(line, "Source") {
				t.Errorf("%s: %d characters: %s", fx.name, len(line), line)
			}
		}
	}
}

func TestColourTouchesOnlyTheResultsThatNeedAttention(t *testing.T) {
	r := fixtureReport(t, "not-met", fixtures[2].at)
	var plain, painted bytes.Buffer
	_ = Render(&plain, r, Options{Format: FormatText})
	_ = Render(&painted, r, Options{Format: FormatText, Color: true})
	if strings.Contains(plain.String(), "\x1b[") {
		t.Error("colour was used without being asked for")
	}
	if !strings.Contains(painted.String(), "\x1b[31mNotMet") {
		t.Error("a control that is not met is not painted")
	}
	stripped := stripANSI(painted.String())
	if stripped != plain.String() {
		t.Error("colour changed more than the colour")
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func TestAMarkdownTableCellCannotBreakTheTable(t *testing.T) {
	if got, want := cell("one | two\nthree"), `one \| two three`; got != want {
		t.Errorf("cell = %q, want %q", got, want)
	}
	r := fixtureReport(t, "not-met", fixtures[2].at)
	r.EmergencyAccess.Requests[0].Reason = "restart a | b\nthen check"
	out := render(t, r, FormatMarkdown)
	if !strings.Contains(out, `restart a \| b then check`) {
		t.Errorf("the reason broke out of its cell:\n%s", out)
	}
}
