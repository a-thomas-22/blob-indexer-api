package docs

import "testing"

func TestSwaggerInfoRegistered(t *testing.T) {
	if SwaggerInfo == nil {
		t.Fatal("expected SwaggerInfo to be initialized")
	}
	if SwaggerInfo.InstanceName() != "swagger" {
		t.Fatalf("expected swagger instance name, got %q", SwaggerInfo.InstanceName())
	}
	if SwaggerInfo.Title == "" {
		t.Fatal("expected non-empty swagger title")
	}
}
