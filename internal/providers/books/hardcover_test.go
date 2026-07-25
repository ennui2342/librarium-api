// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package books

import "testing"

func TestNormalizeHardcoverVolumeDate(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"1965-06-01", "1965-06-01"},
		// Same failure mode as the ISFDB series-volumes bug this guards
		// against: a bare "YYYY-MM"/"YYYY" must not reach the caller
		// unchanged — series_volumes.release_date is a raw `::date` SQL
		// cast downstream and rejects it outright.
		{"1965-06", "1965-06-01"},
		{"1965", "1965-01-01"},
		{"not a date", ""},
	}
	for _, tc := range cases {
		if got := normalizeHardcoverVolumeDate(tc.in); got != tc.want {
			t.Errorf("normalizeHardcoverVolumeDate(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
