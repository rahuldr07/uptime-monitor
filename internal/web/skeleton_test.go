package web

import (
	"strings"
	"testing"
)

func TestSkeletonTemplatesOnlyInLayoutPages(t *testing.T) {
	e := newEnv(t)
	_, body := e.get(t, e.op, "/")
	for _, id := range []string{"skel-overview", "skel-table", "skel-detail", "skel-list", "skel-generic", "skel-line"} {
		if !strings.Contains(body, `<template id="`+id+`"`) {
			t.Errorf("layout page lacks template %s", id)
		}
	}
	if n := strings.Count(body, `class="skel-row"`); n != 6+8+4+6 {
		t.Errorf("skeleton rows = %d, want 24 (range over the row counts)", n)
	}
	_, body = e.get(t, e.client(t), "/login")
	if strings.Contains(body, "<template") {
		t.Error("public pages have no sidebar navigation and must not carry skeletons")
	}
}
