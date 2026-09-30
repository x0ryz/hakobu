package ops

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/x0ryz/hakobu/internal/store"
)

func TestSealedVars(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(CreateProject(s, "p"))
	p, _ := s.GetProject(ctx(), "p")
	must(s.CreateApp(ctx(), store.CreateAppParams{ProjectID: p.ID, Name: "web", BuildStrategy: "dockerfile"}))

	must(SetSharedEnv(s, "p", "SHARED=1\nOVERRIDDEN=project"))
	must(SealVar(s, "project", "p", "PROJECT_SECRET", "ps"))
	must(SealVar(s, "project", "p", "SHADOWED", "sealed"))
	must(SetAppEnv(s, "web", "VISIBLE=v\nSHADOWED=visible-app"))
	must(SealVar(s, "app", "web", "API_KEY", "sk-live"))
	must(SealVar(s, "app", "web", "OVERRIDDEN", "app-sealed"))

	// A name can't be visible and sealed in the same scope.
	if err := SealVar(s, "app", "web", "VISIBLE", "x"); err == nil {
		t.Error("sealed a name that's visible in the same scope")
	}
	if err := SetAppEnv(s, "web", "API_KEY=plain"); err == nil {
		t.Error("a visible variable took a sealed name")
	}
	if err := SealVar(s, "app", "nope", "K", "v"); err == nil {
		t.Error("sealed a variable of a missing app")
	}
	if err := SealVar(s, "app", "web", "1BAD", "v"); err == nil {
		t.Error("accepted an invalid name")
	}

	app, _ := s.GetApp(ctx(), "web")
	env, err := appEnv(s, app, 8080)
	must(err)
	last := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		last[k] = v
	}
	for k, want := range map[string]string{"API_KEY": "sk-live", "PROJECT_SECRET": "ps", "SHADOWED": "visible-app", "OVERRIDDEN": "app-sealed", "SHARED": "1"} {
		if last[k] != want {
			t.Errorf("container gets %s=%q, want %q", k, last[k], want)
		}
	}

	// The panel's view: sealed values never leave the server.
	shown, err := EffectiveEnv(s, app)
	must(err)
	for _, e := range shown {
		switch e.Key {
		case "API_KEY", "PROJECT_SECRET", "OVERRIDDEN":
			if !e.Sealed || e.Value != "" {
				t.Errorf("%s shown as %+v", e.Key, e)
			}
		case "SHADOWED":
			if e.Sealed || e.Value != "visible-app" {
				t.Errorf("a visible app variable over a sealed project one: %+v", e)
			}
		}
	}
	if keys := SealedKeys(s, "app", "web"); !slices.Equal(keys, []string{"API_KEY", "OVERRIDDEN"}) {
		t.Errorf("sealed keys = %v", keys)
	}

	// Deleting the app takes its sealed variables along.
	must(DeleteApp(s, "web"))
	if vars, _ := s.ListAllSealedVars(ctx()); len(vars) != 2 {
		t.Errorf("%d sealed vars left, want the project's 2", len(vars))
	}
}
