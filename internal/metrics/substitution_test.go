package metrics

import (
	"strconv"
	"testing"
	"time"
)

// substFamily reads the substitution family out of a scrape as label-tuple ->
// value, and reports whether the family appeared at all.
func substFamily(t *testing.T, q *Requests) (map[[3]string]float64, bool) {
	t.Helper()
	r := New(time.Now)
	r.Register(q)
	body := r.Gather(nil)
	if err := Validate(body); err != nil {
		t.Fatalf("the scrape does not validate: %v\n%s", err, body)
	}
	fams, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, body)
	}
	out := map[[3]string]float64{}
	found := false
	for _, f := range fams {
		if f.Name != "dorang_model_substitutions_total" {
			continue
		}
		found = true
		for _, s := range f.Samples {
			out[[3]string{s.Labels["provider"], s.Labels["deployment"], s.Labels["served"]}] += s.Value
		}
	}
	return out, found
}

// A healthy gateway emits no series here at all. Rule 3 of this package's doc:
// a family that would be uniformly zero is omitted rather than published,
// because a zero reads as "measured and fine" and an absence reads as "this has
// not happened", and only the second is true.
func TestSubstitutionFamilyIsAbsentUntilOneHappens(t *testing.T) {
	q := NewRequests(RequestsOptions{})
	q.Observe(Sample{Model: "m1", Provider: "p1", Deployment: "d1", Endpoint: "chat", Status: 200})

	if _, found := substFamily(t, q); found {
		t.Error("dorang_model_substitutions_total appears on honest traffic; it must not " +
			"exist until an upstream answers as a different model")
	}
}

func TestSubstitutionIsCountedByProviderDeploymentAndServed(t *testing.T) {
	q := NewRequests(RequestsOptions{})
	for i := 0; i < 3; i++ {
		q.Observe(Sample{
			Model: "m1", Provider: "zai", Deployment: "zai-glm-5.1",
			Endpoint: "chat", Status: 200, ServedModel: "glm-5.2",
		})
	}
	q.Observe(Sample{
		Model: "m2", Provider: "zai", Deployment: "zai-glm-4.5-air",
		Endpoint: "chat", Status: 200, ServedModel: "glm-4.7",
	})

	got, found := substFamily(t, q)
	if !found {
		t.Fatal("the family is missing after three substitutions")
	}
	if v := got[[3]string{"zai", "zai-glm-5.1", "glm-5.2"}]; v != 3 {
		t.Errorf("zai-glm-5.1 -> glm-5.2 = %v, want 3", v)
	}
	if v := got[[3]string{"zai", "zai-glm-4.5-air", "glm-4.7"}]; v != 1 {
		t.Errorf("zai-glm-4.5-air -> glm-4.7 = %v, want 1", v)
	}
}

// The cap is the backstop under the argument in [substKey], not a replacement
// for it: every dimension of the folded row carries the sentinel, because
// Validate requires one label set per family and a half-folded row cannot be
// joined against anything.
func TestSubstitutionSeriesFoldWholeRows(t *testing.T) {
	q := NewRequests(RequestsOptions{MaxSubstSeries: 4})
	for i := 0; i < 40; i++ {
		q.Observe(Sample{
			Model: "m", Provider: "p", Deployment: "d" + strconv.Itoa(i),
			Endpoint: "chat", Status: 200, ServedModel: "s" + strconv.Itoa(i),
		})
	}
	got, found := substFamily(t, q)
	if !found {
		t.Fatal("the family is missing")
	}
	over := [3]string{OverflowSentinel, OverflowSentinel, OverflowSentinel}
	if v := got[over]; v != 36 {
		t.Errorf("the folded row carries %v, want 36 (40 observations, cap 4)", v)
	}
	if len(got) != 5 {
		t.Errorf("%d series, want 5 (4 retained plus the folded row)", len(got))
	}
	if q.substs.f.Folds() == 0 {
		t.Error("the fold counter did not advance; a cap that does not count what it " +
			"refused is a cap nobody can see engage")
	}
}

// The whole family costs one string comparison on a request that was served
// honestly, which is every request on a healthy gateway.
func TestObserveWithNoSubstitutionDoesNotAllocate(t *testing.T) {
	q := NewRequests(RequestsOptions{})
	s := Sample{Model: "m1", Provider: "p1", Credential: "c1", Deployment: "d1",
		Endpoint: "chat", Status: 200}
	if n := testing.AllocsPerRun(500, func() { q.Observe(s) }); n != 0 {
		t.Errorf("Observe allocated %v times, want 0", n)
	}
	s.ServedModel = "glm-5.2"
	if n := testing.AllocsPerRun(500, func() { q.Observe(s) }); n != 0 {
		t.Errorf("Observe allocated %v times on the substituted path, want 0", n)
	}
}
