package topic

import "testing"

func TestNormalizeName(t *testing.T) {
	existing := []Topic{
		{Name: "Collagen Supplement Comparison", Status: "active"},
		{Name: "plants", Status: "active"},
		{Name: "old-pruned-topic", Status: "pruned"},
		{Name: "A2 Dairy Tolerance", Status: "active"},
		{Name: "Plant Watering", Status: "active"},
		{Name: "Cashew Allergy", Status: "active"},
	}
	cases := []struct {
		proposed     string
		wantCanon    string
		wantMatched  bool
	}{
		// Same first word + token overlap → canonical match
		{"Collagen supplements", "Collagen Supplement Comparison", true},
		{"collagen", "Collagen Supplement Comparison", true},
		{"Collagen", "Collagen Supplement Comparison", true},

		// Exact case-insensitive match
		{"PLANTS", "plants", true},

		// Different first word → no match
		{"meals", "meals", false},
		{"shopping", "shopping", false},

		// Empty proposed
		{"", "", false},

		// Pruned topic should not match
		{"old", "old", false},

		// More-specific variant of an existing simple topic → normalize
		// up to the simpler canonical form (avoids thread fragmentation).
		{"plants exterior landscape", "plants", true},

		// Cycle 144: positionally-divergent shared substantive token.
		// "Dairy Allergy Management" shares "dairy" (pos 0) with existing
		// "A2 Dairy Tolerance" (pos 1). Should merge — that's the bug
		// cycle 101 caught in production.
		{"Dairy Allergy Management", "A2 Dairy Tolerance", true},

		// Plant Watering Schedule vs existing Plant Watering — same
		// substantive anchors. Should merge.
		{"Plant Watering Schedule", "Plant Watering", true},
		{"Watering Schedule", "Plant Watering", true},

		// Cycle 144 false-positive guard: names sharing only a generic
		// suffix should NOT merge. "Cashew Allergy" + "Dairy Allergy"
		// share "allergy" — filtered as a stop-word — so no substantive
		// overlap remains.
		{"Dairy Allergy", "A2 Dairy Tolerance", true},   // "dairy" still anchors
		{"Soy Allergy", "Soy Allergy", false},           // genuinely new
	}
	for _, c := range cases {
		t.Run(c.proposed, func(t *testing.T) {
			got, matched := NormalizeName(c.proposed, existing)
			if got != c.wantCanon || matched != c.wantMatched {
				t.Errorf("NormalizeName(%q) = (%q, %v), want (%q, %v)",
					c.proposed, got, matched, c.wantCanon, c.wantMatched)
			}
		})
	}
}

func TestNameTokens(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"Collagen Supplement Comparison", []string{"collagen", "supplement"}},  // "comparison" stop
		{"plants and meals", []string{"plants", "meals"}},                       // "and" stop
		{"Brazilian wood tree", []string{"brazilian", "wood", "tree"}},
		{"the-discussion", []string{}},                                          // both stop
	}
	for _, c := range cases {
		got := nameTokens(c.in)
		if len(got) != len(c.want) {
			t.Errorf("nameTokens(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("nameTokens(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}
