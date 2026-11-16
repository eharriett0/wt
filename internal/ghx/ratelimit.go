package ghx

// GitHub rate limits (#217). An account's REST quota (5,000 requests an hour)
// is shared by every session and tool on it. When it runs out, every REST call
// fails with a 403 while GraphQL, which has a quota of its own, keeps
// answering. wt used to read that 403 as anything but what it was: `gh auth
// status` validates over REST, so gh "was not authenticated", and `gh pr diff`
// failed, so merge-pr called a 6-line diff EMPTY and pointed at --bypass. A
// call refused for a rate limit is now named as one, with when it lifts.

import (
	"bufio"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateLimitError is a call GitHub refused for a rate limit (#217).
type RateLimitError struct {
	API       string    // "REST" or "GraphQL": each has its own quota
	Secondary bool      // GitHub's secondary limit (too many requests at once), not the hourly quota
	Reset     time.Time // when GitHub takes calls again; zero = not known
	Now       time.Time // when Reset was read, for "in 23m"
	Msg       string    // gh's own first line, for `errors.As` readers that want it
}

func (e *RateLimitError) Error() string {
	what := "GitHub's " + e.API + " API rate limit for this account is used up"
	if e.Secondary {
		what = "GitHub's secondary rate limit refused the call (too many requests in a short time)"
	}
	if e.Reset.IsZero() {
		return what + " (when it lifts is not known)"
	}
	at := e.Reset.UTC().Format("15:04Z")
	if d := e.Reset.Sub(e.Now); d > 0 {
		return what + " (it lifts at " + at + ", in " + roundUp(d) + ")"
	}
	return what + " (it lifted at " + at + ")"
}

// roundUp renders d to the minute, rounded up so "in 0m" never shows for a
// limit that has not lifted yet. Pure.
func roundUp(d time.Duration) string {
	m := int((d + time.Minute - 1) / time.Minute)
	if m < 60 {
		return strconv.Itoa(m) + "m"
	}
	return fmt.Sprintf("%dh%02dm", m/60, m%60)
}

// IsRateLimit reports whether err is, or wraps, a call GitHub refused for a
// rate limit. Retrying such a call a moment later is pointless.
func IsRateLimit(err error) bool {
	var rl *RateLimitError
	return errors.As(err, &rl)
}

var (
	// primaryLimit is GitHub's message for an exhausted quota, REST ("API rate
	// limit exceeded for user ID 1234. If you reach out to GitHub Support…") and
	// GraphQL ("API rate limit exceeded for user ID 1234." / "already exceeded").
	primaryLimit = regexp.MustCompile(`(?i)\bAPI rate limit (?:already )?exceeded\b`)
	// secondaryLimit is its message for the burst limit ("You have exceeded a
	// secondary rate limit…").
	secondaryLimit = regexp.MustCompile(`(?i)\bsecondary rate limit\b`)
	// graphQLErr marks text from the GraphQL API: gh prints its errors as
	// "GraphQL: …", GitHub types a rate-limited one RATE_LIMITED, an HTTP error
	// names the endpoint ("… (https://api.github.com/graphql)"), and gh's GraphQL
	// client (a GH_TOKEN login check) says "non-200 OK status code". Measured
	// (#217 review): `gh api graphql` refused for its quota prints only "gh: API
	// rate limit already exceeded for user ID N." on stderr, with no marker at
	// all, so the caller's own knowledge of the API is the fallback.
	graphQLErr = regexp.MustCompile(`\bGraphQL:|\bRATE_LIMITED\b|/graphql\b|non-200 OK status code`)
)

// The APIs a gh call can use, each with its own quota.
const (
	apiREST    = "REST"
	apiGraphQL = "GraphQL"
)

// rateLimitKind reports whether gh's error text says GitHub refused the call
// for a rate limit, on which API, and whether it is the secondary limit. Pure.
// The text decides the API when it names one (graphQLErr); otherwise it is
// called, the API the caller knows it used ("" = REST: what a bare `gh api`
// path and `gh pr diff` use).
func rateLimitKind(text, called string) (api string, secondary, ok bool) {
	switch {
	case secondaryLimit.MatchString(text):
		secondary = true
	case !primaryLimit.MatchString(text):
		return "", false, false
	}
	switch {
	case graphQLErr.MatchString(text):
		api = apiGraphQL
	case called != "":
		api = called
	default:
		api = apiREST
	}
	return api, secondary, true
}

// rateReset reads when a refused call may be retried from the response
// `gh api --include` printed: its status line and headers, then a blank line,
// then the body. Pure. Retry-After (seconds, the secondary limit's answer)
// wins; otherwise the quota's X-RateLimit-Reset (epoch seconds), and only when
// X-RateLimit-Remaining is 0, since a call that still had quota left was not
// refused for the quota. ok=false when the headers do not say.
func rateReset(include string, now time.Time) (time.Time, bool) {
	h := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(include))
	first := true
	for sc.Scan() {
		ln := strings.TrimRight(sc.Text(), "\r")
		if first {
			first = false
			if !strings.HasPrefix(ln, "HTTP/") {
				return time.Time{}, false // not a response gh printed with --include
			}
			continue
		}
		if ln == "" {
			break // the body starts here
		}
		if k, v, ok := strings.Cut(ln, ":"); ok {
			h[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	if s, err := strconv.Atoi(h["retry-after"]); err == nil && s > 0 {
		return now.Add(time.Duration(s) * time.Second), true
	}
	if h["x-ratelimit-remaining"] != "0" {
		return time.Time{}, false
	}
	if epoch, err := strconv.ParseInt(h["x-ratelimit-reset"], 10, 64); err == nil && epoch > 0 {
		return time.Unix(epoch, 0), true
	}
	return time.Time{}, false
}

// probeRateReset asks host when its REST quota resets: the headers of a
// refused call (#217). Not the rate_limit endpoint: during the incident it
// reported 5,000 calls remaining while GitHub refused every call. "" = the
// host gh picks. A test swaps it out.
var probeRateReset = liveProbeRateReset

var (
	resetMu   sync.Mutex
	resetMemo = map[string]resetAnswer{} // per host
)

type resetAnswer struct {
	reset time.Time // zero = asked, not known
	at    time.Time
}

// liveProbeRateReset GETs the API root with --include: one call, which GitHub
// refuses (and does not count) while the quota is used up. Asked once per host
// per authTTL, like the auth check: merge-pr hits the limit on several reads in
// a row, and a long-lived `wt mcp` must not keep a reset that has passed.
func liveProbeRateReset(host string) time.Time {
	resetMu.Lock()
	defer resetMu.Unlock()
	now := time.Now()
	if a, ok := resetMemo[host]; ok && authFresh(a.at, now) {
		return a.reset
	}
	args := []string{"api", "--include", "/"}
	if host != "" {
		args = append(args, "--hostname", host)
	}
	out, _ := exec.Command("gh", args...).Output() // refused: exit 1, the response still on stdout
	t, _ := rateReset(string(out), now)
	resetMemo[host] = resetAnswer{reset: t, at: now}
	return t
}

// rateLimited returns the RateLimitError for gh's error text when GitHub
// refused the call for a rate limit. host is where the call went ("" = the
// host gh picks), called the API the caller used (rateLimitKind). Only the
// REST quota is probed for its reset: GraphQL's lifts on its own clock, which
// a REST call's headers do not carry, and a probe during a secondary limit
// only adds to the burst it is refusing.
func rateLimited(host, called, text string) (*RateLimitError, bool) {
	api, secondary, ok := rateLimitKind(text, called)
	if !ok {
		return nil, false
	}
	e := &RateLimitError{API: api, Secondary: secondary, Msg: firstLine(text), Now: time.Now()}
	if api == apiREST && !secondary {
		e.Reset = probeRateReset(host)
	}
	return e, true
}

// firstLine is s's first line, trimmed. Pure.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}
