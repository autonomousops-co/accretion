// Package nsscope provides the live namespace scope for a collector data source.
//
// Namespace scoping is configured as a config.NamespaceSelector, which may name
// namespaces directly or select them by label / the "*" all-namespaces sentinel.
// The latter forms have to be resolved against the Kubernetes API, and the set
// they resolve to changes as namespaces are created, deleted, and relabeled.
//
// A Scope keeps that resolution current WITHOUT making callers pay for it:
// resolution happens on a background ticker, and reads (Namespaces, Allows)
// serve the last resolved snapshot from memory. This split is the point of the
// package — Allows runs per record on the trace worker's push path and the
// collector's namespace check runs inside informer callbacks, so a read must
// never block on a Kubernetes round-trip.
package nsscope

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/logger"
)

// refreshTimeout bounds a single background re-resolution, so a hung API server
// cannot wedge the refresh goroutine and leave the scope frozen forever.
const refreshTimeout = 30 * time.Second

// Scope provides the current namespace scope for a data source. Reads are cheap,
// non-blocking snapshot lookups — never an API call.
type Scope interface {
	// Namespaces returns the concrete namespaces a polled source should iterate.
	// The returned slice is the caller's to keep; mutating it does not affect the
	// Scope.
	Namespaces() []string
	// Allows reports whether a push/event source should ingest a record from ns.
	Allows(ns string) bool
	// Contains reports whether ns is in the resolved namespace set. It answers
	// the same question as scanning Namespaces() but without copying the set,
	// for callers on per-event paths (informer callbacks, log-work dispatch).
	// Unlike Allows it applies no empty-set interpretation — an empty scope
	// contains nothing — so callers keep full control of their own fail-open or
	// fail-closed policy (see IsEmpty).
	Contains(ns string) bool
	// IsEmpty reports whether the resolved namespace set is empty. Paired with
	// Contains it lets a caller express "scoped to nothing" separately from
	// "scoped, but not to this namespace" — a distinction the log worker's
	// fail-open guard depends on.
	IsEmpty() bool
	// IsStatic reports whether the namespace set is an explicit list that
	// never changes, as opposed to a label or "*" selector that is re-resolved
	// in the background. A static scope lets a watcher open one informer per
	// namespace instead of one over the whole cluster (#96).
	IsStatic() bool
}

// New returns a Scope for sel.
//
// When sel needs no expansion — a purely name-based config — New returns a
// static Scope that makes ZERO API calls and starts no goroutine. lister may be
// nil in that case, so callers that would otherwise have to construct a
// Kubernetes client just to satisfy this signature do not need one.
//
// Otherwise New resolves sel once and RETURNS ANY ERROR: a misconfigured or
// unreachable scope must fail fast at startup rather than silently degrade. It
// then refreshes the resolution every interval in the background until ctx is
// done. Refresh failures are logged and the last good snapshot is kept, so an
// API blip neither stops ingestion nor widens scope. An interval <= 0 disables
// refreshing, leaving a single startup snapshot.
func New(
	ctx context.Context,
	sel config.NamespaceSelector,
	lister config.NamespaceLister,
	interval time.Duration,
) (Scope, error) {
	if !sel.NeedsExpansion() {
		return newStatic(sel), nil
	}

	expanded, err := sel.Expand(ctx, lister)
	if err != nil {
		return nil, fmt.Errorf("resolving namespace scope: %w", err)
	}

	if interval <= 0 {
		logger.Logger.Info("Namespace scope refresh disabled; using startup snapshot",
			zap.Strings("namespaces", expanded.Resolve()),
		)
		return newStatic(expanded), nil
	}

	s := &refreshingScope{source: sel, lister: lister}
	s.publish(expanded)

	go s.refreshLoop(ctx, interval)

	return s, nil
}

// Static returns a Scope serving sel as-is, with no resolution and no refresh.
// sel must already be name-based; label selectors and the "*" sentinel are not
// resolved. It is intended for tests and for callers that have their own
// resolved selector.
func Static(sel config.NamespaceSelector) Scope { return newStatic(sel) }

// staticScope serves a fixed, already-resolved selector.
type staticScope struct {
	selector   config.NamespaceSelector
	namespaces []string
}

func newStatic(sel config.NamespaceSelector) *staticScope {
	return &staticScope{selector: sel, namespaces: sel.Resolve()}
}

func (s *staticScope) Namespaces() []string { return slices.Clone(s.namespaces) }

func (s *staticScope) Allows(ns string) bool { return s.selector.Allows(ns) }

func (s *staticScope) Contains(ns string) bool { return slices.Contains(s.namespaces, ns) }

func (s *staticScope) IsEmpty() bool { return len(s.namespaces) == 0 }

func (s *staticScope) IsStatic() bool { return true }

// refreshingScope re-resolves its source selector on a ticker and serves the
// most recent successful resolution to readers.
type refreshingScope struct {
	// source is the ORIGINAL, unexpanded selector: every refresh re-expands it
	// from scratch so newly created and newly labeled namespaces are picked up
	// and stale ones drop out.
	source config.NamespaceSelector
	lister config.NamespaceLister

	mu sync.RWMutex
	// expanded and namespaces are the current snapshot. namespaces is
	// Resolve()'d at publish time so a read is just an RLock and a copy, and is
	// never mutated after publish.
	expanded   config.NamespaceSelector
	namespaces []string
}

func (s *refreshingScope) Namespaces() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.namespaces)
}

func (s *refreshingScope) Allows(ns string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.expanded.Allows(ns)
}

func (s *refreshingScope) Contains(ns string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Contains(s.namespaces, ns)
}

func (s *refreshingScope) IsEmpty() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.namespaces) == 0
}

func (s *refreshingScope) IsStatic() bool { return false }

// publish swaps in a newly resolved selector and reports the namespaces added
// and removed relative to the previous snapshot.
func (s *refreshingScope) publish(expanded config.NamespaceSelector) (added, removed []string) {
	namespaces := expanded.Resolve()

	s.mu.Lock()
	defer s.mu.Unlock()

	previous := s.namespaces
	s.expanded = expanded
	s.namespaces = namespaces

	return diff(previous, namespaces), diff(namespaces, previous)
}

// refreshLoop re-resolves the scope every interval until ctx is done.
func (s *refreshingScope) refreshLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refresh(ctx)
		}
	}
}

// refresh performs one re-resolution. On failure it logs and keeps the last
// good snapshot: emptying the scope would silently stop ingestion and widening
// it would silently breach the configured excludes, and a transient API error
// warrants neither.
func (s *refreshingScope) refresh(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, refreshTimeout)
	defer cancel()

	expanded, err := s.source.Expand(ctx, s.lister)
	if err != nil {
		// A refresh canceled by shutdown is not a failure. select picks
		// pseudo-randomly when both ctx.Done and the tick are ready, so this is
		// reachable on any clean exit; warning here would cry wolf every time.
		if parent.Err() != nil || errors.Is(err, context.Canceled) {
			return
		}
		logger.Logger.Warn("Failed to refresh namespace scope; keeping last resolved set",
			zap.Strings("namespaces", s.Namespaces()),
			zap.Error(err),
		)
		return
	}

	added, removed := s.publish(expanded)
	if len(added) == 0 && len(removed) == 0 {
		return
	}

	logger.Logger.Info("Namespace scope changed",
		zap.Strings("added", added),
		zap.Strings("removed", removed),
		zap.Strings("namespaces", s.Namespaces()),
	)
}

// Derived returns a Scope that serves parent's live namespace set with
// extraExcludes (name-based) additionally removed. It makes NO API calls and
// starts NO goroutine: it reads parent at the point of use, so it tracks
// parent's background refreshes automatically. Use it when two sources share
// the same include/label/"*" resolution but differ only by name-based excludes
// (e.g. the log scope self-excludes on top of the k8s scope).
//
// When extraExcludes contributes no non-blank name, there is nothing to
// subtract and Derived returns parent directly.
func Derived(parent Scope, extraExcludes ...string) Scope {
	excludes := make(map[string]struct{}, len(extraExcludes))
	for _, ns := range extraExcludes {
		if ns == "" {
			continue
		}
		excludes[ns] = struct{}{}
	}
	if len(excludes) == 0 {
		return parent
	}
	return &derivedScope{parent: parent, excludes: excludes}
}

// derivedScope is a lightweight read-time view over parent that additionally
// removes a fixed set of name-based excludes. It holds no lister and runs no
// refresh loop; every method defers to parent's current snapshot and filters,
// so parent's background refreshes are reflected automatically.
type derivedScope struct {
	parent   Scope
	excludes map[string]struct{}
}

func (s *derivedScope) excluded(ns string) bool {
	_, ok := s.excludes[ns]
	return ok
}

func (s *derivedScope) Namespaces() []string {
	parent := s.parent.Namespaces()
	// Filter in place: parent.Namespaces() already returns a fresh slice we own.
	out := parent[:0]
	for _, ns := range parent {
		if !s.excluded(ns) {
			out = append(out, ns)
		}
	}
	return out
}

func (s *derivedScope) Allows(ns string) bool {
	// Exclude wins, consistent with the name-based Exclude semantics.
	return s.parent.Allows(ns) && !s.excluded(ns)
}

func (s *derivedScope) Contains(ns string) bool {
	return s.parent.Contains(ns) && !s.excluded(ns)
}

func (s *derivedScope) IsEmpty() bool {
	// parent may be non-empty while every namespace it holds is excluded here, so
	// defer to the filtered set rather than parent.IsEmpty().
	return len(s.Namespaces()) == 0
}

func (s *derivedScope) IsStatic() bool { return s.parent.IsStatic() }

// diff returns the entries of b that are absent from a.
func diff(a, b []string) []string {
	var only []string
	for _, ns := range b {
		if !slices.Contains(a, ns) {
			only = append(only, ns)
		}
	}
	return only
}
