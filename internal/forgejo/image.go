package forgejo

import (
	"strconv"
	"strings"
)

// ImageLayout describes how the operator runs the official Forgejo images. Both the root
// image (codeberg.org/forgejo/forgejo:<v>) and the "-rootless" flavor ship the same binary
// at /app/gitea/gitea, so the operator bypasses the image entrypoints (s6 / docker-setup.sh)
// and runs "forgejo web --config <ConfigPath>" directly as uid 1000 with Forgejo's built-in
// SSH server. That gives one non-root layout for both flavors.
type ImageLayout struct {
	Rootless bool
	// DataMountPath is where the data PVC is mounted.
	DataMountPath string
	// ConfigDir is where the "<name>-config" Secret is mounted read-only.
	ConfigDir string
	// ConfigPath is the app.ini location passed with --config.
	ConfigPath string
	// WorkPath is WORK_PATH / APP_DATA_PATH / GITEA_WORK_DIR.
	WorkPath string
	// CustomPath is GITEA_CUSTOM (templates, public assets).
	CustomPath string
	// HomePath is $HOME of the git user (gitconfig, .ssh).
	HomePath string
	// RepoRoot is [repository] ROOT.
	RepoRoot string
	// LFSPath is [lfs] PATH.
	LFSPath string
	// TmpPath is an emptyDir for temporary files.
	TmpPath string
	// SSHContainerPort is the built-in SSH server's SSH_LISTEN_PORT.
	SSHContainerPort int32
	// SSHHostKeysDir is where the Secret of SSH host keys is mounted read-only.
	SSHHostKeysDir string
	// S3CADir is where the CA certificate of spec.objectStorage.s3.tls.caSecret is mounted
	// read-only; it is added to SSL_CERT_DIR after SystemCertDir.
	S3CADir string
	// SystemCertDir is the image's directory of trusted CA certificates (Alpine).
	SystemCertDir string
	// HTTPContainerPort is HTTP_PORT.
	HTTPContainerPort int32
	// UID/GID Forgejo runs as (user "git" in both images).
	UID int64
	GID int64
	// RunUser is RUN_USER; must match the uid's passwd entry.
	RunUser string
	// CLI is the Forgejo binary.
	CLI string
	// InternalSocketPath is [server] INTERNAL_LISTENER_PATH when Traits.InternalSocket: in a
	// directory Forgejo creates (and chmods) itself on the per-pod TmpPath volume.
	InternalSocketPath string
	// Traits of the image's Forgejo major (Versions.Traits).
	Traits Traits
}

// LayoutForImage returns the runtime layout for the given official image reference.
func LayoutForImage(image string) ImageLayout {
	v, ok := ImageVersion(image)
	return ImageLayout{
		Rootless:          IsRootless(image),
		DataMountPath:     "/data",
		ConfigDir:         "/etc/forgejo",
		ConfigPath:        "/etc/forgejo/app.ini",
		WorkPath:          "/data/gitea",
		CustomPath:        "/data/gitea/custom",
		HomePath:          "/data/git",
		RepoRoot:          "/data/git/repositories",
		LFSPath:           "/data/git/lfs",
		TmpPath:           "/tmp",
		SSHContainerPort:  2222,
		SSHHostKeysDir:    "/etc/forgejo-ssh",
		S3CADir:           "/etc/forgejo-s3-ca",
		SystemCertDir:     "/etc/ssl/certs",
		HTTPContainerPort: 3000,
		UID:               1000,
		GID:               1000,
		RunUser:           "git",
		CLI:               "/app/gitea/gitea",
		// Short enough for the 108-byte limit of a UNIX socket path.
		InternalSocketPath: "/tmp/forgejo-run/forgejo.sock",
		Traits:             Versions.Traits(v, ok),
	}
}

// ImageTag returns the tag of an image reference ("" when absent or digest-only).
func ImageTag(image string) string {
	ref := image
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon > slash {
		return ref[colon+1:]
	}
	return ""
}

// IsRootless reports whether the image is the "-rootless" flavor.
func IsRootless(image string) bool {
	return strings.Contains(ImageTag(image), "rootless")
}

// Version is a parsed Forgejo version (major.minor.patch). Missing parts are -1, meaning
// "floating" (e.g. tag "16" tracks the latest 16.x.y).
type Version struct {
	Major, Minor, Patch int
}

// ParseVersion extracts a version from a tag or version string such as "16",
// "16.0.6", "v16.0.6", "16.0-rootless" or "16.0.6+gitea-1.22.0".
func ParseVersion(s string) (Version, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+ "); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return Version{}, false
	}
	parts := strings.Split(s, ".")
	v := Version{Major: -1, Minor: -1, Patch: -1}
	for i, p := range parts {
		if i > 2 {
			break
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return Version{}, false
		}
		switch i {
		case 0:
			v.Major = n
		case 1:
			v.Minor = n
		case 2:
			v.Patch = n
		}
	}
	return v, true
}

// ImageVersion parses the version from an image tag.
func ImageVersion(image string) (Version, bool) {
	return ParseVersion(ImageTag(image))
}

// Compare returns -1, 0 or 1. Floating parts (-1) compare equal to anything.
func (v Version) Compare(o Version) int {
	pairs := [][2]int{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}}
	for _, p := range pairs {
		if p[0] < 0 || p[1] < 0 {
			return 0
		}
		if p[0] < p[1] {
			return -1
		}
		if p[0] > p[1] {
			return 1
		}
	}
	return 0
}

func (v Version) String() string {
	out := strconv.Itoa(v.Major)
	if v.Minor >= 0 {
		out += "." + strconv.Itoa(v.Minor)
	}
	if v.Patch >= 0 {
		out += "." + strconv.Itoa(v.Patch)
	}
	return out
}
