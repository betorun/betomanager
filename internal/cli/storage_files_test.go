package cli

import "testing"

func TestRemoteRef(t *testing.T) {
	cases := []struct {
		in, bucket, key string
		ok              bool
	}{
		{"uploads:images/logo.png", "uploads", "images/logo.png", true},
		{"uploads:", "uploads", "", true},
		{"uploads:/a.txt", "uploads", "a.txt", true},
		{"./logo.png", "", "", false},
		{"/tmp/a:b", "", "", false},
		{`C:\files\a.png`, "", "", false}, // lettre de lecteur
		{":x", "", "", false},
	}
	for _, c := range cases {
		b, k, ok := remoteRef(c.in)
		if ok != c.ok || b != c.bucket || k != c.key {
			t.Errorf("%q → (%q, %q, %v)", c.in, b, k, ok)
		}
	}
}
