package api

// B10: proves the alert rule of docs/capstone/alert.md fires in a forced
// scenario, and only then. The rule (metric, window, threshold, for) is read
// from that document so the test and the document cannot drift apart. No
// Prometheus server is available here, so the rule is evaluated by the small
// evaluator below over real scrapes of /metrics (see the limitation in alert.md).

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"
)

type alertRule struct {
	metric    string
	window    time.Duration
	threshold float64
	forDur    time.Duration
}

func loadAlertRule(t *testing.T) alertRule {
	t.Helper()
	b, err := os.ReadFile("../../docs/capstone/alert.md")
	if err != nil {
		t.Fatal(err)
	}
	expr := regexp.MustCompile(`expr: sum\(rate\((\w+)\[(\d+)m\]\)\) > ([0-9.]+)`).FindStringSubmatch(string(b))
	forM := regexp.MustCompile(`(?m)^\s+for: (\d+)m$`).FindStringSubmatch(string(b))
	if expr == nil || forM == nil {
		t.Fatal("alert.md: could not find the QuotaRejectionsHigh expr / for lines")
	}
	win, _ := strconv.Atoi(expr[2])
	thr, _ := strconv.ParseFloat(expr[3], 64)
	forMin, _ := strconv.Atoi(forM[1])
	return alertRule{metric: expr[1], window: time.Duration(win) * time.Minute, threshold: thr, forDur: time.Duration(forMin) * time.Minute}
}

// fires evaluates the rule once per step over counter readings taken every
// step (readings[i] = the counter, summed over its labels, at time i*step).
// rate = increase over the window / window seconds; the alert fires once the
// condition has held at every evaluation for the whole for-duration. It
// returns the step at which it fired, or -1.
func (r alertRule) fires(readings []float64, step time.Duration) int {
	w := int(r.window / step)
	var pendingSince = -1
	for i := w; i < len(readings); i++ {
		rate := (readings[i] - readings[i-w]) / r.window.Seconds()
		if rate <= r.threshold {
			pendingSince = -1
			continue
		}
		if pendingSince < 0 {
			pendingSince = i
		}
		if time.Duration(i-pendingSince)*step >= r.forDur {
			return i
		}
	}
	return -1
}

// rejectionScenario runs a real server where every new-key write is rejected
// (enforcement on, max_keys 0) and, for each simulated minute, sends perMinute[m]
// such writes and then scrapes the counter. It returns the readings.
func rejectionScenario(t *testing.T, rule alertRule, perMinute []int) []float64 {
	t.Helper()
	e := setupWith(t, 300*time.Millisecond)
	e.setEnforcement(true)
	e.setQuota(1, lim(0), nil)
	readings := []float64{0}
	n := 0
	for m, count := range perMinute {
		rs := concurrently(count, func(i int) result {
			return send(e.url, "PUT", fmt.Sprintf("/v1/keys/m%d-%d", m, i), e.tokenA, `{"value":1}`, nil)
		})
		for _, r := range rs {
			if r.err != nil || r.status != 403 || r.code != "quota_exceeded" {
				t.Fatalf("forced rejection got %+v", r)
			}
		}
		n += count
		_, sc := e.scrape()
		readings = append(readings, sc.Sum(rule.metric))
	}
	if got := readings[len(readings)-1]; got != float64(n) {
		t.Fatalf("%s = %v after %d rejections", rule.metric, got, n)
	}
	return readings
}

func repeat(n, v int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// Not named "Quota...": it is not part of the -count=20 concurrency gate.
func TestAlertRuleFiresInForcedScenario(t *testing.T) {
	rule := loadAlertRule(t)
	if rule.metric != "storecraft_quota_rejections_total" || rule.threshold != 1 || rule.window != 5*time.Minute || rule.forDur != 10*time.Minute {
		t.Fatalf("alert.md rule changed (%+v): update this test's scenarios", rule)
	}
	const step = time.Minute
	minutes := int((rule.window+rule.forDur)/step) + 1 // long enough to fire

	// Forced: 1.5 rejections per second, sustained -> fires exactly when the
	// condition has held for 10 minutes after the first full 5-minute window.
	sustained := rejectionScenario(t, rule, repeat(minutes, 90))
	if at := rule.fires(sustained, step); at != int(rule.window/step)+int(rule.forDur/step) {
		t.Fatalf("sustained 1.5/s: fired at minute %d, want %d", at, int((rule.window+rule.forDur)/step))
	}

	// The negative cases check the evaluator on given counter readings (the
	// counter itself is proven above and in the next test).
	cumulative := func(perMinute []int) []float64 {
		out := []float64{0}
		for _, c := range perMinute {
			out = append(out, out[len(out)-1]+float64(c))
		}
		return out
	}
	// Exactly at the threshold (1 per second) never fires: the rule is "> 1".
	if at := rule.fires(cumulative(repeat(minutes, 60)), step); at != -1 {
		t.Fatalf("1/s fired at minute %d; the threshold is exclusive", at)
	}
	// One big burst (600 in one minute), then quiet: above the threshold for
	// only 5 evaluations, shorter than the for-duration -> does not fire.
	if at := rule.fires(cumulative(append([]int{600}, repeat(minutes-1, 0)...)), step); at != -1 {
		t.Fatalf("a single burst fired at minute %d", at)
	}
	// Sustained just above the threshold for less than the for-duration, then
	// quiet: does not fire; the same rate for long enough does.
	if at := rule.fires(cumulative(append(repeat(9, 61), repeat(minutes, 0)...)), step); at != -1 {
		t.Fatalf("9 minutes above the threshold fired at minute %d", at)
	}
	if at := rule.fires(cumulative(repeat(minutes, 61)), step); at == -1 {
		t.Fatal("61 per minute (1.02/s) sustained did not fire")
	}
}

// The counter behind the alert counts 403 quota_exceeded responses and nothing else.
func TestQuotaRejectionCounterCountsOnlyQuotaRejections(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	e.put(e.tokenA, "k", `{"value":1}`)
	e.expect(e.do("PUT", "/v1/keys/k", e.tokenA, `{"value":2}`, map[string]string{"If-Match": `"9"`}), 412, "precondition_failed")
	e.expect(e.do("DELETE", "/v1/keys/missing", e.tokenA, "", nil), 404, "not_found")
	e.expect(e.do("PUT", "/v1/keys/k", "", `{"value":2}`, nil), 401, "unauthorized")
	e.expect(e.do("PUT", "/v1/keys/k", e.tokenA, `nope`, nil), 400, "invalid_request")
	if _, sc := e.scrape(); sc.Sum("storecraft_quota_rejections_total") != 0 {
		t.Fatal("non-quota errors were counted as quota rejections")
	}
	e.setEnforcement(true)
	e.setQuota(1, lim(1), nil)
	e.requireQuotaExceeded(e.do("PUT", "/v1/keys/k2", e.tokenA, `{"value":1}`, nil))
	if _, sc := e.scrape(); sc.Sum("storecraft_quota_rejections_total") != 1 {
		t.Fatal("a quota rejection was not counted exactly once")
	}
}
