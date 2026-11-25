package dbbackup

import (
	"sort"
	"time"

	"cs-agent/s3upload"
)

// Keep returns the keys to delete from objs, all the snapshots of ONE database
// (any lineages). Per lineage: everything newer than keepAll is kept, beyond
// that the newest object per UTC day within `days`, and the newest object of the
// lineage is always kept while the database exists. Keys that do not parse are
// never deleted.
func Keep(objs []s3upload.Object, now time.Time, keepAll time.Duration, days int, dbExists bool) (del []string) {
	type item struct {
		key string
		ts  time.Time
	}
	byLineage := map[string][]item{}
	for _, o := range objs {
		_, _, lin, ts, ok := ParseKey(o.Key)
		if !ok {
			continue
		}
		byLineage[lin] = append(byLineage[lin], item{o.Key, ts})
	}
	horizon := time.Duration(days) * 24 * time.Hour
	// The current lineage is the one holding the newest object. Only its
	// newest object is kept regardless of age; older lineages (a reimaged
	// node's previous history, or the history before a restore) age out
	// after `days` like everything else.
	current, newest := "", time.Time{}
	for lin, items := range byLineage {
		for _, it := range items {
			if it.ts.After(newest) {
				current, newest = lin, it.ts
			}
		}
	}
	for lin, items := range byLineage {
		sort.Slice(items, func(i, j int) bool { return items[i].ts.After(items[j].ts) })
		seenDay := map[string]bool{}
		for i, it := range items {
			if i == 0 && dbExists && lin == current {
				continue
			}
			age := now.Sub(it.ts)
			if age <= keepAll {
				continue
			}
			if age > horizon {
				del = append(del, it.key)
				continue
			}
			day := it.ts.UTC().Format("20060102")
			if seenDay[day] {
				del = append(del, it.key)
				continue
			}
			seenDay[day] = true
		}
	}
	sort.Strings(del)
	return del
}
