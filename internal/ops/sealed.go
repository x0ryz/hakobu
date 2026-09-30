package ops

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

// Sealed variables, like Railway's: once saved, a value is only ever
// passed to the containers. The panel shows the key, never the value, so
// someone who gets into the panel can't simply copy the app's API keys.
// Each scope's sealed variables go on top of its visible ones.

var validEnvKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// sealedOwner checks the scope and that its owner (project or app) exists.
func sealedOwner(s *store.Store, scope, owner string) error {
	var err error
	switch scope {
	case "project":
		_, err = s.GetProject(ctx(), owner)
	case "app":
		_, err = s.GetApp(ctx(), owner)
	case "worker":
		_, err = s.GetWorker(ctx(), owner)
	default:
		return fmt.Errorf("unknown scope %q", scope)
	}
	if err != nil {
		return fmt.Errorf("%s %s not found", scope, owner)
	}
	return nil
}

// visibleEnv is the scope's visible "KEY=value" text.
func visibleEnv(s *store.Store, scope, owner string) (string, error) {
	switch scope {
	case "project":
		p, err := s.GetProject(ctx(), owner)
		return string(p.SharedEnv), err
	case "app":
		a, err := s.GetApp(ctx(), owner)
		return string(a.Env), err
	default:
		w, err := s.GetWorker(ctx(), owner)
		return string(w.Env), err
	}
}

func envKeys(text string) map[string]bool {
	keys := map[string]bool{}
	for _, line := range ParseEnv(text) {
		k, _, _ := strings.Cut(line, "=")
		keys[strings.TrimSpace(k)] = true
	}
	return keys
}

// SealVar sets a sealed variable; the value can be replaced but never read.
func SealVar(s *store.Store, scope, owner, key, value string) error {
	if err := sealedOwner(s, scope, owner); err != nil {
		return err
	}
	if !validEnvKey.MatchString(key) {
		return fmt.Errorf("invalid variable name %q: letters, digits and _, not starting with a digit", key)
	}
	if value == "" {
		return fmt.Errorf("the value of %s is empty", key)
	}
	visible, err := visibleEnv(s, scope, owner)
	if err != nil {
		return err
	}
	if envKeys(visible)[key] {
		return fmt.Errorf("%s is already a visible variable here: remove it there first", key)
	}
	return s.SetSealedVar(ctx(), store.SetSealedVarParams{Scope: scope, Owner: owner, Key: key, Value: secret.String(value)})
}

func RemoveSealedVar(s *store.Store, scope, owner, key string) error {
	return s.DeleteSealedVar(ctx(), store.DeleteSealedVarParams{Scope: scope, Owner: owner, Key: key})
}

// SealedKeys lists a scope's sealed variables, without their values.
func SealedKeys(s *store.Store, scope, owner string) []string {
	vars, _ := s.ListSealedVars(ctx(), store.ListSealedVarsParams{Scope: scope, Owner: owner})
	keys := make([]string, len(vars))
	for i, v := range vars {
		keys[i] = v.Key
	}
	return keys
}

func sealedEnv(s *store.Store, scope, owner string) ([]string, error) {
	vars, err := s.ListSealedVars(ctx(), store.ListSealedVarsParams{Scope: scope, Owner: owner})
	if err != nil {
		return nil, err
	}
	env := make([]string, len(vars))
	for i, v := range vars {
		env[i] = v.Key + "=" + string(v.Value)
	}
	return env, nil
}

// checkNotSealed refuses visible variables that are sealed in the same
// scope: which one would win would be anyone's guess.
func checkNotSealed(s *store.Store, scope, owner, text string) error {
	sealed := SealedKeys(s, scope, owner)
	keys := envKeys(text)
	for _, k := range sealed {
		if keys[k] {
			return fmt.Errorf("%s is sealed here: replace it under Sealed variables instead", k)
		}
	}
	return nil
}

// SetSharedEnv saves the project's visible shared variables.
func SetSharedEnv(s *store.Store, project, text string) error {
	if err := checkNotSealed(s, "project", project, text); err != nil {
		return err
	}
	return s.SetProjectSharedEnv(ctx(), store.SetProjectSharedEnvParams{Name: project, SharedEnv: secret.String(text)})
}

// SetAppEnv saves the app's visible variables.
func SetAppEnv(s *store.Store, app, text string) error {
	if err := checkNotSealed(s, "app", app, text); err != nil {
		return err
	}
	return s.SetAppEnv(ctx(), store.SetAppEnvParams{Name: app, Env: secret.String(text)})
}

// EnvVar is one variable of the effective environment as the panel shows
// it: a sealed one has no value.
type EnvVar struct {
	Key, Value string
	Sealed     bool
}

// EffectiveEnv is the environment of the app's next deploy, one entry per
// variable (the last one wins), with sealed values left out.
func EffectiveEnv(s *store.Store, app store.App) ([]EnvVar, error) {
	env, err := appEnv(s, app, portHint(app, 0))
	if err != nil {
		return nil, err
	}
	appVisible := envKeys(string(app.Env))
	sealed := map[string]bool{}
	for _, k := range SealedKeys(s, "project", app.ProjectName) {
		sealed[k] = !appVisible[k] // a visible app variable overrides it
	}
	for _, k := range SealedKeys(s, "app", app.Name) {
		sealed[k] = true
	}
	var out []EnvVar
	index := map[string]int{}
	for _, line := range env {
		k, v, _ := strings.Cut(line, "=")
		e := EnvVar{Key: k, Value: v}
		if sealed[k] {
			e.Value, e.Sealed = "", true
		}
		if i, ok := index[k]; ok {
			out[i] = e
			continue
		}
		index[k] = len(out)
		out = append(out, e)
	}
	return out, nil
}
