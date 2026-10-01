package modeltests

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDatabaseTimeRejectsInvalidStoredValue(t *testing.T) {
	for _, tc := range []struct {
		name     string
		input    any
		expected string
		wantErr  bool
	}{
		{name: "millis", input: int64(1790841600123), expected: "2026-10-01T08:00:00.123Z"},
		{name: "utc_bytes", input: []byte("2026-10-01T08:00:00.123Z"), expected: "2026-10-01T08:00:00.123Z"},
		{name: "offset", input: "2026-10-01 09:00:00.123+01:00", expected: "2026-10-01T08:00:00.123Z"},
		{name: "mysql", input: "2026-10-01 08:00:00.123", expected: "2026-10-01T08:00:00.123Z"},
		{name: "malformed", input: "2026-02-30", wantErr: true},
		{name: "unsupported", input: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stamp model.DatabaseTime
			err := stamp.Scan(tc.input)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.expected, stamp.UTC().Format("2006-01-02T15:04:05.000Z"))
			assert.Equal(t, time.UTC, stamp.Location())
		})
	}
}
