// Package imagediff produces a file-level diff report between the original
// ("before") image and the minified ("after") image.
//
// Design decisions (fixed rules, so two reports can be compared with plain
// text tools):
//
//   - Categories: added, deleted, content-changed, mode-changed,
//     owner-changed. A kept-but-changed file appears in every "changed"
//     category that applies to it (content/mode/owner are independent
//     dimensions); the totals count it once.
//   - "Content" covers anything about the file body or its type: content
//     hash change, type change (e.g. symlink replaced by a regular file),
//     or link target change.
//   - Output order is fixed: sections in the order above, entries sorted
//     by path in byte order. No map iteration leaks into the output.
//   - Whole-directory deletions collapse into a single record carrying the
//     entry count and content byte total, instead of expanding into
//     potentially tens of thousands of lines. A directory collapses only
//     when the directory itself and every entry under it are all deleted.
//   - Directory summary aggregates at a configurable depth (default 1,
//     i.e. "/usr", "/etc"); siblings sort by total changed bytes
//     (descending), ties broken by path (ascending).
//
// Size statistics use two distinct measures, each with its own definition:
//
//   - ContentBytes: total uncompressed file content, i.e. the sum of the
//     sizes of all regular files in the flattened filesystem view.
//   - ImageBytes: the actual image footprint = compressed layer blobs plus
//     image metadata (manifest, config, layer metadata after the layer
//     rebuild). MetadataBytes is reported separately so both calibres are
//     visible.
package imagediff

import (
	"fmt"
	"sort"
	"strings"
)

// ReportVersion is the diff report format version. Bump on incompatible
// changes only; additive fields keep the version.
const ReportVersion = "1.0"

// EntryType classifies a filesystem entry.
type EntryType string

const (
	TypeFile    EntryType = "file"
	TypeDir     EntryType = "dir"
	TypeSymlink EntryType = "symlink"
	TypeOther   EntryType = "other"
)

// FileEntry is one filesystem entry in an image manifest.
type FileEntry struct {
	Path       string    `json:"path"`
	Type       EntryType `json:"type"`
	Size       int64     `json:"size,omitempty"` // content bytes, regular files only
	Mode       uint32    `json:"mode,omitempty"` // permission bits
	UID        int       `json:"uid"`
	GID        int       `json:"gid"`
	Hash       string    `json:"hash,omitempty"`        // content hash, regular files only
	LinkTarget string    `json:"link_target,omitempty"` // symlinks only
}

// SizeStats carries the two size calibres for one image.
type SizeStats struct {
	// ContentBytes is the total uncompressed file content
	// (sum of regular file sizes in the flattened filesystem view).
	ContentBytes int64 `json:"content_bytes"`
	// ImageBytes is the actual image footprint: compressed layer blobs
	// plus image metadata.
	ImageBytes int64 `json:"image_bytes"`
	// MetadataBytes is the part of ImageBytes attributable to image
	// metadata (manifest, config, layer metadata after layer rebuild).
	MetadataBytes int64 `json:"metadata_bytes"`
}

// Manifest is the file-level description of one image.
type Manifest struct {
	ImageRef string      `json:"image_ref"`
	Entries  []FileEntry `json:"-"`
	Stats    SizeStats   `json:"stats"`
}

// ChangeKind identifies one independent dimension of modification.
type ChangeKind string

const (
	ChangeContent ChangeKind = "content"
	ChangeMode    ChangeKind = "mode"
	ChangeOwner   ChangeKind = "owner"
)

// changeKindOrder fixes the evaluation/serialization order of change kinds.
var changeKindOrder = []ChangeKind{ChangeContent, ChangeMode, ChangeOwner}

// ModifiedEntry describes a file kept in both images with changed attributes.
type ModifiedEntry struct {
	Path   string    `json:"path"`
	Before FileEntry `json:"before"`
	After  FileEntry `json:"after"`
}

// DeletedDir is a collapsed whole-directory deletion.
type DeletedDir struct {
	Path         string `json:"path"` // directory path, no trailing slash
	EntryCount   int    `json:"entry_count"`
	ContentBytes int64  `json:"content_bytes"`
}

// DirStats is the per-directory aggregated summary line.
type DirStats struct {
	Dir          string `json:"dir"`
	Added        int    `json:"added"`
	Deleted      int    `json:"deleted"` // includes entries inside collapsed dirs
	Modified     int    `json:"modified"`
	BytesAdded   int64  `json:"bytes_added"`
	BytesRemoved int64  `json:"bytes_removed"`
}

// Totals aggregates the whole report. Deleted counts every deleted entry,
// including the ones folded into collapsed directories.
type Totals struct {
	Added               int   `json:"added"`
	Deleted             int   `json:"deleted"`
	DeletedCollapsed    int   `json:"deleted_collapsed"` // entries hidden inside DeletedDirs
	Modified            int   `json:"modified"`          // union of all changed categories
	ContentChanged      int   `json:"content_changed"`
	ModeChanged         int   `json:"mode_changed"`
	OwnerChanged        int   `json:"owner_changed"`
	ContentBytesBefore  int64 `json:"content_bytes_before"`
	ContentBytesAfter   int64 `json:"content_bytes_after"`
	ImageBytesBefore    int64 `json:"image_bytes_before"`
	ImageBytesAfter     int64 `json:"image_bytes_after"`
	MetadataBytesBefore int64 `json:"metadata_bytes_before"`
	MetadataBytesAfter  int64 `json:"metadata_bytes_after"`
}

// Report is the full diff report. Slice fields are always sorted
// (entries by path, summary by the sibling sort rule), so serializing
// the same report twice yields byte-identical output.
type Report struct {
	Version        string          `json:"version"`
	BeforeRef      string          `json:"before_ref"`
	AfterRef       string          `json:"after_ref"`
	BeforeStats    SizeStats       `json:"before_stats"`
	AfterStats     SizeStats       `json:"after_stats"`
	Added          []FileEntry     `json:"added"`
	Deleted        []FileEntry     `json:"deleted"` // individual deletions outside collapsed dirs
	DeletedDirs    []DeletedDir    `json:"deleted_dirs,omitempty"`
	ContentChanged []ModifiedEntry `json:"content_changed,omitempty"`
	ModeChanged    []ModifiedEntry `json:"mode_changed,omitempty"`
	OwnerChanged   []ModifiedEntry `json:"owner_changed,omitempty"`
	SummaryDepth   int             `json:"summary_depth"`
	DirSummary     []DirStats      `json:"dir_summary"`
	Totals         Totals          `json:"totals"`
}

// DefaultSummaryDepth is the default directory aggregation level:
// 1 aggregates under top-level directories ("/usr", "/etc", ...).
const DefaultSummaryDepth = 1

// Options controls diff generation.
type Options struct {
	// SummaryDepth is the directory aggregation level for DirSummary.
	// Values < 1 are treated as DefaultSummaryDepth.
	SummaryDepth int
}

// Diff computes the report between two manifests. It does not mutate its
// inputs. The result is fully deterministic for a given input pair.
func Diff(before, after *Manifest, opts Options) *Report {
	depth := opts.SummaryDepth
	if depth < 1 {
		depth = DefaultSummaryDepth
	}

	beforeByPath := indexByPath(before.Entries)
	afterByPath := indexByPath(after.Entries)

	report := &Report{
		Version:      ReportVersion,
		BeforeRef:    before.ImageRef,
		AfterRef:     after.ImageRef,
		BeforeStats:  before.Stats,
		AfterStats:   after.Stats,
		SummaryDepth: depth,
	}

	// added: in after, not in before
	for _, e := range after.Entries {
		if _, ok := beforeByPath[e.Path]; !ok {
			report.Added = append(report.Added, e)
		}
	}

	// deleted: in before, not in after
	var deleted []FileEntry
	for _, e := range before.Entries {
		if _, ok := afterByPath[e.Path]; !ok {
			deleted = append(deleted, e)
		}
	}

	// modified: in both, attributes differ
	modified := map[string][]ChangeKind{} // path -> kinds (in changeKindOrder)
	modifiedEntries := map[string]ModifiedEntry{}
	for _, afterE := range after.Entries {
		beforeE, ok := beforeByPath[afterE.Path]
		if !ok {
			continue
		}
		kinds := classifyChange(beforeE, afterE)
		if len(kinds) == 0 {
			continue
		}
		modified[afterE.Path] = kinds
		modifiedEntries[afterE.Path] = ModifiedEntry{
			Path:   afterE.Path,
			Before: beforeE,
			After:  afterE,
		}
	}

	// collapse whole-directory deletions
	deletedSet := map[string]bool{}
	for _, e := range deleted {
		deletedSet[e.Path] = true
	}
	collapsed := collapseDeletedDirs(before.Entries, deletedSet)
	collapsedPaths := map[string]bool{} // entries folded into a collapsed dir
	for _, cd := range collapsed {
		prefix := cd.Path + "/"
		for _, e := range deleted {
			if e.Path == cd.Path || strings.HasPrefix(e.Path, prefix) {
				collapsedPaths[e.Path] = true
			}
		}
	}
	for _, e := range deleted {
		if !collapsedPaths[e.Path] {
			report.Deleted = append(report.Deleted, e)
		}
	}
	report.DeletedDirs = collapsed

	// sort everything: path byte order
	sortEntries(report.Added)
	sortEntries(report.Deleted)
	sort.Slice(report.DeletedDirs, func(i, j int) bool {
		return report.DeletedDirs[i].Path < report.DeletedDirs[j].Path
	})

	modPaths := make([]string, 0, len(modified))
	for p := range modified {
		modPaths = append(modPaths, p)
	}
	sort.Strings(modPaths)
	for _, p := range modPaths {
		me := modifiedEntries[p]
		for _, kind := range modified[p] {
			switch kind {
			case ChangeContent:
				report.ContentChanged = append(report.ContentChanged, me)
			case ChangeMode:
				report.ModeChanged = append(report.ModeChanged, me)
			case ChangeOwner:
				report.OwnerChanged = append(report.OwnerChanged, me)
			}
		}
	}

	// totals
	report.Totals = Totals{
		Added:               len(report.Added),
		Deleted:             len(deleted),
		Modified:            len(modPaths),
		ContentChanged:      len(report.ContentChanged),
		ModeChanged:         len(report.ModeChanged),
		OwnerChanged:        len(report.OwnerChanged),
		ContentBytesBefore:  before.Stats.ContentBytes,
		ContentBytesAfter:   after.Stats.ContentBytes,
		ImageBytesBefore:    before.Stats.ImageBytes,
		ImageBytesAfter:     after.Stats.ImageBytes,
		MetadataBytesBefore: before.Stats.MetadataBytes,
		MetadataBytesAfter:  after.Stats.MetadataBytes,
	}
	for _, cd := range collapsed {
		report.Totals.DeletedCollapsed += cd.EntryCount
	}

	report.DirSummary = summarize(beforeByPath, report, deleted, collapsedPaths, depth)

	return report
}

func indexByPath(entries []FileEntry) map[string]FileEntry {
	m := make(map[string]FileEntry, len(entries))
	for _, e := range entries {
		m[e.Path] = e
	}
	return m
}

// classifyChange returns the change kinds that apply, in fixed order.
// Mode is only compared between entries of the same type: permission bits
// are not comparable across types (a symlink always reads 0777), so a type
// change is reported as a content change only.
func classifyChange(before, after FileEntry) []ChangeKind {
	contentChanged := before.Type != after.Type ||
		before.Hash != after.Hash ||
		before.LinkTarget != after.LinkTarget
	modeChanged := before.Type == after.Type && before.Mode != after.Mode
	ownerChanged := before.UID != after.UID || before.GID != after.GID

	flags := map[ChangeKind]bool{
		ChangeContent: contentChanged,
		ChangeMode:    modeChanged,
		ChangeOwner:   ownerChanged,
	}
	var kinds []ChangeKind
	for _, k := range changeKindOrder {
		if flags[k] {
			kinds = append(kinds, k)
		}
	}
	return kinds
}

// collapseDeletedDirs finds maximal deleted subtrees: directories present in
// the before manifest whose every descendant (and the directory itself) is
// deleted. Nested collapsible dirs are folded into their topmost ancestor.
func collapseDeletedDirs(beforeEntries []FileEntry, deletedSet map[string]bool) []DeletedDir {
	// candidate dirs, shallowest first, so ancestors claim their subtree
	var dirs []FileEntry
	for _, e := range beforeEntries {
		if e.Type == TypeDir && deletedSet[e.Path] {
			dirs = append(dirs, e)
		}
	}
	sort.Slice(dirs, func(i, j int) bool {
		di, dj := depthOf(dirs[i].Path), depthOf(dirs[j].Path)
		if di != dj {
			return di < dj
		}
		return dirs[i].Path < dirs[j].Path
	})

	claimed := map[string]bool{} // paths already folded into a collapsed dir
	var out []DeletedDir
	for _, d := range dirs {
		if claimed[d.Path] {
			continue
		}
		prefix := d.Path + "/"
		allDeleted := true
		count := 0
		var bytes int64
		for _, e := range beforeEntries {
			if e.Path == d.Path {
				continue
			}
			if !strings.HasPrefix(e.Path, prefix) {
				continue
			}
			if !deletedSet[e.Path] {
				allDeleted = false
				break
			}
		}
		if !allDeleted {
			continue
		}
		for _, e := range beforeEntries {
			if e.Path == d.Path || strings.HasPrefix(e.Path, prefix) {
				claimed[e.Path] = true
				count++
				if e.Type == TypeFile {
					bytes += e.Size
				}
			}
		}
		out = append(out, DeletedDir{
			Path:         d.Path,
			EntryCount:   count,
			ContentBytes: bytes,
		})
	}
	return out
}

func depthOf(path string) int {
	return strings.Count(strings.Trim(path, "/"), "/")
}

// summaryKey maps a path to its aggregation directory at the given depth.
// "/usr/lib/x.so" at depth 1 -> "/usr", at depth 2 -> "/usr/lib".
// Paths shallower than the depth aggregate under themselves.
func summaryKey(path string, depth int) string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return "/"
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) <= depth {
		return "/" + trimmed
	}
	return "/" + strings.Join(parts[:depth], "/")
}

func summarize(beforeByPath map[string]FileEntry, report *Report, deleted []FileEntry, collapsedPaths map[string]bool, depth int) []DirStats {
	agg := map[string]*DirStats{}
	get := func(path string) *DirStats {
		key := summaryKey(path, depth)
		ds, ok := agg[key]
		if !ok {
			ds = &DirStats{Dir: key}
			agg[key] = ds
		}
		return ds
	}

	for _, e := range report.Added {
		ds := get(e.Path)
		ds.Added++
		if e.Type == TypeFile {
			ds.BytesAdded += e.Size
		}
	}
	// deleted entries hidden inside collapsed dirs are attributed via the
	// collapsed dir record itself (one summary contribution per record)
	for _, e := range deleted {
		if collapsedPaths[e.Path] {
			continue
		}
		ds := get(e.Path)
		ds.Deleted++
		if e.Type == TypeFile {
			ds.BytesRemoved += e.Size
		}
	}
	for _, cd := range report.DeletedDirs {
		ds := get(cd.Path)
		ds.Deleted += cd.EntryCount
		ds.BytesRemoved += cd.ContentBytes
	}
	// modified: union of all change categories
	seen := map[string]bool{}
	for _, list := range [][]ModifiedEntry{report.ContentChanged, report.ModeChanged, report.OwnerChanged} {
		for _, me := range list {
			if seen[me.Path] {
				continue
			}
			seen[me.Path] = true
			ds := get(me.Path)
			ds.Modified++
			if me.Before.Type == TypeFile {
				ds.BytesRemoved += me.Before.Size
			}
			if me.After.Type == TypeFile {
				ds.BytesAdded += me.After.Size
			}
		}
	}

	out := make([]DirStats, 0, len(agg))
	for _, ds := range agg {
		out = append(out, *ds)
	}
	// sibling sort: total changed bytes descending, path ascending
	sort.Slice(out, func(i, j int) bool {
		bi := out[i].BytesAdded + out[i].BytesRemoved
		bj := out[j].BytesAdded + out[j].BytesRemoved
		if bi != bj {
			return bi > bj
		}
		return out[i].Dir < out[j].Dir
	})
	return out
}

func sortEntries(entries []FileEntry) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})
}

// changeDetail renders the per-category detail for a modified entry.
func changeDetail(kind ChangeKind, me ModifiedEntry) string {
	switch kind {
	case ChangeContent:
		if me.Before.Type != me.After.Type {
			detail := fmt.Sprintf("type=%s->%s", me.Before.Type, me.After.Type)
			if me.After.Type == TypeSymlink {
				detail += fmt.Sprintf(" link=->%s", me.After.LinkTarget)
			}
			if me.Before.Type == TypeSymlink {
				detail += fmt.Sprintf(" link=%s->", me.Before.LinkTarget)
			}
			return detail
		}
		if me.Before.LinkTarget != me.After.LinkTarget {
			return fmt.Sprintf("link=%s->%s", me.Before.LinkTarget, me.After.LinkTarget)
		}
		return fmt.Sprintf("hash=%s->%s", me.Before.Hash, me.After.Hash)
	case ChangeMode:
		return fmt.Sprintf("mode=%04o->%04o", me.Before.Mode, me.After.Mode)
	case ChangeOwner:
		return fmt.Sprintf("owner=%d:%d->%d:%d",
			me.Before.UID, me.Before.GID, me.After.UID, me.After.GID)
	}
	return ""
}
