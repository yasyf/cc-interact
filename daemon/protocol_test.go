package daemon

import "testing"

func TestRuntimeHealthOperationNamespaceIsExact(t *testing.T) {
	if OpRuntimeHealth != "cc-interact.runtime.health" {
		t.Fatalf("runtime health op = %q", OpRuntimeHealth)
	}
}
