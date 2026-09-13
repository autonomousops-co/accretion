package worker

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"

	"github.com/autonomousops-co/accretion/internal/config"
)

// logLevelRank defines the severity ordering used by the level threshold.
// Higher numbers are more severe. Keys are the canonical lowercase levels
// returned by detectLogLevel plus the config-only tiers (trace, notice) that are
// valid min_level thresholds but intentionally not keyword-detected.
var logLevelRank = map[string]int{
	"trace":    0,
	"debug":    1,
	"info":     2,
	"notice":   3,
	"warn":     4,
	"error":    5,
	"critical": 6,
	"alert":    7,
	"emerg":    8,
}

// levelSynonyms maps non-canonical level spellings — common in non-Go loggers
// (Python "warning"/"critical", log4j "fatal", zap "panic"/"dpanic", syslog
// "err"/"emergency") — onto the canonical names in logLevelRank, so min_level
// can be configured with whatever vocabulary the operator's services use.
var levelSynonyms = map[string]string{
	"warning":   "warn",
	"err":       "error",
	"crit":      "critical",
	"fatal":     "critical",
	"panic":     "critical",
	"dpanic":    "critical",
	"emergency": "emerg",
}

// normalizeLevel lowercases a level string and maps any synonym to its canonical
// name. Canonical names pass through unchanged.
func normalizeLevel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if canonical, ok := levelSynonyms[s]; ok {
		return canonical
	}
	return s
}

// LogFilter decides whether a log line should be ingested (embedded + stored).
// It is built once from a config.LogFilterConfig and is safe for concurrent use
// because all fields are read-only after construction. Compiling the denylist
// once here keeps the per-line hot path (the expensive embedding call) cheap.
type LogFilter struct {
	denylist   []*regexp.Regexp
	minRank    int
	sampleRate float64
	hasMinRank bool
}

// NewLogFilter builds a LogFilter from cfg. It fails loudly on a bad denylist
// pattern or unknown min level so a misconfiguration surfaces at worker startup
// rather than silently ingesting (or dropping) everything. With the zero-value
// config the returned filter is a no-op that allows every line.
func NewLogFilter(cfg config.LogFilterConfig) (*LogFilter, error) {
	f := &LogFilter{sampleRate: cfg.SampleRate}

	if cfg.MinLevel != "" {
		rank, ok := logLevelRank[normalizeLevel(cfg.MinLevel)]
		if !ok {
			return nil, fmt.Errorf("invalid log filter min_level %q (want one of trace, debug, "+
				"info, notice, warn, error, critical, alert, emerg; synonyms "+
				"warning/err/crit/fatal/panic/emergency accepted)", cfg.MinLevel)
		}
		f.minRank = rank
		f.hasMinRank = true
	}

	f.denylist = make([]*regexp.Regexp, 0, len(cfg.Denylist))
	for _, pattern := range cfg.Denylist {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid log filter denylist pattern %q: %w", pattern, err)
		}
		f.denylist = append(f.denylist, re)
	}

	return f, nil
}

// Allow reports whether a log line should be ingested. message drives the
// denylist and level checks; sampleKey is a per-occurrence key (e.g. the
// content hash) used only when sampling is enabled — a stable-per-line value
// gives each occurrence an independent keep/drop decision.
//
// A line is dropped when it matches any denylist pattern, when its detected
// level is below the configured minimum (undetectable levels fail open and are
// kept), or when sampling is enabled and the line falls outside the kept
// fraction. A nil filter (or the zero-value config) allows everything.
func (f *LogFilter) Allow(message, sampleKey string) bool {
	if f == nil {
		return true
	}

	for _, re := range f.denylist {
		if re.MatchString(message) {
			return false
		}
	}

	if f.hasMinRank {
		if level, found := detectLogLevel(message); found && logLevelRank[level] < f.minRank {
			return false
		}
	}

	if f.sampleRate > 0 && f.sampleRate < 1 && sampleKey != "" {
		if sampledOut(sampleKey, f.sampleRate) {
			return false
		}
	}

	return true
}

// sampledOut maps key to a uniform fraction in [0,1) and reports whether it
// falls outside the kept fraction (rate). It is deterministic, so the same line
// occurrence is always kept or always dropped.
func sampledOut(key string, rate float64) bool {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key)) // hash.Write never returns an error
	const buckets = 1_000_000
	frac := float64(h.Sum64()%buckets) / float64(buckets)
	return frac >= rate
}
