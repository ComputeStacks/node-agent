package backup

import (
	"reflect"
	"testing"

	"github.com/coreos/go-semver/semver"
)

func TestParseMysqlRows(t *testing.T) {
	got := parseMysqlRows("42\t61\r\n7\t3\r\n")
	want := [][]string{{"42", "61"}, {"7", "3"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := parseMysqlRows("\r\n"); got != nil {
		t.Errorf("empty output: got %q", got)
	}
}

func TestWatchBackupLockDisabled(t *testing.T) {
	if b := watchBackupLock(nil, nil, 0)(); b != nil {
		t.Errorf("disabled watch returned %+v", b)
	}
}

func TestUsesBackupStage(t *testing.T) {
	for v, want := range map[string]bool{"10.2.0": false, "10.3.39": false, "10.4.0": true, "10.6.0": true, "10.11.0": true, "11.8.9": true} {
		if got := usesBackupStage(&MysqlInstance{Version: semver.New(v)}); got != want {
			t.Errorf("%s: got %v, want %v", v, got, want)
		}
	}
}
