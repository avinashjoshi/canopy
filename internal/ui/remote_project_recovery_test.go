package ui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"

	"github.com/avinashjoshi/canopy/internal/host"
)

// TestAttachRemoteRow_NotRegisteredErrorWording: when attaching to a
// remote (main) row whose project the laptop hasn't registered, the
// error message has to point the user at a CLI that actually works.
// Pre-fix the hint said `--host`, but canopy project add has only
// ever accepted `--on` — copy-pasting the hint resulted in
// "unknown flag --host" and the user couldn't recover.
//
// Regression for the v0.21.1 add-project-on-tower flow where the
// remote canopy was pre-v0.20 (silent auto-register failure → row
// shows up via canopy ls --json but attach errors).
func TestAttachRemoteRow_NotRegisteredErrorWording(t *testing.T) {
	m := &Model{
		nameInput:       textinput.New(),
		listInput:       textinput.New(),
		targetInput:     textinput.New(),
		addProjectInput: textinput.New(),
	}
	// Empty hostList → remoteCwdForRow returns "" → the IsMain branch
	// at update_attach.go:209 fires.
	m.hostList = []host.Host{{Name: "tower", SSHTarget: "u@t", Type: "ssh"}}

	row := Row{
		Host:    "tower",
		Project: "chrome-tab-close-guard",
		IsMain:  true,
	}
	cmd := m.attachRemoteRow(row, false)
	if cmd == nil {
		t.Fatal("attachRemoteRow: nil cmd; want errMsg")
	}
	msg := cmd()
	em, ok := msg.(errMsg)
	if !ok {
		t.Fatalf("attachRemoteRow: got %T; want errMsg", msg)
	}
	if em.err == nil {
		t.Fatal("errMsg: nil err")
	}
	text := em.err.Error()
	if !strings.Contains(text, "--on tower") {
		t.Errorf("error missing `--on tower` hint: %q", text)
	}
	if strings.Contains(text, "--host ") {
		t.Errorf("error still mentions deprecated `--host` flag: %q", text)
	}
	// The CLI shape is `canopy project add <name> <path> --on <host>`
	// — the hint has to put the project name in the right slot, not
	// jam the host name there.
	if !strings.Contains(text, "canopy project add chrome-tab-close-guard") {
		t.Errorf("error hint has wrong arg order: %q", text)
	}
}

// TestShowAddProjectToast_WarningRendersInToast: when
// registerRemoteAddProject returns a non-empty warning string (e.g.
// the remote was pre-v0.20 and the laptop couldn't auto-register),
// the toast has to carry it through so the user knows they need to
// manually register before attach will work. Without this the user
// sees only "✓ Added X" and discovers the breakage later when
// pressing Enter on the new row.
func TestShowAddProjectToast_WarningRendersInToast(t *testing.T) {
	m, _ := newAddProjectTestModel(t)

	// Plain success path — no warning.
	cmd := m.showAddProjectToast("foo", "/path/foo", "")
	if !strings.HasPrefix(m.addProjectToast, "✓ Added foo at /path/foo") {
		t.Errorf("clean toast = %q; want '✓ Added foo at /path/foo' prefix", m.addProjectToast)
	}
	if strings.Contains(m.addProjectToast, "⚠") {
		t.Errorf("clean toast unexpectedly contains warning glyph: %q", m.addProjectToast)
	}
	// Clean toast uses the short 3s timer; check the expiry is short
	// enough to be the "no warning" path. Allow slack for clock skew.
	if delta := time.Until(m.addProjectToastFor); delta > 4*time.Second {
		t.Errorf("clean toast expiry %v > 4s; want short 3s window", delta)
	}
	if cmd == nil {
		t.Error("clean toast: nil cmd; want refresh + expire batch")
	}

	// Warning path — message has to include both the ✓ success line
	// AND the ⚠ warning with the manual-recovery command verbatim.
	want := "remote tower didn't return a project path"
	m.showAddProjectToast("bar on tower", "tower", want)
	if !strings.Contains(m.addProjectToast, "✓ Added bar on tower") {
		t.Errorf("warning toast missing success line: %q", m.addProjectToast)
	}
	if !strings.Contains(m.addProjectToast, "⚠ "+want) {
		t.Errorf("warning toast missing warning content: %q", m.addProjectToast)
	}
	// Extended expiry: the warning includes a multi-line recovery
	// command the user needs time to read + copy. 3s is too short.
	if delta := time.Until(m.addProjectToastFor); delta < 6*time.Second {
		t.Errorf("warning toast expiry %v < 6s; want extended window", delta)
	}
}

// TestAutoRegisterRemoteOrphans_RegistersUnknownProject: self-heals
// the stuck state from the v0.21.1 add-project bug. After the bug
// triggered, the laptop has a host in hosts.json but no project
// entry for chrome-tab-close-guard, while `canopy ls --json` on
// tower keeps reporting the project's (main) row.  Once the remote
// emits the v0.21.2 project_root field, the refresh tick has to
// register the missing project automatically — otherwise the user
// is permanently stuck.
func TestAutoRegisterRemoteOrphans_RegistersUnknownProject(t *testing.T) {
	dir := t.TempDir()
	reg, err := host.NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := reg.Add("tower", host.Host{
		Type:      "ssh",
		SSHTarget: "u@tower",
		Projects:  map[string]string{"canopy": "/home/avi/Work/canopy"},
	}); err != nil {
		t.Fatalf("Add tower: %v", err)
	}
	hosts, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	results := []host.Result{{
		HostName: "tower",
		Workspaces: []host.RemoteWorkspace{
			// Already-registered project: leave it alone.
			{Name: "(main)", Project: "canopy", ProjectRoot: "/home/avi/Work/canopy"},
			// Orphan: ProjectRoot present, laptop never saw it.
			{Name: "(main)", Project: "chrome-tab-close-guard", ProjectRoot: "/home/avi/Work/chrome-tab-close-guard"},
			// Same orphan, second workspace — must dedup (one
			// AddProject call, not two).
			{Name: "feature-x", Project: "chrome-tab-close-guard", ProjectRoot: "/home/avi/Work/chrome-tab-close-guard"},
		},
	}}

	updated := autoRegisterRemoteOrphans(reg, hosts, results)

	// Returned hosts list reflects the registration.
	var towerUpdated *host.Host
	for i := range updated {
		if updated[i].Name == "tower" {
			towerUpdated = &updated[i]
			break
		}
	}
	if towerUpdated == nil {
		t.Fatal("returned hosts missing 'tower'")
	}
	got := towerUpdated.Projects["chrome-tab-close-guard"]
	if got != "/home/avi/Work/chrome-tab-close-guard" {
		t.Errorf("auto-registered path = %q; want /home/avi/Work/chrome-tab-close-guard", got)
	}
	// And the existing registration must be intact.
	if towerUpdated.Projects["canopy"] != "/home/avi/Work/canopy" {
		t.Errorf("existing canopy registration overwritten: %q", towerUpdated.Projects["canopy"])
	}

	// hosts.json on disk is the source of truth — verify by reloading.
	reg2, err := host.NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry (reload): %v", err)
	}
	got2, err := reg2.GetProject("tower", "chrome-tab-close-guard")
	if err != nil {
		t.Fatalf("ResolveProject after auto-register: %v", err)
	}
	if got2 != "/home/avi/Work/chrome-tab-close-guard" {
		t.Errorf("on-disk path = %q; want /home/avi/Work/chrome-tab-close-guard", got2)
	}
}

// TestAutoRegisterRemoteOrphans_SkipsEmptyProjectRoot: pre-v0.21.2
// remotes leave the project_root field empty. The laptop has to
// no-op — never invent a path, never log an error. This is the
// "graceful degradation against an older remote" branch.
func TestAutoRegisterRemoteOrphans_SkipsEmptyProjectRoot(t *testing.T) {
	dir := t.TempDir()
	reg, _ := host.NewRegistry(dir)
	if err := reg.Add("tower", host.Host{Type: "ssh", SSHTarget: "u@tower"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	hosts, _ := reg.List()

	results := []host.Result{{
		HostName: "tower",
		Workspaces: []host.RemoteWorkspace{
			{Name: "(main)", Project: "chrome-tab-close-guard", ProjectRoot: ""},
		},
	}}

	updated := autoRegisterRemoteOrphans(reg, hosts, results)

	// Same instance comes back unchanged when there's nothing to do
	// — the function short-circuits the disk reload.
	for _, h := range updated {
		if h.Name == "tower" && len(h.Projects) > 0 {
			t.Errorf("tower.Projects unexpectedly populated: %v", h.Projects)
		}
	}
	// And no orphan reached disk either.
	reg2, _ := host.NewRegistry(dir)
	if _, err := reg2.GetProject("tower", "chrome-tab-close-guard"); !errors.Is(err, host.ErrProjectNotFound) {
		t.Errorf("ResolveProject err = %v; want ErrProjectNotFound", err)
	}
}

// TestAutoRegisterRemoteOrphans_RejectsInvalidPath: a remote could be
// on an older canopy that wrote garbage, or actively compromised;
// the laptop's blanket "auto-register everything the remote claims"
// stance has to validate paths the same way the v0.20 result-file
// channel does. Otherwise a hostile remote could poison hosts.json
// with relative paths, control characters, or oversize blobs.
func TestAutoRegisterRemoteOrphans_RejectsInvalidPath(t *testing.T) {
	dir := t.TempDir()
	reg, _ := host.NewRegistry(dir)
	if err := reg.Add("tower", host.Host{Type: "ssh", SSHTarget: "u@tower"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	hosts, _ := reg.List()

	results := []host.Result{{
		HostName: "tower",
		Workspaces: []host.RemoteWorkspace{
			// Relative path: validateRemoteResultPath rejects.
			{Name: "(main)", Project: "evil", ProjectRoot: "../../../etc"},
			// Control-character poisoning attempt.
			{Name: "(main)", Project: "evil2", ProjectRoot: "/tmp/x\x00y"},
		},
	}}

	autoRegisterRemoteOrphans(reg, hosts, results)

	reg2, _ := host.NewRegistry(dir)
	if _, err := reg2.GetProject("tower", "evil"); !errors.Is(err, host.ErrProjectNotFound) {
		t.Errorf("relative-path orphan registered: %v", err)
	}
	if _, err := reg2.GetProject("tower", "evil2"); !errors.Is(err, host.ErrProjectNotFound) {
		t.Errorf("control-char orphan registered: %v", err)
	}
}

// TestAutoRegisterRemoteOrphans_SkipsFailedHost: when a host's
// refresh errored (offline, ssh failure), Workspaces is unreliable
// — sometimes empty, sometimes a stale snapshot. Either way, treating
// it as authoritative for orphan discovery would cause false negatives
// or weird interleavings. Iron rule: if r.Err is non-nil, skip the
// whole host this tick.
func TestAutoRegisterRemoteOrphans_SkipsFailedHost(t *testing.T) {
	dir := t.TempDir()
	reg, _ := host.NewRegistry(dir)
	if err := reg.Add("tower", host.Host{Type: "ssh", SSHTarget: "u@tower"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	hosts, _ := reg.List()

	results := []host.Result{{
		HostName: "tower",
		Err:      errors.New("ssh: connection refused"),
		Workspaces: []host.RemoteWorkspace{
			{Name: "(main)", Project: "ghost", ProjectRoot: "/home/avi/Work/ghost"},
		},
	}}

	autoRegisterRemoteOrphans(reg, hosts, results)

	reg2, _ := host.NewRegistry(dir)
	if _, err := reg2.GetProject("tower", "ghost"); !errors.Is(err, host.ErrProjectNotFound) {
		t.Errorf("orphan from errored host got registered: %v", err)
	}
}
