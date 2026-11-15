package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUsersLastAdmin(t *testing.T) {
	s := open(t)
	if s.HasUsers() {
		t.Fatal("fresh store has users")
	}
	if _, err := s.PutUser(User{Login: "alice", Role: RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutUser(User{Login: "bob", Role: "root"}); err == nil {
		t.Fatal("invalid role accepted")
	}
	// The only admin can be neither demoted nor removed.
	if _, err := s.PutUser(User{Login: "ALICE", Role: RoleViewer}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demote last admin: %v", err)
	}
	if _, err := s.DeleteUser("alice"); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("remove last admin: %v", err)
	}
	if prev, err := s.PutUser(User{Login: "bob", Role: RoleViewer}); err != nil || prev != "" {
		t.Fatalf("add bob: %q %v", prev, err)
	}
	// A viewer does not count as an admin.
	if _, err := s.DeleteUser("alice"); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("remove last admin with a viewer left: %v", err)
	}
	if prev, err := s.PutUser(User{Login: "Bob", Role: RoleAdmin}); err != nil || prev != RoleViewer {
		t.Fatalf("promote bob: %q %v", prev, err)
	}
	if got := s.Get("allowed_users", ""); got != "alice,bob" {
		t.Fatalf("legacy mirror %q", got)
	}
	if err := s.CreateSession("tok", "Alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	if u, err := s.DeleteUser("alice"); err != nil || u.Role != RoleAdmin {
		t.Fatalf("remove alice: %+v %v", u, err)
	}
	if _, ok := s.SessionLogin("tok"); ok {
		t.Fatal("session of a removed user kept")
	}
	if _, err := s.DeleteUser("alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove twice: %v", err)
	}
	if u, ok := s.User("BOB"); !ok || u.Login != "bob" || u.Role != RoleAdmin {
		t.Fatalf("lookup %+v %v", u, ok)
	}
	if got := s.Get("allowed_users", ""); got != "bob" {
		t.Fatalf("legacy mirror %q", got)
	}
	users, err := s.Users()
	if err != nil || len(users) != 1 {
		t.Fatalf("users %+v %v", users, err)
	}
}

func TestAccessRules(t *testing.T) {
	s := open(t)
	r, err := s.PutAccessRule(AccessRule{Org: "Acme", Team: "Ops", Role: RoleAdmin, CreatedBy: "alice"})
	if err != nil || r.ID == 0 || r.Subject() != "acme/ops" {
		t.Fatalf("%+v %v", r, err)
	}
	// Same organization and team (any case): the role changes.
	r2, err := s.PutAccessRule(AccessRule{Org: "acme", Team: "ops", Role: RoleViewer})
	if err != nil || r2.ID != r.ID || r2.Role != RoleViewer || r2.CreatedBy != "alice" {
		t.Fatalf("%+v %v", r2, err)
	}
	if _, err := s.PutAccessRule(AccessRule{Org: "acme", Role: RoleViewer}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutAccessRule(AccessRule{Org: "acme", Role: "nope"}); err == nil {
		t.Fatal("invalid role accepted")
	}
	rules, err := s.AccessRules()
	if err != nil || len(rules) != 2 || rules[0].Subject() != "acme" {
		t.Fatalf("%+v %v", rules, err)
	}
	if d, err := s.DeleteAccessRule(r.ID); err != nil || d.Subject() != "acme/ops" {
		t.Fatalf("%+v %v", d, err)
	}
	if _, err := s.DeleteAccessRule(r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestSessionMemberships(t *testing.T) {
	s := open(t)
	if err := s.CreateSession("a", "alice", time.Hour, "acme", "acme/ops"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("b", "bob", time.Hour); err != nil {
		t.Fatal(err)
	}
	if sess, ok := s.Session("a"); !ok || sess.Login != "alice" || len(sess.Memberships) != 2 || sess.Memberships[1] != "acme/ops" {
		t.Fatalf("%+v %v", sess, ok)
	}
	if sess, ok := s.Session("b"); !ok || sess.Memberships != nil {
		t.Fatalf("%+v %v", sess, ok)
	}
}

func TestAuditLog(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	old := time.Now().Add(-40 * 24 * time.Hour).Unix()
	for i, e := range []AuditEntry{
		{At: old, Actor: "alice", Action: "settings.update"},
		{At: 100 + old, Actor: "alice", Action: "target.create", Subject: "acme/api"},
		{Actor: "bob", Action: "user.add", Subject: "carol", Detail: "role viewer"},
		{Actor: "bob", Action: "user.remove", Subject: "carol"},
	} {
		if err := s.AddAudit(e); err != nil {
			t.Fatal(i, err)
		}
	}
	page, total, err := s.AuditPage(ctx, 2, 0)
	if err != nil || total != 4 || len(page) != 2 {
		t.Fatalf("%+v %d %v", page, total, err)
	}
	// Latest first (same second: the last inserted first).
	if page[0].Action != "user.remove" || page[1].Action != "user.add" || page[0].At == 0 {
		t.Fatalf("order %+v", page)
	}
	page, _, _ = s.AuditPage(ctx, 2, 2)
	if len(page) != 2 || page[0].Action != "target.create" || page[1].Action != "settings.update" {
		t.Fatalf("second page %+v", page)
	}
	if err := s.PruneAudit(30 * 24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, total, _ = s.AuditPage(ctx, 10, 0); total != 2 {
		t.Fatalf("after prune: %d", total)
	}
}
