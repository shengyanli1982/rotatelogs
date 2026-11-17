package rotator

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParsePatternAllTokens(t *testing.T) {
	assert.Equal(t, "2006-01-02T15:04:05", parsePattern("%Y-%m-%dT%H:%M:%S"))
}

func TestParsePatternDaily(t *testing.T) {
	assert.Equal(t, "app-2006-01-02.log", parsePattern("app-%Y-%m-%d.log"))
}

// TestParsePatternHourly 钉住 ST-01 验收断言（acceptance_criteria #3）。
func TestParsePatternHourly(t *testing.T) {
	assert.Equal(t, "app-2006-01-02T15.log", parsePattern("app-%Y-%m-%dT%H.log"))
}

func TestParsePatternNestedDirs(t *testing.T) {
	assert.Equal(t, "/var/log/2006/01/app-02.log", parsePattern("/var/log/%Y/%m/app-%d.log"))
}

func TestParsePatternNoPlaceholder(t *testing.T) {
	assert.Equal(t, "app.log", parsePattern("app.log"))
}

func TestParsePatternUnknownToken(t *testing.T) {
	assert.Equal(t, "app-%Q.log", parsePattern("app-%Q.log"))
}

func TestParsePatternLiteralSafe(t *testing.T) {
	assert.Equal(t, "2006-2006.log", parsePattern("2006-%Y.log"))
}

func TestHasTimePlaceholder(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		want    bool
	}{
		{"all tokens", "%Y-%m-%dT%H:%M:%S", true},
		{"daily", "app-%Y-%m-%d.log", true},
		{"nested dirs", "/var/log/%Y/%m/app-%d.log", true},
		{"no placeholder", "app.log", false},
		{"unknown token", "app-%Q.log", false},
		{"literal with token", "2006-%Y.log", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, HasTimePlaceholder(tt.pattern))
		})
	}
}
