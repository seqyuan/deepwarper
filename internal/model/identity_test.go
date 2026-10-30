package model

import (
	"strings"
	"testing"
)

func TestValidateUserID(t *testing.T) {
	valid := []string{
		"alice",
		"Alice",
		"_svc",
		"a",
		"user.name",
		"user_name",
		"user@example.com",
		"user+tag",
		"user-name",
		"0abc",
		strings.Repeat("a", 128),
	}
	for _, s := range valid {
		if err := ValidateUserID(s); err != nil {
			t.Fatalf("%q must be accepted: %v", s, err)
		}
	}
	invalid := []string{
		"",
		".",
		"..",
		".hidden",
		"trailing.",
		"a/b",
		"a\\b",
		"has space",
		"has\x00nul",
		"has\nnewline",
		"-leading-dash",
		strings.Repeat("a", 129),
	}
	for _, s := range invalid {
		if err := ValidateUserID(s); err == nil {
			t.Fatalf("%q must be rejected", s)
		}
	}
}

func TestValidateDistKey(t *testing.T) {
	for _, s := range []string{"default", "a", "a1", "my-dist", strings.Repeat("a", 32)} {
		if err := ValidateDistKey(s); err != nil {
			t.Fatalf("%q must be accepted: %v", s, err)
		}
	}
	for _, s := range []string{"", "Default", "-lead", "a_b", "a.b", "a/b", "a b", strings.Repeat("a", 33)} {
		if err := ValidateDistKey(s); err == nil {
			t.Fatalf("%q must be rejected", s)
		}
	}
}

func TestIsSafeFileComponent(t *testing.T) {
	for _, s := range []string{"a", "a-b.c", "0", "user_name"} {
		if !IsSafeFileComponent(s) {
			t.Fatalf("%q must be safe", s)
		}
	}
	for _, s := range []string{"", ".", "..", "a/b", "a\\b", "a\x00b", "a\nb", "a\tb"} {
		if IsSafeFileComponent(s) {
			t.Fatalf("%q must be unsafe", s)
		}
	}
}

func TestLocalSlugKeepsCaseApart(t *testing.T) {
	if LocalSlug("Alice") == LocalSlug("alice") {
		t.Fatal("Alice and alice must not share a local slug")
	}
	if got := LocalSlug("alice"); got != "alice" {
		t.Fatalf("canonical username must be used verbatim: %q", got)
	}
	if got := LocalSlug("Alice"); !strings.HasPrefix(got, "alice-") || len(got) <= len("alice-") {
		t.Fatalf("non-canonical username must get a stable hash suffix: %q", got)
	}
	if LocalSlug("Alice") != LocalSlug("Alice") {
		t.Fatal("stable slug is not deterministic")
	}
}

func TestInstanceIDIsUnambiguous(t *testing.T) {
	if InstanceID("a", "b-c") == InstanceID("a-b", "c") {
		t.Fatal("different (user, dist) pairs collided")
	}
	id := InstanceID("alice", "default")
	if !IsSafeFileComponent(id) {
		t.Fatalf("instance id is not safe: %q", id)
	}
	if !strings.HasPrefix(id, "alice-") || !strings.HasSuffix(id, "-default") {
		t.Fatalf("unexpected id shape: %q", id)
	}
	if InstanceID("alice", "default") != id {
		t.Fatal("instance id is not deterministic")
	}
	if InstanceID("alice", "other") == id {
		t.Fatal("distribution must change the id")
	}
}

func TestLegacyInstanceIDUnchanged(t *testing.T) {
	if got := LegacyInstanceID("alice", "default"); got != "alice-default" {
		t.Fatalf("legacy id: %q", got)
	}
}
