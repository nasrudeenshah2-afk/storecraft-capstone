// Package metricstest parses and validates Prometheus text exposition output
// (format 0.0.4) so tests can check what a /metrics endpoint really emits.
package metricstest

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var nameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
var labelNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Sample is one metric line.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Scrape is a parsed, validated exposition.
type Scrape struct {
	Samples []Sample
	Types   map[string]string // family name -> counter|gauge|histogram|...
}

// Value returns the sample with exactly these labels.
func (s *Scrape) Value(name string, labels map[string]string) (float64, bool) {
	for _, sm := range s.Samples {
		if sm.Name == name && sameLabels(sm.Labels, labels) {
			return sm.Value, true
		}
	}
	return 0, false
}

// MustValue is Value that returns 0 when the series does not exist.
func (s *Scrape) MustValue(name string, labels map[string]string) float64 {
	v, _ := s.Value(name, labels)
	return v
}

// Sum adds up every sample with this name (all label sets).
func (s *Scrape) Sum(name string) float64 {
	var t float64
	for _, sm := range s.Samples {
		if sm.Name == name {
			t += sm.Value
		}
	}
	return t
}

func sameLabels(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// Parse validates text and returns its samples. It checks line syntax, label
// escaping, HELP/TYPE ordering, duplicate series, non-negative counters, and
// histogram invariants (ascending le, cumulative counts, +Inf == _count,
// _sum present).
func Parse(text string) (*Scrape, error) {
	sc := &Scrape{Types: map[string]string{}}
	helped := map[string]bool{}
	seen := map[string]bool{}

	for n, line := range strings.Split(text, "\n") {
		lineNo := n + 1
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			fields := strings.SplitN(line, " ", 4)
			if len(fields) >= 3 && fields[1] == "HELP" {
				if !nameRE.MatchString(fields[2]) {
					return nil, fmt.Errorf("line %d: bad metric name in HELP", lineNo)
				}
				helped[fields[2]] = true
			} else if len(fields) == 4 && fields[1] == "TYPE" {
				if !nameRE.MatchString(fields[2]) {
					return nil, fmt.Errorf("line %d: bad metric name in TYPE", lineNo)
				}
				if _, dup := sc.Types[fields[2]]; dup {
					return nil, fmt.Errorf("line %d: duplicate TYPE for %s", lineNo, fields[2])
				}
				switch fields[3] {
				case "counter", "gauge", "histogram", "summary", "untyped":
				default:
					return nil, fmt.Errorf("line %d: unknown type %q", lineNo, fields[3])
				}
				sc.Types[fields[2]] = fields[3]
			}
			continue
		}
		sm, err := parseSample(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w (%q)", lineNo, err, line)
		}
		fam := family(sm.Name, sc.Types)
		if _, ok := sc.Types[fam]; !ok {
			return nil, fmt.Errorf("line %d: sample %s before its TYPE", lineNo, sm.Name)
		}
		if !helped[fam] {
			return nil, fmt.Errorf("line %d: sample %s before its HELP", lineNo, sm.Name)
		}
		if sc.Types[fam] == "counter" && (sm.Value < 0 || math.IsNaN(sm.Value)) {
			return nil, fmt.Errorf("line %d: counter %s has invalid value %v", lineNo, sm.Name, sm.Value)
		}
		id := sm.Name + labelID(sm.Labels)
		if seen[id] {
			return nil, fmt.Errorf("line %d: duplicate series %s", lineNo, id)
		}
		seen[id] = true
		sc.Samples = append(sc.Samples, sm)
	}
	if err := checkHistograms(sc); err != nil {
		return nil, err
	}
	return sc, nil
}

// family maps name_bucket/_sum/_count back to its histogram family name.
func family(name string, types map[string]string) string {
	for _, suf := range []string{"_bucket", "_sum", "_count"} {
		if base, ok := strings.CutSuffix(name, suf); ok && types[base] == "histogram" {
			return base
		}
	}
	return name
}

func labelID(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%q,", k, l[k])
	}
	b.WriteByte('}')
	return b.String()
}

func parseSample(line string) (Sample, error) {
	sm := Sample{Labels: map[string]string{}}
	i := 0
	for i < len(line) && line[i] != '{' && line[i] != ' ' {
		i++
	}
	sm.Name = line[:i]
	if !nameRE.MatchString(sm.Name) {
		return sm, fmt.Errorf("bad metric name %q", sm.Name)
	}
	if i < len(line) && line[i] == '{' {
		i++
		for {
			if i >= len(line) {
				return sm, fmt.Errorf("unterminated label set")
			}
			if line[i] == '}' {
				i++
				break
			}
			j := i
			for j < len(line) && line[j] != '=' {
				j++
			}
			if j+1 >= len(line) || line[j+1] != '"' {
				return sm, fmt.Errorf("label value must be quoted")
			}
			ln := line[i:j]
			if !labelNameRE.MatchString(ln) {
				return sm, fmt.Errorf("bad label name %q", ln)
			}
			j += 2
			var val strings.Builder
			for {
				if j >= len(line) {
					return sm, fmt.Errorf("unterminated label value")
				}
				c := line[j]
				if c == '\\' {
					if j+1 >= len(line) {
						return sm, fmt.Errorf("dangling backslash")
					}
					switch line[j+1] {
					case '\\':
						val.WriteByte('\\')
					case '"':
						val.WriteByte('"')
					case 'n':
						val.WriteByte('\n')
					default:
						return sm, fmt.Errorf("bad escape \\%c", line[j+1])
					}
					j += 2
					continue
				}
				if c == '"' {
					j++
					break
				}
				val.WriteByte(c)
				j++
			}
			if _, dup := sm.Labels[ln]; dup {
				return sm, fmt.Errorf("duplicate label %q", ln)
			}
			sm.Labels[ln] = val.String()
			i = j
			if i < len(line) && line[i] == ',' {
				i++
			}
		}
	}
	rest := strings.TrimPrefix(line[i:], " ")
	if rest == "" || strings.ContainsAny(rest, " \t") {
		return sm, fmt.Errorf("expected exactly one value after the name/labels")
	}
	v, err := strconv.ParseFloat(rest, 64)
	if err != nil {
		return sm, fmt.Errorf("bad value %q", rest)
	}
	sm.Value = v
	return sm, nil
}

func checkHistograms(sc *Scrape) error {
	type series struct {
		buckets    []Sample // in output order
		sum, count *float64
	}
	byFamily := map[string]map[string]*series{}
	get := func(fam string, labels map[string]string) *series {
		l := map[string]string{}
		for k, v := range labels {
			if k != "le" {
				l[k] = v
			}
		}
		id := labelID(l)
		if byFamily[fam] == nil {
			byFamily[fam] = map[string]*series{}
		}
		if byFamily[fam][id] == nil {
			byFamily[fam][id] = &series{}
		}
		return byFamily[fam][id]
	}
	for _, sm := range sc.Samples {
		fam := family(sm.Name, sc.Types)
		if sc.Types[fam] != "histogram" {
			continue
		}
		s := get(fam, sm.Labels)
		v := sm.Value
		switch {
		case strings.HasSuffix(sm.Name, "_bucket"):
			if _, ok := sm.Labels["le"]; !ok {
				return fmt.Errorf("%s: bucket without le label", sm.Name)
			}
			s.buckets = append(s.buckets, sm)
		case strings.HasSuffix(sm.Name, "_sum"):
			s.sum = &v
		case strings.HasSuffix(sm.Name, "_count"):
			s.count = &v
		}
	}
	for fam, all := range byFamily {
		for id, s := range all {
			where := fam + id
			if len(s.buckets) == 0 || s.sum == nil || s.count == nil {
				return fmt.Errorf("%s: histogram needs buckets, _sum and _count", where)
			}
			prevLE := math.Inf(-1)
			prevCount := -1.0
			for _, b := range s.buckets {
				le, err := strconv.ParseFloat(b.Labels["le"], 64)
				if err != nil {
					return fmt.Errorf("%s: bad le %q", where, b.Labels["le"])
				}
				if le <= prevLE {
					return fmt.Errorf("%s: le not strictly ascending (%v after %v)", where, le, prevLE)
				}
				if b.Value < prevCount {
					return fmt.Errorf("%s: buckets not cumulative (%v after %v)", where, b.Value, prevCount)
				}
				prevLE, prevCount = le, b.Value
			}
			if !math.IsInf(prevLE, 1) {
				return fmt.Errorf("%s: last bucket must be le=\"+Inf\"", where)
			}
			if prevCount != *s.count {
				return fmt.Errorf("%s: +Inf bucket %v != _count %v", where, prevCount, *s.count)
			}
		}
	}
	return nil
}
