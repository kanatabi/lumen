package adminsvc

import "testing"

func TestValidDisplayName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"simple", "Faiz", true},
		{"trimmed", "  Faiz  ", true},
		{"unicode", "Fáiz Äinur", true},
		{"blank", "   ", false},
		{"one rune", "F", true},
		{"eighty", string(make([]rune, 80)), true},
	}
	for _, c := range cases {
		if got := validDisplayName(c.in); got != c.want {
			t.Errorf("%s: validDisplayName(%q) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
	// 81 runes is over the limit
	eightyOne := ""
	for i := 0; i < 81; i++ {
		eightyOne += "a"
	}
	if validDisplayName(eightyOne) {
		t.Error("81-character display name must be refused")
	}
}

func TestNormalizeEmail(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"plain", "faiz@example.com", "faiz@example.com", true},
		{"trim + lower", "  Faiz@Example.COM ", "faiz@example.com", true},
		{"empty is allowed", "", "", true},
		{"display form refused", "Faiz <faiz@example.com>", "", false},
		{"missing domain", "faiz@", "", false},
		{"no at", "faiz.example.com", "", false},
	}
	for _, c := range cases {
		got, ok := normalizeEmail(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: normalizeEmail(%q) = (%q, %v), want (%q, %v)", c.name, c.in, got, ok, c.want, c.ok)
		}
	}
}
