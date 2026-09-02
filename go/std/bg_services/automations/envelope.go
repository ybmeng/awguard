package automations

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Envelope is the automation-123 result envelope every invocation reports —
// the last non-empty stdout line of a run (see skills/automation_123/SKILL.md).
//
// OPEN: forms 2/1 escalation and needs_human routing land here later. The MVP
// records every status including needs_human and surfaces escalation_reason,
// so the chain can attach without a schema change; it does not yet notify a
// human or invoke a cheaper/richer driver — retry_every is the pace either way.
type Envelope struct {
	Automation       string          `json:"automation"`
	Status           string          `json:"status"` // ok | degraded | failed | needs_human
	FormUsed         int             `json:"form_used"`
	Artifacts        []ArtifactEntry `json:"artifacts"`
	EscalationReason *string         `json:"escalation_reason"`
}

// ArtifactEntry describes one artifact the run touched. Newest is the latest
// period present, in a lexicographically-safe format (YYYY-MM-DD, YYYYMM).
type ArtifactEntry struct {
	Path   string `json:"path"`
	Rows   int64  `json:"rows"`
	Newest string `json:"newest"`
}

// envelopeStatuses is the closed set an envelope may carry. A run that ends
// without one of these (or without an envelope at all) is recorded with the
// service-side status "error": per the spec a driver that ends without an
// envelope is itself a defect, and it must be visible as one.
var envelopeStatuses = map[string]bool{"ok": true, "degraded": true, "failed": true, "needs_human": true}

// parseEnvelope extracts the result envelope from a run's captured stdout: the
// last non-empty line must be a single-line JSON envelope with a valid status.
// It returns the parsed envelope and the raw line (stored verbatim).
func parseEnvelope(stdout []byte) (Envelope, string, error) {
	lines := strings.Split(string(stdout), "\n")
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			last = t
			break
		}
	}
	if last == "" {
		return Envelope{}, "", errors.New("stdout is empty (the driver must emit the result envelope as its last stdout line)")
	}
	var env Envelope
	if err := json.Unmarshal([]byte(last), &env); err != nil {
		return Envelope{}, "", fmt.Errorf("last stdout line %q is not envelope JSON: %v", truncate(last, 200), err)
	}
	if !envelopeStatuses[env.Status] {
		return Envelope{}, "", fmt.Errorf("envelope status %q is not one of ok, degraded, failed, needs_human", env.Status)
	}
	return env, last, nil
}

// advanced reports whether env carries data beyond baseline.
//
// No baseline → true iff the run was ok (the first fetch is progress by
// definition). Else true iff any envelope artifact has a path absent from the
// baseline, or a newest lexicographically greater than the baseline artifact's
// newest at the same path.
//
// DECISION: string compare — the spec's period formats (YYYY-MM-DD, YYYYMM)
// are lexicographic-safe; rows is NOT compared (sources restate history at
// constant row counts, and fred-m2's full-rewrite semantics make rows
// meaningless as a progress signal).
func advanced(env Envelope, baseline *Envelope) bool {
	if baseline == nil {
		return env.Status == "ok"
	}
	base := make(map[string]string, len(baseline.Artifacts))
	for _, a := range baseline.Artifacts {
		base[a.Path] = a.Newest
	}
	for _, a := range env.Artifacts {
		newest, ok := base[a.Path]
		if !ok || a.Newest > newest {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
