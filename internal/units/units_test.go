package units

import "testing"

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"0":      0,
		"4096":   4096,
		"64K":    64 << 10,
		"64k":    64 << 10,
		"64KiB":  64 << 10,
		"64KB":   64 << 10,
		"1.5G":   3 << 29,
		"20 GiB": 20 << 30,
		"2T":     2 << 40,
	}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q: got %d want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "abc", "-1", "K", "1X"} {
		if _, err := ParseSize(bad); err == nil {
			t.Fatalf("%q: expected error", bad)
		}
	}
}

func TestFormatSize(t *testing.T) {
	if got := FormatSize(512); got != "512 B" {
		t.Fatal(got)
	}
	if got := FormatSize(64 << 10); got != "64.0 KiB" {
		t.Fatal(got)
	}
	if got := FormatSize(3 << 29); got != "1.5 GiB" {
		t.Fatal(got)
	}
}
