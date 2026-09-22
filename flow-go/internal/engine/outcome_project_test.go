package engine

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/kodelyx/flow-go/flow-go/internal/pool"
)

// marshalToMap renders a value the way a caller receives it.
func marshalToMap(t *testing.T, value any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return decoded
}

// All three generation outcomes name the account and the project the job ran in,
// and they have to use the same keys.
//
// A divergence here is silent rather than loud: a caller reading `project_id`
// against a struct that spells it `projectId` decodes into an empty string, which
// is indistinguishable from a project that was never resolved. The batch path
// already carried the field, so this is what keeps the pool-routed outcomes
// aligned with it.
func TestGenerationOutcomesAgreeOnTheProjectKey(t *testing.T) {
	outcomes := map[string]any{
		"VideoOutcome":      VideoOutcome{},
		"BatchVideoOutcome": BatchVideoOutcome{},
		"ImageOutcome":      ImageOutcome{},
	}

	for name, outcome := range outcomes {
		decoded := marshalToMap(t, outcome)
		for _, key := range []string{"project_id", "account_id"} {
			if _, ok := decoded[key]; !ok {
				t.Errorf("%s has no %q key; it has %s", name, key, sortedKeys(decoded))
			}
		}
	}
}

// The project travels on the value. An outcome built from a job the pool routed
// has to report the serving worker's project, which is the reason it is read off
// the worker instead of from the engine.
func TestVideoOutcomeCarriesTheProject(t *testing.T) {
	decoded := marshalToMap(t, VideoOutcome{
		JobID:     "job-1",
		Account:   "acct-a",
		ProjectID: "project-a",
	})

	if decoded["project_id"] != "project-a" {
		t.Errorf("project_id = %v, want project-a", decoded["project_id"])
	}
	if decoded["account_id"] != "acct-a" {
		t.Errorf("account_id = %v, want acct-a", decoded["account_id"])
	}
}

func TestImageOutcomeCarriesTheProject(t *testing.T) {
	decoded := marshalToMap(t, ImageOutcome{
		JobID:     "job-2",
		Account:   "acct-b",
		ProjectID: "project-b",
	})

	if decoded["project_id"] != "project-b" {
		t.Errorf("project_id = %v, want project-b", decoded["project_id"])
	}
}

// An unresolved project stays visible as an empty string rather than being
// dropped from the payload. The key's presence is what tells a caller the field
// was considered, so `omitempty` here would make "no project resolved" and "a
// server that predates this field" look identical.
func TestOutcomesKeepAnEmptyProjectKey(t *testing.T) {
	for name, decoded := range map[string]map[string]any{
		"VideoOutcome": marshalToMap(t, VideoOutcome{JobID: "job-1"}),
		"ImageOutcome": marshalToMap(t, ImageOutcome{JobID: "job-2"}),
	} {
		value, ok := decoded["project_id"]
		if !ok {
			t.Errorf("%s drops project_id when it is empty; an unresolved project must still be reported", name)
			continue
		}
		if value != "" {
			t.Errorf("%s project_id = %v, want empty", name, value)
		}
	}
}

// sortedKeys keeps a failure message readable without pulling in a helper that
// has to know the shape of every outcome.
func sortedKeys(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// The account and the project are read off the same worker, and that is the point
// of returning them together. A pair assembled from two different workers would
// name a project the account cannot generate into, and nothing downstream could
// tell: the upstream refusal reads as unusual activity, not as a mismatched
// project, so it would present as an intermittent generation failure.
func TestServingWorkerNamesTheAccountAndItsProject(t *testing.T) {
	w := pool.NewWorker("acct-a", nil)
	w.SetProjectID("project-a")

	accountID, projectID := servingWorker(w)
	if accountID != "acct-a" {
		t.Errorf("accountID = %q, want acct-a", accountID)
	}
	if projectID != "project-a" {
		t.Errorf("projectID = %q, want project-a", projectID)
	}
}

// A project that was never resolved is passed through as empty rather than
// substituted. The engine has no second source to fall back to, and inventing one
// would attribute the job to a project it never touched.
func TestServingWorkerPassesThroughAnUnresolvedProject(t *testing.T) {
	w := pool.NewWorker("acct-b", nil)

	accountID, projectID := servingWorker(w)
	if accountID != "acct-b" {
		t.Errorf("accountID = %q, want acct-b", accountID)
	}
	if projectID != "" {
		t.Errorf("projectID = %q, want empty — nothing may be invented here", projectID)
	}
}
