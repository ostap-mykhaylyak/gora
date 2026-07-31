package main

import (
	"log/slog"

	"github.com/ostap-mykhaylyak/gora/internal/confd"
	"github.com/ostap-mykhaylyak/gora/internal/firewall"
	"github.com/ostap-mykhaylyak/gora/internal/rewrite"
	"github.com/ostap-mykhaylyak/gora/internal/throttle"
)

// traffic groups the three things gora does to a statement on its way to
// the backend: change it, refuse it, or make it wait its turn. They are
// built together because they are configured together, in the same conf.d
// drop-ins, and reloaded together.
type traffic struct {
	rewriter *rewrite.Rewriter
	firewall *firewall.Firewall
	throttle *throttle.Limiter
}

func newTraffic(rules confd.Set, prefix string, log *slog.Logger) (*traffic, error) {
	rewriter, err := rewrite.New(rules.Rewrites, prefix, log)
	if err != nil {
		return nil, err
	}
	fw, err := firewall.New(rules.Blocks, prefix)
	if err != nil {
		return nil, err
	}
	limiter, err := throttle.New(rules.Throttles, prefix)
	if err != nil {
		return nil, err
	}
	return &traffic{rewriter: rewriter, firewall: fw, throttle: limiter}, nil
}

// setRules swaps every section at once. A section that fails to compile
// leaves all of them untouched, so a reload is all or nothing.
func (t *traffic) setRules(rules confd.Set, prefix string) error {
	if err := t.rewriter.SetRules(rules.Rewrites, prefix); err != nil {
		return err
	}
	if err := t.firewall.SetRules(rules.Blocks, prefix); err != nil {
		return err
	}
	return t.throttle.SetRules(rules.Throttles, prefix)
}

// log reports which traffic features actually have rules, so an empty
// section does not add a line to every startup.
func (t *traffic) log(log *slog.Logger, rulesDir string) {
	if n := t.rewriter.Len(); n > 0 {
		log.Info("query rewriting enabled", "rules", n, "rules_dir", rulesDir)
	}
	if n := t.firewall.Len(); n > 0 {
		log.Info("query firewall enabled", "rules", n, "rules_dir", rulesDir)
	}
	if n := t.throttle.Len(); n > 0 {
		log.Info("query throttling enabled", "rules", n, "rules_dir", rulesDir)
	}
}
