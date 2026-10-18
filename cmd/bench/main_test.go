package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateMakesUniqueFirstTagOnly(t *testing.T) {
	dir := t.TempDir()
	tmpl := filepath.Join(dir, "t.xml")
	src := `<R><ID>1</ID><X><ID>keep</ID></X></R>`
	if err := os.WriteFile(tmpl, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := generate(out, tmpl, 3, "ID"); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		b, err := os.ReadFile(filepath.Join(out, "f00000"+string(rune('0'+i))+".xml"))
		if err != nil {
			t.Fatal(err)
		}
		body := string(b)
		if !strings.Contains(body, "<ID>keep</ID>") {
			t.Errorf("later <ID> must be left alone: %s", body)
		}
		seen[body] = true
	}
	if len(seen) != 3 {
		t.Errorf("want 3 distinct files, got %d", len(seen))
	}
}

func TestGenerateRequiresTag(t *testing.T) {
	dir := t.TempDir()
	tmpl := filepath.Join(dir, "t.xml")
	_ = os.WriteFile(tmpl, []byte(`<R/>`), 0o644)
	if err := generate(filepath.Join(dir, "o"), tmpl, 1, "ID"); err == nil {
		t.Fatal("expected error when template lacks the tag")
	}
}

func TestIntsAndMedian(t *testing.T) {
	got := ints("1, 4,x,0,16")
	if len(got) != 3 || got[0] != 1 || got[1] != 4 || got[2] != 16 {
		t.Errorf("ints = %v", got)
	}
	if m := median([]float64{9, 1, 5}); m != 5 {
		t.Errorf("median = %v", m)
	}
}

func TestNormalizeErrGroupsByShape(t *testing.T) {
	a := normalizeErr("f000001.xml: oracle: ORA-12516 at 5")
	b := normalizeErr("f000002.xml: oracle: ORA-12516 at 9")
	if a != b {
		t.Errorf("want equal after normalizing, got %q vs %q", a, b)
	}
}
