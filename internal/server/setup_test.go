package server

import (
	"errors"
	"testing"

	"github.com/TuroYT/pitlane/internal/gh"
)

type fakeSettings struct {
	set     map[string]string
	failKey string
}

func (f *fakeSettings) Set(k, v string) error {
	if k == f.failKey {
		return errors.New("disk full")
	}
	f.set[k] = v
	return nil
}

func (f *fakeSettings) SetSecret(k, v string) error { return f.Set(k, v) }

func TestSaveAppSurfacesErrors(t *testing.T) {
	app := &gh.App{ID: 1, Slug: "pitlane", ClientID: "Iv1.x", PEM: "pem", ClientSecret: "s"}
	ok := &fakeSettings{set: map[string]string{}}
	if err := saveApp(ok, app); err != nil || ok.set["app_client_id"] != "Iv1.x" || ok.set["app_pem"] != "pem" {
		t.Fatalf("err=%v set=%v", err, ok.set)
	}
	for _, key := range []string{"app_pem", "app_client_secret", "app_slug"} {
		f := &fakeSettings{set: map[string]string{}, failKey: key}
		if err := saveApp(f, app); err == nil {
			t.Fatalf("failure on %s not reported", key)
		}
		if _, ok := f.set["app_client_id"]; ok {
			t.Fatalf("failure on %s left the App marked as configured", key)
		}
	}
}
