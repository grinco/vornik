package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/cli"
)

func TestWriteIndexJSON(t *testing.T) {
	dir := t.TempDir()
	idx := &cli.SkillIndex{
		Version: 1,
		Skills: []cli.SkillIndexItem{
			{Handle: "vadim", Skill: "research", GitURL: "https://example.com/research.git", Description: "test"},
		},
	}
	if err := writeIndexJSON(dir, idx); err != nil {
		t.Fatalf("write json: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		t.Fatalf("read json: %v", err)
	}
	var got cli.SkillIndex
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse json: %v", err)
	}
	if len(got.Skills) != 1 || got.Skills[0].Handle != "vadim" {
		t.Errorf("json shape lost: %#v", got)
	}
}

func TestWriteIndexHTML(t *testing.T) {
	dir := t.TempDir()
	idx := &cli.SkillIndex{
		Version: 1,
		Skills: []cli.SkillIndexItem{
			{Handle: "vadim", Skill: "research", Description: "Find facts.", Tags: []string{"research"}},
		},
	}
	if err := writeIndexHTML(dir, idx); err != nil {
		t.Fatalf("write html: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatalf("read html: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, "vadim/research") {
		t.Errorf("html missing skill link:\n%s", s)
	}
	if !strings.Contains(s, "Find facts.") {
		t.Errorf("html missing description:\n%s", s)
	}
}

func TestWriteSkillPages(t *testing.T) {
	dir := t.TempDir()
	idx := &cli.SkillIndex{
		Skills: []cli.SkillIndexItem{
			{Handle: "alice", Skill: "news", Description: "News.", GitURL: "https://example.com/news.git"},
		},
	}
	if err := writeSkillPages(dir, idx); err != nil {
		t.Fatalf("write skill pages: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "alice", "news.html"))
	if err != nil {
		t.Fatalf("read skill page: %v", err)
	}
	if !strings.Contains(string(body), "vornikctl skill install alice/news") {
		t.Errorf("skill page missing install snippet:\n%s", body)
	}
}
