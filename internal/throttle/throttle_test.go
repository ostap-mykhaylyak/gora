package throttle

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ostap-mykhaylyak/gora/internal/config"
)

func newLimiter(t *testing.T, rules []Rule) *Limiter {
	t.Helper()
	l, err := New(rules, "wp_")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l
}

const search = "SELECT ID FROM wp_posts WHERE post_title LIKE '%chair%'"

func searchRule(maxConcurrent int, wait time.Duration) Rule {
	return Rule{
		Name:          "heavy-search",
		Match:         "(?i)LIKE '%",
		MaxConcurrent: maxConcurrent,
		Wait:          config.Duration(wait),
	}
}

// Up to the limit, statements run untouched.
func TestUnderTheLimitNothingWaits(t *testing.T) {
	l := newLimiter(t, []Rule{searchRule(2, 0)})

	first, _, err := l.Acquire(search)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	second, _, err := l.Acquire(search)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	first()
	second()

	if st := l.Stat(); st.Rejects != 0 || st.Waits != 0 {
		t.Fatalf("stats = %+v, want nothing counted", st)
	}
}

// Past the limit, with no wait configured, the excess is refused straight
// away: the point is to shed load, not to queue it.
func TestOverTheLimitIsRefused(t *testing.T) {
	l := newLimiter(t, []Rule{searchRule(1, 0)})

	release, _, err := l.Acquire(search)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()

	start := time.Now()
	_, rule, err := l.Acquire(search)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("error = %v, want ErrBusy", err)
	}
	if rule != "heavy-search" {
		t.Fatalf("rule = %q, want the rule that refused it", rule)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("refusing took %s, want it immediate", elapsed)
	}
	if st := l.Stat(); st.Rejects != 1 {
		t.Fatalf("rejects = %d, want 1", st.Rejects)
	}
}

// With a wait configured, a slot freed in time is taken.
func TestWaitsForASlot(t *testing.T) {
	l := newLimiter(t, []Rule{searchRule(1, 500*time.Millisecond)})

	release, _, err := l.Acquire(search)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		release()
	}()

	second, _, err := l.Acquire(search)
	if err != nil {
		t.Fatalf("Acquire waited and still failed: %v", err)
	}
	second()

	if st := l.Stat(); st.Waits != 1 || st.Rejects != 0 {
		t.Fatalf("stats = %+v, want one wait and no rejects", st)
	}
}

// And a slot that never frees up ends in a refusal, not in a client hanging
// forever.
func TestWaitExpires(t *testing.T) {
	l := newLimiter(t, []Rule{searchRule(1, 80*time.Millisecond)})

	release, _, err := l.Acquire(search)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()

	start := time.Now()
	if _, _, err := l.Acquire(search); !errors.Is(err, ErrBusy) {
		t.Fatalf("error = %v, want ErrBusy", err)
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("gave up after %s, before the configured wait", elapsed)
	}
}

// Limits are per statement shape: one runaway query is held back without
// touching everything else the same rule matches.
func TestLimitsArePerDigest(t *testing.T) {
	l := newLimiter(t, []Rule{searchRule(1, 0)})

	release, _, err := l.Acquire("SELECT ID FROM wp_posts WHERE post_title LIKE '%chair%'")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()

	// Same shape, different literal: same digest, so it is refused.
	if _, _, err := l.Acquire("SELECT ID FROM wp_posts WHERE post_title LIKE '%table%'"); !errors.Is(err, ErrBusy) {
		t.Fatalf("error = %v, want ErrBusy: the literal changed the digest", err)
	}

	// A different statement that the same rule matches has its own slot.
	other, _, err := l.Acquire("SELECT ID FROM wp_comments WHERE comment_content LIKE '%spam%'")
	if err != nil {
		t.Fatalf("a different statement was refused: %v", err)
	}
	other()
}

// Statements no rule matches are not slowed down at all.
func TestUnmatchedStatementsRunFreely(t *testing.T) {
	l := newLimiter(t, []Rule{searchRule(1, 0)})

	for i := 0; i < 10; i++ {
		release, rule, err := l.Acquire("SELECT 1")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if rule != "" {
			t.Fatalf("rule = %q, want none", rule)
		}
		release()
	}
}

func TestNoRulesIsCheap(t *testing.T) {
	l := newLimiter(t, nil)
	release, _, err := l.Acquire(search)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()
}

// The limit holds under concurrency, which is the only condition that
// matters.
func TestConcurrentAcquisitionsRespectTheLimit(t *testing.T) {
	const limit = 3
	l := newLimiter(t, []Rule{searchRule(limit, time.Second)})

	var (
		mu      sync.Mutex
		current int
		peak    int
		wg      sync.WaitGroup
	)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, _, err := l.Acquire(search)
			if err != nil {
				return // refused, which is allowed
			}
			mu.Lock()
			current++
			if current > peak {
				peak = current
			}
			mu.Unlock()

			time.Sleep(10 * time.Millisecond)

			mu.Lock()
			current--
			mu.Unlock()
			release()
		}()
	}
	wg.Wait()

	if peak > limit {
		t.Fatalf("%d executions ran at once, want at most %d", peak, limit)
	}
}

func TestRuleValidate(t *testing.T) {
	if err := (Rule{Match: "^SELECT", MaxConcurrent: 1}).Validate(); err == nil {
		t.Error("a throttle rule without a name was accepted")
	}
	if err := (Rule{Name: "x", MaxConcurrent: 1}).Validate(); err == nil {
		t.Error("a throttle rule without a match expression was accepted")
	}
	if err := (Rule{Name: "x", Match: "^SELECT"}).Validate(); err == nil {
		t.Error("a throttle rule without a limit was accepted")
	}
	if err := (Rule{Name: "x", Match: "^SELECT", MaxConcurrent: 1}).Validate(); err != nil {
		t.Errorf("a valid rule was refused: %v", err)
	}
}
