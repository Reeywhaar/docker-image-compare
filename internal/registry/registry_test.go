package registry

import "testing"

func TestParseRef(t *testing.T) {
	cases := []struct {
		in       string
		registry string
		repo     string
		ref      string
	}{
		{"nginx", dockerHub, "library/nginx", "latest"},
		{"nginx:1.27", dockerHub, "library/nginx", "1.27"},
		{"redis:7", dockerHub, "library/redis", "7"},
		{"bitnami/redis", dockerHub, "bitnami/redis", "latest"},
		{"bitnami/redis:latest", dockerHub, "bitnami/redis", "latest"},
		{"ghcr.io/owner/img", "ghcr.io", "owner/img", "latest"},
		{"ghcr.io/owner/img:v2", "ghcr.io", "owner/img", "v2"},
		{"localhost:5000/foo:bar", "localhost:5000", "foo", "bar"},
		{"gcr.io/proj/sub/img:tag", "gcr.io", "proj/sub/img", "tag"},
		{"nginx@sha256:abc", dockerHub, "library/nginx", "sha256:abc"},
		{"ghcr.io/o/i@sha256:def", "ghcr.io", "o/i", "sha256:def"},
	}
	for _, c := range cases {
		got, err := ParseRef(c.in)
		if err != nil {
			t.Errorf("ParseRef(%q) error: %v", c.in, err)
			continue
		}
		if got.registry != c.registry || got.repo != c.repo || got.ref != c.ref {
			t.Errorf("ParseRef(%q) = %+v, want {%s %s %s}", c.in, got, c.registry, c.repo, c.ref)
		}
	}
}

func TestParseRefErrors(t *testing.T) {
	for _, in := range []string{"", "   ", ":tag"} {
		if _, err := ParseRef(in); err == nil {
			t.Errorf("ParseRef(%q) expected error, got nil", in)
		}
	}
}

func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"Nginx":                           "nginx",
		"NGINX:latest":                    "nginx:latest",
		"GHCR.io/Reeywhaar/Backio:LATEST": "ghcr.io/reeywhaar/backio:LATEST", // tag case preserved
		"library/Redis":                   "library/redis",
		"Foo@SHA256:ABC":                  "foo@SHA256:ABC", // digest preserved verbatim
		"  Alpine:3.20  ":                 "alpine:3.20",
	}
	for in, want := range cases {
		if got := NormalizeName(in); got != want {
			t.Errorf("NormalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParsePlatformAndString(t *testing.T) {
	p := ParsePlatform("linux/arm/v7")
	if p.os != "linux" || p.arch != "arm" || p.variant != "v7" {
		t.Fatalf("ParsePlatform = %+v", p)
	}
	if p.String() != "linux/arm/v7" {
		t.Errorf("String() = %q", p.String())
	}
	if ParsePlatform("linux/amd64").String() != "linux/amd64" {
		t.Errorf("variant-less roundtrip failed")
	}
}

func TestParseChallenge(t *testing.T) {
	c := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"`)
	if c.realm != "https://auth.docker.io/token" {
		t.Errorf("realm = %q", c.realm)
	}
	if c.service != "registry.docker.io" {
		t.Errorf("service = %q", c.service)
	}
}

func TestIntersectPlatforms(t *testing.T) {
	a := []Platform{{"linux", "amd64", ""}, {"linux", "arm64", ""}, {"windows", "amd64", ""}}
	b := []Platform{{"linux", "arm64", ""}, {"linux", "amd64", ""}}
	got := intersectPlatforms(a, b)
	if len(got) != 2 || got[0].String() != "linux/amd64" || got[1].String() != "linux/arm64" {
		t.Errorf("intersect = %v", got)
	}
}

func TestPickPlatform(t *testing.T) {
	common := []Platform{{"linux", "arm64", ""}, {"linux", "amd64", ""}}
	if PickPlatform(common, "linux/arm64").String() != "linux/arm64" {
		t.Error("should honor selected")
	}
	if PickPlatform(common, "").String() != "linux/amd64" {
		t.Error("should default to linux/amd64")
	}
	if PickPlatform(common, "bogus/x").String() != "linux/amd64" {
		t.Error("unknown selection should fall back to linux/amd64")
	}
	onlyArm := []Platform{{"linux", "arm64", ""}}
	if PickPlatform(onlyArm, "").String() != "linux/arm64" {
		t.Error("should fall back to first when no amd64")
	}
}
