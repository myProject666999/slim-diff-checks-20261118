package imagediff

import (
	"encoding/json"
	"fmt"
	"strings"
)

// RenderText renders the report in a fixed-order, line-oriented text format
// so two reports (e.g. from different slim runs) can be compared directly
// with standard text diff tools. Section order and entry order are fixed;
// the same report always renders byte-identically.
func RenderText(r *Report) string {
	var b strings.Builder

	fmt.Fprintf(&b, "== image-diff v%s ==\n", r.Version)
	fmt.Fprintf(&b, "before: ref=%s content_bytes=%d image_bytes=%d metadata_bytes=%d\n",
		r.BeforeRef, r.BeforeStats.ContentBytes, r.BeforeStats.ImageBytes, r.BeforeStats.MetadataBytes)
	fmt.Fprintf(&b, "after: ref=%s content_bytes=%d image_bytes=%d metadata_bytes=%d\n",
		r.AfterRef, r.AfterStats.ContentBytes, r.AfterStats.ImageBytes, r.AfterStats.MetadataBytes)

	fmt.Fprintf(&b, "\n== added (%d) ==\n", len(r.Added))
	for _, e := range r.Added {
		fmt.Fprintf(&b, "A %s\n", formatEntry(e))
	}

	fmt.Fprintf(&b, "\n== deleted (%d) ==\n", r.Totals.Deleted)
	for _, e := range r.Deleted {
		fmt.Fprintf(&b, "D %s\n", formatEntry(e))
	}
	for _, cd := range r.DeletedDirs {
		fmt.Fprintf(&b, "D %s/ collapsed entries=%d content_bytes=%d\n",
			cd.Path, cd.EntryCount, cd.ContentBytes)
	}

	fmt.Fprintf(&b, "\n== content-changed (%d) ==\n", len(r.ContentChanged))
	for _, me := range r.ContentChanged {
		fmt.Fprintf(&b, "M %s %s\n", me.Path, changeDetail(ChangeContent, me))
	}

	fmt.Fprintf(&b, "\n== mode-changed (%d) ==\n", len(r.ModeChanged))
	for _, me := range r.ModeChanged {
		fmt.Fprintf(&b, "M %s %s\n", me.Path, changeDetail(ChangeMode, me))
	}

	fmt.Fprintf(&b, "\n== owner-changed (%d) ==\n", len(r.OwnerChanged))
	for _, me := range r.OwnerChanged {
		fmt.Fprintf(&b, "M %s %s\n", me.Path, changeDetail(ChangeOwner, me))
	}

	fmt.Fprintf(&b, "\n== directory-summary (depth=%d) ==\n", r.SummaryDepth)
	for _, ds := range r.DirSummary {
		fmt.Fprintf(&b, "S %s added=%d deleted=%d modified=%d bytes_added=%d bytes_removed=%d\n",
			ds.Dir, ds.Added, ds.Deleted, ds.Modified, ds.BytesAdded, ds.BytesRemoved)
	}

	t := r.Totals
	fmt.Fprintf(&b, "\n== totals ==\n")
	fmt.Fprintf(&b, "added=%d deleted=%d deleted_collapsed=%d modified=%d content_changed=%d mode_changed=%d owner_changed=%d\n",
		t.Added, t.Deleted, t.DeletedCollapsed, t.Modified,
		t.ContentChanged, t.ModeChanged, t.OwnerChanged)
	fmt.Fprintf(&b, "content_bytes=%d->%d image_bytes=%d->%d metadata_bytes=%d->%d\n",
		t.ContentBytesBefore, t.ContentBytesAfter,
		t.ImageBytesBefore, t.ImageBytesAfter,
		t.MetadataBytesBefore, t.MetadataBytesAfter)

	return b.String()
}

// RenderJSON renders the report as indented JSON. Field order follows the
// struct definition and all slices are pre-sorted, so the output is
// byte-identical for identical reports.
func RenderJSON(r *Report) ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

func formatEntry(e FileEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s type=%s", e.Path, e.Type)
	if e.Type == TypeFile {
		fmt.Fprintf(&b, " size=%d", e.Size)
	}
	fmt.Fprintf(&b, " mode=%04o uid=%d gid=%d", e.Mode, e.UID, e.GID)
	if e.Type == TypeSymlink {
		fmt.Fprintf(&b, " link=%s", e.LinkTarget)
	}
	return b.String()
}
