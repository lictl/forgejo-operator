package forgejo

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// strictDurationKeys are the app.ini settings that Forgejo 17 parses strictly as durations
// (forgejo#10273): a value that is set but does not parse stops Forgejo at start, also in
// the bootstrap's `forgejo migrate`. Earlier majors silently fell back to the default.
// Section names are lower case.
var strictDurationKeys = map[string][]string{
	"actions":                {"ZOMBIE_TASK_TIMEOUT", "ENDLESS_TASK_TIMEOUT", "ABANDONED_JOB_TIMEOUT"},
	"authorized_integration": {"REQUEST_TIMEOUT", "CACHE_TTL"},
	"database": {"CONN_MAX_LIFETIME", "CONN_MAX_IDLETIME", "DB_RETRY_BACKOFF", "SLOW_QUERY_THRESHOLD",
		"SLOW_QUERY_TRESHOLD"},
	"indexer":    {keyStartupTimeout},
	"mailer":     {"SENDMAIL_TIMEOUT"},
	"migrations": {"AVATAR_FETCH_TIMEOUT"},
	"moderation": {"KEEP_RESOLVED_REPORTS_FOR"},
	"server": {"LFS_HTTP_AUTH_EXPIRY", "PROXY_PROTOCOL_HEADER_TIMEOUT", "GRACEFUL_HAMMER_TIME", keyStartupTimeout,
		"PER_WRITE_TIMEOUT", "PER_WRITE_PER_KB_TIMEOUT", "STATIC_CACHE_TIME", "SUBPROCESS_TERMINATE_GRACE",
		"SSH_PER_WRITE_TIMEOUT", "SSH_PER_WRITE_PER_KB_TIMEOUT"},
	"service": {"USER_DELETE_WITH_COMMENTS_MAX_TIME"},
}

// keyStartupTimeout is in [indexer] and [server].
const keyStartupTimeout = "STARTUP_TIMEOUT"

// durationPattern is what Forgejo's duration parser accepts: Go's time.ParseDuration syntax
// plus the units d and y, or a bare (signed) 0.
var durationPattern = regexp.MustCompile(`^[-+]?(0|((\d+(\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h|d|y))+)$`)

// InvalidDurations lists the settings of an app.ini (section -> key -> value; the root section
// may be "" or "DEFAULT") that Forgejo 17 and newer refuse to start with, as "[section] KEY".
// Values are not included: they may come from Secrets.
func InvalidDurations(cfg map[string]map[string]string) []string {
	var bad []string
	for section, kv := range cfg {
		keys := strictDurationKeys[strings.ToLower(section)]
		for _, k := range keys {
			v := strings.TrimSpace(kv[k])
			if v != "" && !durationPattern.MatchString(v) {
				bad = append(bad, fmt.Sprintf("[%s] %s", strings.ToLower(section), k))
			}
		}
	}
	slices.Sort(bad)
	return slices.Compact(bad)
}
