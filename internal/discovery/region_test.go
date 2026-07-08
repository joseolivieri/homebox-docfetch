package discovery

import "testing"

func TestIsCountrySpecificURL(t *testing.T) {
	cases := []struct {
		url, region string
		want        bool
	}{
		{"https://www.anker.com/products/a1289", "us", false},
		{"https://www.anker.com/nz/products/a1289", "us", true},
		{"https://www.anker.com/us/products/a1289", "us", false},
		{"https://support.anker.com/en-nz/warranty", "us", true},
		{"https://support.anker.com/en-us/warranty", "us", false},
		{"https://ankervietnam.vn/wp-content/x.webp", "us", true},
		{"https://lp.ankerjapan.com/manual.pdf", "us", true},
		{"https://anker.co.uk/warranty", "us", true},
		{"https://www.lg.com/us/support", "us", false},
		{"https://www.lg.com/de/support", "us", true},
		{"https://www.lg.com/de/support", "de", false},
		{"https://example.de/manual.pdf", "de", false},
		{"https://example.de/manual.pdf", "us", true},
		{"https://salesforce-knowledge-download.s3.us-west-2.amazonaws.com/x/en_US/x.pdf", "us", false},
		{"https://anything.example.com/whatever", "", false}, // region off
	}
	for _, c := range cases {
		if got := isCountrySpecificURL(c.url, c.region); got != c.want {
			t.Errorf("isCountrySpecificURL(%q, %q) = %v, want %v", c.url, c.region, got, c.want)
		}
	}
}
