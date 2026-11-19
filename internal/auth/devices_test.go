package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDescribeDevice(t *testing.T) {
	cases := []struct{ kind, ua, label string }{
		{KindWeb, "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36", "Chrome · Windows"},
		{KindWeb, "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/130.0 Safari/537.36 Edg/130.0", "Edge · Windows"},
		{KindWeb, "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Version/18.0 Mobile/15E148 Safari/604.1", "Safari · iPhone"},
		{KindWeb, "Mozilla/5.0 (Linux; Android 14; SM-A145F) AppleWebKit/537.36 Chrome/130.0 Mobile Safari/537.36", "Chrome · Android"},
		{KindWeb, "Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:132.0) Gecko/20100101 Firefox/132.0", "Firefox · Linux"},
		{KindCLI, "betorun-cli (darwin)", "CLI betorun · macOS"},
		{KindWeb, "", "?"},
	}
	for _, c := range cases {
		_, label := DescribeDevice(c.kind, c.ua)
		assert.Equal(t, c.label, label, c.ua)
	}
	a, _ := DescribeDevice(KindWeb, cases[0].ua)
	b, _ := DescribeDevice(KindWeb, "Mozilla/5.0 (Windows NT 11.0; Win64; x64) Chrome/131.0 Safari/537.36")
	c, _ := DescribeDevice(KindCLI, "betorun-cli (windows)")
	assert.Equal(t, a, b, "nouvelle version du navigateur : même appareil")
	assert.NotEqual(t, a, c)
}
