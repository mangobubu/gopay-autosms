package proxy

import (
	"net/url"
	"testing"
)

func TestNormalizeFormats(t *testing.T) {
	cases := []struct {
		input      string
		wantScheme string
	}{
		{input: "host.example:8080:user:pass", wantScheme: "http"},
		{input: "host.example:8080", wantScheme: "http"},
		{input: "[::1]:8080", wantScheme: "http"},
		{input: "socks5://user:pass@host.example:8080", wantScheme: "socks5"},
		{input: "HTTPS://host.example:8080", wantScheme: "https"},
		{input: "user:pass@host.example:8080", wantScheme: "http"},
		{input: "host.example:8080@user:pass", wantScheme: "http"},
	}
	for _, test := range cases {
		got, err := Normalize(test.input)
		if err != nil {
			t.Errorf("Normalize(%q): %v", test.input, err)
			continue
		}
		parsed, err := url.Parse(got)
		if err != nil {
			t.Errorf("url.Parse(Normalize(%q)): %v", test.input, err)
			continue
		}
		if parsed.Scheme != test.wantScheme {
			t.Errorf("Normalize(%q) scheme = %q, want %q", test.input, parsed.Scheme, test.wantScheme)
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
