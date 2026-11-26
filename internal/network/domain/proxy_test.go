package domain_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/network/domain"
)

func TestParseProxyURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in      string
		want          string // normalized URL (never with the password)
		user, pass    string
		host          string
		port          int
		scheme        domain.Scheme
		remoteDNSHint bool
	}{
		{name: "http with credentials", in: "http://user:password@proxy.example:8080",
			want: "http://user@proxy.example:8080", user: "user", pass: "password", host: "proxy.example", port: 8080, scheme: domain.SchemeHTTP},
		{name: "https", in: "https://user:password@proxy.example:8443",
			want: "https://user@proxy.example:8443", user: "user", pass: "password", host: "proxy.example", port: 8443, scheme: domain.SchemeHTTPS},
		{name: "socks5", in: "socks5://user:password@proxy.example:1080",
			want: "socks5://user@proxy.example:1080", user: "user", pass: "password", host: "proxy.example", port: 1080, scheme: domain.SchemeSOCKS5},
		{name: "socks5h is socks5 with remote DNS", in: "socks5h://proxy.example:1080",
			want: "socks5://proxy.example:1080", host: "proxy.example", port: 1080, scheme: domain.SchemeSOCKS5},
		{name: "percent-encoded credentials", in: "http://us%20er:p%40ss%3Aw%2Frd@proxy.example:3128",
			want: "http://us%20er@proxy.example:3128", user: "us er", pass: "p@ss:w/rd", host: "proxy.example", port: 3128, scheme: domain.SchemeHTTP},
		{name: "IPv6 literal", in: "socks5://[2001:DB8::1]:1080",
			want: "socks5://[2001:db8::1]:1080", host: "2001:db8::1", port: 1080, scheme: domain.SchemeSOCKS5},
		{name: "IPv4 literal, no credentials", in: "http://203.0.113.7:3128",
			want: "http://203.0.113.7:3128", host: "203.0.113.7", port: 3128, scheme: domain.SchemeHTTP},
		{name: "normalised case, whitespace and trailing slash", in: "  HTTP://Proxy.Example.COM:8080/  ",
			want: "http://proxy.example.com:8080", host: "proxy.example.com", port: 8080, scheme: domain.SchemeHTTP},
		{name: "default port by scheme", in: "socks5://proxy.example",
			want: "socks5://proxy.example:1080", host: "proxy.example", port: 1080, scheme: domain.SchemeSOCKS5},
		{name: "username only", in: "http://alice@proxy.example:8080",
			want: "http://alice@proxy.example:8080", user: "alice", host: "proxy.example", port: 8080, scheme: domain.SchemeHTTP},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p, pass, err := domain.ParseProxyURL(c.in)
			if err != nil {
				t.Fatalf("ParseProxyURL(%q): %v", c.in, err)
			}
			if got := p.URL(); got != c.want {
				t.Errorf("URL() = %q, want %q", got, c.want)
			}
			if p.Scheme != c.scheme || p.Host != c.host || p.Port != c.port || p.Username != c.user {
				t.Errorf("proxy = %+v", p)
			}
			if pass.Reveal() != c.pass {
				t.Errorf("password = %q, want %q", pass.Reveal(), c.pass)
			}
		})
	}
}

func TestParseProxyURLRejects(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                                    "required",
		"ftp://proxy.example:21":              "proxy_scheme",
		"proxy.example:8080":                  "proxy_scheme",
		"http://proxy.example:8080/path":      "proxy_format",
		"http://proxy.example:8080?x=1":       "proxy_format",
		"http://proxy.example:8080#f":         "proxy_format",
		"http://:8080":                        "proxy_host",
		"http://proxy.example:0":              "proxy_port",
		"http://proxy.example:70000":          "proxy_port",
		"http://[fe80::1%25eth0]:8080":        "proxy_host",
		"http://pro_xy.example:8080":          "proxy_host",
		"http://proxy.ex\tample:8080":         "proxy_format",
		"http://u:s3cret@proxy.example:99999": "proxy_port",
	}
	for in, code := range cases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			_, _, err := domain.ParseProxyURL(in)
			var ve *domain.ValidationError
			if !errors.As(err, &ve) || ve.Code != code {
				t.Fatalf("ParseProxyURL(%q) = %v, want code %s", in, err, code)
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Fatalf("error leaks the password: %v", err)
			}
		})
	}
}

func TestNewProxyFromFields(t *testing.T) {
	t.Parallel()
	p, err := domain.NewProxy("SOCKS5", " Proxy.Example ", 1080, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if p.URL() != "socks5://bob@proxy.example:1080" {
		t.Fatalf("URL() = %q", p.URL())
	}
	if _, err := domain.NewProxy("http", "proxy.example", 0, ""); err == nil {
		t.Fatal("port 0 accepted")
	}
	if _, err := domain.NewProxy("http", "proxy.example", 8080, "bad\nuser"); err == nil {
		t.Fatal("control character in the username accepted")
	}
}

func TestPasswordIsRedacted(t *testing.T) {
	t.Parallel()
	pw, err := domain.NewPassword("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{fmt.Sprint(pw), fmt.Sprintf("%v %+v %#v %s %q", pw, pw, pw, pw, pw), pw.String()} {
		if strings.Contains(s, "hunter2") {
			t.Fatalf("password printed: %s", s)
		}
	}
	if b, _ := pw.MarshalJSON(); strings.Contains(string(b), "hunter2") {
		t.Fatalf("password marshalled: %s", b)
	}
	if _, err := domain.NewPassword(strings.Repeat("x", domain.MaxPasswordBytes+1)); err == nil {
		t.Fatal("over-long password accepted")
	}
}
