package proxy

import "testing"

func TestNormalizeFormats(t *testing.T) {
	cases := []string{
		"host.example:8080:user:pass",
		"socks5://user:pass@host.example:8080",
		"user:pass@host.example:8080",
		"host.example:8080@user:pass",
	}
	for _, input := range cases {
		got, err := Normalize(input)
		if err != nil {
			t.Errorf("Normalize(%q): %v", input, err)
		}
		if got == "" {
			t.Errorf("Normalize(%q) returned empty", input)
		}
	}
}

func TestParseLinesPreservesDuplicates(t *testing.T) {
	entries, err := ParseLines("host:1:u:p\nhost:1:u:p\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].ID == entries[1].ID {
		t.Fatalf("entries = %#v", entries)
	}
	available, total := Counts(entries)
	if available != 2 || total != 2 {
		t.Fatalf("counts = %d/%d", available, total)
	}
}
