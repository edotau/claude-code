package memory

import "github.com/edotau/claude-code/internal/memstore"

// temporary until cli and agent move to memstore
var (
	Dir           = memstore.Dir
	EnsureRepoDir = memstore.EnsureRepoDir
	GitToplevel   = memstore.GitToplevel
	HandoffsDir   = memstore.HandoffsDir
	HomeBankDir   = memstore.HomeBankDir
	NativeDir     = memstore.NativeDir
	ProjectSlug   = memstore.ProjectSlug
	RepoByName    = memstore.RepoByName
	RepoDir       = memstore.RepoDir
	RepoDirs      = memstore.RepoDirs
	StoreDir      = memstore.StoreDir
	WriteOrigin   = memstore.WriteOrigin
)

type Origin = memstore.Origin

const OriginVersion = memstore.OriginVersion
