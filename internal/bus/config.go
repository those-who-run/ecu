package bus

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// DefaultURL is the NATS server ecu publishes to when neither --nats nor
// TWR_NATS_URL names one.
const DefaultURL = "nats://127.0.0.1:4222"

// DefaultSrc is the <src> ecu publishes under when neither --src nor TWR_SRC names
// one: the PS5, live.
const DefaultSrc = "live"

// Config is where ecu publishes and under which source.
type Config struct {
	// URL is the NATS server: --nats, or TWR_NATS_URL, or DefaultURL.
	URL string
	// Src is the <src> token of every subject: --src, or TWR_SRC, or DefaultSrc.
	Src string
}

// Flags registers --nats and --src on fs, each defaulting to its environment
// variable through getenv, then to DefaultURL and DefaultSrc. The values land in
// c when fs is parsed; Validate checks them.
func (c *Config) Flags(fs *flag.FlagSet, getenv func(string) string) {
	fs.StringVar(&c.URL, "nats", or(getenv("TWR_NATS_URL"), DefaultURL), "the NATS server to publish to (TWR_NATS_URL)")
	fs.StringVar(&c.Src, "src", or(getenv("TWR_SRC"), DefaultSrc),
		"the source token in every subject: live, parity or replay-<run> (TWR_SRC)")
}

func or(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// srcRule is what a <src> may be (paddock#34, R7): one NATS subject token, live,
// parity or replay-<run>. The run is letters, digits, _ and -, so no ., * or >,
// which would make the subject more than one token or a wildcard, and no white
// space.
var srcRule = regexp.MustCompile(`^(live|parity|replay-[A-Za-z0-9][A-Za-z0-9_-]{0,63})$`)

// ErrSrc is Validate's error for a <src> that breaks the rule.
var ErrSrc = errors.New(`a src is one subject token: "live", "parity" or "replay-<run>", ` +
	`the run of letters, digits, "_" and "-" (at most 64), with no ".", "*", ">" or white space`)

// Validate refuses a <src> that is not one subject token of the allowed forms, and
// a URL with nothing in it.
func (c Config) Validate() error {
	if !srcRule.MatchString(c.Src) {
		return fmt.Errorf("src %q: %w", c.Src, ErrSrc)
	}
	if strings.TrimSpace(c.URL) == "" {
		return errors.New("nats: no server URL")
	}
	return nil
}

// redacted is the URL list with any user and password left out, for the log: a log
// is read by more people than hold the credentials.
func redacted(urls string) string {
	parts := strings.Split(urls, ",")
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if u, err := url.Parse(p); err == nil && u.User != nil {
			u.User = nil
			p = u.String()
		} else if at := strings.LastIndex(p, "@"); at >= 0 {
			p = p[at+1:]
		}
		parts[i] = p
	}
	return strings.Join(parts, ",")
}
