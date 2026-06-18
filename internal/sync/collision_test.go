package sync

import "testing"

func TestCollisionKey(t *testing.T) {
	// Case-only differences collide.
	if collisionKey("Dir/File.TXT") != collisionKey("dir/file.txt") {
		t.Fatal("case-only difference should produce the same collision key")
	}
	// Backslash and forward slash normalise to the same key.
	if collisionKey(`a\b\c.txt`) != collisionKey("a/b/c.txt") {
		t.Fatal("path separators should normalise to the same collision key")
	}
	// Genuinely different names do not collide.
	if collisionKey("a/one.txt") == collisionKey("a/two.txt") {
		t.Fatal("distinct names should not share a collision key")
	}
}
