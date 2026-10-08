package tui

import (
	"reflect"
	"testing"
)

func TestEditorSubmissionFilesTracksOnlyLiveFileChips(t *testing.T) {
	e := NewEditor("> ")
	e.files = map[int]string{1: singleQuote("/client/notes 'draft'.txt"), 2: singleQuote("/client/removed.txt")}
	e.dirs = map[int]string{1: singleQuote("/client/folder")}
	e.Insert("review [file:1:notes.txt] [file:1:notes.txt] [dir:1:folder/] /plain/path")
	if got := e.SubmissionFiles(); !reflect.DeepEqual(got, []string{"/client/notes 'draft'.txt"}) {
		t.Fatalf("attachments: %q", got)
	}
	if got := e.SubmitValue(); got == e.Value() {
		t.Fatal("normal path expansion changed")
	}
	e.Clear()
	if len(e.SubmissionFiles()) != 0 {
		t.Fatal("cleared file still attached")
	}
}
