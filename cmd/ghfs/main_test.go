package main

import "testing"

func TestHasFatalPreflight(t *testing.T) {
	if !hasFatalPreflight([]string{"missing /dev/fuse (FUSE unavailable)"}) {
		t.Fatal("expected /dev/fuse issue to be fatal")
	}
	if !hasFatalPreflight([]string{"missing fusermount and umount"}) {
		t.Fatal("expected missing unmount tools to be fatal")
	}
	if hasFatalPreflight([]string{"GHFS_GITHUB_TOKEN is unset"}) {
		t.Fatal("token warning should not be fatal")
	}
}

func TestStatePathDeterministic(t *testing.T) {
	a := statePath("/tmp/x")
	b := statePath("/tmp/x")
	if a != b {
		t.Fatalf("state path should be deterministic: %s != %s", a, b)
	}
}
