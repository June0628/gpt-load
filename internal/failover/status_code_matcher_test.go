package failover

import "testing"

func TestParseStatusCodeMatcherEmptySpec(t *testing.T) {
	for _, spec := range []string{"", "   ", ",, ,", "\n"} {
		m, err := ParseStatusCodeMatcher(spec)
		if err != nil {
			t.Fatalf("ParseStatusCodeMatcher(%q) error: %v", spec, err)
		}
		if !m.IsEmpty() {
			t.Errorf("ParseStatusCodeMatcher(%q) should be empty", spec)
		}
		if m.Match(500) {
			t.Errorf("empty matcher matched 500 for spec %q", spec)
		}
	}
}

func TestZeroValueMatcherMatchesNothing(t *testing.T) {
	var m StatusCodeMatcher
	if !m.IsEmpty() || m.Match(200) {
		t.Error("zero value matcher should match nothing")
	}
}

func TestParseStatusCodeMatcherMatching(t *testing.T) {
	m, err := ParseStatusCodeMatcher(" 404 , 429, 500-599 ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.IsEmpty() {
		t.Fatal("matcher should not be empty")
	}

	matching := []int{404, 429, 500, 550, 599}
	for _, code := range matching {
		if !m.Match(code) {
			t.Errorf("Match(%d) = false, want true", code)
		}
	}
	notMatching := []int{100, 200, 403, 405, 428, 430, 499, 600, 999}
	for _, code := range notMatching {
		if m.Match(code) {
			t.Errorf("Match(%d) = true, want false", code)
		}
	}
}

func TestParseStatusCodeMatcherAcceptsNewlinesAndSpacedRanges(t *testing.T) {
	m, err := ParseStatusCodeMatcher("404\n250 - 260")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, code := range []int{250, 255, 260, 404} {
		if !m.Match(code) {
			t.Errorf("Match(%d) = false, want true", code)
		}
	}
	if m.Match(249) || m.Match(261) {
		t.Error("range boundaries are not exclusive as expected")
	}
}

func TestParseStatusCodeMatcherMergesRanges(t *testing.T) {
	// 500-505 and 506-510 are adjacent, 503 overlaps: all merge into 500-510.
	m, err := ParseStatusCodeMatcher("506-510,500-505,503")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := len(m.ranges); got != 1 {
		t.Fatalf("expected ranges to merge into 1, got %d: %+v", got, m.ranges)
	}
	if m.ranges[0] != (StatusCodeRange{Start: 500, End: 510}) {
		t.Errorf("merged range = %+v, want {500 510}", m.ranges[0])
	}
	for code := 500; code <= 510; code++ {
		if !m.Match(code) {
			t.Errorf("Match(%d) = false, want true", code)
		}
	}
	if m.Match(499) || m.Match(511) {
		t.Error("merged range expanded beyond its bounds")
	}
}

func TestParseStatusCodeMatcherKeepsDisjointRanges(t *testing.T) {
	m, err := ParseStatusCodeMatcher("400-401,500-501")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := len(m.ranges); got != 2 {
		t.Fatalf("expected 2 ranges, got %d: %+v", got, m.ranges)
	}
	if m.Match(450) {
		t.Error("Match(450) = true, want false")
	}
}

func TestParseStatusCodeMatcherErrors(t *testing.T) {
	invalid := []string{
		"abc",
		"4o4",
		"500-",
		"-500",
		"500-501-502",
		"599-500",
		"99",
		"1000",
		"99-200",
		"200-1000",
		"50x-599",
		"500-59x",
	}

	for _, spec := range invalid {
		t.Run(spec, func(t *testing.T) {
			m, err := ParseStatusCodeMatcher(spec)
			if err == nil {
				t.Fatalf("ParseStatusCodeMatcher(%q) expected error, got matcher %+v", spec, m)
			}
			if !m.IsEmpty() {
				t.Errorf("matcher returned alongside error should be empty: %+v", m)
			}
		})
	}
}
