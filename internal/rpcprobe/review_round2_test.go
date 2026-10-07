package rpcprobe

import (
	"net/url"
	"strings"
	"testing"
)

func TestRound2ReviewSecretBoundaries(t *testing.T) {
	for name, input := range map[string]string{
		"literal slash before apostrophe": `ov --password '\'\''00 FAKE_SECRET_SUFFIX omega' failed`,
		"input process substitution":      `OV_API_KEY=<(printf 'alpha FAKE_SECRET_SUFFIX omega') failed`,
		"output process substitution":     `OV_API_KEY=>(printf 'alpha FAKE_SECRET_SUFFIX omega') failed`,
		"assignment substitution":         `OV_API_KEY=$(printf 'alpha FAKE_SECRET_SUFFIX omega') failed`,
		"nested substitution":             `OV_API_KEY=$(printf '%s' $(printf 'alpha FAKE_SECRET_SUFFIX omega')) failed`,
		"parameter expansion":             `OV_API_KEY=${unset:-alpha FAKE_SECRET_SUFFIX omega} failed`,
		"structured shell suffix":         `ov --password [alpha]FAKE_SECRET_SUFFIX failed`,
		"brace shell suffix":              `ov --password {alpha}FAKE_SECRET_SUFFIX failed`,
		"shell comma":                     `ov --password alpha,FAKE_SECRET_SUFFIX failed`,
		"shell bracket":                   `ov --password alpha]FAKE_SECRET_SUFFIX failed`,
		"shell brace":                     `ov --password alpha}FAKE_SECRET_SUFFIX failed`,
		"assignment comma":                `OV_API_KEY=alpha,FAKE_SECRET_SUFFIX failed`,
		"ANSI C flag":                     `ov --password $'alpha FAKE_SECRET_SUFFIX omega' failed`,
		"ANSI C assignment":               `OV_API_KEY=$'alpha FAKE_SECRET_SUFFIX omega' failed`,
		"shell escaped flag":              `ov --password alpha\ FAKE_SECRET_SUFFIX\ omega failed`,
		"concatenated quotes":             `ov --password 'alpha '"FAKE_SECRET_SUFFIX"\ omega failed`,
		"dollar double quotes":            `ov --password $"alpha FAKE_SECRET_SUFFIX omega" failed`,
		"unterminated concatenation":      `ov --password alpha' FAKE_SECRET_SUFFIX omega`,
		"quoted bearer":                   `Authorization: Bearer "alpha FAKE_SECRET_SUFFIX omega" failed`,
		"username userinfo":               `dial https://FAKE_SECRET_SUFFIX@db failed`,
		"empty password userinfo":         `dial https://FAKE_SECRET_SUFFIX:@db failed`,
		"empty username userinfo":         `dial https://:FAKE_SECRET_SUFFIX@db failed`,
	} {
		t.Run(name, func(t *testing.T) {
			got := exchangeToolError(t, []any{text(input)}).Error()
			if strings.Contains(got, "FAKE_SECRET_SUFFIX") || strings.Contains(got, "omega") {
				t.Fatalf("secret reached error: %q", got)
			}
			if strings.HasSuffix(input, " failed") && !strings.HasSuffix(got, " failed") {
				t.Fatalf("lost useful suffix: %q", got)
			}
		})
	}
}

func TestRound2CredentialURLsAreValid(t *testing.T) {
	for _, input := range []string{"https://FAKE_SECRET_SUFFIX@db", "https://FAKE_SECRET_SUFFIX:@db", "https://:FAKE_SECRET_SUFFIX@db"} {
		u, err := url.Parse(input)
		if err != nil || u.User == nil || u.Host != "db" || !strings.Contains(u.User.String(), "FAKE_SECRET_SUFFIX") {
			t.Fatalf("URL parse: %v %v", u, err)
		}
	}
}
