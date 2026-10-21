package env

import "testing"

// An empty variable and garbage are treated differently: the first gives the
// default, the second must fail the configuration. The test holds that line.
func TestIntRejectsNonPositiveAndKeepsFallback(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		want  int
		fails bool
	}{
		{"empty gives default", "", 7, false},
		{"value overrides default", "3", 3, false},
		{"zero is not a value", "0", 0, true},
		{"negative", "-1", 0, true},
		{"not a number", "many", 0, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("LIMIT", c.raw)
			got, err := Int("LIMIT", 7)
			if c.fails {
				if err == nil {
					t.Fatalf("LIMIT=%q accepted, expected an error", c.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("LIMIT=%q: %v", c.raw, err)
			}
			if got != c.want {
				t.Fatalf("LIMIT=%q gave %d, expected %d", c.raw, got, c.want)
			}
		})
	}
}

func TestInt64ListRejectsPartialGarbage(t *testing.T) {
	t.Setenv("IDS", "10, 20 ,30")
	got, err := Int64List("IDS")
	if err != nil {
		t.Fatalf("list parsing: %v", err)
	}
	if len(got) != 3 || got[0] != 10 || got[2] != 30 {
		t.Fatalf("list parsed as %v", got)
	}

	// One bad element fails the whole list: a partly parsed allowlist is more
	// dangerous than an empty one because it looks functional.
	t.Setenv("IDS", "10,abc,30")
	if _, err := Int64List("IDS"); err == nil {
		t.Fatal("list with garbage accepted")
	}
}

func TestProxyURLRejectsUnsupportedScheme(t *testing.T) {
	t.Setenv("PROXY", "")
	parsed, err := ProxyURL("PROXY")
	if err != nil || parsed != nil {
		t.Fatalf("empty proxy gave %v, %v", parsed, err)
	}

	t.Setenv("PROXY", "socks5://10.0.0.1:20170")
	if _, err := ProxyURL("PROXY"); err != nil {
		t.Fatalf("socks5 rejected: %v", err)
	}

	t.Setenv("PROXY", "ftp://10.0.0.1:21")
	if _, err := ProxyURL("PROXY"); err == nil {
		t.Fatal("ftp accepted as a proxy")
	}

	t.Setenv("PROXY", "10.0.0.1:20170")
	if _, err := ProxyURL("PROXY"); err == nil {
		t.Fatal("address without a scheme accepted")
	}
}

func TestRequireReportsMissingName(t *testing.T) {
	t.Setenv("TOKEN", "")
	_, err := RequireString("TOKEN")
	if err == nil {
		t.Fatal("empty required variable accepted")
	}
	// The variable name in the error text is the only way to see from the
	// startup log what is missing.
	if got := err.Error(); got != "TOKEN is required" {
		t.Fatalf("error does not name the variable: %q", got)
	}
}
