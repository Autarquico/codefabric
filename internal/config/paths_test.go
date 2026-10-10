package config

import (
	"errors"
	"path/filepath"
	"testing"
)

func resolve(t *testing.T, env map[string]string) (Paths, error) {
	t.Helper()
	return Resolve(func(k string) string { return env[k] }, func() (string, error) { return "/home/ana", nil })
}

func TestResolveUsesXDGDefaultsOnEveryOS(t *testing.T) {
	p, err := resolve(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigDir != "/home/ana/.config/codefabric" || p.DataDir != "/home/ana/.local/share/codefabric" {
		t.Errorf("%+v", p)
	}
	if p.CredentialsDir() != "/home/ana/.config/codefabric/credentials" ||
		p.DatabaseFile() != "/home/ana/.local/share/codefabric/controller.db" ||
		p.PKIDir() != "/home/ana/.local/share/codefabric/pki" {
		t.Errorf("derived paths: %s %s %s", p.CredentialsDir(), p.DatabaseFile(), p.PKIDir())
	}
}

func TestResolvePrecedence(t *testing.T) {
	p, err := resolve(t, map[string]string{
		"XDG_CONFIG_HOME": "/xdg/c", "XDG_DATA_HOME": "/xdg/d",
		"CF_CONFIG_DIR": "/override/c",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigDir != "/override/c" {
		t.Errorf("CF_CONFIG_DIR lost: %s", p.ConfigDir)
	}
	if p.DataDir != filepath.Join("/xdg/d", "codefabric") {
		t.Errorf("XDG_DATA_HOME lost: %s", p.DataDir)
	}
}

func TestResolveIgnoresRelativeXDGAndRejectsRelativeOverrides(t *testing.T) {
	p, err := resolve(t, map[string]string{"XDG_CONFIG_HOME": "relative/dir"})
	if err != nil || p.ConfigDir != "/home/ana/.config/codefabric" {
		t.Errorf("%+v %v", p, err)
	}
	if _, err := resolve(t, map[string]string{"CF_DATA_DIR": "relative"}); err == nil {
		t.Error("a relative CF_DATA_DIR was accepted")
	}
}

func TestResolveReportsAMissingHome(t *testing.T) {
	_, err := Resolve(func(string) string { return "" }, func() (string, error) { return "", errors.New("no home") })
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestControllerConfigRoundTripAndMissing(t *testing.T) {
	p := Paths{ConfigDir: t.TempDir(), DataDir: t.TempDir()}
	if _, err := LoadController(p); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("err = %v", err)
	}
	want := Controller{Listen: "0.0.0.0:7443", Advertise: "ctl.example:7443"}
	if err := SaveController(p, want); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadController(p); err != nil || got != want {
		t.Fatalf("%+v %v", got, err)
	}
}
