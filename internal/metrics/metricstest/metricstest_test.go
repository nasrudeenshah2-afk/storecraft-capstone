package metricstest

import (
	"strings"
	"testing"
)

const goodHistogram = `# HELP h A histogram.
# TYPE h histogram
h_bucket{r="/a",le="1"} 1
h_bucket{r="/a",le="+Inf"} 2
h_sum{r="/a"} 3.5
h_count{r="/a"} 2
# HELP c_total A counter.
# TYPE c_total counter
c_total 5
`

func TestParseAcceptsValidText(t *testing.T) {
	sc, err := Parse(goodHistogram)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := sc.Value("c_total", nil); !ok || v != 5 {
		t.Fatalf("c_total = %v %v", v, ok)
	}
	if v := sc.MustValue("h_bucket", map[string]string{"r": "/a", "le": "+Inf"}); v != 2 {
		t.Fatalf("+Inf bucket = %v", v)
	}
	if sc.Sum("h_count") != 2 {
		t.Fatal("Sum wrong")
	}
}

func TestParseRejectsBrokenText(t *testing.T) {
	cases := map[string]string{
		"sample before TYPE":      "# HELP x h\nx 1\n",
		"sample before HELP":      "# TYPE x counter\nx 1\n",
		"bad name":                "# HELP 1x h\n# TYPE 1x counter\n1x 1\n",
		"bad value":               "# HELP x h\n# TYPE x counter\nx abc\n",
		"two values":              "# HELP x h\n# TYPE x counter\nx 1 2\n",
		"unquoted label":          "# HELP x h\n# TYPE x counter\nx{a=b} 1\n",
		"unterminated labels":     "# HELP x h\n# TYPE x counter\nx{a=\"b\" 1\n",
		"bad escape":              "# HELP x h\n# TYPE x counter\nx{a=\"\\q\"} 1\n",
		"duplicate series":        "# HELP x h\n# TYPE x counter\nx 1\nx 2\n",
		"duplicate TYPE":          "# HELP x h\n# TYPE x counter\n# TYPE x counter\nx 1\n",
		"negative counter":        "# HELP x h\n# TYPE x counter\nx -1\n",
		"unknown type":            "# HELP x h\n# TYPE x bogus\nx 1\n",
		"non-cumulative buckets":  strings.Replace(goodHistogram, `h_bucket{r="/a",le="+Inf"} 2`, `h_bucket{r="/a",le="+Inf"} 0`, 1),
		"+Inf != count":           strings.Replace(goodHistogram, "h_count{r=\"/a\"} 2", "h_count{r=\"/a\"} 9", 1),
		"missing +Inf":            strings.Replace(goodHistogram, `h_bucket{r="/a",le="+Inf"} 2`+"\n", "", 1),
		"missing sum":             strings.Replace(goodHistogram, "h_sum{r=\"/a\"} 3.5\n", "", 1),
		"le not ascending":        strings.Replace(goodHistogram, `h_bucket{r="/a",le="1"} 1`+"\n", `h_bucket{r="/a",le="1"} 1`+"\n"+`h_bucket{r="/a",le="0.5"} 1`+"\n", 1),
		"bucket without le label": strings.Replace(goodHistogram, `h_bucket{r="/a",le="1"} 1`, `h_bucket{r="/a"} 1`, 1),
	}
	for name, text := range cases {
		if _, err := Parse(text); err == nil {
			t.Errorf("%s: Parse accepted invalid text:\n%s", name, text)
		}
	}
}
