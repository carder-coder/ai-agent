package routing

import "testing"

func TestSelectUsesTaskTypeAndContextThreshold(t *testing.T) {
	rules := Rules{
		DefaultModel:     "standard",
		SimpleModel:      "economy",
		ComplexModel:     "premium",
		ContextThreshold: 8192,
	}
	tests := []struct {
		name        string
		taskType    string
		contextSize int
		wantModel   string
		wantReason  string
	}{
		{name: "simple task", taskType: "simple", contextSize: 20, wantModel: "economy", wantReason: "simple_task"},
		{name: "complex task", taskType: "complex", contextSize: 20, wantModel: "premium", wantReason: "complex_task"},
		{name: "long context takes priority", taskType: "simple", contextSize: 8192, wantModel: "premium", wantReason: "context_threshold"},
		{name: "default route", contextSize: 20, wantModel: "standard", wantReason: "default"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Select(test.taskType, test.contextSize, "requested", rules)
			if got.Model != test.wantModel || got.Reason != test.wantReason {
				t.Fatalf("Select() = %+v, want model=%q reason=%q", got, test.wantModel, test.wantReason)
			}
		})
	}
}

func TestSelectFallsBackToRequestedModel(t *testing.T) {
	got := Select("", 0, "client-model", Rules{})
	if got.Model != "client-model" || got.Reason != "default" {
		t.Fatalf("Select() = %+v, want requested model fallback", got)
	}
}
