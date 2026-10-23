package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindRepoByPathUsesRootNotPrefix(t *testing.T) {
	owner, loser := t.TempDir(), t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(owner, alias))
	ownerEntry := RepoEntry{Path: alias, Name: "src", Exclude: []string{"owner-only"}}
	gc := &GlobalConfig{
		Repos:    []RepoEntry{{Path: loser, Name: "src"}},
		Projects: map[string]ProjectConfig{"project": {Repos: []RepoEntry{ownerEntry}}},
	}
	assert.Equal(t, &ownerEntry, gc.FindRepoByPath(owner))
	assert.Nil(t, gc.FindRepoByPath(t.TempDir()), "do not fall back to another root with the same prefix")
	assert.Nil(t, gc.FindRepoByPath(""))
	assert.Nil(t, (*GlobalConfig)(nil).FindRepoByPath(owner))
	gc.Repos = append(gc.Repos, ownerEntry)
	assert.Same(t, &gc.Repos[1], gc.FindRepoByPath(owner), "top-level entry keeps precedence over project membership")
}
