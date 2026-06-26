package tmux_test

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/avinashjoshi/canopy/internal/tmux"
)

// stripPaneIndices replaces the trailing pane-index integer inside each
// tmux window_layout pane spec with a placeholder. The format is
// "checksum,WxH,X,Y[...pane specs...]" where each leaf spec ends in
// ",<pane-index>". Pane indices are assigned by tmux at creation time
// so they DIFFER across a kill+respawn even when geometry is identical.
// The byte-precise-geometry contract is "all the W/H/X/Y values match";
// pane IDs are bookkeeping.
//
// The first two-digit field at the start (`<HEX>,<WxH>`) is the layout
// checksum + window dimensions, which DO depend on pane IDs (because
// tmux folds them into the checksum). We strip the checksum too —
// the checksum is what tmux uses to detect a corrupted layout string
// at parse time, not a stable identifier across mutations.
func stripPaneIndices(layout string) string {
	// Pane-index suffix: a comma + digits immediately before `,`, `}`, or `]`.
	idxRe := regexp.MustCompile(`,\d+([,}\]])`)
	stripped := idxRe.ReplaceAllString(layout, ",X$1")
	// Drop the leading "checksum," prefix.
	if i := strings.Index(stripped, ","); i > 0 {
		stripped = stripped[i+1:]
	}
	return stripped
}

// TestCaptureWindowLayout_RoundTrip creates a multi-pane session,
// captures its layout, kills+respawns one pane, then SelectLayout-
// restores. The captured-before layout string must equal the
// captured-after layout string — otherwise the swap path drifts pane
// geometry across iterations.
func TestCaptureWindowLayout_RoundTrip(t *testing.T) {
	cwd := t.TempDir()
	name := "tmux-layout-roundtrip-" + strings.ReplaceAll(t.Name(), "/", "_")
	ctx := context.Background()
	c := tmux.WithSocket(testSocket)

	// 3-pane session: IDE (top-left), shell (bottom), agent (top-right).
	// Matches canopy's buildSession layout.
	idePane, err := c.Create(ctx, name, cwd, "sleep 120")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", testSocket, "kill-session", "-t", name).Run()
	})
	_ = idePane
	shellPane, err := c.SplitPane(ctx, name, cwd, "sleep 120", tmux.SplitVertical, 15)
	if err != nil {
		t.Fatalf("SplitPane shell: %v", err)
	}
	_ = shellPane
	agentPane, err := c.SplitPane(ctx, name, cwd, "sleep 120", tmux.SplitHorizontal, 30)
	if err != nil {
		t.Fatalf("SplitPane agent: %v", err)
	}

	before, err := c.CaptureWindowLayout(ctx, name)
	if err != nil {
		t.Fatalf("CaptureWindowLayout: %v", err)
	}
	if before == "" {
		t.Fatal("CaptureWindowLayout returned empty layout")
	}

	// Simulate canopy agent swap: kill agent pane and respawn.
	if err := c.KillPane(ctx, agentPane); err != nil {
		t.Fatalf("KillPane: %v", err)
	}
	newAgent, err := c.SplitPane(ctx, name, cwd, "sleep 120", tmux.SplitHorizontal, 30)
	if err != nil {
		t.Fatalf("SplitPane respawn: %v", err)
	}
	_ = newAgent

	// Without SelectLayout, geometry typically drifts (the 30% is now
	// percent-of-redistributed-IDE, not percent-of-original). After
	// SelectLayout, geometry should be byte-identical to `before`.
	if err := c.SelectLayout(ctx, name, before); err != nil {
		t.Fatalf("SelectLayout: %v", err)
	}
	after, err := c.CaptureWindowLayout(ctx, name)
	if err != nil {
		t.Fatalf("CaptureWindowLayout after: %v", err)
	}
	// Compare geometry (W/H/X/Y) ignoring pane IDs and checksum.
	if got, want := stripPaneIndices(after), stripPaneIndices(before); got != want {
		t.Errorf("layout drift after kill+respawn+restore:\nbefore (raw)=%q\nafter  (raw)=%q\nbefore (geom)=%q\nafter  (geom)=%q",
			before, after, want, got)
	}
}

// TestKillPane_EmptyPaneID is a quick guard: empty pane ID must fail
// fast rather than asking tmux to kill nothing-in-particular.
func TestKillPane_EmptyPaneID(t *testing.T) {
	c := tmux.WithSocket(testSocket)
	if err := c.KillPane(context.Background(), ""); err == nil {
		t.Error("KillPane(empty) returned nil; want error")
	}
}
