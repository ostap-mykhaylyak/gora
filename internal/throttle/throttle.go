// Package throttle bounds how many copies of the same statement run at once.
//
// A blocked query is a decision; a throttled one is a brake. When a plugin
// starts issuing the same expensive query from every worker, the database
// does not fall over because the query is wrong — it falls over because
// forty copies of it run at the same time. Letting two through and making
// the rest wait keeps the site slow instead of down.
//
// Limits apply per statement digest, not per rule: literals are normalised
// away, so one runaway query is held back without touching everything else
// the same rule happens to match.
package throttle

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ostap-mykhaylyak/gora/internal/config"
	"github.com/ostap-mykhaylyak/gora/internal/statement"
)

// maxDigests caps how many distinct statement shapes one rule tracks.
// Past it new shapes run unthrottled: a limiter that grows without bound is
// a worse problem than the one it was added to solve.
const maxDigests = 4096

// ErrBusy is returned when a statement cannot get a slot in time.
var ErrBusy = errors.New("too many concurrent executions of this statement")

// Rule limits the statements matching an expression.
type Rule struct {
	Name  string `yaml:"name"`
	Match string `yaml:"match"`
	// MaxConcurrent is how many executions of one statement shape may run
	// at the same time.
	MaxConcurrent int `yaml:"max_concurrent"`
	// Wait is how long an execution waits for a slot before being refused.
	// Zero refuses immediately, which is what you want when the point is to
	// shed load rather than to queue it.
	Wait config.Duration `yaml:"wait"`

	re *regexp.Regexp
}

// Validate checks a rule as written in a drop-in, before any prefix is
// known.
func (r Rule) Validate() error {
	if r.Name == "" {
		return fmt.Errorf("a throttle rule has no name")
	}
	if r.Match == "" {
		return fmt.Errorf("throttle rule %q has no match expression", r.Name)
	}
	probe := strings.ReplaceAll(r.Match, config.PrefixPlaceholder, "wp_")
	if _, err := regexp.Compile(probe); err != nil {
		return fmt.Errorf("throttle rule %q has an invalid match expression: %w", r.Name, err)
	}
	if r.MaxConcurrent < 1 {
		return fmt.Errorf("throttle rule %q needs max_concurrent >= 1", r.Name)
	}
	if r.Wait < 0 {
		return fmt.Errorf("throttle rule %q has a negative wait", r.Name)
	}
	return nil
}

// limiter holds the semaphores of one rule, one per statement digest.
type limiter struct {
	Rule

	mu    sync.Mutex
	slots map[string]chan struct{}
}

func (l *limiter) semaphore(digest string) chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()

	if sem, ok := l.slots[digest]; ok {
		return sem
	}
	if len(l.slots) >= maxDigests {
		return nil
	}
	sem := make(chan struct{}, l.MaxConcurrent)
	l.slots[digest] = sem
	return sem
}

// Limiter applies the configured rules. It is safe for concurrent use and
// its rules can be replaced while gora runs.
type Limiter struct {
	rules atomic.Pointer[[]*limiter]

	waits    atomic.Uint64
	rejects  atomic.Uint64
	untraced atomic.Uint64
}

// Stats counts what the limiter has done since gora started.
type Stats struct {
	Rules   int    `json:"rules"`
	Waits   uint64 `json:"waits"`
	Rejects uint64 `json:"rejects"`
}

// New compiles the rules against the table prefix.
func New(rules []Rule, prefix string) (*Limiter, error) {
	l := &Limiter{}
	if err := l.SetRules(rules, prefix); err != nil {
		return nil, err
	}
	return l, nil
}

// SetRules replaces the rules atomically (hot reload). On error nothing
// changes. Replacing the rules also forgets the slots in use, which is
// safe: the executions holding them release into semaphores nobody consults
// any more.
func (l *Limiter) SetRules(rules []Rule, prefix string) error {
	compiled := make([]*limiter, 0, len(rules))
	for _, rule := range rules {
		expr, err := config.ExpandPrefix(rule.Match, prefix)
		if err != nil {
			return fmt.Errorf("throttle rule %q: %w", rule.Name, err)
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return fmt.Errorf("throttle rule %q: %w", rule.Name, err)
		}
		rule.re = re
		compiled = append(compiled, &limiter{Rule: rule, slots: make(map[string]chan struct{})})
	}
	l.rules.Store(&compiled)
	return nil
}

// Len returns how many rules are active.
func (l *Limiter) Len() int {
	rules := l.rules.Load()
	if rules == nil {
		return 0
	}
	return len(*rules)
}

// Stat returns the counters.
func (l *Limiter) Stat() Stats {
	return Stats{
		Rules:   l.Len(),
		Waits:   l.waits.Load(),
		Rejects: l.rejects.Load(),
	}
}

// noop is what an unthrottled statement gets back.
func noop() {}

// Acquire takes a slot for the statement, waiting up to the rule's wait.
// The returned function releases it and must be called; on ErrBusy the
// statement must not run. The rule that applied is named for the log.
func (l *Limiter) Acquire(query string) (release func(), rule string, err error) {
	rules := l.rules.Load()
	if rules == nil || len(*rules) == 0 {
		return noop, "", nil
	}

	for _, lim := range *rules {
		if !lim.re.MatchString(query) {
			continue
		}
		sem := lim.semaphore(statement.Fingerprint(query))
		if sem == nil {
			l.untraced.Add(1)
			return noop, lim.Name, nil
		}

		select {
		case sem <- struct{}{}:
			return func() { <-sem }, lim.Name, nil
		default:
		}

		if lim.Wait <= 0 {
			l.rejects.Add(1)
			return nil, lim.Name, ErrBusy
		}

		l.waits.Add(1)
		timer := time.NewTimer(lim.Wait.Std())
		defer timer.Stop()
		select {
		case sem <- struct{}{}:
			return func() { <-sem }, lim.Name, nil
		case <-timer.C:
			l.rejects.Add(1)
			return nil, lim.Name, ErrBusy
		}
	}
	return noop, "", nil
}
