package backup

import (
	"reflect"
	"testing"
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
