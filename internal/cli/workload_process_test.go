package cli

import (
	"reflect"
	"testing"
)

func TestSplitCommand(t *testing.T) {
	cases := map[string][]string{
		`node queue.js`:                        {"node", "queue.js"},
		`  python  -m  app  `:                  {"python", "-m", "app"},
		`sh -c 'npm run migrate && npm start'`: {"sh", "-c", "npm run migrate && npm start"},
		`echo "a b" c\ d`:                      {"echo", "a b", "c d"},
		`echo ''`:                              {"echo", ""},
		``:                                     nil,
	}
	for in, want := range cases {
		got, err := splitCommand(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q → %q, %v ; attendu %q", in, got, err, want)
		}
	}
	for _, bad := range []string{`echo "a`, `echo 'a`, `echo \`} {
		if _, err := splitCommand(bad); err == nil {
			t.Errorf("%q accepté", bad)
		}
	}
}

func TestSplitRoute(t *testing.T) {
	cases := map[string][2]string{
		"www.exemple.sn":              {"www.exemple.sn", "/"},
		"www.exemple.sn.":             {"www.exemple.sn", "/"},
		"backoffice.exemple.sn/auth":  {"backoffice.exemple.sn", "/auth"},
		"backoffice.exemple.sn/auth/": {"backoffice.exemple.sn", "/auth"},
		"exemple.sn/":                 {"exemple.sn", "/"},
	}
	for in, want := range cases {
		h, p := splitRoute(in)
		if h != want[0] || p != want[1] {
			t.Errorf("%q → %q %q", in, h, p)
		}
	}
}
