package auth

import (
	"reflect"
	"strings"
	"testing"

	"github.com/huponx/courier/internal/appconfig"
)

func TestIsLocal(t *testing.T) {
	cases := map[string]bool{
		"local":      true,
		"LOCAL":      true,
		" local ":    true,
		"dev":        false,
		"staging":    false,
		"production": false,
		"":           false,
		"foo":        false,
		"localhost":  false,
	}
	for env, want := range cases {
		if got := isLocal(env); got != want {
			t.Errorf("isLocal(%q) = %v, want %v", env, got, want)
		}
	}
}

func courierCfg(env, token, audience string, allowed ...string) appconfig.Courier {
	var c appconfig.Courier
	c.Env = env
	c.S2SToken = token
	c.AuthAudience = audience
	c.AuthAllowed = allowed
	return c
}

func hasWarning(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func TestAuthSettingsLocal(t *testing.T) {
	local, _, ws := authSettings(courierCfg("local", "", "courier"))
	if !local || !hasWarning(ws, "auth is disabled") {
		t.Fatalf("local without a token must warn that auth is off: local=%v warnings=%v", local, ws)
	}
	local, _, ws = authSettings(courierCfg("local", "s3cret", "courier"))
	if !local || len(ws) != 0 {
		t.Fatalf("local with a token has optional auth enabled and no warning: local=%v warnings=%v", local, ws)
	}
}

func TestAuthSettingsRequiresKubeAuthEverywhereElse(t *testing.T) {
	for _, env := range []string{"dev", "staging", "production", "", "foo"} {
		local, kube, _ := authSettings(courierCfg(env, "", "courier", " system:serviceaccount:a:b ", "", "system:serviceaccount:c:d"))
		if local {
			t.Fatalf("ENV=%q must not be local", env)
		}
		if kube.Audience != "courier" {
			t.Fatalf("ENV=%q audience %q", env, kube.Audience)
		}
		want := []string{"system:serviceaccount:a:b", "system:serviceaccount:c:d"}
		if !reflect.DeepEqual(kube.AllowedSubjects, want) {
			t.Fatalf("ENV=%q allowed %v", env, kube.AllowedSubjects)
		}
	}
}

func TestAuthSettingsEmptyAllowedWarnsButIsValid(t *testing.T) {
	_, kube, ws := authSettings(courierCfg("production", "", "courier"))
	if len(kube.AllowedSubjects) != 0 || !hasWarning(ws, "every gRPC caller is rejected") {
		t.Fatalf("allowed=%v warnings=%v", kube.AllowedSubjects, ws)
	}
	_, _, ws = authSettings(courierCfg("production", "", "courier", "", "  "))
	if !hasWarning(ws, "every gRPC caller is rejected") {
		t.Fatalf("blank entries count as empty: %v", ws)
	}
}

func TestAuthSettingsIgnoresStaticTokenOutsideLocal(t *testing.T) {
	_, _, ws := authSettings(courierCfg("staging", "left-over", "courier", "system:serviceaccount:a:b"))
	if !hasWarning(ws, "COURIER_S2S_TOKEN is ignored") {
		t.Fatalf("warnings %v", ws)
	}
}
