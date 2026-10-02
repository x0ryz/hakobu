package config

// The images hakobu runs itself, pinned by digest: a tag can be moved to
// other content, a digest can't. Each digest is the multi-arch index, so
// amd64 and arm64 get the same release. To update one, pick the new tag
// and run `docker buildx imagetools inspect <image>:<tag>` for its digest.
var (
	PostgresImage = envString("HAKOBU_POSTGRES_IMAGE",
		"postgres:18.6@sha256:5a5a84b19854a9ffaa54082c166ff4ec27473a361e496e5ea167f298f2da9722")
)

const (
	CloudflaredImage = "cloudflare/cloudflared:2026.9.3@sha256:072c067d25ccbe61d46e18f0d0723255f2bb5304f7317caa95b27031520ff92c"
	// BuildKit runs Railpack builds; keep it in step with the Railpack
	// version install.sh installs.
	BuildKitImage = "moby/buildkit:v0.33.0@sha256:6c2fa84a6b61ccd72899dde4239f8d5717f05f9a8ca6f3cad185fb1a95a94de3"
)
