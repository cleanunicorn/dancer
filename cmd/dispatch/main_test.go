package main

import (
	"testing"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/config"
)

func TestDriversIncludeCodex(t *testing.T) {
	d := drivers(&config.Config{})
	for _, kind := range []agent.Kind{agent.KindClaude, agent.KindCodex} {
		if _, ok := d[kind]; !ok {
			t.Errorf("drivers missing %q", kind)
		}
	}
}
