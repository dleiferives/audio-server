package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteReadDelete(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write("abc", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	data, err := s.Read("abc")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("expected 'hello', got %q", data)
	}
	if err := s.Delete("abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read("abc"); err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestWriteExists(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir, 0)
	s.Write("x", []byte("first"))
	data, _ := s.Read("x")
	if string(data) != "first" {
		t.Fatalf("expected 'first', got %q", data)
	}
	s.Write("x", []byte("second"))
	data, _ = s.Read("x")
	if string(data) != "second" {
		t.Fatalf("expected 'second', got %q", data)
	}
}

func TestSweep(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write("keep", []byte("keep")); err != nil {
		t.Fatal(err)
	}
	if err := s.Write("gone", []byte("gone")); err != nil {
		t.Fatal(err)
	}
	gonePath := filepath.Join(s.Dir, "gone.pcm")
	if err := os.Chtimes(gonePath, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.sweep()
	if _, err := s.Read("gone"); err == nil {
		t.Fatal("expected 'gone' to be swept")
	}
	if _, err := s.Read("keep"); err != nil {
		t.Fatalf("expected 'keep' to remain: %v", err)
	}
}
