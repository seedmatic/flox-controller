package floxenv

import (
	"reflect"
	"testing"
)

func TestRefsFromAnnotations(t *testing.T) {
	got := RefsFromAnnotations(map[string]string{
		AnnotationPrefix + "app":   "networking/kdns", // flat folder
		AnnotationPrefix + "tools": "debug",           // bare → DefaultCategory
		AnnotationPrefix + "mesh":  "mesh/base/relay", // STRUCTURED folder: name is the LAST segment
		AnnotationPrefix + "dup":   "networking/kdns", // dedup with app
		"unrelated/annotation":     "ignored",
		AnnotationPrefix + "empty": "", // empty value skipped
	})
	want := []EnvRef{
		{Folder: "mesh/base", Name: "relay"}, // sorted: "mesh/base" < "networking"
		{Folder: "networking", Name: "debug"},
		{Folder: "networking", Name: "kdns"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RefsFromAnnotations:\n got  %+v\n want %+v", got, want)
	}
}

func TestRefsFromAnnotations_None(t *testing.T) {
	if refs := RefsFromAnnotations(map[string]string{"foo": "bar"}); len(refs) != 0 {
		t.Errorf("expected no refs, got %+v", refs)
	}
}
