// Package selfupdate is wt's throttled staleness check (eharriett0/wt#54): once
// per interval it compares the installed binary's build commit against the
// remote HEAD and nudges the operator to reinstall if behind. The throttle is
// the whole point — a fast local tool used many times a day must not pay a
// network round-trip on every invocation, so a stamp file gates the actual
// check (and its git ls-remote) to at most once per interval; every other run
// reads one timestamp and does ZERO network I/O. Best-effort: any error, a dev
// build, or being offline is a silent no-op.
package selfupdate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// DefaultInterval is how often the network check actually runs.
const DefaultInterval = 24 * time.Hour

// DefaultStampPath is ~/.wt/update-check — the last-check throttle timestamp.
func DefaultStampPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".wt", "update-check")
}

// Elapsed reports whether interval has passed since the unix-seconds timestamp
// in stamp. Empty/unparseable → true (a first run always checks). Pure.
func Elapsed(stamp string, now time.Time, interval time.Duration) bool {
	sec, err := strconv.ParseInt(strings.TrimSpace(stamp), 10, 64)
	if err != nil {
		return true
	}
	return now.Sub(time.Unix(sec, 0)) >= interval
}

// LocalBuild returns the installed binary's module path + build commit from the
// Go build info. commit is vcs.revision (a full SHA) when present, else the
// 12+-hex suffix of the module pseudo-version. Empty when unavailable (a plain
// `go build` / `dev` binary), which callers treat as "can't compare".
func LocalBuild() (modulePath, commit string) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "", ""
	}
	modulePath = bi.Main.Path
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			return modulePath, s.Value
		}
	}
	// pseudo-version fallback: vX.Y.Z-0.YYYYMMDDhhmmss-<12hex>
	if v := bi.Main.Version; strings.Contains(v, "-") {
		parts := strings.Split(v, "-")
		if last := parts[len(parts)-1]; len(last) >= 12 {
			return modulePath, last
		}
	}
	return modulePath, ""
}

// RemoteHead runs `git ls-remote https://<modulePath> HEAD` and returns the
// remote HEAD commit SHA. Timeout-bounded; any error (offline, no git, private
// repo without creds) returns "".
func RemoteHead(modulePath string, timeout time.Duration) string {
	if modulePath == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "ls-remote", "https://"+modulePath, "HEAD").Output()
	if err != nil {
		return ""
	}
	if fields := strings.Fields(string(out)); len(fields) > 0 {
		return fields[0]
	}
	return ""
}

// Behind reports whether local and remote refer to DIFFERENT commits (both
// non-empty). Compared by common prefix — local may be a 12-hex pseudo-version
// suffix while remote is a full SHA. Pure.
func Behind(localCommit, remoteCommit string) bool {
	l, r := strings.TrimSpace(localCommit), strings.TrimSpace(remoteCommit)
	if l == "" || r == "" {
		return false
	}
	n := min(len(l), len(r))
	return l[:n] != r[:n]
}

// Check performs a THROTTLED update check. If interval hasn't elapsed since the
// stamp, it returns "" doing NO network I/O (the hot path). Otherwise it stamps
// now (so failures also throttle), compares the local build commit to the
// remote HEAD, and returns a one-line nudge if behind — "" on match / any error
// / a dev build. Never errors; safe to call on every command.
func Check(stampPath string, now time.Time, interval time.Duration) string {
	data, _ := os.ReadFile(stampPath)
	if !Elapsed(string(data), now, interval) {
		return ""
	}
	_ = writeStamp(stampPath, now)

	modulePath, localCommit := LocalBuild()
	if localCommit == "" {
		return ""
	}
	if !Behind(localCommit, RemoteHead(modulePath, 3*time.Second)) {
		return ""
	}
	return "wt: a newer version is available — reinstall with `go install " + modulePath +
		"@latest` (or from your checkout: `go install .`).  Silence with WT_NO_UPDATE_CHECK=1."
}

func writeStamp(path string, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.FormatInt(now.Unix(), 10)), 0o644)
}
