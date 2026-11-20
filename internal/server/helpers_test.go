package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/TuroYT/pitlane/internal/gh"
	"github.com/TuroYT/pitlane/internal/store"
)

func TestValidateSettings(t *testing.T) {
	short, long, blank := "short", strings.Repeat("x", minMetricsToken), "  "
	for _, tc := range []struct {
		in   Settings
		want string // part of the message, "" when valid
	}{
		{Settings{Slots: 4}, ""},
		{Settings{Slots: 4, StrictSlots: -1, MetricsToken: &long}, ""},
		{Settings{Slots: 4, MetricsToken: &blank}, ""}, // removes the token
		{Settings{Slots: 0}, "parallel jobs"},
		{Settings{Slots: maxSlots + 1}, "parallel jobs"},
		{Settings{Slots: 4, StrictSlots: 4}, "strict jobs"},
		{Settings{Slots: 4, StrictSlots: -2}, "strict jobs"},
		{Settings{Slots: 4, MemoryReserveMB: -1}, "memory reserve"},
		{Settings{Slots: 4, MetricsToken: &short}, "metrics token"},
	} {
		got := validateSettings(tc.in)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%+v: got %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestJoinNonEmpty(t *testing.T) {
	if got := joinNonEmpty("; ", "", "a", "", "b"); got != "a; b" {
		t.Fatalf("got %q", got)
	}
	if got := joinNonEmpty("; ", "", ""); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestTargetPatch(t *testing.T) {
	on, off, label, bad := true, false, "gpu", "a/b"
	minR, maxR, mins := 9, 3, -5
	if (targetPatch{Enabled: &off}).adminOnly() {
		t.Error("enabling or disabling is not admin only")
	}
	if !(targetPatch{Enabled: &on, MinRunners: &minR}).adminOnly() {
		t.Error("warm runners are admin only")
	}
	if !(targetPatch{Strict: &off}).turnsStrictOff(store.Target{Strict: true}) ||
		(targetPatch{Strict: &off}).turnsStrictOff(store.Target{}) || (targetPatch{}).turnsStrictOff(store.Target{Strict: true}) {
		t.Error("turnsStrictOff")
	}
	tg := store.Target{Enabled: true, MinRunners: 1, MaxRunners: 4, ScaleSetName: "pitlane", ScaleSetID: 7, MaxJobMinutes: 60}
	if err := (targetPatch{Enabled: &off, MinRunners: &minR, MaxRunners: &maxR, ScaleSetName: &label, MaxJobMins: &mins}).apply(&tg); err != nil {
		t.Fatal(err)
	}
	want := store.Target{Enabled: false, MinRunners: 3, MaxRunners: 3, ScaleSetName: "gpu", ScaleSetID: 0, MaxJobMinutes: 0}
	if tg != want {
		t.Fatalf("got %+v\nwant %+v", tg, want)
	}
	if err := (targetPatch{ScaleSetName: &bad}).apply(&tg); err == nil || err.Error() != "invalid runner label" {
		t.Fatalf("bad label: %v", err)
	}
}

type fakeInstallations struct {
	insts []gh.Installation
	repos map[int64][]gh.Repo
	err   error
}

func (f fakeInstallations) Installations(context.Context) ([]gh.Installation, error) {
	return f.insts, f.err
}

func (f fakeInstallations) Repositories(_ context.Context, id int64) ([]gh.Repo, error) {
	r, ok := f.repos[id]
	if !ok {
		return nil, errors.New("403")
	}
	return r, nil
}

func TestListInstallations(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var a, b gh.Installation
	a.ID, a.Account.Login, a.Account.Type = 1, "acme", "Organization"
	b.ID, b.Account.Login = 2, "bob"
	list, err := listInstallations(context.Background(), fakeInstallations{
		insts: []gh.Installation{a, b},
		repos: map[int64][]gh.Repo{1: {{Name: "Web", FullName: "acme/Web", Private: true}, {Name: "docs", FullName: "acme/docs"}}},
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Account != "acme" || len(list[0].Repos) != 2 || list[0].Error != "" {
		t.Fatalf("first installation: %+v", list)
	}
	if list[1].Error == "" || list[1].Repos == nil {
		t.Fatalf("failed listing must carry an error and an empty list: %+v", list[1])
	}
	if got := repoPrivacy(list); !reflect.DeepEqual(got, map[string]bool{"acme/web": true, "acme/docs": false}) {
		t.Fatalf("privacy %v", got)
	}
	if _, err := listInstallations(context.Background(), fakeInstallations{err: errors.New("down")}, log); err == nil {
		t.Fatal("error lost")
	}
}
