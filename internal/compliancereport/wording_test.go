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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// plainDoc is a docs page as running text: no Markdown marks, no quote or
// heading markers, one space between words.
func plainDoc(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", name))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimLeft(line, "> #")
		line = strings.NewReplacer("*", "", "`", "").Replace(line)
		lines = append(lines, line)
	}
	return strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
}

func TestFixedWordingIsInTheDocs(t *testing.T) {
	compliance := plainDoc(t, "compliance.md")
	emergency := plainDoc(t, "emergency-access.md")

	inCompliance := map[string]string{
		"PresetIsNotCompliance": PresetIsNotCompliance,
		"MappingUnreviewed":     MappingUnreviewed,
		"ControlsMetMeaning":    ControlsMetMeaning,
		"NotAProofForAuditors":  NotAProofForAuditors,
		"NotExhaustive":         NotExhaustive,
		"ChecksStop":            ChecksStop,
	}
	for name, sentence := range inCompliance {
		if !strings.Contains(compliance, sentence) {
			t.Errorf("%s is not in docs/compliance.md word for word:\n  %s", name, sentence)
		}
	}

	inEmergency := map[string]string{
		"EmergencyWarningTitle":        EmergencyWarningTitle,
		"EmergencyNotInPgaudit":        EmergencyNotInPgaudit,
		"EmergencyClearText":           EmergencyClearText,
		"EmergencyWhateverTheSettings": EmergencyWhateverTheSettings,
		"EmergencyAuditors":            EmergencyAuditors,
	}
	for name, sentence := range inEmergency {
		if !strings.Contains(emergency, sentence) {
			t.Errorf("%s is not in docs/emergency-access.md word for word:\n  %s", name, sentence)
		}
	}
}

// The page that documents the command says the same things, in the same words.
func TestTheReportsOwnPageSaysTheSameThings(t *testing.T) {
	page := plainDoc(t, "compliance-report.md")
	for _, sentence := range []string{PresetIsNotCompliance, MappingUnreviewed, ControlsMetMeaning, NotAProofForAuditors} {
		if !strings.Contains(page, sentence) {
			t.Errorf("docs/compliance-report.md lacks:\n  %s", sentence)
		}
	}
}

func TestNotCheckedFollowsTheDocsInOrder(t *testing.T) {
	compliance := plainDoc(t, "compliance.md")
	// The heading's marks are stripped, so look for its words.
	_, section, found := strings.Cut(compliance, "What you remain responsible for Anything that is not in the table above is not checked")
	if !found {
		t.Fatal("docs/compliance.md has no section on what the user remains responsible for")
	}
	at := 0
	for _, item := range NotChecked {
		i := strings.Index(section[at:], item)
		if i < 0 {
			t.Fatalf("%q is not in the docs' list of what the user remains responsible for, or is out of order", item)
		}
		at += i + len(item)
	}
}

// The docs and the report both say 35 minutes. The report's threshold is the
// operator's, from one constant.
func TestTheThresholdTheWordingNamesIsTheOperatorsOwn(t *testing.T) {
	if dbv1alpha1.ComplianceStaleAfter != 35*time.Minute {
		t.Fatalf("ComplianceStaleAfter is %s; ChecksStop and docs/compliance.md say 35 minutes", dbv1alpha1.ComplianceStaleAfter)
	}
	if !strings.Contains(ChecksStop, "35 minutes") {
		t.Fatal("ChecksStop does not name the threshold")
	}
}
