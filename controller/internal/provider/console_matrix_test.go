package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// Sprint 27 O2: the provider console mirrors the role matrix to decide what
// it shows (UX only; decide() authorizes). provider-console/src/auth/
// role-matrix.json must list exactly roleActions, so the console can't
// drift from the server.
func TestConsoleRoleMatrix(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "provider-console", "src", "auth", "role-matrix.json"))
	if err != nil {
		t.Fatalf("read console role matrix: %v", err)
	}
	var doc struct {
		Roles map[string][]string `json:"roles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{}
	for role, set := range roleActions {
		var list []string
		for a := range set {
			list = append(list, a)
		}
		sort.Strings(list)
		want[role] = list
	}
	for role := range doc.Roles {
		sort.Strings(doc.Roles[role])
	}
	if !reflect.DeepEqual(doc.Roles, want) {
		t.Fatalf("console role-matrix.json differs from roleActions:\n console %v\n server  %v", doc.Roles, want)
	}
}
