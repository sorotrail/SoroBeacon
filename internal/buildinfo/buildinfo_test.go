package buildinfo

import (
	"testing"
)

func TestBuildInfoDefaults(t *testing.T) {
	if Version != "dev" {
		t.Errorf("expected Version 'dev', got %q", Version)
	}
	if Commit != "none" {
		t.Errorf("expected Commit 'none', got %q", Commit)
	}
	if Date != "unknown" {
		t.Errorf("expected Date 'unknown', got %q", Date)
	}
}

func TestInfoStruct(t *testing.T) {
	info := Info{
		Version: Version,
		Commit:  Commit,
		Date:    Date,
	}

	if info.Version != "dev" {
		t.Errorf("expected Info.Version 'dev', got %q", info.Version)
	}
	if info.Commit != "none" {
		t.Errorf("expected Info.Commit 'none', got %q", info.Commit)
	}
	if info.Date != "unknown" {
		t.Errorf("expected Info.Date 'unknown', got %q", info.Date)
	}
}
