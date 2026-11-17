package imagediff

import (
	"strings"
	"testing"
)

// testManifests builds a before/after pair covering: addition, deletion,
// content change (hash), type change (symlink -> regular file), mode
// change, owner change, and a whole-directory deletion.
func testManifests() (*Manifest, *Manifest) {
	before := &Manifest{
		ImageRef: "app:1.0",
		Entries: []FileEntry{
			{Path: "/", Type: TypeDir, Mode: 0755},
			{Path: "/app", Type: TypeDir, Mode: 0755},
			{Path: "/app/server", Type: TypeFile, Size: 5000, Mode: 0755, Hash: "aaa"},
			// kept, content changed
			{Path: "/app/config.json", Type: TypeFile, Size: 100, Mode: 0644, Hash: "c1"},
			// kept, symlink replaced by a regular file (content category)
			{Path: "/app/current", Type: TypeSymlink, Mode: 0777, LinkTarget: "/app/server"},
			// kept, mode changed
			{Path: "/app/run.sh", Type: TypeFile, Size: 40, Mode: 0644, Hash: "r1"},
			// kept, owner changed
			{Path: "/app/data", Type: TypeFile, Size: 60, Mode: 0644, UID: 0, GID: 0, Hash: "d1"},
			// kept, unchanged
			{Path: "/etc/hostname", Type: TypeFile, Size: 10, Mode: 0644, Hash: "h1"},
			// deleted individually (parent dir survives)
			{Path: "/tmp", Type: TypeDir, Mode: 1777},
			{Path: "/tmp/cache.bin", Type: TypeFile, Size: 200, Mode: 0644, Hash: "t1"},
			// whole directory deleted -> must collapse
			{Path: "/usr/share/doc", Type: TypeDir, Mode: 0755},
			{Path: "/usr/share/doc/a.md", Type: TypeFile, Size: 1000, Mode: 0644, Hash: "m1"},
			{Path: "/usr/share/doc/sub", Type: TypeDir, Mode: 0755},
			{Path: "/usr/share/doc/sub/b.md", Type: TypeFile, Size: 2000, Mode: 0644, Hash: "m2"},
		},
		Stats: SizeStats{ContentBytes: 8410, ImageBytes: 3000, MetadataBytes: 500},
	}

	after := &Manifest{
		ImageRef: "app:1.0-slim",
		Entries: []FileEntry{
			{Path: "/", Type: TypeDir, Mode: 0755},
			{Path: "/app", Type: TypeDir, Mode: 0755},
			{Path: "/app/server", Type: TypeFile, Size: 5000, Mode: 0755, Hash: "aaa"},
			{Path: "/app/config.json", Type: TypeFile, Size: 120, Mode: 0644, Hash: "c2"},
			{Path: "/app/current", Type: TypeFile, Size: 5000, Mode: 0755, Hash: "aaa"},
			{Path: "/app/run.sh", Type: TypeFile, Size: 40, Mode: 0755, Hash: "r1"},
			{Path: "/app/data", Type: TypeFile, Size: 60, Mode: 0644, UID: 1000, GID: 1000, Hash: "d1"},
			{Path: "/etc/hostname", Type: TypeFile, Size: 10, Mode: 0644, Hash: "h1"},
			{Path: "/tmp", Type: TypeDir, Mode: 1777},
			// added file
			{Path: "/app/newlib.so", Type: TypeFile, Size: 800, Mode: 0644, Hash: "n1"},
		},
		Stats: SizeStats{ContentBytes: 11030, ImageBytes: 1200, MetadataBytes: 300},
	}

	return before, after
}

func paths(entries []FileEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Path
	}
	return out
}

func modPaths(entries []ModifiedEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Path
	}
	return out
}

func TestDiffCategories(t *testing.T) {
	before, after := testManifests()
	r := Diff(before, after, Options{})

	// added
	if got := paths(r.Added); len(got) != 1 || got[0] != "/app/newlib.so" {
		t.Fatalf("added = %v", got)
	}

	// deleted: individual file only; /usr/share/doc must be collapsed
	if got := paths(r.Deleted); len(got) != 1 || got[0] != "/tmp/cache.bin" {
		t.Fatalf("deleted = %v", got)
	}
	if len(r.DeletedDirs) != 1 {
		t.Fatalf("deleted dirs = %+v", r.DeletedDirs)
	}
	cd := r.DeletedDirs[0]
	if cd.Path != "/usr/share/doc" || cd.EntryCount != 4 || cd.ContentBytes != 3000 {
		t.Fatalf("collapsed dir = %+v", cd)
	}

	// changed categories, each sorted by path
	if got := modPaths(r.ContentChanged); len(got) != 2 ||
		got[0] != "/app/config.json" || got[1] != "/app/current" {
		t.Fatalf("content changed = %v", got)
	}
	if got := modPaths(r.ModeChanged); len(got) != 1 || got[0] != "/app/run.sh" {
		t.Fatalf("mode changed = %v", got)
	}
	if got := modPaths(r.OwnerChanged); len(got) != 1 || got[0] != "/app/data" {
		t.Fatalf("owner changed = %v", got)
	}
}

func TestDiffTotals(t *testing.T) {
	before, after := testManifests()
	r := Diff(before, after, Options{})

	want := Totals{
		Added:               1,
		Deleted:             5, // /tmp/cache.bin + 4 under /usr/share/doc
		DeletedCollapsed:    4,
		Modified:            4,
		ContentChanged:      2,
		ModeChanged:         1,
		OwnerChanged:        1,
		ContentBytesBefore:  8410,
		ContentBytesAfter:   11030,
		ImageBytesBefore:    3000,
		ImageBytesAfter:     1200,
		MetadataBytesBefore: 500,
		MetadataBytesAfter:  300,
	}
	if r.Totals != want {
		t.Fatalf("totals = %+v, want %+v", r.Totals, want)
	}
}

func TestDiffDirSummary(t *testing.T) {
	before, after := testManifests()
	r := Diff(before, after, Options{SummaryDepth: 1})

	byDir := map[string]DirStats{}
	for _, ds := range r.DirSummary {
		byDir[ds.Dir] = ds
	}

	// /app: 1 added (800B), 4 modified; removed 100+40+60=200,
	// added 120+5000(symlink->file)+40+60=5120
	app := byDir["/app"]
	if app.Added != 1 || app.Modified != 4 || app.Deleted != 0 {
		t.Fatalf("/app summary = %+v", app)
	}
	if app.BytesAdded != 800+120+5000+40+60 || app.BytesRemoved != 100+40+60 {
		t.Fatalf("/app bytes = %+v", app)
	}

	// /usr: collapsed dir contributes 4 entries / 3000 bytes
	usr := byDir["/usr"]
	if usr.Deleted != 4 || usr.BytesRemoved != 3000 {
		t.Fatalf("/usr summary = %+v", usr)
	}

	// /tmp: one deleted file
	tmp := byDir["/tmp"]
	if tmp.Deleted != 1 || tmp.BytesRemoved != 200 {
		t.Fatalf("/tmp summary = %+v", tmp)
	}

	// sibling sort: total changed bytes descending, path ascending on ties
	for i := 1; i < len(r.DirSummary); i++ {
		prev := r.DirSummary[i-1].BytesAdded + r.DirSummary[i-1].BytesRemoved
		cur := r.DirSummary[i].BytesAdded + r.DirSummary[i].BytesRemoved
		if prev < cur {
			t.Fatalf("summary not sorted by bytes desc: %+v", r.DirSummary)
		}
		if prev == cur && r.DirSummary[i-1].Dir > r.DirSummary[i].Dir {
			t.Fatalf("summary tie not broken by path asc: %+v", r.DirSummary)
		}
	}
}

func TestRenderDeterministic(t *testing.T) {
	before, after := testManifests()
	r1 := Diff(before, after, Options{})
	r2 := Diff(before, after, Options{})

	text1, text2 := RenderText(r1), RenderText(r2)
	if text1 != text2 {
		t.Fatal("text render not byte-identical across runs")
	}

	json1, err := RenderJSON(r1)
	if err != nil {
		t.Fatal(err)
	}
	json2, err := RenderJSON(r2)
	if err != nil {
		t.Fatal(err)
	}
	if string(json1) != string(json2) {
		t.Fatal("JSON render not byte-identical across runs")
	}
}

func TestRenderTextFixedOrder(t *testing.T) {
	before, after := testManifests()
	r := Diff(before, after, Options{})
	text := RenderText(r)

	// sections appear in fixed order
	sections := []string{
		"== added (1) ==",
		"== deleted (5) ==",
		"== content-changed (2) ==",
		"== mode-changed (1) ==",
		"== owner-changed (1) ==",
		"== directory-summary (depth=1) ==",
		"== totals ==",
	}
	pos := 0
	for _, s := range sections {
		idx := strings.Index(text[pos:], s)
		if idx < 0 {
			t.Fatalf("section %q missing or out of order in:\n%s", s, text)
		}
		pos += idx + len(s)
	}

	// collapsed dir renders as one line, not four
	if !strings.Contains(text, "D /usr/share/doc/ collapsed entries=4 content_bytes=3000\n") {
		t.Fatalf("collapsed dir line missing in:\n%s", text)
	}
	if strings.Contains(text, "/usr/share/doc/a.md") {
		t.Fatal("collapsed dir contents must not be expanded")
	}

	// type change detail shows symlink -> file
	if !strings.Contains(text, "M /app/current type=symlink->file link=/app/server->\n") {
		t.Fatalf("type change line missing in:\n%s", text)
	}
}
