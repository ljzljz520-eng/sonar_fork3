package runsreg

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/raskrebs/sonar/internal/runs"
)

// legacyFixture is the runs.json shape an older sonar left behind on each
// OS. The fields that identify a run (pid, id, startedAt, tag/group/name) are
// identical in all three fixtures; only platform-shaped cwd/cmd differ.
// pid 10 is an old-format entry (tag, no token); pid 11 carries a token.
func legacyFixture(platform string) string {
	cwd, cmd10, cmd11 := "", "sleep 10", "npm run dev"
	switch platform {
	case "linux":
		cwd = "/home/dev/shop"
	case "darwin":
		cwd = "/Users/dev/shop"
		cmd10 = "/bin/sleep 10"
	case "windows":
		cwd = `C:\Users\dev\shop`
		cmd10 = "timeout 10"
		cmd11 = "npm.cmd run dev"
	}
	// Pretty JSON with platform-typical content but identical identity fields.
	return `{
  "runs": {
    "10": {
      "pid": 10,
      "tag": "shop",
      "id": "run10",
      "cmd": "` + jsonEscape(cmd10) + `",
      "startedAt": "2026-09-05T12:00:00Z",
      "cwd": "` + jsonEscape(cwd) + `"
    },
    "11": {
      "pid": 11,
      "tag": "web",
      "id": "run11",
      "cmd": "` + jsonEscape(cmd11) + `",
      "startedAt": "2026-09-05T12:05:00Z",
      "group": "web",
      "name": "api",
      "cwd": "` + jsonEscape(cwd) + `",
      "ppid": 5,
      "portHint": 5173,
      "token": "keep-token"
    }
  }
}
`
}

func jsonEscape(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			out = append(out, '\\', '\\')
		case '"':
			out = append(out, '\\', '"')
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}

// TestImportLegacyFixturesAcrossOperatingSystems: every fixture migrates to
// the same identities; the old-format entry gets its deterministic token and
// an explicit-token entry keeps its token. Reopening repeats without dupes.
func TestImportLegacyFixturesAcrossOperatingSystems(t *testing.T) {
	wantOldToken := legacyToken(runs.Entry{
		PID: 10, Tag: "shop", ID: "run10", StartedAt: "2026-09-05T12:00:00Z",
	})
	for _, platform := range []string{"linux", "darwin", "windows"} {
		t.Run(platform, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			if err := os.MkdirAll(filepath.Join(home, ".config", "sonar"), 0o755); err != nil {
				t.Fatal(err)
			}
			fixture := legacyFixture(platform)
			if err := os.WriteFile(runs.Path(), []byte(fixture), 0o644); err != nil {
				t.Fatal(err)
			}
			info := map[int]runs.Info{
				10: {PID: 10, Alive: true, Birth: birth1},
				11: aliveInfo(11, "keep-token", birth1.Add(5*time.Minute)),
			}
			r, imported := openDurable(t, info, nil)
			if imported != 2 {
				t.Fatalf("imported = %d, want 2", imported)
			}
			assertInvariants(t, r)
			live := r.List()
			if len(live) != 2 {
				t.Fatalf("live = %+v", live)
			}
			rec10, ok := r.Lookup(10)
			if !ok {
				t.Fatal("pid 10 not imported")
			}
			if rec10.Token != wantOldToken {
				t.Errorf("pid 10 token = %q, want deterministic %q", rec10.Token, wantOldToken)
			}
			if rec10.Group != "shop" || rec10.Name != "shop" || rec10.ID != "run10" {
				t.Errorf("pid 10 identity fields = %+v", rec10)
			}
			rec11, ok := r.Lookup(11)
			if !ok || rec11.Token != "keep-token" || rec11.Group != "web" ||
				rec11.Name != "api" || rec11.PortHint != 5173 {
				t.Fatalf("pid 11 migrated wrong: %+v", rec11)
			}
			// Without mirroring the consumed legacy file is removed.
			if _, err := os.Stat(runs.Path()); !os.IsNotExist(err) {
				t.Errorf("consumed runs.json still present: %v", err)
			}

			// Second daemon generation: nothing to import, same live set.
			r2, imported2 := openDurable(t, info, nil)
			if imported2 != 0 {
				t.Errorf("second import moved %d runs", imported2)
			}
			assertInvariants(t, r2)
			if len(r2.List()) != 2 {
				t.Fatalf("live after reopen = %+v", r2.List())
			}
		})
	}
}

// TestTamperedLegacyFileIsQuarantined: truncation or tampering must be moved
// aside, backed up and reported — never silently replaced with an empty file.
func TestTamperedLegacyFileIsQuarantined(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{"truncated", `{"runs": {"10": {"pid": 10, `},
		{"tampered", `{"runs": {"10": not-json}}`},
		{"garbage", "\xff\xfe{}\x00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			dir := filepath.Join(home, ".config", "sonar")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(runs.Path(), []byte(tc.data), 0o644); err != nil {
				t.Fatal(err)
			}
			r, imported := openDurable(t, nil, nil)
			if imported != 0 {
				t.Errorf("imported = %d from a damaged file", imported)
			}
			if !diagCode(r, DiagLegacyQuarantined) {
				t.Fatalf("damage was not reported: %+v", r.Diagnostics())
			}
			var backup string
			for _, d := range r.Diagnostics() {
				if d.Code == DiagLegacyQuarantined {
					backup = d.Backup
				}
			}
			if backup == "" {
				t.Fatal("no backup path reported")
			}
			got, err := os.ReadFile(backup)
			if err != nil || string(got) != tc.data {
				t.Errorf("backup = %q, %v; want the original bytes", got, err)
			}
			// No fresh, silently-emptied runs.json may be written.
			if data, err := os.ReadFile(runs.Path()); err == nil && len(data) > 0 {
				t.Errorf("runs.json was rewritten rather than isolated: %q", data)
			}
			assertInvariants(t, r)
		})
	}
}

// TestStaleLegacyEntryIsNotImported: a legacy pid now held by a different
// process is skipped with stale_identity; a matching sibling still migrates.
func TestStaleLegacyEntryIsNotImported(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "sonar")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := `{
  "runs": {
    "12": {"pid": 12, "tag": "old", "id": "r12", "startedAt": "2026-09-05T12:00:00Z"},
    "13": {"pid": 13, "tag": "keep", "id": "r13", "startedAt": "2026-09-05T12:01:00Z", "token": "keep-13"}
  }
}`
	if err := os.WriteFile(runs.Path(), []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	info := map[int]runs.Info{
		12: aliveInfo(12, "someone-else", birth2),
		13: aliveInfo(13, "keep-13", birth1.Add(time.Minute)),
	}
	r, imported := openDurable(t, info, nil)
	if imported != 1 {
		t.Fatalf("imported = %d, want only the matching pid 13", imported)
	}
	if live := r.List(); len(live) != 1 || live[0].PID != 13 {
		t.Fatalf("live = %+v", live)
	}
	if !diagCode(r, DiagStaleIdentity) {
		t.Errorf("the stale legacy entry produced no stale_identity diagnostic: %+v", r.Diagnostics())
	}
}

// TestImportLegacyMirrorRewritesTheFile: with mirroring on, runs.json is
// replaced by the live set only (dead entries gone) and stays stable when the
// daemon generation restarts.
func TestImportLegacyMirrorRewritesTheFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "sonar")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := `{
  "runs": {
    "10": {"pid": 10, "tag": "shop", "id": "run10", "startedAt": "2026-09-05T12:00:00Z"},
    "14": {"pid": 14, "tag": "dead", "id": "run14", "startedAt": "2026-09-05T11:00:00Z"}
  }
}`
	if err := os.WriteFile(runs.Path(), []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	info := map[int]runs.Info{10: {PID: 10, Alive: true, Birth: birth1}}

	// Generation 1, mirror on: open manually instead of openDurable.
	r := New()
	r.Mirror = true
	r.Parents = func() map[int]int { return nil }
	r.Probe = fakeProbe(info)
	r.Reset()
	if imported := r.Open(); imported != 1 {
		t.Fatalf("imported = %d, want 1", imported)
	}
	reg, _ := runs.LoadRaw()
	if len(reg.Runs) != 1 {
		t.Fatalf("mirrored runs.json = %+v, want only pid 10", reg.Runs)
	}
	if e, ok := reg.Runs[10]; !ok || e.ID != "run10" || e.Token == "" {
		t.Fatalf("mirrored entry = %+v", e)
	}

	// Generation 2, mirror still on: the rewritten file migrates zero runs
	// (already present in the journal) and the file stays a one-entry mirror.
	r2 := New()
	r2.Mirror = true
	r2.Parents = func() map[int]int { return nil }
	r2.Probe = fakeProbe(info)
	r2.Reset()
	if imported := r2.Open(); imported != 0 {
		t.Errorf("second generation imported %d", imported)
	}
	reg2, rep2 := runs.LoadRaw()
	if rep2.Quarantined || len(reg2.Runs) != 1 {
		t.Fatalf("mirror after reopen = %+v, %+v", reg2.Runs, rep2)
	}
}
