package forgejo

import (
	"strconv"
	"strings"
)

// ImageLayout describes where the official Forgejo image flavors keep their files.
// The root image runs s6 (sshd + forgejo as uid 1000); the rootless image runs forgejo
// directly as uid 1000 with the built-in SSH server.
type ImageLayout struct {
	Rootless bool
	// DataMountPath is where the data PVC is mounted.
	DataMountPath string
	// ConfigPath is the app.ini location Forgejo reads.
	ConfigPath string
	// WorkPath is APP_DATA_PATH / GITEA_WORK_DIR.
	WorkPath string
	// CustomPath is GITEA_CUSTOM.
	CustomPath string
	// RepoRoot is [repository] ROOT.
	RepoRoot string
	// LFSPath is [lfs] PATH.
	LFSPath string
	// SSHContainerPort is the port sshd / the built-in SSH server listens on.
	SSHContainerPort int32
	// HTTPContainerPort is HTTP_PORT.
	HTTPContainerPort int32
	// UID/GID Forgejo runs as.
	UID int64
	GID int64
	// CLI is the Forgejo binary to call for admin commands.
	CLI string
}

// LayoutForImage returns the file layout of the given official image reference.
func LayoutForImage(image string) ImageLayout {
	if IsRootless(image) {
		return ImageLayout{
			Rootless:          true,
			DataMountPath:     "/var/lib/gitea",
			ConfigPath:        "/etc/gitea/app.ini",
			WorkPath:          "/var/lib/gitea",
			CustomPath:        "/var/lib/gitea/custom",
			RepoRoot:          "/var/lib/gitea/git/repositories",
			LFSPath:           "/var/lib/gitea/git/lfs",
			SSHContainerPort:  2222,
			HTTPContainerPort: 3000,
			UID:               1000,
			GID:               1000,
			CLI:               "/usr/local/bin/forgejo",
		}
	}
	return ImageLayout{
		DataMountPath:     "/data",
		ConfigPath:        "/data/gitea/conf/app.ini",
		WorkPath:          "/data/gitea",
		CustomPath:        "/data/gitea",
		RepoRoot:          "/data/git/repositories",
		LFSPath:           "/data/git/lfs",
		SSHContainerPort:  22,
		HTTPContainerPort: 3000,
		UID:               1000,
		GID:               1000,
		CLI:               "/usr/local/bin/forgejo",
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
