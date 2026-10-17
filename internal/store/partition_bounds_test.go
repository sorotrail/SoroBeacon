package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestMonthStartBoundaryCases covers the classic off-by-one traps in month
// arithmetic: exactly on midnight, one microsecond before, across timezones,
// and at year boundaries.
func TestMonthStartBoundaryCases(t *testing.T) {
	tests := []struct {
		name     string
		input    time.Time
		expected time.Time
	}{
		{
			name:     "exactly on midnight first of month",
			input:    time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			expected: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name:     "one microsecond before midnight",
			input:    time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Add(-time.Microsecond),
			expected: time.Date(2023, 12, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name:     "mid-month timestamp",
			input:    time.Date(2024, 6, 15, 12, 30, 45, 123456789, time.UTC),
			expected: time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name:     "non-UTC timezone is converted",
			input:    time.Date(2024, 3, 15, 0, 0, 0, 0, time.FixedZone("EST", -5*3600)),
			expected: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC), // Converts to UTC then truncates
		},
		{
			name:     "December wraps to next year correctly",
			input:    time.Date(2024, 12, 31, 23, 59, 59, 999999999, time.UTC),
			expected: time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := monthStart(tt.input)
			assert.Equal(t, tt.expected, got)
		})
	}
}

// TestAlertPartitionNameRoundTrip proves that alertPartitionName and
// alertPartitionBounds are inverses for a range of months, including the
// tricky ones where month arithmetic breaks.
func TestAlertPartitionNameRoundTrip(t *testing.T) {
	testMonths := []time.Time{
		time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC),   // January
		time.Date(2024, 2, 15, 0, 0, 0, 0, time.UTC),   // February (leap year)
		time.Date(2024, 12, 15, 0, 0, 0, 0, time.UTC),  // December
		time.Date(2023, 2, 15, 0, 0, 0, 0, time.UTC),   // February (non-leap year)
		time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC),    // July (DST in many zones)
		time.Date(2024, 3, 10, 0, 0, 0, 0, time.UTC),   // March (US DST transition)
		time.Date(2024, 11, 3, 0, 0, 0, 0, time.UTC),   // November (US DST ends)
	}

	for _, ts := range testMonths {
		t.Run(ts.Format("2006-01"), func(t *testing.T) {
			name := alertPartitionName(ts)
			from, to, ok := alertPartitionBounds(name)
			assert.True(t, ok, "partition name %q should parse", name)

			expectedFrom := time.Date(ts.Year(), ts.Month(), 1, 0, 0, 0, 0, time.UTC)
			expectedTo := expectedFrom.AddDate(0, 1, 0)

			assert.Equal(t, expectedFrom, from)
			assert.Equal(t, expectedTo, to)

			// Verify half-open interval property
			assert.True(t, from.Before(to))
			assert.Equal(t, 1, from.Day(), "from should be first of month")
			assert.Equal(t, 0, from.Hour(), "from should be midnight")
			assert.Equal(t, 1, to.Day(), "to should be first of next month")
			assert.Equal(t, 0, to.Hour(), "to should be midnight")
		})
	}
}

// TestAlertPartitionBoundsRejectsInvalidNames ensures that unexpected catalog
// entries or malformed names are safely ignored rather than causing a drop.
func TestAlertPartitionBoundsRejectsInvalidNames(t *testing.T) {
	invalidNames := []string{
		alertDefaultPartition,         // "alerts_default"
		"alerts_2024",                 // missing month
		"alerts_2024_1",               // single-digit month
		"alerts_2024_13",              // invalid month number
		"alerts_2024_00",              // zero month
		"alerts_other",                // wrong format
		"",                            // empty
		"some_random_table",           // unrelated table
	}

	for _, name := range invalidNames {
		t.Run(name, func(t *testing.T) {
			from, to, ok := alertPartitionBounds(name)
			assert.False(t, ok, "name %q should not parse as a monthly partition", name)
			assert.Zero(t, from)
			assert.Zero(t, to)
		})
	}
}

// TestAlertPartitionNameConsistency verifies that any timestamp within a
// month maps to the same partition name, regardless of day, hour, or timezone.
func TestAlertPartitionNameConsistency(t *testing.T) {
	base := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	expectedName := "alerts_2024_06"

	variants := []time.Time{
		base,                                          // first instant
		base.Add(29 * 24 * time.Hour),                 // end of month
		base.AddDate(0, 0, 15).Add(12*time.Hour + 30*time.Minute), // mid-month afternoon
		base.In(time.FixedZone("PST", -8*3600)),       // different timezone
	}

	for _, ts := range variants {
		t.Run(ts.String(), func(t *testing.T) {
			got := alertPartitionName(ts)
			assert.Equal(t, expectedName, got)
		})
	}
}
